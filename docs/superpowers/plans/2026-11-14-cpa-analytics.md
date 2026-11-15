# cpa-analytics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A CLIProxyAPI v8.0.71 plugin that records every request's token usage in SQLite and serves a usage, cost and per-request drift dashboard inside CPA.

**Architecture:** One Go `c-shared` library. A thin cgo shim (`main.go`) decodes the C ABI calls and hands them to a pure-Go dispatcher (`internal/plugin`), which the tests drive directly. Usage records become rows through an async batching writer. Reads go through query functions behind exact-path management routes. The dashboard is one embedded HTML file.

**Tech Stack:** Go 1.26+ (`go 1.26` in go.mod), `github.com/router-for-me/CLIProxyAPI/v8 v8.0.71` (only `sdk/pluginabi` and `sdk/pluginapi`), `modernc.org/sqlite`, `gopkg.in/yaml.v3`. Vanilla JS + SVG for the page.

**Spec:** `docs/superpowers/specs/2026-11-14-cpa-analytics-design.md`

## Global Constraints

- Target host CPA `v8.0.71`, plugin ABI 1, registration `schema_version` = `pluginabi.SchemaVersion` (10). Refuse to register when the host sends `schema_version < 7` (header-only interceptor needs 7).
- Module path `github.com/marcusgrando/cpa-analytics`. Plugin file `cpa-analytics.so`, so the plugin ID is `cpa-analytics`.
- Wire format is JSON of `pluginapi` structs as the host marshals them. Decode into the `pluginapi` types; never hand-write field names.
- Never store request or response bodies, failure bodies or raw API keys. `api_key_hash` = first 12 hex chars of SHA-256 of `APIKey`, empty when `APIKey` is empty.
- Every exported ABI entry point recovers panics and returns an error envelope.
- Usage handling never blocks on SQLite. Channel capacity 10,000, batch every 1s or 500 rows.
- Management routes live under `/v0/management/cpa-analytics/`. The resource page lives at `/v0/resource/plugins/cpa-analytics/` with menu label `Analytics`.
- Caller errors return 400 `{"error":{"code":"invalid_parameter","message":"..."}}`. Database faults return 503 with code `storage_unavailable`.
- Config fields under `plugins.configs.cpa-analytics`: `db_path` (default `/CLIProxyAPI/data/cpa-analytics.db`), `retention_days` (default 0), `price_overrides` (map model -> `{input, output, cache_read, cache_write}` USD per 1M tokens), `pricing_url` (default `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json`).
- Interceptor map TTL 10 minutes, capacity 50,000 entries (oldest evicted).
- Pricing refresh at startup and every 24h. Last good table stored in SQLite.
- Code and comments in English. Commits in Conventional Commits with a body.

## Review Focus

1. Usage record with `TraceID` that the interceptor never saw (WebSocket, host model callbacks): row is stored with empty client fields, not dropped.
2. Same `RequestID` delivered twice (host replay after plugin reload): one row, no error.
3. Model name absent from LiteLLM (`gpt-5.6-sol`, aliases with dates or `-thinking` suffix): counted as unpriced, never priced at zero.
4. `db_path` directory missing or read-only: plugin registers, health reports the error, CPA keeps serving.
5. Query params `from > to`, non-numeric, unknown `group`: 400 naming the parameter.

---

### Task 1: Module, ABI shim and registration

**Files:**
- Create: `go.mod`, `main.go` (cgo shim, copied in shape from CPA `examples/plugin/request-lifecycle/go/main.go`, plus storing the host API and a `callHost(method string, req []byte) ([]byte, error)` C bridge)
- Create: `internal/plugin/plugin.go`, `internal/plugin/config.go`
- Test: `internal/plugin/plugin_test.go`

