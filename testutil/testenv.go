// Package testutil provides shared test environment helpers for E2E and
// integration tests. It depends only on stdlib so that E2E tests (which
// cannot import internal/) can use it.
package testutil

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/trustedpath"
)

// LoadDotEnv reads KEY=VALUE pairs from a .env file at the given path.
// Missing file is not an error (CI sets env vars directly).
// Existing env vars take precedence over .env values.
func LoadDotEnv(envPath string) {
	f, err := trustedpath.Open(envPath)
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

// metadataFilePerms is owner-only read/write for metadata files.
const metadataFilePerms = 0o600

// CopyMetadataFiles copies all account_*.json and drive_*.json files from
// srcDir to dstDir. Missing files are silently skipped; copy failures are fatal.
func CopyMetadataFiles(srcDir, dstDir string) {
	for _, prefix := range []string{"account_", "drive_"} {
		matches, err := filepath.Glob(filepath.Join(srcDir, prefix+"*.json"))
		if err != nil {
			continue
		}

		for _, m := range matches {
			CopyFile(m, filepath.Join(dstDir, filepath.Base(m)), metadataFilePerms)
		}
	}
}

// CopyFile copies a file from src to dst with the given permissions.
// Crashes on failure because tests cannot proceed without the file.
func CopyFile(src, dst string, perm os.FileMode) {
	data, err := trustedpath.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot read %s: %v\n", src, err)
		fmt.Fprintln(os.Stderr, "Run scripts/bootstrap-test-credentials.sh to create test credentials.")
		os.Exit(1)
	}

	root, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: opening destination directory for %s: %v\n", dst, err)
		os.Exit(1)
	}

	file, err := root.OpenFile(filepath.Base(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		if closeErr := root.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "FATAL: opening destination file for %s: %v (close: %v)\n", dst, err, closeErr)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "FATAL: opening destination file for %s: %v\n", dst, err)
		os.Exit(1)
	}

	if _, writeErr := file.Write(data); writeErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "FATAL: writing %s: %v (close: %v)\n", dst, writeErr, closeErr)
			os.Exit(1)
		}

		if closeErr := root.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "FATAL: writing %s: %v (root close: %v)\n", dst, writeErr, closeErr)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "FATAL: writing %s: %v\n", dst, writeErr)
		os.Exit(1)
	}

	if closeErr := file.Close(); closeErr != nil {
		if rootCloseErr := root.Close(); rootCloseErr != nil {
			fmt.Fprintf(os.Stderr, "FATAL: closing %s: %v (root close: %v)\n", dst, closeErr, rootCloseErr)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "FATAL: closing %s: %v\n", dst, closeErr)
		os.Exit(1)
	}

	if closeErr := root.Close(); closeErr != nil {
		fmt.Fprintf(os.Stderr, "FATAL: closing destination directory for %s: %v\n", dst, closeErr)
		os.Exit(1)
	}
}
