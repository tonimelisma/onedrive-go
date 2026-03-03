package sync

import (
	"context"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// itemTypeFromDirEntry maps a DirEntry to the sync engine's ItemType.
// Test-only helper — not used in production code.
func itemTypeFromDirEntry(d fs.DirEntry) ItemType {
	if d.IsDir() {
		return ItemTypeFolder
	}

	return ItemTypeFile
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// writeTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed. Returns the absolute path.
func writeTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755), "MkdirAll(%s)", filepath.Dir(fullPath))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644), "WriteFile(%s)", fullPath)

	return fullPath
}

// hashContent computes the QuickXorHash of a string, returning the
// base64-encoded digest. Matches the output of driveops.ComputeQuickXorHash
// for the same content written to a file.
func hashContent(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "hash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// findEvent returns the first ChangeEvent with the given path, or nil.
func findEvent(events []ChangeEvent, path string) *ChangeEvent {
	for i := range events {
		if events[i].Path == path {
			return &events[i]
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// FullScan tests
// ---------------------------------------------------------------------------

func TestFullScan_NewFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "hello.txt", "hello world")
	writeTestFile(t, dir, "data.csv", "a,b,c")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 2)

	ev := findEvent(events, "hello.txt")
	require.NotNil(t, ev, "hello.txt event not found")

	assert.Equal(t, SourceLocal, ev.Source)
	assert.Equal(t, ChangeCreate, ev.Type)
	assert.Equal(t, "hello.txt", ev.Name)
	assert.Equal(t, hashContent(t, "hello world"), ev.Hash)
	assert.Equal(t, int64(len("hello world")), ev.Size)
	assert.NotZero(t, ev.Mtime, "Mtime should be non-zero")
	assert.Equal(t, ItemTypeFile, ev.ItemType)
}

func TestFullScan_NewFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "photos"), 0o755), "Mkdir")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, ChangeCreate, ev.Type)
	assert.Equal(t, ItemTypeFolder, ev.ItemType)
	assert.Empty(t, ev.Hash, "folders have no hash")
	assert.Equal(t, "photos", ev.Path)
}

func TestFullScan_ModifiedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "doc.txt", "updated content")

	baseline := baselineWith(&BaselineEntry{
		Path: "doc.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, "original content"),
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, ChangeModify, ev.Type)
	assert.Equal(t, hashContent(t, "updated content"), ev.Hash)
	assert.Equal(t, SourceLocal, ev.Source)
}

func TestFullScan_UnchangedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "same content"
	writeTestFile(t, dir, "stable.txt", content)

	baseline := baselineWith(&BaselineEntry{
		Path: "stable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, content),
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	assert.Empty(t, events, "file unchanged")
}

func TestFullScan_DeletedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// File is in baseline but NOT on disk.

	baseline := baselineWith(&BaselineEntry{
		Path: "gone.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: "some-hash",
		Size: 42, Mtime: 1234567890,
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, ChangeDelete, ev.Type)
	assert.True(t, ev.IsDeleted)
	assert.Equal(t, "gone.txt", ev.Path)
	assert.Equal(t, "gone.txt", ev.Name)

	// Size and Mtime should be populated from baseline.
	assert.Equal(t, int64(42), ev.Size)
	assert.Equal(t, int64(1234567890), ev.Mtime)
}

func TestFullScan_DeletedFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	baseline := baselineWith(&BaselineEntry{
		Path: "old-folder", DriveID: driveid.New("d"), ItemID: "f1",
		ItemType: ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, ChangeDelete, ev.Type)
	assert.Equal(t, ItemTypeFolder, ev.ItemType)
}

func TestFullScan_MtimeChangeNoContentChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "unchanged"
	writeTestFile(t, dir, "stable.txt", content)

	// Baseline has a different mtime but the same hash.
	baseline := baselineWith(&BaselineEntry{
		Path: "stable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, content),
		Mtime: 999, // intentionally different from actual file mtime
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	assert.Empty(t, events, "mtime change only, hash matches")
}

func TestFullScan_MtimeSizeFastPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "fast path content"
	writeTestFile(t, dir, "cached.txt", content)

	// Set mtime to 1 hour ago so it's well outside the racily-clean window.
	oldTime := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "cached.txt"), oldTime, oldTime), "Chtimes")

	info, err := os.Stat(filepath.Join(dir, "cached.txt"))
	require.NoError(t, err, "Stat")

	// Baseline matches file's actual mtime, size, and hash — fast path should skip hashing.
	baseline := baselineWith(&BaselineEntry{
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, content),
		Size: info.Size(), Mtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	assert.Empty(t, events, "fast path should skip unchanged file")
}

