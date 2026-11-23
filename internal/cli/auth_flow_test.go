package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

const (
	loginTestUserID         = "user-123"
	loginTestDisplayName    = "Login User"
	loginTestOldOrg         = "Old Org"
	loginTestNewDriveID     = "drive-new"
	loginTestPreviousAccess = "old-token"
	loginTestReadOnly       = 0o500
	loginTestWritable       = 0o700
)

// managedFileState is one managed file as the test saw it: its bytes, or
// absent.
type managedFileState struct {
	data   []byte
	exists bool
}

func readManagedFileState(t *testing.T, path string) managedFileState {
	t.Helper()

	data, err := localpath.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return managedFileState{}
	}
	require.NoError(t, err)

	return managedFileState{data: data, exists: true}
}

// loginFixture is a signed-in account whose drive section was removed: the
// catalog still knows the account, the config holds another drive only, and
// a previous token may still be on disk. A login from here runs every step of
// enrolling the drive.
type loginFixture struct {
	cid         driveid.CanonicalID
	cfgPath     string
	catalogPath string
	tokenPath   string
	pendingPath string
}

func newLoginFixture(t *testing.T, withPreviousToken bool) *loginFixture {
	t.Helper()

	return newLoginFixtureFor(t, driveid.MustCanonicalID("personal:user@example.com"), withPreviousToken)
}

// newLoginFixtureFor is newLoginFixture for account, a personal or a work or
// school account.
func newLoginFixtureFor(t *testing.T, account driveid.CanonicalID, withPreviousToken bool) *loginFixture {
	t.Helper()
	setTestDriveHome(t)

	fixture := &loginFixture{
		cid:         account,
		cfgPath:     filepath.Join(t.TempDir(), "conf", "config.toml"),
		catalogPath: config.CatalogPathForDataDir(config.DefaultDataDir()),
		pendingPath: pendingTokenPath(),
	}
	fixture.tokenPath = config.DriveTokenPath(fixture.cid)

	other := driveid.MustCanonicalID("business:other@contoso.com")
	require.NoError(t, config.AppendDriveSection(t.Context(), fixture.cfgPath, other, filepath.Join(t.TempDir(), "Other")))
	seedCatalogAccount(t, fixture.cid, func(account *config.CatalogAccount) {
		account.UserID = loginTestUserID
		account.DisplayName = loginTestDisplayName
		account.OrgName = loginTestOldOrg
	})
	if withPreviousToken {
		writeAccessTokenFile(t, fixture.cid, loginTestPreviousAccess)
	}

	require.NoError(t, os.MkdirAll(filepath.Dir(fixture.pendingPath), loginTestWritable))
	require.NoError(t, tokenfile.Save(fixture.pendingPath, &oauth2.Token{AccessToken: "pending", TokenType: "Bearer"}))

	return fixture
}

