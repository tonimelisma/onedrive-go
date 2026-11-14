package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	enrollTestOwnedDrive   = "business:enroll@example.com"
	enrollTestSharedOwner  = "personal:enroll@example.com"
	enrollTestSharedDrive  = "shared:enroll@example.com:driveX:itemY"
	enrollTestLoginAccount = "personal:login@example.com"
	enrollTestOtherDrive   = "business:other@contoso.com"
	enrollTestOrgName      = "Contoso"
	enrollTestSharedName   = "Shared Folder"
	enrollTestUserID       = "user-123"
	enrollTestUserName     = "Login User"
	enrollTestPrimaryID    = "drv-primary"
	enrollTestDirPerms     = 0o700
	enrollTestWorkers      = 6
	enrollResultPrefix     = "enroll-result:"
	enrollOutcomeAdded     = "added"
)

var errInjected = errors.New("injected failure")

// enrollFixture is one home directory with a config directory and a data
// directory. With files, the config already holds another drive and the
// catalog knows every account the tests enroll for, so "unchanged" is never
// the trivial empty file.
type enrollFixture struct {
	configPath  string
	catalogPath string
	dataDir     string
}

func newEnrollFixture(t *testing.T, withFiles bool) enrollFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	root := t.TempDir()
	f := enrollFixture{
		configPath: filepath.Join(root, "config", "config.toml"),
		dataDir:    filepath.Join(root, "data"),
	}
	f.catalogPath = CatalogPathForDataDir(f.dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(f.configPath), enrollTestDirPerms))
	require.NoError(t, os.MkdirAll(f.dataDir, enrollTestDirPerms))

	if withFiles {
		other := driveid.MustCanonicalID(enrollTestOtherDrive)
		require.NoError(t, AppendDriveSection(f.configPath, other, filepath.Join(root, "Other")))
		require.NoError(t, UpdateCatalogForDataDir(f.dataDir, func(catalog *Catalog) error {
			for _, raw := range []string{enrollTestOwnedDrive, enrollTestSharedOwner, enrollTestLoginAccount} {
				cid := driveid.MustCanonicalID(raw)
				catalog.UpsertAccount(&CatalogAccount{
					CanonicalID: cid.String(),
					Email:       cid.Email(),
					DriveType:   cid.DriveType(),
					OrgName:     enrollTestOrgName,
				})
			}
			return nil
		}))
	}

	return f
}

func makeSyncDirUnderHome(syncDir string) error {
	if err := localpath.MkdirAll(ExpandTilde(syncDir), enrollTestDirPerms); err != nil {
		return fmt.Errorf("creating %s: %w", syncDir, err)
	}

	return nil
}

func (f enrollFixture) request(drive string) AddDriveRequest {
	return AddDriveRequest{
		ConfigPath:  f.configPath,
		DataDir:     f.dataDir,
		Drive:       driveid.MustCanonicalID(drive),
		MakeSyncDir: makeSyncDirUnderHome,
	}
}

func (f enrollFixture) sharedRequest() AddDriveRequest {
	req := f.request(enrollTestSharedDrive)
	req.Owner = driveid.MustCanonicalID(enrollTestSharedOwner)
	req.DisplayName = enrollTestSharedName

	return req
}

func (f enrollFixture) loginRequest() AddDriveRequest {
	req := f.request(enrollTestLoginAccount)
	req.Login = &AccountLogin{
		UserID:         enrollTestUserID,
		DisplayName:    enrollTestUserName,
		OrgName:        "",
		PrimaryDriveID: driveid.New(enrollTestPrimaryID),
	}

	return req
}

// enrollKind is one of the three callers of AddDrive.
type enrollKind struct {
	name    string
	request func(f enrollFixture) AddDriveRequest
}

func enrollKinds() []enrollKind {
	return []enrollKind{
		{name: "own drive", request: func(f enrollFixture) AddDriveRequest { return f.request(enrollTestOwnedDrive) }},
		{name: "shared drive", request: enrollFixture.sharedRequest},
		{name: "login", request: enrollFixture.loginRequest},
	}
}