**Interfaces:**
- Produces: `type Host interface { Call(method string, req []byte) ([]byte, error) }`; `func New(host Host) *Plugin`; `func (p *Plugin) Handle(method string, req []byte) []byte` (always returns an envelope, recovers panics); `type Config struct { DBPath string; RetentionDays int; PriceOverrides map[string]PriceOverride; PricingURL string }`; `func ParseConfig(yaml []byte) (Config, error)`.

- [ ] Step 1: Tests `TestRegisterDeclaresCapabilities` (envelope ok, `capabilities.usage_plugin`, `request_interceptor`, `request_interceptor_omit_body_before`, `management_api` all true, `metadata.Name == "cpa-analytics"`, four ConfigFields), `TestRegisterRejectsOldHostSchema` (schema_version 6 -> envelope ok=false), `TestParseConfigRejectsNegativeRetention`, `TestHandleUnknownMethod`, `TestHandleRecoversPanic` (a test-only method that panics yields ok=false).
- [ ] Step 2: Run `go test ./internal/plugin/...`, expect FAIL.
- [ ] Step 3: Implement. `plugin.reconfigure` re-applies config the same way as register.
- [ ] Step 4: `go test ./... && go vet ./... && CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/x.so .` all pass.
- [ ] Step 5: Commit `feat: scaffold cpa-analytics plugin registration`.

### Task 2: Token accounting port

**Files:** Create `internal/accounting/accounting.go`, test `internal/accounting/accounting_test.go`.

**Interfaces:**
- Produces: `type Breakdown struct { Uncached, CacheRead, CacheWrite, OutputNonReasoning, Reasoning, Unclassified, Total int64; Quality string }`; `func Normalize(provider, executorType string, d pluginapi.UsageDetail) Breakdown`.

Port `tokenAccountingSemanticsFor`, the four `New*TokenBreakdown` builders, `EnsureTokenBreakdownForProvider`'s cached-tokens fallback and `tokenBreakdownForSemantics` from CPA `sdk/cliproxy/usage/accounting.go` at v8.0.71, with a header comment naming that file and tag.

- [ ] Step 1: Table test `TestNormalize` covering: codex subset (input 1000, cache read 800, output 200, reasoning 50 -> uncached 200, nonreasoning 150); claude independent (input 100, cache read 900, cache write 50, output 300 -> uncached 100, total 1350); gemini separate reasoning (input 500, cache read 100, output 40, reasoning 60 -> output total 100); unknown provider -> quality `unclassified`; inconsistent (cache read > input on subset) -> quality `inconsistent`.
- [ ] Step 2-4: fail, implement, pass.
- [ ] Step 5: Commit `feat: port CPA token accounting rules`.

### Task 3: Client identification

**Files:** Create `internal/client/useragent.go`, `internal/client/traces.go`, tests alongside.

**Interfaces:**
- Produces: `func Parse(ua string) (name, version string)`; `type Traces struct`; `func NewTraces(ttl time.Duration, capacity int, now func() time.Time) *Traces`; `(*Traces).Put(traceID, ua string)`; `(*Traces).Take(traceID string) (string, bool)` (does not delete, since one trace can have several attempts); `(*Traces).Stats() (hits, misses uint64)`.

- [ ] Step 1: Tests: `codex_cli_rs/0.130.0 (...)` -> `codex`, `0.130.0`; `claude-cli/2.3.1 (external, cli)` -> `claude-code`, `2.3.1`; `t3code/...` -> `t3code`; `opencode/...` -> `opencode`; `curl/8.5.0` -> `other`, ``; empty -> ``, ``. Traces: entry expires after TTL, capacity evicts oldest, Take counts hits and misses.
- [ ] Step 2-4: fail, implement, pass.
- [ ] Step 5: Commit `feat: parse client user agents and keep trace map`.

### Task 4: Storage and writer

**Files:** Create `internal/store/store.go`, `internal/store/migrations/001_init.sql` (embedded), `internal/store/writer.go`, tests alongside.

