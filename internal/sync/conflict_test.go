package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- NewConflictHandler tests ---

func TestNewConflictHandler_NilLogger(t *testing.T) {
	h := NewConflictHandler("/tmp/sync", nil)
	require.NotNil(t, h)
	require.NotNil(t, h.logger, "nil logger should be replaced with slog.Default()")
}

// --- generateConflictPath tests ---

func TestGenerateConflictPath_RegularFile(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "report.docx")

	result := generateConflictPath(original)

	// Must not equal original.
	assert.NotEqual(t, original, result)
	// Stem part preserved.
	assert.Contains(t, result, filepath.Join(dir, "report.conflict-"))
	// Extension preserved after timestamp.
	assert.True(t, strings.HasSuffix(result, ".docx"), "expected .docx suffix, got %q", result)
	// Timestamp pattern: YYYYMMDD-HHMMSS.
	base := filepath.Base(result)
	assert.Regexp(t, `^report\.conflict-\d{8}-\d{6}\.docx$`, base)
}

func TestGenerateConflictPath_Dotfile(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, ".bashrc")

	result := generateConflictPath(original)

	// Must not equal original.
	assert.NotEqual(t, original, result)
	// Dotfile with no extension: pattern is <dotfile>.conflict-<timestamp> (no trailing dot).
	base := filepath.Base(result)
	assert.Regexp(t, `^\.bashrc\.conflict-\d{8}-\d{6}$`, base)
}

func TestGenerateConflictPath_NoExtension(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "Makefile")

	result := generateConflictPath(original)

	base := filepath.Base(result)
	assert.Regexp(t, `^Makefile\.conflict-\d{8}-\d{6}$`, base)
}

func TestGenerateConflictPath_CollisionAvoidance(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "notes.txt")

	// Generate a path and create a file there to force a collision.
	first := generateConflictPath(original)
	require.NoError(t, os.WriteFile(first, []byte("taken"), 0o644))

	// Second call must return a different path (numeric suffix).
	second := generateConflictPath(original)
	assert.NotEqual(t, first, second)
	assert.True(t, strings.HasSuffix(second, ".txt"), "expected .txt suffix on collision path")
}

func TestGenerateConflictPath_NoCollision_ReturnsBase(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "data.csv")

	// No existing file → first call returns base (no suffix).
	result := generateConflictPath(original)
	base := filepath.Base(result)
	// No numeric suffix in base path.
	assert.Regexp(t, `^data\.conflict-\d{8}-\d{6}\.csv$`, base)
}

// --- ConflictHandler.Resolve tests ---

func newTestConflictHandler(t *testing.T, syncRoot string) *ConflictHandler {
	t.Helper()
	return NewConflictHandler(syncRoot, testLogger(t))
}

func TestConflictHandler_Resolve_EditEdit(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	// Create local file to be renamed.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "file.txt"), []byte("local"), 0o644))

	action := &Action{
		DriveID: "d1",
		ItemID:  "i1",
		Path:    "file.txt",
		Item:    &Item{DriveID: "d1", ItemID: "i1", Path: "file.txt"},
		ConflictInfo: &ConflictRecord{
			DriveID:    "d1",
			ItemID:     "i1",
			Path:       "file.txt",
			LocalHash:  "AAA",
			RemoteHash: "BBB",
			Type:       ConflictEditEdit,
		},
	}

	result, err := h.Resolve(context.Background(), action)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Record)

	// Resolution is keep_both.
	assert.Equal(t, ConflictKeepBoth, result.Record.Resolution)
	assert.NotNil(t, result.Record.ResolvedBy)
	assert.Equal(t, ResolvedByAuto, *result.Record.ResolvedBy)
	assert.NotNil(t, result.Record.ResolvedAt)
	assert.NotEmpty(t, result.Record.ID)
	assert.Equal(t, ConflictEditEdit, result.Record.Type)

	// Sub-action is a download.
	require.Len(t, result.SubActions, 1)
	assert.Equal(t, ActionDownload, result.SubActions[0].Type)
	assert.Equal(t, "file.txt", result.SubActions[0].Path)

	// Local file was renamed away.
	_, statErr := os.Stat(filepath.Join(syncRoot, "file.txt"))
	assert.True(t, os.IsNotExist(statErr), "original file should be gone after rename")

	// Conflict copy exists and contains original content.
	matches, _ := filepath.Glob(filepath.Join(syncRoot, "file.conflict-*.txt"))
	require.Len(t, matches, 1, "expected one conflict copy")
	got, readErr := os.ReadFile(matches[0])
	require.NoError(t, readErr)
	assert.Equal(t, []byte("local"), got, "conflict copy should preserve original content")
}