// fileState is one managed file as the test saw it: its bytes, or absent.
type fileState struct {
	data   []byte
	exists bool
}

func readFileState(t *testing.T, path string) fileState {
	t.Helper()

	data, err := localpath.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{}
	}
	require.NoError(t, err)

	return fileState{data: data, exists: true}
}

// noWaitLocker is the production locker with its pause between lock attempts
// replaced, so a held lock runs out the bounded wait at once.
func noWaitLocker() configLocker {
	locker := defaultConfigLocker()
	locker.sleep = func(time.Duration) {}

	return locker
}

func failWritesTo(next func(string, []byte) error, failPath string) func(string, []byte) error {
	return func(path string, data []byte) error {
		if path == failPath {
			return errInjected
		}

		return next(path, data)
	}
}

// Validates: R-3.3.5, R-3.3.1, R-4.4.4
func TestAddDrive_FailureAtEachStepLeavesConfigAndCatalogUnchanged(t *testing.T) {
	steps := []struct {
		name    string
		inject  func(t *testing.T, f enrollFixture, req *AddDriveRequest, io *configIO)
		wantErr error
	}{
		{
			name: "the config lock is held by another process",
			inject: func(t *testing.T, f enrollFixture, _ *AddDriveRequest, _ *configIO) {
				t.Helper()
				holdDirectoryLock(t, filepath.Dir(f.configPath))
			},
			wantErr: ErrConfigLocked,
		},
		{
			name: "the catalog lock is held by another process",
			inject: func(t *testing.T, f enrollFixture, _ *AddDriveRequest, _ *configIO) {
				t.Helper()
				holdDirectoryLock(t, f.dataDir)
			},
			wantErr: ErrConfigLocked,
		},
		{
			name: "the sync directory cannot be created",
			inject: func(_ *testing.T, _ enrollFixture, req *AddDriveRequest, _ *configIO) {
				req.MakeSyncDir = func(string) error { return errInjected }
			},
			wantErr: errInjected,
		},
		{
			name: "the catalog cannot be written",
			inject: func(_ *testing.T, f enrollFixture, _ *AddDriveRequest, io *configIO) {
				io.writeManagedFile = failWritesTo(io.writeManagedFile, f.catalogPath)
			},
			wantErr: errInjected,
		},
		{
			name: "the config cannot be written",
			inject: func(_ *testing.T, f enrollFixture, _ *AddDriveRequest, io *configIO) {
				io.writeManagedFile = failWritesTo(io.writeManagedFile, f.configPath)
			},
			wantErr: errInjected,
		},
	}

	for _, withFiles := range []bool{true, false} {
		for _, kind := range enrollKinds() {
			for _, step := range steps {
				name := kind.name + "/" + step.name
				if !withFiles {
					name += " (no config or catalog yet)"
				}
				t.Run(name, func(t *testing.T) {
					f := newEnrollFixture(t, withFiles)
					configBefore := readFileState(t, f.configPath)
					catalogBefore := readFileState(t, f.catalogPath)

					req := kind.request(f)
					io := defaultConfigIO()
					step.inject(t, f, &req, &io)

					_, err := addDrive(&req, io, noWaitLocker(), time.Now(), testLogger(t))
					require.ErrorIs(t, err, step.wantErr)

					assert.Equal(t, configBefore, readFileState(t, f.configPath), "config changed")
					assert.Equal(t, catalogBefore, readFileState(t, f.catalogPath), "catalog changed")
				})
			}
		}
	}
}

