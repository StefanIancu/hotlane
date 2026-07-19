# MVP scope

Goal: prove or kill one claim - **push-to-verified-live in under 30 seconds changes how people (and agents) ship**. Everything not needed to measure that claim is out.

## In scope (v0.1)

- **Single host.** One daemon, one Go binary, systemd unit. No fleet, no gossip, no HA.
- **Container runtime.** Docker with overlayfs commits as the "snapshot" primitive. No microVMs yet - keeps the MVP runnable on any Linux box (and macOS for dev).
- **Two app classes.** Node and Python (interpreted: patch-apply is near-zero cost, best-case demo; also the dominant agent-loop languages).
- **Warm pool.** Per app: the live container plus a paused clone ready to fork. Fork = `docker commit` the baseline + `docker run` the clone with the patch applied.
- **Delta push.** `hotlane push` sends `git diff HEAD~1` (or explicit range) over HTTP to the daemon; daemon applies with `git apply` inside the fork, runs the incremental `build` command.
- **Verifier.** Two hook types only: `http` (path + expected status) and `run` (script exit code). Timeout per hook, total verify budget configurable.
- **Embedded proxy.** stdlib `httputil.ReverseProxy` in front of the app port. Promote = atomically swap the backend target. No TLS (sits behind whatever edge you already have).
- **Version ring.** Last N verified containers kept stopped-but-present. `hotlane rollback [n]` restarts (if needed) and flips the proxy. Target: under 1 second when the previous version is still running, under 5 when it needs a cold start from the ring.
- **CLI.** `hotlane serve`, `hotlane push`, `hotlane status`, `hotlane rollback`.

## Out of scope (v0.1)

- Archivist / async OCI build (v0.2 - needed before anyone trusts it, not needed to measure the claim)
- MicroVM isolation (Firecracker / Cloud Hypervisor)
- Database forking / branchable storage
- Multi-host, TLS, auth beyond a shared token
- Web UI

## Milestones

- **M0 - skeleton.** CLI parses, config loads, daemon serves `/healthz`. (this repo, day 1)
- **M1 - warm pool.** `hotlane serve` adopts or starts the app container from `hotlane.yml`, proxy routes to it.
- **M2 - push.** Delta over HTTP, applied in a fork, incremental build runs, fork boots on a side port.
- **M3 - verify + promote.** Hooks run against the fork; pass = proxy flip, fail = fork destroyed with logs returned to the pusher.
- **M4 - ring + rollback.** Version ring, `rollback` flips in under a second.
- **M5 - benchmark.** Side-by-side harness (below). This is the go/no-go gate for investing further.

## Benchmark plan (M5)

Test workloads: real apps currently deployed through a classical pipeline (orkestr projects work as guinea pigs - they deploy via clone -> Kaniko build -> registry push -> docker run today, and the repo's GitHub Actions flow is the industry-standard baseline).

Protocol, per workload (one Node/Express app, one FastAPI app):

1. Same one-line change (bump a string in a route response).
2. Path A: existing pipeline (GitHub Actions or orkestr's 7-step), measure push-to-live-verified.
3. Path B: `hotlane push`, measure push-to-live-verified.
4. Rollback both ways, measure time-to-previous-version-serving.
5. Repeat 10x, report median + p95.

Success criteria:

- push-to-verified-live: **under 30s median** on Path B, and at least **5x faster** than Path A
- rollback: **under 1s** on Path B
- zero failed promotes that a verify hook should have caught

If Path B lands at 2x instead of 10x, the thesis is weak - kill or rescope before writing more code.

## Open questions (decide during M1-M3, not now)

- Fork base: `docker commit` per push vs a long-lived paused clone that gets re-synced. Commit is simpler; measure whether its latency (~1-3s on big images) hurts the budget.
- Patch transport: raw `git diff` is fine for the MVP, but binary files and rebases will break it. Bundle fallback (`git bundle`) if it bites during testing.
- Verify traffic replay (mirroring a slice of live requests into the fork) - powerful, but only if the basic hooks prove insufficient in the benchmark.
