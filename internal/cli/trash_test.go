package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
)

// seedTrashEntry puts one file into the trash of the drive whose state lives
// at statePath, as sync would after deleting syncRoot/rel, and returns its ID.
func seedTrashEntry(t *testing.T, statePath string, syncRoot string, rel string) string {
	t.Helper()

	trash := localtrash.Open(localtrash.DirForStatePath(statePath), nil)
	placement, err := trash.Reserve(syncRoot, rel, localtrash.ReasonDeleted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(placement.ItemPath, []byte("kept\n"), 0o600))
	entry, err := placement.Commit()
	require.NoError(t, err)

	return entry.ID
}

func newTrashTestContext(t *testing.T) (*CLIContext, *bytes.Buffer, driveid.CanonicalID) {
	t.Helper()
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))

	var out bytes.Buffer
	return &CLIContext{
		Flags:        CLIFlags{Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
	}, &out, cid
}

// Validates: R-6.4.9
func TestTrashCommands_ListRestoreAndEmpty(t *testing.T) {
	cc, out, cid := newTrashTestContext(t)
	syncRoot := t.TempDir()
	id := seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "Docs/report.txt")
	seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "other.txt")

	require.NoError(t, runTrashList(t.Context(), cc))
	assert.Contains(t, out.String(), "deleted Docs/report.txt")
	assert.Contains(t, out.String(), id)

	out.Reset()
	require.NoError(t, runTrashRestore(t.Context(), cc, id))
	data, err := os.ReadFile(filepath.Join(syncRoot, "Docs", "report.txt"))
	require.NoError(t, err)
	assert.Equal(t, "kept\n", string(data))

	err = runTrashRestore(t.Context(), cc, id)
	require.ErrorIs(t, err, localtrash.ErrEntryNotFound, "a restored entry leaves the trash")

	require.Error(t, runTrashEmpty(t.Context(), cc, false), "emptying needs --confirm")
	out.Reset()
	require.NoError(t, runTrashEmpty(t.Context(), cc, true))
	assert.Contains(t, out.String(), "Permanently deleted 1 files")
}

// Validates: R-6.4.9
func TestTrashRestore_NeverOverwrites(t *testing.T) {
	cc, _, cid := newTrashTestContext(t)
	syncRoot := t.TempDir()
	id := seedTrashEntry(t, config.DriveStatePath(cid), syncRoot, "a.txt")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "a.txt"), []byte("current\n"), 0o600))

	err := runTrashRestore(t.Context(), cc, id)
	require.ErrorIs(t, err, localtrash.ErrRestoreTargetExists)
	data, readErr := os.ReadFile(filepath.Join(syncRoot, "a.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "current\n", string(data))
}