// Validates: R-3.3.5
func TestAddDrive_FailedCatalogRestoreLeavesWhatARerunCompletes(t *testing.T) {
	f := newEnrollFixture(t, true)
	configBefore := readFileState(t, f.configPath)
	req := f.request(enrollTestOwnedDrive)

	io := defaultConfigIO()
	catalogWrites := 0
	writeFile := io.writeManagedFile
	io.writeManagedFile = func(path string, data []byte) error {
		switch path {
		case f.configPath:
			return errInjected
		case f.catalogPath:
			catalogWrites++
			if catalogWrites > 1 {
				return fmt.Errorf("restore: %w", fs.ErrPermission)
			}
		}

		return writeFile(path, data)
	}

	_, err := addDrive(&req, io, noWaitLocker(), time.Now(), testLogger(t))
	require.ErrorIs(t, err, errInjected)
	require.ErrorIs(t, err, fs.ErrPermission, "the failed restore must be reported with the first error")
	assert.Contains(t, err.Error(), "putting the catalog back")

	// What is left is the state a kill after the catalog write leaves.
	assert.Equal(t, configBefore, readFileState(t, f.configPath))
	catalog, err := LoadCatalogForDataDir(f.dataDir)
	require.NoError(t, err)
	_, found := catalog.DriveByCanonicalID(req.Drive)
	require.True(t, found)

	result, err := AddDrive(&req, testLogger(t))
	require.NoError(t, err)
	assert.True(t, result.Added)
	assertEnrollmentComplete(t, f, &req)
}

// assertEnrollmentComplete checks the end state every enrollment reaches: a
// section with sync_dir (and a shared drive's display_name), the catalog
// records, and the sync directory.
func assertEnrollmentComplete(t *testing.T, f enrollFixture, req *AddDriveRequest) {
	t.Helper()

	cfg := loadWrittenConfig(t, f.configPath)
	section, found := cfg.Drives[req.Drive]
	require.True(t, found, "no section for %s", req.Drive)
	require.NotEmpty(t, section.SyncDir)
	if req.Drive.IsShared() {
		assert.NotEmpty(t, section.DisplayName)
	}

	catalog, err := LoadCatalogForDataDir(f.dataDir)
	require.NoError(t, err)
	_, found = catalog.DriveByCanonicalID(req.Drive)
	assert.True(t, found, "no catalog record for %s", req.Drive)
	if req.Login != nil {
		_, found = catalog.AccountByCanonicalID(req.Drive)
		assert.True(t, found, "no catalog account for %s", req.Drive)
	}

	info, err := os.Stat(ExpandTilde(section.SyncDir))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// Validates: R-3.3.5, R-3.3.1
func TestAddDrive_RerunCompletesEachPartialState(t *testing.T) {
	cases := []struct {
		name          string
		request       func(f enrollFixture) AddDriveRequest
		leave         func(t *testing.T, f enrollFixture, req *AddDriveRequest)
		wantAdded     bool
		wantCompleted bool
	}{
		{
			name:    "a section without its catalog record",
			request: func(f enrollFixture) AddDriveRequest { return f.request(enrollTestOwnedDrive) },
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				enrollThenForget(t, req, func(catalog *Catalog) { catalog.DeleteDrive(req.Drive) })
			},
			wantCompleted: true,
		},
		{
			name:    "a shared section without display_name",
			request: enrollFixture.sharedRequest,
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				enrollThenForget(t, req, nil)
				require.NoError(t, DeleteDriveKey(f.configPath, req.Drive, displayNameKey))
			},
			wantCompleted: true,
		},
		{
			name:    "a section without sync_dir",
			request: func(f enrollFixture) AddDriveRequest { return f.request(enrollTestOwnedDrive) },
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				enrollThenForget(t, req, nil)
				require.NoError(t, DeleteDriveKey(f.configPath, req.Drive, syncDirKey))
			},
			wantCompleted: true,
		},
		{
			name:    "a section whose sync directory is gone",
			request: func(f enrollFixture) AddDriveRequest { return f.request(enrollTestOwnedDrive) },
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				result := enrollThenForget(t, req, nil)
				require.NoError(t, os.Remove(ExpandTilde(result.SyncDir)))
			},
			wantCompleted: true,
		},
		{
			name:    "a login whose account record is missing",
			request: enrollFixture.loginRequest,
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				enrollThenForget(t, req, func(catalog *Catalog) { catalog.DeleteAccount(req.Drive) })
			},
			wantCompleted: true,
		},
		{
			name:    "a catalog record without a section",
			request: func(f enrollFixture) AddDriveRequest { return f.request(enrollTestOwnedDrive) },
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				enrollThenForget(t, req, nil)
				require.NoError(t, DeleteDriveSection(f.configPath, req.Drive))
			},
			wantAdded: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnrollFixture(t, true)
			req := tc.request(f)
			tc.leave(t, f, &req)

			result, err := AddDrive(&req, testLogger(t))
			require.NoError(t, err)
			assert.Equal(t, tc.wantAdded, result.Added, "added")
			assert.Equal(t, tc.wantCompleted, result.Completed, "completed")
			assertEnrollmentComplete(t, f, &req)
		})
	}
}

