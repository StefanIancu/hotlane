# Integrating hotlane into your pipeline

hotlane does not replace your CI - it replaces the **build + deploy stage**. Tests, lint, and review stay wherever they are; the deploy job stops being "build image, push registry, pull, roll" and becomes one command that finishes in seconds:

```
hotlane push
```

## The shape

```
┌─ CI runner (GitHub Actions, GitLab, CodeBuild, Jenkins...) ─┐
│  checkout -> test -> hotlane push ──────────────┐           │
└─────────────────────────────────────────────────┼───────────┘
                                                  │ HTTPS + bearer token
┌─ your server (any box with Docker) ─────────────▼───────────┐
│  hotlane serve: fork -> verify -> flip     app traffic :7480│
│  archivist (background): clean image -> your registry       │
└─────────────────────────────────────────────────────────────┘
```

The daemon runs next to your app and keeps it warm. CI just delivers the delta. The reproducible image still lands in your registry - the archivist builds and pushes it in the background after every promote, so the audit trail is identical to classical CI/CD; it's just off the critical path.

## How push knows what to send

The daemon records the **baseline commit** - the git commit your source was at when the daemon first snapshotted it (visible in `hotlane status` / `GET /v1/status` as `baseline_commit`). `hotlane push` diffs **from that baseline to your working tree**, which makes both loops work with the same command:

- **Local dev**: uncommitted edits are included - edit, push, iterate.
- **CI**: the runner checks out a newer commit with a clean worktree - the baseline-to-HEAD diff *is* the deployment.

Two requirements on the CI side:

1. **Full history in the checkout.** A shallow clone won't contain the baseline commit and push will refuse with a clear error. Use `fetch-depth: 0` (Actions), `GIT_DEPTH: 0` (GitLab), or `git fetch --unshallow`.
2. **Serialize deploy jobs** (one push at a time per app) with your CI's concurrency controls. The daemon also serializes internally; this just avoids queued-forever jobs.

`hotlane push -from <ref>` overrides the base when you need to hand-pick it.

## Reaching the daemon

**Recommended: direct HTTPS + token.** The daemon does its own TLS - point a DNS record at your server and start it with a domain:

```bash
hotlane serve -tls-domain deploy.example.com -token $(openssl rand -hex 24)
```

Certificates come from Let's Encrypt automatically (TLS-ALPN on :443; renewals handled for you), and the listener is shared the way humans expect: **your app is served at `https://deploy.example.com/` with TLS included**, while the daemon API tucks under the reserved `/-/` prefix (`https://deploy.example.com/-/v1/...`). Port 80 redirects to https. CI then needs exactly two secrets and zero infrastructure:

```bash
HOTLANE_DAEMON=https://deploy.example.com HOTLANE_TOKEN=... hotlane push
```

`-tls-domain` refuses to start without a token, tokens are compared in constant time, and every route except `/healthz` requires one. This is the same trust model as any SaaS API: TLS for the transport, a bearer token for identity.

Alternatives when they fit better:

- **Private network** - if CI and the server already share a VPC or a WireGuard/Tailscale mesh, skip public exposure entirely: `HOTLANE_DAEMON=http://<private-ip>:7433` (still set a token).
- **Your existing reverse proxy** - already running Traefik/Caddy/nginx with TLS? Front `:7433` with it instead of `-tls-domain`.
- **SSH tunnel** - for a box with no domain at all: `ssh -f -N -L 7433:127.0.0.1:7433 deploy@server`, then push to `http://127.0.0.1:7433`. Works everywhere SSH does; clunkiest of the four.

## GitHub Actions

