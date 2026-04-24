// Package testutil provides shared test-environment helpers for E2E and
// integration tests.
package testutil

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

const metadataDirPerms = 0o755

const sharedFixtureEnvPath = ".testdata/fixtures.env"
const (
	testBundleConfigFile  = "config.toml"
	testBundleCatalogFile = "catalog.json"
	testConfigFilePerms   = 0o644
)

const (
	defaultWritableShortcutRecipientEmail = "testitesti18@outlook.com"
	defaultReadOnlyShortcutRecipientEmail = "kikkelimies123@outlook.com"
	defaultWritableShortcutName           = "Kikkeli Shared Test Folder"
	defaultReadOnlyShortcutName           = "Read-only Shared Folder"
	defaultStandaloneSharedFolderName     = "Testi2 Shared Folder"
	defaultShortcutSentinelPath           = "shortcut-sentinel.txt"
)

// LiveFixtures captures durable live Graph fixtures loaded from the test
// environment. The values stay as strings so testutil's exported API does not
// leak internal package types.
type LiveFixtures struct {
	SharedFileLink               string
	SharedFolderLink             string
	WritableSharedFolderSelector string
	ReadOnlySharedFolderSelector string
	WritableShortcutParentDrive  string
	WritableShortcutName         string
	WritableShortcutSharerEmail  string
	WritableShortcutSentinelPath string
	ReadOnlyShortcutParentDrive  string
	ReadOnlyShortcutName         string
	ReadOnlyShortcutSharerEmail  string
	ReadOnlyShortcutSentinelPath string
	StandaloneSharedFolderName   string
}

// LiveTestConfig is the typed live-test view over the loaded environment.
// PrimaryDrive is required for all live suites; SecondaryDrive is optional.
type LiveTestConfig struct {
	PrimaryDrive   string
	SecondaryDrive string
	Fixtures       LiveFixtures
}

// LoadDotEnv reads KEY=VALUE pairs from a .env file at the given path.
// Missing file is not an error (CI sets env vars directly).
// Existing env vars take precedence over .env values.
func LoadDotEnv(envPath string) {
	f, err := localpath.Open(envPath)
	if err != nil {
		return
	}

	if err := applyDotEnv(f); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			fatalTestEnv()
		}

		fatalTestEnv()
	}

	if err := f.Close(); err != nil {
		fatalTestEnv()
	}
}

// LoadTestEnv loads live-test environment files in repo precedence order.
// Exported process env wins over both files. Root .env overrides durable
// fixture defaults from .testdata/fixtures.env.
func LoadTestEnv(moduleRoot string) {
	LoadDotEnv(filepath.Join(moduleRoot, ".env"))
	LoadDotEnv(filepath.Join(moduleRoot, sharedFixtureEnvPath))
}

// LoadLiveTestConfig loads the live-test env files and returns a typed view of
// the resulting configuration. Optional shared-folder fixtures are validated as
// real shared selectors up front so E2E code does not re-parse raw env vars.
func LoadLiveTestConfig(moduleRoot string) (LiveTestConfig, error) {
	LoadTestEnv(moduleRoot)

	cfg := LiveTestConfig{
		PrimaryDrive:   os.Getenv("ONEDRIVE_TEST_DRIVE"),
		SecondaryDrive: os.Getenv("ONEDRIVE_TEST_DRIVE_2"),
		Fixtures: LiveFixtures{
			SharedFileLink:               os.Getenv("ONEDRIVE_TEST_SHARED_LINK"),
			SharedFolderLink:             os.Getenv("ONEDRIVE_TEST_SHARED_FOLDER_LINK"),
			WritableSharedFolderSelector: os.Getenv("ONEDRIVE_TEST_WRITABLE_SHARED_FOLDER"),
			ReadOnlySharedFolderSelector: os.Getenv("ONEDRIVE_TEST_READONLY_SHARED_FOLDER"),
			WritableShortcutParentDrive:  os.Getenv("ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE"),
			WritableShortcutName:         os.Getenv("ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME"),
			WritableShortcutSharerEmail:  os.Getenv("ONEDRIVE_TEST_SHORTCUT_WRITABLE_SHARER_EMAIL"),
			WritableShortcutSentinelPath: os.Getenv("ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH"),
			ReadOnlyShortcutParentDrive:  os.Getenv("ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE"),
			ReadOnlyShortcutName:         os.Getenv("ONEDRIVE_TEST_SHORTCUT_READONLY_NAME"),
			ReadOnlyShortcutSharerEmail:  os.Getenv("ONEDRIVE_TEST_SHORTCUT_READONLY_SHARER_EMAIL"),
			ReadOnlyShortcutSentinelPath: os.Getenv("ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH"),
			StandaloneSharedFolderName:   os.Getenv("ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME"),
		},
	}

	if cfg.PrimaryDrive == "" {
		return LiveTestConfig{}, fmt.Errorf("load live test config: ONEDRIVE_TEST_DRIVE not set")
	}

	applyShortcutFixtureDefaults(&cfg)

	for _, selector := range []struct {
		name  string
		value string
	}{
		{
			name:  "ONEDRIVE_TEST_WRITABLE_SHARED_FOLDER",
			value: cfg.Fixtures.WritableSharedFolderSelector,
		},
		{
			name:  "ONEDRIVE_TEST_READONLY_SHARED_FOLDER",
			value: cfg.Fixtures.ReadOnlySharedFolderSelector,
		},
	} {
		if selector.value == "" {
			continue
		}

		if _, err := sharedref.Parse(selector.value); err != nil {
			return LiveTestConfig{}, fmt.Errorf("load live test config: parse %s: %w", selector.name, err)
		}
	}

	return cfg, nil
}

