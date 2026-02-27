package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// writeTestFile creates a file with the given content under dir/relPath,
// creating parent directories as needed. Returns the absolute path.
func writeTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	fullPath := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(fullPath), err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", fullPath, err)
	}

	return fullPath
}

// hashContent computes the QuickXorHash of a string, returning the
// base64-encoded digest. Matches the output of computeQuickXorHash for
// the same content written to a file.
func hashContent(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	if _, err := h.Write([]byte(content)); err != nil {
		t.Fatalf("hash.Write: %v", err)
	}

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

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}

	ev := findEvent(events, "hello.txt")
	if ev == nil {
		t.Fatal("hello.txt event not found")
	}

	if ev.Source != SourceLocal {
		t.Errorf("Source = %v, want SourceLocal", ev.Source)
	}

	if ev.Type != ChangeCreate {
		t.Errorf("Type = %v, want ChangeCreate", ev.Type)
	}

	if ev.Name != "hello.txt" {
		t.Errorf("Name = %q, want %q", ev.Name, "hello.txt")
	}

	if ev.Hash != hashContent(t, "hello world") {
		t.Errorf("Hash = %q, want %q", ev.Hash, hashContent(t, "hello world"))
	}

	if ev.Size != int64(len("hello world")) {
		t.Errorf("Size = %d, want %d", ev.Size, len("hello world"))
	}

	if ev.Mtime == 0 {
		t.Error("Mtime = 0, want non-zero")
	}

	if ev.ItemType != ItemTypeFile {
		t.Errorf("ItemType = %v, want ItemTypeFile", ev.ItemType)
	}
}

func TestFullScan_NewFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "photos"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	ev := events[0]
	if ev.Type != ChangeCreate {
		t.Errorf("Type = %v, want ChangeCreate", ev.Type)
	}

	if ev.ItemType != ItemTypeFolder {
		t.Errorf("ItemType = %v, want ItemTypeFolder", ev.ItemType)
	}

	if ev.Hash != "" {
		t.Errorf("Hash = %q, want empty (folders have no hash)", ev.Hash)
	}

	if ev.Path != "photos" {
		t.Errorf("Path = %q, want %q", ev.Path, "photos")
	}
}

func TestFullScan_ModifiedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "doc.txt", "updated content")

	baseline := baselineWith(&BaselineEntry{
		Path: "doc.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, "original content"),
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	ev := events[0]
	if ev.Type != ChangeModify {
		t.Errorf("Type = %v, want ChangeModify", ev.Type)
	}

	if ev.Hash != hashContent(t, "updated content") {
		t.Errorf("Hash = %q, want %q", ev.Hash, hashContent(t, "updated content"))
	}

	if ev.Source != SourceLocal {
		t.Errorf("Source = %v, want SourceLocal", ev.Source)
	}
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

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (file unchanged)", len(events))
	}
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

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	ev := events[0]
	if ev.Type != ChangeDelete {
		t.Errorf("Type = %v, want ChangeDelete", ev.Type)
	}

	if !ev.IsDeleted {
		t.Error("IsDeleted = false, want true")
	}

	if ev.Path != "gone.txt" {
		t.Errorf("Path = %q, want %q", ev.Path, "gone.txt")
	}

	if ev.Name != "gone.txt" {
		t.Errorf("Name = %q, want %q", ev.Name, "gone.txt")
	}

	// Size and Mtime should be populated from baseline.
	if ev.Size != 42 {
		t.Errorf("Size = %d, want 42", ev.Size)
	}

	if ev.Mtime != 1234567890 {
		t.Errorf("Mtime = %d, want 1234567890", ev.Mtime)
	}
}

func TestFullScan_DeletedFolder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	baseline := baselineWith(&BaselineEntry{
		Path: "old-folder", DriveID: driveid.New("d"), ItemID: "f1",
		ItemType: ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	ev := events[0]
	if ev.Type != ChangeDelete {
		t.Errorf("Type = %v, want ChangeDelete", ev.Type)
	}

	if ev.ItemType != ItemTypeFolder {
		t.Errorf("ItemType = %v, want ItemTypeFolder", ev.ItemType)
	}
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

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (mtime change only, hash matches)", len(events))
	}
}

