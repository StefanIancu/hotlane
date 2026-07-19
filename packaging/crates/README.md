# hotlane

**Validation-first deployment.** Push a change and hotlane forks the app that is *already running*, applies your delta, verifies the fork in isolation, and flips traffic to it - about a second, end to end. Rollback is a pointer, not a pipeline. The reproducible container image is built in the background, off the critical path.

## Installing hotlane

The hotlane CLI/daemon is a Go binary; **this crate does not install it** - it holds the name for a possible future cargo-native distribution. Install via:

```bash
npm install -g hotlane     # or: pip install hotlane
```

or grab a binary from [GitHub Releases](https://github.com/StefanIancu/hotlane/releases).

## Quickstart

```bash
cd your-app
hotlane init      # detects Node / Python / Go, writes hotlane.yml
hotlane serve     # boots the warm pool, proxies traffic
hotlane push      # ~1s later: verified and live
hotlane rollback  # flip back, sub-second
```

## Learn more

- Website: https://hotlane.dev
- Docs, CI integration, benchmark method: https://github.com/StefanIancu/hotlane

MIT licensed.
