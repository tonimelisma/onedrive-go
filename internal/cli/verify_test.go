package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func TestPrintVerifyTable_NoMismatches(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printVerifyTable(&buf, &synctypes.VerifyReport{Verified: 10}))
	out := buf.String()

	assert.Contains(t, out, "Verified: 10")
	assert.Contains(t, out, "All files verified successfully")
}

func TestPrintVerifyTable_WithMismatches(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printVerifyTable(&buf, &synctypes.VerifyReport{
		Verified: 8,
		Mismatches: []synctypes.VerifyResult{
			{Path: "/foo.txt", Status: "hash_mismatch", Expected: "aaa", Actual: "bbb"},
		},
	}))
	out := buf.String()

	assert.Contains(t, out, "Mismatches: 1")
	assert.Contains(t, out, "/foo.txt")
}

// Validates: R-2.7.1
func TestPrintVerifyJSON(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printVerifyJSON(&buf, &synctypes.VerifyReport{Verified: 5}))
	out := buf.String()

	assert.Contains(t, out, `"verified"`)

	var parsed synctypes.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))
	assert.Equal(t, 5, parsed.Verified)
}

// Validates: R-2.7
func TestNewVerifyCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newVerifyCmd()
	assert.Equal(t, "verify", cmd.Use)
}

func newVerifyContext(output io.Writer, jsonOutput bool, syncDir string, cid driveid.CanonicalID) context.Context {
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: jsonOutput},
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: output,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			SyncDir:     syncDir,
		},
	}

	return context.WithValue(context.Background(), cliContextKey{}, cc)
}

// Validates: R-2.7
func TestLoadAndVerify_EmptyBaseline(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	dbPath := filepath.Join(t.TempDir(), "state.db")
	syncDir := t.TempDir()

	report, err := loadAndVerify(t.Context(), dbPath, syncDir, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NotNil(t, report)
	assert.Zero(t, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestLoadAndVerify_OpenSyncStoreError(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(parentFile, []byte("x"), 0o600))

	report, err := loadAndVerify(t.Context(), filepath.Join(parentFile, "state.db"), t.TempDir(), slog.New(slog.DiscardHandler))
	require.Error(t, err)
	assert.Nil(t, report)
	assert.Contains(t, err.Error(), "open sync store")
}

// Validates: R-2.7
func TestRunVerify_PropagatesSyncStoreOpenError(t *testing.T) {
	xdgFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(xdgFile, []byte("x"), 0o600))
	t.Setenv("XDG_DATA_HOME", xdgFile)

	cid := driveid.MustCanonicalID("personal:test@example.com")
	cmd := newVerifyCmd()
	cmd.SetContext(newVerifyContext(io.Discard, false, t.TempDir(), cid))

	err := runVerify(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open sync store")
}

// Validates: R-2.7
func TestRunVerify_RequiresSyncDir(t *testing.T) {
	cmd := newVerifyCmd()
	cmd.SetContext(newVerifyContext(io.Discard, false, "", driveid.MustCanonicalID("personal:test@example.com")))

	err := runVerify(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir not configured")
}

// Validates: R-2.7
func TestRunVerify_RequiresStatePath(t *testing.T) {
	cmd := newVerifyCmd()
	cmd.SetContext(newVerifyContext(io.Discard, false, t.TempDir(), driveid.CanonicalID{}))

	err := runVerify(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot determine state DB path")
}

// Validates: R-2.7
func TestRunVerify_Success(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:test@example.com")
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))

	cmd := newVerifyCmd()
	var out bytes.Buffer
	cmd.SetContext(newVerifyContext(&out, false, t.TempDir(), cid))

	require.NoError(t, runVerify(cmd, nil))

	assert.Contains(t, out.String(), "All files verified successfully.")
}

// Validates: R-2.7.1
func TestRunVerify_SuccessJSON(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:test@example.com")
	require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))

	cmd := newVerifyCmd()
	var out bytes.Buffer
	cmd.SetContext(newVerifyContext(&out, true, t.TempDir(), cid))

	require.NoError(t, runVerify(cmd, nil))

	var report synctypes.VerifyReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Zero(t, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestRunVerify_ReturnsMismatchSentinel(t *testing.T) {
	xdgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgDir)

	cid := driveid.MustCanonicalID("personal:test@example.com")
	dbPath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o700))

	syncDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(syncDir, "docs"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, "docs", "readme.txt"), []byte("hello"), 0o600))

	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	_, err = mgr.DB().ExecContext(t.Context(),
		`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
		 local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"docs/readme.txt", "drv1", "item1", "parent1", "file",
		"wrong-hash", "wrong-hash", 5, 5, 1700000000, 1700000000, 1700000000, "etag1")
	require.NoError(t, err)
	require.NoError(t, mgr.Close(t.Context()))

	cmd := newVerifyCmd()
	var out bytes.Buffer
	cmd.SetContext(newVerifyContext(&out, false, syncDir, cid))

	err = runVerify(cmd, nil)

	require.ErrorIs(t, err, errVerifyMismatch)
	assert.Contains(t, out.String(), "Mismatches: 1")
}
