# Design sketch: traffic-replay verification

Status: shipped in v0.6.0 as sketched; phase 2 (the slice in drift
checks) shipped in v0.6.1, which also fixed a phase-1 latency bug in the
model itself: recorded exchanges describe the version that served them,
so EVERY traffic flip (promote, held-promote, rollback) now resets the
buffer - without that, the second of two rapid pushes replayed stale
expectations and false-positived. Measured capture overhead on the live
path: ~1&micro;s/request (480ns bare vs 1.45&micro;s captured,
BenchmarkCapture) - noise against millisecond-scale request handling.

## Problem

Verify hooks are author-written guesses about what matters: `/health ==
200` proves the app boots, `./smoke.sh` proves what you remembered to
check. Neither proves that the endpoint real users hit 400 times a minute
still behaves. The richest regression suite for a running service is its
own recent traffic - and hotlane is uniquely positioned to use it: the
proxy already sees every live request, and every push already produces a
warm, isolated fork that is about to become production.

## Shape

Record a rolling slice of live traffic; before promoting a fork, replay
that slice against it and diff the fork's answers against what live
actually answered. Same comparison machinery as the v0.4.2 drift checks:
volatile patterns masked, self-dynamic paths compare status only.

```yaml
# hotlane.yml
replay:
  last: 200              # replay the most recent N buffered requests (0/absent = off)
  mode: report           # report (annotate the push) | gate (mismatch rejects it)
  methods: [GET, HEAD]   # what gets captured and replayed; safe-by-default
  exclude: [/metrics]    # paths never captured
  budget: 5s             # replay time cap; partial coverage is reported, never silent
```

## Capture

- The app-traffic proxy keeps an in-memory ring per app: method, path,
  headers, request body (capped), and the response live actually served
  (status + body, capped) - the recorded response is the diff baseline,
  so replay adds ZERO extra load on live.
- Memory only, never disk: the buffer holds real user data; it dies with
  the process and is never archived. Default caps: 512 requests, 64KB
  body each, sampled 1-in-N under load if the ring churns too fast.
- Auth headers are kept (in memory) and replayed as-is - the fork is the
  same app with the same secrets; stripping them would make every
  replayed request a 401 and the whole feature would test the login page.
- Only `methods:` are captured (default GET/HEAD). Excluded paths and
  the `/-/` API prefix are never recorded.

## Replay

After verify hooks pass (and before promote/hold):

1. Take the newest `last:` entries from the ring.
2. Fire them at the fork's backend, small fixed concurrency (4), until
   done or `budget:` expires.
3. Compare each fork response against the recorded live response using
   the drift normalizer (timestamps/UUIDs/hex ids/epoch numbers masked).
   A path that appears more than once in the buffer with differing
   normalized live bodies is self-dynamic: status-only comparison, same
   rule as drift checks.
4. Attach the result to the push/test response:

```json
"replay": {"replayed": 200, "matched": 197, "dynamic": 2, "mismatched": 1,
           "coverage": "200/512 buffered", "budget_hit": false,
           "mismatches": [{"path": "/api/items", "want": "...", "got": "..."}]}
```

- `mode: report` - mismatches annotate the output and the notify webhook;
  the push still promotes. The trust-building default.
- `mode: gate` - any mismatch rejects the push exactly like a failing
  verify hook: fork destroyed, live untouched, mismatch detail in the
  422. For apps whose hook-path responses are deterministic enough that
  report mode has stayed quiet.

`hotlane test` composes naturally: hold the fork, replay against it, let
the agent read the diff report before deciding promote/discard.

## Why not replay writes

A fork's own state is isolated, but its *external* side effects are not:
a replayed `POST /orders` can hit the same Stripe, the same SMTP, the
same shared database as live. Double-charging a customer to verify a
deploy is not a trade. Writes stay out unless explicitly opted in via
`methods:`, and the docs will say exactly why that's a loaded gun.

## Synergy: drift checks (phase 2)

The archivist's 6-hourly drift check compares cold boot vs live on the
verify-hook paths only. Once the replay buffer exists, the same slice can
replay against the cold boot too - behavioral drift coverage across the
endpoints users actually exercise, not just the ones named in config.
Same normalizer, same report shape, zero new machinery.

## Implementation plan

1. `internal/replay`: the ring buffer + capture middleware (records only
   when `replay.last > 0`), unit-tested against httptest servers.
2. Capture wiring in the app-traffic path; buffer surfaced in status
   (`replay: {buffered: N}`) so operators can see it fill.
3. Replayer + comparison (reuse archive's normalize/self-dynamic logic -
   export it from a shared spot), `replay` block in push/test responses.
4. `mode: gate` rejection path + notify events.
5. e2e scenario: buffer real requests, push a change that alters an
   unrelated-to-hooks endpoint, watch report mode flag it and gate mode
   reject it.
6. Phase 2: replay slice in DriftCheck.

## Open questions

- Default `mode` when `replay:` is configured: report (safe, builds
  trust) or gate (the feature's whole point)? Leaning report.
- Should `hotlane status` expose recent mismatch history, or is the
  push/test output + webhook enough?
- Per-path opt-in for write replay (`methods: [POST]` under an explicit
  `unsafe_paths:`): worth the loaded gun at all, or refuse on principle?
- Buffer persistence across daemon restarts: memory-only loses the slice
  on every restart (first push after boot has nothing to replay). Accept,
  or spill sanitized entries to the state dir? Leaning accept - privacy
  beats coverage here.
