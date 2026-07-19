#!/usr/bin/env node
// Thin shim: exec the platform binary fetched by install.js.
const path = require("path");
const { spawnSync } = require("child_process");

const bin = path.join(__dirname, "hotlane");
const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
process.exit(res.status ?? 1);
