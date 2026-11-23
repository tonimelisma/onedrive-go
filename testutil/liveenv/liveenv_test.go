package liveenv

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyDotEnv(t *testing.T) {
	t.Setenv("KEEP_EXISTING", "keep")

	err := ApplyDotEnv(strings.NewReader(`
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

// Validates: R-6.10.27
//
// The files are read through the capability the caller hands over: the root
// .env first, then the fixture defaults, a missing file is no error, and a
// file the capability cannot open is one that names its path.
func TestLoadEnvFilesReadsThroughTheCallersOpener(t *testing.T) {
	t.Setenv("LIVEENV_FROM_ENV", "")
	t.Setenv("LIVEENV_SHARED", "")
	files := map[string]string{
		filepath.Join("root", dotEnvFile):           "LIVEENV_FROM_ENV=env\nLIVEENV_SHARED=from-env\n",
		filepath.Join("root", sharedFixtureEnvPath): "LIVEENV_SHARED=from-fixture\n",
	}
	opened := []string{}
	open := func(path string) (io.ReadCloser, error) {
		opened = append(opened, path)
		content, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}

		return io.NopCloser(strings.NewReader(content)), nil
	}

	require.NoError(t, LoadEnvFiles(open, "root"))
	assert.Equal(t, []string{filepath.Join("root", dotEnvFile), filepath.Join("root", sharedFixtureEnvPath)}, opened)
	assert.Equal(t, "env", os.Getenv("LIVEENV_FROM_ENV"))
	assert.Equal(t, "from-env", os.Getenv("LIVEENV_SHARED"), "the root .env overrides the fixture defaults")

	require.NoError(t, LoadDotEnvFile(open, "missing.env"))
	errRefused := errors.New("permission denied")
	err := LoadDotEnvFile(func(string) (io.ReadCloser, error) { return nil, errRefused }, "refused.env")
	require.ErrorIs(t, err, errRefused)
	assert.Contains(t, err.Error(), "refused.env")
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
