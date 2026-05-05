package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-4.8.4, R-6.2.4
func TestStatusCommand_NoAccountsDoesNotMutateManagedState(t *testing.T) {
	setTestDriveHome(t)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "missing-config.toml")
	before := snapshotSideEffectRoots(t, append(cliManagedStateRoots(), cfgDir))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)

	require.NoError(t, runStatusCommand(cc, false))
	assert.Contains(t, out.String(), "No accounts configured")
	assert.Equal(t, before, snapshotSideEffectRoots(t, append(cliManagedStateRoots(), cfgDir)))
}

// Validates: R-1.1, R-6.2.4
func TestRunLs_DoesNotMutateManagedState(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cc := newFileCommandTestContext(
		t,
		driveid.MustCanonicalID("personal:user@example.com"),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/drives/0000000drive-123/items/root/children", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{"value":[{"id":"child-1","name":"docs","folder":{"childCount":0}}]}`)
		}),
		&stdout,
		&stderr,
	)
	before := snapshotSideEffectRoots(t, cliManagedStateRoots())

	cmd := newLsCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))

	require.NoError(t, cmd.Execute())
	assert.Contains(t, stdout.String(), "docs/")
	assert.Equal(t, before, snapshotSideEffectRoots(t, cliManagedStateRoots()))
}

// Validates: R-1.3, R-6.2.4
func TestRunRm_RequiresExplicitPathBeforeGraphMutation(t *testing.T) {
	var graphCalls atomic.Int32
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cc := newFileCommandTestContext(
		t,
		driveid.MustCanonicalID("personal:user@example.com"),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			graphCalls.Add(1)
			http.Error(w, "unexpected graph call", http.StatusInternalServerError)
		}),
		&stdout,
		&stderr,
	)
	before := snapshotSideEffectRoots(t, cliManagedStateRoots())

	cmd := newRmCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))

	err := cmd.Execute()
	require.Error(t, err)
	assert.Zero(t, graphCalls.Load(), "rm without a path must fail before any remote mutation")
	assert.Equal(t, before, snapshotSideEffectRoots(t, cliManagedStateRoots()))
}

func cliManagedStateRoots() []string {
	return []string{
		os.Getenv("HOME"),
		os.Getenv("XDG_DATA_HOME"),
		os.Getenv("XDG_CONFIG_HOME"),
		os.Getenv("XDG_CACHE_HOME"),
	}
}

func snapshotSideEffectRoots(t *testing.T, roots []string) map[string]string {
	t.Helper()

	snapshot := make(map[string]string)
	for _, root := range roots {
		if root == "" {
			continue
		}
		info, err := os.Lstat(root)
		if err != nil {
			if os.IsNotExist(err) {
				snapshot[root] = "missing"
				continue
			}
			require.NoError(t, err)
		}
		require.NotNil(t, info)

		rootLabel := filepath.Clean(root)
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return fmt.Errorf("rel snapshot path %s: %w", path, relErr)
			}
			key := rootLabel
			if rel != "." {
				key = filepath.ToSlash(filepath.Join(rootLabel, rel))
			}
			value, valueErr := sideEffectSnapshotValue(path, entry)
			if valueErr != nil {
				return valueErr
			}
			snapshot[key] = value

			return nil
		})
		require.NoError(t, err)
	}

	return snapshot
}

func sideEffectSnapshotValue(path string, entry fs.DirEntry) (string, error) {
	info, err := entry.Info()
	if err != nil {
		return "", fmt.Errorf("stat snapshot path %s: %w", path, err)
	}

	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return "", fmt.Errorf("read snapshot symlink %s: %w", path, readErr)
		}
		return fmt.Sprintf("symlink:%s:%s", mode.String(), target), nil
	case mode.IsDir():
		return fmt.Sprintf("dir:%s", mode.String()), nil
	case mode.IsRegular():
		data, readErr := os.ReadFile(path) //nolint:gosec // test snapshots read only temp roots created by the test.
		if readErr != nil {
			return "", fmt.Errorf("read snapshot file %s: %w", path, readErr)
		}
		sum := sha256.Sum256(data)
		return fmt.Sprintf("file:%s:%d:%x", mode.String(), info.Size(), sum), nil
	default:
		return fmt.Sprintf("other:%s:%d", mode.String(), info.Size()), nil
	}
}
