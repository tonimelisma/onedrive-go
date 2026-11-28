#!/usr/bin/env bash
# ci-keyvault.sh reads and writes the CI Key Vault from a GitHub Actions job,
# keylessly: the job's OIDC token is exchanged for a Key Vault access token
# with curl, as the shared fetch step does, with no Azure CLI and no login
# action.
#
#   ci-keyvault.sh get <secret>            print the secret's value
#   ci-keyvault.sh download <secret> <file> write the value to <file>
#   ci-keyvault.sh upload <secret> <file>   set the secret to <file>'s content
#                                           (a rotated token write-back)
#
# The job needs `permissions: id-token: write` and the `live` environment,
# whose variables AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_KEY_VAULT the
# calling step passes in its env. Every call exchanges a fresh token, so a
# write-back at the end of a three-hour job never meets an expired one. The
# tokens stay in this process: nothing prints or exports them, and values go
# only to stdout (get) or the named file. A refusal names the secret and Key
# Vault's error code, never a value.
#
# It keeps to the bash 3.2 that macOS ships, because the devtool tests run the
# masking script, which calls it, on both CI platforms.
set -euo pipefail

: "${AZURE_CLIENT_ID:?AZURE_CLIENT_ID must name the Entra app}"
: "${AZURE_TENANT_ID:?AZURE_TENANT_ID must name the tenant}"
: "${AZURE_KEY_VAULT:?AZURE_KEY_VAULT must name the Key Vault}"
: "${ACTIONS_ID_TOKEN_REQUEST_URL:?the job needs permissions: id-token: write}"
: "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:?the job needs permissions: id-token: write}"

api_version=7.4
body=$(mktemp)
trap 'rm -f "${body}"' EXIT

# field prints one top-level field of the JSON document on stdin.
field() {
    python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}

# request runs curl with the given arguments, its response body in ${body},
# and fails naming what for and the error code of a non-2xx answer.
request() {
    local what=$1 status
    shift
    status=$(curl -sS --retry 3 --retry-delay 2 -o "${body}" -w '%{http_code}' "$@") || {
        echo "ci-keyvault: ${what}: the request did not complete" >&2
        return 1
    }
    case "${status}" in
        2??) ;;
        *)
            code=$(python3 -c 'import json,sys
try:
    e = json.load(open(sys.argv[1])).get("error", "")
    print(e.get("code", "") if isinstance(e, dict) else e)
except Exception:
    print("")' "${body}")
            echo "ci-keyvault: ${what}: HTTP ${status} ${code}" >&2
            return 1
            ;;
    esac
}

# vault_token prints a Key Vault access token for this job.
vault_token() {
    local oidc
    request "GitHub OIDC token" -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
        "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=api://AzureADTokenExchange"
    oidc=$(field value < "${body}")
    request "Entra token exchange" "https://login.microsoftonline.com/${AZURE_TENANT_ID}/oauth2/v2.0/token" \
        -d "client_id=${AZURE_CLIENT_ID}" -d grant_type=client_credentials \
        --data-urlencode scope=https://vault.azure.net/.default \
        --data-urlencode client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer \
        --data-urlencode "client_assertion=${oidc}"
    field access_token < "${body}"
}

secret_url() {
    echo "https://${AZURE_KEY_VAULT}.vault.azure.net/secrets/$1?api-version=${api_version}"
}

# read_secret leaves the secret's JSON document in ${body}.
read_secret() {
    local token
    token=$(vault_token)
    request "read secret $1" -H "Authorization: Bearer ${token}" "$(secret_url "$1")"
}

case "${1:-} $#" in
    "get 2")
        read_secret "$2"
        field value < "${body}"
        ;;
    "download 3")
        read_secret "$2"
        python3 -c 'import json,sys
with open(sys.argv[2], "w", encoding="utf-8") as out:
    out.write(json.load(open(sys.argv[1]))["value"])' "${body}" "$3"
        ;;
    "upload 3")
        token=$(vault_token)
        payload=$(mktemp)
        trap 'rm -f "${body}" "${payload}"' EXIT
        python3 -c 'import json,sys
value = open(sys.argv[1], encoding="utf-8").read()
json.dump({"value": value, "contentType": "application/json"}, open(sys.argv[2], "w"))' "$3" "${payload}"
        request "write secret $2" -X PUT -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/json" --data-binary "@${payload}" "$(secret_url "$2")"
        ;;
    *)
        echo "usage: ci-keyvault.sh get <secret> | download <secret> <file> | upload <secret> <file>" >&2
        exit 2
        ;;
esac