func applyShortcutFixtureDefaults(cfg *LiveTestConfig) {
	if cfg.Fixtures.WritableShortcutParentDrive == "" {
		if driveID, ok := cfg.DriveIDForEmail(defaultWritableShortcutRecipientEmail); ok {
			cfg.Fixtures.WritableShortcutParentDrive = driveID
		}
	}
	if cfg.Fixtures.ReadOnlyShortcutParentDrive == "" {
		if driveID, ok := cfg.DriveIDForEmail(defaultReadOnlyShortcutRecipientEmail); ok {
			cfg.Fixtures.ReadOnlyShortcutParentDrive = driveID
		}
	}

	if cfg.Fixtures.WritableShortcutName == "" {
		cfg.Fixtures.WritableShortcutName = defaultWritableShortcutName
	}
	if cfg.Fixtures.ReadOnlyShortcutName == "" {
		cfg.Fixtures.ReadOnlyShortcutName = defaultReadOnlyShortcutName
	}
	if cfg.Fixtures.WritableShortcutSentinelPath == "" {
		cfg.Fixtures.WritableShortcutSentinelPath = defaultShortcutSentinelPath
	}
	if cfg.Fixtures.ReadOnlyShortcutSentinelPath == "" {
		cfg.Fixtures.ReadOnlyShortcutSentinelPath = defaultShortcutSentinelPath
	}
	if cfg.Fixtures.StandaloneSharedFolderName == "" {
		cfg.Fixtures.StandaloneSharedFolderName = defaultStandaloneSharedFolderName
	}

	if cfg.Fixtures.WritableShortcutSharerEmail == "" {
		cfg.Fixtures.WritableShortcutSharerEmail = driveEmailFromCanonicalID(cfg.Fixtures.ReadOnlyShortcutParentDrive)
	}
	if cfg.Fixtures.ReadOnlyShortcutSharerEmail == "" {
		cfg.Fixtures.ReadOnlyShortcutSharerEmail = driveEmailFromCanonicalID(cfg.Fixtures.WritableShortcutParentDrive)
	}
}

// CandidateDriveIDs returns the configured live drive IDs without duplicates.
func (cfg *LiveTestConfig) CandidateDriveIDs() []string {
	candidates := make([]string, 0, 2)
	seen := map[string]struct{}{}

	for _, candidate := range []string{cfg.PrimaryDrive, cfg.SecondaryDrive} {
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}

		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}

	return candidates
}

// DriveIDForEmail returns the configured drive whose canonical ID embeds the
// given recipient email.
func (cfg *LiveTestConfig) DriveIDForEmail(email string) (string, bool) {
	for _, driveID := range cfg.CandidateDriveIDs() {
		if driveEmailFromCanonicalID(driveID) == email {
			return driveID, true
		}
	}

	return "", false
}

func driveEmailFromCanonicalID(driveID string) string {
	parts := strings.SplitN(driveID, ":", 2)
	if len(parts) != 2 {
		return ""
	}

	return parts[1]
}

func applyDotEnv(r io.Reader) error {
	scanner := bufio.NewScanner(r)
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
			if setErr := os.Setenv(key, value); setErr != nil {
				return fmt.Errorf("setting environment from .env")
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading .env file")
	}

	return nil
}

func fatalTestEnv() {
	if _, err := os.Stderr.WriteString("FATAL: test environment setup failed\n"); err != nil {
		os.Exit(1)
	}

	os.Exit(1)
}

