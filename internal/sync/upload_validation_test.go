package sync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSingleUpload_ValidFile(t *testing.T) {
	a := &Action{
		Type: ActionUpload,
		Path: "docs/report.xlsx",
		View: &PathView{Local: &LocalState{Size: 1024}},
	}
	assert.Nil(t, validateSingleUpload(a))
}

func TestValidateSingleUpload_InvalidFilename(t *testing.T) {
	a := &Action{
		Type: ActionUpload,
		Path: "docs/CON",
		View: &PathView{Local: &LocalState{Size: 1024}},
	}
	fail := validateSingleUpload(a)
	require.NotNil(t, fail)
	assert.Equal(t, "invalid_filename", fail.IssueType)
	assert.Contains(t, fail.Error, "CON")
}

func TestValidateSingleUpload_ReservedNames(t *testing.T) {
	// Exact reserved device names (no extension) are rejected by isValidOneDriveName.
	reserved := []string{
		"CON", "PRN", "AUX", "NUL",
		"COM0", "COM1", "COM9",
		"LPT0", "LPT1", "LPT9",
	}

	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			a := &Action{
				Type: ActionUpload,
				Path: "dir/" + name,
				View: &PathView{Local: &LocalState{Size: 100}},
			}
			fail := validateSingleUpload(a)
			require.NotNil(t, fail, "expected failure for reserved name %s", name)
			assert.Equal(t, "invalid_filename", fail.IssueType)
		})
	}
}

func TestValidateSingleUpload_PathTooLong(t *testing.T) {
	// Build a path >400 chars with short valid components.
	longPath := strings.Repeat("abcdefgh/", 51) + "file.txt" // ~460 chars
	require.Greater(t, len(longPath), maxOneDrivePathLength)

	a := &Action{
		Type: ActionUpload,
		Path: longPath,
		View: &PathView{Local: &LocalState{Size: 100}},
	}
	fail := validateSingleUpload(a)
	require.NotNil(t, fail)
	assert.Equal(t, "path_too_long", fail.IssueType)
}

func TestValidateSingleUpload_FileTooLarge(t *testing.T) {
	a := &Action{
		Type: ActionUpload,
		Path: "huge.bin",
		View: &PathView{Local: &LocalState{Size: 300 * 1024 * 1024 * 1024}}, // 300 GB
	}
	fail := validateSingleUpload(a)
	require.NotNil(t, fail)
	assert.Equal(t, "file_too_large", fail.IssueType)
}

func TestValidateUploadActions_Mixed(t *testing.T) {
	actions := []Action{
		{Type: ActionDownload, Path: "a.txt"},
		{Type: ActionUpload, Path: "valid.txt", View: &PathView{Local: &LocalState{Size: 100}}},
		{Type: ActionUpload, Path: "dir/CON", View: &PathView{Local: &LocalState{Size: 100}}}, // invalid: reserved name
		{Type: ActionDownload, Path: "b.txt"},
		{Type: ActionUpload, Path: "ok.txt", View: &PathView{Local: &LocalState{Size: 100}}},
	}

	keep, failures := validateUploadActions(actions)

	// 2 downloads + 2 valid uploads = 4 kept
	assert.Len(t, keep, 4)
	assert.Contains(t, keep, 0) // download a.txt
	assert.Contains(t, keep, 1) // valid upload
	assert.Contains(t, keep, 3) // download b.txt
	assert.Contains(t, keep, 4) // valid upload ok.txt

	require.Len(t, failures, 1)
	assert.Equal(t, 2, failures[0].Index)
	assert.Equal(t, "dir/CON", failures[0].Path)
	assert.Equal(t, "invalid_filename", failures[0].IssueType)
}

func TestRemoveActionsByIndex_NoRemoval(t *testing.T) {
	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionDownload, Path: "a.txt"},
			{Type: ActionUpload, Path: "b.txt"},
		},
		Deps: [][]int{{}, {0}},
	}

	keep := []int{0, 1}
	result := removeActionsByIndex(plan, keep)

	assert.Equal(t, plan, result, "no-op: should return same plan")
}

func TestRemoveActionsByIndex_MiddleRemoval(t *testing.T) {
	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionFolderCreate, Path: "dir"},
			{Type: ActionUpload, Path: "dir/CON"},    // will be removed
			{Type: ActionUpload, Path: "dir/ok.txt"}, // depends on dir
		},
		Deps: [][]int{{}, {0}, {0}},
	}

	keep := []int{0, 2} // remove index 1
	result := removeActionsByIndex(plan, keep)

	require.Len(t, result.Actions, 2)
	assert.Equal(t, "dir", result.Actions[0].Path)
	assert.Equal(t, "dir/ok.txt", result.Actions[1].Path)

	// dir/ok.txt had dep on index 0 (dir) → old index 0 maps to new index 0
	require.Len(t, result.Deps, 2)
	assert.Equal(t, []int{0}, result.Deps[1])
}

func TestRemoveActionsByIndex_DroppedDeps(t *testing.T) {
	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionFolderCreate, Path: "dir"},
			{Type: ActionUpload, Path: "dir/a.txt"}, // depends on dir, will be removed
			{Type: ActionUpload, Path: "dir/b.txt"}, // depends on dir
		},
		Deps: [][]int{{}, {0}, {0}},
	}

	keep := []int{0, 2} // remove index 1
	result := removeActionsByIndex(plan, keep)

	require.Len(t, result.Actions, 2)
	// Dep on removed action is dropped.
	assert.Empty(t, result.Deps[0])
	assert.Equal(t, []int{0}, result.Deps[1])
}

func TestIsReservedOneDriveName(t *testing.T) {
	assert.True(t, isReservedOneDriveName("CON"))
	assert.True(t, isReservedOneDriveName("con"))
	assert.True(t, isReservedOneDriveName("LPT1"))
	assert.False(t, isReservedOneDriveName("normal.txt"))
}
