//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/testutil"
)

// testDataDir holds the path to the isolated data directory containing the
// test token file. Set by TestMain, used by writeSyncConfig for per-test
// token copying.
var testDataDir string

// realHomeDir holds the original HOME directory before TestMain overrides it.
// Used by isolation tests to verify env overrides are in effect.
var realHomeDir string

// testCredentialDir holds the path to .testdata/ (repo-root-relative).
// Token files and config are read from here, never from production dirs.
var testCredentialDir string

// validateTestData checks that .testdata/ has the expected structure before
// tests start. Catches bootstrap failures with actionable error messages.
// E2E tests can't import internal packages, so validation uses stdlib JSON.
func validateTestData(credDir, driveID string) {
	tokenName := testutil.TokenFileName(driveID)
	tokenPath := filepath.Join(credDir, tokenName)

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read token file %s: %v\n", tokenPath, err)
		fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
		os.Exit(1)
	}

	var parsed map[string]json.RawMessage
	if jsonErr := json.Unmarshal(data, &parsed); jsonErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: token file %s is not valid JSON: %v\n", tokenPath, jsonErr)
		fmt.Fprintln(os.Stderr, "Re-run scripts/bootstrap-test-credentials.sh.")
		os.Exit(1)
	}

	if _, ok := parsed["token"]; !ok {
		fmt.Fprintf(os.Stderr, "FATAL: token file %s missing \"token\" key\n", tokenPath)
		fmt.Fprintln(os.Stderr, "Re-run scripts/bootstrap-test-credentials.sh.")
		os.Exit(1)
	}

	if _, ok := parsed["meta"]; !ok {
		fmt.Fprintf(os.Stderr, "FATAL: token file %s missing \"meta\" key\n", tokenPath)
		fmt.Fprintln(os.Stderr, "Re-run scripts/bootstrap-test-credentials.sh.")
		os.Exit(1)
	}

	configPath := filepath.Join(credDir, "config.toml")
	if _, statErr := os.Stat(configPath); statErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: config.toml not found at %s\n", configPath)
		fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
		os.Exit(1)
	}
}

// setupIsolation overrides HOME and XDG directories to temp directories,
// copies the test token and config from .testdata/, and verifies isolation.
// Must be called AFTER drive is set. Returns a cleanup function that
// copies rotated tokens back and removes the temp root.
func setupIsolation() func() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	realHomeDir = home
	moduleRoot := findModuleRoot()
	testCredentialDir = testutil.FindTestCredentialDir(moduleRoot)
	validateTestData(testCredentialDir, drive)

	// Unset app-specific env vars that could leak production paths.
	os.Unsetenv("ONEDRIVE_GO_CONFIG")
	os.Unsetenv("ONEDRIVE_GO_DRIVE")

	tempRoot, err := os.MkdirTemp("", "onedrive-e2e-isolation-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating isolation temp dir: %v\n", err)
		os.Exit(1)
	}

	tempHome := filepath.Join(tempRoot, "home")
	tempConfig := filepath.Join(tempRoot, "config")
	tempData := filepath.Join(tempRoot, "data")
	tempCache := filepath.Join(tempRoot, "cache")

	for _, d := range []string{tempHome, tempConfig, tempData, tempCache} {
		if mkErr := os.MkdirAll(d, 0o755); mkErr != nil {
			fmt.Fprintf(os.Stderr, "FATAL: creating dir %s: %v\n", d, mkErr)
			os.Exit(1)
		}
	}

	os.Setenv("HOME", tempHome)
	os.Setenv("XDG_CONFIG_HOME", tempConfig)
	os.Setenv("XDG_DATA_HOME", tempData)
	os.Setenv("XDG_CACHE_HOME", tempCache)

	// Copy token file from .testdata/ to isolated data dir.
	appDataDir := filepath.Join(tempData, "onedrive-go")
	if mkErr := os.MkdirAll(appDataDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating app data dir: %v\n", mkErr)
		os.Exit(1)
	}

	tokenName := testutil.TokenFileName(drive)
	testutil.CopyFile(
		filepath.Join(testCredentialDir, tokenName),
		filepath.Join(appDataDir, tokenName),
		0o600,
	)
	testDataDir = appDataDir

	// Copy config.toml from .testdata/ to isolated config dir.
	appConfigDir := filepath.Join(tempConfig, "onedrive-go")
	if mkErr := os.MkdirAll(appConfigDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating app config dir: %v\n", mkErr)
		os.Exit(1)
	}

	testutil.CopyFile(
		filepath.Join(testCredentialDir, "config.toml"),
		filepath.Join(appConfigDir, "config.toml"),
		0o644,
	)

	// Hard crash guards: verify isolation BEFORE any tests run.
	verifyIsolation(tempRoot)

	fmt.Fprintf(os.Stderr, "E2E isolation: HOME=%s XDG_DATA_HOME=%s (credentials from .testdata/)\n", tempHome, tempData)

	return func() {
		// Preserve rotated token: copy back from temp to .testdata/.
		rotatedPath := filepath.Join(appDataDir, tokenName)
		if _, statErr := os.Stat(rotatedPath); statErr == nil {
			origPath := filepath.Join(testCredentialDir, tokenName)
			data, readErr := os.ReadFile(rotatedPath)
			if readErr != nil {
				fmt.Fprintf(os.Stderr, "WARNING: cannot read rotated token %s: %v\n", rotatedPath, readErr)
			} else if writeErr := os.WriteFile(origPath, data, 0o600); writeErr != nil {
				fmt.Fprintf(os.Stderr, "WARNING: cannot write rotated token back to %s: %v\n", origPath, writeErr)
			}
		}

		os.RemoveAll(tempRoot)
	}
}