// ValidateAllowlist crashes the process if ONEDRIVE_ALLOWED_TEST_ACCOUNTS
// is not set or if the given drive is not in the allowlist.
func ValidateAllowlist(driveEnvVar string) {
	allowlist := os.Getenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS")
	if allowlist == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ONEDRIVE_ALLOWED_TEST_ACCOUNTS not set")
		fmt.Fprintln(os.Stderr, "Set it in .env or as an environment variable.")
		fmt.Fprintln(os.Stderr, "Example: ONEDRIVE_ALLOWED_TEST_ACCOUNTS=personal:user@outlook.com")
		os.Exit(1)
	}

	testDrive := os.Getenv(driveEnvVar)
	if testDrive == "" {
		fmt.Fprintf(os.Stderr, "FATAL: %s not set\n", driveEnvVar)
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

// FindModuleRoot walks up from the current directory to find go.mod.
// Returns the fallback if the root is not found.
func FindModuleRoot(fallback string) string {
	dir, err := os.Getwd()
	if err != nil {
		return fallback
	}
	for {
		if _, err := localpath.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return fallback
		}

		dir = parent
	}
}

// FindTestCredentialDir locates the root .testdata/ test bundle relative
// to the module root. The durable bundle contract is config.toml,
// catalog.json, token_*.json, and optional fixtures.env.
func FindTestCredentialDir(moduleRoot string) string {
	dir := filepath.Join(moduleRoot, ".testdata")

	if _, err := localpath.Stat(dir); err != nil {
		fmt.Fprintln(os.Stderr, "FATAL: .testdata/ directory not found at "+dir)
		fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
		os.Exit(1)
	}

	return dir
}

// TokenFileName returns the token filename for the given canonical drive ID.
// Format: token_<type>_<email>.json (e.g., token_personal_user@outlook.com.json).
func TokenFileName(driveID string) string {
	parts := strings.SplitN(driveID, ":", 2)
	if len(parts) < 2 {
		fmt.Fprintf(os.Stderr, "FATAL: cannot parse drive %q for token filename\n", driveID)
		os.Exit(1)
	}

	return "token_" + parts[0] + "_" + parts[1] + ".json"
}

// metadataFilePerms is owner-only read/write for managed test credential files.
const metadataFilePerms = 0o600

// CopyCatalogFile copies the managed catalog from srcDir to dstDir when present.
// Missing catalogs are silently skipped; copy failures are fatal.
func CopyCatalogFile(srcDir, dstDir string) {
	src := filepath.Join(srcDir, testBundleCatalogFile)
	if _, err := localpath.Stat(src); err != nil {
		return
	}

	CopyFile(src, filepath.Join(dstDir, testBundleCatalogFile), metadataFilePerms)
}

// EnsureTestCredentialBundle verifies that the durable root bundle contains
// config.toml, catalog.json, and token files for the requested drive IDs.
// Crashes on failure because live tests cannot proceed without the bundle.
func EnsureTestCredentialBundle(dir string, driveIDs []string) {
	required := []string{
		filepath.Join(dir, testBundleConfigFile),
		filepath.Join(dir, testBundleCatalogFile),
	}
	for _, path := range required {
		if _, err := localpath.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: required test credential file not found: %s\n", path)
			fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
			os.Exit(1)
		}
	}

	for _, driveID := range driveIDs {
		tokenPath := filepath.Join(dir, TokenFileName(driveID))
		if _, err := localpath.Stat(tokenPath); err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: required token file not found: %s\n", tokenPath)
			fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
			os.Exit(1)
		}
	}
}

// CopyTestCredentialBundle copies the durable root credential bundle into an
// isolated config/data layout for tests.
func CopyTestCredentialBundle(srcDir, dstConfigDir, dstDataDir string, driveIDs []string) {
	EnsureTestCredentialBundle(srcDir, driveIDs)

	for _, driveID := range driveIDs {
		tokenName := TokenFileName(driveID)
		CopyFile(filepath.Join(srcDir, tokenName), filepath.Join(dstDataDir, tokenName), metadataFilePerms)
	}

	CopyCatalogFile(srcDir, dstDataDir)
	CopyFile(filepath.Join(srcDir, testBundleConfigFile), filepath.Join(dstConfigDir, testBundleConfigFile), testConfigFilePerms)
}

// CopyFile copies a file from src to dst with the given permissions.
// Crashes on failure because tests cannot proceed without the file.
func CopyFile(src, dst string, perm os.FileMode) {
	data, err := localpath.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read %s: %v\n", src, err)
		fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
		os.Exit(1)
	}

	if mkdirErr := localpath.MkdirAll(filepath.Dir(dst), metadataDirPerms); mkdirErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: creating destination directory for %s: %v\n", dst, mkdirErr)
		os.Exit(1)
	}

	file, err := localpath.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: opening destination file for %s: %v\n", dst, err)
		os.Exit(1)
	}

	if _, writeErr := file.Write(data); writeErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "FATAL: writing %s: %v (close: %v)\n", dst, writeErr, closeErr)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "FATAL: writing %s: %v\n", dst, writeErr)
		os.Exit(1)
	}

	if closeErr := file.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: closing %s: %v\n", dst, closeErr)
		os.Exit(1)
	}
}
