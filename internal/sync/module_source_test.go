package sync

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// walkModuleGoFiles calls visit with every Go file of the module, test files
// included (its path, and its slash-separated path relative to the module
// root), skipping hidden directories, vendor and testdata, and returns how
// many it visited.
func walkModuleGoFiles(t *testing.T, visit func(path, rel string) error) int {
	t.Helper()

	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
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

// walkModuleProductFiles is walkModuleGoFiles without the test files: it
// calls visit with every non-test Go file of the module and returns how many
// it visited.
func walkModuleProductFiles(t *testing.T, visit func(path, rel string) error) int {
	t.Helper()

	files := 0
	walkModuleGoFiles(t, func(path, rel string) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		return visit(path, rel)
	})

	return files
}
