package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type stubResolveConflictStore struct {
	requestCalls int
	result       syncengine.ConflictRequestResult
	err          error
}

const controlSocketTestReadHeaderTimeout = 5 * time.Second

func (s *stubResolveConflictStore) ListConflicts(context.Context) ([]syncengine.ConflictRecord, error) {
	return nil, nil
}

func (s *stubResolveConflictStore) ListAllConflicts(context.Context) ([]syncengine.ConflictRecord, error) {
	return nil, nil
}

func (s *stubResolveConflictStore) RequestConflictResolution(
	context.Context,
	string,
	string,
) (syncengine.ConflictRequestResult, error) {
	s.requestCalls++
	return s.result, s.err
}

func (s *stubResolveConflictStore) Close(context.Context) error {
	return nil
}

func startCLIControlSocket(
	t *testing.T,
	status synccontrol.StatusResponse,
	mutate func(http.ResponseWriter, *http.Request),
) {
	t.Helper()

	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, json.NewEncoder(w).Encode(status))
	}, mutate)
}

func startCLIControlSocketWithStatusHandler(
	t *testing.T,
	statusHandler func(http.ResponseWriter, *http.Request),
	mutate func(http.ResponseWriter, *http.Request),
) {
	t.Helper()

	socketPath, err := config.ControlSocketPath()
	require.NoError(t, err)
	require.NotEmpty(t, socketPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	require.NoError(t, os.RemoveAll(socketPath))

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", socketPath)
	require.NoError(t, err)

	server := &http.Server{
		ReadHeaderTimeout: controlSocketTestReadHeaderTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == synccontrol.PathStatus {
				statusHandler(w, r)
				return
			}
			mutate(w, r)
		}),
	}

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("test control server: %v", err)
		}
	}()

	t.Cleanup(func() {
		assert.NoError(t, server.Shutdown(context.Background()))
		assert.NoError(t, os.RemoveAll(socketPath))
	})
}

// Validates: R-2.3.6, R-2.9.1
func TestResolveDeletes_WritesDirectDBIntentForOneShotOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:oneshot@example.com")
	driveID := driveid.New("drive-oneshot")

	store, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, store.UpsertHeldDeletes(t.Context(), []syncengine.HeldDeleteRecord{{
		DriveID:       driveID,
		ActionType:    syncengine.ActionRemoteDelete,
		Path:          "delete-me.txt",
		ItemID:        "item-delete",
		State:         syncengine.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.Close(t.Context()))

	var postCalls atomic.Int32
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeOneShot}, func(w http.ResponseWriter, _ *http.Request) {
		postCalls.Add(1)
		w.WriteHeader(http.StatusConflict)
		assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorForegroundSyncRunning,
			Message: "foreground sync is running",
		}))
	})

	var out bytes.Buffer
	cc := &CLIContext{
		OutputWriter: &out,
		Logger:       slog.New(slog.DiscardHandler),
		Cfg:          &config.ResolvedDrive{CanonicalID: cid},
	}

	require.NoError(t, runApproveDeletes(t.Context(), cc))
	assert.Zero(t, postCalls.Load())
	assert.Contains(t, out.String(), resolveApproveDeletesSuccess)

	reopened, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})
	approved, err := reopened.ListHeldDeletesByState(t.Context(), syncengine.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "item-delete", approved[0].ItemID)
}

// Validates: R-2.3.12, R-2.9.3
func TestResolveConflict_FallsBackToDBIntentWhenNoDaemonSocketExists(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:no-daemon@example.com")

	resolver := &stubResolveConflictStore{
		result: syncengine.ConflictRequestResult{Status: syncengine.ConflictRequestQueued},
	}
	cc := &CLIContext{
		Logger: slog.New(slog.DiscardHandler),
		Cfg:    &config.ResolvedDrive{CanonicalID: cid},
	}

	result, err := requestConflictResolution(t.Context(), cc, resolver, "conflict-1", syncengine.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, syncengine.ConflictRequestQueued, result.Status)
	assert.Equal(t, 1, resolver.requestCalls)
}

