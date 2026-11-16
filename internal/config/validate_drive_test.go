package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestValidateDrives_NoDrives_Valid(t *testing.T) {
	cfg := DefaultConfig()
	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_ValidDrive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_ValidDriveFilterConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{
		SyncDir: "~/OneDrive",
		DriveFilterConfig: DriveFilterConfig{
			IgnoredDirs:     []string{"node_modules", "build/cache"},
			IncludedDirs:    []string{"Projects"},
			IgnoredPaths:    []string{"*.log", "tmp/*"},
			IgnoreDotfiles:  true,
			IgnoreJunkFiles: new(true),
			FollowSymlinks:  true,
		},
	}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_FilterDirsMustBeNormalizedRootRelativeExactPaths(t *testing.T) {
	tests := []struct {
		name  string
		drive Drive
		want  string
	}{
		{
			name:  "absolute ignored dir",
			drive: Drive{DriveFilterConfig: DriveFilterConfig{IgnoredDirs: []string{"/node_modules"}}},
			want:  "root-relative",
		},
		{
			name:  "parent ignored dir",
			drive: Drive{DriveFilterConfig: DriveFilterConfig{IgnoredDirs: []string{"../secret"}}},
			want:  "..",
		},
		{
			name:  "root included dir",
			drive: Drive{DriveFilterConfig: DriveFilterConfig{IncludedDirs: []string{"."}}},
			want:  "sync root",
		},
		{
			name:  "glob included dir",
			drive: Drive{DriveFilterConfig: DriveFilterConfig{IncludedDirs: []string{"Projects/*"}}},
			want:  "exact",
		},
		{
			name:  "unnormalized ignored dir",
			drive: Drive{DriveFilterConfig: DriveFilterConfig{IgnoredDirs: []string{"build/../dist"}}},
			want:  "..",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = tt.drive

			err := Validate(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestValidateDrives_IgnoredPathsMustBeNormalizedRootRelativeGlobs(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{name: "empty", pattern: "", want: "empty"},
		{name: "absolute", pattern: "/tmp/*", want: "root-relative"},
		{name: "root", pattern: ".", want: "sync root"},
		{name: "parent", pattern: "../secret", want: ".."},
		{name: "unnormalized", pattern: "a/../b", want: ".."},
		{name: "invalid glob", pattern: "[", want: "invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{
				DriveFilterConfig: DriveFilterConfig{IgnoredPaths: []string{tt.pattern}},
			}

			err := Validate(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestValidateDrives_MultipleDrives_Valid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_EmptySyncDir_Valid(t *testing.T) {
	// Empty sync_dir is valid — runtime defaults computed in buildResolvedDrive().
	// This supports minimal drive sections during initial setup.
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{}

	err := Validate(cfg)
	assert.NoError(t, err)
}

// Validates: R-4.8.3
func TestValidateDrives_DuplicateSyncDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/OneDrive"}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same sync_dir")
}

// Validates: R-4.8.3
func TestValidateDrives_DuplicateSyncDirTildeExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: filepath.Join(home, "OneDrive")}

	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same sync_dir")
}

func TestValidateDrives_SharePointDrive_Valid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")] = Drive{
		SyncDir: "~/Marketing",
	}

	err := Validate(cfg)
	assert.NoError(t, err)
}

// --- Sync dir overlap tests ---

// Validates: R-4.8.3
func TestValidateDrives_ChildIsSubdir_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/OneDrive/Work"}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlap")
}

// Validates: R-4.8.3
func TestValidateDrives_ParentIsSubdir_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive/Personal"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/OneDrive"}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlap")
}

func TestValidateDrives_SamePrefixDifferentDir_OK(t *testing.T) {
	// /OneDrive vs /OneDriveBackup should NOT be flagged as overlapping.
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/OneDriveBackup"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_SiblingDirs_OK(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_EmptySyncDirSkippedForOverlap(t *testing.T) {
	// Empty sync_dir drives are skipped — no overlap possible.
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{}

	err := Validate(cfg)
	assert.NoError(t, err)
}

// Validates: R-4.8.3
func TestValidateDrives_SymlinkedSyncDir_DetectedAsDuplicate(t *testing.T) {
	// Two drives pointing to the same directory via a symlink should be detected.
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	linkDir := filepath.Join(dir, "link")

	require.NoError(t, os.MkdirAll(realDir, 0o700))
	require.NoError(t, os.Symlink(realDir, linkDir))

	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: realDir}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: linkDir}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "same sync_dir")
}

