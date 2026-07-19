<div align="center">
  <img src="docs/logo.svg" width="96" alt="hotlane - a flame with a lane through it">
  <h1>hotlane</h1>
  <p><strong>Validation-first deployment.</strong><br>
  Push a change, get a verified running fork of your app in about a second.<br>
  Roll back by pointer. Images build in the background.</p>

  <p>
    <a href="https://github.com/StefanIancu/hotlane/releases"><img src="https://img.shields.io/github/v/release/StefanIancu/hotlane?color=ffa028&label=release" alt="Release"></a>
    <a href="https://www.npmjs.com/package/hotlane"><img src="https://img.shields.io/npm/v/hotlane?color=cb3837&label=npm" alt="npm"></a>
    <a href="https://pypi.org/project/hotlane/"><img src="https://img.shields.io/pypi/v/hotlane?color=3775a9&label=pypi" alt="PyPI"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-8bc47a" alt="MIT license"></a>
  </p>

  <p>
    <a href="https://hotlane.dev">Website</a> ·
    <a href="https://hotlane.dev/docs">Docs</a> ·
    <a href="docs/ci.md">CI integration</a> ·
    <a href="docs/benchmark.md">Benchmark</a> ·
    <a href="docs/roadmap.md">Roadmap</a>
  </p>

  <img src="docs/demo.gif" alt="hotlane demo: edit, push, verified live in ~1s, rollback in ~700ms" width="720">
</div>

## Why

Classical CI/CD treats every change as a cold-start artifact production problem. Push a one-line fix and the pipeline rebuilds the world: cold clone, cold caches, full image build, registry push, registry pull, scheduler round-trip. The change was 40 bytes; the pipeline moved gigabytes - for eight minutes. Rollback usually means running that same pipeline again.

hotlane inverts the model:

1. **Validation + serving** is a *delta* operation on a warm, running system.
2. **Artifact production** (reproducible image, audit trail) still happens - asynchronously, after the change is already verified and live.

The deploy unit is a **verified running fork**, not an image.

## How it works

```
push (delta)                          ~0.1s
└─ fork warm instance                 ~0.2s
└─ apply patch + incremental build    ~2-15s   (hot caches; near-zero for interpreted langs)
└─ verify in isolation                ~2-10s   (health + smoke hooks, no traffic exposure)
└─ promote: router flips to fork      ~0.05s   (previous version stays parked in a ring)
                                      ────────
                        total         ~5-30s   (~1s on small apps - see the benchmark)
└─ (background) clean image build + registry push + drift check
```

A fork that fails verification is destroyed - the pusher gets the failing hook and the fork's last logs; **unverified code never receives a byte of traffic**. Rollback flips the router to any kept version: sub-second, no builds, works when the builder is down.

## Quickstart

```bash
curl -fsSL https://hotlane.dev/install.sh | sh
# or: brew install StefanIancu/hotlane/hotlane  |  npm install -g hotlane  |  pip install hotlane

cd your-app
hotlane init      # detects Node / Python / Go, writes hotlane.yml
hotlane serve     # boots the warm pool, proxies traffic

# make a change, then:
hotlane push
#   ok   http: /health == 200 (13ms)
# PROMOTED v2 live in 978ms

hotlane rollback  # flip back, sub-second
```

Requirements: a Linux or macOS host with Docker and git. One Go binary is both the daemon (`serve`) and the CLI.

## Measured, not promised

