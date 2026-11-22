# cpa-analytics

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that records token usage per request and shows it in a dashboard inside the CPA management center. It answers one question first: when input or output tokens per request go up, was it your client upgrade or the model and provider?

It runs in-process as a CPA C ABI plugin and needs no CPA changes. It is built and tested against CPA v8.0.71, and released for Linux x86_64 and macOS arm64.

## What it records

One row per upstream call, stored in SQLite:

- Provider, requested model, the model the upstream reported, and the service tier.
- Tokens split into uncached input, cache read, cache write, output, reasoning, and unclassified. The split uses the same rules CPA applies internally.
- The client name and version, parsed from the inbound `User-Agent`. Known coding agents (`codex`, `claude-code`, `t3code`, `opencode`, `antigravity`, `gemini-cli`, `cursor`, `aider`, `cline`, `roo-code`, `kilo-code`) are normalized, and other valid `product[/version]` agents keep their own name and version. Unparseable agents become `other`.
- A hash of the client API key, never the key itself.
- For Claude and Codex subscription accounts, the 5-hour and weekly limit usage reported in response headers, and which credential made the call. For Antigravity accounts, per-group 5-hour and weekly quota snapshots polled in the background every 5 minutes and stored in `quota_snapshots`.

Request and response bodies are never read or stored.

## Install

### 1. Give the database a volume

The default database path is `/CLIProxyAPI/data/cpa-analytics.db`, inside the CPA container. Mount a volume there so the history survives container upgrades:

```yaml
# docker-compose.yml
services:
  cli-proxy-api:
    volumes:
      - cpa-analytics-data:/CLIProxyAPI/data
volumes:
  cpa-analytics-data:
```

### 2. Install the library

From the plugin store: add this repository's `registry.json` to `plugins.store-sources`, then install `cpa-analytics` from the management center. The store checks the latest GitHub release of this repository and offers each new version as an update.

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/marcusgrando/cpa-analytics/main/registry.json"
```

By hand: download `cpa-analytics_<version>_linux_amd64.zip` or `cpa-analytics_<version>_darwin_arm64.zip` from a [release](https://github.com/marcusgrando/cpa-analytics/releases), check it against `checksums.txt`, and unzip the library (`cpa-analytics.so` or `cpa-analytics.dylib`) into `plugins.dir`.

On macOS, outside Docker, set `db_path` (step 3) to a writable path such as `~/.cli-proxy-api/cpa-analytics.db`. The default path only exists in the CPA container.

### 3. Configure it

Everything under `plugins.configs.cpa-analytics` is optional:

```yaml
plugins:
  configs:
    cpa-analytics:
      enabled: true
      db_path: "/CLIProxyAPI/data/cpa-analytics.db"
      retention_days: 0          # default 0 keeps everything. Positive values prune at start, on reconfigure and daily.
      pricing_url: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
      plan_costs:                # optional USD/month overrides by "provider:Plan" or auth_index
        "claude:Max 20x": 200
      price_overrides:           # USD per one million tokens
        my-internal-model:
          input: 1.25
          output: 10
          cache_read: 0.125        # left out: priced like input. 0: free.