// Validates: R-4.8.3
//
// The filesystem root is above every other folder, so it overlaps every other
// drive's sync_dir.
func TestValidateDrives_FilesystemRootOverlapsEveryOtherSyncDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:a@example.com")] = Drive{SyncDir: "/"}
	cfg.Drives[driveid.MustCanonicalID("personal:b@example.com")] = Drive{SyncDir: "/nonexistent/OneDrive"}

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_dir overlap")
}

// The drive and the three relations the refusals of a sync_dir that meets a
// managed folder name, as the tests below expect them.
const (
	managedFolderTestDrive = "personal:a@example.com"
	refusalSyncDirHolds    = " contains "
	refusalSyncDirIs       = " is "
	refusalSyncDirInside   = " lies inside "
)

// Validates: R-4.8.3
//
// A sync_dir and a folder onedrive-go keeps for itself (its data folder, with
// the sync state, the sign-ins and the trash, and its configuration folder)
// may not be the same folder or hold one another: the client would sync its
// own state, or move the files it trashes into a tree it syncs. The refusal
// names both paths, which holds which, and what to change. A folder beside
// them is accepted.
func TestValidateDrives_SyncDirAndManagedFolderHoldingOneAnotherAreRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	dataDir := canonicalSyncDirPath(DefaultDataDir())
	configDir := canonicalSyncDirPath(DefaultConfigDir())

	for _, tc := range []struct {
		name    string
		syncDir string
		refused []string
	}{
		{name: "above both", syncDir: home, refused: []string{refusalSyncDirHolds + dataDir, refusalSyncDirHolds + configDir}},
		{name: "the filesystem root", syncDir: "/", refused: []string{refusalSyncDirHolds + dataDir, refusalSyncDirHolds + configDir}},
		{name: "the data folder itself", syncDir: DefaultDataDir(), refused: []string{refusalSyncDirIs + dataDir}},
		{name: "above the configuration folder only", syncDir: filepath.Join(home, "config"), refused: []string{refusalSyncDirHolds + configDir}},
		{name: "the trash inside the data folder", syncDir: filepath.Join(DefaultDataDir(), "trash"), refused: []string{refusalSyncDirInside + dataDir}},
		{name: "inside the configuration folder", syncDir: filepath.Join(DefaultConfigDir(), "drives"), refused: []string{refusalSyncDirInside + configDir}},
		{name: "beside them", syncDir: filepath.Join(home, "OneDrive")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireManagedFolderRefusals(t, tc.syncDir, tc.refused)
		})
	}
}

// Validates: R-4.8.3
//
// On a volume that ignores letter case, a sync_dir spelled in another case
// than a folder is still that folder: the check compares the folders
// themselves, not only their spelling, and needs only the folder whose
// identity it compares to exist. So a sync_dir in another case above the data
// folder holds it whether the data folder exists yet or not, and a sync_dir
// not made yet inside the data folder, spelled in another case, lies inside
// it. The test runs where the temporary volume ignores case (macOS by
// default) and is skipped where it does not.
func TestValidateDrives_SyncDirSpelledInAnotherCaseStillMeetsTheDataFolder(t *testing.T) {
	for _, tc := range []struct {
		name             string
		dataFolderExists bool
		insideDataFolder bool
	}{
		{name: "above an existing data folder", dataFolderExists: true},
		{name: "above a data folder not made yet"},
		{name: "not made yet, inside the data folder", dataFolderExists: true, insideDataFolder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "Home")
			require.NoError(t, os.Mkdir(home, 0o700))
			homeInAnotherCase := filepath.Join(base, "home")
			if _, err := os.Stat(homeInAnotherCase); err != nil {
				t.Skip("the temporary volume tells letter case apart")
			}
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
			if tc.dataFolderExists {
				require.NoError(t, os.MkdirAll(DefaultDataDir(), 0o700))
			}
			dataFolder := canonicalSyncDirPath(DefaultDataDir())

			if !tc.insideDataFolder {
				requireManagedFolderRefusals(t, homeInAnotherCase, []string{refusalSyncDirHolds + dataFolder})
				return
			}
			dataBelowHome, err := filepath.Rel(home, DefaultDataDir())
			require.NoError(t, err)
			requireManagedFolderRefusals(t, filepath.Join(homeInAnotherCase, dataBelowHome, "incoming"),
				[]string{refusalSyncDirInside + dataFolder})
		})
	}
}

