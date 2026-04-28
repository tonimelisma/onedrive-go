package multisync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// Validates: R-2.4.8
func TestPurgeShortcutChildArtifacts_IgnoresExplicitMountID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountID := "business:user@example.com"
	statePath := config.MountStatePath(mountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("state"), 0o600))
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.Drives[mountID] = config.CatalogDrive{CanonicalID: mountID, DisplayName: "Explicit shared drive"}
		return nil
	}))

	executor := newShortcutChildArtifactCleanupExecutor(slog.New(slog.DiscardHandler), config.DefaultDataDir())
	err := executor.purge(context.Background(), shortcutChildArtifactScope{mountID: mountID})

	require.NoError(t, err)
	assert.FileExists(t, statePath)
	catalog, err := config.LoadCatalog()
	require.NoError(t, err)
	assert.Contains(t, catalog.Drives, mountID)
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_ReturnsStateArtifactRemoveFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-remove-fail")
	removeErr := errors.New("state db locked")
	executor := shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  slog.New(slog.DiscardHandler),
		remove: func(string) error {
			return removeErr
		},
		pruneCatalogRecord: func(string) error {
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}

	err := executor.purge(context.Background(), shortcutChildArtifactScope{mountID: childMountID})

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupStateArtifactFailure)
	assert.Contains(t, err.Error(), "remove child state artifact")
	assert.Contains(t, err.Error(), "state db locked")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_ReturnsCatalogFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-catalog-fail")
	executor := shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  slog.New(slog.DiscardHandler),
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(string) error {
			return errors.New("catalog write denied")
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}

	err := executor.purge(context.Background(), shortcutChildArtifactScope{mountID: childMountID})

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupCatalogFailure)
	assert.Contains(t, err.Error(), "catalog write denied")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_ReturnsUploadSessionFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-session-fail")
	executor := shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  slog.New(slog.DiscardHandler),
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(string) error {
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return errors.New("session store unavailable")
		},
	}

	err := executor.purge(context.Background(), shortcutChildArtifactScope{mountID: childMountID})

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupUploadSessionFailure)
	assert.Contains(t, err.Error(), "delete child upload sessions")
	assert.Contains(t, err.Error(), "session store unavailable")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_RequiresDataDir(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-missing-executor")
	err := shortcutChildArtifactCleanupExecutor{}.purge(
		context.Background(),
		shortcutChildArtifactScope{mountID: childMountID},
	)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupSetupFailure)
	assert.Contains(t, err.Error(), "cleanup executor data dir is not configured")
}

