#!/usr/bin/env bash
# hotlane end-to-end test: the full lifecycle against real Docker.
# Runs locally (macOS/Linux) and in CI. Needs: go, docker, git, curl, python3.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
BIN="$TMP/hotlane"
APP="$TMP/app"
API="127.0.0.1:7433"
PROXY="127.0.0.1:7480"
DLOG="$TMP/daemon.log"

cleanup() {
  pkill -x hotlane 2>/dev/null || true
  for app in demo alpha beta; do
    for c in $(docker ps -aq --filter label=hotlane.app=$app); do docker rm -f "$c" >/dev/null 2>&1 || true; done
    docker rm -f "hotlane-$app-drift" >/dev/null 2>&1 || true
    docker rmi -f $(docker images -q "hotlane-$app") >/dev/null 2>&1 || true
    rm -rf "$HOME/.hotlane/$app"
  done
}
trap 'code=$?; [ $code -ne 0 ] && { echo "--- daemon log:"; cat "$DLOG" 2>/dev/null; }; cleanup; exit $code' EXIT

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*"; exit 1; }

wait_http() { # url want tries
  for _ in $(seq 1 "${3:-60}"); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$1" 2>/dev/null)" = "$2" ] && return 0
    sleep 2
  done
  return 1
}
expect_body() { # url want
  local got; got="$(curl -s "$1")"
  [ "$got" = "$2" ] || fail "expected '$2' from $1, got '$got'"
}
wait_archive() { # version: clean image built for it, not building, drift clean
  for _ in $(seq 1 120); do
    curl -s -H "Authorization: Bearer supersecret" "http://$API/v1/status" | python3 -c "
import json, sys
d = json.load(sys.stdin)
a = d['archive']
sys.exit(0 if a['last_version'] == $1 and not a['building'] and a['drift'] == 'clean' else 1)
" 2>/dev/null && return 0
    sleep 2
  done
  return 1
}

step "build"
cd "$ROOT"
go build -o "$BIN" ./cmd/hotlane

step "pre-clean + set up demo app"
cleanup
mkdir -p "$APP"
cp "$ROOT"/example/demo-app/server.js "$ROOT"/example/demo-app/message.txt "$ROOT"/example/demo-app/hotlane.yml "$APP/"
cd "$APP"
git init -q
git config user.email ci@hotlane.dev
git config user.name ci
git add -A && git commit -qm baseline

step "serve: baseline boots and serves"
"$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" >"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 90 || fail "baseline never became healthy"
expect_body "http://$PROXY/" "hello from demo-app v1"

step "push: dirty-worktree change promotes"
perl -pi -e 's/"v1"/"v2"/' server.js
OUT="$("$BIN" push)" || fail "good push rejected: $OUT"
echo "$OUT" | grep -q "PROMOTED v2" || fail "no PROMOTED v2 in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v2"

step "push: committed change with clean worktree promotes (CI mode)"
git commit -qam v2
perl -pi -e 's/"v2"/"v3"/' server.js
git commit -qam v3
[ -z "$(git status --porcelain)" ] || fail "worktree not clean before CI-mode push"
OUT="$("$BIN" push)" || fail "CI-mode push rejected: $OUT"
echo "$OUT" | grep -q "PROMOTED v3" || fail "no PROMOTED v3 in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v3"

step "test: hold a fork, poke it via header, live untouched, then promote"
perl -pi -e 's/"v3"/"v3t"/' server.js
OUT="$("$BIN" test)" || fail "test rejected: $OUT"
HV=$(echo "$OUT" | grep -oE "HELD v[0-9]+" | grep -oE "[0-9]+")
[ -n "$HV" ] || fail "no HELD version in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v3"
FORK_BODY=$(curl -s -H "X-Hotlane-Fork: $HV" "http://$PROXY/")
[ "$FORK_BODY" = "hello from demo-app v3t" ] || fail "fork body wrong: $FORK_BODY"
"$BIN" promote "$HV" | grep -q "PROMOTED" || fail "promote of held fork failed"
expect_body "http://$PROXY/" "hello from demo-app v3t"
curl -s -H "X-Hotlane-Fork: $HV" "http://$PROXY/" | grep -q "no held fork" || fail "promoted fork still routable as held"

