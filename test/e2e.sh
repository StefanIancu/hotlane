#!/usr/bin/env bash
# hotlane end-to-end test: the full lifecycle against real Docker.
# Runs locally (macOS/Linux) and in CI. Needs: go, docker, git, curl, python3.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
BIN="$TMP/hotlane"
APP="$TMP/app"
# Deliberately NOT the default ports: a hotlane someone is dogfooding on
# this machine would otherwise answer the suite's health checks (and get
# pkilled for its trouble either way - see stop_daemons).
API="127.0.0.1:17433"
PROXY="127.0.0.1:17480"
# Client commands default to the standard port; point them at the
# suite's daemon. Scenario-local env prefixes (HOTLANE_TOKEN=... "$BIN"
# push) compose fine with an exported variable.
export HOTLANE_DAEMON="http://$API"
DLOG="$TMP/daemon.log"

# Every suite daemon carries -addr $API on its command line, so this
# pattern kills suite daemons (this run's or a crashed previous run's
# still holding the port) and never a hotlane someone is dogfooding.
# No leading dash: pkill -f would parse it as an option and silently
# kill nothing.
KILLPAT="addr $API"

# stop_daemons kills the suite's daemons and WAITS for them to die: a
# daemon mid-archive-build can outlive SIGTERM by seconds, and a fixed
# sleep let the old daemon answer the next scenario's first health check.
stop_daemons() {
  pkill -f "$KILLPAT" 2>/dev/null || true
  for _ in $(seq 1 20); do
    pgrep -f "$KILLPAT" >/dev/null 2>&1 || return 0
    sleep 0.5
  done
  pkill -9 -f "$KILLPAT" 2>/dev/null || true
  sleep 1
}

cleanup() {
  pkill -f "$KILLPAT" 2>/dev/null || true
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
# The fork address is <version>-<token>; the bare version must NOT work,
# or anyone on the internet could read unreleased code by counting.
HDR=$(echo "$OUT" | grep -oE "X-Hotlane-Fork: [0-9]+-[0-9a-f]+" | sed 's/X-Hotlane-Fork: //')
[ -n "$HDR" ] || fail "no tokenized fork header in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v3"
FORK_BODY=$(curl -s -H "X-Hotlane-Fork: $HDR" "http://$PROXY/")
[ "$FORK_BODY" = "hello from demo-app v3t" ] || fail "fork body wrong: $FORK_BODY"
curl -s -H "X-Hotlane-Fork: $HV" "http://$PROXY/" | grep -q "no held fork" || fail "bare version number reached a held fork - enumeration is possible"
curl -s -H "X-Hotlane-Fork: $HV-deadbeefdeadbeefdeadbeefdeadbeef" "http://$PROXY/" | grep -q "no held fork" || fail "wrong token reached a held fork"
"$BIN" promote "$HV" | grep -q "PROMOTED" || fail "promote of held fork failed"
expect_body "http://$PROXY/" "hello from demo-app v3t"
curl -s -H "X-Hotlane-Fork: $HDR" "http://$PROXY/" | grep -q "no held fork" || fail "promoted fork still routable as held"

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
stop_daemons
HOTLANE_REBASE_DEPTH=1 "$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/" 200 30 || fail "adopt after restart failed"
expect_body "http://$PROXY/" "hello from demo-app v4"
# The API is a SEPARATE listener from the proxy: wait for it before
# calling it, or this races daemon startup.
wait_http "http://$API/healthz" 200 30 || fail "healthz should stay open without token"
if "$BIN" status >/dev/null 2>&1; then fail "API served without token"; fi
HOTLANE_TOKEN=supersecret "$BIN" status | grep -q "live: v7" || fail "authorized status failed"

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
# Depth is measured as growth PAST the clean image, not absolute image
# depth - otherwise an app with a deep base image would rebase on every
# push and silently lose its warm caches. With the threshold at 1 and a
# chain already one layer past clean, the next push must rebase.
# v7 was forked FROM the clean image, so the live chain sits AT clean
# depth. This push grows it one layer past clean...
perl -pi -e 's/"v4"/"v5"/' server.js
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push)" || fail "push rejected: $OUT"
echo "$OUT" | grep -q "PROMOTED v8" || fail "no PROMOTED v8 in: $OUT"
# ...so this one crosses the threshold and must rebase.
perl -pi -e 's/"v5"/"v5b"/' server.js
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push)" || fail "rebase push rejected: $OUT"
echo "$OUT" | grep -q "rebased from the clean image" || fail "no rebase marker in: $OUT"
echo "$OUT" | grep -q "PROMOTED v9" || fail "no PROMOTED v9 in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v5b"

