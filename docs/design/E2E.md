# E2E Test Isolation Design

## Problem

E2E tests on a dev machine share the **same token file, config, and state DB** as production:

```
~/.config/onedrive-go/config.toml
~/.local/share/onedrive-go/token_personal_testitesti18@outlook.com.json
~/.local/share/onedrive-go/state_personal_testitesti18@outlook.com.db
```

Risks:
- **Token refresh race**: daemon and test both refresh the OAuth token concurrently
- **SQLite sole-writer violation**: daemon holds the state DB open while tests try to use it
- **Conflicting mutations**: both daemon and tests mutate the same OneDrive folder
- **Corruption propagation**: a test bug that corrupts the token or state DB breaks production sync

## Target State

Each context runs in full isolation:

| Context | Config | Token | State DB | OneDrive folder | Graph API |
|---------|--------|-------|----------|-----------------|-----------|
| **Production** | Real XDG config | Real XDG token | Real XDG state DB | User's real files | Real |
| **Unit tests** | Temp dir or none | None (DriveID hardcoded) | Temp dir SQLite | None (mocked) | Mocked |
| **E2E local** | Temp dir (generated) | **Copy** in temp dir | Temp dir SQLite | Dedicated test folder | Real |
| **E2E CI** | Temp dir (generated) | Downloaded from Key Vault to temp dir | Temp dir SQLite | Dedicated test folder | Real |

## Changes Required

### 1. E2E test harness: isolated XDG override

Set `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, and `XDG_CACHE_HOME` to temp directories on subprocess `exec.Cmd.Env`. Each test run gets its own config, token, and state DB — completely isolated from production.

On macOS, `DefaultDataDir()` and `DefaultConfigDir()` both resolve to `~/Library/Application Support/onedrive-go`. The XDG env vars override this behavior, so setting them works cross-platform.

### 2. E2E token bootstrap: copy, don't share

**Local dev**: The test harness copies the token file from the real XDG data dir into the test's temp data dir. The copy is what gets used and potentially refreshed. The production token continues working because Azure AD refresh tokens for consumer Microsoft accounts are multi-use and long-lived.

**CI**: Already downloads from Azure Key Vault. Target the temp dir instead of a hardcoded path.

**Guideline**: Stop the daemon before running E2E tests on a dev machine to avoid any token refresh races.

### 3. E2E config generation

The test harness generates a minimal `config.toml` in the temp config dir with the test drive section. Already partially implemented — just needs to consistently target the temp dir.

### 4. E2E token metadata bootstrap

After copying the token file, metadata (drive_id, user_id, display_name, org_name) must exist:

- **Local dev**: The source token file already has metadata from the original login. The copy carries it.
- **CI**: Already calls `whoami --json` and writes metadata via `jq`. No change needed, just target the temp dir.

### 5. State DB

Each test run starts with a fresh state DB in the temp dir. Already correct — no change needed.

### 6. CI workflow

Point downloads and bootstrap steps to temp dirs. Minor path changes in `.github/workflows/integration.yml`.

## Unit Test Implications

Unit tests are unaffected — they already use `t.TempDir()` for everything and mock the Graph API. The `testResolvedDrive()` helper hardcodes `DriveID: driveid.New("test-drive-id")`, bypassing token resolution entirely.

For the Orchestrator `reload()` path specifically: `config.ResolveDrives()` creates fresh `ResolvedDrive` structs via `buildResolvedDrive()` → `ReadTokenMeta()` → `DriveTokenPath()` → `DefaultDataDir()`. In unit tests, there's no token file at the XDG path, so DriveID comes back zero.

The fix for unit tests: `reload()` carries over DriveIDs from pre-reload drives by matching on CanonicalID. DriveIDs don't change between config reloads — they're immutable identifiers from the Graph API cached in token metadata at login time.
