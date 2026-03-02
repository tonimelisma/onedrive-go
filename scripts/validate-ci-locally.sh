#!/usr/bin/env bash
# validate-ci-locally.sh — Mirror the integration.yml workflow locally.
#
# Use this script before pushing changes that affect CI token paths, secret
# naming, environment variables, or workflow logic. It catches issues like
# wrong secret names or broken token paths without waiting for GitHub Actions.
#
# Usage:
#   ./scripts/validate-ci-locally.sh [DRIVE]
#
# Arguments:
#   DRIVE  Canonical drive ID (default: from ONEDRIVE_TEST_DRIVES or gh variable)
#
# Prerequisites:
#   - az CLI logged in (az login)
#   - gh CLI authenticated
#   - go toolchain installed
#   - jq installed

set -euo pipefail

VAULT_NAME="kv-onedrivego-ci"
DATA_DIR="$HOME/.local/share/onedrive-go"

# Determine drive ID.
if [ $# -ge 1 ]; then
    DRIVE="$1"
elif [ -n "${ONEDRIVE_TEST_DRIVES:-}" ]; then
    # Take the first drive from comma-separated list.
    DRIVE=$(echo "$ONEDRIVE_TEST_DRIVES" | cut -d',' -f1 | xargs)
else
    echo "Fetching ONEDRIVE_TEST_DRIVES from GitHub..."
    DRIVE=$(gh variable get ONEDRIVE_TEST_DRIVES 2>/dev/null | cut -d',' -f1 | xargs) || true
    if [ -z "$DRIVE" ]; then
        echo "ERROR: No drive specified. Pass a canonical drive ID or set ONEDRIVE_TEST_DRIVES."
        echo "Usage: $0 personal:user@example.com"
        exit 1
    fi
fi

echo "=== Local CI Validation ==="
echo "Drive:     $DRIVE"
echo "Vault:     $VAULT_NAME"
echo "Data dir:  $DATA_DIR"
echo ""

# Step 1: Derive names (same logic as integration.yml).
SANITIZED=$(echo "$DRIVE" | sed 's/:/_/')
TOKEN_FILE="token_${SANITIZED}.json"
TOKEN_PATH="${DATA_DIR}/${TOKEN_FILE}"
SECRET_NAME="onedrive-oauth-token-$(echo "$DRIVE" | sed 's/[:@.]/-/g')"

echo "--- Derived names ---"
echo "Token file:  $TOKEN_FILE"
echo "Token path:  $TOKEN_PATH"
echo "Secret name: $SECRET_NAME"
echo ""

# Step 2: Verify az CLI access.
echo "--- Checking Azure access ---"
if ! az account show --query "name" -o tsv > /dev/null 2>&1; then
    echo "ERROR: az CLI not logged in. Run: az login"
    exit 1
fi
echo "Azure account: $(az account show --query 'name' -o tsv)"

# Step 3: Verify secret exists in Key Vault.
echo ""
echo "--- Checking Key Vault secret ---"
if ! az keyvault secret show --vault-name "$VAULT_NAME" --name "$SECRET_NAME" --query "name" -o tsv > /dev/null 2>&1; then
    echo "ERROR: Secret '$SECRET_NAME' not found in vault '$VAULT_NAME'"
    echo ""
    echo "Available secrets:"
    az keyvault secret list --vault-name "$VAULT_NAME" --query "[].name" -o tsv
    exit 1
fi
echo "Secret found: $SECRET_NAME"

# Step 4: Download token and validate structure.
echo ""
echo "--- Downloading and validating token ---"
mkdir -p "$DATA_DIR"
TEMP_TOKEN=$(mktemp)
az keyvault secret download \
    --vault-name "$VAULT_NAME" \
    --name "$SECRET_NAME" \
    --file "$TEMP_TOKEN" \
    --encoding utf-8

if ! jq -e '.token.refresh_token' "$TEMP_TOKEN" > /dev/null 2>&1; then
    echo "ERROR: Token is missing .token.refresh_token field (may need re-login with new token format)"
    rm -f "$TEMP_TOKEN"
    exit 1
fi
echo "Token structure valid (has .token.refresh_token)"

# Step 5: Copy to expected location.
cp "$TEMP_TOKEN" "$TOKEN_PATH"
rm -f "$TEMP_TOKEN"
echo "Token installed: $TOKEN_PATH"

# Step 6: Test whoami (same as CI integration test step).
echo ""
echo "--- Testing whoami ---"
DRIVE_ID=$(go run . whoami --json --drive "$DRIVE" | jq -r '.drives[0].id')
if [ -z "$DRIVE_ID" ] || [ "$DRIVE_ID" = "null" ]; then
    echo "ERROR: whoami failed to return a drive ID"
    exit 1
fi
echo "Drive ID: $DRIVE_ID"

# Step 7: Run integration tests.
echo ""
echo "--- Running integration tests ---"
ONEDRIVE_TEST_DRIVE="$DRIVE" \
    ONEDRIVE_TEST_DRIVE_ID="$DRIVE_ID" \
    ONEDRIVE_ALLOWED_TEST_ACCOUNTS="$DRIVE" \
    go test -tags=integration -race -v -timeout=5m ./internal/graph/...

# Step 8: Run E2E tests.
echo ""
echo "--- Running E2E tests ---"
ONEDRIVE_TEST_DRIVE="$DRIVE" \
    ONEDRIVE_ALLOWED_TEST_ACCOUNTS="$DRIVE" \
    go test -tags=e2e -race -v -timeout=5m ./e2e/...

# Step 9: Save rotated token back (same as CI post-test step).
echo ""
echo "--- Saving rotated token back to Key Vault ---"
if jq -e '.token.refresh_token' "$TOKEN_PATH" > /dev/null 2>&1; then
    az keyvault secret set \
        --vault-name "$VAULT_NAME" \
        --name "$SECRET_NAME" \
        --file "$TOKEN_PATH" \
        --content-type "application/json" \
        --output none
    echo "Rotated token saved"
else
    echo "WARNING: Token file missing or invalid — skipping save"
fi

echo ""
echo "=== ALL LOCAL CI CHECKS PASSED ==="