// enrollThenForget enrolls req completely and then, through forget, removes
// catalog records, leaving a state a hand edit or an earlier version of the
// client leaves; AddDrive itself cannot leave it.
func enrollThenForget(t *testing.T, req *AddDriveRequest, forget func(*Catalog)) AddDriveResult {
	t.Helper()

	result, err := AddDrive(req, testLogger(t))
	require.NoError(t, err)
	require.True(t, result.Added)

	if forget != nil {
		require.NoError(t, UpdateCatalogForDataDir(req.DataDir, func(catalog *Catalog) error {
			forget(catalog)
			return nil
		}))
	}

	return result
}

// Validates: R-3.3.5
func TestAddDrive_CompleteEnrollmentWritesNothing(t *testing.T) {
	for _, kind := range enrollKinds() {
		t.Run(kind.name, func(t *testing.T) {
			f := newEnrollFixture(t, true)
			req := kind.request(f)
			first, err := AddDrive(&req, testLogger(t))
			require.NoError(t, err)
			require.True(t, first.Added)

			io := defaultConfigIO()
			var written []string
			writeFile := io.writeManagedFile
			io.writeManagedFile = func(path string, data []byte) error {
				written = append(written, path)
				return writeFile(path, data)
			}
			// A login refreshes what it learned, so only its config stays as it is.
			again, err := addDrive(&req, io, noWaitLocker(), time.Now(), testLogger(t))
			require.NoError(t, err)

			assert.False(t, again.Added)
			assert.False(t, again.Completed)
			assert.Equal(t, first.SyncDir, again.SyncDir)
			assert.NotContains(t, written, f.configPath)
			if req.Login == nil {
				assert.Empty(t, written)
			}
		})
	}
}

// Validates: R-3.3.5
func TestAddDrive_SharedSectionIsOneWrite(t *testing.T) {
	cases := []struct {
		name  string
		leave func(t *testing.T, f enrollFixture, req *AddDriveRequest)
	}{
		{name: "a new section", leave: func(*testing.T, enrollFixture, *AddDriveRequest) {}},
		{
			name: "a section that lacks sync_dir and display_name",
			leave: func(t *testing.T, f enrollFixture, req *AddDriveRequest) {
				t.Helper()
				enrollThenForget(t, req, nil)
				require.NoError(t, DeleteDriveKey(f.configPath, req.Drive, syncDirKey))
				require.NoError(t, DeleteDriveKey(f.configPath, req.Drive, displayNameKey))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnrollFixture(t, true)
			req := f.sharedRequest()
			tc.leave(t, f, &req)

			io := defaultConfigIO()
			var configWrites [][]byte
			writeFile := io.writeManagedFile
			io.writeManagedFile = func(path string, data []byte) error {
				if path == f.configPath {
					configWrites = append(configWrites, bytes.Clone(data))
				}
				return writeFile(path, data)
			}

			_, err := addDrive(&req, io, noWaitLocker(), time.Now(), testLogger(t))
			require.NoError(t, err)

			require.Len(t, configWrites, 1, "the section must be written once")
			cfg, err := newConfigLoader(defaultConfigIO()).parseStrict(f.configPath, configWrites[0])
			require.NoError(t, err)
			assert.Equal(t, enrollTestSharedName, cfg.Drives[req.Drive].DisplayName)
			assert.NotEmpty(t, cfg.Drives[req.Drive].SyncDir)
		})
	}
}

