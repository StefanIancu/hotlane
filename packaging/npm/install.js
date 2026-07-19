// Fetches the platform binary from GitHub Releases at install time and
// places it next to the shim. Runs as the package's postinstall hook.
const fs = require("fs");
const path = require("path");
const { execFileSync } = require("child_process");

// The daemon/CLI release this wrapper fetches. Deliberately decoupled from
// the package version so wrapper-only fixes don't require binary releases.
const BINARY_VERSION = "0.4.2";

const OS = { darwin: "darwin", linux: "linux" }[process.platform];
const ARCH = { x64: "amd64", arm64: "arm64" }[process.arch];

if (!OS || !ARCH) {
  console.error(`hotlane: unsupported platform ${process.platform}/${process.arch}`);
  console.error("build from source instead: go install github.com/StefanIancu/hotlane/cmd/hotlane@latest");
  process.exit(1);
}

const url = `https://github.com/StefanIancu/hotlane/releases/download/v${BINARY_VERSION}/hotlane_${OS}_${ARCH}.tar.gz`;
const dir = path.join(__dirname, "bin");
const tarball = path.join(dir, "hotlane.tar.gz");

fs.mkdirSync(dir, { recursive: true });

fetch(url)
  .then((res) => {
    if (!res.ok) throw new Error(`${res.status} ${res.statusText} for ${url}`);
    return res.arrayBuffer();
  })
  .then((buf) => {
    fs.writeFileSync(tarball, Buffer.from(buf));
    execFileSync("tar", ["-xzf", tarball, "-C", dir, "hotlane"]);
    fs.unlinkSync(tarball);
    fs.chmodSync(path.join(dir, "hotlane"), 0o755);
    console.log(`hotlane ${BINARY_VERSION} installed (${OS}/${ARCH})`);
  })
  .catch((err) => {
    console.error(`hotlane: failed to download binary: ${err.message}`);
    process.exit(1);
  });
