#!/usr/bin/env bash
# Upload test data to Azure Key Vault for CI.
# Uploads: cache files (token + metadata) + config.toml
#
# Usage: ./scripts/migrate-test-data-to-ci.sh
#
# Prerequisites:
#   - az CLI logged in (az login)
#   - .testdata/ populated (run bootstrap-test-credentials.sh first)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TESTDATA="$REPO_ROOT/.testdata"
VAULT_NAME="kv-onedrivego-ci"

if [ ! -d "$TESTDATA" ]; then
    echo "ERROR: .testdata/ not found. Run scripts/bootstrap-test-credentials.sh first."
    exit 1
fi

echo "=== Migrating test data to Key Vault ==="
echo "Vault: $VAULT_NAME"
echo ""

# Upload each cache file as a separate secret.
for token_file in "$TESTDATA"/token_*.json; do
    [ -f "$token_file" ] || continue

    filename=$(basename "$token_file")
    # Derive secret name: token_personal_user@outlook.com.json
    #                   → onedrive-cache-personal-user-outlook-com
    # The character class includes _ so that the underscore between type and
    # email (from the filename) becomes a hyphen, matching the CI derivation
    # which starts from the canonical drive ID (personal:user@outlook.com).
    sanitized=$(echo "$filename" | sed 's/^token_//; s/\.json$//; s/[:@._]/-/g')
    secret_name="onedrive-cache-${sanitized}"

    echo "Uploading: $filename → $secret_name"
    az keyvault secret set \
        --vault-name "$VAULT_NAME" \
        --name "$secret_name" \
        --file "$token_file" \
        --content-type "application/json" \
        --output none
done

# Upload config as a separate secret.
if [ -f "$TESTDATA/config.toml" ]; then
    echo "Uploading: config.toml → onedrive-test-config"
    az keyvault secret set \
        --vault-name "$VAULT_NAME" \
        --name "onedrive-test-config" \
        --file "$TESTDATA/config.toml" \
        --content-type "text/plain" \
        --output none
else
    echo "WARNING: config.toml not found in .testdata/"
fi

echo ""
echo "=== Migration complete ==="
az keyvault secret list --vault-name "$VAULT_NAME" --query "[].name" -o tsv
