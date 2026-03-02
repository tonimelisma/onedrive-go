# E2E Test Isolation Design

## Status: IMPLEMENTED

Isolation is fully implemented. `TestMain` in both `e2e/` and `internal/graph/` sets up XDG + HOME overrides, copies the token to a temp dir, and validates the account allowlist.

## Problem

E2E tests on a dev machine must not share the **same token file, config, and state DB** as production:

```
~/.config/onedrive-go/config.toml
~/.local/share/onedrive-go/token_personal_user@example.com.json
~/.local/share/onedrive-go/state_personal_user@example.com.db
```

Risks:
- **Token refresh race**: daemon and test both refresh the OAuth token concurrently
- **SQLite sole-writer violation**: daemon holds the state DB open while tests try to use it
- **Conflicting mutations**: both daemon and tests mutate the same OneDrive folder
- **Corruption propagation**: a test bug that corrupts the token or state DB breaks production sync

## Implementation

### Safety guards (TestMain)

1. **`.env` loading**: `loadDotEnv()` reads `KEY=VALUE` from `.env` at module root (gitignored). CI sets env vars directly.
2. **Account allowlist**: `validateAllowlist()` crashes if `ONEDRIVE_ALLOWED_TEST_ACCOUNTS` is unset or if `ONEDRIVE_TEST_DRIVE` is not in the list.
3. **Directory isolation**: `setupIsolation()` overrides `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` to temp directories and copies the token.

### XDG override on all platforms

`internal/config/paths.go` checks XDG env vars first on ALL platforms (not just Linux). This enables test isolation on macOS without platform-specific workarounds.

### Per-test state isolation

Sync tests that need isolated state DBs override `XDG_DATA_HOME` per-test via `t.Setenv()` and copy the token from `testDataDir` (set by TestMain).

### Isolation matrix

| Context | Config | Token | State DB | OneDrive folder | Graph API |
|---------|--------|-------|----------|-----------------|-----------|
| **Production** | Real XDG config | Real XDG token | Real XDG state DB | User's real files | Real |
| **Unit tests** | Temp dir or none | None (DriveID hardcoded) | Temp dir SQLite | None (mocked) | Mocked |
| **E2E local** | Temp dir (generated) | **Copy** in temp dir | Temp dir SQLite | Dedicated test folder | Real |
| **E2E CI** | Temp dir (generated) | Downloaded from Key Vault to temp dir | Temp dir SQLite | Dedicated test folder | Real |

### Verification tests

`TestIsolation_*` tests verify:
- `HOME` env var points to temp dir
- `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME` point to temp dirs
- Token file exists in temp data dir
- Integration tests: `config.DefaultDataDir()` resolves under temp

## Unit Test Implications

Unit tests are unaffected — they already use `t.TempDir()` for everything and mock the Graph API. The `testResolvedDrive()` helper hardcodes `DriveID: driveid.New("test-drive-id")`, bypassing token resolution entirely.

For the Orchestrator `reload()` path specifically: `config.ResolveDrives()` creates fresh `ResolvedDrive` structs via `buildResolvedDrive()` → `ReadTokenMeta()` → `DriveTokenPath()` → `DefaultDataDir()`. In unit tests, there's no token file at the XDG path, so DriveID comes back zero.

The fix for unit tests: `reload()` carries over DriveIDs from pre-reload drives by matching on CanonicalID. DriveIDs don't change between config reloads — they're immutable identifiers from the Graph API cached in token metadata at login time.
