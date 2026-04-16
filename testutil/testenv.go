// Package testutil provides shared test-environment helpers for E2E and
// integration tests.
package testutil

import (
	"bufio"
	"encoding/json"
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

type credentialCatalog struct {
	Accounts map[string]credentialCatalogAccount `json:"accounts"`
	Drives   map[string]credentialCatalogDrive   `json:"drives"`
}

type credentialCatalogAccount struct {
	CanonicalID    string `json:"canonical_id"`
	DisplayName    string `json:"display_name"`
	OrgName        string `json:"org_name,omitempty"`
	PrimaryDriveID string `json:"primary_drive_id"`
}

type credentialCatalogDrive struct {
	CanonicalID             string `json:"canonical_id"`
	OwnerAccountCanonicalID string `json:"owner_account_canonical_id"`
	RemoteDriveID           string `json:"remote_drive_id"`
	OwnerName               string `json:"owner_name,omitempty"`
	OwnerEmail              string `json:"owner_email,omitempty"`
}

// LiveFixtures captures durable live Graph fixtures loaded from the test
// environment. The values stay as strings so testutil's exported API does not
// leak internal package types.
type LiveFixtures struct {
	SharedFileLink               string
	SharedFolderLink             string
	WritableSharedFolderSelector string
	ReadOnlySharedFolderSelector string
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
		},
	}

	if cfg.PrimaryDrive == "" {
		return LiveTestConfig{}, fmt.Errorf("load live test config: ONEDRIVE_TEST_DRIVE not set")
	}

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

// CandidateDriveIDs returns the configured live drive IDs without duplicates.
func (cfg LiveTestConfig) CandidateDriveIDs() []string {
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
func (cfg LiveTestConfig) DriveIDForEmail(email string) (string, bool) {
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

// FindTestCredentialDir locates .testdata/ relative to the module root.
// Crashes if the directory does not exist.
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

// metadataFilePerms is owner-only read/write for metadata files.
const metadataFilePerms = 0o600

// CopyMetadataFiles copies all account_*.json and drive_*.json files from
// srcDir to dstDir. When the durable test credential layout only provides
// catalog.json, the helper materializes the per-file account and drive
// metadata that the isolated CLI runtime expects.
func CopyMetadataFiles(srcDir, dstDir string) {
	copiedMetadata := false
	for _, prefix := range []string{"account_", "drive_"} {
		matches, err := filepath.Glob(filepath.Join(srcDir, prefix+"*.json"))
		if err != nil {
			continue
		}

		for _, m := range matches {
			CopyFile(m, filepath.Join(dstDir, filepath.Base(m)), metadataFilePerms)
			copiedMetadata = true
		}
	}

	if copiedMetadata {
		return
	}

	catalogPath := filepath.Join(srcDir, "catalog.json")
	if _, err := localpath.Stat(catalogPath); err != nil {
		return
	}

	materializeCatalogMetadata(catalogPath, dstDir)
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

func materializeCatalogMetadata(catalogPath, dstDir string) {
	data, err := localpath.ReadFile(catalogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read %s: %v\n", catalogPath, err)
		os.Exit(1)
	}

	var catalog credentialCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot decode %s: %v\n", catalogPath, err)
		os.Exit(1)
	}

	for _, account := range catalog.Accounts {
		accountPath := filepath.Join(dstDir, accountFileName(account.CanonicalID))
		accountBody, err := json.MarshalIndent(map[string]any{
			"profile": map[string]string{
				"user_id":          account.PrimaryDriveID,
				"display_name":     account.DisplayName,
				"org_name":         account.OrgName,
				"primary_drive_id": account.PrimaryDriveID,
			},
		}, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: cannot encode account metadata for %s: %v\n", account.CanonicalID, err)
			os.Exit(1)
		}
		writeMetadataFile(accountPath, accountBody)
	}

	for _, drive := range catalog.Drives {
		owner := catalog.Accounts[drive.OwnerAccountCanonicalID]
		ownerName := drive.OwnerName
		if ownerName == "" {
			ownerName = owner.DisplayName
		}
		ownerEmail := drive.OwnerEmail
		if ownerEmail == "" {
			ownerEmail = canonicalIDEmail(drive.OwnerAccountCanonicalID)
		}

		drivePath := filepath.Join(dstDir, driveFileName(drive.CanonicalID))
		driveBody, err := json.MarshalIndent(map[string]string{
			"drive_id":             drive.RemoteDriveID,
			"account_canonical_id": drive.OwnerAccountCanonicalID,
			"owner_name":           ownerName,
			"owner_email":          ownerEmail,
		}, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "FATAL: cannot encode drive metadata for %s: %v\n", drive.CanonicalID, err)
			os.Exit(1)
		}
		writeMetadataFile(drivePath, driveBody)
	}
}

func writeMetadataFile(path string, data []byte) {
	if err := localpath.AtomicWrite(path, append(data, '\n'), metadataFilePerms, metadataDirPerms, ".testdata-*.tmp"); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot write %s: %v\n", path, err)
		os.Exit(1)
	}
}

func accountFileName(canonicalID string) string {
	parts := strings.SplitN(canonicalID, ":", 2)
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "FATAL: cannot derive account file from canonical ID %q\n", canonicalID)
		os.Exit(1)
	}

	return "account_" + parts[0] + "_" + parts[1] + ".json"
}

func driveFileName(canonicalID string) string {
	return "drive_" + strings.ReplaceAll(canonicalID, ":", "_") + ".json"
}

func canonicalIDEmail(canonicalID string) string {
	parts := strings.SplitN(canonicalID, ":", 2)
	if len(parts) != 2 {
		return ""
	}

	return parts[1]
}
