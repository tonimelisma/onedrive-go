//go:build integration

package graph

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// integrationRealHomeDir holds the original HOME directory before TestMain
// overrides it. Used by isolation tests.
var integrationRealHomeDir string

// loadIntegrationDotEnv reads KEY=VALUE pairs from .env at the module root.
func loadIntegrationDotEnv() {
	root := findIntegrationModuleRoot()
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

		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// validateIntegrationAllowlist crashes if ONEDRIVE_ALLOWED_TEST_ACCOUNTS is
// not set or ONEDRIVE_TEST_DRIVE is not in the allowlist.
func validateIntegrationAllowlist() {
	allowlist := os.Getenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS")
	if allowlist == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ONEDRIVE_ALLOWED_TEST_ACCOUNTS not set")
		fmt.Fprintln(os.Stderr, "Set it in .env or as an environment variable.")
		os.Exit(1)
	}

	testDrive := os.Getenv(driveEnvVar)
	if testDrive == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ONEDRIVE_TEST_DRIVE not set")
		os.Exit(1)
	}

	for _, a := range strings.Split(allowlist, ",") {
		if strings.TrimSpace(a) == testDrive {
			return
		}
	}

	fmt.Fprintf(os.Stderr, "FATAL: %s=%q is not in ONEDRIVE_ALLOWED_TEST_ACCOUNTS=%q\n",
		driveEnvVar, testDrive, allowlist)
	os.Exit(1)
}

// setupIntegrationIsolation overrides HOME and XDG directories to temp
// directories and copies the test token file. Returns a cleanup function.
func setupIntegrationIsolation() func() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	integrationRealHomeDir = home

	// Capture real data dir before overriding env.
	realDataDir := config.DefaultDataDir()

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

	// Copy token file to isolated data dir.
	appDataDir := filepath.Join(tempData, "onedrive-go")
	if mkErr := os.MkdirAll(appDataDir, 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating app data dir: %v\n", mkErr)
		os.Exit(1)
	}

	drive := os.Getenv(driveEnvVar)
	if drive == "" {
		drive = defaultTestDrive
	}

	copyIntegrationToken(realDataDir, appDataDir, drive)

	fmt.Fprintf(os.Stderr, "Integration isolation: HOME=%s XDG_DATA_HOME=%s\n", tempHome, tempData)

	return func() { os.RemoveAll(tempRoot) }
}

// copyIntegrationToken copies the token file for the given drive.
func copyIntegrationToken(srcDir, dstDir, drive string) {
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
		fmt.Fprintln(os.Stderr, "Run 'onedrive-go login' first to create a token.")
		os.Exit(1)
	}

	if writeErr := os.WriteFile(filepath.Join(dstDir, tokenName), data, 0o600); writeErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: writing token file: %v\n", writeErr)
		os.Exit(1)
	}
}

// findIntegrationModuleRoot walks up from CWD to find go.mod.
func findIntegrationModuleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "../.."
		}

		dir = parent
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
