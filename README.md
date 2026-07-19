# hotlane

**Validation-first deployment.** Push a change, get a verified running fork of your app in seconds, promote it with an atomic traffic flip, roll back in milliseconds. The container image is built in the background, off the critical path.

> Status: pre-MVP. Design is settled, daemon is being built. See [docs/mvp.md](docs/mvp.md) for scope.

## Why

Classical CI/CD treats every change as a cold-start artifact production problem. Push a one-line fix and the pipeline rebuilds the world: cold clone, cold caches, full image build, registry push, registry pull, scheduler round-trip, health-check grace period. The change was 40 bytes; the pipeline moved gigabytes. Rollback usually means re-running that same slow pipeline against an old commit.

That loop shape is tolerable for humans shipping a few times a day. It is deadly for agentic development, where the loop is "apply fix, validate, iterate" and needs to complete in seconds, not minutes.

hotlane inverts the model:

1. **Validation + serving** is a *delta* operation on a warm, running system.
2. **Artifact production** (reproducible OCI image, provenance, audit trail) still happens, but asynchronously, after the change is already verified and live.

The deploy unit is a **verified running fork**, not an image.

## How it works

```
push (delta)                          ~0.1s
└─ fork warm instance                 ~0.2s
└─ apply patch + incremental build    ~2-15s   (hot caches; near-zero for interpreted langs)
└─ verify in isolation                ~2-10s   (health + smoke hooks, no traffic exposure)
└─ promote: router flips to fork      ~0.05s   (previous version stays running, frozen)
                                      ────────
                        total         ~5-30s
└─ (background) clean image build + registry push + drift check
```

**Rollback** flips the router pointer to any entry in the version ring: sub-second, no builds, no registry pulls, works even when the builder is down.

## Components

- **Daemon** - a single Go binary per host. Owns the warm pool (current live version + snapshot per app), an embedded reverse proxy (promote/rollback = pointer flip), and a local version ring of the last N verified versions kept alive or frozen.
- **Trigger** - a webhook endpoint or `hotlane push`. Ships a git delta, not a repo: often under 1 KB on the wire for an agent loop.
- **Verifier** - pluggable checks that run inside the fork before any traffic sees it: process boots, health endpoint answers, plus user hooks (smoke script, affected tests).
- **Archivist** - async, off the critical path. Produces the canonical reproducible image after promotion and periodically cold-boots it to diff behavior against the warm instance. Drift flags the app red and the next deploy goes through the cold path. The fast lane earns trust continuously or loses it.

## Config

```yaml
# hotlane.yml
app: api
image: node:22-alpine         # base image for the warm baseline
build: npm run build          # incremental command, runs inside the fork
run: node dist/server.js
port: 3000
verify:
  - http: /health == 200
  - run: ./smoke.sh
ring: 5                       # versions kept for instant rollback
archive: ghcr.io/acme/api     # registry ref for the archivist's clean images
notify: https://hooks.slack.com/services/...  # webhook: drift detected/healed, push rejected
```

## What hotlane is not

- Not a CI system. Your CI keeps doing lint, tests, and review on the repo. hotlane replaces only the build+deploy jobs (the CI step becomes "curl the daemon").
- Not a Kubernetes operator. v1 targets a single host - bare metal, a VPS, an EC2 instance - which covers a huge share of real apps and all agent-loop use cases.
- Not a reproducibility replacement. The async clean build remains the source of truth; the warm path is a fast lane, continuously reconciled against it.

## Honest tradeoffs

- **State does not fork.** Forks share the real database in v1. Migrations run through an explicit gate and require expansive-then-contractive discipline. Branchable storage (Neon, ZFS/LVM snapshots) is the v2 answer.
- **Warm drift is the killer risk.** That is why the archivist's periodic cold-boot diff is non-negotiable.
- **Speed cuts both ways.** A 30-second push-to-live loop for agent-generated code needs a real isolation story. v1 uses containers with overlayfs; microVM isolation (Firecracker / Cloud Hypervisor) is on the roadmap for hosts with KVM.

## License

MIT