**Interfaces:**
- Produces: `type Row struct` (one field per `requests` column in the spec, times as unix ms); `func Open(path string) (*Store, error)` (creates parent dir, WAL, applies migrations); `(*Store).InsertBatch([]Row) error` (`INSERT OR IGNORE` on `request_id`); `(*Store).DB() *sql.DB`; `(*Store).Prune(before time.Time) (int64, error)`; `(*Store).SaveKV(key string, val []byte) error` / `LoadKV(key string) ([]byte, error)` (table `kv`, used for pricing); `type Writer`; `func NewWriter(s *Store, capacity int, flushEvery time.Duration, batch int, logf func(string, ...any)) *Writer`; `(*Writer).Enqueue(Row) bool`; `(*Writer).Stats() WriterStats{Written, Dropped uint64; LastError string; LastErrorAt time.Time}`; `(*Writer).Close()`.

- [ ] Step 1: Tests: open in `t.TempDir()`, insert two rows, re-insert one (count stays 2); Enqueue on a full channel returns false and increments Dropped; a writer over a closed store records LastError and keeps accepting; Prune removes old rows; Open on a path under a read-only dir returns an error.
- [ ] Step 2-4: fail, implement, pass. Indexes on `requested_at`, `(model, requested_at)`, `(client_name, client_version, requested_at)`, `trace_id`.
- [ ] Step 5: Commit `feat: add sqlite store and async writer`.

### Task 5: Usage and interceptor handlers

**Files:** Modify `internal/plugin/plugin.go`; create `internal/plugin/usage.go`; test `internal/plugin/usage_test.go`.

**Interfaces:**
- Consumes: Tasks 2-4.
- Produces: `func RowFromRecord(rec pluginapi.UsageRecord, ua string) store.Row`.

`request.intercept_before` stores `TraceID -> Headers.Get("User-Agent")` and returns `{}`. `request.intercept_after` returns `{}`. `usage.handle` builds the row and enqueues it. If the store failed to open, `usage.handle` returns ok and counts the record as dropped.

- [ ] Step 1: Functional tests through `Plugin.Handle`: intercept then usage with the same TraceID -> row has `client_name=codex`; usage with unknown TraceID -> row with empty client; same RequestID twice -> one row; APIKey `sk-abc` stored as 12-hex hash, never raw (scan the DB file bytes for `sk-abc`); failed record keeps `status_code` and no body.
- [ ] Step 2-4: fail, implement, pass.
- [ ] Step 5: Commit `feat: record usage rows with client identity`.

### Task 6: Pricing

**Files:** Create `internal/pricing/table.go`, `internal/pricing/refresh.go`, `internal/pricing/testdata/litellm_sample.json`, tests alongside.

**Interfaces:**
- Produces: `type Rates struct { Input, Output, CacheRead, CacheWrite float64 }` (USD per token); `type Table struct`; `func ParseLiteLLM(raw []byte) (*Table, error)`; `(*Table).Lookup(model, tier string) (Rates, Source, bool)` with `Source` in `{"litellm","override"}`; `func WithOverrides(t *Table, o map[string]plugin.PriceOverride) *Table` (overrides in per-1M units); `func Cost(r Rates, b accounting.Breakdown) float64` (reasoning priced as output); `type Refresher`; `func NewRefresher(fetch func(url string) ([]byte, error), kv KV, url string, every time.Duration) *Refresher`; `(*Refresher).Current() (*Table, time.Time)`.

Matching follows T3 Code `apps/server/src/usage/usagePricing.ts`: lowercase, strip provider prefix (`openai/`, `anthropic/`...), strip date suffix (`-20251001`, `-2025-10-01`), strip `-thinking`/`-high` style variant suffixes, prefer first-party entries for bare names. Tier `priority` uses `_priority` keys when present.