// Validates: R-2.3.12, R-2.9.3
func TestResolveConflict_DoesNotFallbackOnTypedWatchDaemonError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:watch-error@example.com")
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorConflictNotFound,
			Message: "conflict not found",
		}))
	})

	resolver := &stubResolveConflictStore{
		result: syncengine.ConflictRequestResult{Status: syncengine.ConflictRequestQueued},
	}
	cc := &CLIContext{
		Logger: slog.New(slog.DiscardHandler),
		Cfg:    &config.ResolvedDrive{CanonicalID: cid},
	}

	_, err := requestConflictResolution(t.Context(), cc, resolver, "missing", syncengine.ResolutionKeepLocal)
	require.Error(t, err)
	assert.True(t, isControlDaemonError(err))
	assert.Contains(t, err.Error(), string(synccontrol.ErrorConflictNotFound))
	assert.Contains(t, err.Error(), "conflict not found")
	assert.Zero(t, resolver.requestCalls)
}

// Validates: R-2.3.6, R-2.9.3
func TestResolveDeletes_DoesNotFallbackOnTypedWatchDaemonError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:approve-error@example.com")
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.MutationResponse{
			Status:  synccontrol.StatusError,
			Code:    synccontrol.ErrorDriveNotManaged,
			Message: "drive not managed",
		}))
	})

	var out bytes.Buffer
	cc := &CLIContext{
		OutputWriter: &out,
		Logger:       slog.New(slog.DiscardHandler),
		Cfg:          &config.ResolvedDrive{CanonicalID: cid},
	}

	err := runApproveDeletes(t.Context(), cc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(synccontrol.ErrorDriveNotManaged))
	assert.Contains(t, err.Error(), "drive not managed")
	assert.NotContains(t, out.String(), resolveApproveDeletesSuccess)
}

// Validates: R-2.3.6, R-2.9.3
func TestResolveDeletes_DoesNotFallbackWhenControlProbeIsAmbiguous(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:probe-failed@example.com")
	driveID := driveid.New("drive-probe-failed")

	store, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, store.UpsertHeldDeletes(t.Context(), []syncengine.HeldDeleteRecord{{
		DriveID:       driveID,
		ActionType:    syncengine.ActionRemoteDelete,
		Path:          "delete-me.txt",
		ItemID:        "item-delete",
		State:         syncengine.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.Close(t.Context()))

	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "status unavailable", http.StatusInternalServerError)
	}, func(http.ResponseWriter, *http.Request) {
		t.Fatal("mutating control request should not be attempted after an ambiguous probe failure")
	})

	var out bytes.Buffer
	cc := &CLIContext{
		OutputWriter: &out,
		Logger:       slog.New(slog.DiscardHandler),
		Cfg:          &config.ResolvedDrive{CanonicalID: cid},
	}

	err = runApproveDeletes(t.Context(), cc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "probe control owner")
	assert.NotContains(t, out.String(), resolveApproveDeletesSuccess)

	reopened, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(t.Context()))
	})

	held, err := reopened.ListHeldDeletesByState(t.Context(), syncengine.HeldDeleteStateHeld)
	require.NoError(t, err)
	require.Len(t, held, 1)
}

// Validates: R-2.3.12, R-2.9.3
func TestResolveConflict_DoesNotFallbackToDBIntentWhenWatchSocketPostFailsButWatchOwnerRemains(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:watch-disappears@example.com")
	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unavailable", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack control socket connection: %v", err)
			return
		}
		assert.NoError(t, conn.Close())
	})

	resolver := &stubResolveConflictStore{
		result: syncengine.ConflictRequestResult{Status: syncengine.ConflictRequestQueued},
	}
	cc := &CLIContext{
		Logger: slog.New(slog.DiscardHandler),
		Cfg:    &config.ResolvedDrive{CanonicalID: cid},
	}

	_, err := requestConflictResolution(t.Context(), cc, resolver, "conflict-1", syncengine.ResolutionKeepLocal)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "watch daemon is still active")
	assert.Zero(t, resolver.requestCalls)
}

// Validates: R-2.3.12, R-2.9.3
func TestResolveConflict_FallsBackToDBIntentWhenWatchSocketPostFailsAndSocketDisappears(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cid := driveid.MustCanonicalID("personal:watch-disappears@example.com")
	socketPath, err := config.ControlSocketPath()
	require.NoError(t, err)

	startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unavailable", http.StatusInternalServerError)
			return
		}
		conn, _, hijackErr := hijacker.Hijack()
		if hijackErr != nil {
			t.Errorf("hijack control socket connection: %v", hijackErr)
			return
		}
		assert.NoError(t, conn.Close())
		assert.NoError(t, os.Remove(socketPath))
	})

	resolver := &stubResolveConflictStore{
		result: syncengine.ConflictRequestResult{Status: syncengine.ConflictRequestQueued},
	}
	cc := &CLIContext{
		Logger: slog.New(slog.DiscardHandler),
		Cfg:    &config.ResolvedDrive{CanonicalID: cid},
	}

	result, err := requestConflictResolution(t.Context(), cc, resolver, "conflict-1", syncengine.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, syncengine.ConflictRequestQueued, result.Status)
	assert.Equal(t, 1, resolver.requestCalls)
}

