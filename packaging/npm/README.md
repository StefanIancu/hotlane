# hotlane

**Validation-first deployment.** Push a change and hotlane forks the app that is *already running*, applies your delta, verifies the fork in isolation, and flips traffic to it - about a second, end to end. Rollback is a pointer, not a pipeline. The reproducible container image is built in the background, off the critical path.

![hotlane demo](https://raw.githubusercontent.com/StefanIancu/hotlane/main/docs/demo.gif)

## Install

```bash
npm install -g hotlane
```

This package fetches the platform binary (Linux/macOS, amd64/arm64) from GitHub Releases at install time. One binary - it is both the daemon and the CLI.

## Quickstart

```bash
cd your-app
hotlane init      # detects Node / Python / Go, writes hotlane.yml
hotlane serve     # boots the warm pool, proxies traffic

# make a change, then:
hotlane push
#   ok   http: /health == 200 (13ms)
# PROMOTED v2 live in 978ms

hotlane rollback  # flip back, sub-second
```

Runs anywhere Docker runs: your VPS, bare metal, an EC2 instance. Benchmarked at ~1-2s push-to-verified-live where a classical pipeline took 8+ minutes.

## Learn more

- Website: https://hotlane.dev
- Docs, CI integration (GitHub Actions, GitLab, CodeBuild), benchmark method: https://github.com/StefanIancu/hotlane

MIT licensed.
