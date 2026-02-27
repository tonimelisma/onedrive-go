package sync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchLoop is the main select loop for Watch(). It processes fsnotify events,
// watcher errors, safety scan ticks, and context cancellation.
func (o *LocalObserver) watchLoop(
	ctx context.Context, watcher FsWatcher, syncRoot string, events chan<- ChangeEvent,
) error {
	safetyTicker := time.NewTicker(safetyScanInterval)
	defer safetyTicker.Stop()

	errBackoff := watchErrInitBackoff

	for {
		select {
		case <-ctx.Done():
			return nil

		case fsEvent, ok := <-watcher.Events():
			if !ok {
				return nil
			}

			o.handleFsEvent(ctx, fsEvent, watcher, syncRoot, events)

			// Successful event resets error backoff.
			errBackoff = watchErrInitBackoff

		case watchErr, ok := <-watcher.Errors():
			if !ok {
				return nil
			}

			o.logger.Warn("filesystem watcher error",
				slog.String("error", watchErr.Error()),
				slog.Duration("backoff", errBackoff),
			)

			// Exponential backoff prevents tight loop under sustained errors
			// (e.g., kernel buffer overflow).
			if sleepErr := timeSleep(ctx, errBackoff); sleepErr != nil {
				return nil
			}

			errBackoff *= watchErrBackoffMult
			if errBackoff > watchErrMaxBackoff {
				errBackoff = watchErrMaxBackoff
			}

		case <-safetyTicker.C:
			o.runSafetyScan(ctx, syncRoot, events)
			errBackoff = watchErrInitBackoff
		}
	}
}

// handleFsEvent processes a single fsnotify event and sends the appropriate
// ChangeEvent to the output channel.
func (o *LocalObserver) handleFsEvent(
	ctx context.Context, fsEvent fsnotify.Event, watcher FsWatcher,
	syncRoot string, events chan<- ChangeEvent,
) {
	// Ignore chmod events — mode changes are not synced.
	if fsEvent.Has(fsnotify.Chmod) && !fsEvent.Has(fsnotify.Create) && !fsEvent.Has(fsnotify.Write) {
		return
	}

	relPath, err := filepath.Rel(syncRoot, fsEvent.Name)
	if err != nil {
		o.logger.Warn("failed to compute relative path",
			slog.String("path", fsEvent.Name), slog.String("error", err.Error()))

		return
	}

	dbRelPath := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(fsEvent.Name))

	if isAlwaysExcluded(name) {
		o.logger.Debug("watch: skipping excluded file", slog.String("name", name), slog.String("path", dbRelPath))
		return
	}

	if !isValidOneDriveName(name) {
		o.logger.Debug("watch: skipping invalid OneDrive name", slog.String("name", name))
		return
	}

	switch {
	case fsEvent.Has(fsnotify.Create):
		o.handleCreate(ctx, fsEvent.Name, dbRelPath, name, watcher, events)

	case fsEvent.Has(fsnotify.Write):
		o.handleWrite(ctx, fsEvent.Name, dbRelPath, name, events)

	case fsEvent.Has(fsnotify.Remove) || fsEvent.Has(fsnotify.Rename):
		o.handleDelete(ctx, dbRelPath, name, events)
	}
}