// Validates: R-4.4.4
func TestAddDrive_SameDirectoryForConfigAndDataDoesNotSelfDeadlock(t *testing.T) {
	cases := []struct {
		name    string
		dataDir func(t *testing.T, configDir string) string
	}{
		{name: "the same path", dataDir: func(_ *testing.T, configDir string) string { return configDir }},
		{
			name: "a symlink to the config directory",
			dataDir: func(t *testing.T, configDir string) string {
				t.Helper()
				link := filepath.Join(t.TempDir(), "data")
				require.NoError(t, os.Symlink(configDir, link))
				return link
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			configDir := t.TempDir()
			req := AddDriveRequest{
				ConfigPath:  filepath.Join(configDir, "config.toml"),
				DataDir:     tc.dataDir(t, configDir),
				Drive:       driveid.MustCanonicalID(enrollTestOwnedDrive),
				MakeSyncDir: makeSyncDirUnderHome,
			}

			waits := 0
			locker := defaultConfigLocker()
			locker.sleep = func(time.Duration) { waits++ }

			result, err := addDrive(&req, defaultConfigIO(), locker, time.Now(), testLogger(t))
			require.NoError(t, err)
			assert.Zero(t, waits, "the enrollment waited on its own lock")
			assert.True(t, result.Added)

			catalog, err := LoadCatalogForDataDir(configDir)
			require.NoError(t, err)
			_, found := catalog.DriveByCanonicalID(req.Drive)
			assert.True(t, found)
		})
	}
}

// Validates: R-3.3.5
func TestAddDrive_CrossProcessSameDriveAddsOneSection(t *testing.T) {
	f := newEnrollFixture(t, true)
	drive := driveid.MustCanonicalID(enrollTestOwnedDrive)

	type worker struct {
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		output *bytes.Buffer
	}

	workers := make([]worker, 0, enrollTestWorkers)
	for range enrollTestWorkers {
		//nolint:gosec // Fixed worker subprocess uses the current test binary, a fixed test name, and controlled env.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAddDrive_CrossProcessWorker$")
		cmd.Env = append(os.Environ(),
			"ONEDRIVE_GO_ENROLL_WORKER=1",
			"ONEDRIVE_GO_ENROLL_CONFIG="+f.configPath,
			"ONEDRIVE_GO_ENROLL_DATA="+f.dataDir,
			"ONEDRIVE_GO_ENROLL_DRIVE="+drive.String(),
		)
		output := &bytes.Buffer{}
		cmd.Stdout = output
		cmd.Stderr = output

		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		workers = append(workers, worker{cmd: cmd, stdin: stdin, output: output})
	}

	// Every worker waits for its stdin to close, so they all start together.
	for _, w := range workers {
		require.NoError(t, w.stdin.Close())
	}

	added := 0
	for _, w := range workers {
		require.NoError(t, w.cmd.Wait(), w.output.String())
		if strings.Contains(w.output.String(), enrollResultPrefix+enrollOutcomeAdded) {
			added++
		}
	}
	assert.Equal(t, 1, added, "exactly one process adds the drive")

	data, err := localpath.ReadFile(f.configPath)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), renderSectionHeaderLine(drive.String())))
	loadWrittenConfig(t, f.configPath) // the config must still load
}

func TestAddDrive_CrossProcessWorker(t *testing.T) {
	if os.Getenv("ONEDRIVE_GO_ENROLL_WORKER") == "" {
		return
	}

	_, err := io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)

	result, err := AddDrive(&AddDriveRequest{
		ConfigPath:  os.Getenv("ONEDRIVE_GO_ENROLL_CONFIG"),
		DataDir:     os.Getenv("ONEDRIVE_GO_ENROLL_DATA"),
		Drive:       driveid.MustCanonicalID(os.Getenv("ONEDRIVE_GO_ENROLL_DRIVE")),
		MakeSyncDir: makeSyncDirUnderHome,
	}, testLogger(t))
	require.NoError(t, err)

	outcome := "unchanged"
	if result.Added {
		outcome = enrollOutcomeAdded
	}
	_, err = fmt.Fprintln(os.Stdout, enrollResultPrefix+outcome)
	require.NoError(t, err)
}

