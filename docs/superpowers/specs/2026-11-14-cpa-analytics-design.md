# cpa-analytics: token usage analytics plugin for CLIProxyAPI

Status: for owner approval. Date: 2026-11-14.
CLIProxyAPI reference: `github.com/router-for-me/CLIProxyAPI/v8`, release `v8.0.71` (commit `bd9c766`), the version in production. File references below point into that tree. The plugin ABI, usage, accounting and interceptor files are identical to `e96d4c4`, where the analysis started.

## Why

All AI traffic from Codex, Claude Code and T3 Code goes through one CLIProxyAPI (CPA) instance. When token spend goes up, the owner needs to answer one question quickly: did I change something, or did the model or provider change?

Daily totals cannot answer that, because more usage also raises them. The plugin compares tokens per request over time and splits that curve by the two things that change independently:

- the client and its version (from the inbound `User-Agent`), which the owner controls;
- the model the upstream says it ran (`ResponseModel`), which the provider controls.

A step in input or output per request with no client version change points at the provider. A step that lines up with a new client version points at the owner.

## Goals

- Record every request CPA executes: tokens by type, model requested and model reported, provider, credential, client and version, latency, failure.
- Show usage and API-rate cost estimates over time, in the style of T3 Code's Usage page.
- Show per-request medians of input, cache read, cache write, output and reasoning tokens, plus cache hit rate, split by client version and by reported model, with markers where either changes.
- Run fully inside CPA: install through the CPA plugin store, configure through CPA plugin config, open the dashboard from the CPA management UI.

## Non-goals

- Reading or storing request or response bodies. No prompt, tool or system-prompt fingerprints.
- Alerts, notifications or automatic anomaly detection. The dashboard shows the data and the markers. The owner reads it.
- Billing reconciliation. Cost is "what these tokens would cost at API list price", not what was paid. Most traffic uses subscription OAuth credentials.
- Multi-instance aggregation across several CPA nodes.

## Anti-goals

- The plugin must never take CPA down or slow requests. A full disk, a locked database or a pricing fetch failure degrades analytics only.
- No analytics data is reachable without the CPA management key.
- No raw client API keys in the database.

## Constraints found in CPA

- Plugins are C ABI shared libraries loaded in-process (`examples/plugin/README.md`). The CPA image builds on `golang:1.26-bookworm` and runs on `debian:bookworm` (`Dockerfile`), so the plugin is a Go `c-shared` build for linux/amd64 glibc. A panic in the plugin can crash CPA.
- `UsagePlugin.HandleUsage` receives a `UsageRecord` per completed execution (`sdk/pluginapi/types.go:1534`, `:1691`). The host delivers records asynchronously with a pending buffer of 4096 per plugin (`internal/pluginhost/adapters_usage_translation.go:134`), so a slow plugin does not block requests.
- `UsageRecord` has no `User-Agent`. The before-auth request interceptor receives the request headers and, from schema version 7, can opt out of the body via `RequestInterceptorOmitBodyBefore` (`sdk/pluginapi/types.go:120-125`, `sdk/pluginabi/types.go`).
- Correlation key: the interceptor gets `TraceID: logging.GetRequestID(ctx)` (`sdk/api/handlers/handlers_interceptors.go:498-509`) and `UsageRecord.TraceID` is "the parent inbound HTTP request". `UsageRecord.RequestID` falls back to a random UUID when the context carries none (`sdk/cliproxy/usage/manager.go:732-737`), so `TraceID` is the join key, not `RequestID`. Unverified: that both IDs are equal at runtime, and that the before-auth `Headers` are the inbound client headers. The first implementation step proves both against a running CPA.
- `UsageRecord` has no `User-Agent` even though CPA keeps it on the request context (`internal/logging/requestmeta.go:20`).
- `UsageDetail` given to plugins has raw counters only: `InputTokens`, `OutputTokens`, `ReasoningTokens`, `CachedTokens`, `CacheReadTokens`, `CacheCreationTokens`, `TotalTokens` (`sdk/pluginapi/types.go:1765`). Whether `InputTokens` includes cached tokens depends on the upstream protocol. CPA computes a canonical non-overlapping `TokenBreakdown` (uncached input, cache read, cache write, non-reasoning output, reasoning, plus a quality flag) in `sdk/cliproxy/usage/accounting.go`, but does not pass it to plugins. The plugin re-derives the same buckets (see Token normalization).
- Plugin storage: CPA offers no storage callback to plugins. The plugin owns a SQLite file on a volume the owner mounts.
- Plugin HTTP surface (`sdk/pluginapi/types.go:1610-1657`): `ManagementRoute` registers routes under `/v0/management/`, which require the management key. `ResourceRoute` registers browser pages under `/v0/resource/plugins/<pluginID>/` with a menu label, and these are not authenticated.
- Outbound HTTP from a plugin goes through the `host.http.do` callback.