func TestFullScan_MtimeSizeFastPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "fast path content"
	writeTestFile(t, dir, "cached.txt", content)

	// Set mtime to 1 hour ago so it's well outside the racily-clean window.
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "cached.txt"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "cached.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Baseline matches file's actual mtime, size, and hash — fast path should skip hashing.
	baseline := baselineWith(&BaselineEntry{
		Path: "cached.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, content),
		Size: info.Size(), Mtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (fast path should skip unchanged file)", len(events))
	}
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
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Baseline has same mtime and size but different hash.
	baseline := baselineWith(&BaselineEntry{
		Path: "racy.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, baselineContent),
		Size: info.Size(), Mtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// Racily-clean guard should force hash, detect the mismatch → ChangeModify.
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 (racily clean should force hash)", len(events))
	}

	ev := events[0]
	if ev.Type != ChangeModify {
		t.Errorf("Type = %v, want ChangeModify", ev.Type)
	}

	if ev.Hash != hashContent(t, actualContent) {
		t.Errorf("Hash = %q, want %q", ev.Hash, hashContent(t, actualContent))
	}
}

func TestFullScan_SizeChangeForcesHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "new longer content"
	writeTestFile(t, dir, "grown.txt", content)

	// Set mtime to 1 hour ago.
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "grown.txt"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "grown.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// Baseline has same mtime but different size — should force hash.
	baseline := baselineWith(&BaselineEntry{
		Path: "grown.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, "short"),
		Size: 5, Mtime: info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 (size change should force hash)", len(events))
	}

	if events[0].Type != ChangeModify {
		t.Errorf("Type = %v, want ChangeModify", events[0].Type)
	}
}

func TestFullScan_NosyncGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, ".nosync", "")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	_, err := obs.FullScan(context.Background(), dir)

	if !errors.Is(err, ErrNosyncGuard) {
		t.Errorf("err = %v, want ErrNosyncGuard", err)
	}
}

func TestFullScan_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}

func TestFullScan_Symlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "real.txt", "content")

	if err := os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// Only the real file should produce an event, not the symlink.
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 (symlink should be skipped)", len(events))
	}

	if events[0].Path != "real.txt" {
		t.Errorf("Path = %q, want %q", events[0].Path, "real.txt")
	}
}

func TestFullScan_InvalidName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "CON", "reserved")
	writeTestFile(t, dir, "valid.txt", "ok")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// CON should be skipped; only valid.txt produces an event.
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Path != "valid.txt" {
		t.Errorf("Path = %q, want %q", events[0].Path, "valid.txt")
	}
}

func TestFullScan_AlwaysExcluded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "download.partial", "incomplete")
	writeTestFile(t, dir, "temp.tmp", "temporary")
	writeTestFile(t, dir, "~backup", "old")
	writeTestFile(t, dir, "legit.txt", "keep me")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// Only legit.txt should produce an event.
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Path != "legit.txt" {
		t.Errorf("Path = %q, want %q", events[0].Path, "legit.txt")
	}
}

func TestFullScan_ContextCanceled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "content")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	_, err := obs.FullScan(ctx, dir)

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestFullScan_NFCNormalization(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// NFD decomposed: e + combining acute accent (U+0301).
	nfdName := "re\u0301sume\u0301.txt"
	// NFC composed: precomposed characters.
	nfcName := "r\u00e9sum\u00e9.txt"

	writeTestFile(t, dir, nfdName, "resume content")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Path != nfcName {
		t.Errorf("Path = %q, want %q (NFC-normalized)", events[0].Path, nfcName)
	}

	if events[0].Name != nfcName {
		t.Errorf("Name = %q, want %q (NFC-normalized)", events[0].Name, nfcName)
	}
}

func TestFullScan_NestedDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "a/b/deep.txt", "deep content")
	writeTestFile(t, dir, "top.txt", "top content")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// Expect: folder "a", folder "a/b", file "a/b/deep.txt", file "top.txt".
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4", len(events))
	}

	deepEv := findEvent(events, "a/b/deep.txt")
	if deepEv == nil {
		t.Fatal("a/b/deep.txt event not found")
	}

	if deepEv.Type != ChangeCreate {
		t.Errorf("Type = %v, want ChangeCreate", deepEv.Type)
	}

	// Verify folder events exist with correct paths.
	aEv := findEvent(events, "a")
	if aEv == nil {
		t.Fatal("folder 'a' event not found")
	}

	if aEv.ItemType != ItemTypeFolder {
		t.Errorf("folder 'a' ItemType = %v, want ItemTypeFolder", aEv.ItemType)
	}

	abEv := findEvent(events, "a/b")
	if abEv == nil {
		t.Fatal("folder 'a/b' event not found")
	}
}

