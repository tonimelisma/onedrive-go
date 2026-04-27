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
		logger: slog.New(slog.DiscardHandler),
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
	assert.Contains(t, err.Error(), "remove child state artifact")
	assert.Contains(t, err.Error(), "state db locked")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_ReturnsCatalogFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-catalog-fail")
	executor := shortcutChildArtifactCleanupExecutor{
		logger: slog.New(slog.DiscardHandler),
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
	assert.Contains(t, err.Error(), "catalog write denied")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_ReturnsUploadSessionFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-session-fail")
	executor := shortcutChildArtifactCleanupExecutor{
		logger: slog.New(slog.DiscardHandler),
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
	assert.Contains(t, err.Error(), "delete child upload sessions")
	assert.Contains(t, err.Error(), "session store unavailable")
}

// Validates: R-2.4.8
func TestShortcutChildArtifactCleanupExecutor_RequiresInjectedDependencies(t *testing.T) {
	t.Parallel()

	childMountID := config.ChildMountID("personal:parent@example.com", "binding-missing-executor")
	err := shortcutChildArtifactCleanupExecutor{}.purge(
		context.Background(),
		shortcutChildArtifactScope{mountID: childMountID},
	)

	require.Error(t, err)
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
		logger: orch.logger,
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
	decisions := &runnerDecisionSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:       childMountID,
			namespaceID:   "personal:parent@example.com",
			bindingItemID: "binding-cleanup",
			localRoot:     filepath.Join(t.TempDir(), "Shortcut"),
		}},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(_ context.Context, ack syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildRunnerPublication, error) {
				acked = append(acked, ack.BindingItemID)
				return syncengine.ShortcutChildRunnerPublication{}, nil
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers)

	require.NoError(t, err)
	assert.Len(t, removed, 4)
	assert.Equal(t, []string{childMountID}, pruned)
	require.Len(t, sessions, 1)
	assert.Equal(t, childMountID, sessions[0].mountID)
	assert.Equal(t, []string{"binding-cleanup"}, acked)
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
		logger: orch.logger,
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
	decisions := &runnerDecisionSet{
		CleanupChildren: []shortcutChildArtifactCleanup{{
			mountID:       childMountID,
			namespaceID:   "personal:parent@example.com",
			bindingItemID: "binding-ack-fail",
		}},
	}
	ackers := map[mountID]shortcutChildAckHandle{
		"personal:parent@example.com": mockShortcutChildAckHandle{
			ackCleanupFn: func(context.Context, syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildRunnerPublication, error) {
				return syncengine.ShortcutChildRunnerPublication{}, errors.New("parent store temporarily unavailable")
			},
		},
	}

	err := orch.purgeShortcutChildArtifactsForDecisions(context.Background(), decisions, ackers)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent store temporarily unavailable")
	assert.Equal(t, []string{childMountID}, pruned)
}