step "restart while a fork is HELD must not adopt it as live"
# A held fork is a RUNNING container with this app's label, so a naive
# restart adopts it - putting unpromoted, unverified code in front of
# users. This is the core promise, so it gets a permanent test.
LIVE_BEFORE="$(curl -s "http://$PROXY/")"
perl -pi -e 's/"v5b"/"vNEVERPROMOTED"/' server.js
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" test)" || fail "hold before restart failed: $OUT"
HVR=$(echo "$OUT" | grep -oE "HELD v[0-9]+" | grep -oE "[0-9]+")
[ -n "$HVR" ] || fail "no held fork before restart: $OUT"
stop_daemons
HOTLANE_REBASE_DEPTH=1 "$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 60 || fail "daemon did not restart after holding a fork"
wait_http "http://$API/healthz" 200 30 || fail "API did not come up after restart"
[ "$(curl -s "http://$PROXY/")" = "$LIVE_BEFORE" ] || fail "restart changed live traffic: held fork was adopted"
docker ps --filter "name=hotlane-demo-v$HVR" --format '{{.Names}}' | grep -q . && fail "held fork container v$HVR still running after restart"
HOTLANE_TOKEN=supersecret "$BIN" status -json | python3 -c "
import json,sys; d=json.load(sys.stdin)
assert d['live'] != 'hotlane-demo-v$HVR', 'daemon adopted the held fork as live'
assert d['held'] == [], 'stale held entries after restart: %r' % d['held']
" || fail "post-restart state wrong"
perl -pi -e 's/"vNEVERPROMOTED"/"v5b"/' server.js


step "replay: buffer fills, report mode flags a content change but promotes"
stop_daemons
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
expect_body "http://$PROXY/" "howdy from demo-app v5b"
git commit -qam howdy   # align HEAD with the promoted source for the next dirty diff

step "replay: gate mode rejects what verify hooks missed"
stop_daemons
perl -pi -e 's/^  last: 20$/  last: 20\n  mode: gate/' hotlane.yml
git commit -qam "gate replay"
"$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 30 || fail "gate daemon never came up"
for _ in $(seq 1 6); do curl -s "http://$PROXY/" >/dev/null; done
perl -pi -e 's/howdy/sneaky/' message.txt
if OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push 2>&1)"; then fail "gated mismatch was promoted: $OUT"; fi
echo "$OUT" | grep -q "MISMATCH GET /" || fail "no mismatch detail in gate rejection: $OUT"
echo "$OUT" | grep -q "REJECTED" || fail "no REJECTED in gate output: $OUT"
expect_body "http://$PROXY/" "howdy from demo-app v5b"
git checkout -q message.txt

