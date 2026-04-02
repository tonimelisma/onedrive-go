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

func TestCopyMetadataFiles(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "account_a.json"), []byte("account"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "drive_b.json"), []byte("drive"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "other.json"), []byte("ignore"), 0o600))

	CopyMetadataFiles(srcDir, dstDir)

	accountData, err := localpath.ReadFile(filepath.Join(dstDir, "account_a.json"))
	require.NoError(t, err)
	assert.Equal(t, "account", string(accountData))

	driveData, err := localpath.ReadFile(filepath.Join(dstDir, "drive_b.json"))
	require.NoError(t, err)
	assert.Equal(t, "drive", string(driveData))

	_, err = os.Stat(filepath.Join(dstDir, "other.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
