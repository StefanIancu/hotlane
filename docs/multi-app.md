# Design sketch: multi-app daemons (0.5)

Status: sketch, pre-implementation. Decision made: **static configuration** — the
daemon reads a directory of config files at startup; there is no runtime
registration API. `hotlane app add` may later become sugar that writes a file
into the directory, keeping disk the single source of truth.

## Problem

One daemon serves one app. That is fine until the box hosts a second thing:

- `-tls-domain` binds :443, so exactly one app per host gets
  `https://domain/` with built-in TLS. Everything else is back to manual
  reverse-proxy plumbing - on the very box hotlane claims to own well.
- N apps means N daemons: N API ports, N tokens, N systemd units, and no
  single status surface for the machine.

## Shape

One daemon, N apps. Each app keeps its own full machinery - warm pool, ring,
held forks, archivist, drift verdict, notifier - and they share the front
doors: one API listener, one proxy/TLS listener, one autocert manager, one
token.

```
hotlane serve -apps /etc/hotlane/apps/
```

Every `*.yml` in the directory is one app. Adding/removing an app is a file
change + daemon restart (restarts already adopt running containers without
dropping traffic, so this is invisible to users).

## Config additions

Two new fields, both only meaningful in multi-app mode:

```yaml
# /etc/hotlane/apps/api.yml
app: api
src: /srv/api                # REQUIRED in -apps mode: the checkout to snapshot/diff against
domain: api.example.com      # host-header route + TLS cert (required with -tls, optional without)
image: node:22-alpine
run: node dist/server.js
port: 3000
...
```

- `src`: today the daemon infers the source tree from the config file's
  directory (`filepath.Dir(cfgPath)`), because serve starts inside the repo.
  With configs living in /etc/hotlane/apps, that inference breaks; `src`
  makes it explicit. Single-app mode keeps the old inference; `src` overrides
  it if present (also useful today for running serve outside the repo).
- `domain`: the routing key. Multiple domains later (`domains:`) if needed;
  start with one.

Validation at startup, all-or-nothing: duplicate `app` names, duplicate
`domain`s, missing `src` -> the daemon refuses to start with all problems
listed (same style as config.validate today). A half-loaded daemon that
silently skipped one app is how apps disappear unnoticed.

## Routing

**App traffic** - one listener, Host header picks the app:

- With `-tls-domain` replaced by `-tls` (flag becomes a mode, domains come
  from the configs): the shared :443 listener looks up `r.Host` in the
  domain map. autocert's HostPolicy is the union of all `domain:` values;
  one cert cache, one manager.
- Unknown host -> 421 with a short body listing nothing (no app enumeration
  to strangers). No default app, no fall-through: serving app A's traffic to
  app B because of a typo'd DNS record must be impossible.
- The `X-Hotlane-Fork` header keeps working per app: resolve the app by
  Host first, then the header against THAT app's held forks.
- Plain-HTTP mode (`-proxy :7480`, no TLS): same Host-based routing on the
  single proxy port. For local use without DNS, `curl -H "Host: api.local"`
  or the app's backend address from status. An app with no `domain` in
  plain mode is reachable only via its backend port - fine for dev.

**API** - app name in the path:

    GET  /-/v1                        index; now also lists apps
    GET  /-/v1/apps                   [{app, domain, version, drift}, ...]
    POST /-/v1/apps/<app>/push        everything that exists today, per app
    GET  /-/v1/apps/<app>/status      ...same for test/promote/discard/
                                      rollback/drift-check/logs

Back-compat rule: when the daemon serves EXACTLY ONE app, today's unprefixed
routes (`/-/v1/push` etc.) keep working as aliases. A single-app daemon
upgraded to 0.5 breaks zero clients, zero CI jobs, zero MCP configs. With
two or more apps the bare routes return 400 with "this daemon serves
multiple apps; use /-/v1/apps/<app>/..." - loud, not silent.

**CLI/MCP**: no new flags for the common case. Client commands already run
inside the app repo; they read `app:` from the local hotlane.yml and target
`/-/v1/apps/<name>/...`. Old daemon + new CLI: on 404 from the namespaced
path, fall back to the bare path once (and vice versa for new daemon + old
CLI via the alias rule above). MCP is a client; it inherits this for free.

`hotlane status` gains `-all`: daemon-wide view from `/-/v1/apps`.

## Internal structure

cmdServe's per-app wiring (pool + proxy target + archivist + notifier +
reaper + drift ticker) moves into a constructor:

```go
type appRuntime struct {
    cfg   *config.Config
    pool  *pool.Pool      // DataDir: <root>/<app>
    front *proxy.Proxy
    arch  *archive.Archivist
    notif *notify.Notifier
}

apps map[string]*appRuntime   // by app name
byHost map[string]*appRuntime // by domain
```

pool, archive, notify, verify are already per-app structs with no package
globals - this refactor is main.go-local. The API handlers close over a
lookup instead of a single `p`/`arch`/`front`.

## What is shared vs per-app

| shared | per-app |
|---|---|
| API listener + token | pool, ring, held forks |
| proxy/TLS listener, autocert manager | archivist + clean image + drift verdict |
| state root (`~/.hotlane` or `/var/lib/hotlane`) | `<root>/<app>/` subtree (already namespaced) |
| build semaphore (below) | notifier (per-app `notify:` URL) |
| drift ticker (staggered, below) | push serialization (per-app mutex, as today) |

**Build semaphore**: pushes are serialized per app; with N apps, pushes and
archivist builds can now overlap across apps on one Docker daemon. A global
semaphore (2 concurrent docker builds) keeps an agent hammering app A from
starving app B's push, without letting 5 apps build at once on a small box.

**Drift ticker**: one 6h ticker walking the apps with a small stagger
between them, instead of N tickers cold-booting N containers at the same
instant.

## Non-goals (unchanged from the roadmap)

- Dynamic registration / app CRUD over the API. The set of apps is what is
  on disk. (Future: `hotlane app add` as local sugar that writes the file.)
- Cross-host anything. One daemon, one box.
- Per-app tokens/authorization. One token per daemon, as today. If the box
  serves apps with different trust domains, run two daemons on different
  API ports - still possible, just no longer necessary for the common case.

## Implementation plan

1. **Refactor, no behavior change**: extract `appRuntime` + constructor from
   cmdServe; single-app mode wires exactly one. e2e must pass untouched.
2. **Config**: `src:` field (with single-app inference fallback), `domain:`
   field, `-apps DIR` flag, startup validation across the set.
3. **Routing**: host map on the proxy path, `/-/v1/apps/<app>/` API routes,
   single-app aliases, `/-/v1/apps` index, autocert union.
4. **CLI**: clients target namespaced paths with fallback; `status -all`.
5. **Shared plumbing**: build semaphore, staggered drift ticker.
6. **e2e**: new scenario - two apps from one `-apps` dir, host-routed pushes,
   one app's rejected push leaves the other untouched, `status -all`,
   single-app back-compat run of the existing suite.

Steps 1-2 are mechanical; 3 is the real work; each step lands green.

## Decided (2026-07-19)

- `-tls-domain` keeps working in single-app mode as an override of
  `domain:` - back-compat is cheap here.
- `/-/v1/apps` sits behind the token like every other route (only healthz
  is open); no app-name enumeration on an unauthenticated daemon.
- Held-fork cap is **3 per app**, not per daemon. The cap bounds one
  workload's appetite; a per-daemon cap would let an agent holding forks on
  app A block anyone from testing app B - cross-app interference the rest
  of the design deliberately avoids.