func TestFullScan_RacilyCleanForcesHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// File content differs from baseline hash, but mtime+size will match.
	// Since the file was just created, mtime is within 1 second of scan start,
	// so the racily-clean guard should force a hash check and detect the change.
	actualContent := "actual_xx"
	baselineContent := "baseline_"
	writeTestFile(t, dir, "racy.txt", actualContent)

	info, err := os.Stat(filepath.Join(dir, "racy.txt"))
	require.NoError(t, err, "Stat")

	// Baseline has same mtime and size but different hash.
	baseline := baselineWith(&BaselineEntry{
		Path: "racy.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, baselineContent),
		Size: info.Size(), Mtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// Racily-clean guard should force hash, detect the mismatch -> ChangeModify.
	require.Len(t, events, 1, "racily clean should force hash")

	ev := events[0]
	assert.Equal(t, ChangeModify, ev.Type)
	assert.Equal(t, hashContent(t, actualContent), ev.Hash)
}

func TestFullScan_SizeChangeForcesHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "new longer content"
	writeTestFile(t, dir, "grown.txt", content)

	// Set mtime to 1 hour ago.
	oldTime := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "grown.txt"), oldTime, oldTime), "Chtimes")

	info, err := os.Stat(filepath.Join(dir, "grown.txt"))
	require.NoError(t, err, "Stat")

	// Baseline has same mtime but different size — should force hash.
	baseline := baselineWith(&BaselineEntry{
		Path: "grown.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, "short"),
		Size: 5, Mtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1, "size change should force hash")
	assert.Equal(t, ChangeModify, events[0].Type)
}

func TestFullScan_NosyncGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, ".nosync", "")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	_, err := obs.FullScan(t.Context(), dir)

	assert.ErrorIs(t, err, ErrNosyncGuard)
}

func TestFullScan_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	assert.Empty(t, events)
}

func TestFullScan_Symlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "real.txt", "content")

	require.NoError(t, os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")), "Symlink")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// Only the real file should produce an event, not the symlink.
	require.Len(t, events, 1, "symlink should be skipped")
	assert.Equal(t, "real.txt", events[0].Path)
}

func TestFullScan_InvalidName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "CON", "reserved")
	writeTestFile(t, dir, "valid.txt", "ok")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// CON should be skipped; only valid.txt produces an event.
	require.Len(t, events, 1)
	assert.Equal(t, "valid.txt", events[0].Path)
}

func TestFullScan_AlwaysExcluded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "download.partial", "incomplete")
	writeTestFile(t, dir, "temp.tmp", "temporary")
	writeTestFile(t, dir, "~backup", "old")
	writeTestFile(t, dir, "legit.txt", "keep me")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// Only legit.txt should produce an event.
	require.Len(t, events, 1)
	assert.Equal(t, "legit.txt", events[0].Path)
}

func TestFullScan_ContextCanceled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "content")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	_, err := obs.FullScan(ctx, dir)

	require.Error(t, err, "expected error")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFullScan_NFCNormalization(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// NFD decomposed: e + combining acute accent (U+0301).
	nfdName := "re\u0301sume\u0301.txt"
	// NFC composed: precomposed characters.
	nfcName := "r\u00e9sum\u00e9.txt"

	writeTestFile(t, dir, nfdName, "resume content")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1)
	assert.Equal(t, nfcName, events[0].Path, "NFC-normalized")
	assert.Equal(t, nfcName, events[0].Name, "NFC-normalized")
}

func TestFullScan_NestedDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a/b/deep.txt", "deep content")
	writeTestFile(t, dir, "top.txt", "top content")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// Expect: folder "a", folder "a/b", file "a/b/deep.txt", file "top.txt".
	require.Len(t, events, 4)

	deepEv := findEvent(events, "a/b/deep.txt")
	require.NotNil(t, deepEv, "a/b/deep.txt event not found")
	assert.Equal(t, ChangeCreate, deepEv.Type)

	// Verify folder events exist with correct paths.
	aEv := findEvent(events, "a")
	require.NotNil(t, aEv, "folder 'a' event not found")
	assert.Equal(t, ItemTypeFolder, aEv.ItemType)

	abEv := findEvent(events, "a/b")
	require.NotNil(t, abEv, "folder 'a/b' event not found")
}