## Approaches

1. **Single plugin: collector, API and dashboard (chosen).** One `.so` declares the usage, request interceptor (headers only) and management capabilities. One artifact, installed and configured through CPA. UI changes ship as a plugin release.
2. **Collector plugin plus separate dashboard container on the same SQLite volume.** Freer UI iteration, but two deployables, and less integrated than the owner asked for.
3. **External reader of CPA's usage queue (`/usage-queue`).** No plugin, but no `User-Agent` and no integration. Discarded.

The owner asked for the most integrated option, which is approach 1.

## Design

### Capabilities

- `usage_plugin`: converts each `UsageRecord` into a row and hands it to the writer.
- `request_interceptor` before auth, with `request_interceptor_omit_body_before`: reads `User-Agent` and stores `TraceID -> User-Agent` in a bounded in-memory map with a TTL of 10 minutes (long streaming requests finish well inside that). It never modifies the request and returns an empty response.
- `management_api`: data endpoints under `/v0/management/plugins/cpa-analytics/...` (exact base path comes from `ManagementRegistrationRequest.BasePath`) and one `ResourceRoute` serving the dashboard page, labelled "Analytics" in the CPA menu.

### Storage

SQLite via `modernc.org/sqlite` (pure Go, so the only cgo is the ABI shim). WAL mode. Path from config field `db_path`, default `/CLIProxyAPI/data/cpa-analytics.db`, which the owner maps to a Docker volume.

One table of raw facts. Aggregates are computed at query time. At the expected volume (one owner, tens of thousands of requests per month) indexed scans over months of rows return in milliseconds. If that stops being true, add a daily rollup table then.

`requests` columns:

| Column | Source |
|---|---|
| `trace_id`, `request_id` | record IDs. Unique index on `request_id` makes replays idempotent. |
| `requested_at` (unix ms) | `RequestedAt` |
| `provider`, `executor_type`, `auth_type`, `auth_id` | record |
| `model`, `alias`, `response_model` | record |
| `api_key_hash` | first 12 hex chars of SHA-256 of `APIKey`. Raw key never stored. |
| `access_provider`, `source`, `session_id`, `parent_session_id` | record |
| `reasoning_effort`, `service_tier`, `effective_service_tier`, `response_service_tier` | record |
| `stream`, `generate`, `failed`, `status_code` | record. Failure body is not stored. |
| `latency_ms`, `ttft_ms` | record |
| `input_tokens`, `output_tokens`, `reasoning_tokens`, `cached_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `total_tokens` | raw `UsageDetail` counters, kept as received |
| `uncached_input`, `cache_read`, `cache_write`, `output_non_reasoning`, `output_reasoning`, `accounting_quality` | normalized buckets (see Token normalization) |
| `user_agent`, `client_name`, `client_version` | joined from the interceptor map. Empty when the join misses. |
| `auth_index` | record. Matches `auth_index` in CPA's credential list. |
| `limit_session_pct`, `limit_session_reset`, `limit_weekly_pct`, `limit_weekly_reset` | subscription limit snapshot from the upstream response headers (see Weekly limit value). Percent of the limit and reset in unix ms. NULL when the response did not report the window. |

`client_name` and `client_version` are parsed from `User-Agent` with a table of known coding agents (Codex variants, Claude Code, T3 Code, opencode, Antigravity, Gemini CLI, Cursor, Aider, Cline, Roo Code, Kilo Code). Any other valid `product[/version]` token keeps its normalized name and version so version drift still produces markers; only unparseable agents fall back to `"other"`. Migration `004_reparse_clients` rebuilds historical rows from `user_agent`.

Token normalization also covers `meta` (subset semantics, matching CPA's `MetaExecutor`) and `devin` (separate-reasoning semantics, matching CPA's `DevinExecutor`), with migration `005_meta_devin_accounting` reclassifying historical rows.

Schema migrations are numbered SQL files applied at startup inside a transaction.

### Token normalization

The plugin ports CPA's accounting rules from `sdk/cliproxy/usage/accounting.go` (about 200 lines). The rule set is chosen by `provider` and `executor_type` (`accounting.go:340-364`), both present in `UsageRecord`, and produces the same non-overlapping buckets and quality flag as CPA. Raw counters stay in the row, so a fix to the port can re-derive old rows with one SQL migration.

That function is CPA's fallback for records whose executor attached no breakdown. CPA's Claude executor always attaches one (`internal/runtime/executor/helps/usage_helpers.go`, `parseClaudeUsageNode`): `OutputTokens` includes the thinking counted in `ReasoningTokens`, and `TotalTokens` is input plus cache plus `OutputTokens`. The fallback treats reasoning as separate from output for Anthropic, so a ported-only rule marks every Claude call with thinking as inconsistent. The port therefore recognizes that layout first and splits it the way the executor does. Plugin 0.1.3 and earlier lacked this rule, and migration `002_claude_thinking.sql` re-derives the rows they stored.

The port names the CPA release it was copied from. Table tests use fixtures taken from CPA's own `accounting_test.go` cases, so a CPA upgrade that changes the rules shows up as a diff when the fixtures are refreshed.

### Writer and failure behavior

`HandleUsage` never touches the database. It pushes the row onto a bounded channel (capacity 10,000). One goroutine batches inserts every second or every 500 rows. When the channel is full, the row is dropped and a dropped counter increments. When an insert fails, the batch is logged through `host.log` once per minute with the error and dropped.

Every exported ABI entry point recovers from panics and returns an error envelope.

`GET .../health` returns rows written, rows dropped, last write error, interceptor join hit rate, and pricing table age. The dashboard shows only storage errors, lost records, write errors and pricing refresh failures. Writer and interceptor counters describe the running process, not the selected period, so they stay out of the usage page.

### Pricing and cost

- Source: LiteLLM `model_prices_and_context_window.json`, fetched through `host.http.do` at startup and every 24 hours. The last good copy is stored in SQLite and used when a fetch fails. With no copy at all, every model shows as unpriced. Cost never blocks usage.
- Model matching follows T3 Code (`apps/server/src/usage/usagePricing.ts`): normalize the name, strip date and variant suffixes, prefer first-party provider entries over resellers. Matching uses `response_model` when present, else `model`.
- Rates per token type: input, cache read, cache write, output. Reasoning is priced as output. Service tiers `priority` and `flex` use LiteLLM's `_priority` and `_flex` rates when the model has them, and the standard rates otherwise.
- Long context: a request whose input (uncached, cache read and cache write) exceeds a model's `_above_<N>k_tokens` threshold pays those rates for all its tokens, with the tier's long-context rates when LiteLLM lists them. Each row is one request for this purpose, since split-billing rows carry another model. Queries group rows by input band (`Table.BandSQL`) so a long request never reprices short ones it is summed with.
- Overrides: config field `price_overrides`, an object of model name to per-1M-token rates, for models LiteLLM lacks.
- Cost is computed at query time from stored tokens, so price corrections apply to history. The dashboard labels it "API estimate".
- A "cost quality" panel shows the share of tokens priced by LiteLLM, by override, and unpriced, as in T3 Code.

### Dashboard

A single static HTML page with inline JS and SVG charts, no build step and no external network requests. It calls the management endpoints with the management key. The page reads the key the CPA management UI already stores in the browser for the same origin when available, otherwise it asks for it and keeps it in `sessionStorage` (unverified how the CPA management UI stores the key, see builder notes).

Filters: period (15m, 1h, 6h, 24h, 7d, 30d, 90d), provider, model, client, account, API key hash.

Filter choices come from the authenticated `options` route, which reads distinct provider, reported-model and client names without calculating costs or percentiles. Options cover the selected period regardless of the active dimension filters. Each full load uses one time range for all sections. The summary and daily series load first and render without waiting for the detail charts, filter options or credential names. Details then load with at most two concurrent queries, and each section renders when its response arrives. Pending sections hide the previous selection behind a loading message. A failed detail shows an error in that section while completed sections remain usable. Authentication failure clears reusable data, returns to the key form and stops queued work. Changing the drift or breakdown grouping reloads only that section, using the displayed range. Rapid full-refresh requests finish the active batch and then load only the latest selection. Superseded responses and errors never update the page.

Changing provider, model, client or API key keeps the displayed time range and reuses filter options and credential names in page memory. Weekly limit results are also reused when the provider is unchanged, since model, client and API key do not affect them. Manual refresh, auto-refresh, a period change or a timezone change renews the range and data. The status names the end of the displayed range. No shared or persistent cache is added.

Account affinity, quota left at reset and cost quality use collapsed native disclosures after the main analyses. They query on expansion and retain the result for that selection. Closed sections do not query on refresh. Open sections refresh with the page, and later expansions wait for the active load before querying. Tokens without a price always show a warning next to the cost total, using the summary response. The previous-period summary is queried only when the current summary's `data_since` confirms that recording covers that entire period.

Provider colors (owner's request, 2026-11-15): wherever the provider is the series (cost share by provider, usage over time, the provider mark in the model breakdown), Claude, Codex and Gemini wear their brand colors. Light: Claude `#d97757` (the logo orange), Codex `#3941ff` (the darkest stop of the Codex icon gradient), Gemini `#4796e3` (the start of the Gemini gradient). Dark: Claude `#d4704f` and Codex `#4549ff`, stepped into the dark lightness band and to 3:1 on the dark surface, and Gemini unchanged. The three pass the dataviz palette checks against each other in both themes. Any other provider takes one spare green (light `#008300`, dark `#13a065`), the only color that stays apart from all three for color-blind viewers, in order of first appearance. The rest share the overflow gray, and the legend and tables name them. Google's blue, Kimi's and Meta's blues, and the monochrome xAI and OpenAI marks would collide with these, so they get no brand color. The Limits view keeps neutral bars. Claude orange is too close to the critical red that marks a spent account to share a section with it. The value-of-the-weekly-limit chart draws each account in its provider's color, and a further account of the same provider takes the next dash pattern (in auth index order). Past four accounts of one provider the rest share the overflow gray.

