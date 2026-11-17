package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outputBoundaryAllowedSamples is a file of every kind outputBoundaryFileAllowed
// allows: the CLI's two writer files, a test, the verification tool, the live
// suite and testutil.
func outputBoundaryAllowedSamples() []string {
	return []string{
		"internal/cli/root.go", "internal/cli/format.go", "internal/config/write_test.go",
		"internal/devtool/verify.go", "cmd/devtool/main.go", "e2e/helpers.go", "testutil/testenv.go",
	}
}

// outputBoundaryBoundSamples are product files outputBoundaryFileAllowed does
// not allow: the filesystem packages, and paths that hold an allowed folder's
// name below the repository's top.
func outputBoundaryBoundSamples() []string {
	return []string{
		"main.go", "internal/config/write.go", "internal/cli/status.go", "internal/synctest/logcapture.go",
		"internal/fsroot/root.go", "internal/synctree/root.go", "internal/localpath/atomic.go",
		"pkg/quickxorhash/quickxorhash.go", "internal/graph/e2e/fixture.go", "internal/cli/testutil/fake.go",
	}
}

// Validates: R-6.10.5
//
// The CLI's writer files, tests, the verification tool, the live suite and
// testutil are not held to the product's logging rule; every other file is.
func TestFindOutputBoundaryViolationsAllowsCLIOwnedWritersAndTests(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	for _, rel := range append(outputBoundaryAllowedSamples(), outputBoundaryBoundSamples()...) {
		writeOutputBoundaryFixture(t, repoRoot, rel, `package sample

import "log/slog"

func attrs(logger *slog.Logger, err error) {
	logger.Warn("flattened", slog.String("error", err.Error()))
}
`)
	}

	violations, err := findOutputBoundaryViolations(repoRoot)
	require.NoError(t, err)

	reported := make([]string, 0, len(violations))
	for _, violation := range violations {
		reported = append(reported, violation.Path)
	}
	assert.ElementsMatch(t, outputBoundaryBoundSamples(), reported)
}

func writeOutputBoundaryFixture(t *testing.T, repoRoot string, rel string, content string) {
	t.Helper()

	path := filepath.Join(repoRoot, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// Validates: R-6.10.5
//
// Regression: flattened with err.Error(), an "error" attribute was shown on
// the default console as its whole wrap chain (the remote observer's outage
// warning printed the delta URL with its cursor). Logged as an error value it
// renders as a plain reason.
func TestFindOutputBoundaryViolationsRejectsFlattenedErrorAttrs(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeOutputBoundaryFixture(t, repoRoot, "internal/sync/bad.go", `package sync

import (
	"errors"
	"log/slog"
)

func attrs(logger *slog.Logger, err error, storedMessage string) {
	logger.Warn("flattened", slog.String("error", err.Error()))
	logger.Warn("value", slog.Any("error", err))
	logger.Warn("other key", slog.String("reason", err.Error()))
	logger.Warn("stored message", slog.String("error", storedMessage))
	_ = errors.New("x")
}
`)

	violations, err := findOutputBoundaryViolations(repoRoot)
	require.NoError(t, err)

	require.Len(t, violations, 1)
	assert.Equal(t, "internal/sync/bad.go", violations[0].Path)
	assert.Equal(t, 9, violations[0].Line)
	assert.Contains(t, violations[0].Message, `slog.Any("error", err)`)
}