- [ ] Step 1: Tests with the sample fixture: `claude-opus-4-8` and `anthropic/claude-opus-4-8-20260101` resolve to the same rates; `gpt-5.5` with tier `priority` uses priority rates; unknown model -> ok=false; override wins over LiteLLM; Cost of a known breakdown equals a hand-computed float within 1e-9; refresher with failing fetch serves the KV copy; with no copy, `Current()` returns an empty table.
- [ ] Step 2-4: fail, implement, pass. The host fetch calls `host.http.do` with `{"Method":"GET","URL":url}` and requires `StatusCode == 200`.
- [ ] Step 5: Commit `feat: price tokens from LiteLLM rates`.

### Task 7: Queries and management API

**Files:** Create `internal/query/query.go`, `internal/query/drift.go`, `internal/plugin/management.go`; tests `internal/query/query_test.go`, `internal/plugin/management_test.go`.

**Interfaces:**
- Produces: `type Filter struct { From, To time.Time; Provider, Model, Client, APIKeyHash string }`; `func ParseFilter(q url.Values, now time.Time) (Filter, error)` (default last 7 days, `from < to`, max 400 days); `Summary`, `Timeseries(bucket, group)`, `Drift(group)`, `Breakdown(by)` returning JSON-ready structs, each taking `(*sql.DB, Filter, *pricing.Table)`.

Drift: per inbound request = rows grouped by `trace_id` (rows with empty trace use `request_id`), excluding `generate = 0`; per day and group compute median and p90 of uncached, cache read, cache write, output, reasoning, and cache hit rate = sum(cache_read)/sum(input total). Markers: first day each `client_name/client_version` or `response_model` value appears within the filter range, plus whether it appeared before `From` (no marker then).

Routes (all GET): `summary`, `timeseries`, `drift`, `breakdown`, `health`, `pricing`.

- [ ] Step 1: Tests seed rows across 3 days with a client version change on day 2 and a response model change on day 3; assert drift medians, markers on exactly those days, summary totals and cost, 400 on `from=abc`, `from>to`, `group=nope`; 503 when the DB is closed.
- [ ] Step 2-4: fail, implement, pass.
- [ ] Step 5: Commit `feat: serve analytics through management routes`.

### Task 8: Dashboard page

**Files:** Create `internal/web/index.html` (embedded via `internal/web/web.go`), modify `internal/plugin/management.go` to register the resource; test `internal/web/web_test.go`.

Page asks for the management key once and keeps it in `sessionStorage`, sends it as `Authorization: Bearer <key>`. Views and filters as in the spec's Dashboard section, light and dark themes, works at 375px width.

- [ ] Step 1: Tests: resource handler returns 200, `text/html; charset=utf-8`, body contains `id="drift"`; the page has no `http://` or `https://` script or stylesheet references.
- [ ] Step 2-4: fail, implement, pass. Manual check in a browser against seeded data.
- [ ] Step 5: Commit `feat: add analytics dashboard page`.

### Task 9: Build, end-to-end smoke and docs

**Files:** Create `Makefile` (`test`, `build` via `docker run golang:1.26-bookworm` producing `dist/cpa-analytics.so`), `test/e2e/run.sh`, `README.md`, `AGENTS.md`, `registry.json` template.

- [ ] Step 1: e2e script: build CPA v8.0.71 from `~/.cache/cpa-analytics/CLIProxyAPI` into `~/.cache/cpa-analytics/build/`, start a stub OpenAI-compatible upstream returning fixed usage, start CPA with the plugin in its `plugins.dir`, send one request with `User-Agent: codex_cli_rs/0.130.0`, poll `summary` with the management key until one request appears, assert `client_version == 0.130.0` via `breakdown?by=client`.
- [ ] Step 2: Run it; it is the proof for the TraceID join and the inbound headers claim in the spec.
- [ ] Step 3: README covers install (volume, config YAML, store-sources), dashboard URL, and what cost means. AGENTS.md records the no-CPA-changes decision and how to refresh the accounting port.
- [ ] Step 4: Commit `chore: add build, e2e smoke and docs`.
