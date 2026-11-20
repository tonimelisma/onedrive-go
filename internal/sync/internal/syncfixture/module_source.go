package syncfixture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// WalkModuleGoFiles calls visit with every Go file of the module, test files
// included (its path, and its slash-separated path relative to the module
// root), skipping hidden directories, vendor and testdata, and returns how
// many it visited.
func WalkModuleGoFiles(t *testing.T, visit func(path, rel string) error) int {
	t.Helper()

	moduleRoot := ModuleRoot(t)
	files := 0
	walkErr := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != moduleRoot && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(moduleRoot, path)
		if relErr != nil {
			return fmt.Errorf("relative path of %s: %w", path, relErr)
		}
		files++
		return visit(path, filepath.ToSlash(rel))
	})
	require.NoError(t, walkErr)

	return files
}

// WalkModuleProductFiles is WalkModuleGoFiles without the test code: it
// calls visit with every non-test Go file of the module outside the sync
// tree's two test-helper packages, and returns how many it visited.
func WalkModuleProductFiles(t *testing.T, visit func(path, rel string) error) int {
	t.Helper()

	files := 0
	WalkModuleGoFiles(t, func(path, rel string) error {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(rel, "internal/sync/internal/syncfixture/") ||
			strings.HasPrefix(rel, "internal/sync/internal/syncstoretest/") {
			return nil
		}
		files++
		return visit(path, rel)
	})

	return files
}

// ModuleRoot is the module's root directory: the nearest directory above the
// test's working directory, its package's directory, that holds go.mod.
func ModuleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test's directory")
		dir = parent
	}
}
