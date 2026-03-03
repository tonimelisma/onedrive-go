package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestTruncateID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "longer than prefix", id: "abcdefghijklmnop", want: "abcdefgh"},
		{name: "exact prefix length", id: "abcdefgh", want: "abcdefgh"},
		{name: "shorter than prefix", id: "abc", want: "abc"},
		{name: "empty string", id: "", want: ""},
		{name: "one char", id: "x", want: "x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateID(tt.id)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatNanoTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nanos int64
		want  string
	}{
		{name: "zero returns empty", nanos: 0, want: ""},
		{name: "unix epoch", nanos: 0 + 1, want: "1970-01-01T00:00:00Z"},
		{name: "known timestamp", nanos: 1704067200_000000000, want: "2024-01-01T00:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatNanoTimestamp(tt.nanos)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- toConflictJSON ---

func TestToConflictJSON(t *testing.T) {
	t.Parallel()

	c := &sync.ConflictRecord{
		ID:           "abc123",
		Path:         "/foo.txt",
		ConflictType: "edit_edit",
		DetectedAt:   1700000000000000000,
		LocalHash:    "aaa",
		RemoteHash:   "bbb",
		Resolution:   "keep_local",
		ResolvedBy:   "user",
		ResolvedAt:   1700000001000000000,
	}

	j := toConflictJSON(c)
	assert.Equal(t, "abc123", j.ID)
	assert.Equal(t, "/foo.txt", j.Path)
	assert.Equal(t, "edit_edit", j.ConflictType)
	assert.NotEmpty(t, j.DetectedAt)
	assert.Equal(t, "aaa", j.LocalHash)
	assert.Equal(t, "bbb", j.RemoteHash)
	assert.Equal(t, "keep_local", j.Resolution)
	assert.Equal(t, "user", j.ResolvedBy)
	assert.NotEmpty(t, j.ResolvedAt)
}

// --- newConflictsCmd ---

func TestNewConflictsCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newConflictsCmd()
	assert.Equal(t, "conflicts", cmd.Use)
	assert.NotNil(t, cmd.Flags().Lookup("history"))
}

// --- printConflictsJSON ---

func TestPrintConflictsJSON_EmptyList(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printConflictsJSON(&buf, nil)
	require.NoError(t, err)

	var result []conflictJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	assert.Empty(t, result)
}

func TestPrintConflictsJSON_WithConflicts(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "conflict-001",
			Path:         "/docs/readme.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
			LocalHash:    "local-hash",
			RemoteHash:   "remote-hash",
		},
		{
			ID:           "conflict-002",
			Path:         "/photos/cat.jpg",
			ConflictType: "delete_edit",
			DetectedAt:   1700000001000000000,
			Resolution:   "keep_local",
			ResolvedBy:   "user",
			ResolvedAt:   1700000002000000000,
		},
	}

	var buf bytes.Buffer
	err := printConflictsJSON(&buf, conflicts)
	require.NoError(t, err)

	var result []conflictJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result))
	require.Len(t, result, 2)
	assert.Equal(t, "conflict-001", result[0].ID)
	assert.Equal(t, "edit_edit", result[0].ConflictType)
	assert.Equal(t, "keep_local", result[1].Resolution)
}

// --- printConflictsTable ---

func TestPrintConflictsTable_Unresolved(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "abcdefghijklmnop",
			Path:         "/test.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
		},
	}

	var buf bytes.Buffer
	printConflictsTable(&buf, conflicts, false)

	output := buf.String()
	assert.Contains(t, output, "abcdefgh") // truncated ID
	assert.Contains(t, output, "/test.txt")
	assert.Contains(t, output, "edit_edit")
}

func TestPrintConflictsTable_History(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{
			ID:           "abcdefghijklmnop",
			Path:         "/test.txt",
			ConflictType: "edit_edit",
			DetectedAt:   1700000000000000000,
			Resolution:   "keep_local",
			ResolvedBy:   "user",
		},
	}

	var buf bytes.Buffer
	printConflictsTable(&buf, conflicts, true)

	output := buf.String()
	assert.Contains(t, output, "RESOLUTION")
	assert.Contains(t, output, "keep_local")
	assert.Contains(t, output, "user")
}