// runLogin runs everything login does after the sign-in exchange against a
// fake Graph that knows the fixture's account.
func (f *loginFixture) runLogin(t *testing.T) error {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{"id":"`+loginTestUserID+`","displayName":"`+loginTestDisplayName+`","mail":"`+f.cid.Email()+`"}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"`+loginTestNewDriveID+`","name":"OneDrive","driveType":"`+f.cid.DriveType()+`"}`)
		default:
			http.Error(w, "unexpected graph path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	cc := newCommandContext(&bytes.Buffer{}, f.cfgPath)
	cc.Runtime = newDiscoverAccountTestRuntime(srv.URL)

	return completeLogin(t.Context(), cc, staticTokenSource{}, f.pendingPath)
}

// signInRequirementOf is the sign-in requirement the catalog holds for the
// account.
func signInRequirementOf(t *testing.T, account driveid.CanonicalID) authstate.Reason {
	t.Helper()

	record, found := loadCatalogAccount(t, account)
	require.True(t, found)

	return record.AuthRequirementReason
}

// offlineStatusAccounts is what `status --json` lists, without asking
// OneDrive.
func offlineStatusAccounts(t *testing.T, cfgPath string) []statusAccount {
	t.Helper()

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		CfgPath:      cfgPath,
		Flags:        CLIFlags{JSON: true},
		seams:        cliSeams{statusLiveOverlay: noStatusLiveOverlay},
	}
	require.NoError(t, runStatusCommand(t.Context(), cc, false))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))

	return decoded.Accounts
}

// A personal and a work or school account can share an address. The sign-in
// requirement is written on the account whose sign-in was refused, by the
// writer sync and status use, and a login that works ends the requirement of
// the account it signed in alone, whichever of the two sorts first: the other
// account's requirement stands. status lists the address once, for the work
// or school account, the first by canonical ID and the one whose saved login
// it checks, so it shows that account's requirement and only that one's.
//
// Validates: R-3.3.1, R-2.10.45, R-2.10.47
func TestLogin_EndsTheSignInRequirementOfTheAccountItSignedIn(t *testing.T) {
	personal := driveid.MustCanonicalID("personal:user@example.com")
	business := driveid.MustCanonicalID("business:user@example.com")
	cases := []struct {
		name             string
		marked, signedIn driveid.CanonicalID
	}{
		{name: "the personal account's own login", marked: personal, signedIn: personal},
		{name: "the work account's own login", marked: business, signedIn: business},
		{name: "the work account's login after the personal one was refused", marked: personal, signedIn: business},
		{name: "the personal account's login after the work one was refused", marked: business, signedIn: personal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixtureFor(t, tc.signedIn, true)
			// The sign-in status checks afterwards is a whole saved login.
			require.NoError(t, tokenfile.Save(f.pendingPath, &oauth2.Token{
				AccessToken: "pending", RefreshToken: "refresh-pending", TokenType: "Bearer",
				Expiry: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
			}))
			// status refuses a configured drive the inventory lacks, so the
			// fixture's other drive gets its record.
			unrelated := driveid.MustCanonicalID("business:other@contoso.com")
			seedCatalogAccount(t, unrelated, nil)
			seedCatalogDrive(t, unrelated, nil)
			other := personal
			if tc.signedIn == personal {
				other = business
			}
			seedCatalogAccount(t, other, nil)
			writeAccessTokenFile(t, other, "other-token")
			require.NoError(t, config.MarkAccountAuthRequired(t.Context(), config.DefaultDataDir(), tc.marked, authReasonSyncAuthRejected))

			require.NoError(t, f.runLogin(t))

			want := map[driveid.CanonicalID]authstate.Reason{tc.marked: authReasonSyncAuthRejected, tc.signedIn: ""}
			assert.Empty(t, signInRequirementOf(t, tc.signedIn), "the login ended its account's requirement")
			assert.Equal(t, want[other], signInRequirementOf(t, other), "the other account keeps its own state")

			accounts := offlineStatusAccounts(t, f.cfgPath)
			var listed []statusAccount
			for i := range accounts {
				if accounts[i].Email == business.Email() {
					listed = append(listed, accounts[i])
				}
			}
			require.Len(t, listed, 1, "status lists the address once")
			assert.Equal(t, want[business] != "", listed[0].SignInRequired != nil,
				"status shows the work account's requirement: %+v", listed[0].SignInRequired)
		})
	}
}

// Validates: R-3.3.1
func TestLogin_FailureAtEachStepLeavesConfigCatalogAndTokenAsTheyWere(t *testing.T) {
	steps := []struct {
		name string
		fail func(t *testing.T, f *loginFixture)
	}{
		{
			name: "the token cannot be installed",
			fail: func(t *testing.T, f *loginFixture) {
				t.Helper()
				require.NoError(t, os.Remove(f.pendingPath))
			},
		},
		{
			name: "the sync directory cannot be created",
			fail: func(t *testing.T, _ *loginFixture) {
				t.Helper()
				home, err := os.UserHomeDir()
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(home, "OneDrive"), []byte("not a directory"), 0o600))
			},
		},
		{
			name: "the config cannot be written",
			fail: func(t *testing.T, f *loginFixture) {
				t.Helper()
				dir := filepath.Dir(f.cfgPath)
				require.NoError(t, os.Chmod(dir, loginTestReadOnly))
				t.Cleanup(func() {
					assert.NoError(t, os.Chmod(dir, loginTestWritable))
				})
			},
		},
	}

	for _, withPreviousToken := range []bool{true, false} {
		for _, step := range steps {
			name := step.name
			if !withPreviousToken {
				name += ", first login"
			}
			t.Run(name, func(t *testing.T) {
				f := newLoginFixture(t, withPreviousToken)
				configBefore := readManagedFileState(t, f.cfgPath)
				catalogBefore := readManagedFileState(t, f.catalogPath)
				tokenBefore := readManagedFileState(t, f.tokenPath)
				step.fail(t, f)

				require.Error(t, f.runLogin(t))

				assert.Equal(t, configBefore, readManagedFileState(t, f.cfgPath), "config changed")
				assert.Equal(t, catalogBefore, readManagedFileState(t, f.catalogPath), "catalog changed")
				assert.Equal(t, tokenBefore, readManagedFileState(t, f.tokenPath), "the previous token was not put back")
			})
		}
	}
}

// A login that fails before it installs its token removes the pending
// sign-in. When that removal fails as well, the login's error says so beside
// its own failure instead of leaving a saved sign-in on disk unsaid.
//
// Validates: R-3.3.1
func TestLogin_APendingSignInItCannotRemoveJoinsTheLoginsError(t *testing.T) {
	f := newLoginFixture(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the profile is unavailable", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	cc := newCommandContext(&bytes.Buffer{}, f.cfgPath)
	cc.Runtime = newDiscoverAccountTestRuntime(srv.URL)
	pendingDir := filepath.Dir(f.pendingPath)
	require.NoError(t, os.Chmod(pendingDir, loginTestReadOnly))
	t.Cleanup(func() {
		assert.NoError(t, os.Chmod(pendingDir, loginTestWritable))
	})

	err := completeLogin(t.Context(), cc, staticTokenSource{}, f.pendingPath)

	require.ErrorContains(t, err, "discovering account")
	require.ErrorContains(t, err, "remove the pending sign-in")
	require.ErrorIs(t, err, os.ErrPermission)
	assert.FileExists(t, f.pendingPath)
}
