package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

func TestApplyDotEnv(t *testing.T) {
	t.Setenv("KEEP_EXISTING", "keep")

	err := applyDotEnv(strings.NewReader(`
# comment
FIRST=value
KEEP_EXISTING=replace
QUOTED="quoted value"
SINGLE='single quoted'
INVALID
`))
	require.NoError(t, err)

	assert.Equal(t, "value", os.Getenv("FIRST"))
	assert.Equal(t, "keep", os.Getenv("KEEP_EXISTING"))
	assert.Equal(t, "quoted value", os.Getenv("QUOTED"))
	assert.Equal(t, "single quoted", os.Getenv("SINGLE"))
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	require.NoError(t, os.WriteFile(envPath, []byte("FROM_FILE=loaded\n"), 0o600))

	LoadDotEnv(envPath)

	assert.Equal(t, "loaded", os.Getenv("FROM_FILE"))
}

func TestLoadDotEnv_MissingFile(t *testing.T) {
	LoadDotEnv(filepath.Join(t.TempDir(), ".missing"))
}

func TestLoadTestEnv(t *testing.T) {
	moduleRoot := t.TempDir()
	fixturesDir := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.MkdirAll(fixturesDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte("FROM_ENV=env\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixturesDir, "fixtures.env"), []byte("FROM_FIXTURE=fixture\n"), 0o600))

	LoadTestEnv(moduleRoot)

	assert.Equal(t, "env", os.Getenv("FROM_ENV"))
	assert.Equal(t, "fixture", os.Getenv("FROM_FIXTURE"))
}

func TestLoadTestEnv_Precedence(t *testing.T) {
	moduleRoot := t.TempDir()
	fixturesDir := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.MkdirAll(fixturesDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte("PRIORITY=env\nENV_ONLY=env-only\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixturesDir, "fixtures.env"), []byte("PRIORITY=fixture\nFIXTURE_ONLY=fixture-only\n"), 0o600))
	t.Setenv("PRIORITY", "exported")
	t.Setenv("EXPORTED_ONLY", "exported-only")

	LoadTestEnv(moduleRoot)

	assert.Equal(t, "exported", os.Getenv("PRIORITY"))
	assert.Equal(t, "env-only", os.Getenv("ENV_ONLY"))
	assert.Equal(t, "fixture-only", os.Getenv("FIXTURE_ONLY"))
	assert.Equal(t, "exported-only", os.Getenv("EXPORTED_ONLY"))
}

func TestLoadTestEnv_MissingFixturesFile(t *testing.T) {
	moduleRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte("FROM_ENV=env\n"), 0o600))

	LoadTestEnv(moduleRoot)

	assert.Equal(t, "env", os.Getenv("FROM_ENV"))
}

func TestLoadLiveTestConfig(t *testing.T) {
	for _, key := range []string{
		"ONEDRIVE_TEST_DRIVE",
		"ONEDRIVE_TEST_DRIVE_2",
		"ONEDRIVE_TEST_SHARED_LINK",
		"ONEDRIVE_TEST_SHARED_FOLDER_LINK",
		"ONEDRIVE_TEST_WRITABLE_SHARED_FOLDER",
		"ONEDRIVE_TEST_READONLY_SHARED_FOLDER",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_NAME",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH",
		"ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME",
	} {
		t.Setenv(key, "")
	}

	moduleRoot := t.TempDir()
	fixturesDir := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.MkdirAll(fixturesDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte(
		"ONEDRIVE_TEST_DRIVE=personal:primary@example.com\n"+
			"ONEDRIVE_TEST_DRIVE_2=personal:secondary@example.com\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixturesDir, "fixtures.env"), []byte(strings.Join([]string{
		"ONEDRIVE_TEST_SHARED_LINK=https://1drv.ms/example",
		"ONEDRIVE_TEST_SHARED_FOLDER_LINK=https://1drv.ms/shared-folder",
		"ONEDRIVE_TEST_WRITABLE_SHARED_FOLDER=shared:secondary@example.com:drive123:item123",
		"ONEDRIVE_TEST_READONLY_SHARED_FOLDER=shared:primary@example.com:drive456:item456",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE=personal:secondary@example.com",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME=Owner Shortcut",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SHARER_EMAIL=primary@example.com",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH=sentinel.txt",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE=personal:primary@example.com",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_NAME=Read-only Shortcut",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SHARER_EMAIL=secondary@example.com",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH=readonly.txt",
		"ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME=Standalone Shared Folder",
	}, "\n")+"\n"), 0o600))

	cfg, err := LoadLiveTestConfig(moduleRoot)
	require.NoError(t, err)

	assert.Equal(t, "personal:primary@example.com", cfg.PrimaryDrive)
	assert.Equal(t, "personal:secondary@example.com", cfg.SecondaryDrive)
	assert.Equal(t, "https://1drv.ms/example", cfg.Fixtures.SharedFileLink)
	assert.Equal(t, "https://1drv.ms/shared-folder", cfg.Fixtures.SharedFolderLink)
	assert.Equal(t, "shared:secondary@example.com:drive123:item123", cfg.Fixtures.WritableSharedFolderSelector)
	assert.Equal(t, "shared:primary@example.com:drive456:item456", cfg.Fixtures.ReadOnlySharedFolderSelector)
	assert.Equal(t, "personal:secondary@example.com", cfg.Fixtures.WritableShortcutParentDrive)
	assert.Equal(t, "Owner Shortcut", cfg.Fixtures.WritableShortcutName)
	assert.Equal(t, "primary@example.com", cfg.Fixtures.WritableShortcutSharerEmail)
	assert.Equal(t, "sentinel.txt", cfg.Fixtures.WritableShortcutSentinelPath)
	assert.Equal(t, "personal:primary@example.com", cfg.Fixtures.ReadOnlyShortcutParentDrive)
	assert.Equal(t, "Read-only Shortcut", cfg.Fixtures.ReadOnlyShortcutName)
	assert.Equal(t, "secondary@example.com", cfg.Fixtures.ReadOnlyShortcutSharerEmail)
	assert.Equal(t, "readonly.txt", cfg.Fixtures.ReadOnlyShortcutSentinelPath)
	assert.Equal(t, "Standalone Shared Folder", cfg.Fixtures.StandaloneSharedFolderName)
}