func TestFullScan_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "empty.txt", "")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	require.Len(t, events, 1)

	ev := events[0]
	assert.Equal(t, ChangeCreate, ev.Type)
	assert.NotEmpty(t, ev.Hash, "want non-empty hash for empty file")
	assert.Equal(t, hashContent(t, ""), ev.Hash)
	assert.Equal(t, int64(0), ev.Size)
}

func TestFullScan_MixedChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// New file (not in baseline).
	writeTestFile(t, dir, "new.txt", "new content")

	// Modified file (baseline has old hash).
	writeTestFile(t, dir, "modified.txt", "updated content")

	// Unchanged file (baseline has matching hash).
	writeTestFile(t, dir, "unchanged.txt", "same content")

	// Deleted file: in baseline but NOT on disk (don't create).

	baseline := baselineWith(
		&BaselineEntry{
			Path: "modified.txt", DriveID: driveid.New("d"), ItemID: "i1",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "original content"),
		},
		&BaselineEntry{
			Path: "unchanged.txt", DriveID: driveid.New("d"), ItemID: "i2",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "same content"),
		},
		&BaselineEntry{
			Path: "deleted.txt", DriveID: driveid.New("d"), ItemID: "i3",
			ItemType: ItemTypeFile, LocalHash: "some-hash",
		},
	)

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// 3 events: new, modified, deleted. Unchanged produces no event.
	require.Len(t, events, 3)

	newEv := findEvent(events, "new.txt")
	require.NotNil(t, newEv, "new.txt event not found")
	assert.Equal(t, ChangeCreate, newEv.Type)

	modEv := findEvent(events, "modified.txt")
	require.NotNil(t, modEv, "modified.txt event not found")
	assert.Equal(t, ChangeModify, modEv.Type)

	delEv := findEvent(events, "deleted.txt")
	require.NotNil(t, delEv, "deleted.txt event not found")
	assert.Equal(t, ChangeDelete, delEv.Type)
}

// ---------------------------------------------------------------------------
// Unit tests for helper functions
// ---------------------------------------------------------------------------

func TestIsAlwaysExcluded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Suffix-based exclusions.
		{"partial file", "download.partial", true},
		{"tmp file", "temp.tmp", true},
		{"swap file", "file.swp", true},
		{"crdownload", "file.crdownload", true},
		{"db file", "data.db", true},
		{"db-wal file", "data.db-wal", true},
		{"db-shm file", "data.db-shm", true},

		// Case insensitive.
		{"tmp uppercase", "FILE.TMP", true},
		{"partial mixed case", "Download.Partial", true},
		{"db uppercase", "DATA.DB", true},

		// Prefix-based exclusions.
		{"tilde prefix", "~backup", true},
		{"dot-tilde prefix", ".~lock.file", true},
		{"tilde-dollar", "~$Budget.xlsx", true},

		// Not excluded.
		{"normal txt", "hello.txt", false},
		{"go file", "main.go", false},
		{"dotfile", ".gitignore", false},
		{"csv file", "data.csv", false},
		{"partial in middle", "my.partial.bak", false},
		{"db in middle", "data.db.backup", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isAlwaysExcluded(tt.in)
			assert.Equal(t, tt.want, got, "isAlwaysExcluded(%q)", tt.in)
		})
	}
}