// Validates: R-2.3.6, R-2.9.1
func TestResolveDeletes_FallsBackToDirectDBWhenControlSocketPathIsUnavailable(t *testing.T) {
	longDataHome := filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8))
	t.Setenv("XDG_DATA_HOME", longDataHome)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

	cid := driveid.MustCanonicalID("personal:no-socket-path@example.com")
	driveID := driveid.New("drive-direct-fallback")

	store, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, store.UpsertHeldDeletes(t.Context(), []syncengine.HeldDeleteRecord{{
		DriveID:       driveID,
		ActionType:    syncengine.ActionRemoteDelete,
		Path:          "delete-me.txt",
		ItemID:        "item-delete",
		State:         syncengine.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.Close(t.Context()))

	var out bytes.Buffer
	cc := &CLIContext{
		OutputWriter: &out,
		Logger:       slog.New(slog.DiscardHandler),
		Cfg:          &config.ResolvedDrive{CanonicalID: cid},
	}

	require.NoError(t, runApproveDeletes(t.Context(), cc))
	assert.Contains(t, out.String(), resolveApproveDeletesSuccess)

	reopened, err := syncengine.NewSyncStore(t.Context(), config.DriveStatePath(cid), slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(t.Context()))
	})

	approved, err := reopened.ListHeldDeletesByState(t.Context(), syncengine.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
}

func TestNotifyDaemon_ReportsControlSocketPathFailureClearly(t *testing.T) {
	longDataHome := filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8))
	t.Setenv("XDG_DATA_HOME", longDataHome)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

	var status bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		StatusWriter: &status,
	}

	notifyDaemon(cc)
	assert.Contains(t, status.String(), "control socket unavailable")
	assert.Contains(t, status.String(), "changes take effect on next daemon start")
}

func TestNotifyDaemon_ReportsAmbiguousProbeFailureClearly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{"))
		assert.NoError(t, err)
	}, func(http.ResponseWriter, *http.Request) {
		t.Fatal("daemon notification should stop before mutating when the owner probe is ambiguous")
	})

	var status bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.DiscardHandler),
		StatusWriter: &status,
	}

	notifyDaemon(cc)
	assert.Contains(t, status.String(), "control socket probe failed")
	assert.Contains(t, status.String(), "changes take effect on next daemon start")
	assert.NotContains(t, status.String(), "no running daemon found")
}

func TestProbeControlOwner_ClassifiesOutcomes(t *testing.T) {
	t.Run("watch owner", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		assert.Equal(t, controlOwnerStateWatchOwner, probe.state)
		require.NotNil(t, probe.client)
	})

	t.Run("one-shot owner", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeOneShot}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		})

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		assert.Equal(t, controlOwnerStateOneShotOwner, probe.state)
		require.NotNil(t, probe.client)
	})

	t.Run("no socket", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())

		probe, err := probeControlOwner(t.Context())
		require.NoError(t, err)
		assert.Equal(t, controlOwnerStateNoSocket, probe.state)
		assert.Nil(t, probe.client)
	})

	t.Run("path unavailable", func(t *testing.T) {
		longDataHome := filepath.Join(t.TempDir(), strings.Repeat("very-long-control-root-", 8))
		t.Setenv("XDG_DATA_HOME", longDataHome)
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("very-long-runtime-root-", 8)))

		probe, err := probeControlOwner(t.Context())
		require.Error(t, err)
		assert.Equal(t, controlOwnerStatePathUnavailable, probe.state)
		assert.Contains(t, err.Error(), "control socket path")
	})

	t.Run("probe failed", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		startCLIControlSocketWithStatusHandler(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "status unavailable", http.StatusInternalServerError)
		}, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		})

		probe, err := probeControlOwner(t.Context())
		require.Error(t, err)
		assert.Equal(t, controlOwnerStateProbeFailed, probe.state)
		assert.Contains(t, err.Error(), "unexpected control status response")
	})
}
