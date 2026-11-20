#!/usr/bin/env bash
# End-to-end smoke test: stock CLIProxyAPI v8.0.71 loads the plugin, proxies
# one request to a stub upstream, and the plugin records it with the client
# version taken from the inbound User-Agent. Proves the TraceID join and the
# host.http.do price download against a real host.
#
# PLUGIN_SO=path tests a prebuilt library (for example the output of make build).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CACHE=${CPA_ANALYTICS_CACHE:-$HOME/.cache/cpa-analytics}
CPA_SRC=${CPA_SRC:-$CACHE/CLIProxyAPI}
CPA_TAG=v8.0.71
WORK=$CACHE/build/e2e
CPA_PORT=${CPA_PORT:-18318}
STUB_PORT=${STUB_PORT:-18319}
MGMT_KEY=e2e-management-key
CLIENT_KEY=e2e-client-key
API=http://127.0.0.1:$CPA_PORT/v0/management/cpa-analytics

fail() { echo "FAIL: $*" >&2; echo "--- cpa log ---" >&2; tail -40 "$WORK/cpa.log" >&2 || true; exit 1; }

if [ ! -d "$CPA_SRC/.git" ]; then
  git clone --quiet --depth 1 --branch "$CPA_TAG" https://github.com/router-for-me/CLIProxyAPI "$CPA_SRC"
fi
[ "$(git -C "$CPA_SRC" describe --tags)" = "$CPA_TAG" ] || fail "$CPA_SRC is not at $CPA_TAG"

for port in "$CPA_PORT" "$STUB_PORT"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "FAIL: port $port is already in use, stop the process holding it" >&2
    exit 1
  fi
done

rm -rf "$WORK"
mkdir -p "$WORK/plugins" "$WORK/data"
echo "building CLIProxyAPI $CPA_TAG"
(cd "$CPA_SRC" && CGO_ENABLED=1 go build -buildvcs=false -o "$WORK/cpa" ./cmd/server)
if [ -n "${PLUGIN_SO:-}" ]; then
  echo "using prebuilt plugin $PLUGIN_SO"
  cp "$PLUGIN_SO" "$WORK/plugins/cpa-analytics.so"
else
  echo "building plugin"
  (cd "$ROOT" && CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o "$WORK/plugins/cpa-analytics.so" .)
fi

cat > "$WORK/config.yaml" <<YAML
config-version: 8
server:
  host: "127.0.0.1"
  port: $CPA_PORT
management:
  secret-key: "$MGMT_KEY"
access:
  api-keys: ["$CLIENT_KEY"]
api-keys:
  openai-compatibility:
    - name: "stub"
      base-url: "http://127.0.0.1:$STUB_PORT/v1"
      keys:
        - api-key: "stub-key"
      models:
        - name: "stub-model"
          alias: "stub-model"
plugins:
  enabled: true
  dir: "$WORK/plugins"
  configs:
    cpa-analytics:
      enabled: true
      db_path: "$WORK/data/cpa-analytics.db"
      pricing_url: "http://127.0.0.1:$STUB_PORT/prices.json"
YAML

python3 "$ROOT/test/e2e/stub_upstream.py" "$STUB_PORT" 2>"$WORK/stub.log" &
STUB_PID=$!
(cd "$WORK" && exec ./cpa -config "$WORK/config.yaml" >"$WORK/cpa.log" 2>&1) &
CPA_PID=$!
trap 'kill $CPA_PID $STUB_PID 2>/dev/null || true' EXIT

mgmt() { curl -fsS -H "Authorization: Bearer $MGMT_KEY" "$API/$1"; }

for _ in $(seq 1 60); do
  mgmt health >/dev/null 2>&1 && break
  sleep 0.5
done
mgmt health >/dev/null || fail "plugin management routes never came up"

