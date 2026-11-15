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

`client_name` and `client_version` are parsed from `User-Agent` with a small table of known clients (Codex CLI, Claude Code, T3 Code, opencode). Unknown agents keep the raw string with `client_name = "other"`. The raw `user_agent` column lets the parser change later and re-derive old rows.

Schema migrations are numbered SQL files applied at startup inside a transaction.

### Token normalization

The plugin ports CPA's accounting rules from `sdk/cliproxy/usage/accounting.go` (about 200 lines). The rule set is chosen by `provider` and `executor_type` (`accounting.go:340-364`), both present in `UsageRecord`, and produces the same non-overlapping buckets and quality flag as CPA. Raw counters stay in the row, so a fix to the port can re-derive old rows with one SQL migration.

The port names the CPA release it was copied from. Table tests use fixtures taken from CPA's own `accounting_test.go` cases, so a CPA upgrade that changes the rules shows up as a diff when the fixtures are refreshed.

### Writer and failure behavior

`HandleUsage` never touches the database. It pushes the row onto a bounded channel (capacity 10,000). One goroutine batches inserts every second or every 500 rows. When the channel is full, the row is dropped and a dropped counter increments. When an insert fails, the batch is logged through `host.log` once per minute with the error and dropped.

Every exported ABI entry point recovers from panics and returns an error envelope.

`GET .../health` returns rows written, rows dropped, last write error, interceptor join hit rate, and pricing table age. The dashboard shows these in a status strip.

### Pricing and cost

- Source: LiteLLM `model_prices_and_context_window.json`, fetched through `host.http.do` at startup and every 24 hours. The last good copy is stored in SQLite and used when a fetch fails. With no copy at all, every model shows as unpriced. Cost never blocks usage.
- Model matching follows T3 Code (`apps/server/src/usage/usagePricing.ts`): normalize the name, strip date and variant suffixes, prefer first-party provider entries over resellers. Matching uses `response_model` when present, else `model`.
- Rates per token type: input, cache read, cache write, output. Reasoning is priced as output. Service tier `priority` uses LiteLLM `_priority` rates when present.
- Overrides: config field `price_overrides`, an object of model name to per-1M-token rates, for models LiteLLM lacks.
- Cost is computed at query time from stored tokens, so price corrections apply to history. The dashboard labels it "API estimate".
- A "cost quality" panel shows the share of tokens priced by LiteLLM, by override, and unpriced, as in T3 Code.

### Dashboard

A single static HTML page with inline JS and SVG charts, no build step and no external network requests. It calls the management endpoints with the management key. The page reads the key the CPA management UI already stores in the browser for the same origin when available, otherwise it asks for it and keeps it in `sessionStorage` (unverified how the CPA management UI stores the key, see builder notes).

Filters: period (24h, 7d, 30d, 90d), provider, model, client, API key hash.

Views:

1. Totals: API-estimate cost, requests, sessions, processed tokens, cache hit rate (cache read / total input), output including reasoning. Per-provider cost share.
2. Daily cost and tokens, stacked by provider (toggle cost or tokens).
3. Per-request drift, the main view. CPA emits one record per upstream attempt, and split billing can add extra records for one response (`internal/runtime/executor/helps/usage_helpers.go:564-572`). "Per request" therefore means per inbound client request: rows summed by `trace_id`, excluding rows with `generate = false`. Totals and cost still count every row, since every attempt that reports tokens consumed them. The view shows daily medians (and p90) per request of uncached input, cache read, cache write, output and reasoning, plus daily cache hit rate. Grouped by reported model or by client version. Vertical markers on the days a new `client_version` or a new `response_model` first appears.
4. Breakdown table by model, by client and version, or by day: requests, tokens by type, cost, share, median input and output per request.
5. Model detail: cost per 1M tokens, cache hit rate, token-type mix, and the drift chart for that model.
6. Cost quality and plugin health.

### Management API

All `GET`, all behind the management key. All accept `from`, `to` (unix ms) and the filters above. Invalid parameters return `400` with a JSON error naming the parameter. Database faults return `503` with a distinct error code.

- `summary`: totals and per-provider share.
- `timeseries?bucket=day|hour&group=provider|model|client`: tokens and cost per bucket.
- `drift?group=response_model|client_version`: medians and p90 per bucket, plus change markers.
- `breakdown?by=model|client|day`
- `health`
- `pricing`: table age, unmatched models, overrides in use.

### Configuration

Under `plugins.configs.cpa-analytics`, declared as `ConfigFields`: `enabled`, `priority`, `db_path`, `retention_days` (default 0, keep forever), `price_overrides`, `pricing_url` (default the LiteLLM raw URL).

### Distribution

GitHub releases with the linux/amd64 `.so` and a `registry.json` the owner adds under `plugins.store-sources` (`internal/config/config_types.go:35`). Installation through the CPA plugin store.

### Testing

- Functional tests drive the plugin through its exported ABI entry point with recorded `usage.handle` and `request.intercept_before` payloads, then query the management routes and assert JSON results against hand-computed expectations.
- Normalization and pricing get table tests with fixtures for each protocol family (OpenAI chat, OpenAI responses/Codex, Claude, Gemini).
- An end-to-end smoke test builds CPA `v8.0.71` from source, loads the plugin, sends one request through a stub upstream, and checks that a row with the right `client_version` lands in SQLite. This is also the proof for the two unverified constraints above.

## Decided by the owner

- Approach 1, as integrated with CPA as possible.
- SQLite on a mounted volume.
- No request content analysis. Capture client and version, plus the model reported by the upstream.
- API-rate cost estimate in the first version.
- No changes to CPA. The plugin works with stock CPA releases so CPA upgrades stay routine. An upstream PR adding `TokenBreakdown` and `UserAgent` to the plugin usage record was considered and rejected for that reason. Consequences: the plugin keeps its own port of the accounting rules and reads `User-Agent` through the request interceptor.
- Target host: CPA `v8.0.71`, plugin ABI schema 10.

## Calls made while writing this (open to correction)

- Join on `TraceID`, not `RequestID`.
- Store only a hash of the client API key.
- No rollup tables in the first version.
- Cost computed at query time with current prices.
- Dashboard is a single static page with hand-written SVG charts.
- Plugin name `cpa-analytics`.
- "Per request" in the drift view means per inbound client request, summed by `trace_id`.

## Left to the builder

- Package layout, Go module path, and the User-Agent parsing table.
- Chart implementation details and page styling, matching the CPA management UI's look where practical.
- How the dashboard obtains the management key from the CPA management UI, after checking how that UI stores it.
- Retention job scheduling.
