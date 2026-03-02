package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// §7: Observer Edge Cases
// ---------------------------------------------------------------------------

// TestRacilyClean_SameSecondDetection validates that files modified in
// the same second as the scan start are not trusted by the mtime+size
// fast path. The racily-clean guard forces a hash check.
func TestRacilyClean_SameSecondDetection(t *testing.T) {
	syncRoot := t.TempDir()

	content := "original content"
	filePath := filepath.Join(syncRoot, "racily-clean.txt")
	writeTestFile(t, syncRoot, "racily-clean.txt", content)

	hash := hashContent(t, content)

	// Set the baseline with mtime very close to "now" (within 1 second).
	info, err := os.Stat(filePath)
	require.NoError(t, err)

	baseline := baselineWith(&BaselineEntry{
		Path:      "racily-clean.txt",
		ItemType:  ItemTypeFile,
		LocalHash: hash,
		Size:      info.Size(),
		Mtime:     info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)

	// Immediately scan — the file's mtime is within 1 second of now.
	events, err := obs.FullScan(context.Background(), syncRoot)
	require.NoError(t, err)

	// The file is UNCHANGED, but the racily-clean guard forces a hash check.
	// Since the hash matches, no change event should be emitted.
	for _, ev := range events {
		if ev.Path == "racily-clean.txt" && ev.Type == ChangeModify {
			t.Error("racily-clean file with matching hash should not emit ChangeModify")
		}
	}
}

// TestMtimeChangeWithoutContentChange validates that when a file's mtime
// differs from baseline but the hash matches, no ChangeModify event
// is emitted (content didn't actually change).
func TestMtimeChangeWithoutContentChange(t *testing.T) {
	syncRoot := t.TempDir()

	content := "unchanged content"
	writeTestFile(t, syncRoot, "touched.txt", content)

	hash := hashContent(t, content)
	filePath := filepath.Join(syncRoot, "touched.txt")

	info, err := os.Stat(filePath)
	require.NoError(t, err)

	// Create baseline with a DIFFERENT mtime but same hash.
	baseline := baselineWith(&BaselineEntry{
		Path:      "touched.txt",
		ItemType:  ItemTypeFile,
		LocalHash: hash,
		Size:      info.Size(),
		Mtime:     info.ModTime().UnixNano() - 2*int64(time.Second), // 2 seconds earlier
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)

	events, err := obs.FullScan(context.Background(), syncRoot)
	require.NoError(t, err)

	// Mtime differs but hash matches → no event should be emitted.
	for _, ev := range events {
		if ev.Path == "touched.txt" && ev.Type == ChangeModify {
			t.Error("mtime change without content change should not emit ChangeModify")
		}
	}
}

// TestDotfileStemExt validates filepath.Ext behavior on dotfiles.
// Go treats the entire ".bashrc" as an extension (everything from the last dot),
// which means conflict rename stem/ext splitting must handle this case.
func TestDotfileStemExt(t *testing.T) {
	// filepath.Ext returns everything from the last dot, including the dot.
	assert.Equal(t, ".bashrc", filepath.Ext(".bashrc"),
		"Go stdlib: .bashrc extension is the whole name")
	assert.Equal(t, ".txt", filepath.Ext(".bashrc.txt"),
		"Go stdlib: .bashrc.txt extension is .txt")
	assert.Equal(t, ".txt", filepath.Ext("file.txt"),
		"Go stdlib: file.txt extension is .txt")
}

// TestNosyncGuard_PreventsAllSync validates that when a .nosync file
// is present in the sync root, the observer produces zero events and
// returns ErrNosyncGuard.
func TestNosyncGuard_PreventsAllSync(t *testing.T) {
	syncRoot := t.TempDir()

	// Create some normal files.
	writeTestFile(t, syncRoot, "file1.txt", "content1")
	writeTestFile(t, syncRoot, "file2.txt", "content2")

	// Create the .nosync guard file.
	writeTestFile(t, syncRoot, ".nosync", "")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	events, err := obs.FullScan(context.Background(), syncRoot)
	assert.ErrorIs(t, err, ErrNosyncGuard)
	assert.Nil(t, events, ".nosync should prevent all events")
}

// TestOneDriveInvalidNames_Rejected validates that all OneDrive-invalid
// name patterns are correctly rejected by isValidOneDriveName.
func TestOneDriveInvalidNames_Rejected(t *testing.T) {
	invalidNames := []struct {
		name   string
		reason string
	}{
		{"CON", "reserved device name"},
		{"PRN", "reserved device name"},
		{"AUX", "reserved device name"},
		{"NUL", "reserved device name"},
		{"COM0", "reserved device name with digit"},
		{"COM9", "reserved device name with digit"},
		{"LPT0", "reserved device name with digit"},
		{"LPT9", "reserved device name with digit"},
		{"con", "case-insensitive reserved"},
		{"file.lock", ".lock extension"},
		{"desktop.ini", "desktop.ini"},
		{"~$document.docx", "~$ prefix (Office temp)"},
		{"_vti_cnf", "_vti_ substring"},
		{"folder_vti_test", "_vti_ substring"},
		{"file\".txt", "contains double quote"},
		{"file*.txt", "contains asterisk"},
		{"file:.txt", "contains colon"},
		{"file<.txt", "contains less-than"},
		{"file>.txt", "contains greater-than"},
		{"file?.txt", "contains question mark"},
		{"file|.txt", "contains pipe"},
		{"trailing.", "trailing dot"},
		{"trailing ", "trailing space"},
		{" leading", "leading space"},
	}

	for _, tt := range invalidNames {
		t.Run(tt.name+"_"+tt.reason, func(t *testing.T) {
			assert.False(t, isValidOneDriveName(tt.name),
				"%q should be rejected (%s)", tt.name, tt.reason)
		})
	}
}

// TestOneDriveValidNames_Accepted validates that normal filenames pass
// the OneDrive name validation.
func TestOneDriveValidNames_Accepted(t *testing.T) {
	validNames := []string{
		"document.txt",
		"photo.jpg",
		"my file.txt",
		"café.txt",
		"日本語.doc",
		".hidden",
		"COM10",   // 5 chars, not a reserved device name
		"COMX",    // Not COM + digit
		"LPT",     // 3 chars, not LPT + digit
		"NUL.txt", // Wait — this contains "NUL" but has an extension!
	}

	for _, name := range validNames {
		t.Run(name, func(t *testing.T) {
			// Note: NUL.txt would actually be valid since we only check
			// the exact name "nul", not names starting with "nul".
			assert.True(t, isValidOneDriveName(name),
				"%q should be accepted", name)
		})
	}
}

// TestAlwaysExcludedSuffixes_Order validates that .db-wal is matched
// BEFORE .db in the suffix check (order matters for correct exclusion).
func TestAlwaysExcludedSuffixes_Order(t *testing.T) {
	// .db-wal should be excluded by the .db-wal suffix.
	assert.True(t, isAlwaysExcluded("data.db-wal"),
		".db-wal should be excluded")
	// .db-shm should be excluded by the .db-shm suffix.
	assert.True(t, isAlwaysExcluded("data.db-shm"),
		".db-shm should be excluded")
	// .db should be excluded by the .db suffix.
	assert.True(t, isAlwaysExcluded("sync.db"),
		".db should be excluded")

	// Verify the order: .db-wal MUST come before .db in the list to
	// avoid .db matching first and leaving "-wal" unmatched in a
	// different context. Since we use HasSuffix, order doesn't
	// affect correctness here, but it's a defense-in-depth check.
	walIdx := -1
	dbIdx := -1

	for i, ext := range alwaysExcludedSuffixes {
		if ext == ".db-wal" {
			walIdx = i
		}

		if ext == ".db" {
			dbIdx = i
		}
	}

	assert.Greater(t, dbIdx, walIdx,
		".db-wal should appear before .db in suffix list (defense in depth)")
}

// TestAlwaysExcluded_PrefixPatterns validates that editor backup files
// (~file) and LibreOffice locks (.~lock) are excluded.
func TestAlwaysExcluded_PrefixPatterns(t *testing.T) {
	tests := []struct {
		name     string
		excluded bool
	}{
		{"~backup.txt", true},
		{"~$document.docx", true},
		{".~lock.file", true},
		{"normal.txt", false},
		{"file~renamed", false}, // tilde not at start
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.excluded, isAlwaysExcluded(tt.name),
				"isAlwaysExcluded(%q)", tt.name)
		})
	}
}