// Validates: R-4.8.3
//
// nestingOf compares what the folders are and needs only the folder whose
// identity it compares to exist. A second name for a folder, a link here
// standing in for another spelling on a volume that ignores letter case,
// holds a path below the folder that does not exist yet, and a path not made
// yet below that name lies inside the folder. Two paths that do not exist
// are compared by text alone.
func TestNestingOf_ComparesAnExistingFolderWithAPathNotMadeYet(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	folder := filepath.Join(base, "folder")
	require.NoError(t, os.Mkdir(folder, 0o700))
	otherName := filepath.Join(base, "other-name")
	require.NoError(t, os.Symlink(folder, otherName))
	belowFolder := filepath.Join(folder, "not-made-yet")

	assert.Equal(t, firstFolderHoldsSecond, nestingOf(otherName, belowFolder))
	assert.Equal(t, secondFolderHoldsFirst, nestingOf(filepath.Join(otherName, "inbox"), folder))
	assert.Equal(t, foldersSame, nestingOf(otherName, folder))
	assert.Equal(t, foldersApart, nestingOf(filepath.Join(base, "apart"), belowFolder))
}

// requireManagedFolderRefusals validates a config whose one drive has syncDir
// and requires one refusal per entry of refused, in order, each naming the
// drive and the sync_dir followed by the entry (a relation and a managed
// folder) and saying what to change.
func requireManagedFolderRefusals(t *testing.T, syncDir string, refused []string) {
	t.Helper()

	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID(managedFolderTestDrive)] = Drive{SyncDir: syncDir}

	errs := validateDrives(cfg)
	require.Len(t, errs, len(refused))
	for i, refusal := range refused {
		text := errs[i].Error()
		assert.Contains(t, text, `drive "`+managedFolderTestDrive+`" sync_dir `+canonicalSyncDirPath(syncDir)+refusal)
		assert.Contains(t, text, "; set sync_dir to a folder ")
	}
}

// Validates: R-4.8.3
//
// A drive without sync_dir syncs into its default folder, so the check runs
// on that folder when the drive is resolved for sync: here the data folder
// lies inside the default ~/OneDrive, and syncing the drive is refused.
func TestValidateResolvedForSync_DefaultSyncDirHoldingTheDataFolderIsRefused(t *testing.T) {
	home := setConfigTestHome(t)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "OneDrive", ".local", "share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	path := writeTestConfig(t, "[\""+managedFolderTestDrive+"\"]\n")

	resolved, _, err := ResolveDrive(EnvOverrides{ConfigPath: path}, CLIOverrides{}, testLogger(t))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "OneDrive"), resolved.SyncDir)

	err = ValidateResolvedForSync(resolved)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, `drive "`+managedFolderTestDrive+`" sync_dir `+
		canonicalSyncDirPath(resolved.SyncDir)+refusalSyncDirHolds+canonicalSyncDirPath(DefaultDataDir()))
}

func TestValidateDrives_NonexistentSyncDir_NoPanic(t *testing.T) {
	// A sync_dir that doesn't exist yet should not panic — falls back to lexical comparison.
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "/nonexistent/path/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "/other/nonexistent/Work"}

	err := Validate(cfg)
	assert.NoError(t, err)
}

func TestValidateDrives_InvalidCanonicalID_RejectedAtParseTime(t *testing.T) {
	// Invalid canonical IDs are rejected at parse time (driveSectionDecoder),
	// not at validation time. This test verifies that the TOML load path catches them.
	// The key must contain ":" to be recognized as a drive section.
	path := writeTestConfig(t, `
["badtype:user@example.com"]
sync_dir = "~/OneDrive"
`)
	_, err := LoadOrDefault(path, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid canonical ID")
}

// Validates: R-2.6.5
func TestValidateDrives_UnparseablePausedUntilIsReported(t *testing.T) {
	t.Parallel()

	bad := "tomorrow"
	good := "2026-10-24T15:00:00Z"
	cfg := &Config{Drives: map[driveid.CanonicalID]Drive{
		driveid.MustCanonicalID("personal:a@example.com"): {SyncDir: "~/A", PausedUntil: &bad},
		driveid.MustCanonicalID("personal:b@example.com"): {SyncDir: "~/B", PausedUntil: &good},
	}}

	errs := validateDrives(cfg)
	require.Len(t, errs, 1, "a corrupt paused_until must not stay a silent indefinite pause")
	assert.Contains(t, errs[0].Error(), `paused_until "tomorrow"`)
	assert.Contains(t, errs[0].Error(), "personal:a@example.com")
}
