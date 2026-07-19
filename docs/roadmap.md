# Roadmap

## Shipped in v0.1

- The core loop: `push` forks the warm live instance, applies your git delta, verifies the fork in isolation, and promotes it with an atomic proxy flip - ~1s end to end on real apps ([benchmark](benchmark.md))
- Pointer-flip `rollback` against a ring of kept versions (~0.6s)
- The archivist: after every promote, an async from-source image build, optional registry push, and behavioral drift checks with self-healing (drifted apps rebuild from the clean image on the next push)
- Webhook notifications (Slack/Discord-native) on drift transitions, rejected pushes, and failed clean builds
- `init` app detection (Node/TS, FastAPI, Flask, Django, Go), `status`, `logs`, `drift`
- CI-grade push: baseline-commit tracking so clean CI checkouts deploy correctly ([integration guide](ci.md))
- Built-in HTTPS for the API (`-tls-domain`, Let's Encrypt) + bearer-token auth
- Linux and macOS, amd64 and arm64

## Next

Roughly in order of intent - open an issue if your priority differs:

- **Multi-app daemons** - one daemon serving several `hotlane.yml` apps on one host
- **Configurable budgets** - per-hook verify timeouts, drift-check interval
- **Traffic-replay verification** - mirror a slice of live requests into the fork and diff responses before promoting
- **Database branching hooks** - integrate branchable storage (Neon, ZFS/LVM snapshots) so forks can get forked state, not just shared state
- **GitHub Action** - a published action wrapping install + push
- **Response normalization for drift checks** - tolerate timestamps and request IDs on hook paths
- **Multi-host** - a version ring gossiped across daemons behind a shared load balancer

## Non-goals

- Replacing your CI (tests and review stay where they are)
- Running on ECS / Fargate / Cloud Run / Kubernetes - hotlane is an alternative to that layer, not a passenger on it ([details](ci.md#where-does-hotlane-run-and-not-run))
- Orchestrating fleets. One box, owned well, is the point.