step "replay phase 2: drift check catches tampering on a path no hook names"
LIVEV=$(HOTLANE_TOKEN=supersecret "$BIN" status -json | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')
wait_archive "$LIVEV" || fail "archive never caught up to v$LIVEV before the phase-2 drift check"
stop_daemons
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

step "rollback during an in-flight push is not undone by that push"
# handleRollback used to skip the push lock, and Fork releases the pool
# mutex before verify runs - so a rollback landing in a push's verify
# window returned SUCCESS and was then silently reverted by that push's
# promote. The operator kept serving the version they were escaping.
stop_daemons
# Write the config explicitly rather than patching the inherited one:
# earlier scenarios left replay in GATE mode, which would reject these
# deliberate content changes and make the race untestable.
cat > hotlane.yml <<'YAMLEOF'
app: demo
image: node:22-alpine
run: node server.js
port: 3000
ring: 3
verify:
  - run: sleep 12
YAMLEOF
git commit -qam "explicit slow-verify config for the race scenarios"
HOTLANE_REBASE_DEPTH=1 "$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 60 || fail "daemon did not start for the race scenario"
wait_http "http://$API/healthz" 200 30 || fail "API did not come up"
PRE_BODY="$(curl -s "http://$PROXY/")"
perl -pi -e 's/"v[0-9a-zA-Z]*"/"vRACE"/' server.js
( HOTLANE_TOKEN=supersecret "$BIN" push >/dev/null 2>&1 ) &
PUSH_PID=$!
sleep 4   # the push is now inside its 12s verify hook
RB=$(HOTLANE_TOKEN=supersecret "$BIN" rollback 2>&1) || fail "rollback errored during a push: $RB"
wait $PUSH_PID 2>/dev/null || true
sleep 2
POST_BODY="$(curl -s "http://$PROXY/")"
[ "$POST_BODY" = "$PRE_BODY" ] || fail "rollback was undone by the in-flight push: expected '$PRE_BODY', serving '$POST_BODY'"
LIVEC=$(HOTLANE_TOKEN=supersecret "$BIN" status -json | python3 -c 'import json,sys; print(json.load(sys.stdin)["live"])')
docker ps --filter "name=^${LIVEC}$" --format '{{.Names}}' | grep -q . || fail "live container $LIVEC is not running after the race"

step "a held fork whose TTL expired cannot be promoted onto live"
# The reaper ran unsynchronized with promote: if a TTL expired inside
# PromoteHeld's verify window it destroyed the container, and promote
# then pointed the public proxy at a port that no longer existed.
stop_daemons
HOTLANE_HOLD_TTL=5s "$BIN" serve -config "$APP/hotlane.yml" -addr "$API" -proxy "$PROXY" -token supersecret >>"$DLOG" 2>&1 &
wait_http "http://$PROXY/health" 200 60 || fail "daemon did not start for the TTL scenario"
wait_http "http://$API/healthz" 200 30 || fail "API did not come up"
BEFORE_TTL="$(curl -s "http://$PROXY/")"
perl -pi -e 's/"vRACE"/"vEXPIRED"/' server.js
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" test)" || fail "hold for the TTL scenario failed: $OUT"
TV=$(echo "$OUT" | grep -oE "HELD v[0-9]+" | grep -oE "[0-9]+")
sleep 40   # past the 5s TTL and past a 30s reaper tick
if OUT="$(HOTLANE_TOKEN=supersecret "$BIN" promote "$TV" 2>&1)"; then
  fail "promoted a fork whose TTL had expired and whose container was reaped: $OUT"
fi
[ "$(curl -s "http://$PROXY/")" = "$BEFORE_TTL" ] || fail "live changed after a failed expired-fork promote"
wait_http "http://$PROXY/health" 200 10 || fail "app unreachable after the expired-fork promote attempt"
perl -pi -e 's/"vEXPIRED"/"vRACE"/' server.js

step "rollback during an archivist build does not false-positive drift"
# Archive captured the live backend BY VALUE before a multi-minute
# build, so a rollback in between left the drift check comparing
# against a stopped container - a connection error read as drift, which
# then forced every later push through the clean image.
perl -pi -e 's/"vRACE"/"vBUILD"/' server.js
OUT="$(HOTLANE_TOKEN=supersecret "$BIN" push)" || fail "push before the build race failed: $OUT"
HOTLANE_TOKEN=supersecret "$BIN" rollback >/dev/null 2>&1 || fail "rollback during the archivist build failed"
for _ in $(seq 1 60); do
  BUILDING=$(HOTLANE_TOKEN=supersecret "$BIN" status -json | python3 -c 'import json,sys; print(json.load(sys.stdin)["archive"]["building"])')
  [ "$BUILDING" = "False" ] && break
  sleep 2
done
DETAIL=$(HOTLANE_TOKEN=supersecret "$BIN" status -json | python3 -c 'import json,sys; print(json.load(sys.stdin)["archive"].get("detail",""))')
case "$DETAIL" in
  *"connection refused"*|*"comparing"*)
    fail "drift verdict came from a dead backend, not a real comparison: $DETAIL" ;;
esac
wait_http "http://$PROXY/health" 200 10 || fail "app unreachable after the build-race scenario"
git checkout -q hotlane.yml server.js 2>/dev/null || true

step "multi-app: two apps from one -apps dir, host-routed"
stop_daemons
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