// verifyIsolation hard-crashes the process if any production path could leak
// into test execution. Runs BEFORE m.Run() so no tests execute if isolation
// is broken.
func verifyIsolation(tempRoot string) {
	crash := func(msg string) {
		fmt.Fprintf(os.Stderr, "FATAL: isolation check failed: %s\n", msg)
		os.Exit(1)
	}

	// 1. Production env vars must not be set.
	if os.Getenv("ONEDRIVE_GO_CONFIG") != "" {
		crash("ONEDRIVE_GO_CONFIG is set — would leak production config into tests")
	}

	if os.Getenv("ONEDRIVE_GO_DRIVE") != "" {
		crash("ONEDRIVE_GO_DRIVE is set — would leak production drive into tests")
	}

	// 2. All XDG/HOME vars must point to temp (not production).
	for _, v := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		val := os.Getenv(v)
		if val == "" || !strings.HasPrefix(val, tempRoot) {
			crash(v + " not overridden to temp dir")
		}
	}

	// Check #3 (platform-specific path resolution) is deliberately omitted
	// for E2E tests. E2E can't import internal/config, so any resolution
	// check would be an inaccurate mirror. The integration tests verify
	// config.Default*Dir() using the real functions. Check #2 above is
	// sufficient for E2E since all path resolution starts with the env var.

	// 3. os.UserHomeDir() must return temp home.
	homeDir, _ := os.UserHomeDir()
	if !strings.HasPrefix(homeDir, tempRoot) {
		crash("UserHomeDir() returns " + homeDir + " (not under temp)")
	}
}

// --- Isolation verification tests (belt-and-suspenders with verifyIsolation) ---

func TestIsolation_HomeOverridden(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.NotEqual(t, realHomeDir, home, "HOME should be overridden to temp dir")
}

func TestIsolation_XDGDataDir(t *testing.T) {
	xdg := os.Getenv("XDG_DATA_HOME")
	assert.NotEmpty(t, xdg, "XDG_DATA_HOME should be set")
	assert.NotContains(t, xdg, realHomeDir, "XDG_DATA_HOME should not be under real home")
}

func TestIsolation_XDGConfigDir(t *testing.T) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	assert.NotEmpty(t, xdg, "XDG_CONFIG_HOME should be set")
	assert.NotContains(t, xdg, realHomeDir, "XDG_CONFIG_HOME should not be under real home")
}

func TestIsolation_XDGCacheDir(t *testing.T) {
	xdg := os.Getenv("XDG_CACHE_HOME")
	assert.NotEmpty(t, xdg, "XDG_CACHE_HOME should be set")
	assert.NotContains(t, xdg, realHomeDir, "XDG_CACHE_HOME should not be under real home")
}

func TestIsolation_TokenInTempDir(t *testing.T) {
	assert.NotEmpty(t, testDataDir, "testDataDir should be set by TestMain")
	assert.NotContains(t, testDataDir, realHomeDir,
		"testDataDir should not be under real home")

	tokenName := testutil.TokenFileName(drive)
	tokenPath := filepath.Join(testDataDir, tokenName)

	_, err := os.Stat(tokenPath)
	assert.NoError(t, err, "token file should exist at %s", tokenPath)
}

func TestIsolation_ConfigInTempDir(t *testing.T) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	require.NotEmpty(t, configDir)

	configPath := filepath.Join(configDir, "onedrive-go", "config.toml")
	_, err := os.Stat(configPath)
	assert.NoError(t, err, "config.toml should exist at %s", configPath)
}

func TestIsolation_CredentialsFromTestdata(t *testing.T) {
	assert.NotEmpty(t, testCredentialDir, "testCredentialDir should be set by TestMain")
	assert.True(t, strings.HasSuffix(testCredentialDir, ".testdata"),
		"credentials should come from .testdata/, got: %s", testCredentialDir)
}

// TestIsolation_BinaryResolvesTemp verifies that the CLI binary process
// resolves all paths under the temp isolation directory, not under the real
// home. Runs `status` with --debug and checks that no production paths leak
// into the output.
func TestIsolation_BinaryResolvesTemp(t *testing.T) {
	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)

	// Run status with debug output to capture path resolution logs.
	stdout, stderr := runCLIWithConfig(t, cfgPath, env, "status")

	// The real home directory must not appear anywhere in the output.
	// This proves the binary resolved paths using the temp env overrides.
	assert.NotContains(t, stdout, realHomeDir,
		"binary stdout should not contain real home dir")
	assert.NotContains(t, stderr, realHomeDir,
		"binary stderr should not contain real home dir")

	// The binary should have found the token (proves data dir resolution
	// went to temp, not production).
	assert.Contains(t, stdout, "Token:", "status should show token info")
	assert.NotContains(t, stdout, "No accounts configured",
		"binary should find the test account via isolated config")
}