// Validates: R-4.2.1, R-3.3.1
func TestAddDrive_FirstEnrollmentCreatesTheConfigFromTheTemplate(t *testing.T) {
	f := newEnrollFixture(t, false)

	req := f.loginRequest()
	result, err := AddDrive(&req, testLogger(t))
	require.NoError(t, err)
	assert.True(t, result.Added)
	assert.Equal(t, "~/OneDrive", result.SyncDir)

	data, err := localpath.ReadFile(f.configPath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(data), defaultConfigTemplate()), "the template comes first")
	assert.Contains(t, string(data), renderSectionHeaderLine(enrollTestLoginAccount))
}

// Validates: R-3.3.5, R-3.3.1
func TestAddDrive_ChoosesTheSyncDirFromTheLockedSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		drive   string
		seed    func(t *testing.T, f enrollFixture)
		wantDir string
	}{
		{
			name:    "a business drive takes its organization's name",
			drive:   enrollTestOwnedDrive,
			wantDir: "~/OneDrive - " + enrollTestOrgName,
		},
		{
			name:    "a business drive whose account the catalog does not know",
			drive:   "business:noorg@example.com",
			wantDir: "~/OneDrive - Business",
		},
		{
			name:  "a second personal drive steps around the first one's folder",
			drive: enrollTestLoginAccount,
			seed: func(t *testing.T, f enrollFixture) {
				t.Helper()
				require.NoError(t, AppendDriveSection(f.configPath, driveid.MustCanonicalID(enrollTestSharedOwner), "~/OneDrive"))
			},
			wantDir: "~/OneDrive - login@example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnrollFixture(t, true)
			if tc.seed != nil {
				tc.seed(t, f)
			}

			req := f.request(tc.drive)
			result, err := AddDrive(&req, testLogger(t))
			require.NoError(t, err)
			assert.Equal(t, tc.wantDir, result.SyncDir)

			cfg := loadWrittenConfig(t, f.configPath)
			assert.Equal(t, tc.wantDir, cfg.Drives[driveid.MustCanonicalID(tc.drive)].SyncDir)
		})
	}
}

// Validates: R-3.3.5
func TestAddDrive_RefusesAConfigThatWouldNotLoad(t *testing.T) {
	f := newEnrollFixture(t, true)
	// A drive the user pointed inside the folder a personal drive defaults to.
	other := driveid.MustCanonicalID("personal:inside@example.com")
	require.NoError(t, AppendDriveSection(f.configPath, other, "~/OneDrive/Inside"))
	configBefore := readFileState(t, f.configPath)
	catalogBefore := readFileState(t, f.catalogPath)

	req := f.loginRequest()
	_, err := AddDrive(&req, testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking the config with the drive added")

	assert.Equal(t, configBefore, readFileState(t, f.configPath))
	assert.Equal(t, catalogBefore, readFileState(t, f.catalogPath))
}

// Validates: R-3.3.5
func TestAddDrive_RefusesAnIncoherentRequest(t *testing.T) {
	f := newEnrollFixture(t, false)
	cases := map[string]func(req *AddDriveRequest){
		"no drive":             func(req *AddDriveRequest) { req.Drive = driveid.CanonicalID{} },
		"no config path":       func(req *AddDriveRequest) { req.ConfigPath = "" },
		"no data directory":    func(req *AddDriveRequest) { req.DataDir = "" },
		"no sync dir function": func(req *AddDriveRequest) { req.MakeSyncDir = nil },
		"an owner for an owned drive": func(req *AddDriveRequest) {
			req.Owner = driveid.MustCanonicalID(enrollTestSharedOwner)
		},
		"a shared drive without its owner": func(req *AddDriveRequest) {
			req.Drive = driveid.MustCanonicalID(enrollTestSharedDrive)
		},
		"a login for a SharePoint library": func(req *AddDriveRequest) {
			req.Drive = driveid.MustCanonicalID("sharepoint:enroll@example.com:site:Docs")
			req.Login = &AccountLogin{UserID: enrollTestUserID}
		},
	}

	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			req := f.request(enrollTestOwnedDrive)
			spoil(&req)

			_, err := AddDrive(&req, testLogger(t))
			require.Error(t, err)
			assert.False(t, readFileState(t, f.configPath).exists)
			assert.False(t, readFileState(t, f.catalogPath).exists)
		})
	}
}