func TestConflictHandler_Resolve_CreateCreate(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "new.txt"), []byte("local new"), 0o644))

	action := &Action{
		DriveID: "d1",
		ItemID:  "i1",
		Path:    "new.txt",
		Item:    &Item{DriveID: "d1", ItemID: "i1", Path: "new.txt"},
		ConflictInfo: &ConflictRecord{
			DriveID:    "d1",
			ItemID:     "i1",
			Path:       "new.txt",
			LocalHash:  "AAA",
			RemoteHash: "CCC",
			Type:       ConflictCreateCreate,
		},
	}

	result, err := h.Resolve(context.Background(), action)

	require.NoError(t, err)
	require.Len(t, result.SubActions, 1)
	assert.Equal(t, ActionDownload, result.SubActions[0].Type)
	assert.Equal(t, ConflictCreateCreate, result.Record.Type)
	assert.Equal(t, ConflictKeepBoth, result.Record.Resolution)

	// Local file was renamed.
	_, statErr := os.Stat(filepath.Join(syncRoot, "new.txt"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestConflictHandler_Resolve_EditDelete(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	// Local file remains (no rename for edit-delete).
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "edited.txt"), []byte("local edit"), 0o644))

	action := &Action{
		DriveID: "d1",
		ItemID:  "i1",
		Path:    "edited.txt",
		Item:    &Item{DriveID: "d1", ItemID: "i1", Path: "edited.txt"},
		ConflictInfo: &ConflictRecord{
			DriveID:   "d1",
			ItemID:    "i1",
			Path:      "edited.txt",
			LocalHash: "BBB",
			Type:      ConflictEditDelete,
		},
	}

	result, err := h.Resolve(context.Background(), action)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, ConflictKeepBoth, result.Record.Resolution)
	assert.Equal(t, ConflictEditDelete, result.Record.Type)

	// Sub-action is an upload (re-upload local to remote).
	require.Len(t, result.SubActions, 1)
	assert.Equal(t, ActionUpload, result.SubActions[0].Type)
	assert.Equal(t, "edited.txt", result.SubActions[0].Path)

	// Local file still present (not renamed).
	_, statErr := os.Stat(filepath.Join(syncRoot, "edited.txt"))
	assert.NoError(t, statErr, "local file should still exist for edit-delete")
}

func TestConflictHandler_Resolve_NilConflictInfo(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	action := &Action{
		DriveID:      "d1",
		ItemID:       "i1",
		Path:         "file.txt",
		Item:         &Item{DriveID: "d1", ItemID: "i1"},
		ConflictInfo: nil,
	}

	_, err := h.Resolve(context.Background(), action)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil ConflictInfo")
}

func TestConflictHandler_Resolve_NilItem(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	action := &Action{
		DriveID: "d1",
		ItemID:  "i1",
		Path:    "file.txt",
		Item:    nil,
		ConflictInfo: &ConflictRecord{
			Type: ConflictEditEdit,
		},
	}

	_, err := h.Resolve(context.Background(), action)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil Item")
}

func TestConflictHandler_Resolve_UnknownType(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	action := &Action{
		DriveID: "d1",
		ItemID:  "i1",
		Path:    "file.txt",
		Item:    &Item{DriveID: "d1", ItemID: "i1"},
		ConflictInfo: &ConflictRecord{
			Type: ConflictType("unknown_type"),
		},
	}

	_, err := h.Resolve(context.Background(), action)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown conflict type")
}

func TestConflictHandler_Resolve_EditEdit_MissingLocalFile(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	// File does not exist — rename should fail.
	action := &Action{
		DriveID: "d1",
		ItemID:  "i1",
		Path:    "missing.txt",
		Item:    &Item{DriveID: "d1", ItemID: "i1"},
		ConflictInfo: &ConflictRecord{
			Type:      ConflictEditEdit,
			LocalHash: "AAA",
		},
	}

	_, err := h.Resolve(context.Background(), action)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "rename")
}

func TestConflictHandler_Record_FieldsPopulated(t *testing.T) {
	syncRoot := t.TempDir()
	h := newTestConflictHandler(t, syncRoot)

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.pdf"), []byte("content"), 0o644))

	mt := NowNano()
	action := &Action{
		DriveID: "drive-1",
		ItemID:  "item-2",
		Path:    "doc.pdf",
		Item:    &Item{DriveID: "drive-1", ItemID: "item-2"},
		ConflictInfo: &ConflictRecord{
			DriveID:    "drive-1",
			ItemID:     "item-2",
			Path:       "doc.pdf",
			LocalHash:  "LOCALHASH",
			RemoteHash: "REMOTEHASH",
			LocalMtime: &mt,
			Type:       ConflictEditEdit,
		},
	}

	result, err := h.Resolve(context.Background(), action)
	require.NoError(t, err)

	rec := result.Record
	assert.Equal(t, "drive-1", rec.DriveID)
	assert.Equal(t, "item-2", rec.ItemID)
	assert.Equal(t, "doc.pdf", rec.Path)
	assert.Equal(t, "LOCALHASH", rec.LocalHash)
	assert.Equal(t, "REMOTEHASH", rec.RemoteHash)
	assert.Equal(t, mt, *rec.LocalMtime)
	assert.Greater(t, rec.DetectedAt, int64(0))
}