Benchmarked against a real production GitHub Actions pipeline (15 runs of a real repo's deploy history) - method and honest caveats in [docs/benchmark.md](docs/benchmark.md):

| | push to verified live (median) | rollback |
|---|---|---|
| Classical pipeline (GitHub Actions) | **8m 13s** | re-run the pipeline |
| hotlane - TypeScript/Express (tsc build) | **1.72s** | 0.64s |
| hotlane - FastAPI (pip) | **1.18s** | 0.65s |

## Config

Everything is one file:

```yaml
# hotlane.yml
app: api
image: node:22-alpine         # base image for the warm baseline
build: npm run build          # incremental command, reruns against warm caches
run: node dist/server.js
port: 3000
verify:
  - http: /health == 200
    timeout: 5s               # optional; defaults: 15s for http, 60s for run
  - run: ./smoke.sh
ring: 5                       # versions kept for instant rollback
archive: ghcr.io/acme/api     # registry ref for the archivist's clean images
notify: ${HOTLANE_NOTIFY_URL} # drift detected/healed, push rejected (Slack/Discord)
```

`notify` and `archive` interpolate `${VAR}` from the daemon's environment, so webhook URLs and registry refs never have to live in a committed file (an unset variable fails the load loudly). Build/run/verify scripts are left untouched - their `${VAR}`s belong to the shell inside the container. For running the daemon as a service, a systemd unit ships in [`packaging/systemd/`](packaging/systemd/hotlane.service) ([guide](docs/ci.md#running-the-daemon-under-systemd)); without `$HOME`, state lands in `/var/lib/hotlane`.

**Several apps, one box**: `hotlane serve -apps /etc/hotlane/apps/` serves every `*.yml` in the directory. Each config adds `src:` (the app's checkout) and `domain:`; traffic routes by Host header on the shared listeners (`-tls` provisions a Let's Encrypt cert per domain), and every app keeps its own ring, archivist, held forks, and verify gate - a rejected push on one app cannot touch another. Client commands name their app via `./hotlane.yml`, `HOTLANE_APP`, or `-app`; `hotlane status -all` shows the whole box. Design notes: [docs/multi-app.md](docs/multi-app.md).

## CLI

```bash
hotlane init         # detect the app, write a starter hotlane.yml
hotlane serve        # run the daemon (-token / -tls-domain to expose safely;
                     #   -apps DIR serves every config in a directory, -tls certs them all)
hotlane push         # git delta -> verified running fork -> traffic flip (~1-2s)
hotlane test         # like push, but HOLD the verified fork: poke it via the
                     #   X-Hotlane-Fork header, then promote or discard it
hotlane promote <n>  # flip traffic to a held fork - byte-identical to what you tested
hotlane discard <n>  # destroy a held fork; live traffic never knew
hotlane rollback [n] # flip to the previous (or a specific) kept version
hotlane status       # live version, ring, drift verdict, timings (-all: every app)
hotlane logs [-n N]  # tail the live version's output
hotlane drift        # cold-boot the clean image, diff behavior vs live; exit 1 on drift
hotlane mcp          # serve hotlane as MCP tools over stdio - agents get push/test/
                     #   promote/rollback natively, no shell parsing
```

Every state-touching command accepts `-json` for machine-readable output. The daemon API is self-describing (`GET /-/v1`), and agents can read the complete operating contract in one fetch: [hotlane.dev/llms-full.txt](https://hotlane.dev/llms-full.txt).

Client commands read `HOTLANE_DAEMON` and send `HOTLANE_TOKEN` as a bearer token. With `serve -tls-domain yourapp.example.com`, the daemon does its own HTTPS via Let's Encrypt and shares :443 the way humans expect: **your app is served at `https://yourapp.example.com/` with TLS included**, the API tucks under the reserved `/-/` prefix, and port 80 redirects. CI deploys with two secrets and one command ([full guide](docs/ci.md)).

## The archivist

The warm fork chain is a cache; the archivist is its validation. After every promote it rebuilds your app **from source, from scratch, in the background** - the image classical CI would have made, minus the waiting - pushes it to your registry, and periodically cold-boots it to diff behavior against live:

```
$ hotlane drift
DRIFTED: behavior differs on /: clean build serves "hello", live serves "TAMPERED"
next push will rebuild from hotlane-api:clean
```

The comparison is behavior-based and tolerant of dynamic content: timestamps, UUIDs, request ids and the like are masked, and anything that differs between two requests to the *same* instance is excluded as evidence (status codes always compare) - so a `/health` that reports uptime doesn't cry wolf. Divergence pings your webhook (Slack/Discord native), and the next ordinary push rebuilds from the clean image - the chain heals itself. The clean image also keeps the fast lane sustainable: fork chains auto-rebase onto it every ~40 pushes, so agent-speed pushing never hits Docker's layer-depth limit. Fast lane and audit trail, both real.

## Built for agent loops

An agent can't wait eight minutes to learn it was wrong. hotlane makes each push-observe-fix turn cost about a second, over one HTTP endpoint (POST a raw git diff, get JSON back: timings, hook verdicts, promoted or rejected with logs), with the verify gate as the guardrail - a bad agent push dies in isolation while production keeps serving. And with `hotlane test`, the agent's own task-specific checks join the gate: fork, poke the running result through the `X-Hotlane-Fork` header while users stay on live, then promote the exact instance it validated - no rebuild between tested and live. Agents can learn the whole tool from [hotlane.dev/llms.txt](https://hotlane.dev/llms.txt).

## Where it runs (and doesn't)

On a machine you own that runs Docker: a VPS, bare metal, an EC2 instance, a homelab box. **Not** on ECS/Fargate/Cloud Run/Kubernetes - hotlane commands the host's Docker daemon and is an alternative to that layer, not a passenger on it. Teams keeping a managed platform for prod can run hotlane on a cheap box for the fast inner loop and feed the archivist's images to the existing pipeline. [Details](docs/ci.md#where-does-hotlane-run-and-not-run).

## Honest tradeoffs

- **State does not fork.** Forks share the real database. Run migrations expansively; treat schema changes with respect.
- **Drift checks cover hook paths.** An endpoint without a hook can drift undetected - add hooks for what matters.
- **Single host.** One daemon, one box. That covers a huge share of real apps; multi-host is on the [roadmap](docs/roadmap.md).
- **Container-grade isolation.** Fine for your own code and trusted teams; not a sandbox for hostile code.

## License

[MIT](LICENSE)
