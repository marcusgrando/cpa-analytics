# cpa-analytics

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that records token usage per request and shows it in a dashboard inside the CPA management center. It answers one question first: when input or output tokens per request go up, was it your client upgrade or the model and provider?

It runs in-process as a CPA C ABI plugin and needs no CPA changes. It is built and tested against CPA v8.0.71.

## What it records

One row per upstream call, stored in SQLite:

- Provider, requested model, the model the upstream reported, and the service tier.
- Tokens split into uncached input, cache read, cache write, output, reasoning, and unclassified. The split uses the same rules CPA applies internally.
- The client name and version, parsed from the inbound `User-Agent`: `codex`, `claude-code`, `t3code` and `opencode` are recognized. Anything else is `other`.
- A hash of the client API key, never the key itself.

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

From the plugin store: add this repository's `registry.json` to `plugins.store-sources`, then install `cpa-analytics` from the management center.

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/marcus-grando-azion/cpa-analytics/main/registry.json"
```

By hand: download `cpa-analytics_<version>_linux_amd64.zip` from a release, check it against `checksums.txt`, and unzip `cpa-analytics.so` into `plugins.dir`.

### 3. Configure it

Everything under `plugins.configs.cpa-analytics` is optional:

```yaml
plugins:
  configs:
    cpa-analytics:
      enabled: true
      db_path: "/CLIProxyAPI/data/cpa-analytics.db"
      retention_days: 180        # 0 keeps everything. Pruning runs at start and then daily.
      pricing_url: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
      price_overrides:           # USD per one million tokens
        my-internal-model:
          input: 1.25
          output: 10
          cache_read: 0.125
          cache_write: 0
```

Invalid values (negative retention or prices, a non-http(s) `pricing_url`) make registration fail with an error naming the field.

## Dashboard

Open the management center and pick **Analytics** in the menu. The page is also served directly at:

```
http://<cpa-host>:<port>/v0/resource/plugins/cpa-analytics/dashboard
```

The page itself holds no data. It reads every number from the management routes below with your management key. Inside the management center it reuses the key the center stored, when you enabled "remember password". Otherwise it asks for the key and keeps it in `sessionStorage` for that tab only.

The drift chart is the main view. It plots median or p90 input and output tokens per inbound request, grouped by reported model or by client version, and draws a marker on the first day a new model or client version appears. A jump that lines up with a client version marker points at your change. A jump that lines up with a model marker, or with no marker at all, points at the provider.

## Management routes

All routes are `GET`, need the management key (`Authorization: Bearer <key>`), and return JSON.

| Route | Extra parameters |
|---|---|
| `/v0/management/cpa-analytics/summary` | |
| `/v0/management/cpa-analytics/timeseries` | `bucket=day\|hour`, `group=provider\|model\|client` |
| `/v0/management/cpa-analytics/drift` | `group=response_model\|client_version` |
| `/v0/management/cpa-analytics/breakdown` | `by=model\|client\|day` |
| `/v0/management/cpa-analytics/pricing` | |
| `/v0/management/cpa-analytics/health` | none, ignores filters |

Shared filters:

- `from` and `to`: unix milliseconds. The default is the 7 days before now, and the range may not exceed 400 days.
- `provider`, `model` (the reported model), `client` (client name), and `api_key` (key hash).
- `tz`: the viewer's offset in minutes east of UTC, from -840 to 840. Day and hour buckets follow it.

Errors come back as `{"error":{"code":"...","message":"..."}}`. `invalid_parameter` (400) names the parameter and retrying will not help. `storage_unavailable` (503) means the database could not be opened or queried, and a retry may succeed.

## What the cost means

Cost is an estimate at public API rates. It is not your bill: subscription plans and negotiated prices are not reflected.

- Prices come from the LiteLLM price table, downloaded through CPA's HTTP client at start and every 24 hours, and cached in the database for offline starts. `price_overrides` win over the table.
- Cost is computed when you query, so a price fix applies to the whole history.
- Tokens with no price are reported as unpriced, never counted as free. The cost quality bar shows how many tokens were priced from LiteLLM, from overrides, or not at all, and the pricing route lists the unpriced models.

## Development

```sh
make test      # go vet and go test -race
make build     # linux/amd64 .so built in golang:1.26-bookworm, output in ~/.cache/cpa-analytics/dist
make package   # release zip and checksums.txt for the plugin store
make e2e       # stock CPA v8.0.71 + stub upstream + the plugin, one real request
```

`make build GOARCH=arm64` builds for arm64. `GO_IMAGE` overrides the toolchain image. The image must stay on Debian bookworm so the library needs no newer glibc than the CPA image ships.

The e2e test clones CPA into `~/.cache/cpa-analytics/CLIProxyAPI` on first run and needs ports 18318 and 18319 free. `PLUGIN_SO=<path> make e2e` tests a prebuilt library instead of a host build.

A release tag is `v<version>` and carries `cpa-analytics_<version>_linux_<arch>.zip` and `checksums.txt`, the layout the CPA plugin store installs from.
