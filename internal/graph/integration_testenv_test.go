//go:build integration

package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
	"github.com/tonimelisma/onedrive-go/testutil"
)

// integrationRealHomeDir holds the original HOME directory before TestMain
// overrides it. Used by isolation tests.
var integrationRealHomeDir string

// integrationTestCredentialDir holds the path to .testdata/.
var integrationTestCredentialDir string

// validateIntegrationTestData checks that .testdata/ has the expected structure.
// Integration tests CAN import internal packages, so we use tokenfile.ReadMeta().
func validateIntegrationTestData(credDir, driveID string) {
	tokenName := testutil.TokenFileName(driveID)
	tokenPath := filepath.Join(credDir, tokenName)

	meta, err := tokenfile.ReadMeta(tokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read token metadata from %s: %v\n", tokenPath, err)
		fmt.Fprintln(os.Stderr, "Re-run scripts/bootstrap-test-credentials.sh.")
		os.Exit(1)
	}

	if id, ok := meta["drive_id"]; !ok || id == "" {
		fmt.Fprintf(os.Stderr, "FATAL: token file %s missing .meta.drive_id\n", tokenPath)
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

// setupIntegrationIsolation overrides HOME and XDG directories to temp
// directories, copies the test token and config from .testdata/, and
// verifies isolation. Returns a cleanup function.
func setupIntegrationIsolation() func() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	integrationRealHomeDir = home

	// Fallback to "../.." — internal/graph/ is two levels below module root.
	moduleRoot := testutil.FindModuleRoot("../..")
	integrationTestCredentialDir = testutil.FindTestCredentialDir(moduleRoot)

	drive := os.Getenv(driveEnvVar)
	validateIntegrationTestData(integrationTestCredentialDir, drive)

	// Unset app-specific env vars that could leak production paths.
	os.Unsetenv("ONEDRIVE_GO_CONFIG")
	os.Unsetenv("ONEDRIVE_GO_DRIVE")

	tempRoot, err := os.MkdirTemp("", "onedrive-integration-isolation-*")
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
		filepath.Join(integrationTestCredentialDir, tokenName),
		filepath.Join(appDataDir, tokenName),
		0o600,
	)

	// Copy config.toml from .testdata/ to isolated config dir.
	appConfigDir := filepath.Join(tempConfig, "onedrive-go")
	if mkErr := os.MkdirAll(appConfigDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating app config dir: %v\n", mkErr)
		os.Exit(1)
	}

	testutil.CopyFile(
		filepath.Join(integrationTestCredentialDir, "config.toml"),
		filepath.Join(appConfigDir, "config.toml"),
		0o644,
	)

	// Hard crash guards: verify isolation BEFORE any tests run.
	verifyIntegrationIsolation(tempRoot)

	fmt.Fprintf(os.Stderr, "Integration isolation: HOME=%s XDG_DATA_HOME=%s (credentials from .testdata/)\n", tempHome, tempData)

	return func() {
		// Preserve rotated token: copy back from temp to .testdata/.
		rotatedPath := filepath.Join(appDataDir, tokenName)
		if _, statErr := os.Stat(rotatedPath); statErr == nil {
			origPath := filepath.Join(integrationTestCredentialDir, tokenName)
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

// verifyIntegrationIsolation hard-crashes if any production path could leak.
func verifyIntegrationIsolation(tempRoot string) {
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

	// 2. All XDG/HOME vars must point to temp.
	for _, v := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		val := os.Getenv(v)
		if val == "" || !strings.HasPrefix(val, tempRoot) {
			crash(v + " not overridden to temp dir")
		}
	}

	// 3. DefaultDataDir/ConfigDir/CacheDir must resolve under temp.
	// Integration tests CAN import internal/config.
	for name, fn := range map[string]func() string{
		"DefaultDataDir":   config.DefaultDataDir,
		"DefaultConfigDir": config.DefaultConfigDir,
		"DefaultCacheDir":  config.DefaultCacheDir,
	} {
		dir := fn()
		if !strings.HasPrefix(dir, tempRoot) {
			crash(name + " resolves to " + dir + " (not under temp)")
		}
	}

	// 4. os.UserHomeDir() must return temp home.
	homeDir, _ := os.UserHomeDir()
	if !strings.HasPrefix(homeDir, tempRoot) {
		crash("UserHomeDir() returns " + homeDir + " (not under temp)")
	}
}

// --- Isolation verification tests ---

func TestIntegration_Isolation_HomeOverridden(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("cannot get home dir: %v", err)
	}

	assert.NotEqual(t, integrationRealHomeDir, home, "HOME should be overridden")
}

func TestIntegration_Isolation_XDGDataDir(t *testing.T) {
	xdg := os.Getenv("XDG_DATA_HOME")
	assert.NotEmpty(t, xdg, "XDG_DATA_HOME should be set")
	assert.NotContains(t, xdg, integrationRealHomeDir)
}

func TestIntegration_Isolation_DataDirResolvesToTemp(t *testing.T) {
	dataDir := config.DefaultDataDir()
	assert.NotContains(t, dataDir, integrationRealHomeDir,
		"DefaultDataDir() should resolve under temp, not real home")
}

func TestIntegration_Isolation_CredentialsFromTestdata(t *testing.T) {
	assert.NotEmpty(t, integrationTestCredentialDir)
	assert.True(t, strings.HasSuffix(integrationTestCredentialDir, ".testdata"),
		"credentials should come from .testdata/, got: %s", integrationTestCredentialDir)
}
