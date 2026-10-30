package devtool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindOutputBoundaryViolationsRejectsAmbientOutput(t *testing.T) {
	repoRoot := t.TempDir()
	writeOutputBoundaryFixture(t, repoRoot, "internal/config/bad.go", `package config

import (
	"log"
	"log/slog"
	"os"
)

func bad() {
	slog.Info("bypass")
	log.Print("bypass")
	_, _ = os.Stdout.Write(nil)
	_, _ = os.Stderr.Write(nil)
}
`)

	violations, err := findOutputBoundaryViolations(repoRoot)
	require.NoError(t, err)

	require.Len(t, violations, 4)
	assert.Contains(t, violations[0].Message, "direct slog.Info")
	assert.Contains(t, violations[1].Message, "direct log.Print")
	assert.Contains(t, violations[2].Message, "direct os.Stdout")
	assert.Contains(t, violations[3].Message, "direct os.Stderr")
}

func TestFindOutputBoundaryViolationsAllowsCLIOwnedWritersAndTests(t *testing.T) {
	repoRoot := t.TempDir()
	writeOutputBoundaryFixture(t, repoRoot, "internal/cli/root.go", `package cli

import "os"

func outputWriter() any { return os.Stdout }
`)
	writeOutputBoundaryFixture(t, repoRoot, "internal/config/write_test.go", `package config

import (
	"log/slog"
	"os"
)

func testOnly() {
	slog.Info("ok in tests")
	_, _ = os.Stderr.Write(nil)
}
`)
	writeOutputBoundaryFixture(t, repoRoot, "testutil/testenv.go", `package testutil

import "os"

func fatalTestEnv() {
	_, _ = os.Stderr.Write(nil)
}
`)

	violations, err := findOutputBoundaryViolations(repoRoot)
	require.NoError(t, err)

	assert.Empty(t, violations)
}

func writeOutputBoundaryFixture(t *testing.T, repoRoot string, rel string, content string) {
	t.Helper()

	path := filepath.Join(repoRoot, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// Regression: flattened with err.Error(), an "error" attribute was shown on
// the default console as its whole wrap chain (the remote observer's outage
// warning printed the delta URL with its cursor). Logged as an error value it
// renders as a plain reason.
func TestFindOutputBoundaryViolationsRejectsFlattenedErrorAttrs(t *testing.T) {
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