Hue is reserved (owner's request for fewer scattered colors, 2026-11-15). Color marks identity that recurs across the page (providers, and the categorical slots of the drift chart) and status (good, warning, critical). Everything that only splits one bar (token types, service tiers, price sources) and the Limits meters and trend use one cool gray ramp of five steps, light `#1e2633 #3f495b #626d81 #8590a3 #a9b2c1` on `#ffffff` and dark `#eceef2 #c4cad4 #9ba3b1 #77808f #5a6372` on `#181a1e`, validated as an ordinal ramp in both themes. Service tiers take every other step first so two or three tiers stay apart. No text sits on a colored fill: each legend entry carries the value and its share. Secondary text clears 4.5:1 on page and card in both themes.

Copy (owner's request, 2026-11-15): each section explains itself in one or two short sentences. Empty states say what is missing and when it appears, without naming the plugin version that started recording it. The drift chart's list of new versions, models and efforts sits folded under "Changes (N)", since the vertical lines already mark them.

Tooltips on bar and column charts open beside the hovered column, flipping left at the viewport edge, so they never cover the marks they describe (owner's request, 2026-11-15). Other tooltips follow the pointer.

Reading aids (owner's decision, 2026-11-15, from common dashboard practice):

- The cost figure and every totals tile with a per-bucket value carry a sparkline of that value per day, or per hour in the 24-hour view, in the de-emphasis gray with the latest bucket marked. Sessions and cache savings have no per-bucket value in the series, so their tiles have none. The failed-records sparkline is the failure trend, so there is no separate failure chart. Hovering a sparkline draws a hairline and a dot at the nearest bucket and opens the tooltip beside that point, with the value followed by the tile's name, since the tooltip otherwise reads as belonging to the neighbouring tile (owner's report, 2026-11-15). Color was considered and not used: every tile is the same kind of figure, so a hue would not tell them apart by meaning. Buckets that ended before recording began (`data_since`) have no value, so unrecorded days do not read as idle ones.
- The drift chart names up to four lines at their end, only lines that reach the latest plotted day, and drops a name that would sit within 14 px of a busier line's name instead of moving it away from its line. Charts narrower than 560 px keep the legend only. Hovering or focusing a legend entry dims the other lines.
- Not built: a month-end cost forecast (the cost is an API estimate, and the weekly limit, which the Limits view already projects, is what constrains subscription accounts), a cost-per-1M-tokens trend per model (the drift view and model detail cover it), and an hour-by-weekday heatmap (no question needs it yet). Reopen the heatmap if the owner wants to plan usage around the limit windows.
- Drift lines keep the categorical slots and never take the provider color, since several models of one provider would get hard-to-tell shades and the provider means nothing when grouping by client version or effort. Limits bars keep neutral fills with warning and critical for state. Claude orange sits between those two and would read as a state, and texture is only for accessibility or print, never a default fill.

Views:

1. Totals: API-estimate cost, requests, sessions, cost per request, processed tokens (with the average per active day, or per active hour in the 24-hour view), cache hit rate (cache read / total input), cache savings (cache reads priced as uncached input, minus their actual price), output including reasoning. Cost share by provider, with each provider's tokens and sessions. A session that used two providers counts once in each. Cost and tokens by token type (input, cache read, cache write, output), and cost by service tier. The tier split shows the premium paid over standard rates for priority calls and the discount for flex calls, since turning on a fast mode raises cost without changing any token count. Cost, requests, sessions, cost per request, processed tokens and output show their change against the previous period of the same length, and cache hit rate its change in percentage points. The change is left out when recording began inside that previous period (`data_since` in `summary`), since its partial totals would read as growth.
2. Daily cost and tokens, stacked by provider (toggle cost or tokens).
3. Per-request drift, the main view. "Per request" means per successful upstream call: generating, non-failed rows summed by `trace_id` plus `requested_at` (see "Unit of a request" below). Totals and cost still count every row, since every attempt that reports tokens consumed them. The view shows per-bucket (day for 7 to 90 days, hour for 6 to 24 hours, minute for 15 minutes to 1 hour) medians and p90 per request of uncached input, cache read, cache write, output, reasoning, output including reasoning, API-estimated cost per request, latency, time to first token and output tokens/s, plus cache hit rate. CPA reports time to first token only for streamed calls, so its percentiles leave out calls without one, and a bucket with none has no point. Grouped by reported model, by client (harness, one continuous line per client name), by client version or by reasoning effort, since a client that raises its default effort spends more output with no model change. Change markers follow the active grouping: the model view marks only new models and never harness versions, and the client (harness) view marks new versions of each plotted harness. The chart draws only the markers of plotted series, so small one-off values do not clutter it (owner's request, 2026-11-20). A request that names no effort is grouped as `unset` and never gets a marker. The default measure is input including cache, which moves when the context grows whether or not the cache absorbs it. The first recorded day gets no markers, since every value is new on it.
4. Breakdown table by provider, model (one row per provider and model, since the same model can be reached through two providers at different prices), client and version, raw User-Agent, day, API key hash, reasoning effort, account (clickable to filter) or failed records (`errors`, grouped by provider, model, account and HTTP status): requests, failures, tokens by type, cost, cost per request, share, median input and output per request.
5. Model detail: cost per 1M tokens, cache hit rate, token-type mix, and the drift chart for that model.
6. Cost quality and plugin health.
7. Weekly limit value: API-priced cost per 1% of each Claude and Codex account's weekly limit, per day. For the same kind of work, a lower value means the provider tightened the limit. See below.
8. Quota left at reset: for each weekly cycle that ended in the range, what the account had not used, priced at that cycle's own cost per 1%. Unused quota does not carry over, so this is what the subscription paid for and nobody used. See below.
9. Account affinity: per provider, how often a session moved to another account and how much cache it wrote again because of it. See below.
10. Latency: one dot per successful streamed call, time to first token against total latency on log scales, colored by provider, with dashed lines at the 95th percentile. At most 3,000 calls are drawn, evenly sampled in time order. Percentiles cover every call.
11. Requests: one row per upstream call (see "Unit of a request"), newest first or by cost, latency or tokens, all calls or failed or successful only, 50 per page. Selecting a row shows its metadata, tokens and cost. It never shows request or response content, which the plugin does not store.

Short periods (15m, 1h, 6h) in the top Period filter replace the earlier separate "Right now" card (owner's request, 2026-11-20), so Totals, Usage over time, Per-request drift, Breakdown, Latency and Requests all cover short windows with previous-period comparisons and minute/hour buckets without duplicating tiles.

### Weekly limit value

Decided 2026-11-15. Claude and Codex report the account's limit usage on every response, in headers CPA passes to the plugin in `UsageRecord.ResponseHeaders` (`publishRecord` in `internal/runtime/executor/helps/usage_helpers.go`, set for every request, Codex WebSocket quota events merged in). The plugin stores a snapshot per row, so limit history needs no extra call and no token.

- Claude: `Anthropic-Ratelimit-Unified-5h-Utilization` and `-7d-Utilization` are fractions of the limit, `-Reset` epoch seconds. Scoped windows such as `7d_opus` are not read.
- Codex: `X-Codex-Primary-*` and `X-Codex-Secondary-*` with `Used-Percent` (0 to 100), `Window-Minutes`, and `Reset-At` (epoch seconds) or `Reset-After-Seconds` (counted from the response). A window of a day or less is the session, 7 to 30 days the week. Code review and other windows are not read.
- A value that cannot be read is stored as NULL, never clamped or guessed. A readable percent with an unreadable reset keeps the percent.
- `limit-burn` walks each account's rows in time order. Between two consecutive weekly snapshots of the same cycle (resets within an hour of each other and the percent not lower), the cost of the rows after the first snapshot up to the second is what moved the percent. A pair that crosses a reset or goes down is dropped. Each pair counts on the day of its second snapshot. Days on which an account moved less than 1 point, or cost nothing, are left out. The weekly percent belongs to the whole account, so the route takes only the range and provider and rejects model, client and API key filters. The same rule holds for `limit-leftover` and `limit-history`.
- A weekly cycle is the run of an account's snapshots whose resets stay within an hour of each other. A percent that drops inside a cycle is a Claude refill: that pair is dropped and the cycle goes on.
- `limit-leftover` ends each cycle at its reset, or earlier at the first snapshot of the next cycle, as when a banked Codex reset is redeemed. Those are marked early. Used is the last snapshot's percent, capped at 100, and left is 100 minus it. The cycle's cost per 1% is the cost of its pairs over the points they moved, and a cycle that moved less than 1 point through CPA has no price. To catch a cycle that started before the range, the walk starts 8 days before `from`. Only cycles that ended inside the range and before now are listed.
- `limit-history` returns each account's weekly snapshots, the last one per bucket. The bucket is 15 minutes, or wider when the range would need more than 700. The last snapshot before `from` comes first, so the chart starts from a known value.

### Limits view

Decided 2026-11-15, expanded 2026-11-20 to cover all CPA providers. A second view, picked by a Usage | Limits switch in the filter row, shows the subscription limits of every provider in CPA that exposes read-only quota (Claude, Codex, Antigravity via `loadCodeAssist` + `fetchAvailableModels`, Gemini CLI OAuth via `retrieveUserQuota`, Devin via CPA's `quota.signals`, and Kimi via `/coding/v1/usages`), pooled across accounts, plus CPA status, active cooldowns and recent traffic for every other active provider (`xai`, `meta`, `aistudio`, `vertex`, API-key accounts, etc.). It reads live. Its weekly trend also reads `limit-history`, which the plugin records from response headers (see "Weekly limit value").

- Accounts come from `GET /v8/management/credentials`, provider `claude` or `codex`, disabled ones left out. Each account's usage is read with `POST /v8/management/requests/api-call` and `Authorization: Bearer $TOKEN$`, so CPA substitutes the token and applies the credential's proxy. At most 4 calls run at once. The view reads when it opens and every 5 minutes.
- Claude: `GET https://api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20`. Windows come from `limits[]`: `session` (5 hours), `weekly_all`, and `weekly_scoped` per model. Without `limits[]` the page falls back to `five_hour` and `seven_day`. An account with no session limit has no session in progress and joins the 5-hour pool at 0%. The plan comes from `GET /api/oauth/profile`, `organization.rate_limit_tier` (`max_20x` weighs 20, `max_5x` 5) or `account.has_claude_pro` (1), read once per page load.
- Codex: `GET https://chatgpt.com/backend-api/wham/usage`, with `ChatGPT-Account-Id` from the credential's `id_token` when present. `rate_limit.primary_window` and `secondary_window` are named by `limit_window_seconds`: 30 days or more is monthly, 7 days or more weekly, shorter is a session. `plan_type` weighs `plus` 1, `prolite` 5, `pro` 20, and `business` (formerly `team`), `enterprise` and `edu` 1 each, since their seats get Plus-level limits.
- Antigravity: `loadCodeAssist` gives the tier (`paidTier.id` first, then `currentTier.id`). `retrieveUserQuotaSummary` gives groups of models (Gemini, Claude and GPT, Gemini 4 Argon) whose buckets carry `window` (`5h` or `weekly`), `remainingFraction` and `resetTime`. Each becomes "5-hour, <group>" or "Weekly, <group>", and a spent weekly bucket blocks its own group's 5-hour window. A bucket nobody used reports a reset of now plus its window, so it reads as not started. Requests send the Antigravity Hub User-Agent of the pinned CPA tag, because Google leaves newer model groups out for older clients.
- Without groups from the summary, the page falls back to `fetchAvailableModels`, one `quotaInfo` per model and no weekly figure. Models are grouped into Gemini (Pro and Flash collapse when they match), Claude & OSS and other. Pro and Ultra windows are "5-hour, <group>" and free-tier ones "Weekly, <group>". A paid group whose reset is more than 5 hours (plus 5 minutes of slack) away is held by the weekly cap and shows as "Weekly, <group>".
- Antigravity has no usage-over-time chart: its responses carry no limit headers, so the plugin records no history for it. Not page-recorded snapshots or a token chart from recorded traffic, because the owner chose on 2026-11-20 to show only the live reading when the limit history does not exist.
- Gemini CLI quotas are daily, named "Daily, <model family>". Every window label starts with its length, as Claude's and Codex's do.
- Claude extra usage, the credits bought past the plan limits, is listed per account under the pools as this month's spend and its limit, from `spend` (`amount_minor` / 10^`exponent`) or, in older responses, `extra_usage` (`used_credits` and `monthly_limit` / 10^`decimal_places`). Accounts with extra usage off are not listed.
- Codex windows other than `primary_window` and `secondary_window`, such as code review, are not shown. The owner left them out until they are needed.
- Pool per window: used = sum(w * u) / sum(w), with plan weights only when every account in the pool has a known plan, otherwise equal weights. The pool is one bar. When a window has more than one account, each account has its own bar below against its own limit, labelled with its plan, its share of the pool (w / sum(w)), its used percent and its reset, so a spent account reads as full without hovering. A spent account's bar turns critical red with "Limit reached", and a blocked session is gray. When a window has only one account, the duplicate per-account bar is omitted: the single bar carries the status color and reset time, the provider subtitle names the account and plan (or the window header names it when the provider has other readable accounts), and the weekly trend shows a single "At limit" figure instead of both "First at limit" and "Whole pool at limit". The first version stacked one segment per account (w * u / sum(w)) in the pool bar. The owner rejected it on 2026-11-15 because a spent account looked like a small segment and only the tooltip said it was at 100%.
- Pace: elapsed share e = clamp((L - (reset - now)) / L, 0, 1), 0 for a window with no reset. Expected = 100 * weighted mean of e. Usage more than 5 points above expected is ahead of pace, more than 5 below is under pace. Blocked windows (below) are left out of the pace, since they consume nothing.
- Restores: each account gives back round(refill * w / sum(w)) percent of the pool at its reset, soonest first. A reset already in the past gives back nothing.
- A spent weekly limit blocks the account's session window, which counts as 100% used. It comes back whole at the weekly reset when the session resets first or has no reset, whole at its own reset when it is spent and resets after the week, and otherwise gives back its unused part at the weekly reset.
- Errors are per account and do not stop the others. CPA's `api-call` renews tokens only for antigravity, xai and meta, so a Claude or Codex 401 or 403 reads "Token expired. CPA renews it on the next proxied request", or asks for a sign-in when the credential has `requires_sign_in`.
- Never called: routes that spend credits or quota, such as Codex `rate-limit-reset-credits/consume`.
- Weekly trend, under each pool's weekly bar: the pool's used percent over the past 7 days and projected over the next 7, in 15-minute steps. History pools the accounts the view reads live, with their live weights. Each account's recorded snapshots end with the live reading, and each snapshot holds until the next one or until its reset, after which it reads 0. Before an account's first snapshot CPA sent it no traffic, so from the start of recording it holds that first value, though never back past the start of that snapshot's cycle. Without this, an account spent before recording began had no snapshot at all, dropped out of the pool, and the recorded line ended below the bar. The owner chose this on 2026-11-15 over a gap until every account has a snapshot and over a note on the chart. Usage outside CPA, such as claude.ai, can make the held value overstate the past. The projection runs each account linearly at its own rate over the last 24 hours, caps it at 100 and starts it from 0 at each reset, a week apart. It ignores traffic moving to other accounts when one is spent. The stats above the chart show the pool's burn over the last 3 hours and the last 24 hours, the first account to reach its limit and when the whole pool would. A projection on the 3-hour rate read "never" every morning after an idle night, which is why the projection uses 24 hours and the 3-hour rate is shown next to it. An account with one snapshot or none has no rate and stays flat.
- The math is in `internal/web/limits.js`, a DOM-free script tested with bun and inlined into the page by `web.go`, so the page stays one static file.

### Account affinity

CPA keeps a session on one account, keyed by provider, session and model (`Pick` in `sdk/cliproxy/auth/selector.go`), because the prompt cache belongs to the account. A call that lands on another account writes its cache again.

- `affinity` groups generating, non-failed rows that name both a session and an account by provider, session and model, in request order (see "Unit of a request"). A switch is a request on a different account from the one before it. A session is a key with two or more requests.
- Per provider: requests, sessions, sessions with at least one switch, switches, cache write tokens and their cost, and the cache written by requests right after a switch, with its cost.
- Unverified: that `UsageRecord.SessionID` is the ID the selector keys on. Both come from the client's request metadata in v8.0.71.

### Management API

All `GET`, all behind the management key. All accept `from`, `to` (unix ms) and the filters above. Invalid parameters return `400` with a JSON error naming the parameter. Database faults return `503` with a distinct error code.

- `options`: distinct non-empty provider, reported-model and client names in the filter, sorted alphabetically.
- `summary`: totals, cost by provider, token type and service tier, and `data_since`, the first recorded row of any filter.
- `timeseries?bucket=day|hour|minute&group=provider|model|client`: tokens, cost, requests and failed records per bucket.
- `drift?group=response_model|client|client_version|reasoning_effort&bucket=day|hour|minute`: medians and p90 per bucket, plus change markers scoped to the active grouping.
- `breakdown?by=model|client|day|api_key|reasoning_effort|account`
- `health`
- `pricing`: table age, unmatched models, overrides in use.
- `limit-burn`: per day and account, the weekly limit points used, their cost and the cost per point.
- `limit-leftover`: per ended weekly cycle and account, the percent used and left, the cost per point and the left part at API prices.
- `limit-history`: per account, weekly snapshots, the last one per bucket.
- `affinity`: per provider, session switches between accounts and the cache written again after them.
- `requests?sort=recent|cost|latency|tokens&outcome=all|failed|ok&offset=&limit=1..200`: one page of upstream calls and the number of calls in the filter.
- `latency`: an even sample of up to 3,000 successful calls with a first token, and percentiles over all of them.

### Configuration

Under `plugins.configs.cpa-analytics`, declared as `ConfigFields`: `enabled`, `priority`, `db_path`, `retention_days` (default 0, keep forever), `price_overrides`, `pricing_url` (default the LiteLLM raw URL).

### Distribution

GitHub releases with the linux/amd64 `.so` and a `registry.json` the owner adds under `plugins.store-sources` (`internal/config/config_types.go:35`). Installation through the CPA plugin store.

### Testing

- Functional tests drive the plugin through its exported ABI entry point with recorded `usage.handle` and `request.intercept_before` payloads, then query the management routes and assert JSON results against hand-computed expectations.
- Normalization and pricing get table tests with fixtures for each protocol family (OpenAI chat, OpenAI responses/Codex, Claude, Gemini).
- An end-to-end smoke test builds CPA `v8.0.71` from source, loads the plugin, sends two requests through a stub upstream, and checks that a row with the right `client_version` lands in SQLite and that the stub's Codex limit headers reach `limit-burn`. This is also the proof for the two unverified constraints above.

## Decided by the owner

- Approach 1, as integrated with CPA as possible.
- SQLite on a mounted volume.
- No request content analysis. Capture client and version, plus the model reported by the upstream.
- API-rate cost estimate in the first version.
- No changes to CPA. The plugin works with stock CPA releases so CPA upgrades stay routine. An upstream PR adding `TokenBreakdown` and `UserAgent` to the plugin usage record was considered and rejected for that reason. Consequences: the plugin keeps its own port of the accounting rules and reads `User-Agent` through the request interceptor.
- Target host: CPA `v8.0.71`, plugin ABI schema 10.
- Limits view (2026-11-15): pooled subscription limits per account, like T3 Code's Limits page. The browser reads them through CPA's `api-call`, so tokens stay in CPA. Rejected: a plugin backend reading tokens with `host.auth.get` and calling out with `host.http.do`. First version covers Claude and Codex only. Claude accounts are weighted by plan, at the cost of one profile call per account per page load.
- Limit history (2026-11-15): the plugin keeps the limit snapshot each response reports in its headers, and the dashboard prices each point of the weekly limit to show when a provider changes it. Rejected: response interceptors, which receive bodies with no opt-out, run per stream chunk and sit in the delivery path. Not chosen: snapshots taken by the dashboard page from the Limits view, which would record only while the page is open.
- Previous-period comparison on the totals and total input as the default drift measure (2026-11-15). Alerts and an automatic "what changed" summary were deferred.
- After comparing the plugin with Theo's CPA setup (the prox dashboard and T3 Code's Limits page), 2026-11-15: add the weekly pool history and projection, quota left at reset, account affinity and the breakdown by account. The Limits view keeps its live reading and gains the history, since both matter.
- Codex banked resets are not shown. The CPA management center already shows them, so the plugin would only repeat it. Reopen if the owner asks.

## Unit of a request

Decided 2026-11-15, replacing "rows summed by `trace_id`".

- CPA emits one usage record per upstream attempt. Split billing adds extra records for the same response with fresh request IDs (`internal/runtime/executor/helps/usage_helpers.go:552-574`).
- `TraceID` is the ID of the inbound HTTP request (`logging.GetRequestID`). `usage.WithTraceID` is never called in v8.0.71. A Responses WebSocket connection builds every turn from the upgrade request's context (`sdk/api/handlers/openai/openai_responses_websocket.go:697,943`), so all turns of one connection share one trace. Grouping by trace turned a 40-turn session into one request.
- Every upstream call, including each WebSocket turn and each Codex duplex follow-up, gets its own `UsageReporter`, which stamps `RequestedAt` once (`usage_helpers.go:134-142`, `codex_websockets_duplex.go:974,1060`). Split billing records reuse their reporter, so they share that timestamp.
- So a request is the generating, non-failed rows sharing `trace_id` and `requested_at`. That is one per HTTP request (failed retries are excluded) and one per WebSocket turn, with split billing kept together. No schema change, no interceptor timing.
- Rejected: one row per request (splits split billing), segmenting traces by interceptor call times (depends on the order of two clocks inside CPA and needs a new column), and keeping trace grouping with a caveat (the main view misreads Codex WebSocket sessions).
- Reopen if a CPA upgrade calls `usage.WithTraceID` per turn or passes a per-call ID to plugins.

## Calls made while writing this (open to correction)

- Join on `TraceID`, not `RequestID`.
- Store only a hash of the client API key.
- No rollup tables in the first version.
- Cost computed at query time with current prices.
- Dashboard is a single static page with hand-written SVG charts.
- Plugin name `cpa-analytics`.
- "Per request" in the drift view means per successful upstream call. See "Unit of a request".

## Left to the builder

- Package layout, Go module path, and the User-Agent parsing table.
- Chart implementation details and page styling, matching the CPA management UI's look where practical.
- How the dashboard obtains the management key from the CPA management UI, after checking how that UI stores it.
- Retention job scheduling.