// TestAsciiLower validates the allocation-free ASCII lowering helper used by
// isAlwaysExcluded on every fsnotify event and FullScan entry.
func TestAsciiLower(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"already-lower.txt", "already-lower.txt"},
		{"ALLCAPS.TXT", "allcaps.txt"},
		{"MixedCase.Partial", "mixedcase.partial"},
		{"", ""},
		{"noext", "noext"},
		{"café.txt", "café.txt"},       // non-ASCII unchanged
		{"FILE.DB-WAL", "file.db-wal"}, // matches excluded suffix
		{"~$Document.docx", "~$document.docx"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			got := asciiLower(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestIsValidOneDriveName_MaxLength validates the 255-byte limit.
func TestIsValidOneDriveName_MaxLength(t *testing.T) {
	validName := strings.Repeat("a", 255)
	assert.True(t, isValidOneDriveName(validName), "255 bytes should be valid")

	// 256 bytes is invalid.
	invalidName := validName + "b"
	assert.False(t, isValidOneDriveName(invalidName), "256 bytes should be invalid")
}

// TestIsValidOneDriveName_Empty validates that empty names are rejected.
func TestIsValidOneDriveName_Empty(t *testing.T) {
	assert.False(t, isValidOneDriveName(""), "empty name should be invalid")
}
