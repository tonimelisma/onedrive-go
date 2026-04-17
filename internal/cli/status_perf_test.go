package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

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
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:perf-unavailable@example.com")
	cfgPath := writeStatusPerfConfig(t, cid)
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

// Validates: R-6.6.15
func TestStatusPerf_PrintStatusPerfText_UsesActionableCountFallback(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	state := &syncStateInfo{
		Perf: &perf.Snapshot{
			DurationMS:            (1250 * time.Millisecond).Milliseconds(),
			HTTPRequestCount:      4,
			HTTPRetryCount:        1,
			HTTPTransportErrors:   2,
			DBTransactionCount:    3,
			DBTransactionTimeMS:   (240 * time.Millisecond).Milliseconds(),
			DownloadCount:         5,
			DownloadBytes:         64,
			UploadCount:           6,
			UploadBytes:           128,
			ObserveTimeMS:         (100 * time.Millisecond).Milliseconds(),
			PlanTimeMS:            (200 * time.Millisecond).Milliseconds(),
			ExecuteTimeMS:         (300 * time.Millisecond).Milliseconds(),
			ReconcileTimeMS:       (400 * time.Millisecond).Milliseconds(),
			ActionableActionCount: 7,
			WatchBatchCount:       8,
			WatchPathCount:        9,
		},
	}

	require.NoError(t, printStatusPerfText(&out, state))
	rendered := out.String()
	assert.Contains(t, rendered, "PERF")
	assert.Contains(t, rendered, "HTTP:")
	assert.Contains(t, rendered, "4 req, 1 retries, 2 transport errors")
	assert.Contains(t, rendered, "DB:")
	assert.Contains(t, rendered, "3 tx in 240ms")
	assert.Contains(t, rendered, "Transfers:")
	assert.Contains(t, rendered, "down 5 (64 B), up 6 (128 B)")
	assert.Contains(t, rendered, "Phases:")
	assert.Contains(t, rendered, "observe 100ms, plan 200ms, execute 300ms, reconcile 400ms")
	assert.Contains(t, rendered, "Activity:")
	assert.Contains(t, rendered, "actions 7, watch batches 8, watch paths 9")
}

// Validates: R-6.6.15
func TestStatusPerfOverlayLookupAndPersistentFlags(t *testing.T) {
	t.Parallel()

	overlay := statusPerfOverlay{
		enabled:       true,
		ownerPresent:  true,
		managedDrives: map[string]struct{}{"drive:a": {}},
		snapshots: map[string]perf.Snapshot{
			"drive:a": {HTTPRequestCount: 11},
		},
	}

	snapshot, reason := overlay.lookup("drive:a")
	require.NotNil(t, snapshot)
	assert.Equal(t, 11, snapshot.HTTPRequestCount)
	assert.Empty(t, reason)

	snapshot, reason = overlay.lookup("drive:b")
	assert.Nil(t, snapshot)
	assert.Equal(t, statusPerfUnavailableInactive, reason)

	overlay.managedDrives["drive:b"] = struct{}{}
	snapshot, reason = overlay.lookup("drive:b")
	assert.Nil(t, snapshot)
	assert.Equal(t, statusPerfUnavailableCollecting, reason)

	state := &syncStateInfo{
		LastSyncTime:           "yesterday",
		StateStoreStatus:       "healthy",
		StateStoreRecoveryHint: "none",
	}
	assert.True(t, state.hasPersistentSummaryData())
	assert.True(t, state.hasPersistentStoreData())
	assert.True(t, state.hasPersistentStatusData())
}

// Validates: R-6.6.15
func TestStatusPerfOverlayApplyAndUnavailableBranches(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	require.NoError(t, printStatusPerfText(&out, &syncStateInfo{
		PerfUnavailableReason: statusPerfUnavailableGeneric,
	}))
	assert.Contains(t, out.String(), "Live performance unavailable: "+statusPerfUnavailableGeneric)

	assert.Equal(t, "0s", formatPerfElapsed(0))

	disabledSnapshot, disabledReason := (statusPerfOverlay{}).lookup("drive:a")
	assert.Nil(t, disabledSnapshot)
	assert.Empty(t, disabledReason)

	noOwnerSnapshot, noOwnerReason := (statusPerfOverlay{
		enabled:           true,
		unavailableReason: statusPerfUnavailableNoOwner,
	}).lookup("drive:a")
	assert.Nil(t, noOwnerSnapshot)
	assert.Equal(t, statusPerfUnavailableNoOwner, noOwnerReason)

	accounts := []statusAccount{{
		Drives: []statusDrive{{
			CanonicalID: "drive:a",
			SyncState:   nil,
		}},
	}}
	applyStatusPerfOverlay(accounts, statusPerfOverlay{
		enabled:       true,
		ownerPresent:  true,
		managedDrives: map[string]struct{}{"drive:a": {}},
		snapshots: map[string]perf.Snapshot{
			"drive:a": {DurationMS: 1500},
		},
	})
	require.NotNil(t, accounts[0].Drives[0].SyncState)
	require.NotNil(t, accounts[0].Drives[0].SyncState.Perf)
	assert.Equal(t, int64(1500), accounts[0].Drives[0].SyncState.Perf.DurationMS)

	var nilState *syncStateInfo
	assert.False(t, nilState.hasPersistentStatusData())
}

func writeStatusPerfConfig(t *testing.T, cid driveid.CanonicalID) string {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = "Perf User"
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = "drive-perf"
	})

	return cfgPath
}