func TestLoadLiveTestConfig_Precedence(t *testing.T) {
	for _, key := range []string{
		"ONEDRIVE_TEST_DRIVE",
		"ONEDRIVE_TEST_DRIVE_2",
		"ONEDRIVE_TEST_SHARED_LINK",
		"ONEDRIVE_TEST_SHARED_FOLDER_LINK",
		"ONEDRIVE_TEST_WRITABLE_SHARED_FOLDER",
		"ONEDRIVE_TEST_READONLY_SHARED_FOLDER",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_NAME",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH",
		"ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME",
	} {
		t.Setenv(key, "")
	}

	moduleRoot := t.TempDir()
	fixturesDir := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.MkdirAll(fixturesDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte(
		"ONEDRIVE_TEST_DRIVE=personal:env@example.com\n"+
			"ONEDRIVE_TEST_SHARED_LINK=https://1drv.ms/env\n"+
			"ONEDRIVE_TEST_SHARED_FOLDER_LINK=https://1drv.ms/f/env\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixturesDir, "fixtures.env"), []byte(
		"ONEDRIVE_TEST_DRIVE=personal:fixture@example.com\n"+
			"ONEDRIVE_TEST_SHARED_LINK=https://1drv.ms/fixture\n"+
			"ONEDRIVE_TEST_SHARED_FOLDER_LINK=https://1drv.ms/f/fixture\n",
	), 0o600))
	t.Setenv("ONEDRIVE_TEST_DRIVE", "personal:exported@example.com")

	cfg, err := LoadLiveTestConfig(moduleRoot)
	require.NoError(t, err)

	assert.Equal(t, "personal:exported@example.com", cfg.PrimaryDrive)
	assert.Equal(t, "https://1drv.ms/env", cfg.Fixtures.SharedFileLink)
	assert.Equal(t, "https://1drv.ms/f/env", cfg.Fixtures.SharedFolderLink)
}