```

Invalid values (negative retention or prices, a non-http(s) `pricing_url`) make registration fail with an error naming the field.

## Dashboard

Open the management center and pick **Analytics** in the menu. The page is also served directly at:

```
http://<cpa-host>:<port>/v0/resource/plugins/cpa-analytics/dashboard
```

The page itself holds no data. It reads every number from the management routes below with your management key. Inside the management center it reuses the key the center stored, when you enabled "remember password". Otherwise it asks for the key and keeps it in `sessionStorage` for that tab only.

The dashboard has four views in the navigation bar: **Usage**, **Limits**, **Plans**, and **Requests**.

The Usage view reloads its data every 60 seconds while the tab is visible. **Auto refresh** turns it off for that tab. Limits and Plans refresh every 5 minutes. Requests polls every 3 seconds by default (configurable or off).

The totals show how each figure changed against the previous period of the same length, once the plugin has recorded through that whole period.

The drift chart shows how a typical call (median) or a call at the 90th percentile changes over time. It opens on input tokens including cache for the five busiest reported models with qualifying data. Each point needs five measured calls by default. Choose **All measured buckets** to see smaller samples. **Change from first bucket** compares each group with its own first qualifying bucket. Cache hit rate uses percentage points. The largest changes show both endpoint dates, values and call counts. Select a change to isolate its series, then use the legend to add groups. Group by client to see when each client version first appeared.

The same chart plots median or p90 latency, time to first token, API cost, and output tokens/s per request. Time to first token covers streamed calls only, since CPA reports none for the others.

**Value of the weekly limit** plots, per day and account (and per Antigravity quota group: `Gemini` and `Claude and GPT`), the API-priced cost of each 1% of the account's weekly limit. For the same kind of work, a drop means the provider tightened the limit. Days on which an account moved less than 1% of its week are left out.

**Latency** (expandable in Usage) plots time to first token against total latency on log scales for up to 3,000 evenly sampled streamed calls, with p95 lines across all streamed calls.

**Quota left at reset** lists each weekly cycle that ended in the range, per account (and per Antigravity quota group): how much of the limit was left unused and what that part is worth at the cycle's own cost per 1%. A cycle ends early when a new one starts before the reset, as when a banked Codex reset is redeemed.

**Account affinity** shows, per provider, how many sessions moved to another account and how much cache was written again after the move. CPA keeps a session and model on one account because the prompt cache belongs to that account, so a high share here means paying for cache writes twice.

The breakdown table groups by **Provider**, **Model**, **Client**, **User-Agent**, **Day**, **API key**, **Reasoning effort**, **Account**, or **Failed records** (`errors`). Clicking a provider, model, client, account, or API key filters the view.

A request is one successful upstream call: one per HTTP request, and one per turn on a Responses WebSocket connection. Failed attempts count in totals and cost but not in the per-request medians.

**Plans** opens with recorded API value per account in the selected period. For a plan with several accounts, the chart shows the average, including idle accounts. Choose recorded value per subscription dollar to compare value for money. A return of 10× means the recorded tokens would cost $10 at API rates for each $1 of prorated subscription cost. On realized views, all Antigravity traffic for an account is combined into one whole-subscription row so returns compare fairly against Claude and Codex. On weekly-quota views, Antigravity splits into its two quota groups (`Gemini` and `Claude and GPT`), marked as a shared plan, each projected to 100 weekly quota points.

The recorded chart identifies early weekly resets and the API value served between the first reading after an early reset and the previous deadline. The HTTP 429 chart counts upstream calls by the quota readings on each failure (using response headers for Claude and Codex, and the nearest polled snapshot for Antigravity). It shows where the first matching successful call started on another account within five seconds of the failure ending. Both calls must be in the selected period and share the provider, trace, session, client key and requested model. These observations describe traffic through CPA. They do not establish how long an account was unavailable.

**Limits** switches to the live subscription limits of accounts in CPA: Claude, Codex, Antigravity, Gemini CLI OAuth, Devin, and Kimi, plus CPA status and cooldowns for all other providers. Each window is one bar pooled across accounts, with a bar per account below it when multiple accounts share the window. Accounts are weighted by plan when every plan in the pool is known, and equally otherwise. Next to each bar the page shows whether usage runs ahead of or under an even pace over the window, and how much of the pool the next reset gives back. An account whose weekly limit is spent shows its 5-hour window as blocked until the week resets. Under Claude pools, the page lists extra usage this month against its limit. Under Antigravity pools, it lists available Google One AI credits.

Each weekly pool for Claude, Codex, and every Antigravity upstream bucket (including `Gemini`, `Claude and GPT`, and `Gemini 4 Argon`) also has a chart of its recorded usage over the past 7 days and a projection for the next 7, with each account running at its rate over the last 24 hours and starting over at its reset. Above the chart: the burn over the last 3 and 24 hours, the first account to hit its limit and when the whole pool would. Claude and Codex history comes from response headers, while Antigravity history comes from background polling every 5 minutes. Use outside CPA is not seen between readings.

The page asks CPA to call each provider's usage endpoint with the account's token (`/v8/management/requests/api-call`), so tokens never reach the browser. It checks when the view opens and every 5 minutes. CPA renews Claude and Codex tokens when it proxies a request, not on these calls, so an idle account can show "Token expired" until its next proxied request.

**Requests** lists recent upstream calls in reverse chronological order or sorted by cost, latency, or tokens, filtered by all shared filters plus outcome (`all`, `ok`, `failed`). Expanding a row shows its metadata, token breakdown, and cost without showing prompt or response text.

## Management routes

All routes are `GET`, need the management key (`Authorization: Bearer <key>`), and return JSON.

| Route | Extra parameters |
|---|---|
| `/v0/management/cpa-analytics/options` | distinct providers, models, clients and accounts in the period |
| `/v0/management/cpa-analytics/summary` | |
| `/v0/management/cpa-analytics/timeseries` | `bucket=day\|hour\|minute`, `group=provider\|model\|client` |
| `/v0/management/cpa-analytics/drift` | `group=response_model\|client\|client_version\|reasoning_effort`, `bucket=day\|hour\|minute` |
| `/v0/management/cpa-analytics/breakdown` | `by=provider\|model\|client\|user_agent\|day\|api_key\|reasoning_effort\|account` |
| `/v0/management/cpa-analytics/errors` | failed records grouped by provider, model, account and HTTP status |
| `/v0/management/cpa-analytics/pricing` | |
| `/v0/management/cpa-analytics/limit-burn` | only `from`, `to`, `tz`, `provider` and `account`. `model`, `client` and `api_key` are rejected. |
| `/v0/management/cpa-analytics/limit-leftover` | same as `limit-burn` |
| `/v0/management/cpa-analytics/limit-history` | same as `limit-burn` |
| `/v0/management/cpa-analytics/plan-value` | same as `limit-burn` |
| `/v0/management/cpa-analytics/affinity` | |
| `/v0/management/cpa-analytics/requests` | `sort=recent\|cost\|latency\|tokens`, `outcome=all\|failed\|ok`, `offset`, `limit` (1 to 200, default 50) |
| `/v0/management/cpa-analytics/latency` | |
| `/v0/management/cpa-analytics/health` | none, ignores filters |

Shared filters:

- `from` and `to`: unix milliseconds. The default is the 7 days before now, and the range may not exceed 400 days.
- `provider`, `model` (the reported model), `client` (client name, or `unknown`), `account` (`auth_index`), and `api_key` (the 12-character key hash listed by `breakdown?by=api_key`).
- `tz`: the viewer's offset in minutes east of UTC, from -840 to 840. Day and hour buckets follow it.

Errors come back as `{"error":{"code":"...","message":"..."}}`. `invalid_parameter` (400) names the parameter and retrying will not help. `storage_unavailable` (503) means the database could not be opened or queried, and a retry may succeed.

## What the cost means

Cost is an estimate at public API rates. It is not your bill: subscription plans and negotiated prices are not reflected.

- Prices come from the LiteLLM price table, downloaded through CPA's HTTP client at start and every 24 hours, with `models.dev` and OpenRouter merged as fallbacks for models LiteLLM lacks. All copies are cached in SQLite for offline starts. `price_overrides` win over the catalog.
- Calls on OpenAI's priority and flex tiers use LiteLLM's prices for those tiers. The totals card splits cost by service tier and shows the premium paid over standard rates, which is what turning on a fast mode costs.
- Cost is computed when you query, so a price fix applies to the whole history.
- Tokens with no price are reported as unpriced, never counted as free. The cost quality bar shows how many tokens were priced from the catalog, from overrides, or not at all, and the pricing route lists the unpriced models.

## Development

```sh
make test      # go vet, go test -race and bun test for the Limits math
make lib       # library for this machine, in ~/.cache/cpa-analytics/dist
make build     # linux/amd64 .so built in golang:1.26-bookworm with Docker
make package   # linux/amd64 release zip and checksums.txt
make e2e       # stock CPA v8.0.71 + stub upstream + the plugin, one real request
```

The library needs cgo, so each platform builds on its own OS. `GO_IMAGE` overrides the toolchain image for `make build`. It must stay on Debian bookworm so the library needs no newer glibc than the CPA image ships.

The e2e test clones CPA into `~/.cache/cpa-analytics/CLIProxyAPI` on first run and needs ports 18318 and 18319 free. `PLUGIN_SO=<path> make e2e` tests a prebuilt library instead of a host build.

## Releasing

1. Bump `pluginVersion` in `internal/plugin/plugin.go` and `version` in `registry.json` in the same commit. `TestRegistryMatchesPluginMetadata` fails if they differ.
2. Push a tag `v<version>` on that commit.

The release workflow checks that the tag matches `pluginVersion`, runs the tests, builds `cpa-analytics_<version>_linux_amd64.zip` and `cpa-analytics_<version>_darwin_arm64.zip`, and publishes them with `checksums.txt`. CPA instances that installed from the store see the update within an hour.

## License

MIT
