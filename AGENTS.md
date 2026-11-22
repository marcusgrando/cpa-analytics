# AGENTS.md

CLIProxyAPI (CPA) analytics plugin: a Go `c-shared` library loaded in-process by CPA through its C ABI. The design is in `docs/superpowers/specs/2026-11-14-cpa-analytics-design.md`.

## Decisions

- **No CPA changes.** The plugin must work with stock CPA so CPA upgrades stay routine. An upstream PR that added `TokenBreakdown` and `UserAgent` to the plugin usage record was rejected. Do not reopen it, and do not propose forks or patches to CPA. Everything the plugin needs comes from the existing plugin API: `usage.handle`, the request interceptors, the management routes and resources, and `host.http.do`.
- **Token buckets are a port, not an import.** CPA computes the token breakdown internally but does not pass it to plugins, so `internal/accounting` ports `sdk/cliproxy/usage/accounting.go` from the pinned CPA tag. That file is only CPA's fallback. CPA's Claude executor attaches its own breakdown with thinking inside output, so the port matches that layout before the fallback rules (`claudeThinkingInsideOutput`). Without it every Claude call with thinking is stored as unclassified.
- **The client version comes from a TraceID join.** `request.intercept_before` sees the inbound `User-Agent`, and `usage.handle` sees the tokens. Both carry the same TraceID, and `internal/client` keeps a bounded TTL map between them. A miss is stored as an empty client and shown as `unknown`.
- **Cost is computed at query time** from raw token columns. Never store a cost column.
- **Limits are read through CPA's `api-call`, from the browser; Antigravity quota history is polled by the backend.** The Limits view lists credentials with `GET /v8/management/credentials` and reads each provider's live usage endpoint with `POST /v8/management/requests/api-call`, so OAuth tokens stay in CPA for live checks and each credential's proxy applies. Because Antigravity does not return limit headers on generation responses, the plugin backend polls `retrieveUserQuotaSummary` every 5 minutes (and debounced after Antigravity traffic) via `host.auth.list`, `host.auth.get` and `host.http.do`, storing per-group snapshots in `quota_snapshots`. Claude and Codex tokens are never read by the backend.
- **Limit history comes from response headers (Claude, Codex) and polled `quota_snapshots` (Antigravity).** Claude and Codex report the account's limit usage on every response in `UsageRecord.ResponseHeaders`, parsed by `internal/ratelimit`. Antigravity reports per-model-group 5-hour and weekly buckets only on `retrieveUserQuotaSummary`, so `internal/plugin/antigravity.go` polls and stores them in `quota_snapshots` and `internal/query` keeps Antigravity groups separate in Plans, Burn, Leftover and Limits trends. Response interceptors remain rejected: they receive bodies with no opt-out, run per stream chunk and sit in the delivery path.
- **Codex banked resets are not shown.** The CPA management center already shows the banked reset count and its redeem action, so the plugin would only repeat it. Reopen only if the owner asks.
- **A request is one upstream call**: rows sharing `trace_id` and `requested_at`. A WebSocket connection shares one trace across turns, so never group per-request statistics by trace alone. The reasoning is in the spec under "Unit of a request".

## Guardrails

- The resource route (`/v0/resource/plugins/cpa-analytics/dashboard`) is unauthenticated in CPA. It serves only the static embedded page. Never put data, config or secrets in it.
- The dashboard never writes the management key to `localStorage`. It reads the key the management center saved, only when the center runs on the same origin, and otherwise keeps a typed key in `sessionStorage`.
- The page loads nothing from other origins. Provider usage URLs appear only as `upstreamCall` targets, which CPA fetches server-side. `internal/web/web_test.go` enforces this.
- Limits only read. Never call a route that spends credits or quota, such as Codex `rate-limit-reset-credits/consume` or a chat request used as a probe. `TestIndexReadsLimitsThroughCPA` enforces this.
- The management center floats its own toolbar (refresh, language, theme, logout) over the top-right corner of the page. Keep buttons and selects out of the header and put page controls in the filter row. `TestIndexKeepsHeaderFreeOfControls` enforces this.
- Reject invalid query parameters with `invalid_parameter` naming the parameter. Never clamp or guess.
- Never read or store request or response bodies.

## Upgrading CPA

1. Diff `sdk/cliproxy/usage/accounting.go` and `parseClaudeUsageNode` in `internal/runtime/executor/helps/usage_helpers.go` between the old tag and the new one, and port any rule change into `internal/accounting/accounting.go`. Update the tag named in its package comment.
2. Bump `github.com/router-for-me/CLIProxyAPI/v8` in `go.mod` and run `go mod tidy`.
3. Run `go test ./internal/accounting/`. `TestMatchesCPA` compares the port with CPA's own function on random inputs and fails on any divergence.
4. Update `CPA_TAG` in `test/e2e/run.sh` and run `make e2e`. It proves the TraceID join, the inbound headers and the upstream limit headers against the real host.
5. Check the plugin ABI methods and the `pluginapi` request and response types for changes.
6. Diff the limit headers CPA reads in `sdk/cliproxy/auth/selector.go` (Claude) and `internal/runtime/executor/helps/codex_quota.go` (Codex), and update `internal/ratelimit` to match. `make e2e` checks that response headers still reach `usage.handle`.
7. Copy the Antigravity Hub User-Agent the new tag falls back to (`antigravityFallbackVersion` and its changelist in `internal/misc/antigravity_version.go`) into `ANTIGRAVITY_HEADERS` in `internal/web/index.html` and `antigravityUserAgent` in `internal/plugin/antigravity.go`.

## Testing

- `make test` runs `go vet`, `go test -race ./...` and `bun test internal/web`. Run it before every commit.
- The Limits math lives in `internal/web/limits.js`, a plain script with no DOM so bun tests it directly. `web.go` inlines it into the page at the `/* limits.js */` placeholder.
- `make e2e` builds stock CPA and the plugin, sends one proxied request, and checks the recorded row through the management routes.
- Prefer functional tests that drive the plugin through its ABI methods with a real SQLite file (see `call` in `internal/plugin/usage_test.go`) over unit tests of internals.
