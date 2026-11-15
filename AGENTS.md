# AGENTS.md

CLIProxyAPI (CPA) analytics plugin: a Go `c-shared` library loaded in-process by CPA through its C ABI. The design is in `docs/superpowers/specs/2026-11-14-cpa-analytics-design.md`.

## Decisions

- **No CPA changes.** The plugin must work with stock CPA so CPA upgrades stay routine. An upstream PR that added `TokenBreakdown` and `UserAgent` to the plugin usage record was rejected. Do not reopen it, and do not propose forks or patches to CPA. Everything the plugin needs comes from the existing plugin API: `usage.handle`, the request interceptors, the management routes and resources, and `host.http.do`.
- **Token buckets are a port, not an import.** CPA computes the token breakdown internally but does not pass it to plugins, so `internal/accounting` ports `sdk/cliproxy/usage/accounting.go` from the pinned CPA tag.
- **The client version comes from a TraceID join.** `request.intercept_before` sees the inbound `User-Agent`, and `usage.handle` sees the tokens. Both carry the same TraceID, and `internal/client` keeps a bounded TTL map between them. A miss is stored as an empty client and shown as `unknown`.
- **Cost is computed at query time** from raw token columns. Never store a cost column.

## Guardrails

- The resource route (`/v0/resource/plugins/cpa-analytics/dashboard`) is unauthenticated in CPA. It serves only the static embedded page. Never put data, config or secrets in it.
- The dashboard never writes the management key to `localStorage`. It reads the key the management center saved, only when the center runs on the same origin, and otherwise keeps a typed key in `sessionStorage`.
- The page loads nothing from other origins. `internal/web/web_test.go` enforces this.
- Reject invalid query parameters with `invalid_parameter` naming the parameter. Never clamp or guess.
- Never read or store request or response bodies.

## Upgrading CPA

1. Diff `sdk/cliproxy/usage/accounting.go` between the old tag and the new one, and port any rule change into `internal/accounting/accounting.go`. Update the tag named in its package comment.
2. Bump `github.com/router-for-me/CLIProxyAPI/v8` in `go.mod` and run `go mod tidy`.
3. Run `go test ./internal/accounting/`. `TestMatchesCPA` compares the port with CPA's own function on random inputs and fails on any divergence.
4. Update `CPA_TAG` in `test/e2e/run.sh` and run `make e2e`. It proves the TraceID join and the inbound headers against the real host.
5. Check the plugin ABI methods and the `pluginapi` request and response types for changes.

## Testing

- `make test` runs `go vet` and `go test -race ./...`. Run it before every commit.
- `make e2e` builds stock CPA and the plugin, sends one proxied request, and checks the recorded row through the management routes.
- Prefer functional tests that drive the plugin through its ABI methods with a real SQLite file (see `call` in `internal/plugin/usage_test.go`) over unit tests of internals.