step "test: discard a held fork, nothing changes"
perl -pi -e 's/"v3t"/"v3x"/' server.js
OUT="$("$BIN" test)" || fail "second test rejected: $OUT"
HV2=$(echo "$OUT" | grep -oE "HELD v[0-9]+" | grep -oE "[0-9]+")
"$BIN" discard "$HV2" | grep -q "discarded" || fail "discard failed"
expect_body "http://$PROXY/" "hello from demo-app v3t"
perl -pi -e 's/"v3x"/"v3t"/' server.js

step "push: broken change is rejected, live untouched"
perl -pi -e 's/writeHead\(200/writeHead(500/g' server.js
if OUT="$("$BIN" push 2>&1)"; then fail "broken push was promoted: $OUT"; fi
echo "$OUT" | grep -q "REJECTED" || fail "no REJECTED in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v3t"
git checkout -q server.js
perl -pi -e 's/"v3"/"v3t"/' server.js

step "rollback: previous version, then forward"
"$BIN" rollback | grep -q "ROLLED BACK to v3" || fail "rollback to v3 failed"
expect_body "http://$PROXY/" "hello from demo-app v3"
"$BIN" rollback 4 >/dev/null || fail "rollback 4 failed"
expect_body "http://$PROXY/" "hello from demo-app v3t"

step "archivist: clean build catches up to the promoted held fork (v4)"
wait_archive 4 || fail "archive never reached v4/clean (overlap-collapse regression?)"

step "drift: tampering the live container is detected"
docker exec hotlane-demo-v4 sh -c 'echo TAMPERED > /app/message.txt'
if "$BIN" drift >/dev/null 2>&1; then fail "drift not detected"; fi

step "drift: recovery push forks from the clean image and heals"
# version bookkeeping: v2,v3 pushes; v4 promoted-held; v5 discarded-held;
# the REJECTED push consumed v6 - so recovery promotes as v7. Version
# numbers are honest history and never reused.
perl -pi -e 's/"v3t"/"v4"/' server.js
OUT="$("$BIN" push)" || fail "recovery push rejected: $OUT"
echo "$OUT" | grep -q "PROMOTED v7" || fail "no PROMOTED v7 in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v4"
wait_archive 7 || fail "archive never reached v7/clean after recovery"
"$BIN" drift | grep -q "CLEAN" || fail "drift not clean after recovery"

step "auth: daemon restart adopts the live container, token gates the API"
# Unpushed local edit before the restart: the restarted daemon must NOT
# re-snapshot the (now dirty) worktree - the archivist keeps the last
# promoted source, or every post-restart drift check false-positives.
echo "hello-dirty" > message.txt
pkill -x hotlane; sleep 2
HOTLANE_REBASE_DEPTH=5 "$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/" 200 30 || fail "adopt after restart failed"
expect_body "http://$PROXY/" "hello from demo-app v4"
if "$BIN" status >/dev/null 2>&1; then fail "API served without token"; fi
HOTLANE_TOKEN=supersecret "$BIN" status | grep -q "live: v7" || fail "authorized status failed"
wait_http "http://$API/healthz" 200 5 || fail "healthz should stay open without token"

step "restart keeps the promoted snapshot (stale-checkout guard)"
wait_archive 7 || fail "drift not clean after restart - archivist re-snapshotted the dirty worktree"
git checkout -q message.txt

