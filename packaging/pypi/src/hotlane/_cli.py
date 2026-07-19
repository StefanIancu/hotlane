"""Launcher: fetch the platform binary once, then exec it forever after.

PyPI wheels cannot run post-install hooks, so the download happens on the
first `hotlane` invocation and is cached under ~/.cache/hotlane/<version>.
"""

import os
import platform
import ssl
import stat
import sys
import tarfile
import tempfile
import urllib.request

import certifi

# The daemon/CLI release this wrapper fetches. Deliberately decoupled from
# the package version so wrapper-only fixes don't require binary releases.
BINARY_VERSION = "0.1.1"

_OS = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
_ARCH = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(
    platform.machine()
)


def _cache_dir() -> str:
    base = os.environ.get("XDG_CACHE_HOME", os.path.expanduser("~/.cache"))
    return os.path.join(base, "hotlane", BINARY_VERSION)


def _ensure_binary() -> str:
    path = os.path.join(_cache_dir(), "hotlane")
    if os.path.exists(path):
        return path
    if not _OS or not _ARCH:
        sys.exit(
            f"hotlane: unsupported platform {platform.system()}/{platform.machine()}\n"
            "build from source: go install github.com/StefanIancu/hotlane/cmd/hotlane@latest"
        )
    url = (
        "https://github.com/StefanIancu/hotlane/releases/download/"
        f"v{BINARY_VERSION}/hotlane_{_OS}_{_ARCH}.tar.gz"
    )
    os.makedirs(_cache_dir(), exist_ok=True)
    print(f"hotlane: fetching {url}", file=sys.stderr)
    try:
        # certifi's bundle, not the interpreter's: stock macOS Pythons often
        # ship without a usable system trust store.
        ctx = ssl.create_default_context(cafile=certifi.where())
        with tempfile.NamedTemporaryFile(suffix=".tar.gz", delete=False) as tmp:
            with urllib.request.urlopen(url, context=ctx) as resp:
                tmp.write(resp.read())
            tmp_path = tmp.name
        with tarfile.open(tmp_path) as tar:
            member = tar.getmember("hotlane")
            tar.extract(member, _cache_dir())
        os.unlink(tmp_path)
    except Exception as exc:  # noqa: BLE001 - single user-facing failure path
        sys.exit(f"hotlane: failed to download binary: {exc}")
    os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return path


def main() -> None:
    binary = _ensure_binary()
    os.execv(binary, [binary, *sys.argv[1:]])
