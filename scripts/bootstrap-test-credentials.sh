#!/usr/bin/env bash
# Bootstrap test credentials for a single account.
# Usage: ./scripts/bootstrap-test-credentials.sh
#
# Runs the interactive login flow with XDG overrides pointing to .testdata/.
# The login command creates the token file (token + metadata) AND
# updates config.toml (adds drive section with sync_dir).
#
# Run once per test account. Config accumulates drive sections across runs.
# After bootstrapping all accounts, run scripts/migrate-test-data-to-ci.sh
# to upload credentials to Azure Key Vault for CI.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TESTDATA="$REPO_ROOT/.testdata"

# XDG subdirs (login writes to $XDG_DATA_HOME/onedrive-go/ and
# $XDG_CONFIG_HOME/onedrive-go/)
DATA_SUBDIR="$TESTDATA/data/onedrive-go"
CONFIG_SUBDIR="$TESTDATA/config/onedrive-go"
mkdir -p "$DATA_SUBDIR" "$CONFIG_SUBDIR"

# Preserve existing config so login adds a new drive section to it
# rather than creating a fresh config (multi-account accumulation).
if [ -f "$TESTDATA/config.toml" ]; then
    cp "$TESTDATA/config.toml" "$CONFIG_SUBDIR/config.toml"
fi

# Preserve existing token files so they survive the subdir cleanup.
for f in "$TESTDATA"/token_*.json; do
    [ -f "$f" ] && cp "$f" "$DATA_SUBDIR/"
done

echo "=== Test Credential Bootstrap ==="
echo "Login output will go to .testdata/ (not production)"
echo ""

XDG_DATA_HOME="$TESTDATA/data" \
XDG_CONFIG_HOME="$TESTDATA/config" \
XDG_CACHE_HOME="$TESTDATA/cache" \
HOME="$TESTDATA/home" \
  go run . login

# Flatten: move files from subdirs to .testdata/ root.
# cp (not mv) so existing tokens from prior runs are preserved.
cp "$DATA_SUBDIR"/token_*.json "$TESTDATA/" 2>/dev/null || true
cp "$CONFIG_SUBDIR/config.toml" "$TESTDATA/config.toml" 2>/dev/null || true

# Clean up subdirs.
rm -rf "$TESTDATA/data" "$TESTDATA/config" "$TESTDATA/cache" "$TESTDATA/home"

echo ""
echo "=== Bootstrap complete ==="
ls -la "$TESTDATA/"
echo ""
echo "Run again for additional test accounts (config.toml accumulates drive sections)."
echo "Then run: ./scripts/migrate-test-data-to-ci.sh"
