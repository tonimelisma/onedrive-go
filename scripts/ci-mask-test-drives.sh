#!/usr/bin/env bash
# ci-mask-test-drives.sh reads a live CI job's test drives and account
# allowlist from Azure Key Vault, masks them, then hands them to the job's
# later steps.
#
# This repository's CI logs are public, and GitHub prints a step's env values
# in its header, so the test drives and the allowlist are never repository
# variables and never in any step's env. The step that runs this script, which
# only the checkout and the job's Azure login precede, names them by the Key
# Vault secret that holds each: <NAME>_FROM_VAULT for ONEDRIVE_TEST_DRIVE,
# ONEDRIVE_TEST_DRIVE_2 (optional) and ONEDRIVE_ALLOWED_TEST_ACCOUNTS, read
# from the vault AZURE_KEY_VAULT_NAME names through the az session the login
# left. The script
#   - masks each value the moment it is read, before anything else runs;
#   - masks every spelling of them the job or its tests can print: each drive
#     identifier and allowlist entry, the account address inside it, the Key
#     Vault secret and token-file spellings derived from it, and the
#     lower-cased and percent-encoded (@ as %40, : as %3A, as Graph URLs in
#     debug output spell them) form of each;
#   - lists those spellings in $RUNNER_TEMP/onedrive-test-masks, which the
#     artifact redaction step reads;
#   - exports the three values, and for each drive its Key Vault secret name
#     (<NAME>_VAULT_SECRET) and token file name (<NAME>_TOKEN_FILE), through
#     $GITHUB_ENV, so no later step derives a spelling itself;
#   - exports ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE and
#     ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE from the drive that
#     <FIXTURE>_FROM names, ONEDRIVE_TEST_DRIVE or ONEDRIVE_TEST_DRIVE_2: the
#     job states which drive holds each shortcut fixture by variable name,
#     never by value;
#   - prints nothing but the mask commands, which the runner consumes.
#
# It keeps to the bash 3.2 that macOS ships, because the devtool tests run it
# on both CI platforms.
set -euo pipefail

masks="${RUNNER_TEMP:?}/onedrive-test-masks"
: > "${masks}"

# value prints the variable a name names, or nothing when it is unset.
value() {
  printenv "$1" || true
}

trim() {
  sed 's/^[[:space:]]*//; s/[[:space:]]*$//' <<< "$1"
}

# The values come from the vault alone: a value already in the environment is
# dropped, so a step that sets one cannot slip it past the vault.
vault="${AZURE_KEY_VAULT_NAME:?AZURE_KEY_VAULT_NAME must name the Key Vault}"
for name in ONEDRIVE_TEST_DRIVE ONEDRIVE_TEST_DRIVE_2 ONEDRIVE_ALLOWED_TEST_ACCOUNTS; do
  unset "${name}"
  secret="$(value "${name}_FROM_VAULT")"
  if [ -z "${secret}" ]; then
    case "${name}" in
      ONEDRIVE_TEST_DRIVE_2) continue ;;
      *)
        echo "${name}_FROM_VAULT must name the Key Vault secret that holds ${name}" >&2
        exit 1
        ;;
    esac
  fi
  fetched="$(az keyvault secret show --vault-name "${vault}" --name "${secret}" --query value --output tsv)"
  if [ -z "$(trim "${fetched}")" ]; then
    echo "the Key Vault secret ${name}_FROM_VAULT names holds nothing" >&2
    exit 1
  fi
  echo "::add-mask::${fetched}"
  export "${name}=${fetched}"
done

# vault_spelling and token_spelling are how a drive's Key Vault secret and
# token file spell it.
vault_spelling() {
  sed 's/[:@._]/-/g' <<< "$1"
}

token_spelling() {
  sed 's/:/_/' <<< "$1"
}

# list_spellings lists every spelling of one drive identifier or allowlist
# entry in the masks file.
list_spellings() {
  local derived cased
  for derived in "$1" "$(cut -d: -f2 <<< "$1")" "$(vault_spelling "$1")" "$(token_spelling "$1")"; do
    for cased in "${derived}" "$(tr '[:upper:]' '[:lower:]' <<< "${derived}")"; do
      printf '%s\n' "${cased}" "$(sed 's/@/%40/g; s/:/%3A/g' <<< "${cased}")" >> "${masks}"
    done
  done
}

for name in ONEDRIVE_TEST_DRIVE ONEDRIVE_TEST_DRIVE_2; do
  drive="$(trim "$(value "${name}")")"
  if [ -n "${drive}" ]; then
    list_spellings "${drive}"
  fi
done
while IFS= read -r entry; do
  entry="$(trim "${entry}")"
  if [ -n "${entry}" ]; then
    list_spellings "${entry}"
  fi
done <<< "$(tr ',' '\n' <<< "$(value ONEDRIVE_ALLOWED_TEST_ACCOUNTS)")"
printf '%s\n' "$(value ONEDRIVE_ALLOWED_TEST_ACCOUNTS)" >> "${masks}"

sort -u -o "${masks}" "${masks}"
sed -i.blank '/^$/d' "${masks}"
rm -f "${masks}.blank"
while IFS= read -r spelling; do
  echo "::add-mask::${spelling}"
done < "${masks}"

for name in ONEDRIVE_TEST_DRIVE ONEDRIVE_TEST_DRIVE_2 ONEDRIVE_ALLOWED_TEST_ACCOUNTS; do
  if [ -n "$(value "${name}")" ]; then
    echo "${name}=$(value "${name}")" >> "${GITHUB_ENV:?}"
  fi
done
for name in ONEDRIVE_TEST_DRIVE ONEDRIVE_TEST_DRIVE_2; do
  drive="$(trim "$(value "${name}")")"
  if [ -n "${drive}" ]; then
    echo "${name}_VAULT_SECRET=onedrive-cache-$(vault_spelling "${drive}")" >> "${GITHUB_ENV}"
    echo "${name}_TOKEN_FILE=token_$(token_spelling "${drive}").json" >> "${GITHUB_ENV}"
  fi
done
for fixture in ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE; do
  from="$(value "${fixture}_FROM")"
  case "${from}" in
    "")
      continue
      ;;
    ONEDRIVE_TEST_DRIVE | ONEDRIVE_TEST_DRIVE_2) ;;
    *)
      echo "${fixture}_FROM must name ONEDRIVE_TEST_DRIVE or ONEDRIVE_TEST_DRIVE_2" >&2
      exit 1
      ;;
  esac
  if [ -n "$(value "${from}")" ]; then
    echo "${fixture}=$(value "${from}")" >> "${GITHUB_ENV}"
  fi
done
