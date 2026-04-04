package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newFileCommandTestContext(
	t *testing.T,
	cid driveid.CanonicalID,
	handler http.Handler,
	stdout *bytes.Buffer,
	stderr *bytes.Buffer,
) *CLIContext {
	t.Helper()

	setTestDriveHome(t)

	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("not-a-sqlite-db"), 0o600))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	logger := buildLoggerWithStatusWriter(nil, CLIFlags{}, stderr)
	provider := driveops.NewSessionProvider(nil, srv.Client(), srv.Client(), "test-agent", logger)
	provider.GraphBaseURL = srv.URL
	provider.TokenSourceFn = func(context.Context, string, *slog.Logger) (graph.TokenSource, error) {
		return staticTokenSource{}, nil
	}

	return &CLIContext{
		Logger:       logger,
		OutputWriter: stdout,
		StatusWriter: stderr,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			DriveID:     driveid.New("drive-123"),
		},
		Provider: provider,
	}
}

// Validates: R-1.1
func TestRunLs_SucceedsWithoutSurfacingBrokenSyncDBWarnings(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cc := newFileCommandTestContext(
		t,
		driveid.MustCanonicalID("personal:user@example.com"),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/drives/0000000drive-123/items/root/children", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{"value":[{"id":"child-1","name":"docs","folder":{"childCount":0}}]}`)
		}),
		&stdout,
		&stderr,
	)

	cmd := newLsCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))

	require.NoError(t, cmd.Execute())
	assert.Contains(t, stdout.String(), "docs/")
	assert.NotContains(t, stderr.String(), "clearing stale auth scopes after successful graph proof")
	assert.NotContains(t, stderr.String(), "open sync store")
}

// Validates: R-1.4
func TestRunRm_SucceedsWithoutSurfacingBrokenSyncDBWarnings(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cc := newFileCommandTestContext(
		t,
		driveid.MustCanonicalID("business:user@example.com"),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/drives/0000000drive-123/root:/old-report.pdf:":
				writeTestResponse(t, w, `{
					"id":"item-123",
					"name":"old-report.pdf",
					"size":1,
					"createdDateTime":"2026-04-03T00:00:00Z",
					"lastModifiedDateTime":"2026-04-03T00:00:00Z",
					"eTag":"etag"
				}`)
			case r.Method == http.MethodDelete && r.URL.Path == "/drives/0000000drive-123/items/item-123":
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}
		}),
		&stdout,
		&stderr,
	)

	cmd := newRmCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))
	cmd.SetArgs([]string{"/old-report.pdf"})

	require.NoError(t, cmd.Execute())
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "Deleted /old-report.pdf (moved to recycle bin)")
	assert.NotContains(t, stderr.String(), "clearing stale auth scopes after successful graph proof")
	assert.NotContains(t, stderr.String(), "open sync store")
}
