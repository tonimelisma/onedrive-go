package sync

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// spellingRenameLeftover is an entry found under the temporary sibling name
// of a spelling-only local rename, and the path it was on its way to. Both
// are relative to the sync root and spelled as on disk.
type spellingRenameLeftover struct {
	path      string
	plainPath string
}

// restoreSpellingRenameLeftovers is the engine's start-of-run put-back of what
// an interrupted spelling-only rename left (the function of that name). Only
// a live run calls it: a dry run changes nothing on disk.
func (e *Engine) restoreSpellingRenameLeftovers(ctx context.Context) {
	restoreSpellingRenameLeftovers(ctx, e.syncTree, e.contentFilter, e.protectedRoots.current(), e.logger)
}

// restoreSpellingRenameLeftovers puts back, under its plain name, every entry
// a spelling-only local rename left under its temporary sibling name
// (syncdomain.SpellingRenameArtifactPrefix): a crash between the rename's two
// steps, or a put-back that failed, leaves the user's file or folder there,
// and local observation never sees that name. It runs at the start of a live
// run, before the first plan, so the plan reads the entry where it is; the
// rename it belonged to is then planned again from truth.
//
// The move back is the rooted no-replace rename, so it never replaces or
// deletes anything. A plain name that is taken (an entry OneDrive treats as
// the same name is in the folder), a temporary name that does not say which
// name it was on its way to, and a move back that fails each keep the
// leftover where it is, with a Warn naming both paths; the next start tries
// again. Leftovers in folders sync does not observe (excluded by the content
// filter, a shared folder's own mount, another filesystem it does not cross)
// are not sync's to move and are left alone. None of this fails the run.
func restoreSpellingRenameLeftovers(
	ctx context.Context,
	tree *synctree.Root,
	filter syncdomain.ContentFilterConfig,
	protectedRoots []syncshortcut.ProtectedRoot,
	logger *slog.Logger,
) {
	leftovers := findSpellingRenameLeftovers(ctx, tree, filter, protectedRoots, logger)
	for i := range leftovers {
		if ctx.Err() != nil {
			return
		}
		restoreSpellingRenameLeftover(tree, leftovers[i], logger)
	}
}

// findSpellingRenameLeftovers walks the folders local observation walks and
// returns the leftovers it can put back. It reads names only, plus one lstat
// per folder for its filesystem; a folder it cannot read is the local scan's
// to report, so it is skipped at Debug.
func findSpellingRenameLeftovers(
	ctx context.Context,
	tree *synctree.Root,
	filter syncdomain.ContentFilterConfig,
	protectedRoots []syncshortcut.ProtectedRoot,
	logger *slog.Logger,
) []spellingRenameLeftover {
	contentFilter := syncdomain.NewContentFilter(filter)
	protected := make(map[string]struct{}, len(protectedRoots))
	for i := range protectedRoots {
		if protectedRoots[i].Path != "" {
			protected[protectedRoots[i].Path] = struct{}{}
		}
	}

	var leftovers []spellingRenameLeftover
	var rootDevice uint64
	walkErr := tree.WalkDir(func(fsPath string, d fs.DirEntry, entryErr error) error {
		if ctx.Err() != nil {
			// A run that is stopping moves nothing.
			return filepath.SkipAll
		}
		if entryErr != nil {
			logger.Debug("could not check a folder for entries left by an interrupted rename",
				slog.String("path", fsPath), slog.Any("error", entryErr))
			return nil
		}
		rel, err := tree.Rel(fsPath)
		if err != nil {
			logger.Debug("could not check a path for an entry left by an interrupted rename",
				slog.String("path", fsPath), slog.Any("error", err))
			return nil
		}
		if rel == "." {
			rootDevice = entryDevice(d)
			return nil
		}

		if leftover, isLeftover := spellingRenameLeftoverAt(rel, d, contentFilter, logger); isLeftover {
			if leftover.plainPath != "" {
				leftovers = append(leftovers, leftover)
			}
			// Whatever is inside a leftover folder moves with it.
			return skipEntry(d)
		}
		if d.IsDir() && !walksFolderForSpellingLeftovers(rel, d, contentFilter, protected, filter.CrossFilesystems, rootDevice) {
			return filepath.SkipDir
		}

		return nil
	})
	if ctx.Err() != nil {
		return nil
	}
	if walkErr != nil {
		logger.Debug("could not walk the sync folder for entries left by an interrupted rename", slog.Any("error", walkErr))
	}

	return leftovers
}

