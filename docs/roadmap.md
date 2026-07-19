# Roadmap

Last updated: 2026-07-19 (v0.6.0).

## Shipped

**v0.1** - the core loop: `push` forks the warm live instance, applies the git delta, verifies in isolation, promotes with an atomic flip (~1-2s on real apps, [benchmark](benchmark.md)); ring + pointer-flip `rollback`; the archivist (async from-source images, registry push, behavioral drift checks, self-healing); webhook notifications; `init` detection; CI-grade push with baseline-commit tracking ([integration guide](ci.md)); built-in HTTPS (`-tls-domain`) + token auth.

**v0.2** - the app owns `https://domain/` with TLS included; the API tucked under the reserved `/-/` prefix; port-80 redirect. Auto-rebase: fork chains reset onto the clean image past ~40 layers, so agent-speed pushing never hits Docker's layer limit. Three race fixes surfaced by CI's first runs (archivist overlap queueing, verify-before-flip rollback, adopt-newest).

**v0.3** - `hotlane test`: hold a verified fork, validate it via the `X-Hotlane-Fork` header while users stay on live, then `promote` the exact tested instance or `discard`. TTL-reaped, capped, source-retained for correct archiving.

**v0.4** - agent-native surfaces: `hotlane mcp` (eight typed tools over stdio), `-json` on every state-touching command, self-describing `GET /-/v1`, and [llms-full.txt](https://hotlane.dev/llms-full.txt) - the complete one-fetch operating contract.

**v0.4.2** - the three dogfooding papercuts: `${VAR}` interpolation in `notify`/`archive` (secrets out of committed config; unset var fails loudly), per-hook `timeout:` on verify checks, and systemd friendliness (`/var/lib/hotlane` fallback when `$HOME` is unset + a shipped [unit file](../packaging/systemd/hotlane.service)). Plus drift checks that stop false-positiving on dynamic content: volatile patterns (timestamps, UUIDs, hex ids, epoch numbers) are masked before comparing, and each instance is sampled twice so anything that differs between two requests to the same server is excluded as evidence - status codes still always compare.

**GitHub Action** - [`StefanIancu/hotlane-action@v1`](https://github.com/marketplace/actions/hotlane-deploy) on the Marketplace: install + any client command, verify verdict as outputs and a job summary ([integration guide](ci.md#github-actions)).

**v0.5** - multi-app daemons ([design](multi-app.md)): `serve -apps /etc/hotlane/apps/` runs every config in the directory behind shared listeners. Host-header routing with an explicit 421 (never a fall-through to another app), per-app rings/archivists/held forks, the `/-/v1/apps/<app>/` API namespace (bare paths stay full aliases on single-app daemons - zero client breakage), `-tls` with one Let's Encrypt cert per `domain:`, a global clean-build semaphore, `status -all`, and app selection for clients via `-app` / `HOTLANE_APP` / the local hotlane.yml. Static by design: the set of apps is what's on disk.

**v0.6** - traffic-replay verification ([design](traffic-replay.md)): shadow testing built into the deploy. The proxy records a rolling in-memory slice of live traffic (with the responses live served); every push replays it against the verified fork and diffs the answers via the drift normalizer, self-dynamic paths comparing status only. `mode: report` annotates the push and pings the webhook; `mode: gate` rejects a mismatch exactly like a failing verify hook. Reads-only by default, memory-only buffer, and `hotlane test` holds carry the report for agents to read before promoting.

## Next

Roughly ordered; dogfooding findings marked (df). Open an issue if your priority differs.
- **Browser-clickable fork previews** - subdomain-per-held-fork (needs wildcard DNS/DNS-01; the header covers agents today)
- **Database branching hooks** - integrate branchable storage (Neon, ZFS/LVM snapshots) so forks can get forked state
- **Multi-host** - a version ring gossiped across daemons behind a shared load balancer

## Non-goals

- Replacing your CI (tests and review stay where they are)
- Running on ECS / Fargate / Cloud Run / Kubernetes - hotlane is an alternative to that layer, not a passenger on it ([details](ci.md#where-does-hotlane-run-and-not-run))
- Orchestrating fleets. One box, owned well, is the point.