// Validates: R-6.10.27
//
// The shortcut fixtures' folder names have defaults; their parent drives do
// not. Nothing guesses which configured drive holds a shortcut: the
// repository names no test account, so the parent drives come only from
// their variables, and each sharer is the other fixture's parent account.
func TestLoadLiveTestConfig_DefaultShortcutFixtures(t *testing.T) {
	for _, key := range []string{
		"ONEDRIVE_TEST_DRIVE",
		"ONEDRIVE_TEST_DRIVE_2",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_NAME",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH",
		"ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME",
	} {
		t.Setenv(key, "")
	}

	moduleRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte(
		"ONEDRIVE_TEST_DRIVE=personal:first@example.com\n"+
			"ONEDRIVE_TEST_DRIVE_2=personal:second@example.com\n",
	), 0o600))

	cfg, err := LoadLiveTestConfig(moduleRoot)
	require.NoError(t, err)

	assert.Empty(t, cfg.Fixtures.WritableShortcutParentDrive, "no configured drive is guessed as a shortcut's parent")
	assert.Empty(t, cfg.Fixtures.ReadOnlyShortcutParentDrive)
	assert.Empty(t, cfg.Fixtures.WritableShortcutSharerEmail)
	assert.Empty(t, cfg.Fixtures.ReadOnlyShortcutSharerEmail)
	assert.Equal(t, "Kikkeli Shared Test Folder", cfg.Fixtures.WritableShortcutName)
	assert.Equal(t, "shortcut-sentinel.txt", cfg.Fixtures.WritableShortcutSentinelPath)
	assert.Equal(t, "Read-only Shared Folder", cfg.Fixtures.ReadOnlyShortcutName)
	assert.Equal(t, "shortcut-sentinel.txt", cfg.Fixtures.ReadOnlyShortcutSentinelPath)
	assert.Equal(t, "Testi2 Shared Folder", cfg.Fixtures.StandaloneSharedFolderName)

	t.Setenv(ReadOnlyShortcutParentDriveEnvVar, "personal:first@example.com")
	t.Setenv(WritableShortcutParentDriveEnvVar, "personal:second@example.com")
	cfg, err = LoadLiveTestConfig(moduleRoot)
	require.NoError(t, err)

	assert.Equal(t, "personal:second@example.com", cfg.Fixtures.WritableShortcutParentDrive)
	assert.Equal(t, "first@example.com", cfg.Fixtures.WritableShortcutSharerEmail)
	assert.Equal(t, "personal:first@example.com", cfg.Fixtures.ReadOnlyShortcutParentDrive)
	assert.Equal(t, "second@example.com", cfg.Fixtures.ReadOnlyShortcutSharerEmail)
}

// Validates: R-6.10.27
//
// A .env file that cannot be read is an error the loader returns; a missing
// one is not. Only the harness wrappers exit.
func TestLoadDotEnvFile_ReturnsErrors(t *testing.T) {
	require.NoError(t, LoadDotEnvFile(filepath.Join(t.TempDir(), ".missing")))

	unreadable := filepath.Join(t.TempDir(), "dir-not-file")
	require.NoError(t, os.MkdirAll(unreadable, 0o700))
	err := LoadDotEnvFile(unreadable)
	require.ErrorContains(t, err, unreadable)

	t.Setenv("ONEDRIVE_TEST_ENV_FILE_KEY", "")
	envPath := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(envPath, []byte("=no key\nONEDRIVE_TEST_ENV_FILE_KEY='set'\n"), 0o600))
	require.NoError(t, LoadDotEnvFile(envPath))
	assert.Equal(t, "set", os.Getenv("ONEDRIVE_TEST_ENV_FILE_KEY"))
}

func TestLoadLiveTestConfig_InvalidSharedSelector(t *testing.T) {
	for _, key := range []string{
		"ONEDRIVE_TEST_DRIVE",
		"ONEDRIVE_TEST_DRIVE_2",
		"ONEDRIVE_TEST_SHARED_LINK",
		"ONEDRIVE_TEST_WRITABLE_SHARED_FOLDER",
		"ONEDRIVE_TEST_READONLY_SHARED_FOLDER",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_NAME",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SHARER_EMAIL",
		"ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH",
		"ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME",
	} {
		t.Setenv(key, "")
	}

	moduleRoot := t.TempDir()
	fixturesDir := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.MkdirAll(fixturesDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, ".env"), []byte(
		"ONEDRIVE_TEST_DRIVE=personal:primary@example.com\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fixturesDir, "fixtures.env"), []byte(
		"ONEDRIVE_TEST_READONLY_SHARED_FOLDER=not-a-selector\n",
	), 0o600))

	_, err := LoadLiveTestConfig(moduleRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ONEDRIVE_TEST_READONLY_SHARED_FOLDER")
}

func TestLiveTestConfig_DriveIDForEmail(t *testing.T) {
	cfg := LiveTestConfig{
		PrimaryDrive:   "personal:primary@example.com",
		SecondaryDrive: "personal:secondary@example.com",
	}

	assert.Equal(t, []string{
		"personal:primary@example.com",
		"personal:secondary@example.com",
	}, cfg.CandidateDriveIDs())

	driveID, ok := cfg.DriveIDForEmail("secondary@example.com")
	require.True(t, ok)
	assert.Equal(t, "personal:secondary@example.com", driveID)

	_, ok = cfg.DriveIDForEmail("missing@example.com")
	assert.False(t, ok)
}

