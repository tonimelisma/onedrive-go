// Package liveenv owns the two rules every live run shares, the live suites'
// and the live benchmark's: how the live-test environment files are read
// into the process environment, and which drives a live run may touch. It
// imports the standard library alone, so the verification tool can share
// the rules without linking a product package; a caller hands it the
// capability that opens a file.
package liveenv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AllowedTestAccountsEnvVar names the comma-separated allowlist of the
// drives live tests may touch.
const AllowedTestAccountsEnvVar = "ONEDRIVE_ALLOWED_TEST_ACCOUNTS"

// dotEnvFile and sharedFixtureEnvPath are the live-test environment files,
// relative to the module root, in precedence order.
const (
	dotEnvFile           = ".env"
	sharedFixtureEnvPath = ".testdata/fixtures.env"
)

// OpenFile opens a file for reading: the caller's filesystem boundary. A
// file that does not exist is reported with an error that wraps
// fs.ErrNotExist.
type OpenFile func(path string) (io.ReadCloser, error)

// LoadDotEnvFile reads KEY=VALUE pairs from the .env file at path, opened
// through open, into the process environment. A missing file is not an
// error (CI sets the variables directly), and a variable already set wins
// over the file.
func LoadDotEnvFile(open OpenFile, path string) (err error) {
	f, err := open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, closeErr)
		}
	}()

	if applyErr := ApplyDotEnv(f); applyErr != nil {
		return fmt.Errorf("read %s: %w", path, applyErr)
	}

	return nil
}

// LoadEnvFiles loads the live-test environment files of the module at
// moduleRoot in precedence order: exported process env wins over both
// files, and the root .env overrides the durable fixture defaults of
// .testdata/fixtures.env.
func LoadEnvFiles(open OpenFile, moduleRoot string) error {
	for _, path := range []string{filepath.Join(moduleRoot, dotEnvFile), filepath.Join(moduleRoot, sharedFixtureEnvPath)} {
		if err := LoadDotEnvFile(open, path); err != nil {
			return err
		}
	}

	return nil
}

// ApplyDotEnv sets the KEY=VALUE pairs of a .env stream that the
// environment does not set yet. Its errors name a key, never a value.
func ApplyDotEnv(r io.Reader) error {
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
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	return nil
}

// AllowlistIncludes reports whether driveID is an entry of a comma-separated
// allowlist. Entries are trimmed and must match the whole drive.
func AllowlistIncludes(allowlist string, driveID string) bool {
	if driveID == "" {
		return false
	}
	for _, entry := range strings.Split(allowlist, ",") {
		if strings.TrimSpace(entry) == driveID {
			return true
		}
	}

	return false
}

// CheckDriveAllowed returns an error unless driveID, the value of the
// variable driveVar, is set and in the allowlist. The error names the
// variables and never a value: live suites run in public CI logs.
func CheckDriveAllowed(driveVar string, driveID string) error {
	allowlist := os.Getenv(AllowedTestAccountsEnvVar)
	switch {
	case strings.TrimSpace(allowlist) == "":
		return fmt.Errorf("%s is not set; set it in .env or the environment, e.g. %s=personal:user@outlook.com",
			AllowedTestAccountsEnvVar, AllowedTestAccountsEnvVar)
	case driveID == "":
		return fmt.Errorf("%s is not set", driveVar)
	case !AllowlistIncludes(allowlist, driveID):
		return fmt.Errorf("the drive in %s is not an entry of %s", driveVar, AllowedTestAccountsEnvVar)
	default:
		return nil
	}
}
