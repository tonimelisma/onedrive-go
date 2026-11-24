package devtool

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeErrorPrefixSample(t *testing.T, repoRoot, rel, src string) {
	t.Helper()

	require.NoError(t, writeFile(filepath.Join(repoRoot, filepath.FromSlash(rel)), []byte(src)))
}

// Validates: R-6.10.5
//
// After internal/sync split into packages, the new packages kept the old
// package's "sync:" prefix on their errors, so a store failure read as the
// engine's. An error a package makes that leads with another of the
// repository's package names is refused, naming the file and line.
func TestFindErrorPrefixViolationsRefusesAnotherPackagesPrefix(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeErrorPrefixSample(t, repoRoot, "internal/sync/engine.go", "package sync\n")
	writeErrorPrefixSample(t, repoRoot, "internal/graph/client.go", "package graph\n")
	writeErrorPrefixSample(t, repoRoot, "internal/sync/syncstore/store.go", `package syncstore

import (
	"errors"
	"fmt"
)

var errOwn = errors.New("syncstore: own prefix")

func f(err error) error {
	if err != nil {
		return fmt.Errorf("sync: opening the store: %w", err)
	}
	_ = errors.New("graph: not this package")
	_ = fmt.Errorf("opening the store: %w", errOwn)
	_ = fmt.Errorf("unknown: a word that names no package")

	return nil
}
`)
	writeErrorPrefixSample(t, repoRoot, "internal/sync/syncstore/store_test.go", `package syncstore

import "errors"

var errFixture = errors.New("sync: a test may quote any text")
`)

	violations, err := findErrorPrefixViolations(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, []outputBoundaryViolation{
		{Path: "internal/sync/syncstore/store.go", Line: 12, Message: `error message leads with "sync:", another package's name; lead with "syncstore:" or with no package name`},
		{Path: "internal/sync/syncstore/store.go", Line: 14, Message: `error message leads with "graph:", another package's name; lead with "syncstore:" or with no package name`},
	}, violations)
}

// Validates: R-6.10.5
func TestFindErrorPrefixViolations_TheRepositoryHasNone(t *testing.T) {
	t.Parallel()

	violations, err := findErrorPrefixViolations(repoRootForTest(t))
	require.NoError(t, err)
	assert.Empty(t, violations)
}
