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
		"ONEDRIVE_TEST_DRIVE=personal:kikkelimies123@outlook.com\n"+
			"ONEDRIVE_TEST_DRIVE_2=personal:testitesti18@outlook.com\n",
	), 0o600))

	cfg, err := LoadLiveTestConfig(moduleRoot)
	require.NoError(t, err)

	assert.Equal(t, "personal:testitesti18@outlook.com", cfg.Fixtures.WritableShortcutParentDrive)
	assert.Equal(t, "Kikkeli Shared Test Folder", cfg.Fixtures.WritableShortcutName)
	assert.Equal(t, "kikkelimies123@outlook.com", cfg.Fixtures.WritableShortcutSharerEmail)
	assert.Equal(t, "shortcut-sentinel.txt", cfg.Fixtures.WritableShortcutSentinelPath)
	assert.Equal(t, "personal:kikkelimies123@outlook.com", cfg.Fixtures.ReadOnlyShortcutParentDrive)
	assert.Equal(t, "Read-only Shared Folder", cfg.Fixtures.ReadOnlyShortcutName)
	assert.Equal(t, "testitesti18@outlook.com", cfg.Fixtures.ReadOnlyShortcutSharerEmail)
	assert.Equal(t, "shortcut-sentinel.txt", cfg.Fixtures.ReadOnlyShortcutSentinelPath)
	assert.Equal(t, "Testi2 Shared Folder", cfg.Fixtures.StandaloneSharedFolderName)
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