step "agent surfaces: API index, -json output, MCP session"
curl -s -H "Authorization: Bearer supersecret" "http://$API/v1" | python3 -c "import json,sys; d=json.load(sys.stdin); assert d['service']=='hotlane' and len(d['routes'])>=8" || fail "API index wrong"
HOTLANE_TOKEN=supersecret "$BIN" status -json | python3 -c "import json,sys; d=json.load(sys.stdin); assert 'live' in d and 'archive' in d and 'held' in d" || fail "status -json missing keys (held?)"
MCP_OUT=$(printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n{"jsonrpc":"2.0","method":"notifications/initialized"}\n{"jsonrpc":"2.0","id":2,"method":"tools/list"}\n{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hotlane_status","arguments":{}}}\n' | HOTLANE_TOKEN=supersecret HOTLANE_DAEMON="http://$API" "$BIN" mcp)
echo "$MCP_OUT" | grep -q '"name":"hotlane"' || fail "mcp initialize failed"
echo "$MCP_OUT" | grep -q 'hotlane_push' || fail "mcp tools/list missing hotlane_push"
echo "$MCP_OUT" | grep -q 'baseline_commit' || fail "mcp hotlane_status call failed"

step "auto-rebase: a deep layer chain forks from the clean image"
# The restarted daemon runs with HOTLANE_REBASE_DEPTH=5; the base image
# alone has more history entries than that, so the next push must rebase.
perl -pi -e 's/"v4"/"v5"/' server.js
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push)" || fail "rebase push rejected: $OUT"
echo "$OUT" | grep -q "rebased from the clean image" || fail "no rebase marker in: $OUT"
echo "$OUT" | grep -q "PROMOTED v8" || fail "no PROMOTED v8 in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v5"

step "replay: buffer fills, report mode flags a content change but promotes"
pkill -x hotlane; sleep 2
printf 'replay:\n  last: 20\n' >> hotlane.yml
git add hotlane.yml && git commit -qm "enable replay"
"$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 30 || fail "replay daemon never came up"
for _ in $(seq 1 6); do curl -s "http://$PROXY/" >/dev/null; done
curl -s -H "Authorization: Bearer supersecret" "http://$API/v1/status" | python3 -c "
import json, sys
d = json.load(sys.stdin)
assert d['replay']['enabled'] and d['replay']['buffered'] >= 6, d['replay']
" || fail "replay buffer not filling"
perl -pi -e 's/hello/howdy/' message.txt
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push)" || fail "report-mode push rejected: $OUT"
echo "$OUT" | grep -q "MISMATCH GET /" || fail "no replay mismatch reported in: $OUT"
echo "$OUT" | grep -q "PROMOTED" || fail "report mode should still promote: $OUT"
expect_body "http://$PROXY/" "howdy from demo-app v5"
git commit -qam howdy   # align HEAD with the promoted source for the next dirty diff

step "replay: gate mode rejects what verify hooks missed"
pkill -x hotlane; sleep 2
perl -pi -e 's/^  last: 20$/  last: 20\n  mode: gate/' hotlane.yml
git commit -qam "gate replay"
"$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 30 || fail "gate daemon never came up"
for _ in $(seq 1 6); do curl -s "http://$PROXY/" >/dev/null; done
perl -pi -e 's/howdy/sneaky/' message.txt
if OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push 2>&1)"; then fail "gated mismatch was promoted: $OUT"; fi
echo "$OUT" | grep -q "MISMATCH GET /" || fail "no mismatch detail in gate rejection: $OUT"
echo "$OUT" | grep -q "REJECTED" || fail "no REJECTED in gate output: $OUT"
expect_body "http://$PROXY/" "howdy from demo-app v5"
git checkout -q message.txt

step "replay phase 2: drift check catches tampering on a path no hook names"
wait_archive 9 || fail "archive never caught up to v9 before the phase-2 drift check"
pkill -x hotlane; sleep 2
# Hooks watch ONLY /health; "/" is invisible to hook-path comparison.
python3 - <<'EOF'
import re
s = open("hotlane.yml").read()
s = re.sub(r"verify:.*?(?=^\S)", "verify:\n  - http: /health == 200\n", s, flags=re.S | re.M)
open("hotlane.yml", "w").write(s)
EOF
git commit -qam "hooks: health only"
"$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 30 || fail "phase-2 daemon never came up"
LIVE=$(curl -s -H "Authorization: Bearer supersecret" "http://$API/v1/status" | python3 -c "import json,sys; print(json.load(sys.stdin)['live'])")
docker exec "$LIVE" sh -c 'echo TAMPERED > /app/message.txt'
for _ in $(seq 1 6); do curl -s "http://$PROXY/" >/dev/null; done   # users now see the tampered page
if OUT="$(HOTLANE_TOKEN=supersecret "$BIN" drift 2>&1)"; then fail "phase-2 drift not detected: $OUT"; fi
echo "$OUT" | grep -q "replayed traffic differs on GET /" || fail "drift not attributed to replayed traffic: $OUT"

