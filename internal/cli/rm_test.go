package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	rmTestResolvePath = "/drives/0000000drive-123/root:/old-report.pdf:"
	rmTestDeletePath  = "/drives/0000000drive-123/items/item-123"
	rmNestedResolve   = "/drives/0000000drive-123/root:/docs/old-report.pdf:"
	rmNestedParent    = "/drives/0000000drive-123/root:/docs:"
	rmRootChildren    = "/drives/0000000drive-123/items/root/children"
)

type rmRequestCounts struct {
	deleteCalls          int
	permanentDeleteCalls int
}

type fakeRmParentWaiter struct {
	calls []string
	err   error
}

func (f *fakeRmParentWaiter) WaitPathVisible(_ context.Context, remotePath string) (*graph.Item, error) {
	f.calls = append(f.calls, remotePath)
	if f.err != nil {
		return nil, f.err
	}

	return &graph.Item{ID: "parent-id", Name: filepath.Base(remotePath)}, nil
}

// Validates: R-1.4.3
func TestPrintRmJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printRmJSON(&buf, rmJSONOutput{Deleted: "/docs/old-report.pdf"})
	require.NoError(t, err)

	var decoded rmJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "/docs/old-report.pdf", decoded.Deleted)
}

func newRmTestCommand(t *testing.T, cid driveid.CanonicalID, handler http.Handler) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	setTestDriveHome(t)

	writeTestTokenFile(t, config.DefaultDataDir(), filepath.Base(config.DriveTokenPath(cid)))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	provider := driveops.NewSessionProvider(
		nil,
		driveops.StaticClientResolver(srv.Client(), srv.Client()),
		"test-agent",
		testDriveLogger(t),
	)
	provider.GraphBaseURL = srv.URL

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			DriveID:     driveid.New("drive-123"),
		},
		Provider: provider,
	}

	cmd := newRmCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))

	return cmd, &out
}

func newRmTestHandler(t *testing.T, counts *rmRequestCounts) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == rmTestResolvePath:
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id":"item-123",
				"name":"old-report.pdf",
				"size":1,
				"createdDateTime":"2026-04-03T00:00:00Z",
				"lastModifiedDateTime":"2026-04-03T00:00:00Z",
				"eTag":"etag"
			}`)
		case r.Method == http.MethodDelete && r.URL.Path == rmTestDeletePath:
			counts.deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == rmTestDeletePath+"/permanentDelete":
			counts.permanentDeleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	})
}

// Validates: R-6.4.4
func TestRunRm_DefaultUsesRecycleBinDelete(t *testing.T) {
	counts := &rmRequestCounts{}
	cmd, out := newRmTestCommand(
		t,
		driveid.MustCanonicalID("business:user@example.com"),
		newRmTestHandler(t, counts),
	)

	require.NoError(t, runRm(cmd, []string{"/old-report.pdf"}))
	assert.Equal(t, 1, counts.deleteCalls)
	assert.Zero(t, counts.permanentDeleteCalls)
	assert.Contains(t, out.String(), "Deleted /old-report.pdf (moved to recycle bin)")
}

func TestRunRm_PermanentUsesPermanentDelete(t *testing.T) {
	tests := []struct {
		name string
		cid  driveid.CanonicalID
	}{
		{
			name: "business",
			cid:  driveid.MustCanonicalID("business:user@example.com"),
		},
		{
			name: "personal",
			cid:  driveid.MustCanonicalID("personal:user@example.com"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts := &rmRequestCounts{}
			cmd, out := newRmTestCommand(t, tt.cid, newRmTestHandler(t, counts))

			require.NoError(t, cmd.Flags().Set("permanent", "true"))
			require.NoError(t, runRm(cmd, []string{"/old-report.pdf"}))
			assert.Zero(t, counts.deleteCalls)
			assert.Equal(t, 1, counts.permanentDeleteCalls)
			assert.Contains(t, out.String(), "Permanently deleted /old-report.pdf")
		})
	}
}

// Validates: R-1.4.4
func TestRunRm_WaitsForParentVisibilityAfterDelete(t *testing.T) {
	counts := &rmRequestCounts{}
	var parentLookups int

	cmd, _ := newRmTestCommand(t, driveid.MustCanonicalID("business:user@example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == rmNestedResolve:
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id":"item-123",
				"name":"old-report.pdf",
				"size":1,
				"createdDateTime":"2026-04-03T00:00:00Z",
				"lastModifiedDateTime":"2026-04-03T00:00:00Z",
				"eTag":"etag"
			}`)
		case r.Method == http.MethodDelete && r.URL.Path == rmTestDeletePath:
			counts.deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == rmNestedParent:
			parentLookups++
			if parentLookups < 3 {
				w.WriteHeader(http.StatusNotFound)
				writeTestResponse(t, w, `{"error":{"code":"itemNotFound"}}`)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id":"docs-id",
				"name":"docs",
				"size":0,
				"createdDateTime":"2026-04-03T00:00:00Z",
				"lastModifiedDateTime":"2026-04-03T00:00:00Z",
				"folder":{"childCount":1},
				"eTag":"etag-parent"
			}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	require.NoError(t, runRm(cmd, []string{"/docs/old-report.pdf"}))
	assert.Equal(t, 1, counts.deleteCalls)
	assert.Equal(t, 3, parentLookups)
}

