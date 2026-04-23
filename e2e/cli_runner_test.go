//go:build e2e

package e2e

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolveDriveSelection(env map[string]string, requestedDriveID string) string {
	switch requestedDriveID {
	case "", drive:
		if env != nil {
			if selected := env["ONEDRIVE_GO_DRIVE"]; selected != "" {
				return selected
			}
		}

		return drive
	default:
		return requestedDriveID
	}
}

func execCLI(
	cfgPath string,
	env map[string]string,
	driveID string,
	args ...string,
) ([]string, string, string, error) {
	var fullArgs []string
	if cfgPath != "" {
		fullArgs = append(fullArgs, "--config", cfgPath)
	}

	if driveID != "" {
		fullArgs = append(fullArgs, "--drive", driveID)
	}

	if shouldAddDebug(args) {
		fullArgs = append(fullArgs, "--debug")
	}

	fullArgs = append(fullArgs, args...)
	cmd := makeCmd(fullArgs, env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return fullArgs, stdout.String(), stderr.String(), err
}

// runCLICore is the shared implementation for all testing-aware CLI runner
// helpers. It delegates process execution to execCLI, then records quirk and
// debug-log output for the specific test invocation. driveID="" omits --drive
// so callers can intentionally exercise all-drives mode.
func runCLICore(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	args ...string,
) (string, string, error) {
	t.Helper()

	fullArgs, stdout, stderr, err := execCLI(cfgPath, env, driveID, args...)
	recordCLIQuirkEvents(t, fullArgs, stderr, err)
	logCLIExecution(t, fullArgs, stdout, stderr)

	return stdout, stderr, err
}

// runCLIWithConfig runs the CLI binary with a custom config file.
// env overrides (if non-nil) are applied to the child process environment.
func runCLIWithConfig(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, resolveDriveSelection(env, ""), args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s",
		args, stdout, stderr)

	return stdout, stderr
}

// runCLIWithConfigAllowError runs the CLI binary with a custom config file
// and returns the output even on error.
func runCLIWithConfigAllowError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string, error) {
	t.Helper()

	return runCLICore(t, cfgPath, env, resolveDriveSelection(env, ""), args...)
}

func runCLIWithConfigAllowErrorForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	args ...string,
) (string, string, error) {
	t.Helper()

	return runCLICore(t, cfgPath, env, resolveDriveSelection(env, driveID), args...)
}

func runCLIWithoutDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s", args, stdout, stderr)

	return stdout, stderr
}

// runCLIWithConfigAllDrives runs the CLI without --drive flag.
func runCLIWithConfigAllDrives(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s",
		args, stdout, stderr)

	return stdout, stderr
}

// runCLIWithConfigAllDrivesAllowError runs the CLI without --drive and returns
// the output even on error.
func runCLIWithConfigAllDrivesAllowError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (string, string, error) {
	t.Helper()

	return runCLICore(t, cfgPath, env, "", args...)
}

// runCLIWithConfigForDrive runs the CLI with a specific --drive flag.
func runCLIWithConfigForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	driveID string,
	args ...string,
) (string, string) {
	t.Helper()

	resolvedDriveID := resolveDriveSelection(env, driveID)
	stdout, stderr, err := runCLICore(t, cfgPath, env, resolvedDriveID, args...)
	require.NoErrorf(t, err, "CLI command %v (drive=%s) failed\nstdout: %s\nstderr: %s",
		args, resolvedDriveID, stdout, stderr)

	return stdout, stderr
}

// runCLIWithConfigExpectError runs the CLI with a config file and expects
// failure on the resolved default drive.
func runCLIWithConfigExpectError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) string {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, resolveDriveSelection(env, ""), args...)

	require.Error(t, err, "expected CLI to fail for args %v, but it succeeded\nstdout: %s\nstderr: %s",
		args, stdout, stderr)

	return stdout + stderr
}

func TestResolveDriveSelection(t *testing.T) {
	t.Parallel()

	t.Run("defaults to suite drive", func(t *testing.T) {
		assert.Equal(t, drive, resolveDriveSelection(nil, ""))
	})

	t.Run("uses env selected drive for empty request", func(t *testing.T) {
		assert.Equal(t, "shared:user@example.com:drive:item", resolveDriveSelection(
			map[string]string{"ONEDRIVE_GO_DRIVE": "shared:user@example.com:drive:item"},
			"",
		))
	})

	t.Run("uses env selected drive for suite drive request", func(t *testing.T) {
		assert.Equal(t, "business:alt@example.com", resolveDriveSelection(
			map[string]string{"ONEDRIVE_GO_DRIVE": "business:alt@example.com"},
			drive,
		))
	})

	t.Run("preserves explicit non-default drive", func(t *testing.T) {
		assert.Equal(t, "business:other@example.com", resolveDriveSelection(
			map[string]string{"ONEDRIVE_GO_DRIVE": "shared:user@example.com:drive:item"},
			"business:other@example.com",
		))
	})
}
