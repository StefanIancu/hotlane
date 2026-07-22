# M5 benchmark: hotlane vs a classical pipeline

Date: 2026-07-19. Verdict: **GO** - every kill criterion cleared by a wide margin. Numbers, method, and honest caveats below.

## Headline

| | push-to-verified-live (median) | p95 | rollback (cold) |
|---|---|---|---|
| Classical pipeline (GitHub Actions, real history) | **493s** (~8m13s) | 590s | re-runs the pipeline (minutes) |
| hotlane - TS/Express (tsc build) | **1.72s** | 3.9s | 642ms |
| hotlane - FastAPI (pip) | **1.18s** | 4.3s | 652ms |

Speedup vs the classical baseline: **~286x** (TS/Express) and **~417x** (FastAPI).

The go/no-go criteria this project set for itself before writing the daemon: under 30s median (result: 1.2-1.7s), at least 5x faster than the baseline (result: >280x), rollback under 1s (result: ~650ms cold), zero promotes a verify hook should have caught (result: zero; the M3 e2e separately proved a broken health check gets rejected with the live version untouched).

## Method

**Path A (classical)**: the last 15 successful runs of a real production workflow - `deploy.yml` on the orkestr repo (build two Docker images, push to GHCR, SSH deploy, compose up). Durations from the GitHub API: median 493s, min 464s, max 590s. This is not a strawman; it is a tuned, real pipeline that deploys to production on every push to main.

**Path B (hotlane)**: `bench/bench.sh` - for each workload, create the baseline, then 10 pushes each mutating a version string in the source, timing daemon-accept to promoted (the total includes fork, patch, incremental build, boot, and HTTP-verified health). Then one rollback against a stopped ring entry (cold restart, the worst case).

Workloads in `bench/workloads/`:

- **node-ts**: Express + TypeScript, build = `npm install && tsc` (incremental), verify = `/health == 200`
- **fastapi**: FastAPI + uvicorn, build = `pip install -r requirements.txt`, verify = `/health == 200`

Environment: macOS (Apple Silicon), Docker Desktop 27.5.1, single host, daemon and client on the same machine.

## Per-run data

TS/Express: 3906, 2415, 1748, 1671, 1644, 1700, 1753, 1742, 1687, 1663 (ms)
FastAPI: 4329, 1506, 1179, 1183, 1160, 1219, 1174, 1211, 1158, 1075 (ms)

Run 1 in both is the first fork after a cold baseline - it pays the full dependency install once; every later fork inherits the warm filesystem from the snapshot. That first-run cost (~4s) IS the p95 in both series.

## Honest caveats

- **Path A and Path B are not the identical app.** Path A is a real pipeline for a bigger system (two images); running these small bench apps through a fresh GitHub Actions deploy pipeline would be faster than 493s - but not by the two orders of magnitude that separate the paths: runner queue + checkout + toolchain setup alone typically cost 60-120s before any build starts, and an image build + registry round-trip + remote deploy add minutes. The gap survives any reasonable normalization; treat "286x" as "two to three orders of magnitude", not a precise ratio.
- **Phase attribution is approximate on macOS.** Docker Desktop's port forwarding accepts TCP before the app inside actually listens, so some app warm-up that should count as "boot" lands in "verify". The **total** is unaffected - it ends when the health endpoint genuinely returned 200 and traffic flipped.
- **Small apps flatter the snapshot phase.** `docker commit` cost grows with filesystem churn; a giant node_modules or build cache will push snapshot beyond ~200ms. It would need to grow by two orders of magnitude to threaten the 30s budget.
- **Single host, loopback network, no TLS, no DB.** This measures the deploy loop, not a production topology.

## Conclusion

The thesis holds with room to spare: treating the deploy unit as a verified running fork - instead of a cold-built artifact - turns an 8-minute pipeline into a 1-2 second loop on real (if small) apps with real build steps, with sub-second rollback. The MVP's remaining gap to trustworthiness is the archivist (async reproducible image + drift check), which is v0.2.

---

# The swarm test: 398 deploys under load (v0.7.4, 2026-07-22)

The M5 benchmark measures one push in isolation. This one measures the
claim agents actually depend on: **traffic never notices a deploy** -
even when deploys never stop.

## Setup

- One VPS: 2 vCPU, 4 GB RAM, Ubuntu 24.04, stock Docker. Daemon,
  agents and load generator all on the box (loopback network).
- The app: a small Python stdlib HTTP server serving a text page
  (single-threaded on purpose - no app-level concurrency to hide
  behind).
- The swarm, for 25 minutes against one daemon:
  - 4 pusher agents looping `edit -> hotlane push` from their own
    checkouts
  - 2 tester agents looping `edit -> hotlane test -> poke the held
    fork via X-Hotlane-Fork -> promote or discard`, asserting the fork
    served exactly their content every time
  - 1 agent polling status and running MCP sessions (`initialize` +
    `hotlane_status` tool calls)
- Constant load: 100 req/s against the live proxy for the full run,
  every response's status recorded with a timestamp. Replay
  verification on (`last: 50, mode: report`).

## Results

| metric | value |
|---|---|
| requests served | 150,001 (steady 100.0 req/s) |
| requests dropped or non-200 | **0** |
| traffic flips (promotes) under that load | 398 - one every 3.8s |
| pushes / test flows completed | 355 / 92, zero failures |
| fork-isolation violations (held fork served wrong content) | 0 |
| MCP / status failures | 0 |
| response latency p50 / p90 | 6.2 ms / 13.7 ms |
| response latency p99 / worst | 1,019 ms / 2,087 ms |
| push wall-time under 6-agent contention, p50 / p99 | 16.4 s / 20.4 s |

## Honest caveats

- **The p99 is the snapshot pause.** `docker commit` briefly pauses
  the live container at every push; with 398 snapshots in 25 minutes,
  roughly 1% of requests stalled 1-2 seconds. Delayed, never dropped -
  but if your SLO is a hard sub-second p99 under *continuous*
  deploying, this is the number to know. (Evaluating an unpaused
  snapshot is on the roadmap.)
- **Push wall-time is queue, not machinery.** Pushes serialize per
  app by design; ~2s of actual work waited behind five other agents.
  A single agent sees ~2s.
- **Small app, loopback, no TLS, no DB** - same scope as the M5
  benchmark: this measures the deploy loop under load, not a
  production topology.
- The harness (kill -9 chaos and swarm scripts) lives outside the
  repo today; numbers come from its raw logs: 539-cycle crash soak,
  then this run, both on the released binaries.
