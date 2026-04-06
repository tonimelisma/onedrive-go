package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type fakeScopeInspector struct {
	snapshot syncstore.ScopeStateSnapshot
	err      error
}

func (f *fakeScopeInspector) ReadScopeStateSnapshot(context.Context) (syncstore.ScopeStateSnapshot, error) {
	return f.snapshot, f.err
}

func (f *fakeScopeInspector) Close() error { return nil }

// Validates: R-2.4.4, R-2.4.5
func TestSyncScopeService_RunListJSON_PredictsGenerationAndPendingReentry(t *testing.T) {
	setTestDriveHome(t)
	syncDir := t.TempDir()
	canonicalID := driveid.MustCanonicalID("personal:test@example.com")
	liveSnapshot, err := syncscope.NewSnapshot(syncscope.Config{}, nil)
	require.NoError(t, err)

	persistedSnapshot, err := syncscope.NewSnapshot(syncscope.Config{
		IgnoreMarker: ".odignore",
	}, []string{"blocked"})
	require.NoError(t, err)
	persistedJSON, err := syncscope.MarshalSnapshot(persistedSnapshot)
	require.NoError(t, err)

	rdConfig := &configResolvedDriveForScopeTest{
		CanonicalID: canonicalID,
		SyncDir:     syncDir,
		Websocket:   true,
	}
	rd := rdConfig.resolved()
	require.NoError(t, os.MkdirAll(filepath.Dir(rd.StatePath()), 0o700))
	require.NoError(t, os.WriteFile(rd.StatePath(), []byte("placeholder"), 0o600))

	var out bytes.Buffer
	service := &syncScopeService{
		cc: &CLIContext{
			Flags:        CLIFlags{JSON: true},
			Logger:       slog.New(slog.DiscardHandler),
			OutputWriter: &out,
			Cfg:          rd,
		},
		openInspector: func(string, *slog.Logger) (scopeStateInspector, error) {
			return &fakeScopeInspector{
				snapshot: syncstore.ScopeStateSnapshot{
					Found:                 true,
					Generation:            3,
					EffectiveSnapshotJSON: persistedJSON,
					LastReconcileKind:     synctypes.ScopeReconcileEnteredPath,
				},
			}, nil
		},
		buildScopeSnapshot: func(context.Context, *synctree.Root, syncscope.Config, *slog.Logger) (syncscope.Snapshot, error) {
			return liveSnapshot, nil
		},
	}

	require.NoError(t, service.runList(context.Background()))

	var model syncScopeModel
	require.NoError(t, json.Unmarshal(out.Bytes(), &model))
	assert.Equal(t, int64(4), model.Generation)
	assert.Equal(t, synctypes.ScopeObservationRootDelta, model.ObservationMode)
	assert.True(t, model.WebsocketEnabled)
	assert.True(t, model.PendingReentry)
	assert.Equal(t, []string{"/"}, model.SyncPaths)
}

// Validates: R-2.4.4, R-2.4.5
func TestSyncScopeService_RunExplainJSON_ClassifiesMarkerExclusion(t *testing.T) {
	setTestDriveHome(t)
	syncDir := t.TempDir()
	canonicalID := driveid.MustCanonicalID("personal:test@example.com")
	liveSnapshot, err := syncscope.NewSnapshot(syncscope.Config{
		SyncPaths:    []string{"/Docs/report.txt"},
		IgnoreMarker: ".odignore",
	}, []string{"Docs/Private"})
	require.NoError(t, err)

	rdConfig := &configResolvedDriveForScopeTest{
		CanonicalID:  canonicalID,
		SyncDir:      syncDir,
		SyncPaths:    []string{"/Docs/report.txt"},
		IgnoreMarker: ".odignore",
	}
	rd := rdConfig.resolved()
	require.NoError(t, os.MkdirAll(filepath.Dir(rd.StatePath()), 0o700))
	require.NoError(t, os.WriteFile(rd.StatePath(), []byte("placeholder"), 0o600))

	var out bytes.Buffer
	service := &syncScopeService{
		cc: &CLIContext{
			Flags:        CLIFlags{JSON: true},
			Logger:       slog.New(slog.DiscardHandler),
			OutputWriter: &out,
			Cfg:          rd,
		},
		openInspector: func(string, *slog.Logger) (scopeStateInspector, error) {
			return &fakeScopeInspector{}, nil
		},
		buildScopeSnapshot: func(context.Context, *synctree.Root, syncscope.Config, *slog.Logger) (syncscope.Snapshot, error) {
			return liveSnapshot, nil
		},
	}

	require.NoError(t, service.runExplain(context.Background(), "Docs/Private/secret.txt"))

	var explain syncScopeExplainModel
	require.NoError(t, json.Unmarshal(out.Bytes(), &explain))
	assert.Equal(t, scopeExplainExcludedByMarker, explain.Status)
	assert.Equal(t, "/Docs/Private/secret.txt", explain.EffectivePath)
	assert.Equal(t, "/Docs/Private", explain.MatchingRule)
	assert.Equal(t, synctypes.ScopeObservationScopedDelta, explain.ObservationMode)
	assert.False(t, explain.WebsocketEnabled)
}

type configResolvedDriveForScopeTest struct {
	CanonicalID  driveid.CanonicalID
	SyncDir      string
	SyncPaths    []string
	IgnoreMarker string
	Websocket    bool
}

func (c *configResolvedDriveForScopeTest) resolved() *config.ResolvedDrive {
	return &config.ResolvedDrive{
		CanonicalID: c.CanonicalID,
		SyncDir:     c.SyncDir,
		FilterConfig: config.FilterConfig{
			SyncPaths:    append([]string(nil), c.SyncPaths...),
			IgnoreMarker: c.IgnoreMarker,
		},
		SyncConfig: config.SyncConfig{
			Websocket: c.Websocket,
		},
	}
}
