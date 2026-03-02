// Package testutil provides shared test environment helpers for E2E and
// integration tests. It depends only on stdlib so that E2E tests (which
// cannot import internal/) can use it.
package testutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv reads KEY=VALUE pairs from a .env file at the given path.
// Missing file is not an error (CI sets env vars directly).
// Existing env vars take precedence over .env values.
func LoadDotEnv(envPath string) {
	f, err := os.Open(envPath)
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
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
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

	if _, err := os.Stat(dir); err != nil {
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

// CopyFile copies a file from src to dst with the given permissions.
// Crashes on failure because tests cannot proceed without the file.
func CopyFile(src, dst string, perm os.FileMode) {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read %s: %v\n", src, err)
		fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
		os.Exit(1)
	}

	if writeErr := os.WriteFile(dst, data, perm); writeErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: writing %s: %v\n", dst, writeErr)
		os.Exit(1)
	}
}
