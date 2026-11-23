package multisync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// shortcutChildArtifactCleanupClass is the failure class of the first cleanup
// error in err's chain.
func shortcutChildArtifactCleanupClass(err error) (shortcutChildArtifactCleanupFailureClass, bool) {
	var cleanupErr shortcutChildArtifactCleanupError
	if errors.As(err, &cleanupErr) {
		return cleanupErr.class, true
	}
	return "", false
}

// Validates: R-2.4.8
func TestPurgeShortcutChildArtifacts_IgnoresExplicitMountID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountID := "business:user@example.com"
	statePath := config.MountStatePath(mountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("state"), 0o600))
	require.NoError(t, config.UpdateCatalog(t.Context(), func(catalog *config.Catalog) error {
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
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}

	err := executor.purge(context.Background(), shortcutChildArtifactScope{mountID: childMountID})

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupStateArtifactFailure)
	assert.Contains(t, err.Error(), "delete its sync state")
	assert.Contains(t, err.Error(), "state db locked")
}

// A child's state store another engine still has open (another
// configuration's child of the same shortcut, sharing the data directory) is
// not deleted under it. The cleanup fails as a remaining state artifact and
// touches nothing else of the child: its unfinished uploads stay with its
// state until a later attempt finds the store free.
//
// Validates: R-2.4.8, R-6.3.9
func TestShortcutChildArtifactCleanupExecutor_KeepsTheStateWhileAnEngineHoldsIt(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	childMountID := config.ChildMountID("personal:parent@example.com", "binding-held")
	statePath := config.MountStatePathForDataDir(dataDir, childMountID)
	release, err := syncengine.LockDriveForOfflineChange(t.Context(), dataDir, "", statePath,
		slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	var touched []string
	executor := shortcutChildArtifactCleanupExecutor{
		dataDir: dataDir,
		logger:  slog.New(slog.DiscardHandler),
		remove: func(path string) error {
			touched = append(touched, "remove "+path)
			return nil
		},
		deleteUploadSessions: func(string, string) error {
			touched = append(touched, "uploads")
			return nil
		},
	}

	err = executor.purge(t.Context(), shortcutChildArtifactScope{mountID: childMountID, localRoot: t.TempDir()})
	require.ErrorIs(t, err, syncengine.ErrSyncStoreInUse)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupStateArtifactFailure)
	assert.Empty(t, touched, "nothing of a child whose store is in use is cleaned up")

	require.NoError(t, release())
	require.NoError(t, executor.purge(t.Context(), shortcutChildArtifactScope{mountID: childMountID}))
	assert.Len(t, touched, len(config.StateDBFamily(statePath))+1, "a later attempt cleans everything up")
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
		deleteUploadSessions: func(string, string) error {
			return errors.New("session store unavailable")
		},
	}

	err := executor.purge(context.Background(), shortcutChildArtifactScope{mountID: childMountID})

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupUploadSessionFailure)
	assert.Contains(t, err.Error(), "discard its unfinished uploads")
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
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}}},
		map[mountID]shortcutChildAckHandle{},
		nil,
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
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(_ context.Context, ack syncshortcut.ShortcutChildArtifactCleanupAck) (syncshortcut.ShortcutChildWorkSnapshot, error) {
				assert.False(t, ack.Ref.IsZero())
				acked = append(acked, "ack")
				return syncshortcut.ShortcutChildWorkSnapshot{}, nil
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers, nil)

	require.NoError(t, err)
	assert.Len(t, removed, 4)
	require.Len(t, sessions, 1)
	assert.Equal(t, childMountID, sessions[0].mountID)
	assert.Equal(t, []string{"ack"}, acked)
}

