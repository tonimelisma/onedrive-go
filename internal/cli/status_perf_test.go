package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// Validates: R-6.6.15
func TestStatusPerf_SummaryJSON_WithLivePerf(t *testing.T) {
	setTestDriveHome(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:perf-live@example.com")
	cfgPath := writeStatusPerfConfig(t, cid)

	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Drives:    []string{cid.String()},
	}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == synccontrol.PathPerfStatus:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := json.NewEncoder(w).Encode(synccontrol.PerfStatusResponse{
				OwnerMode: synccontrol.OwnerModeWatch,
				Aggregate: perf.Snapshot{HTTPRequestCount: 4},
				Drives: map[string]perf.Snapshot{
					cid.String(): {
						DurationMS:         4200,
						HTTPRequestCount:   4,
						HTTPRetryCount:     1,
						DBTransactionCount: 2,
						DownloadCount:      1,
						DownloadBytes:      128,
						WatchBatchCount:    3,
						WatchPathCount:     7,
					},
				},
			}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(cc, false, true))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.Len(t, decoded.Accounts[0].Drives, 1)
	require.NotNil(t, decoded.Accounts[0].Drives[0].SyncState)
	require.NotNil(t, decoded.Accounts[0].Drives[0].SyncState.Perf)
	assert.Equal(t, int64(4200), decoded.Accounts[0].Drives[0].SyncState.Perf.DurationMS)
	assert.Equal(t, 4, decoded.Accounts[0].Drives[0].SyncState.Perf.HTTPRequestCount)
	assert.Empty(t, decoded.Accounts[0].Drives[0].SyncState.PerfUnavailableReason)
}

// Validates: R-6.6.15
func TestStatusPerf_SummaryText_WithPerfAndNoActiveOwner(t *testing.T) {
	setTestDriveHome(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cid := driveid.MustCanonicalID("personal:perf-no-owner@example.com")
	cfgPath := writeStatusPerfConfig(t, cid)

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)

	require.NoError(t, runStatusCommand(cc, false, true))

	assert.Contains(t, out.String(), "    PERF")
	assert.Contains(t, out.String(), "Live performance unavailable: "+statusPerfUnavailableNoOwner)
}

// Validates: R-6.6.15
func TestStatusPerf_FilteredJSON_WithPerfUnavailableFromActiveOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cfgPath, cid := seedDriveStatusFixture(t)
	startCLIControlSocket(t, synccontrol.StatusResponse{
		OwnerMode: synccontrol.OwnerModeWatch,
		Drives:    []string{cid.String()},
	}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == synccontrol.PathPerfStatus:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			if err := json.NewEncoder(w).Encode(synccontrol.MutationResponse{
				Status:  synccontrol.StatusError,
				Code:    synccontrol.ErrorInternal,
				Message: "perf status unavailable",
			}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	cc.Flags.Drive = []string{cid.String()}
	cc.Flags.JSON = true

	require.NoError(t, runStatusCommand(cc, true, true))

	var decoded statusOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.Len(t, decoded.Accounts[0].Drives, 1)
	require.NotNil(t, decoded.Accounts[0].Drives[0].SyncState)
	assert.Nil(t, decoded.Accounts[0].Drives[0].SyncState.Perf)
	assert.Equal(t, statusPerfUnavailableGeneric, decoded.Accounts[0].Drives[0].SyncState.PerfUnavailableReason)
}

func writeStatusPerfConfig(t *testing.T, cid driveid.CanonicalID) string {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))

	return cfgPath
}