step "multi-app: two apps from one -apps dir, host-routed"
pkill -x hotlane; sleep 2
MAPPS="$TMP/apps"; mkdir -p "$MAPPS"
for app in alpha beta; do
  mkdir -p "$TMP/srv-$app"
  cp "$ROOT"/example/demo-app/server.js "$TMP/srv-$app/"
  echo "hello from $app" > "$TMP/srv-$app/message.txt"
  (cd "$TMP/srv-$app" && git init -q && git config user.email ci@hotlane.dev && git config user.name ci && git add -A && git commit -qm baseline)
  cat > "$MAPPS/$app.yml" <<EOF
app: $app
image: node:22-alpine
run: node server.js
port: 3000
src: ../srv-$app
domain: $app.local
verify:
  - http: /health == 200
replay:
  last: 10
EOF
done
"$BIN" serve -apps "$MAPPS" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$API/healthz" 200 90 || fail "multi-app daemon never came up"
curl -s "http://$API/healthz" | grep -q "ok apps=2" || fail "healthz should count apps, not name them"
host_body() { curl -s -H "Host: $1" "http://$PROXY/"; }
for _ in $(seq 1 60); do
  [ "$(host_body alpha.local)" = "hello from alpha from demo-app v1" ] && \
  [ "$(host_body beta.local)" = "hello from beta from demo-app v1" ] && break
  sleep 2
done
[ "$(host_body alpha.local)" = "hello from alpha from demo-app v1" ] || fail "alpha not routed"
[ "$(host_body beta.local)" = "hello from beta from demo-app v1" ] || fail "beta not routed"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: nope.local' "http://$PROXY/")" = "421" ] || fail "unknown host should 421, never fall through"

step "multi-app: CLI push (-app) updates one app, the other untouched"
cd "$TMP/srv-alpha"
perl -pi -e 's/hello from alpha/ALPHA v2/' message.txt
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push -app alpha)" || fail "alpha push rejected: $OUT"
echo "$OUT" | grep -q "PROMOTED v2" || fail "no PROMOTED v2 in: $OUT"
# replay is per-app: alpha's buffer (filled by the routing checks above)
# flags the intended content change; report mode still promotes.
echo "$OUT" | grep -q "MISMATCH GET /" || fail "alpha replay verdict missing in: $OUT"
[ "$(host_body alpha.local)" = "ALPHA v2 from demo-app v1" ] || fail "alpha not updated"
[ "$(host_body beta.local)" = "hello from beta from demo-app v1" ] || fail "beta changed by alpha's push"

step "multi-app: rejected push (HOTLANE_APP) harms neither app"
cd "$TMP/srv-beta"
perl -pi -e 's/writeHead\(200/writeHead(500/g' server.js
if OUT="$(HOTLANE_TOKEN=supersecret HOTLANE_APP=beta "$BIN" push 2>&1)"; then fail "broken beta push promoted: $OUT"; fi
echo "$OUT" | grep -q "REJECTED" || fail "no REJECTED in: $OUT"
[ "$(host_body beta.local)" = "hello from beta from demo-app v1" ] || fail "beta live changed after rejection"
[ "$(host_body alpha.local)" = "ALPHA v2 from demo-app v1" ] || fail "alpha harmed by beta's rejection"

step "multi-app: bare routes refuse with directions, status -all lists both"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer supersecret' "http://$API/v1/status")" = "400" ] || fail "bare status should 400 on a multi-app daemon"
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" status -all)" || fail "status -all failed"
echo "$OUT" | grep -q "alpha" || fail "status -all missing alpha: $OUT"
echo "$OUT" | grep -q "beta" || fail "status -all missing beta: $OUT"
cd "$APP"

printf '\nALL E2E CHECKS PASSED\n'
