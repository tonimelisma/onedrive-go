//go:build e2e

package e2e

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDataDir holds the path to the isolated data directory containing the
// test token file. Set by TestMain, used by writeSyncConfig for per-test
// token copying.
var testDataDir string

// realHomeDir holds the original HOME directory before TestMain overrides it.
// Used by isolation tests to verify env overrides are in effect.
var realHomeDir string

// loadDotEnv reads KEY=VALUE pairs from a .env file in the module root.
// Missing file is not an error (CI sets env vars directly).
func loadDotEnv() {
	root := findModuleRoot()
	path := filepath.Join(root, ".env")

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, "\"'")

		// Env vars take precedence over .env file.
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// validateAllowlist crashes the test process if ONEDRIVE_ALLOWED_TEST_ACCOUNTS
// is not set or if ONEDRIVE_TEST_DRIVE is not in the allowlist. This prevents
// tests from accidentally running against production accounts.
func validateAllowlist() {
	allowlist := os.Getenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS")
	if allowlist == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ONEDRIVE_ALLOWED_TEST_ACCOUNTS not set")
		fmt.Fprintln(os.Stderr, "Set it in .env or as an environment variable.")
		fmt.Fprintln(os.Stderr, "Example: ONEDRIVE_ALLOWED_TEST_ACCOUNTS=personal:user@outlook.com")
		os.Exit(1)
	}

	testDrive := os.Getenv("ONEDRIVE_TEST_DRIVE")
	if testDrive == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ONEDRIVE_TEST_DRIVE not set")
		os.Exit(1)
	}

	for _, a := range strings.Split(allowlist, ",") {
		if strings.TrimSpace(a) == testDrive {
			return
		}
	}

	fmt.Fprintf(os.Stderr, "FATAL: ONEDRIVE_TEST_DRIVE=%q is not in ONEDRIVE_ALLOWED_TEST_ACCOUNTS=%q\n",
		testDrive, allowlist)
	os.Exit(1)
}

// setupIsolation overrides HOME and XDG directories to temp directories and
// copies the test token file. Must be called AFTER drive is set. Returns a
// cleanup function that removes the temp root.
func setupIsolation() func() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	realHomeDir = home
	realDataDirPath := e2eDataDir(home)

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

	appDataDir := filepath.Join(tempData, "onedrive-go")
	if mkErr := os.MkdirAll(appDataDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating app data dir: %v\n", mkErr)
		os.Exit(1)
	}

	copyTokenForSetup(realDataDirPath, appDataDir)
	testDataDir = appDataDir

	// Create a minimal config file so "no_config" mode tests (which rely on
	// the default config path) find the test drive configured.
	appConfigDir := filepath.Join(tempConfig, "onedrive-go")
	if mkErr := os.MkdirAll(appConfigDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating app config dir: %v\n", mkErr)
		os.Exit(1)
	}

	configContent := fmt.Sprintf("[%q]\n", drive)
	configPath := filepath.Join(appConfigDir, "config.toml")

	if writeErr := os.WriteFile(configPath, []byte(configContent), 0o644); writeErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: writing config file: %v\n", writeErr)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "E2E isolation: HOME=%s XDG_DATA_HOME=%s\n", tempHome, tempData)

	return func() { os.RemoveAll(tempRoot) }
}

// copyTokenForSetup copies the token file for the test drive during TestMain.
// Crashes on failure because tests cannot proceed without authentication.
func copyTokenForSetup(srcDir, dstDir string) {
	parts := strings.SplitN(drive, ":", 2)
	if len(parts) < 2 {
		fmt.Fprintf(os.Stderr, "FATAL: cannot parse drive %q for token filename\n", drive)
		os.Exit(1)
	}

	tokenName := "token_" + parts[0] + "_" + parts[1] + ".json"
	srcPath := filepath.Join(srcDir, tokenName)

	data, err := os.ReadFile(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read token file %s: %v\n", srcPath, err)
		os.Exit(1)
	}

	if writeErr := os.WriteFile(filepath.Join(dstDir, tokenName), data, 0o600); writeErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: writing token file: %v\n", writeErr)
		os.Exit(1)
	}
}

// --- Isolation verification tests ---

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

	parts := strings.SplitN(drive, ":", 2)
	require.Len(t, parts, 2)

	tokenName := "token_" + parts[0] + "_" + parts[1] + ".json"
	tokenPath := filepath.Join(testDataDir, tokenName)

	_, err := os.Stat(tokenPath)
	assert.NoError(t, err, "token file should exist at %s", tokenPath)
}