func TestIsValidOneDriveName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Valid names.
		{"simple file", "hello.txt", true},
		{"with spaces", "my file.txt", true},
		{"with dots", "file.tar.gz", true},
		{"unicode", "caf\u00e9.txt", true},
		{"max length", strings.Repeat("a", 255), true},
		{"numbers", "12345", true},

		// Empty.
		{"empty", "", false},

		// Trailing dot or space.
		{"trailing dot", "file.", false},
		{"trailing space", "file ", false},

		// Leading space.
		{"leading space", " file", false},

		// Too long.
		{"too long", strings.Repeat("a", 256), false},

		// Reserved device names.
		{"CON upper", "CON", false},
		{"con lower", "con", false},
		{"PRN", "PRN", false},
		{"prn", "prn", false},
		{"AUX", "AUX", false},
		{"NUL", "NUL", false},
		{"COM0", "COM0", false},
		{"COM9", "COM9", false},
		{"com5", "com5", false},
		{"LPT0", "LPT0", false},
		{"LPT9", "LPT9", false},
		{"lpt3", "lpt3", false},
		{"COM10 is valid", "COM10", true},
		{"CONX is valid", "CONX", true},
		{"COMMA is valid", "comma", true},

		// .lock extension.
		{"lock file", "file.lock", false},
		{"LOCK upper", "FILE.LOCK", false},

		// desktop.ini.
		{"desktop.ini", "desktop.ini", false},
		{"Desktop.INI", "Desktop.INI", false},

		// ~$ prefix.
		{"tilde-dollar", "~$Budget.xlsx", false},

		// _vti_ substring.
		{"vti substring", "test_vti_file", false},
		{"vti prefix", "_vti_history", false},

		// Invalid characters.
		{"double quote", "file\"name", false},
		{"asterisk", "file*name", false},
		{"colon", "file:name", false},
		{"less than", "file<name", false},
		{"greater than", "file>name", false},
		{"question mark", "file?name", false},
		{"forward slash", "file/name", false},
		{"backslash", "file\\name", false},
		{"pipe", "file|name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isValidOneDriveName(tt.in)
			assert.Equal(t, tt.want, got, "isValidOneDriveName(%q)", tt.in)
		})
	}
}

func TestItemTypeFromDirEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "x")

	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755), "Mkdir")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "ReadDir")

	for _, e := range entries {
		got := itemTypeFromDirEntry(e)

		switch {
		case e.IsDir():
			assert.Equal(t, ItemTypeFolder, got, "%s should be ItemTypeFolder", e.Name())
		default:
			assert.Equal(t, ItemTypeFile, got, "%s should be ItemTypeFile", e.Name())
		}
	}
}

func TestSkipEntry_Dir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755), "Mkdir")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "ReadDir")

	for _, e := range entries {
		if e.IsDir() {
			got := skipEntry(e)
			assert.ErrorIs(t, got, filepath.SkipDir)
		}
	}
}

func TestSkipEntry_File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "x")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "ReadDir")

	for _, e := range entries {
		if !e.IsDir() {
			got := skipEntry(e)
			assert.NoError(t, got)
		}
	}
}

func TestSkipEntry_Nil(t *testing.T) {
	t.Parallel()

	got := skipEntry(nil)
	assert.NoError(t, got)
}

func TestFullScan_NosyncGuardDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// .nosync as a directory should also trigger the guard.
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".nosync"), 0o755), "Mkdir")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	_, err := obs.FullScan(t.Context(), dir)

	assert.ErrorIs(t, err, ErrNosyncGuard)
}

func TestFullScan_ExcludedDirSkipsSubtree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create an always-excluded directory and a file inside it.
	writeTestFile(t, dir, "~excluded/inner.txt", "hidden")
	writeTestFile(t, dir, "visible.txt", "shown")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// Only visible.txt should appear; ~excluded dir and its contents are skipped.
	require.Len(t, events, 1)
	assert.Equal(t, "visible.txt", events[0].Path)
}

func TestFullScan_InvalidNameDirSkipsSubtree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Directory with a trailing dot (invalid OneDrive name).
	invalidDir := filepath.Join(dir, "bad.")

	// On some filesystems, trailing dots are stripped. Create and verify.
	if err := os.Mkdir(invalidDir, 0o755); err != nil {
		t.Skipf("filesystem does not support trailing dot in directory name: %v", err)
	}

	// Verify the directory was actually created with the trailing dot.
	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr, "ReadDir")

	hasBadDir := false

	for _, e := range entries {
		if e.Name() == "bad." {
			hasBadDir = true
		}
	}

	if !hasBadDir {
		t.Skip("filesystem stripped trailing dot from directory name")
	}

	writeTestFile(t, dir, "bad./child.txt", "child")
	writeTestFile(t, dir, "good.txt", "good")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err, "FullScan")

	// Only good.txt should appear; bad. dir and its contents are skipped.
	assert.Nil(t, findEvent(events, "bad./child.txt"), "child inside invalid-name dir should not produce an event")
	assert.NotNil(t, findEvent(events, "good.txt"), "good.txt event not found")
}