func TestFindModuleRoot(t *testing.T) {
	moduleRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(moduleRoot, "go.mod"), []byte("module example.com/test\n"), 0o600))

	nested := filepath.Join(moduleRoot, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o700))

	oldWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(nested))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(oldWD))
	})

	expected, err := filepath.EvalSymlinks(moduleRoot)
	require.NoError(t, err)

	actual, err := filepath.EvalSymlinks(FindModuleRoot("fallback"))
	require.NoError(t, err)

	assert.Equal(t, expected, actual)
}

func TestFindModuleRoot_Fallback(t *testing.T) {
	dir := t.TempDir()

	oldWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(oldWD))
	})

	assert.Equal(t, "fallback", FindModuleRoot("fallback"))
}

func TestFindTestCredentialDir(t *testing.T) {
	moduleRoot := t.TempDir()
	credentialDir := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.MkdirAll(credentialDir, 0o700))

	assert.Equal(t, credentialDir, FindTestCredentialDir(moduleRoot))
}

func TestFindTestCredentialDir_Symlink(t *testing.T) {
	moduleRoot := t.TempDir()
	realCredentialDir := t.TempDir()
	credentialLink := filepath.Join(moduleRoot, ".testdata")
	require.NoError(t, os.Symlink(realCredentialDir, credentialLink))

	assert.Equal(t, credentialLink, FindTestCredentialDir(moduleRoot))
}

func TestValidateAllowlist(t *testing.T) {
	t.Setenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS", "personal:user@example.com,business:user@contoso.com")
	t.Setenv("ONEDRIVE_TEST_DRIVE", "business:user@contoso.com")

	ValidateAllowlist("ONEDRIVE_TEST_DRIVE")
}

// Validates: R-6.10.27
//
// Entries are comma-separated and trimmed, and a drive must match one
// exactly.
func TestAllowlistIncludes(t *testing.T) {
	t.Parallel()

	const allowlist = " personal:user@example.com ,business:user@contoso.com"
	assert.True(t, AllowlistIncludes(allowlist, "personal:user@example.com"))
	assert.True(t, AllowlistIncludes(allowlist, "business:user@contoso.com"))
	assert.False(t, AllowlistIncludes(allowlist, "personal:user@contoso.com"))
	assert.False(t, AllowlistIncludes(allowlist, "user@example.com"), "an entry is a whole drive, not a part of one")
	assert.False(t, AllowlistIncludes("", "personal:user@example.com"))
	assert.False(t, AllowlistIncludes(allowlist, ""))
}

// Validates: R-6.10.27
//
// The live suites run in public CI logs. A refused drive names the variables
// to check and never their values.
func TestCheckDriveAllowed_NamesTheVariablesAndNoValue(t *testing.T) {
	const (
		drive     = "personal:someone@example.test"
		allowlist = "business:other@example.test"
	)

	t.Setenv(AllowedTestAccountsEnvVar, allowlist)
	err := CheckDriveAllowed("ONEDRIVE_TEST_DRIVE_2", drive)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ONEDRIVE_TEST_DRIVE_2")
	assert.Contains(t, err.Error(), AllowedTestAccountsEnvVar)
	for _, value := range []string{drive, allowlist, "someone", "other@example.test"} {
		assert.NotContains(t, err.Error(), value)
	}

	err = CheckDriveAllowed("ONEDRIVE_TEST_DRIVE", "")
	require.ErrorContains(t, err, "ONEDRIVE_TEST_DRIVE is not set")

	t.Setenv(AllowedTestAccountsEnvVar, "")
	err = CheckDriveAllowed("ONEDRIVE_TEST_DRIVE", drive)
	require.ErrorContains(t, err, AllowedTestAccountsEnvVar+" is not set")
	assert.NotContains(t, err.Error(), drive)

	t.Setenv(AllowedTestAccountsEnvVar, allowlist+", "+drive)
	require.NoError(t, CheckDriveAllowed("ONEDRIVE_TEST_DRIVE", drive))
}

