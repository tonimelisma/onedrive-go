package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const (
	rmTestResolvePath = "/drives/0000000drive-123/root:/old-report.pdf:"
	rmTestDeletePath  = "/drives/0000000drive-123/items/item-123"
)

type rmRequestCounts struct {
	deleteCalls          int
	permanentDeleteCalls int
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
