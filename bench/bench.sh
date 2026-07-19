#!/usr/bin/env bash
# hotlane M5 benchmark: N mutated pushes against one workload, plus a
# rollback timing. Prints per-run totals and median/p95.
#
# usage: bench.sh <workload-dir> <version-file> <runs> [hotlane-binary]
set -euo pipefail

WORKLOAD=$1
VFILE=$2
RUNS=$3
HOTLANE=${4:-hotlane}

WORK=$(mktemp -d)
trap 'pkill -f "hotlane serve" 2>/dev/null || true' EXIT
cp -R "$WORKLOAD"/. "$WORK/"
cd "$WORK"
git init -q && git add -A && git commit -qm baseline

APP=$(awk '/^app:/{print $2}' hotlane.yml)
rm -rf "$HOME/.hotlane/$APP"
for c in $(docker ps -aq --filter "label=hotlane.app=$APP"); do docker rm -f "$c" >/dev/null; done
for i in $(docker images -q "hotlane-$APP"); do docker rmi -f "$i" >/dev/null 2>&1 || true; done

echo "== $APP: baseline (cold: full install + build, one-time)"
BASE_START=$(date +%s)
"$HOTLANE" serve -config "$WORK/hotlane.yml" -addr 127.0.0.1:7433 -proxy 127.0.0.1:7480 &>"$WORK/daemon.log" &
until curl -s --max-time 1 http://127.0.0.1:7480/health >/dev/null 2>&1; do
  sleep 1
  if ! pgrep -f "hotlane serve" >/dev/null; then
    echo "daemon died:"; cat "$WORK/daemon.log"; exit 1
  fi
done
echo "   baseline ready in $(( $(date +%s) - BASE_START ))s"

TOTALS=()
for n in $(seq 2 $((RUNS + 1))); do
  sed -i '' "s/VERSION = \"v[0-9]*\"/VERSION = \"v$n\"/" "$VFILE"
  OUT=$("$HOTLANE" push 2>&1) || { echo "$OUT"; exit 1; }
  TOTAL=$(echo "$OUT" | awk '/PROMOTED/{gsub(/ms/,"",$NF); print $NF}')
  PHASES=$(echo "$OUT" | head -1 | sed 's/^fork [^:]*: //')
  echo "   run $((n - 1)): ${TOTAL}ms  ($PHASES)"
  TOTALS+=("$TOTAL")
done

echo "== rollback (previous version, stopped)"
sleep 6  # let the superseded container's stop land so this measures a cold restart
RB=$("$HOTLANE" rollback 2>&1) || { echo "$RB"; exit 1; }
echo "   $RB"

printf '%s\n' "${TOTALS[@]}" | python3 -c '
import statistics, sys
xs = sorted(int(l) for l in sys.stdin)
p95 = xs[min(len(xs) - 1, round(0.95 * len(xs)) - 1)]
print(f"== push-to-verified-live over {len(xs)} runs: median {statistics.median(xs):.0f}ms | p95 {p95}ms | min {xs[0]}ms | max {xs[-1]}ms")
'

pkill -f "hotlane serve" 2>/dev/null || true
for c in $(docker ps -aq --filter "label=hotlane.app=$APP"); do docker rm -f "$c" >/dev/null; done
for i in $(docker images -q "hotlane-$APP"); do docker rmi -f "$i" >/dev/null 2>&1 || true; done
rm -rf "$HOME/.hotlane/$APP"
