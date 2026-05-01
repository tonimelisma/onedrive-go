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
