# cpa-analytics

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that records token usage per request and shows it in a dashboard inside the CPA management center. It answers one question first: when input or output tokens per request go up, was it your client upgrade or the model and provider?

It runs in-process as a CPA C ABI plugin and needs no CPA changes. It is built and tested against CPA v8.0.71, and released for Linux x86_64 and macOS arm64.

## What it records

One row per upstream call, stored in SQLite:

- Provider, requested model, the model the upstream reported, and the service tier.
- Tokens split into uncached input, cache read, cache write, output, reasoning, and unclassified. The split uses the same rules CPA applies internally.
- The client name and version, parsed from the inbound `User-Agent`: `codex`, `claude-code`, `t3code` and `opencode` are recognized. Anything else is `other`.
- A hash of the client API key, never the key itself.
- For Claude and Codex subscription accounts, the 5-hour and weekly limit usage the provider reported in the response headers, and which credential made the call.

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
      retention_days: 180        # 0 keeps everything. Pruning runs at start, on reconfigure and then daily.
      pricing_url: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
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

The page reloads its data every 60 seconds while the tab is visible. **Auto refresh** in the filter row turns it off for that tab.

The totals show how each figure changed against the previous period of the same length, once the plugin has recorded through that whole period.

The drift chart is the main view. It plots median or p90 input (including cache by default) and output tokens per request, grouped by reported model or by client version, and draws a marker on the first day a new model or client version appears. A jump that lines up with a client version marker points at your change. A jump that lines up with a model marker, or with no marker at all, points at the provider. Grouping by reasoning effort catches a client that raised its default effort: output and reasoning go up with no model change.

The same chart plots median or p90 latency and time to first token per request. Time to first token covers streamed calls only, since CPA reports none for the others.

**Value of the weekly limit** plots, per day and account, the API-priced cost of each 1% of the account's weekly limit. For the same kind of work, a drop means the provider tightened the limit. Days on which an account used less than 1% of its week are left out. Snapshots start with plugin 0.1.11, so the chart fills from the day you install it.

A request is one successful upstream call: one per HTTP request, and one per turn on a Responses WebSocket connection. Failed attempts count in totals and cost but not in the per-request medians.

**Limits** in the filter row switches to the subscription limits of the Claude and Codex accounts in CPA. Each window (5-hour, weekly, weekly per model) is one bar pooled across the accounts, with a bar per account below it showing that account against its own limit. Accounts are weighted by plan (Pro 1, Max 5x 5, Max 20x 20; Codex Plus 1, Pro Lite 5, Pro 20) when every plan in the pool is known, and equally otherwise. Next to each bar the page shows whether usage runs ahead of or under an even pace over the window, and how much of the pool the next reset gives back. An account whose weekly limit is spent shows its 5-hour window as blocked until the week resets. Under the Claude pools, the page lists each account's extra usage this month against its limit.

The page asks CPA to call each provider's usage endpoint with the account's token (`/v8/management/requests/api-call`), so tokens never reach the browser. It checks when the view opens and every 5 minutes. CPA renews Claude and Codex tokens when it proxies a request, not on these calls, so an idle account can show "Token expired" until its next proxied request.

## Management routes

All routes are `GET`, need the management key (`Authorization: Bearer <key>`), and return JSON.

| Route | Extra parameters |
|---|---|
| `/v0/management/cpa-analytics/summary` | |
| `/v0/management/cpa-analytics/timeseries` | `bucket=day\|hour`, `group=provider\|model\|client` |
| `/v0/management/cpa-analytics/drift` | `group=response_model\|client_version\|reasoning_effort` |
| `/v0/management/cpa-analytics/breakdown` | `by=model\|client\|day\|api_key\|reasoning_effort` |
| `/v0/management/cpa-analytics/pricing` | |
| `/v0/management/cpa-analytics/limit-burn` | only `from`, `to`, `tz` and `provider`. A weekly limit covers the whole account, so `model`, `client` and `api_key` are rejected. |
| `/v0/management/cpa-analytics/health` | none, ignores filters |

Shared filters:

- `from` and `to`: unix milliseconds. The default is the 7 days before now, and the range may not exceed 400 days.
- `provider`, `model` (the reported model), `client` (client name), and `api_key` (the 12-character key hash listed by `breakdown?by=api_key`).
- `tz`: the viewer's offset in minutes east of UTC, from -840 to 840. Day and hour buckets follow it.

Errors come back as `{"error":{"code":"...","message":"..."}}`. `invalid_parameter` (400) names the parameter and retrying will not help. `storage_unavailable` (503) means the database could not be opened or queried, and a retry may succeed.

## What the cost means

Cost is an estimate at public API rates. It is not your bill: subscription plans and negotiated prices are not reflected.

- Prices come from the LiteLLM price table, downloaded through CPA's HTTP client at start and every 24 hours, and cached in the database for offline starts. `price_overrides` win over the table.
- Calls on OpenAI's priority and flex tiers use LiteLLM's prices for those tiers. The totals card splits cost by service tier and shows the premium paid over standard rates, which is what turning on a fast mode costs.
- Cost is computed when you query, so a price fix applies to the whole history.
- Tokens with no price are reported as unpriced, never counted as free. The cost quality bar shows how many tokens were priced from LiteLLM, from overrides, or not at all, and the pricing route lists the unpriced models.

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