// Validates: R-3.1.5, R-3.3.1
func TestAddDrive_LoginRecordsAccountAndPrimaryDrive(t *testing.T) {
	f := newEnrollFixture(t, true)
	req := f.loginRequest()
	require.NoError(t, UpdateCatalogForDataDir(f.dataDir, func(catalog *Catalog) error {
		account, _ := catalog.AccountByCanonicalID(req.Drive)
		account.AuthRequirementReason = authstate.ReasonSyncAuthRejected
		catalog.UpsertAccount(&account)
		return nil
	}))
	req.Login.OrgName = enrollTestOrgName

	_, err := AddDrive(&req, testLogger(t))
	require.NoError(t, err)

	catalog, err := LoadCatalogForDataDir(f.dataDir)
	require.NoError(t, err)
	account, found := catalog.AccountByCanonicalID(req.Drive)
	require.True(t, found)
	assert.Equal(t, enrollTestUserID, account.UserID)
	assert.Equal(t, enrollTestUserName, account.DisplayName)
	assert.Equal(t, enrollTestOrgName, account.OrgName)
	assert.Equal(t, driveid.New(enrollTestPrimaryID).String(), account.PrimaryDriveID)
	assert.Equal(t, req.Drive.String(), account.PrimaryDriveCanonical)
	assert.Equal(t, authstate.ReasonSyncAuthRejected, account.AuthRequirementReason,
		"login clears the requirement itself, after the command succeeds")

	drive, found := catalog.DriveByCanonicalID(req.Drive)
	require.True(t, found)
	assert.Equal(t, req.Drive.String(), drive.OwnerAccountCanonical)
	assert.Equal(t, driveid.DriveTypePersonal, drive.DriveType)
	assert.True(t, drive.PrimaryForAccount)
	assert.Equal(t, driveid.New(enrollTestPrimaryID).String(), drive.RemoteDriveID)
	assert.NotEmpty(t, drive.DisplayName)
	assert.NotEmpty(t, drive.CachedAt)
}

// Validates: R-3.1.5, R-3.3.5
func TestAddDrive_RecordsTheDrivesOwner(t *testing.T) {
	cases := []struct {
		name        string
		request     func(f enrollFixture) AddDriveRequest
		wantOwner   string
		wantType    string
		wantName    func(cid driveid.CanonicalID) string
		wantSyncDir string
	}{
		{
			name: "a SharePoint library of the signed-in account",
			request: func(f enrollFixture) AddDriveRequest {
				return f.request("sharepoint:enroll@example.com:site-123:Docs")
			},
			wantOwner:   enrollTestOwnedDrive,
			wantType:    driveid.DriveTypeSharePoint,
			wantName:    DefaultDisplayName,
			wantSyncDir: "~/SharePoint - site-123 - Docs",
		},
		{
			name:        "a shared folder",
			request:     enrollFixture.sharedRequest,
			wantOwner:   enrollTestSharedOwner,
			wantType:    driveid.DriveTypeShared,
			wantName:    func(driveid.CanonicalID) string { return enrollTestSharedName },
			wantSyncDir: "~/OneDrive-Shared/" + enrollTestSharedName,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnrollFixture(t, true)
			req := tc.request(f)

			result, err := AddDrive(&req, testLogger(t))
			require.NoError(t, err)
			assert.Equal(t, tc.wantName(req.Drive), result.DisplayName)
			assert.Equal(t, tc.wantSyncDir, result.SyncDir)

			catalog, err := LoadCatalogForDataDir(f.dataDir)
			require.NoError(t, err)
			drive, found := catalog.DriveByCanonicalID(req.Drive)
			require.True(t, found)
			assert.Equal(t, tc.wantOwner, drive.OwnerAccountCanonical)
			assert.Equal(t, tc.wantType, drive.DriveType)
			assert.Equal(t, result.DisplayName, drive.DisplayName)
			assert.False(t, drive.PrimaryForAccount)
		})
	}
}