func TestFullScan_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	t.Parallel()

	syncRoot := t.TempDir()

	// Create readable dir with a file.
	readableDir := filepath.Join(syncRoot, "readable")
	require.NoError(t, os.MkdirAll(readableDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(readableDir, "file.txt"), []byte("ok"), 0o644))

	// Create unreadable dir.
	unreadableDir := filepath.Join(syncRoot, "unreadable")
	require.NoError(t, os.MkdirAll(unreadableDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unreadableDir, "hidden.txt"), []byte("secret"), 0o644))
	require.NoError(t, os.Chmod(unreadableDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadableDir, 0o755) })

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), syncRoot)
	require.NoError(t, err)

	// Should see events for readable dir + file, but not unreadable contents.
	paths := eventPaths(events)
	assert.True(t, containsPath(paths, "readable/file.txt"), "expected event for readable/file.txt")
	assert.False(t, containsPath(paths, "unreadable/hidden.txt"), "should not have event for unreadable/hidden.txt")
}

// eventPaths extracts paths from a slice of ChangeEvents.
func eventPaths(events []ChangeEvent) []string {
	paths := make([]string, len(events))
	for i := range events {
		paths[i] = events[i].Path
	}

	return paths
}

// containsPath checks if a path exists in the paths slice.
func containsPath(paths []string, target string) bool {
	for _, p := range paths {
		if p == target {
			return true
		}
	}

	return false
}

// mockDirEntry implements fs.DirEntry for unit tests of helper functions.
type mockDirEntry struct {
	name  string
	isDir bool
}

func (m mockDirEntry) Name() string               { return m.name }
func (m mockDirEntry) IsDir() bool                { return m.isDir }
func (m mockDirEntry) Info() (fs.FileInfo, error) { return nil, nil }
func (m mockDirEntry) Type() fs.FileMode {
	if m.isDir {
		return fs.ModeDir
	}

	return 0
}

func TestItemTypeFromDirEntry_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		isDir bool
		want  ItemType
	}{
		{"file", false, ItemTypeFile},
		{"directory", true, ItemTypeFolder},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := mockDirEntry{name: "test", isDir: tt.isDir}
			got := itemTypeFromDirEntry(d)

			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Hash failure creates with empty hash (B-102) -- FullScan variant
// ---------------------------------------------------------------------------

// TestFullScan_HashFailureStillEmitsCreate verifies that FullScan emits a
// ChangeCreate event with empty hash when hash computation fails (B-102).
func TestFullScan_HashFailureStillEmitsCreate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	path := writeTestFile(t, dir, "unreadable.txt", "secret")
	require.NoError(t, os.Chmod(path, 0o000))

	t.Cleanup(func() {
		_ = os.Chmod(path, 0o644)
	})

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err)

	ev := findEvent(events, "unreadable.txt")
	require.NotNil(t, ev, "unreadable file should still produce an event")
	require.Equal(t, ChangeCreate, ev.Type)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
	require.Equal(t, SourceLocal, ev.Source)
}

// ---------------------------------------------------------------------------
// Hash failure modifies with empty hash (B-102) -- FullScan variant
// ---------------------------------------------------------------------------

// TestFullScan_HashFailureModifyStillEmitsEvent verifies that FullScan emits a
// ChangeModify event with empty hash when hash computation fails for a modified
// file (B-102). Before the fix, hash failures silently dropped modify events.
func TestFullScan_HashFailureModifyStillEmitsEvent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	path := writeTestFile(t, dir, "unreadable.txt", "modified content")

	// Baseline has a different hash so the slow path (hash check) kicks in.
	baseline := baselineWith(&BaselineEntry{
		Path: "unreadable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, "original content"),
	})

	// Make file unreadable -- stat still succeeds but hash computation fails.
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events, err := obs.FullScan(t.Context(), dir)
	require.NoError(t, err)

	ev := findEvent(events, "unreadable.txt")
	require.NotNil(t, ev, "unreadable modified file should still produce an event")
	require.Equal(t, ChangeModify, ev.Type)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
	require.Equal(t, SourceLocal, ev.Source)
}

// ---------------------------------------------------------------------------
// Sync root deletion detection (B-113)
// ---------------------------------------------------------------------------

func TestSyncRootExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{
			name: "existing directory",
			setup: func(t *testing.T) string {
				t.Helper()
				return t.TempDir()
			},
			want: true,
		},
		{
			name: "nonexistent path",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			want: false,
		},
		{
			name: "file not directory",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				p := filepath.Join(dir, "afile")
				require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
				return p
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := tt.setup(t)
			got := syncRootExists(p)

			assert.Equal(t, tt.want, got, "syncRootExists(%q)", p)
		})
	}
}
