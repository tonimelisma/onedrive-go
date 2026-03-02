#!/usr/bin/env bash
# Bootstrap test credentials for a single account.
# Usage: ./scripts/bootstrap-test-credentials.sh
#
# Runs the login flow with XDG overrides pointing to .testdata/.
# The login command creates the cache file (token + metadata) AND
# updates config.toml (adds drive section with sync_dir).
#
# Run once per test account. Config accumulates drive sections.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TESTDATA="$REPO_ROOT/.testdata"

# XDG subdirs (login writes to $XDG_DATA_HOME/onedrive-go/ and
# $XDG_CONFIG_HOME/onedrive-go/)
DATA_SUBDIR="$TESTDATA/data/onedrive-go"
CONFIG_SUBDIR="$TESTDATA/config/onedrive-go"
mkdir -p "$DATA_SUBDIR" "$CONFIG_SUBDIR"

echo "=== Test Credential Bootstrap ==="
echo "Login output will go to .testdata/ (not production)"
echo ""

XDG_DATA_HOME="$TESTDATA/data" \
XDG_CONFIG_HOME="$TESTDATA/config" \
XDG_CACHE_HOME="$TESTDATA/cache" \
HOME="$TESTDATA/home" \
  go run . login

# Flatten: move files from subdirs to .testdata/ root
mv "$DATA_SUBDIR"/token_*.json "$TESTDATA/" 2>/dev/null || true
cp "$CONFIG_SUBDIR/config.toml" "$TESTDATA/config.toml" 2>/dev/null || true

# Clean up subdirs
rm -rf "$TESTDATA/data" "$TESTDATA/config" "$TESTDATA/cache" "$TESTDATA/home"

echo ""
echo "=== Bootstrap complete ==="
ls -la "$TESTDATA/"
echo ""
echo "Run again for additional test accounts (config.toml accumulates drive sections)."