// spellingRenameLeftoverAt reports whether the entry at rel has a temporary
// spelling-rename name, and returns it with the path to put it back at, or
// with no plainPath when it is to stay: an entry that is neither a file nor
// a folder (no rename made it), a name that does not say where it was going
// (warned about), or a plain path the content filter excludes.
func spellingRenameLeftoverAt(
	rel string, d fs.DirEntry, contentFilter syncdomain.ContentFilter, logger *slog.Logger,
) (spellingRenameLeftover, bool) {
	if !strings.HasPrefix(d.Name(), syncdomain.SpellingRenameArtifactPrefix) {
		return spellingRenameLeftover{}, false
	}
	leftover := spellingRenameLeftover{path: rel}
	if !d.IsDir() && !d.Type().IsRegular() {
		return leftover, true
	}
	plainName, known := syncdomain.SpellingRenameArtifactPlainName(d.Name())
	if !known {
		logger.Warn("Kept "+rel+": an interrupted rename left it under a temporary name that does not say "+
			"which name it had; rename it yourself to keep it",
			slog.String("leftover", rel))
		return leftover, true
	}
	plainPath := filepath.Join(filepath.Dir(rel), plainName)
	itemType := syncdomain.ItemTypeFile
	if d.IsDir() {
		itemType = syncdomain.ItemTypeFolder
	}
	if !contentFilter.Visible(syncdomain.StoredPathOf(plainPath), itemType) {
		logger.Debug("leaving an entry under a temporary rename name whose plain name sync does not sync",
			slog.String("leftover", rel), slog.String("path", plainPath))
		return leftover, true
	}
	leftover.plainPath = plainPath

	return leftover, true
}

// walksFolderForSpellingLeftovers reports whether the leftover walk enters the
// folder at rel: only a folder local observation would enter, so nothing sync
// does not own is moved.
func walksFolderForSpellingLeftovers(
	rel string, d fs.DirEntry, contentFilter syncdomain.ContentFilter, protected map[string]struct{},
	crossFilesystems bool, rootDevice uint64,
) bool {
	stored := syncdomain.StoredPathOf(rel)
	if _, isProtected := protected[stored]; isProtected {
		return false
	}
	if !contentFilter.Visible(stored, syncdomain.ItemTypeFolder) {
		return false
	}
	if crossFilesystems || rootDevice == 0 {
		return true
	}
	device := entryDevice(d)

	return device == 0 || device == rootDevice
}

// entryDevice is the device of the entry, or zero when it cannot be read.
func entryDevice(d fs.DirEntry) uint64 {
	info, err := d.Info()
	if err != nil {
		return 0
	}
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return 0
	}

	return identity.Device
}

// skipEntry stops the walk from entering d when it is a folder.
func skipEntry(d fs.DirEntry) error {
	if d.IsDir() {
		return filepath.SkipDir
	}

	return nil
}

// restoreSpellingRenameLeftover moves one leftover back to its plain path
// when the folder holds no entry OneDrive treats as that name. The check
// keeps a name another spelling holds (a volume that tells the spellings
// apart) from becoming two names of one OneDrive name; the no-replace rename
// refuses whatever took the name after the check.
func restoreSpellingRenameLeftover(tree *synctree.Root, leftover spellingRenameLeftover, logger *slog.Logger) {
	attrs := []any{slog.String("path", leftover.plainPath), slog.String("leftover", leftover.path)}
	keptTaken := "Kept " + leftover.path + ": an interrupted rename left it under this temporary name, and " +
		leftover.plainPath + " is taken; rename or merge it yourself to keep it"

	entries, err := tree.ReadDir(filepath.Dir(leftover.plainPath))
	if err != nil {
		logger.Warn("Kept "+leftover.path+": an interrupted rename left it under this temporary name, and its "+
			"folder could not be read to put it back as "+leftover.plainPath,
			append(attrs, slog.Any("error", err))...)
		return
	}
	match := namekey.Find(entries, filepath.Base(leftover.plainPath), func(entry *fs.DirEntry) (string, bool) {
		return (*entry).Name(), true
	})
	if match.Found() || match.Ambiguous {
		logger.Warn(keptTaken, attrs...)
		return
	}

	err = tree.RenameNoReplace(leftover.path, leftover.plainPath)
	switch {
	case err == nil:
		logger.Info("put back "+leftover.plainPath+", which an interrupted rename had left under a temporary name",
			attrs...)
	case errors.Is(err, synctree.ErrSourceNotRemoved):
		logger.Warn("Put back "+leftover.plainPath+", but could not remove the temporary name an interrupted "+
			"rename had left it under", append(attrs, slog.Any("error", err))...)
	case errors.Is(err, synctree.ErrTargetExists):
		logger.Warn(keptTaken, attrs...)
	default:
		logger.Warn("Kept "+leftover.path+": an interrupted rename left it under this temporary name, and "+
			"putting it back as "+leftover.plainPath+" failed", append(attrs, slog.Any("error", err))...)
	}
}