// Validates: R-2.4.8
func TestOrchestratorCleanupWithEmptyDataDirFailsLoudly(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-empty-data-dir")
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	err := orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-empty-data-dir"),
			localRoot:   filepath.Join(t.TempDir(), "Shortcut"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}}},
		map[mountID]shortcutChildAckHandle{},
	)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupSetupFailure)
	assert.Contains(t, err.Error(), "cleanup executor data dir is not configured")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_RequiresInjectedDependencies(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-missing-executor")
	err := shortcutChildArtifactCleanupExecutor{dataDir: t.TempDir()}.purge(
		context.Background(),
		shortcutChildArtifactScope{mountID: childMountID},
	)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupSetupFailure)
	assert.Contains(t, err.Error(), "cleanup executor is not configured")
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsForDecisionsUsesInjectedExecutor(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-cleanup")
	var removed []string
	var pruned []string
	var sessions []shortcutChildArtifactScope
	var acked []string
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(path string) error {
			removed = append(removed, path)
			return nil
		},
		pruneCatalogRecord: func(mountID string) error {
			pruned = append(pruned, mountID)
			return nil
		},
		deleteUploadSessions: func(mountID string, localRoot string) error {
			sessions = append(sessions, shortcutChildArtifactScope{mountID: mountID, localRoot: localRoot})
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-cleanup"),
			localRoot:   filepath.Join(t.TempDir(), "Shortcut"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(_ context.Context, ack syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildWorkSnapshot, error) {
				assert.False(t, ack.Ref.IsZero())
				acked = append(acked, "ack")
				return syncengine.ShortcutChildWorkSnapshot{}, nil
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers)

	require.NoError(t, err)
	assert.Len(t, removed, 4)
	assert.Equal(t, []string{childMountID}, pruned)
	require.Len(t, sessions, 1)
	assert.Equal(t, childMountID, sessions[0].mountID)
	assert.Equal(t, []string{"ack"}, acked)
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsForDecisionsReturnsAckFailureAfterCleanup(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-ack-fail")
	var pruned []string
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(mountID string) error {
			pruned = append(pruned, mountID)
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-ack-fail"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildWorkSnapshot, error) {
				return syncengine.ShortcutChildWorkSnapshot{}, errors.New("parent store temporarily unavailable")
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupParentAckFailure)
	assert.Contains(t, err.Error(), "parent store temporarily unavailable")
	assert.Equal(t, []string{childMountID}, pruned)
	diagnostics := orch.shortcutCleanupDiagnosticSnapshot()
	require.Len(t, diagnostics, 1)
	assert.Equal(t, string(shortcutChildArtifactCleanupParentAckFailure), diagnostics[0].Class)
	assert.Equal(t, shortcutCleanupDiagnosticParentAckFailed, diagnostics[0].Phase)
	assert.Contains(t, diagnostics[0].Message, "parent store temporarily unavailable")
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsForDecisionsRequiresLiveParentAck(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-no-ack")
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(string) error {
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-no-ack"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, nil)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupParentAckFailure)
	assert.Contains(t, err.Error(), "parent mount personal:parent@example.com is unavailable for child artifact cleanup acknowledgement")
	diagnostics := orch.controlStatus(context.Background(), synccontrol.OwnerModeOneShot).ShortcutCleanupFailures
	require.Len(t, diagnostics, 1)
	assert.Equal(t, shortcutCleanupDiagnosticParentAckFailed, diagnostics[0].Phase)
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsRecordsMixedCleanupDiagnostics(t *testing.T) {
	t.Parallel()

	artifactFailMountID := config.ChildMountID("personal:parent@example.com", "binding-artifact-fail")
	ackFailMountID := config.ChildMountID("personal:parent@example.com", "binding-ack-fail")
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(mountID string) error {
			if mountID == artifactFailMountID {
				return errors.New("catalog write denied")
			}
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{
			{
				mountID:     artifactFailMountID,
				namespaceID: "personal:parent@example.com",
				ackRef:      testShortcutChildAckRef(t, "binding-artifact-fail"),
				localRoot:   filepath.Join(t.TempDir(), "ArtifactFail"),
				reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
			},
			{
				mountID:     ackFailMountID,
				namespaceID: "personal:parent@example.com",
				ackRef:      testShortcutChildAckRef(t, "binding-ack-fail"),
				localRoot:   filepath.Join(t.TempDir(), "AckFail"),
				reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
			},
		},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildWorkSnapshot, error) {
				return syncengine.ShortcutChildWorkSnapshot{}, errors.New("parent store temporarily unavailable")
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers)

	require.Error(t, err)
	diagnostics := orch.shortcutCleanupDiagnosticSnapshot()
	require.Len(t, diagnostics, 2)
	assert.Equal(t, string(shortcutChildArtifactCleanupCatalogFailure), diagnostics[0].Class)
	assert.Equal(t, shortcutCleanupDiagnosticArtifactsRemaining, diagnostics[0].Phase)
	assert.Equal(t, string(shortcutChildArtifactCleanupParentAckFailure), diagnostics[1].Class)
	assert.Equal(t, shortcutCleanupDiagnosticParentAckFailed, diagnostics[1].Phase)
	assert.Contains(t, diagnostics[1].Message, "parent store temporarily unavailable")
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsClearsDiagnosticsAfterSuccessfulRetry(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-retry-success")
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(string) error {
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-retry-success"),
			localRoot:   filepath.Join(t.TempDir(), "RetrySuccess"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, nil)
	require.Error(t, err)
	require.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())

	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildWorkSnapshot, error) {
				return syncengine.ShortcutChildWorkSnapshot{}, nil
			},
		},
	}
	err = orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers)

	require.NoError(t, err)
	assert.Empty(t, orch.shortcutCleanupDiagnosticSnapshot())
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsClearsDiagnosticsWhenNoCleanupWorkRemains(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-no-work-remains")
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(string) error {
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	err := orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-no-work-remains"),
			localRoot:   filepath.Join(t.TempDir(), "NoWorkRemains"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}}},
		nil,
	)
	require.Error(t, err)
	require.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())

	err = orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{CleanupScopeAllParents: true},
		nil,
	)

	require.NoError(t, err)
	assert.Empty(t, orch.shortcutCleanupDiagnosticSnapshot())
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsKeepsDiagnosticsForParentScopedNoCleanupWork(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent-a@example.com", "binding-parent-a")
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		pruneCatalogRecord: func(string) error {
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	err := orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent-a@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-parent-a"),
			localRoot:   filepath.Join(t.TempDir(), "ParentA"),
			reason:      syncengine.ShortcutChildArtifactCleanupParentRemoved,
		}}},
		nil,
	)
	require.Error(t, err)
	require.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())

	err = orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{},
		nil,
	)

	require.NoError(t, err)
	assert.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())
}

func assertShortcutCleanupClass(
	t *testing.T,
	err error,
	want shortcutChildArtifactCleanupFailureClass,
) {
	t.Helper()

	got, ok := shortcutChildArtifactCleanupClass(err)
	require.True(t, ok, "cleanup error class missing from %v", err)
	assert.Equal(t, want, got)
}
