# Roadmap

Last updated: 2026-07-20 (v0.7.1).

## Shipped

**v0.1** - the core loop: `push` forks the warm live instance, applies the git delta, verifies in isolation, promotes with an atomic flip (~1-2s on real apps, [benchmark](benchmark.md)); ring + pointer-flip `rollback`; the archivist (async from-source images, registry push, behavioral drift checks, self-healing); webhook notifications; `init` detection; CI-grade push with baseline-commit tracking ([integration guide](ci.md)); built-in HTTPS (`-tls-domain`) + token auth.

**v0.2** - the app owns `https://domain/` with TLS included; the API tucked under the reserved `/-/` prefix; port-80 redirect. Auto-rebase: fork chains reset onto the clean image past ~40 layers, so agent-speed pushing never hits Docker's layer limit. Three race fixes surfaced by CI's first runs (archivist overlap queueing, verify-before-flip rollback, adopt-newest).

**v0.3** - `hotlane test`: hold a verified fork, validate it via the `X-Hotlane-Fork` header while users stay on live, then `promote` the exact tested instance or `discard`. TTL-reaped, capped, source-retained for correct archiving.

**v0.4** - agent-native surfaces: `hotlane mcp` (eight typed tools over stdio), `-json` on every state-touching command, self-describing `GET /-/v1`, and [llms-full.txt](https://hotlane.dev/llms-full.txt) - the complete one-fetch operating contract.

**v0.4.2** - the three dogfooding papercuts: `${VAR}` interpolation in `notify`/`archive` (secrets out of committed config; unset var fails loudly), per-hook `timeout:` on verify checks, and systemd friendliness (`/var/lib/hotlane` fallback when `$HOME` is unset + a shipped [unit file](../packaging/systemd/hotlane.service)). Plus drift checks that stop false-positiving on dynamic content: volatile patterns (timestamps, UUIDs, hex ids, epoch numbers) are masked before comparing, and each instance is sampled twice so anything that differs between two requests to the same server is excluded as evidence - status codes still always compare.

**GitHub Action** - [`StefanIancu/hotlane-action@v1`](https://github.com/marketplace/actions/hotlane-deploy) on the Marketplace: install + any client command, verify verdict as outputs and a job summary ([integration guide](ci.md#github-actions)).

**v0.5** - multi-app daemons ([design](multi-app.md)): `serve -apps /etc/hotlane/apps/` runs every config in the directory behind shared listeners. Host-header routing with an explicit 421 (never a fall-through to another app), per-app rings/archivists/held forks, the `/-/v1/apps/<app>/` API namespace (bare paths stay full aliases on single-app daemons - zero client breakage), `-tls` with one Let's Encrypt cert per `domain:`, a global clean-build semaphore, `status -all`, and app selection for clients via `-app` / `HOTLANE_APP` / the local hotlane.yml. Static by design: the set of apps is what's on disk.

**v0.6** - traffic-replay verification ([design](traffic-replay.md)): shadow testing built into the deploy. The proxy records a rolling in-memory slice of live traffic (with the responses live served); every push replays it against the verified fork and diffs the answers via the drift normalizer, self-dynamic paths comparing status only. `mode: report` annotates the push and pings the webhook; `mode: gate` rejects a mismatch exactly like a failing verify hook. Reads-only by default, memory-only buffer, and `hotlane test` holds carry the report for agents to read before promoting.

**v0.6.1** - replay phase 2: drift checks replay the recorded slice against the cold boot, so behavioral drift is caught on any endpoint users recently exercised, not just verify-hook paths. Plus the buffer-lifecycle fix phase 2 forced into the open: every traffic flip resets the buffer (recorded exchanges describe the version that served them; replaying them against a successor false-positives - the second of two rapid pushes hit exactly this).

**v0.6.2** - pre-launch security audit: `serve` refuses to bind beyond loopback without a token; snapshot writes never follow symlinks (a pushed symlink could otherwise write any host file as root); held-fork URLs use `<version>-<random token>` instead of a guessable integer; replay bodies and query strings stay out of logs and webhooks; app-name charset enforced; failed forks no longer orphan images. Plus first-run polish: `docker.Preflight` diagnoses a missing/down/permission-denied Docker instead of leaking exec errors, and a [FAQ](ci.md) on the security posture and where hotlane does and doesn't run.

**v0.7.0** - 24 fixes from an adversarial bug hunt (four parallel subsystem reviews, a race soak, and a 40-push steady-state run). The worst: a rollback undone by an in-flight push, the TTL reaper destroying a held fork mid-promote, missing docker timeouts letting `commit --pause` freeze the live instance, replay breaking websockets/SSE, the replay gate bypassed by a hung fork, and an MCP string version triggering an unrequested rollback. The e2e suite grew to 27 scenarios including three dedicated race reproductions.

**v0.7.1** - the last known minors, plus first-contact polish from a fresh-eyes dogfooding session. Prune retains versions by how recently they were live (a new persisted `live-history`), not by number - a rollback no longer marks the known-good version for pruning while keeping the bad one; the same history powers an orphan reaper that collects forks stranded running by a crash in promote's marker-clear window (or mid-verify). Replay preserves the recorded `Host` header instead of sending the fork's loopback hostport, and its counts are disjoint (`replayed = matched + dynamic + mismatched`) as the docs always claimed. First-contact: bare `hotlane serve` now works - the API binds loopback by default (all-interfaces stays the default when a token is set, so tokened deployments are unchanged), the startup banner names each listener's role with a clickable URL, and the API root answers with directions instead of a 404. The e2e suite runs on its own ports and kills only its own daemons, so it coexists with a hotlane being dogfooded on the same machine.

## Next

Roughly ordered; dogfooding findings marked (df). Open an issue if your priority differs.
- **Browser-clickable fork previews** - subdomain-per-held-fork (needs wildcard DNS/DNS-01; the header covers agents today)
- **Database branching hooks** - integrate branchable storage (Neon, ZFS/LVM snapshots) so forks can get forked state
- **Multi-host** - a version ring gossiped across daemons behind a shared load balancer

## Non-goals

- Replacing your CI (tests and review stay where they are)
- Running on ECS / Fargate / Cloud Run / Kubernetes - hotlane is an alternative to that layer, not a passenger on it ([details](ci.md#where-does-hotlane-run-and-not-run))
- Orchestrating fleets. One box, owned well, is the point.