Use the published action - [`StefanIancu/hotlane-action`](https://github.com/marketplace/actions/hotlane-deploy) wraps install + any client command and surfaces the verify verdict as outputs and a job summary:

```yaml
name: deploy
on:
  push:
    branches: [main]

concurrency: deploy-production   # one deploy at a time

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: npm ci && npm test

  deploy:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0                       # the diff base must exist locally
      - uses: StefanIancu/hotlane-action@v1
        with:
          daemon: ${{ vars.HOTLANE_DAEMON }}
          token: ${{ secrets.HOTLANE_TOKEN }}
```

That's the whole deploy job. (Using a private network or SSH tunnel? Add your connectivity step before it and point `daemon` accordingly.) A rejected push fails the job with the verify results and the fork's dying logs; a promoted one writes the phase timings and verify table to the job summary. `command: rollback` / `command: drift` (with `args` where needed) cover [manual rollback and scheduled drift-check workflows](https://github.com/StefanIancu/hotlane-action#manual-rollback) the same way.

Prefer raw steps? The action is sugar over exactly this:

```yaml
      - name: Install hotlane
        run: |
          curl -fsSL https://github.com/StefanIancu/hotlane/releases/latest/download/hotlane_linux_amd64.tar.gz \
            | sudo tar -xz -C /usr/local/bin hotlane
      - name: Push
        env:
          HOTLANE_DAEMON: https://deploy.example.com
          HOTLANE_TOKEN: ${{ secrets.HOTLANE_TOKEN }}
        run: hotlane push
```

## GitLab CI

```yaml
deploy:
  stage: deploy
  resource_group: production          # serializes deploys
  variables:
    GIT_DEPTH: 0
    HOTLANE_DAEMON: https://deploy.example.com
  script:
    - curl -fsSL https://github.com/StefanIancu/hotlane/releases/latest/download/hotlane_linux_amd64.tar.gz | tar -xz
    - ./hotlane push
  only: [main]
```

Set `HOTLANE_TOKEN` as a protected, masked CI/CD variable.

## AWS CodeBuild

```yaml
version: 0.2
env:
  variables:
    HOTLANE_DAEMON: https://deploy.example.com
  parameter-store:
    HOTLANE_TOKEN: /myapp/hotlane-token
phases:
  install:
    commands:
      - curl -fsSL https://github.com/StefanIancu/hotlane/releases/latest/download/hotlane_linux_amd64.tar.gz | tar -xz -C /usr/local/bin hotlane
  build:
    commands:
      - git fetch --unshallow || true
      - hotlane push
```

(Daemon host in the same VPC as CodeBuild? Skip public exposure entirely: `HOTLANE_DAEMON=http://<private-ip>:7433`.)

## Anything else (Jenkins, CircleCI, a cron job, an agent)

The API is one endpoint - any environment that has git and curl can deploy:

```bash
BASE=$(curl -s -H "Authorization: Bearer $HOTLANE_TOKEN" $HOTLANE_DAEMON/-/v1/status | jq -r .baseline_commit)
git diff "$BASE" --relative \
  | curl -f -X POST -H "Authorization: Bearer $HOTLANE_TOKEN" \
      --data-binary @- "$HOTLANE_DAEMON/-/v1/push"
```

The response is JSON: fork phase timings, per-hook verify results, `promoted` true/false, and the fork's last logs on rejection. `POST /-/v1/test` (same body) holds the verified fork instead of promoting - reach it with the `X-Hotlane-Fork: <version>` header on the app URL, then `POST /-/v1/promote` or `/-/v1/discard` (`{"version": N}`). `POST /-/v1/rollback` and `POST /-/v1/drift-check` complete the surface. (On the private API port, bare `/v1/...` paths work as aliases.)

## Isn't this mutable infrastructure?

Yes - deliberately, on the serving path, and no on the artifact path.

The fast lane mutates: a fork is a snapshot of the *running* container, so it inherits the warm filesystem (installed dependencies, build caches, a warm page cache) and the delta applies in milliseconds instead of minutes. That is where the speed comes from, and pretending otherwise would be dishonest.

The trust lane does not mutate. After every promote the archivist rebuilds that exact version **from source, from scratch**, pushes it to your registry, and periodically cold-boots it to diff its behavior against what is actually live. So:

- every promoted version has a reproducible, registry-pushed image - the artifact classical CI would have made, produced off the critical path instead of in front of the user
- divergence between the running system and a from-source rebuild is *detected*, not assumed away, and pings your webhook
- a drifted app self-heals: the next ordinary push forks from the clean image instead of the warm chain
- fork chains rebase onto the clean image every ~40 pushes anyway, so the mutable chain never grows unbounded

The usual argument for immutability is "you cannot trust a machine you have been mutating." The answer here is not faith, it is a continuously running experiment that would catch it. What immutable pipelines give you by construction, hotlane gives you by verification - and hands you the seconds back.

## What happens if the box dies?

You lose the app, exactly as you would lose any single-instance deployment - hotlane is not an HA system and does not pretend to be. What survives is everything needed to rebuild: your source in git, and every promoted version as a registry image the archivist pushed. Recovery is `docker run` of the last archived image on another box, or `hotlane serve` there and a push.

If you need redundancy today, the honest answer is that hotlane is the wrong layer alone: run it per-box behind your own load balancer (each daemon deploys independently), or keep a managed platform for prod and use hotlane for the fast dev/staging/agent loop with the archivist's images feeding the prod pipeline. A gossiped multi-host version ring is on the [roadmap](roadmap.md) and is genuinely unbuilt.

## Where does hotlane run (and not run)?

hotlane runs **on a machine you own with Docker**: a VPS, bare metal, an EC2 instance. It does not run on ECS, Fargate, Cloud Run, or Kubernetes, and not inside your app's image - it works by commanding the host's Docker daemon (forking live containers, keeping the version ring, fronting them with its proxy), which is exactly the layer managed platforms own themselves. hotlane is an alternative to that layer, not a passenger on it: the AWS equivalent of an ECS service is one EC2 instance running `hotlane serve`.

Teams keeping a managed platform for prod can split the loops: hotlane on a cheap box for dev/staging/agent iteration, while the archivist's registry-pushed, from-source images feed the existing prod pipeline - every archived version is a normal image ECS or Kubernetes can deploy directly.

**Could it ever run there?** Per platform, honestly:

- **Cloud Run / Fargate**: no. There is no "snapshot this running container" API, no exec into the sandbox, and routing belongs to the platform. Nothing of the model survives.
- **ECS on EC2**: only by smuggling - a privileged Docker-in-Docker task with hotlane commanding the nested daemon. It works and it is pointless: the platform sees one opaque container, you lose its scheduling, and a plain EC2 instance is strictly better.
- **Kubernetes**: the one port that could preserve the promise, as an operator rather than this binary. Warm standby pods stand in for filesystem snapshots, the delta goes in via exec, and promote flips a Service label selector - which genuinely is a fast atomic pointer flip. That is a different product sharing this one's philosophy, not a flag on this one.

## Is the API safe to expose?

`-tls-domain` / `-tls` refuse to start without a token; tokens are compared in constant time; only `/-/healthz` is unauthenticated (and on a multi-app daemon it reports a count, never app names). App traffic on `https://yourdomain/` is untouched by any of this - it is your public app.

Two things to know rather than discover:

- **The daemon needs the Docker socket.** Anything that can talk to Docker can root the box, so treat the hotlane user as root-equivalent and the API token as a root credential. Keep the API on loopback (or a private network) unless you have a reason not to; `-tls` exists for when CI must reach it across the internet.
- **Traffic replay buffers real user data in memory**, request headers included - stripping auth would make every replayed request a 401 and the feature useless. It is memory-only, capped, never written to disk, never archived, and dies with the process. If your traffic is sensitive enough that this matters, use `exclude:` for those paths or leave `replay:` off (it is off by default).

## Running the daemon under systemd

On the box itself, run `hotlane serve` as a service so it survives reboots. A ready unit file ships in [`packaging/systemd/hotlane.service`](../packaging/systemd/hotlane.service):

```bash
sudo useradd -r -G docker -s /usr/sbin/nologin hotlane
sudo cp packaging/systemd/hotlane.service /etc/systemd/system/
sudo sh -c 'mkdir -p /etc/hotlane && echo HOTLANE_TOKEN=$(openssl rand -hex 24) > /etc/hotlane/env && chmod 600 /etc/hotlane/env'
# edit WorkingDirectory in the unit to your app checkout, then:
sudo systemctl enable --now hotlane
```

Services run without `$HOME`, so the daemon keeps its state (fork ring, clean-image snapshots, autocert cache) in `/var/lib/hotlane` - `StateDirectory=hotlane` in the unit has systemd create it with the right ownership. Secrets stay in `/etc/hotlane/env`: `HOTLANE_TOKEN` becomes the API token, and any `${VAR}` references in `hotlane.yml` (e.g. `notify: ${HOTLANE_NOTIFY_URL}`) interpolate from the same file.

Hosting several apps on the box? Point the unit at a config directory instead: `ExecStart=/usr/local/bin/hotlane serve -apps /etc/hotlane/apps -tls`, one `*.yml` per app (each with `src:` and `domain:`), no `WorkingDirectory` needed. One unit, one token, every app.

## Where the trust guarantees live

Nothing about CI integration weakens the model: a push from CI goes through the same fork-verify-flip gate as a local one, the ring keeps instant rollback, and the archivist still produces the registry-pushed, from-source image for every promoted version plus behavioral drift checks against it. Your pipeline gets faster; the audit trail stays.