func TestFullScan_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "empty.txt", "")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	ev := events[0]
	if ev.Type != ChangeCreate {
		t.Errorf("Type = %v, want ChangeCreate", ev.Type)
	}

	if ev.Hash == "" {
		t.Error("Hash is empty, want non-empty hash for empty file")
	}

	if ev.Hash != hashContent(t, "") {
		t.Errorf("Hash = %q, want %q", ev.Hash, hashContent(t, ""))
	}

	if ev.Size != 0 {
		t.Errorf("Size = %d, want 0", ev.Size)
	}
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

	obs := NewLocalObserver(baseline, testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// 3 events: new, modified, deleted. Unchanged produces no event.
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}

	newEv := findEvent(events, "new.txt")
	if newEv == nil {
		t.Fatal("new.txt event not found")
	}

	if newEv.Type != ChangeCreate {
		t.Errorf("new.txt Type = %v, want ChangeCreate", newEv.Type)
	}

	modEv := findEvent(events, "modified.txt")
	if modEv == nil {
		t.Fatal("modified.txt event not found")
	}

	if modEv.Type != ChangeModify {
		t.Errorf("modified.txt Type = %v, want ChangeModify", modEv.Type)
	}

	delEv := findEvent(events, "deleted.txt")
	if delEv == nil {
		t.Fatal("deleted.txt event not found")
	}

	if delEv.Type != ChangeDelete {
		t.Errorf("deleted.txt Type = %v, want ChangeDelete", delEv.Type)
	}
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
			if got != tt.want {
				t.Errorf("isAlwaysExcluded(%q) = %v, want %v", tt.in, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("isValidOneDriveName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestComputeQuickXorHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello world"
	path := writeTestFile(t, dir, "test.txt", content)

	hash, err := computeQuickXorHash(path)
	if err != nil {
		t.Fatalf("computeQuickXorHash: %v", err)
	}

	want := hashContent(t, content)
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
}

func TestComputeQuickXorHash_EmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeTestFile(t, dir, "empty.txt", "")

	hash, err := computeQuickXorHash(path)
	if err != nil {
		t.Fatalf("computeQuickXorHash: %v", err)
	}

	want := hashContent(t, "")
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}

	if hash == "" {
		t.Error("empty file hash should not be empty string")
	}
}

func TestComputeQuickXorHash_NonexistentFile(t *testing.T) {
	t.Parallel()

	_, err := computeQuickXorHash("/nonexistent/path/file.txt")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestItemTypeFromDirEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "x")

	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		got := itemTypeFromDirEntry(e)

		switch {
		case e.IsDir() && got != ItemTypeFolder:
			t.Errorf("%s: got %v, want ItemTypeFolder", e.Name(), got)
		case !e.IsDir() && got != ItemTypeFile:
			t.Errorf("%s: got %v, want ItemTypeFile", e.Name(), got)
		}
	}
}

func TestSkipEntry_Dir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			got := skipEntry(e)
			if !errors.Is(got, filepath.SkipDir) {
				t.Errorf("skipEntry(dir) = %v, want filepath.SkipDir", got)
			}
		}
	}
}

func TestSkipEntry_File(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "file.txt", "x")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			got := skipEntry(e)
			if got != nil {
				t.Errorf("skipEntry(file) = %v, want nil", got)
			}
		}
	}
}

func TestSkipEntry_Nil(t *testing.T) {
	t.Parallel()

	got := skipEntry(nil)
	if got != nil {
		t.Errorf("skipEntry(nil) = %v, want nil", got)
	}
}

func TestFullScan_NosyncGuardDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// .nosync as a directory should also trigger the guard.
	if err := os.Mkdir(filepath.Join(dir, ".nosync"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	_, err := obs.FullScan(context.Background(), dir)

	if !errors.Is(err, ErrNosyncGuard) {
		t.Errorf("err = %v, want ErrNosyncGuard", err)
	}
}

func TestFullScan_ExcludedDirSkipsSubtree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create an always-excluded directory and a file inside it.
	writeTestFile(t, dir, "~excluded/inner.txt", "hidden")
	writeTestFile(t, dir, "visible.txt", "shown")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// Only visible.txt should appear; ~excluded dir and its contents are skipped.
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Path != "visible.txt" {
		t.Errorf("Path = %q, want %q", events[0].Path, "visible.txt")
	}
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
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}

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

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("FullScan: %v", err)
	}

	// Only good.txt should appear; bad. dir and its contents are skipped.
	if findEvent(events, "bad./child.txt") != nil {
		t.Error("child inside invalid-name dir should not produce an event")
	}

	if findEvent(events, "good.txt") == nil {
		t.Error("good.txt event not found")
	}
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

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), syncRoot)
	require.NoError(t, err)

	// Should see events for readable dir + file, but not unreadable contents.
	paths := eventPaths(events)
	if !containsPath(paths, "readable/file.txt") {
		t.Error("expected event for readable/file.txt")
	}

	if containsPath(paths, "unreadable/hidden.txt") {
		t.Error("should not have event for unreadable/hidden.txt")
	}
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

			if got != tt.want {
				t.Errorf("itemTypeFromDirEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Hash failure creates with empty hash (B-102) — FullScan variant
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

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events, err := obs.FullScan(context.Background(), dir)
	require.NoError(t, err)

	ev := findEvent(events, "unreadable.txt")
	require.NotNil(t, ev, "unreadable file should still produce an event")
	require.Equal(t, ChangeCreate, ev.Type)
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

			if got != tt.want {
				t.Errorf("syncRootExists(%q) = %v, want %v", p, got, tt.want)
			}
		})
	}
}
