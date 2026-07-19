//! # hotlane - validation-first deployment
//!
//! Push a change and hotlane forks the app that is *already running*,
//! applies your delta, verifies the fork in isolation, and flips traffic
//! to it - about a second, end to end. Rollback is a pointer, not a
//! pipeline.
//!
//! **This crate does not install the CLI** (a Go binary); it holds the
//! name for a possible future cargo-native distribution. Install via
//! `npm install -g hotlane`, `pip install hotlane`, or a binary from
//! <https://github.com/StefanIancu/hotlane/releases>.
//!
//! Website: <https://hotlane.dev>

/// Crate version. See the crate-level docs: the CLI itself is installed
/// via npm/pip, not cargo.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