// Validates: R-2.4.8
func TestOrchestratorPurgeShortcutChildArtifactsForDecisionsReturnsAckFailureAfterCleanup(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-ack-fail")
	var cleaned []string
	orch := NewOrchestrator(&OrchestratorConfig{
		Logger: slog.New(slog.DiscardHandler),
	})
	orch.artifactCleanup = shortcutChildArtifactCleanupExecutor{
		dataDir: t.TempDir(),
		logger:  orch.logger,
		remove: func(string) error {
			return nil
		},
		deleteUploadSessions: func(mountID string, _ string) error {
			cleaned = append(cleaned, mountID)
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-ack-fail"),
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncshortcut.ShortcutChildArtifactCleanupAck) (syncshortcut.ShortcutChildWorkSnapshot, error) {
				return syncshortcut.ShortcutChildWorkSnapshot{}, errors.New("parent store temporarily unavailable")
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers, nil)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupParentAckFailure)
	assert.Contains(t, err.Error(), "parent store temporarily unavailable")
	assert.Equal(t, []string{childMountID}, cleaned, "the artifacts are cleaned before the acknowledgement fails")
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
		deleteUploadSessions: func(string, string) error {
			return nil
		},
	}
	decisions := &runtimeWorkSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:     childMountID,
			namespaceID: "personal:parent@example.com",
			ackRef:      testShortcutChildAckRef(t, "binding-no-ack"),
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, nil, nil)

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupParentAckFailure)
	assert.Contains(t, err.Error(), "parent mount personal:parent@example.com is unavailable for child artifact cleanup acknowledgement")
	diagnostics := orch.controlStatus(synccontrol.OwnerModeOneShot).ShortcutCleanupFailures
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
		deleteUploadSessions: func(mountID string, _ string) error {
			if mountID == artifactFailMountID {
				return errors.New("session store unavailable")
			}
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
				reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
			},
			{
				mountID:     ackFailMountID,
				namespaceID: "personal:parent@example.com",
				ackRef:      testShortcutChildAckRef(t, "binding-ack-fail"),
				localRoot:   filepath.Join(t.TempDir(), "AckFail"),
				reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
			},
		},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncshortcut.ShortcutChildArtifactCleanupAck) (syncshortcut.ShortcutChildWorkSnapshot, error) {
				return syncshortcut.ShortcutChildWorkSnapshot{}, errors.New("parent store temporarily unavailable")
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers, nil)

	require.Error(t, err)
	diagnostics := orch.shortcutCleanupDiagnosticSnapshot()
	require.Len(t, diagnostics, 2)
	assert.Equal(t, string(shortcutChildArtifactCleanupUploadSessionFailure), diagnostics[0].Class)
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
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, nil, nil)
	require.Error(t, err)
	require.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())

	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncshortcut.ShortcutChildArtifactCleanupAck) (syncshortcut.ShortcutChildWorkSnapshot, error) {
				return syncshortcut.ShortcutChildWorkSnapshot{}, nil
			},
		},
	}
	err = orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers, nil)

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
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}}},
		nil,
		nil,
	)
	require.Error(t, err)
	require.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())

	err = orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{CleanupScopeAllParents: true},
		nil,
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
			reason:      syncshortcut.ShortcutChildArtifactCleanupParentRemoved,
		}}},
		nil,
		nil,
	)
	require.Error(t, err)
	require.NotEmpty(t, orch.shortcutCleanupDiagnosticSnapshot())

	err = orch.purgeShortcutChildArtifactsForDecisions(
		context.Background(),
		&runtimeWorkSet{},
		nil,
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

// childTrashPurgeFixture is a shortcut child whose trash holds a committed,
// a pending and an orphan entry, and an executor that records what it
// removes of the child's state.
type childTrashPurgeFixture struct {
	dataDir      string
	childMountID string
	trash        shortcutChildTrash
	executor     shortcutChildArtifactCleanupExecutor
	removed      *[]string
	entries      map[string]localtrash.Entry
}

func newChildTrashPurgeFixture(t *testing.T, now func() time.Time) childTrashPurgeFixture {
	t.Helper()

	dataDir := t.TempDir()
	childMountID := config.ChildMountID("personal:parent@example.com", "binding-trash")
	trash, err := shortcutChildTrashDirs(dataDir, childMountID)
	require.NoError(t, err)

	childTrash := localtrash.Open(trash.child, now)
	syncRoot := filepath.Join(t.TempDir(), "Shared")
	committed, err := childTrash.Reserve(syncRoot, "committed.txt", localtrash.ReasonReleased)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(committed.ItemPath, []byte("committed\n"), 0o600))
	_, err = committed.Commit()
	require.NoError(t, err)
	pending, err := childTrash.Reserve(syncRoot, "pending.txt", localtrash.ReasonDeleted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pending.ItemPath, []byte("pending\n"), 0o600))
	orphanDir := filepath.Join(trash.child, "20260901T000000.000000000Z-0badf00d")
	require.NoError(t, os.MkdirAll(orphanDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(orphanDir, "item"), []byte("orphan\n"), 0o600))

	var removed []string
	return childTrashPurgeFixture{
		dataDir:      dataDir,
		childMountID: childMountID,
		trash:        trash,
		executor: shortcutChildArtifactCleanupExecutor{
			dataDir: dataDir,
			logger:  slog.New(slog.DiscardHandler),
			remove: func(path string) error {
				removed = append(removed, path)
				return nil
			},
			deleteUploadSessions: func(string, string) error { return nil },
		},
		removed: &removed,
		entries: trashEntriesByID(t, localtrash.Open(trash.child, now)),
	}
}