// handleCreate processes a Create event: stat, hash (files), add watch (dirs).
func (o *LocalObserver) handleCreate(
	ctx context.Context, fsPath, dbRelPath, name string,
	watcher FsWatcher, events chan<- ChangeEvent,
) {
	info, err := os.Stat(fsPath)
	if err != nil {
		// File may have been removed immediately after creation.
		o.logger.Debug("stat failed for created path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	ev := ChangeEvent{
		Source: SourceLocal,
		Type:   ChangeCreate,
		Path:   dbRelPath,
		Name:   name,
		Size:   info.Size(),
		Mtime:  info.ModTime().UnixNano(),
	}

	if info.IsDir() {
		ev.ItemType = ItemTypeFolder

		if addErr := watcher.Add(fsPath); addErr != nil {
			o.logger.Warn("failed to add watch on new directory",
				slog.String("path", dbRelPath), slog.String("error", addErr.Error()))
		}

		// Scan directory contents for files created before the watch was
		// registered. Duplicates from fsnotify are harmless — the buffer's
		// per-path deduplication handles them.
		o.scanNewDirectory(ctx, fsPath, dbRelPath, watcher, events)
	} else {
		ev.ItemType = ItemTypeFile

		hash, hashErr := computeQuickXorHash(fsPath)
		if hashErr != nil {
			o.logger.Warn("hash failed for new file",
				slog.String("path", dbRelPath), slog.String("error", hashErr.Error()))

			return
		}

		ev.Hash = hash
	}

	o.trySend(ctx, events, &ev)
}

// scanNewDirectory walks a newly-created directory and emits ChangeCreate
// events for any files already present. This catches files created between
// the directory's creation and the fsnotify watch registration.
func (o *LocalObserver) scanNewDirectory(
	ctx context.Context, dirPath, dirRelPath string,
	watcher FsWatcher, events chan<- ChangeEvent,
) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		o.logger.Debug("scan new directory failed",
			slog.String("path", dirRelPath), slog.String("error", err.Error()))

		return
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}

		entryName := nfcNormalize(entry.Name())
		if isAlwaysExcluded(entryName) || !isValidOneDriveName(entryName) {
			continue
		}

		entryFsPath := filepath.Join(dirPath, entry.Name())
		entryRelPath := dirRelPath + "/" + entryName

		// Recurse into subdirectories: add watch and scan contents.
		if entry.IsDir() {
			if addErr := watcher.Add(entryFsPath); addErr != nil {
				o.logger.Warn("failed to add watch on nested directory",
					slog.String("path", entryRelPath), slog.String("error", addErr.Error()))
			}

			dirEv := ChangeEvent{
				Source:   SourceLocal,
				Type:     ChangeCreate,
				Path:     entryRelPath,
				Name:     entryName,
				ItemType: ItemTypeFolder,
			}

			o.trySend(ctx, events, &dirEv)

			o.scanNewDirectory(ctx, entryFsPath, entryRelPath, watcher, events)

			continue
		}

		info, statErr := entry.Info()
		if statErr != nil {
			o.logger.Debug("stat failed during directory scan",
				slog.String("path", entryRelPath), slog.String("error", statErr.Error()))

			continue
		}

		hash, hashErr := computeQuickXorHash(entryFsPath)
		if hashErr != nil {
			o.logger.Warn("hash failed during directory scan",
				slog.String("path", entryRelPath), slog.String("error", hashErr.Error()))

			continue
		}

		fileEv := ChangeEvent{
			Source:   SourceLocal,
			Type:     ChangeCreate,
			Path:     entryRelPath,
			Name:     entryName,
			ItemType: ItemTypeFile,
			Size:     info.Size(),
			Hash:     hash,
			Mtime:    info.ModTime().UnixNano(),
		}

		o.trySend(ctx, events, &fileEv)
	}
}

// handleWrite processes a Write event: classify against baseline.
func (o *LocalObserver) handleWrite(
	ctx context.Context, fsPath, dbRelPath, name string, events chan<- ChangeEvent,
) {
	info, err := os.Stat(fsPath)
	if err != nil {
		o.logger.Debug("stat failed for modified path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	// Ignore directory write events — folder mtime changes are noise.
	if info.IsDir() {
		return
	}

	hash, err := computeQuickXorHash(fsPath)
	if err != nil {
		o.logger.Warn("hash failed for modified file",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	// Check baseline — if hash matches, the write was a no-op.
	if existing, ok := o.baseline.GetByPath(dbRelPath); ok && existing.LocalHash == hash {
		return
	}

	ev := ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     dbRelPath,
		Name:     name,
		ItemType: ItemTypeFile,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}

	o.trySend(ctx, events, &ev)
}

// handleDelete processes a Remove/Rename event.
func (o *LocalObserver) handleDelete(
	ctx context.Context, dbRelPath, name string, events chan<- ChangeEvent,
) {
	itemType := ItemTypeFile

	if existing, ok := o.baseline.GetByPath(dbRelPath); ok {
		itemType = existing.ItemType
	}

	ev := ChangeEvent{
		Source:    SourceLocal,
		Type:      ChangeDelete,
		Path:      dbRelPath,
		Name:      name,
		ItemType:  itemType,
		IsDeleted: true,
	}

	o.trySend(ctx, events, &ev)
}

// runSafetyScan performs a full filesystem scan as a safety net, sending any
// detected changes to the events channel. This catches events that fsnotify
// may have missed.
func (o *LocalObserver) runSafetyScan(ctx context.Context, syncRoot string, events chan<- ChangeEvent) {
	o.logger.Debug("running safety scan")

	scanEvents, err := o.FullScan(ctx, syncRoot)
	if err != nil {
		o.logger.Warn("safety scan failed", slog.String("error", err.Error()))
		return
	}

	for i := range scanEvents {
		o.trySend(ctx, events, &scanEvents[i])

		if ctx.Err() != nil {
			return
		}
	}

	o.logger.Debug("safety scan complete", slog.Int("events", len(scanEvents)))
}
