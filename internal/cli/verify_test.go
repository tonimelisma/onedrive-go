package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type stubVerifyStore struct {
	baseline *synctypes.Baseline
	loadErr  error
	closeErr error
}

func (s stubVerifyStore) Load(context.Context) (*synctypes.Baseline, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}

	return s.baseline, nil
}

func (s stubVerifyStore) Close(context.Context) error {
	return s.closeErr
}

type verifyBaselineRow struct {
	path      string
	itemType  string
	localHash string
	localSize int64
}

func setupVerifyFixture(t *testing.T, syncDir string) (driveid.CanonicalID, string) {
	t.Helper()

	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:test@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))

	dbPath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o700))

	return cid, dbPath
}

func insertVerifyBaselineRows(t *testing.T, dbPath string, rows ...verifyBaselineRow) {
	t.Helper()

	mgr, err := syncstore.NewSyncStore(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, mgr.Close(t.Context()))
	}()

	for i := range rows {
		row := rows[i]
		itemType := row.itemType
		if itemType == "" {
			itemType = "file"
		}

		_, err = mgr.DB().ExecContext(t.Context(),
			`INSERT INTO baseline (path, drive_id, item_id, parent_id, item_type,
			 local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.path,
			"drv1",
			fmt.Sprintf("item-%d", i+1),
			"parent1",
			itemType,
			row.localHash,
			row.localHash,
			row.localSize,
			row.localSize,
			1700000000,
			1700000000,
			1700000000,
			fmt.Sprintf("etag-%d", i+1),
		)
		require.NoError(t, err)
	}
}

// Validates: R-2.7
func TestPrintVerifyTable_NoMismatches(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printVerifyTable(&buf, &synctypes.VerifyReport{Verified: 10}))
	requireGoldenText(t, "verify_no_mismatches.golden", buf.String())
}

// Validates: R-2.7
func TestPrintVerifyTable_WithMismatches(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, printVerifyTable(&buf, &synctypes.VerifyReport{
		Verified: 8,
		Mismatches: []synctypes.VerifyResult{
			{Path: "alpha.txt", Status: "hash_mismatch", Expected: "aaa", Actual: "bbb"},
			{Path: "docs/missing.txt", Status: "missing", Expected: "ccc", Actual: ""},
		},
	}))
	requireGoldenText(t, "verify_with_mismatches.golden", buf.String())
}

// Validates: R-2.7.1
func TestPrintVerifyJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	expected := &synctypes.VerifyReport{
		Verified: 5,
		Mismatches: []synctypes.VerifyResult{
			{Path: "alpha.txt", Status: "hash_mismatch", Expected: "aaa", Actual: "bbb"},
			{Path: "docs/missing.txt", Status: "missing", Expected: "ccc", Actual: ""},
		},
	}
	require.NoError(t, printVerifyJSON(&buf, expected))

	var parsed synctypes.VerifyReport
	require.NoError(t, json.Unmarshal(buf.Bytes(), &parsed))
	assert.Equal(t, *expected, parsed)
}

func newVerifyCLIContext(output io.Writer, jsonOutput bool, syncDir string, cid driveid.CanonicalID) *CLIContext {
	return &CLIContext{
		Flags:        CLIFlags{JSON: jsonOutput},
		Logger:       slog.New(slog.DiscardHandler),
		OutputWriter: output,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			SyncDir:     syncDir,
		},
	}
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
func TestLoadAndVerifyWithStore_CloseFailureSuppressesReport(t *testing.T) {
	syncDir := t.TempDir()
	closeErr := errors.New("db close failed")

	report, err := loadAndVerifyWithStore(
		t.Context(),
		stubVerifyStore{
			baseline: &synctypes.Baseline{},
			closeErr: closeErr,
		},
		syncDir,
		slog.New(slog.DiscardHandler),
	)
	require.Error(t, err)
	assert.Nil(t, report)
	require.ErrorIs(t, err, closeErr)
	assert.Contains(t, err.Error(), "close sync store")
}

// Validates: R-2.7
func TestRunVerify_PropagatesSyncStoreOpenError(t *testing.T) {
	xdgFile := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(xdgFile, []byte("x"), 0o600))
	t.Setenv("XDG_DATA_HOME", xdgFile)

	cid := driveid.MustCanonicalID("personal:test@example.com")
	svc := newVerifyService(newVerifyCLIContext(io.Discard, false, t.TempDir(), cid))

	err := svc.run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open sync store")
}

// Validates: R-2.7
func TestRunVerify_RequiresSyncDir(t *testing.T) {
	svc := newVerifyService(newVerifyCLIContext(io.Discard, false, "", driveid.MustCanonicalID("personal:test@example.com")))

	err := svc.run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir not configured")
}

// Validates: R-2.7
func TestRunVerify_RequiresStatePath(t *testing.T) {
	svc := newVerifyService(newVerifyCLIContext(io.Discard, false, t.TempDir(), driveid.CanonicalID{}))

	err := svc.run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot determine state DB path")
}

// Validates: R-2.7
func TestRunVerify_Success(t *testing.T) {
	syncDir := t.TempDir()
	cid, _ := setupVerifyFixture(t, syncDir)

	var out bytes.Buffer
	svc := newVerifyService(newVerifyCLIContext(&out, false, syncDir, cid))

	require.NoError(t, svc.run(context.Background()))

	assert.Contains(t, out.String(), "All files verified successfully.")
}

// Validates: R-2.7.1
func TestRunVerify_SuccessJSON(t *testing.T) {
	syncDir := t.TempDir()
	cid, _ := setupVerifyFixture(t, syncDir)

	var out bytes.Buffer
	svc := newVerifyService(newVerifyCLIContext(&out, true, syncDir, cid))

	require.NoError(t, svc.run(context.Background()))

	var report synctypes.VerifyReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Zero(t, report.Verified)
	assert.Empty(t, report.Mismatches)
}

// Validates: R-2.7
func TestRunVerify_ReturnsMismatchSentinel(t *testing.T) {
	syncDir := t.TempDir()
	cid, dbPath := setupVerifyFixture(t, syncDir)
	require.NoError(t, os.MkdirAll(filepath.Join(syncDir, "docs"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(syncDir, "docs", "readme.txt"), []byte("hello"), 0o600))
	insertVerifyBaselineRows(t, dbPath, verifyBaselineRow{
		path:      "docs/readme.txt",
		localHash: "wrong-hash",
		localSize: 5,
	})

	var out bytes.Buffer
	svc := newVerifyService(newVerifyCLIContext(&out, false, syncDir, cid))

	err := svc.run(context.Background())

	require.ErrorIs(t, err, errVerifyMismatch)
	assert.Contains(t, out.String(), "Mismatches: 1")
	assert.Contains(t, out.String(), "docs/readme.txt")
	assert.Contains(t, out.String(), "hash_mismatch")
}
