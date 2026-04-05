package sync

import (
	"context"
	"fmt"
	"log/slog"
	"path"

	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const syncMetadataScopeSnapshotKey = "effective_scope_snapshot"

type remoteScopeResult struct {
	observed []synctypes.ObservedItem
	emitted  []synctypes.ChangeEvent
}

func (e *Engine) buildScopeSnapshot(ctx context.Context) (syncscope.Snapshot, error) {
	snapshot, err := syncobserve.BuildScopeSnapshot(ctx, e.syncTree, e.syncScopeConfig, e.logger)
	if err != nil {
		return syncscope.Snapshot{}, fmt.Errorf("build scope snapshot: %w", err)
	}

	return snapshot, nil
}

func (flow *engineFlow) persistedScopeSnapshot(ctx context.Context) (syncscope.Snapshot, error) {
	meta, err := flow.engine.baseline.ReadSyncMetadata(ctx)
	if err != nil {
		return syncscope.Snapshot{}, fmt.Errorf("read persisted scope snapshot: %w", err)
	}

	snapshot, err := syncscope.UnmarshalSnapshot(meta[syncMetadataScopeSnapshotKey])
	if err != nil {
		return syncscope.Snapshot{}, fmt.Errorf("decode persisted scope snapshot: %w", err)
	}

	return snapshot, nil
}

func (flow *engineFlow) persistScopeSnapshot(ctx context.Context, snapshot syncscope.Snapshot) error {
	raw, err := syncscope.MarshalSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("marshal scope snapshot: %w", err)
	}

	if err := flow.engine.baseline.UpsertSyncMetadataEntries(ctx, map[string]string{
		syncMetadataScopeSnapshotKey: raw,
	}); err != nil {
		return fmt.Errorf("persist scope snapshot: %w", err)
	}

	return nil
}

func applyRemoteScope(
	logger *slog.Logger,
	snapshot syncscope.Snapshot,
	events []synctypes.ChangeEvent,
) remoteScopeResult {
	result := remoteScopeResult{
		observed: make([]synctypes.ObservedItem, 0, len(events)),
		emitted:  make([]synctypes.ChangeEvent, 0, len(events)),
	}

	for i := range events {
		ev := events[i]
		if ev.Source != synctypes.SourceRemote {
			result.emitted = append(result.emitted, ev)
			continue
		}

		switch ev.Type {
		case synctypes.ChangeMove:
			oldInScope := snapshot.AllowsPath(ev.OldPath)
			newInScope := snapshot.AllowsPath(ev.Path)

			result.observed = appendObservedEvent(logger, result.observed, &ev, !newInScope)

			switch {
			case oldInScope && newInScope:
				result.emitted = append(result.emitted, ev)
			case oldInScope && !newInScope:
				deleteEv := ev
				deleteEv.Type = synctypes.ChangeDelete
				deleteEv.Path = ev.OldPath
				deleteEv.OldPath = ""
				deleteEv.Name = path.Base(ev.OldPath)
				deleteEv.Hash = ""
				deleteEv.IsDeleted = true
				result.emitted = append(result.emitted, deleteEv)
			case !oldInScope && newInScope:
				createEv := ev
				createEv.Type = synctypes.ChangeCreate
				createEv.OldPath = ""
				createEv.IsDeleted = false
				result.emitted = append(result.emitted, createEv)
			}
		case synctypes.ChangeCreate, synctypes.ChangeModify, synctypes.ChangeDelete, synctypes.ChangeShortcut:
			inScope := snapshot.AllowsPath(ev.Path)
			result.observed = appendObservedEvent(logger, result.observed, &ev, !inScope)
			if inScope {
				result.emitted = append(result.emitted, ev)
			}
		}
	}

	return result
}

func appendObservedEvent(
	logger *slog.Logger,
	items []synctypes.ObservedItem,
	ev *synctypes.ChangeEvent,
	filtered bool,
) []synctypes.ObservedItem {
	if ev.ItemID == "" {
		if logger != nil {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", ev.Path),
			)
		}

		return items
	}

	return append(items, synctypes.ObservedItem{
		DriveID:   ev.DriveID,
		ItemID:    ev.ItemID,
		ParentID:  ev.ParentID,
		Path:      ev.Path,
		ItemType:  ev.ItemType,
		Hash:      ev.Hash,
		Size:      ev.Size,
		Mtime:     ev.Mtime,
		ETag:      ev.ETag,
		IsDeleted: ev.IsDeleted,
		Filtered:  filtered,
	})
}

func (rt *watchRuntime) currentScopeSnapshot() syncscope.Snapshot {
	rt.scopeMu.RLock()
	defer rt.scopeMu.RUnlock()

	return rt.scopeSnapshot
}

func (rt *watchRuntime) setScopeSnapshot(snapshot syncscope.Snapshot) {
	rt.scopeMu.Lock()
	defer rt.scopeMu.Unlock()

	rt.scopeSnapshot = snapshot
}

func (rt *watchRuntime) handleWatchScopeChange(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	change *syncscope.Change,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		p.scopeChanges = nil
		return outbox, false, nil
	}

	rt.setScopeSnapshot(change.New)

	if err := rt.engine.baseline.ApplyRemoteScope(ctx, change.New); err != nil {
		return outbox, false, fmt.Errorf("sync: applying watch scope change: %w", err)
	}

	if change.Diff.HasEntered() {
		if p.mode == synctypes.SyncUploadOnly {
			rt.engine.logger.Info("scope expansion detected during upload-only watch; deferring remote reconciliation",
				slog.Int("entered_paths", len(change.Diff.EnteredPaths)),
			)
			return outbox, false, nil
		}

		rt.runEnteredScopeReconciliationAsync(ctx, p.bl, change.Diff.EnteredPaths)
		return outbox, false, nil
	}

	if err := rt.persistScopeSnapshot(ctx, change.New); err != nil {
		return outbox, false, err
	}

	return outbox, false, nil
}
