#!/usr/bin/env bash
# Builds the loro_cabi static library that internal/loro links against.
# Output: target/release/libloro_cabi.a (git-ignored). Extra arguments are
# passed to cargo, e.g. ./build.sh --locked.
set -euo pipefail
cd "$(dirname "$0")"
cargo build --release "$@"
