package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

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
	setTestDriveHome(t)

	fixture := &loginFixture{
		cid:         driveid.MustCanonicalID("personal:user@example.com"),
		cfgPath:     filepath.Join(t.TempDir(), "conf", "config.toml"),
		catalogPath: config.CatalogPathForDataDir(config.DefaultDataDir()),
		pendingPath: pendingTokenPath(),
	}
	fixture.tokenPath = config.DriveTokenPath(fixture.cid)

	other := driveid.MustCanonicalID("business:other@contoso.com")
	require.NoError(t, config.AppendDriveSection(fixture.cfgPath, other, filepath.Join(t.TempDir(), "Other")))
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
			writeTestResponse(t, w, `{"id":"`+loginTestUserID+`","displayName":"`+loginTestDisplayName+`","mail":"user@example.com"}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"`+loginTestNewDriveID+`","name":"OneDrive","driveType":"personal"}`)
		default:
			http.Error(w, "unexpected graph path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	cc := newCommandContext(&bytes.Buffer{}, f.cfgPath)
	cc.Runtime = newDiscoverAccountTestRuntime(srv.URL)

	return completeLogin(t.Context(), cc, staticTokenSource{}, f.pendingPath)
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
