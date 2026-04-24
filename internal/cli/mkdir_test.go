package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// Validates: R-1.5.1
func TestPrintMkdirJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printMkdirJSON(&buf, mkdirJSONOutput{Created: "projects/new-folder", ID: "folder-abc123"})
	require.NoError(t, err)

	var decoded mkdirJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "projects/new-folder", decoded.Created)
	assert.Equal(t, "folder-abc123", decoded.ID)
}

// Validates: R-1.5.1
func TestMkdirStartParentID(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "root", mkdirStartParentID(nil))
	assert.Equal(t, "root", mkdirStartParentID(driveops.NewMountSession(&driveops.Session{}, "")))
	assert.Equal(t, "root", mkdirStartParentID(driveops.NewMountSession(&driveops.Session{}, "root")))
	assert.Equal(t, "mount-root-id", mkdirStartParentID(driveops.NewMountSession(&driveops.Session{}, "mount-root-id")))
}
