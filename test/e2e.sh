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
  for c in $(docker ps -aq --filter label=hotlane.app=demo); do docker rm -f "$c" >/dev/null 2>&1 || true; done
  docker rm -f hotlane-demo-drift >/dev/null 2>&1 || true
  docker rmi -f $(docker images -q hotlane-demo) >/dev/null 2>&1 || true
  rm -rf "$HOME/.hotlane/demo"
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
    curl -s "http://$API/v1/status" | python3 -c "
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

step "push: broken change is rejected, live untouched"
perl -pi -e 's/writeHead\(200/writeHead(500/g' server.js
if OUT="$("$BIN" push 2>&1)"; then fail "broken push was promoted: $OUT"; fi
echo "$OUT" | grep -q "REJECTED" || fail "no REJECTED in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v3"
git checkout -q server.js

step "rollback: previous version, then forward"
"$BIN" rollback | grep -q "ROLLED BACK to v2" || fail "rollback to v2 failed"
expect_body "http://$PROXY/" "hello from demo-app v2"
"$BIN" rollback 3 >/dev/null || fail "rollback 3 failed"
expect_body "http://$PROXY/" "hello from demo-app v3"

step "archivist: clean build catches up to v3"
wait_archive 3 || fail "archive never reached v3/clean (overlap-collapse regression?)"

step "drift: tampering the live container is detected"
docker exec hotlane-demo-v3 sh -c 'echo TAMPERED > /app/message.txt'
if "$BIN" drift >/dev/null 2>&1; then fail "drift not detected"; fi

step "drift: recovery push forks from the clean image and heals"
# note: the REJECTED push above consumed v4 - version numbers are honest
# history and never reused, so the recovery promotes as v5.
perl -pi -e 's/"v3"/"v4"/' server.js
OUT="$("$BIN" push)" || fail "recovery push rejected: $OUT"
echo "$OUT" | grep -q "PROMOTED v5" || fail "no PROMOTED v5 in: $OUT"
expect_body "http://$PROXY/" "hello from demo-app v4"
wait_archive 5 || fail "archive never reached v5/clean after recovery"
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
HOTLANE_TOKEN=supersecret "$BIN" status | grep -q "live: v5" || fail "authorized status failed"
wait_http "http://$API/healthz" 200 5 || fail "healthz should stay open without token"

printf '\nALL E2E CHECKS PASSED\n'
