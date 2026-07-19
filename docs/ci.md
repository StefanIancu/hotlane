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
                                                  │ HTTPS or SSH tunnel, bearer token
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

The API (`:7433`) should never be plainly exposed. Pick one:

- **SSH tunnel (recommended default)** - zero server-side setup beyond an SSH key:
  ```bash
  ssh -f -N -L 7433:127.0.0.1:7433 deploy@your-server
  HOTLANE_DAEMON=http://127.0.0.1:7433 hotlane push
  ```
- **Private network** - WireGuard, Tailscale, or a VPC: point `HOTLANE_DAEMON` at the private IP.
- **TLS reverse proxy** - put Caddy/nginx/Traefik in front of `:7433` with TLS and use `HOTLANE_DAEMON=https://deploy.example.com`.

In every case, run the daemon with a token (`hotlane serve -token ...` or `HOTLANE_TOKEN`) and give CI the same token as a secret. All API routes except `/healthz` require it.

## GitHub Actions

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

      - name: Install hotlane
        run: |
          curl -fsSL https://github.com/StefanIancu/hotlane/releases/latest/download/hotlane_linux_amd64.tar.gz \
            | sudo tar -xz -C /usr/local/bin hotlane

      - name: Open tunnel to the daemon
        run: |
          install -m 600 /dev/null key && echo "${{ secrets.DEPLOY_SSH_KEY }}" > key
          ssh -i key -o StrictHostKeyChecking=accept-new \
              -f -N -L 7433:127.0.0.1:7433 deploy@${{ secrets.DEPLOY_HOST }}

      - name: Push
        env:
          HOTLANE_TOKEN: ${{ secrets.HOTLANE_TOKEN }}
        run: hotlane push
```

The push output (phase timings, verify hooks, PROMOTED/REJECTED) becomes the job log, and a rejected push fails the job with the fork's logs attached.

**Rollback as a manual action:**

```yaml
name: rollback
on:
  workflow_dispatch:
    inputs:
      version:
        description: "version to roll back to (empty = previous)"
        required: false
jobs:
  rollback:
    runs-on: ubuntu-latest
    steps:
      # ...install + tunnel steps as above...
      - env:
          HOTLANE_TOKEN: ${{ secrets.HOTLANE_TOKEN }}
        run: hotlane rollback ${{ inputs.version }}
```

**Drift check as a scheduled job** - `hotlane drift` exits non-zero on drift, so a red scheduled run is your alarm (on top of the daemon's own `notify:` webhook):

```yaml
on:
  schedule: [{ cron: "0 6 * * *" }]
# ...install + tunnel...
#   run: hotlane drift
```

## GitLab CI

```yaml
deploy:
  stage: deploy
  resource_group: production          # serializes deploys
  variables:
    GIT_DEPTH: 0
  script:
    - curl -fsSL https://github.com/StefanIancu/hotlane/releases/latest/download/hotlane_linux_amd64.tar.gz | tar -xz
    - install -m 600 /dev/null key && echo "$DEPLOY_SSH_KEY" > key
    - ssh -i key -o StrictHostKeyChecking=accept-new -f -N -L 7433:127.0.0.1:7433 deploy@$DEPLOY_HOST
    - ./hotlane push
  only: [main]
```

Set `DEPLOY_SSH_KEY`, `DEPLOY_HOST`, and `HOTLANE_TOKEN` as protected CI/CD variables.

## AWS CodeBuild

```yaml
version: 0.2
env:
  parameter-store:
    HOTLANE_TOKEN: /myapp/hotlane-token
    DEPLOY_SSH_KEY: /myapp/deploy-ssh-key
phases:
  install:
    commands:
      - curl -fsSL https://github.com/StefanIancu/hotlane/releases/latest/download/hotlane_linux_amd64.tar.gz | tar -xz -C /usr/local/bin hotlane
  build:
    commands:
      - git fetch --unshallow || true
      - install -m 600 /dev/null key && echo "$DEPLOY_SSH_KEY" > key
      - ssh -i key -o StrictHostKeyChecking=accept-new -f -N -L 7433:127.0.0.1:7433 deploy@$DEPLOY_HOST
      - hotlane push
```

(If the daemon host is in the same VPC as CodeBuild, skip the tunnel and set `HOTLANE_DAEMON=http://<private-ip>:7433`.)

## Anything else (Jenkins, CircleCI, a cron job, an agent)

The API is one endpoint - any environment that has git and curl can deploy:

```bash
BASE=$(curl -s -H "Authorization: Bearer $HOTLANE_TOKEN" $HOTLANE_DAEMON/v1/status | jq -r .baseline_commit)
git diff "$BASE" --relative \
  | curl -f -X POST -H "Authorization: Bearer $HOTLANE_TOKEN" \
      --data-binary @- "$HOTLANE_DAEMON/v1/push"
```

The response is JSON: fork phase timings, per-hook verify results, `promoted` true/false, and the fork's last logs on rejection. `POST /v1/rollback` (`{"version": N}` or empty for previous) and `POST /v1/drift-check` complete the surface.

## Where the trust guarantees live

Nothing about CI integration weakens the model: a push from CI goes through the same fork-verify-flip gate as a local one, the ring keeps instant rollback, and the archivist still produces the registry-pushed, from-source image for every promoted version plus behavioral drift checks against it. Your pipeline gets faster; the audit trail stays.