// Validates: R-1.4.4
func TestRunRm_ReconcilesTransientDeleteNotFoundAgainstPath(t *testing.T) {
	counts := &rmRequestCounts{}
	var resolveCalls int

	cmd, _ := newRmTestCommand(t, driveid.MustCanonicalID("business:user@example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == rmTestResolvePath && resolveCalls == 0:
			resolveCalls++
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id":"item-123",
				"name":"old-report.pdf",
				"size":1,
				"createdDateTime":"2026-04-03T00:00:00Z",
				"lastModifiedDateTime":"2026-04-03T00:00:00Z",
				"eTag":"etag"
			}`)
		case r.Method == http.MethodDelete && r.URL.Path == rmTestDeletePath:
			counts.deleteCalls++
			w.WriteHeader(http.StatusNotFound)
			writeTestResponse(t, w, `{"error":{"code":"itemNotFound"}}`)
		case r.Method == http.MethodGet && r.URL.Path == rmTestResolvePath && resolveCalls == 1:
			resolveCalls++
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id":"item-456",
				"name":"old-report.pdf",
				"size":1,
				"createdDateTime":"2026-04-03T00:00:00Z",
				"lastModifiedDateTime":"2026-04-03T00:00:00Z",
				"eTag":"etag-2"
			}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/drives/0000000drive-123/items/item-456":
			counts.deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	require.NoError(t, runRm(cmd, []string{"/old-report.pdf"}))
	assert.Equal(t, 2, counts.deleteCalls)
}

// Validates: R-1.4.3
func TestRunRm_ResolveDeleteTargetFallsBackToParentListing(t *testing.T) {
	counts := &rmRequestCounts{}
	var resolveCalls int

	cmd, _ := newRmTestCommand(t, driveid.MustCanonicalID("business:user@example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == rmTestResolvePath:
			resolveCalls++
			w.WriteHeader(http.StatusNotFound)
			writeTestResponse(t, w, `{"error":{"code":"itemNotFound"}}`)
		case r.Method == http.MethodGet && r.URL.Path == rmRootChildren:
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"value": [
					{
						"id":"item-123",
						"name":"old-report.pdf",
						"size":1,
						"createdDateTime":"2026-04-03T00:00:00Z",
						"lastModifiedDateTime":"2026-04-03T00:00:00Z",
						"eTag":"etag"
					}
				]
			}`)
		case r.Method == http.MethodDelete && r.URL.Path == rmTestDeletePath:
			counts.deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	require.NoError(t, runRm(cmd, []string{"/old-report.pdf"}))
	assert.Equal(t, 1, resolveCalls)
	assert.Equal(t, 1, counts.deleteCalls)
}

func TestConfirmRmParentVisibility_WarnsButSucceedsOnSettlingParent(t *testing.T) {
	t.Parallel()

	waiter := &fakeRmParentWaiter{
		err: &driveops.PathNotVisibleError{Path: "docs"},
	}

	var status bytes.Buffer
	err := confirmRmParentVisibility(context.Background(), waiter, "/docs/old-report.pdf", &status)
	require.NoError(t, err)
	assert.Equal(t, []string{"docs"}, waiter.calls)
	assert.Contains(t, status.String(), "warning:")
	assert.Contains(t, status.String(), "/docs")
	assert.Contains(t, status.String(), "/docs/old-report.pdf")
}

func TestConfirmRmParentVisibility_FailsOnNonVisibilityError(t *testing.T) {
	t.Parallel()

	waiter := &fakeRmParentWaiter{
		err: errors.New("transport exploded"),
	}

	var status bytes.Buffer
	err := confirmRmParentVisibility(context.Background(), waiter, "/docs/old-report.pdf", &status)
	require.Error(t, err)
	require.ErrorContains(t, err, "confirming parent \"/docs\" visibility after delete")
	require.ErrorContains(t, err, "transport exploded")
	assert.Empty(t, status.String())
}

func TestRemovableParentPath(t *testing.T) {
	t.Parallel()

	assert.Empty(t, removableParentPath("/old-report.pdf"))
	assert.Equal(t, "docs", removableParentPath("/docs/old-report.pdf"))
	assert.Equal(t, "docs/sub", removableParentPath("/docs/sub/report.pdf"))
}
