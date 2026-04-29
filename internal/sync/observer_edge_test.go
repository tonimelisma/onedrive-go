package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
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
		Path:           "racily-clean.txt",
		ItemType:       ItemTypeFile,
		LocalHash:      hash,
		LocalSize:      info.Size(),
		LocalSizeKnown: true,
		LocalMtime:     info.ModTime().UnixNano(),
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)

	// Immediately scan — the file's mtime is within 1 second of now.
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// The file is UNCHANGED, but the racily-clean guard forces a hash check.
	// Since the hash matches, no change event should be emitted.
	for _, ev := range result.Events {
		if ev.Path == "racily-clean.txt" {
			assert.NotEqual(t, ChangeModify, ev.Type,
				"racily-clean file with matching hash should not emit ChangeModify")
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
		Path:           "touched.txt",
		ItemType:       ItemTypeFile,
		LocalHash:      hash,
		LocalSize:      info.Size(),
		LocalSizeKnown: true,
		LocalMtime:     info.ModTime().UnixNano() - 2*int64(time.Second), // 2 seconds earlier
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)

	// Mtime differs but hash matches → no event should be emitted.
	for _, ev := range result.Events {
		if ev.Path == "touched.txt" {
			assert.NotEqual(t, ChangeModify, ev.Type,
				"mtime change without content change should not emit ChangeModify")
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

func TestFullScan_NosyncMarkerDoesNotGuardSync(t *testing.T) {
	syncRoot := t.TempDir()

	writeTestFile(t, syncRoot, "file1.txt", "content1")
	writeTestFile(t, syncRoot, "file2.txt", "content2")
	writeTestFile(t, syncRoot, ".nosync", "")

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)
	assert.Len(t, result.Events, 3)
	assert.Contains(t, eventPaths(result.Events), ".nosync")
}

// TestOneDriveInvalidNames_Rejected validates that all OneDrive-invalid
// name patterns are correctly rejected by ValidateOneDriveName.
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
			reason, _ := ValidateOneDriveName(tt.name)
			assert.NotEmpty(t, reason,
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
			reason, _ := ValidateOneDriveName(name)
			assert.Empty(t, reason,
				"%q should be accepted", name)
		})
	}
}

// TestAsciiLower validates the allocation-free ASCII lowering helper used by
// bundled junk matching.
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

			got := AsciiLower(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestValidateOneDriveName_MaxLength validates the 255-byte limit.
func TestValidateOneDriveName_MaxLength(t *testing.T) {
	validName := strings.Repeat("a", 255)
	reason, _ := ValidateOneDriveName(validName)
	assert.Empty(t, reason, "255 bytes should be valid")

	// 256 bytes is invalid.
	invalidName := validName + "b"
	reason, _ = ValidateOneDriveName(invalidName)
	assert.NotEmpty(t, reason, "256 bytes should be invalid")
}

// TestValidateOneDriveName_Empty validates that empty names are rejected.
func TestValidateOneDriveName_Empty(t *testing.T) {
	reason, _ := ValidateOneDriveName("")
	assert.NotEmpty(t, reason, "empty name should be invalid")
}

// TestIsOversizedFile validates the boundary behavior of the Stage 2
// observation filter at the 250 GB OneDrive file size limit.
func TestIsOversizedFile(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	// Exactly at the limit — should NOT be oversized.
	assert.False(t, obs.IsOversizedFile(MaxOneDriveFileSize, "exactly-250gb.bin"),
		"file exactly at MaxOneDriveFileSize should not be oversized")

	// One byte over the limit — should be oversized.
	assert.True(t, obs.IsOversizedFile(MaxOneDriveFileSize+1, "over-250gb.bin"),
		"file one byte over MaxOneDriveFileSize should be oversized")

	// Zero-length file — should NOT be oversized.
	assert.False(t, obs.IsOversizedFile(0, "empty.txt"),
		"zero-length file should not be oversized")
}
