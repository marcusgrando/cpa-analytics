# AGENTS.md

CLIProxyAPI (CPA) analytics plugin: a Go `c-shared` library loaded in-process by CPA through its C ABI. The design is in `docs/superpowers/specs/2026-11-14-cpa-analytics-design.md`.

## Decisions

- **No CPA changes.** The plugin must work with stock CPA so CPA upgrades stay routine. An upstream PR that added `TokenBreakdown` and `UserAgent` to the plugin usage record was rejected. Do not reopen it, and do not propose forks or patches to CPA. Everything the plugin needs comes from the existing plugin API: `usage.handle`, the request interceptors, the management routes and resources, `host.http.do`, `host.auth.list`, and `host.auth.get`.
- **Token buckets are a port, not an import.** CPA computes the token breakdown internally but does not pass it to plugins, so `internal/accounting` ports `sdk/cliproxy/usage/accounting.go` from the pinned CPA tag. That file is only CPA's fallback. CPA's Claude executor attaches its own breakdown with thinking inside output, so the port matches that layout before the fallback rules (`claudeThinkingInsideOutput`). Without it every Claude call with thinking is stored as unclassified.
- **The client version comes from a TraceID join.** `request.intercept_before` sees the inbound `User-Agent`, and `usage.handle` sees the tokens. Both carry the same TraceID, and `internal/client` keeps a bounded TTL map between them. A miss is stored as an empty client and shown as `unknown`.
- **Cost is computed at query time** from raw token columns. Never store a cost column.
- **Limits are read through CPA's `api-call`, from the browser. Antigravity quota history is polled by the backend.** The Limits view lists credentials with `GET /v8/management/credentials` and reads each provider's live usage endpoint with `POST /v8/management/requests/api-call`, so OAuth tokens stay in CPA for live checks and each credential's proxy applies. Because Antigravity does not return limit headers on generation responses, the plugin backend polls `retrieveUserQuotaSummary` every 5 minutes (and debounced after Antigravity traffic) via `host.auth.list`, `host.auth.get`, and `host.http.do`, storing per-group snapshots in `quota_snapshots`. Claude and Codex tokens are never read by the backend. Do not add OAuth token refresh to the plugin because CPA proactively refreshes Antigravity tokens in the background.
- **Antigravity quota grouping.** Limits shows every real upstream bucket Google returns (including `Gemini 4 Argon`) with its own bar and weekly trend chart. Plans, Burn, Leftover, and 429 classification keep only two Antigravity groups: `Gemini` (unifying all Gemini models, including Argon) and `Claude and GPT`. Not per-Gemini-model rows, because all Gemini usage on an Antigravity account shares the main Gemini bucket while third-party Claude/GPT models use a separate quota.
- **Plans subscription fairness.** Realized Plans views (recorded API value per account, realized return per subscription dollar, and realized tokens per dollar) combine all Antigravity groups of an account into one whole-subscription row so multiples compare fairly against Claude and Codex. Weekly-quota Plans views (potential return, full-cycle USD, and tokens per weekly quota point) show separate rows per quota group marked as a shared plan.
- **Limit history comes from response headers (Claude, Codex) and polled `quota_snapshots` (Antigravity).** Claude and Codex report the account's limit usage on every response in `UsageRecord.ResponseHeaders`, parsed by `internal/ratelimit`. Response interceptors remain rejected: they receive bodies with no opt-out, run per stream chunk and sit in the delivery path.
- **Codex banked resets are not shown.** The CPA management center already shows the banked reset count and its redeem action, so the plugin would only repeat it. Reopen only if the owner asks.
- **A request is one upstream call**: rows sharing `trace_id` and `requested_at`. A WebSocket connection shares one trace across turns, so never group per-request statistics by trace alone. The reasoning is in the spec under "Unit of a request".

## Guardrails

- The resource route (`/v0/resource/plugins/cpa-analytics/dashboard`) is unauthenticated in CPA. It serves only the static embedded page. Never put data, config or secrets in it.
- The dashboard never writes the management key to `localStorage`. It reads the key the management center saved, only when the center runs on the same origin, and otherwise keeps a typed key in `sessionStorage`.
- The page loads nothing from other origins. Provider usage URLs appear only as `upstreamCall` targets, which CPA fetches server-side. `internal/web/web_test.go` enforces this.
- Limits only read. Never call a route that spends credits or quota, such as Codex `rate-limit-reset-credits/consume` or a chat request used as a probe. `TestIndexReadsLimitsThroughCPA` enforces this.
- The management center floats its own toolbar (refresh, language, theme, logout) over the top-right corner of the page. Keep buttons and selects out of the header and put page controls in `.nav-bar` and `.filters`. `TestIndexKeepsHeaderFreeOfControls` enforces this.
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
- Frontend logic lives in DOM-free scripts under `internal/web/` (`limits.js`, `compare.js`, `colors.js`, `tooltip.js`, `sparkline.js`, `endlabels.js`, `scatter.js`, `drift.js`, `plans.js`) tested directly with bun and inlined into `index.html` by `web.go`.
- `make e2e` builds stock CPA and the plugin, sends one proxied request, and checks the recorded row through the management routes.
- Prefer functional tests that drive the plugin through its ABI methods with a real SQLite file (see `call` in `internal/plugin/usage_test.go`) over unit tests of internals.