// Validates: R-6.10.27
//
// What the live suites record ends up in public CI artifacts. Every spelling
// of a configured drive a recorded line can hold, the identifier, the account
// address, the file-name form and the lower-cased and percent-encoded forms,
// becomes the drive's role.
func TestDriveRoles_RecordedLinesHoldNoConfiguredDrive(t *testing.T) {
	t.Parallel()

	const (
		first  = "personal:Alice.A@example.test"
		second = "business:bob_b@example.test"
	)
	roles := NewDriveRoles(first, second)

	assert.Equal(t, "drive-1", roles.Role(first))
	assert.Equal(t, "drive-2", roles.Role(second))
	assert.Equal(t, "ls --drive drive-1 /", roles.Redact("ls --drive "+first+" /"))
	assert.Equal(t, "account drive-2-account signed in", roles.Redact("account bob_b@example.test signed in"))

	recorded := []string{
		"stat --drive " + first + " /e2e-file.txt",
		"TestE2E_AuthPreflight_Fast/personal_Alice.A@example.test",
		"GET /me/drive drive=" + second,
		`{"account":"alice.a@example.test","drive":"personal:alice.a@example.test"}`,
		"https://graph.microsoft.com/v1.0/users/Alice.A%40example.test/drive and personal%3AAlice.A%40example.test",
		"token_business_bob_b@example.test.json",
	}
	for _, line := range recorded {
		redacted := strings.ToLower(roles.Redact(line))
		for _, value := range []string{"alice.a@example.test", "alice.a%40example.test", "bob_b@example.test"} {
			assert.NotContains(t, redacted, value, "line %q", line)
		}
	}

	var unset DriveRoles
	assert.Equal(t, "ls --drive "+first, unset.Redact("ls --drive "+first), "with no drives configured, nothing changes")
	assert.Equal(t, "drive-1", NewDriveRoles(first, "").Role(first))
	assert.Empty(t, NewDriveRoles(first).Role(""))
}

func TestTokenFileName(t *testing.T) {
	assert.Equal(t, "token_personal_user@example.com.json", TokenFileName("personal:user@example.com"))
}

func TestCopyFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	src := filepath.Join(srcDir, "source.json")
	dst := filepath.Join(dstDir, "copied.json")
	require.NoError(t, os.WriteFile(src, []byte("payload"), 0o600))

	CopyFile(src, dst, 0o600)

	data, err := localpath.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(data))

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestCopyCatalogFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "catalog.json"), []byte("catalog"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "other.json"), []byte("ignore"), 0o600))

	CopyCatalogFile(srcDir, dstDir)

	catalogData, err := localpath.ReadFile(filepath.Join(dstDir, "catalog.json"))
	require.NoError(t, err)
	assert.Equal(t, "catalog", string(catalogData))

	_, err = os.Stat(filepath.Join(dstDir, "other.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestCopyCatalogFile_CopiesManagedCatalog(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	catalog := `{
  "accounts": {
    "personal:alice@example.com": {
      "canonical_id": "personal:alice@example.com",
      "display_name": "Alice Example",
      "primary_drive_id": "drive-alice"
    }
  },
  "drives": {
    "personal:alice@example.com": {
      "canonical_id": "personal:alice@example.com",
      "owner_account_canonical_id": "personal:alice@example.com",
      "remote_drive_id": "drive-alice"
    }
	}
}`
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "catalog.json"), []byte(catalog), 0o600))

	CopyCatalogFile(srcDir, dstDir)

	catalogData, err := localpath.ReadFile(filepath.Join(dstDir, "catalog.json"))
	require.NoError(t, err)
	assert.JSONEq(t, catalog, string(catalogData))
}

// Validates: R-6.10.27
//
// The repository is public, so no tracked file may spell a test account. A
// finding names the file, the line and the account's role, never the
// address, and matches any case and the percent-encoded form.
func TestFindTestAccounts_NamesFileLineAndRoleNeverTheAddress(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := map[string]string{
		"docs/a.md":   "intro\nclean line\nsee /users/Alice.A%40example.test/drive here\n",
		"code/b.go":   "const x = \"ALICE.A@EXAMPLE.TEST\"\n",
		"c.txt":       "bob_b@example.test and alice.a@example.test\n",
		"clean.txt":   "user@example.com\n",
		"binary.bin":  "alice.a@example.test\x00\x01",
		"deleted.txt": "",
	}
	for name, content := range files {
		if name == "deleted.txt" {
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}

	findings, err := FindTestAccounts(root,
		[]string{"docs/a.md", "code/b.go", "c.txt", "clean.txt", "binary.bin", "deleted.txt"},
		[]string{"personal:Alice.A@example.test", "", "business:bob_b@example.test", " personal:Alice.A@example.test "})
	require.NoError(t, err)

	assert.Equal(t, []AccountFinding{
		{File: "docs/a.md", Line: 3, Account: "drive-1-account"},
		{File: "code/b.go", Line: 1, Account: "drive-1-account"},
		{File: "c.txt", Line: 1, Account: "drive-1-account"},
		{File: "c.txt", Line: 1, Account: "drive-2-account"},
		{File: "binary.bin", Line: 1, Account: "drive-1-account"},
	}, findings)
	for _, finding := range findings {
		assert.NotContains(t, strings.ToLower(finding.String()), "example.test")
	}
	assert.Equal(t, "docs/a.md:3 spells drive-1-account", findings[0].String())
}
