package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// forgetMountFixture is a configured drive whose state records mounts as
// crossed mount points.
type forgetMountFixture struct {
	cid       driveid.CanonicalID
	syncDir   string
	statePath string
	cc        *CLIContext
	out       *bytes.Buffer
}

func newForgetMountFixture(t *testing.T, mounts ...string) *forgetMountFixture {
	t.Helper()

	setTestDriveHome(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:forget@example.com")
	syncDir := t.TempDir()
	require.NoError(t, config.AppendDriveSection(t.Context(), cfgPath, cid, syncDir))

	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	store, err := syncstore.NewSyncStore(t.Context(), statePath, testDriveLogger(t))
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
	seedMountBoundaries(t, statePath, mounts...)

	var out bytes.Buffer
	return &forgetMountFixture{
		cid:       cid,
		syncDir:   syncDir,
		statePath: statePath,
		out:       &out,
		cc: &CLIContext{
			Flags:        CLIFlags{Drive: []string{cid.String()}},
			Logger:       testDriveLogger(t),
			OutputWriter: &out,
			StatusWriter: &out,
			CfgPath:      cfgPath,
		},
	}
}

func (f *forgetMountFixture) recorded(t *testing.T) []string {
	t.Helper()

	recorded, err := syncstore.ReadMountBoundaries(t.Context(), f.statePath, testDriveLogger(t))
	require.NoError(t, err)
	return recorded
}

// A recorded mount point whose folder holds the disk's content as a plain
// folder of the sync folder's own disk is forgotten after confirmation, and
// only that one: the next sync reads it as an ordinary folder.
//
// Validates: R-6.2.18
func TestRunDriveForgetMount_ForgetsAPlainFolderWithContent(t *testing.T) {
	f := newForgetMountFixture(t, "nas", "photos")
	require.NoError(t, os.MkdirAll(filepath.Join(f.syncDir, "nas", "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(f.syncDir, "nas", "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(f.syncDir, "nas", "docs", "b.txt"), []byte("b"), 0o600))

	require.NoError(t, runDriveForgetMountWithInput(t.Context(), f.cc, bytes.NewBufferString(""), "nas", true))

	assert.Contains(t, f.out.String(), "nas of "+f.cid.String()+" is recorded as the mount point of another disk, "+
		"and holds 2 files on the sync folder's own disk now.")
	assert.Contains(t, f.out.String(), "The next sync for "+f.cid.String()+
		" reads nas as an ordinary folder and compares its 2 files with OneDrive.\n")
	assert.Equal(t, []string{"photos"}, f.recorded(t), "only the named mount point is forgotten")
	assert.FileExists(t, filepath.Join(f.syncDir, "nas", "a.txt"), "nothing is deleted")
}

// The absolute path of the folder names it as well, and --json reports it.
//
// Validates: R-6.2.18
func TestRunDriveForgetMount_TakesAnAbsolutePathAndReportsJSON(t *testing.T) {
	f := newForgetMountFixture(t, "nas")
	require.NoError(t, os.MkdirAll(filepath.Join(f.syncDir, "nas"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(f.syncDir, "nas", "a.txt"), []byte("a"), 0o600))
	f.cc.Flags.JSON = true
	var status bytes.Buffer
	f.cc.StatusWriter = &status

	require.NoError(t, runDriveForgetMountWithInput(t.Context(), f.cc, bytes.NewBufferString(""),
		filepath.Join(f.syncDir, "nas"), true))

	var decoded driveForgetMountJSONOutput
	require.NoError(t, json.Unmarshal(f.out.Bytes(), &decoded))
	assert.Equal(t, driveForgetMountJSONOutput{Drive: f.cid.String(), Path: "nas", Forgotten: true, FilesInFolder: 1}, decoded)
	assert.Empty(t, f.recorded(t))
}

// Forgetting is refused, and changes nothing, for a folder that is not a
// recorded mount point, one that is missing or empty (a disk that is not
// mounted, whose content the next sync would otherwise delete on OneDrive),
// one outside the sync folder, and without confirmation.
//
// Validates: R-6.2.18
func TestRunDriveForgetMount_RefusesWhatCouldLoseContent(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, f *forgetMountFixture)
		folder  string
		yes     bool
		want    string
	}{
		{
			name:   "not recorded",
			folder: "music",
			yes:    true,
			want:   "music is not a recorded mount point of personal:forget@example.com (recorded: nas); nothing was changed",
		},
		{
			name:   "missing",
			folder: "nas",
			yes:    true,
			want:   "nas is missing from the sync folder: the disk is not mounted",
		},
		{
			name: "empty",
			prepare: func(t *testing.T, f *forgetMountFixture) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(f.syncDir, "nas", "empty"), 0o750))
			},
			folder: "nas",
			yes:    true,
			want:   "nas holds no files: that is a disk that is not mounted",
		},
		{
			name:   "outside the sync folder",
			folder: "../nas",
			yes:    true,
			want:   "does not name a folder inside the sync folder",
		},
		{
			name: "not confirmed",
			prepare: func(t *testing.T, f *forgetMountFixture) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(f.syncDir, "nas"), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(f.syncDir, "nas", "a.txt"), []byte("a"), 0o600))
			},
			folder: "nas",
			want:   "forget-mount requires confirmation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newForgetMountFixture(t, "nas")
			if tt.prepare != nil {
				tt.prepare(t, f)
			}

			err := runDriveForgetMountWithInput(t.Context(), f.cc, bytes.NewBufferString("FORGET\n"), tt.folder, tt.yes)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Equal(t, []string{"nas"}, f.recorded(t), "nothing is forgotten")
		})
	}
}

// Validates: R-6.2.18
func TestRunDriveForgetMount_RefusesLiveSyncOwner(t *testing.T) {
	cc := liveSyncOwnerContext(t, "personal:forget@example.com")

	err := runDriveForgetMountWithInput(t.Context(), cc, bytes.NewBufferString(""), "nas", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot "+forgetMountAction+" while a sync owner is active")
}

// Validates: R-6.2.18
func TestNewDriveCmd_HasForgetMount(t *testing.T) {
	t.Parallel()

	cmd := newDriveCmd()
	forget, _, err := cmd.Find([]string{"forget-mount"})
	require.NoError(t, err)
	assert.Equal(t, "forget-mount", forget.Name())
	require.NotNil(t, forget.Flags().Lookup("yes"))
}

// seedMountBoundaries records paths as crossed mount boundaries in the closed
// state database at statePath, through a connection of the test's own: the
// store hands its database to no importer, and a scan that crosses a mount
// is more than this fixture needs.
func seedMountBoundaries(t *testing.T, statePath string, paths ...string) {
	t.Helper()

	db, err := sql.Open(syncstore.SqliteDriverName, syncstore.SqliteFileURI(statePath, nil))
	require.NoError(t, err)
	defer func() { assert.NoError(t, db.Close()) }()
	for _, path := range paths {
		_, err := db.ExecContext(t.Context(), `INSERT INTO mount_boundaries (path) VALUES (?)`, path)
		require.NoError(t, err)
	}
}