// trashEntriesByID lists trash by entry ID, without where each file sits,
// which a hand-over changes.
func trashEntriesByID(t *testing.T, trash *localtrash.Trash) map[string]localtrash.Entry {
	t.Helper()

	listed, err := trash.List()
	require.NoError(t, err)
	byID := make(map[string]localtrash.Entry, len(listed))
	for i := range listed {
		entry := listed[i]
		entry.ItemPath = ""
		byID[entry.ID] = entry
	}
	return byID
}

// The purge of a removed shortcut child hands its trash over to its parent
// drive's before it removes the child's state: every entry, committed,
// pending or orphan, is in the parent's trash with its ID, time, reason and
// state, the child's trash directory is gone, and the parent's purge expires
// the committed entry once the retention has passed.
//
// Validates: R-2.4.8, R-6.4.12
func TestShortcutChildArtifactCleanupExecutor_HandsTheChildsTrashToItsDrive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	fixture := newChildTrashPurgeFixture(t, clock)
	require.Len(t, fixture.entries, 3)

	err := fixture.executor.purge(t.Context(), shortcutChildArtifactScope{mountID: fixture.childMountID})

	require.NoError(t, err)
	assert.NoDirExists(t, fixture.trash.child, "no trash is left that only the removed child reached")
	parentTrash := localtrash.Open(fixture.trash.parent, clock)
	assert.Equal(t, fixture.entries, trashEntriesByID(t, parentTrash), "every entry is the drive's now, as it was")
	assert.Len(t, *fixture.removed, len(config.StateDBFamily(config.MountStatePathForDataDir(fixture.dataDir, fixture.childMountID))),
		"the child's state is removed after the hand-over")

	now = now.Add(localtrash.Retention + time.Hour)
	expired, err := parentTrash.Purge()
	require.NoError(t, err)
	assert.Equal(t, 1, expired, "the committed entry expires with the drive's own")
	assert.Len(t, trashEntriesByID(t, parentTrash), 2, "what was never committed is never purged")
}

// A hand-over that fails is a cleanup failure of the state kind: the child's
// trash stays as it was, nothing of its state is removed, and the parent
// publishes the cleanup again; the next attempt hands everything over.
//
// Validates: R-2.4.8, R-6.4.12
func TestShortcutChildArtifactCleanupExecutor_KeepsTheChildWhenItsTrashCannotBeHandedOver(t *testing.T) {
	t.Parallel()

	fixture := newChildTrashPurgeFixture(t, nil)
	require.NoError(t, os.MkdirAll(filepath.Dir(fixture.trash.parent), 0o700))
	require.NoError(t, os.WriteFile(fixture.trash.parent, []byte("not a folder"), 0o600))

	err := fixture.executor.purge(t.Context(), shortcutChildArtifactScope{mountID: fixture.childMountID})

	require.Error(t, err)
	assertShortcutCleanupClass(t, err, shortcutChildArtifactCleanupStateArtifactFailure)
	assert.Contains(t, err.Error(), "keep its trash")
	assert.Equal(t, fixture.entries, trashEntriesByID(t, localtrash.Open(fixture.trash.child, nil)),
		"the child's trash is as it was")
	assert.Empty(t, *fixture.removed, "nothing of the child's state is removed")

	require.NoError(t, os.Remove(fixture.trash.parent))
	require.NoError(t, fixture.executor.purge(t.Context(), shortcutChildArtifactScope{mountID: fixture.childMountID}))
	assert.NoDirExists(t, fixture.trash.child)
	assert.Equal(t, fixture.entries, trashEntriesByID(t, localtrash.Open(fixture.trash.parent, nil)))
}