# Two requests: the stub reports two more weekly limit points on the second.
echo "sending two requests as codex_cli_rs/0.130.0"
for _ in 1 2; do
  status=$(curl -sS -o "$WORK/response.json" -w '%{http_code}' "http://127.0.0.1:$CPA_PORT/v1/chat/completions" \
    -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
    -H "User-Agent: codex_cli_rs/0.130.0 (Linux; x86_64) e2e" \
    -d '{"model":"stub-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$status" = 200 ] || fail "proxied request returned $status: $(head -c 400 "$WORK/response.json")"
done

records=0
for _ in $(seq 1 60); do
  records=$(mgmt summary | python3 -c 'import json,sys; print(json.load(sys.stdin)["totals"]["records"])')
  [ "$records" -ge 2 ] && break
  sleep 0.5
done
[ "$records" -ge 2 ] || fail "the usage records did not reach the plugin"

check() {
  python3 -c "import json,sys; d=json.load(sys.stdin); ok=$2; print(('ok   ' if ok else 'BAD ') + '$1'); ok or print(json.dumps(d)[:1500]); sys.exit(0 if ok else 1)" || fail "$1"
}
mgmt "breakdown?by=client" | check "client is codex/0.130.0 (TraceID join)" '[r["key"] for r in d["rows"]] == ["codex/0.130.0"]'
mgmt options | check "filter options use recorded names" 'd["models"] == ["stub-model-2026-11-01"] and d["clients"] == ["codex"]'
mgmt "breakdown?by=model" | check "reported model recorded" '[r["key"] for r in d["rows"]] == ["stub-model-2026-11-01"]'
mgmt summary | check "tokens normalized" 'd["totals"]["uncached_input"] == 400 and d["totals"]["cache_read"] == 2000 and d["totals"]["reasoning"] == 200'
for _ in $(seq 1 20); do
  mgmt pricing | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["status"]["models"] else 1)' && break
  sleep 0.5
done
mgmt pricing | check "price table fetched through host.http.do" 'd["status"]["models"] >= 1'
mgmt summary | check "cost priced from the table" 'd["totals"]["cost_usd"] > 0 and d["totals"]["cost_quality"]["unpriced_tokens"] == 0'
mgmt limit-burn | check "weekly limit read from response headers" 'len(d["points"]) == 1 and d["points"][0]["weekly_pct"] == 2 and d["points"][0]["auth_index"] != ""'
mgmt limit-history | check "weekly snapshots listed per account" 'len(d["accounts"]) == 1 and d["accounts"][0]["points"][-1]["weekly_pct"] == 12'
mgmt "breakdown?by=account" | check "rows grouped by account" 'len(d["rows"]) == 1 and d["rows"][0]["key"] != ""'
mgmt "breakdown?by=provider" | check "rows grouped by provider" 'len(d["rows"]) == 1 and d["rows"][0]["requests"] == 2'
mgmt "drift?group=response_model" | check "drift includes output_total and cost_usd" 'len(d["points"]) == 1 and d["points"][0]["median"]["output_total"] > 0 and d["points"][0]["median"]["cost_usd"] > 0'
mgmt errors | check "errors route returns empty list on success" 'd["total"] == 0 and d["rows"] == []'
errors_code=$(curl -s -o /dev/null -w '%{http_code}' "$API/errors")
[ "$errors_code" = 401 ] && echo "ok   errors require the key" || fail "errors without key returned $errors_code"
options_code=$(curl -s -o /dev/null -w '%{http_code}' "$API/options")
[ "$options_code" = 401 ] && echo "ok   options require the key" || fail "options without key returned $options_code"
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/summary")
[ "$code" = 401 ] && echo "ok   management routes require the key" || fail "summary without key returned $code"
curl -fsS "http://127.0.0.1:$CPA_PORT/v0/resource/plugins/cpa-analytics/dashboard" | grep 'id="drift"' > /dev/null \
  && echo "ok   dashboard served" || fail "dashboard not served"
echo "e2e passed"
