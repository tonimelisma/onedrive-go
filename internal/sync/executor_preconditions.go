package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// errRemoteMoveAlreadyApplied reports that the item a remote move targets
// already sits at the planned destination: the work is done, not stale.
var errRemoteMoveAlreadyApplied = errors.New("remote move already applied")

// remoteItemGoneError reports that a live read found a remote item missing:
// positive evidence it was deleted, even when delta never said so (an item
// moved into a folder that was then deleted is not reported again). It wraps
// errActionPreconditionChanged, so the action is superseded as before, and
// the engine also drops the item from remote_state so the next plan stops
// targeting it instead of superseding the same action on every pass.
type remoteItemGoneError struct {
	op     string
	itemID string
	role   string
}

func (e *remoteItemGoneError) Error() string {
	return fmt.Sprintf("%s: %s remote %s %s is missing", errActionPreconditionChanged, e.op, e.role, e.itemID)
}

func (e *remoteItemGoneError) Unwrap() error { return errActionPreconditionChanged }

func stalePreconditionError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errActionPreconditionChanged, fmt.Sprintf(format, args...))
}

func (e *executor) validateRemoteSourcePrecondition(
	ctx context.Context,
	driveID driveid.ID,
	action *Action,
	op string,
) error {
	_, err := e.remoteSourcePreconditionETag(ctx, driveID, action, op)
	return err
}

func (e *executor) remoteSourcePreconditionETag(
	ctx context.Context,
	driveID driveid.ID,
	action *Action,
	op string,
) (string, error) {
	if action == nil || action.ItemID == "" {
		return "", nil
	}

	item, err := e.items.GetItem(ctx, driveID, action.ItemID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return "", &remoteItemGoneError{op: op, itemID: action.ItemID, role: "item"}
		}
		return "", fmt.Errorf("checking remote %s precondition for %s: %w", op, action.Path, err)
	}

	row := remoteStateRowFromGraphItem(item)
	planned := plannedRemoteStateForPrecondition(action)
	// Already at the destination with the same identity and content: moving
	// it (or the parent that carried it) churned its eTag, so eTag is not
	// compared here.
	if e.remoteMoveAlreadyApplied(action, item, row, planned) {
		return item.ETag, errRemoteMoveAlreadyApplied
	}
	if planned != nil && !remoteRowMatchesLivePreconditionForAction(action, row, planned) {
		return "", stalePreconditionError("%s remote item %s changed since planning", op, action.ItemID)
	}
	if expectedPath := e.expectedRemotePreconditionPath(action); expectedPath != "" {
		if at, known := e.remoteItemIsAt(item, expectedPath); known && !at {
			return "", stalePreconditionError("%s remote item %s moved from %s", op, action.ItemID, expectedPath)
		}
	}

	return item.ETag, nil
}

func (e *executor) remoteMoveAlreadyApplied(action *Action, item *graph.Item, row *remoteStateRow, planned *remoteState) bool {
	if action.Type != ActionRemoteMove {
		return false
	}
	// Mount-root engines plan in another coordinate system; see
	// expectedRemotePreconditionPath.
	if e.expectedRemotePreconditionPath(action) == "" {
		return false
	}
	if at, known := e.remoteItemIsAt(item, action.Path); !known || !at {
		return false
	}

	return planned == nil || remoteRowMatchesLivePrecondition(row, planned, false)
}

// remoteItemIsAt reports whether a live item sits at relPath: relPath's name
// in NFC, under the parent the baseline records for relPath's folder. When
// the baseline does not know that parent, the item's verbatim
// parentReference.path decides (the drive root for a top-level path). known
// is false when neither can be established, and callers then skip the check.
//
// The location used to be a path rebuilt from a URL-decoded
// parentReference.path; L-32 showed Graph does not encode it, so a folder
// literally named "a%20b" read as "a b", and a remote NFD spelling (L-04)
// as a move.
func (e *executor) remoteItemIsAt(item *graph.Item, relPath string) (at, known bool) {
	if item == nil || item.Name == "" {
		return false, false
	}
	if nfcNormalize(item.Name) != nfcNormalize(slashpath.Base(relPath)) {
		return false, true
	}

	parentDir := slashpath.Dir(relPath)
	if parentDir == "." {
		parentDir = ""
	}
	if parentDir != "" && item.ParentID != "" {
		if parent, ok := e.baseline.GetByPath(parentDir); ok && parent.ItemID != "" {
			return item.ParentID == parent.ItemID, true
		}
	}
	if !item.ParentPathKnown {
		return false, false
	}

	return nfcNormalize(item.ParentPath) == nfcNormalize(parentDir), true
}

func remoteRowMatchesLivePreconditionForAction(action *Action, row *remoteStateRow, planned *remoteState) bool {
	if plannedPostRemoteMoveContentUpload(action) {
		return remoteRowMatchesPostRemoteMoveLivePrecondition(row, planned)
	}

	// A folder's eTag changes whenever anything beneath it changes, so a
	// folder eTag recorded at planning time (often before its own children
	// were uploaded) is not evidence that the folder itself changed. Folders
	// are checked on identity, type and path; comparing their eTag made every
	// folder move and delete supersede forever.
	return remoteRowMatchesLivePrecondition(row, planned, planned.ItemType != ItemTypeFolder)
}

func remoteRowMatchesPostRemoteMoveLivePrecondition(row *remoteStateRow, planned *remoteState) bool {
	return remoteRowMatchesLivePrecondition(row, planned, false)
}

func remoteRowMatchesLivePrecondition(row *remoteStateRow, planned *remoteState, compareETag bool) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteLiveIdentityMatches(row, planned, compareETag) {
		return false
	}
	if planned.ItemType == ItemTypeFolder {
		return true
	}

	return remoteLiveContentMatches(row, planned)
}

func remoteLiveIdentityMatches(row *remoteStateRow, planned *remoteState, compareETag bool) bool {
	if planned.ItemID != "" && row.ItemID != planned.ItemID {
		return false
	}
	if !planned.DriveID.IsZero() && !row.DriveID.IsZero() && !row.DriveID.Equal(planned.DriveID) {
		return false
	}
	if row.ItemType != planned.ItemType {
		return false
	}
	if compareETag && planned.ETag != "" && row.ETag != "" && row.ETag != planned.ETag {
		return false
	}

	return true
}

func remoteLiveContentMatches(row *remoteStateRow, planned *remoteState) bool {
	if row.Hash != "" && planned.Hash != "" {
		if row.Hash != planned.Hash {
			return false
		}
		if row.Size != 0 && planned.Size != 0 && row.Size != planned.Size {
			return false
		}
		return true
	}
	if row.Size != 0 && planned.Size != 0 && row.Size != planned.Size {
		return false
	}
	if row.Mtime != 0 && planned.Mtime != 0 && row.Mtime != planned.Mtime {
		return false
	}

	return true
}

func (e *executor) validateRemoteParentPrecondition(
	ctx context.Context,
	driveID driveid.ID,
	parentID string,
	action *Action,
	op string,
) error {
	if parentID == "" {
		return nil
	}
	if parentID == graphRootID {
		return nil
	}
	item, err := e.items.GetItem(ctx, driveID, parentID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return &remoteItemGoneError{op: op, itemID: parentID, role: "parent"}
		}
		return fmt.Errorf("checking remote %s parent precondition for %s: %w", op, action.Path, err)
	}
	if !item.IsFolder && !item.IsRoot {
		return stalePreconditionError("%s remote parent %s is not a folder", op, parentID)
	}

	return nil
}

func remoteStateRowFromGraphItem(item *graph.Item) *remoteStateRow {
	if item == nil {
		return nil
	}

	// NFC, like every path observation stores: OneDrive keeps a name in the
	// NFD spelling it was uploaded with and refuses to rename it to its NFC
	// spelling (L-04), so a raw comparison with the NFC action path read an
	// unchanged NFD item as "moved" and superseded the action forever (D-027).
	path := ""
	if item.ParentPathKnown {
		path = item.Name
		if item.ParentPath != "" {
			path = slashpath.Join(item.ParentPath, item.Name)
		}
		path = nfcNormalize(path)
	}

	resolvedType := ItemTypeFile
	if item.IsFolder || item.IsRoot {
		resolvedType = ItemTypeFolder
	}

	mtime := int64(0)
	if !item.ModifiedAt.IsZero() {
		mtime = item.ModifiedAt.UnixNano()
	}

	return &remoteStateRow{
		DriveID:   item.DriveID,
		ItemID:    item.ID,
		Path:      path,
		ItemType:  resolvedType,
		Hash:      driveops.SelectHash(item),
		Size:      item.Size,
		SizeKnown: true,
		Mtime:     mtime,
		ETag:      item.ETag,
	}
}

func plannedRemoteStateForPrecondition(action *Action) *remoteState {
	if action == nil || action.View == nil {
		return nil
	}
	if action.View.Remote != nil {
		return action.View.Remote
	}
	if action.View.Baseline == nil {
		return nil
	}

	baseline := action.View.Baseline
	return &remoteState{
		DriveID:  baseline.DriveID,
		ItemID:   baseline.ItemID,
		ItemType: baseline.ItemType,
		Hash:     baseline.RemoteHash,
		Size:     baseline.RemoteSize,
		Mtime:    baseline.RemoteMtime,
		ETag:     baseline.ETag,
	}
}

func (e *executor) expectedRemotePreconditionPath(action *Action) string {
	// Graph reports GetItem parentReference paths relative to the drive root.
	// Mount-root engines plan actions relative to the mounted item, so that path
	// is a different coordinate system unless observation rematerializes it.
	if e != nil && e.remoteRootItemID != "" {
		return ""
	}
	if action == nil {
		return ""
	}
	if action.Type == ActionRemoteMove && action.OldPath != "" {
		return action.OldPath
	}

	return actionViewPath(action)
}

func (e *executor) validateUploadSourcePrecondition(action *Action) error {
	if action == nil {
		return nil
	}

	info, err := e.uploadSourceInfo(action.Path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return stalePreconditionError("upload source %s is no longer a regular file", action.Path)
	}

	planned := (*localState)(nil)
	if action.View != nil {
		planned = action.View.Local
	}
	if planned == nil {
		return nil
	}
	if planned.ItemType != ItemTypeFile {
		return stalePreconditionError("upload source %s changed type", action.Path)
	}
	if planned.Size != 0 && info.Size() != planned.Size {
		return stalePreconditionError("upload source %s changed size", action.Path)
	}
	if planned.Mtime != 0 && info.ModTime().UnixNano() != planned.Mtime {
		return stalePreconditionError("upload source %s changed mtime", action.Path)
	}
	if planned.LocalHasIdentity {
		identity, identityOK := synctree.IdentityFromFileInfo(info)
		if identityOK && !synctree.SameIdentity(identity, synctree.FileIdentity{
			Device: planned.LocalDevice,
			Inode:  planned.LocalInode,
		}) {
			return stalePreconditionError("upload source %s changed identity", action.Path)
		}
	}

	return nil
}

// uploadSourceInfo returns the facts of the file an upload is about to read.
//
// Uploading reads through a followed alias (R-2.4.6): observation recorded the
// followed target's facts, and the transfer manager reads the followed path.
// So when the source itself or any of its ancestors is a symbolic link, the
// source is stated by following links, exactly as observation stated it.
//
// The rooted sync tree must not answer that question. It refuses every name
// that resolves outside the sync root, which is the correct answer for a write
// and the wrong one for this read: a file beneath a directory alias that
// points at another disk would otherwise never upload. Containment is not
// weakened by this. The path is still checked lexically first, the boundary
// walk itself is rooted and no-follow, and a path with no link on it is
// stated through the rooted capability as before. Writes beneath an alias
// stay refused by validateNoSymlinkBoundary and by synctree.
func (e *executor) uploadSourceInfo(path string) (os.FileInfo, error) {
	absPath, err := e.syncTree.Abs(path)
	if err != nil {
		return nil, normalizeSyncTreePathError(err)
	}

	_, readsThroughLink, err := e.symlinkBoundaryForPath(path)
	if err != nil {
		return nil, normalizeSyncTreePathError(err)
	}

	if !readsThroughLink {
		info, lstatErr := e.syncTree.Lstat(path)
		if lstatErr != nil {
			if errors.Is(lstatErr, os.ErrNotExist) {
				return nil, stalePreconditionError("upload source %s is missing", path)
			}
			return nil, normalizeSyncTreePathError(lstatErr)
		}

		return info, nil
	}

	info, err := localpath.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, stalePreconditionError("upload source %s target is missing", path)
		}
		return nil, fmt.Errorf("stating upload source target %s: %w", path, err)
	}

	return info, nil
}

func expectedUploadHash(action *Action) string {
	if action == nil || action.View == nil || action.View.Local == nil {
		return ""
	}

	return action.View.Local.Hash
}

func (e *executor) validateDownloadTargetPrecondition(action *Action) error {
	if action == nil {
		return nil
	}

	info, err := e.syncTree.Lstat(action.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A live -wal or -journal with no main file is a database the
			// application is mid-way through creating or replacing: writing
			// a downloaded main file beside it corrupts it just the same.
			if dbErr := e.refuseLiveDatabaseTarget(action.Path); dbErr != nil {
				return dbErr
			}
			if action.View == nil || action.View.Local == nil || actionClearsLocalBeforeDownload(action) {
				return nil
			}
			return stalePreconditionError("download target %s disappeared", action.Path)
		}
		return normalizeSyncTreePathError(err)
	}

	if actionClearsLocalBeforeDownload(action) {
		return stalePreconditionError("download target %s appeared", action.Path)
	}

	if err := e.refuseLiveDatabaseTarget(action.Path); err != nil {
		return err
	}

	planned := plannedLocalState(action)
	if planned == nil {
		return stalePreconditionError("download target %s appeared", action.Path)
	}

	return e.validateExistingDownloadTarget(action, info, planned)
}

func actionClearsLocalBeforeDownload(action *Action) bool {
	return action != nil && action.Type == ActionDownload && action.RequireMissingLocalTarget
}

func plannedLocalState(action *Action) *localState {
	if action == nil || action.View == nil {
		return nil
	}

	return action.View.Local
}

func (e *executor) validateExistingDownloadTarget(action *Action, info os.FileInfo, planned *localState) error {
	if info.IsDir() || planned.ItemType != ItemTypeFile {
		return stalePreconditionError("download target %s changed type", action.Path)
	}
	if planned.Size != 0 && info.Size() != planned.Size {
		return stalePreconditionError("download target %s changed size", action.Path)
	}
	if planned.Mtime != 0 && info.ModTime().UnixNano() != planned.Mtime {
		return stalePreconditionError("download target %s changed mtime", action.Path)
	}
	if planned.Hash != "" {
		absPath, absErr := e.syncTree.Abs(action.Path)
		if absErr != nil {
			return normalizeSyncTreePathError(absErr)
		}
		currentHash, hashErr := e.hashFunc(absPath)
		if hashErr != nil {
			return fmt.Errorf("hashing download target %s: %w", action.Path, hashErr)
		}
		if currentHash != planned.Hash {
			return stalePreconditionError("download target %s changed hash", action.Path)
		}
	}

	return nil
}

func (e *executor) validateLocalMovePrecondition(action *Action) error {
	if action == nil {
		return nil
	}
	source := localMoveSourcePath(action)
	info, err := e.syncTree.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stalePreconditionError("local move source %s is missing", source)
		}
		return normalizeSyncTreePathError(err)
	}
	if err := e.validateLocalMoveDestinationAbsent(action, source, info); err != nil {
		return err
	}

	if action.View == nil || action.View.Baseline == nil {
		return nil
	}

	return e.validateLocalMoveSourceAgainstBaseline(source, info, action.View.Baseline)
}

func (e *executor) validateLocalDeleteFolderPrecondition(action *Action, relPath string) error {
	if action == nil || action.View == nil || action.View.Baseline == nil {
		return nil
	}

	info, err := e.syncTree.Lstat(relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stalePreconditionError("local delete source %s is already absent", action.Path)
		}
		return normalizeSyncTreePathError(err)
	}

	return e.validateLocalSourceAgainstBaseline("local delete", relPath, info, action.View.Baseline)
}

func localMoveSourcePath(action *Action) string {
	if action.OldPath != "" {
		return action.OldPath
	}

	return action.Path
}

// validateLocalMoveDestinationAbsent refuses a move onto an existing entry,
// except the source itself: on a case-insensitive or normalization-insensitive
// volume (macOS by default) a remote case-only or Unicode-form-only rename
// finds its destination already "present" as the same inode, and refusing it
// left the rename stale on every pass forever (D-031).
func (e *executor) validateLocalMoveDestinationAbsent(action *Action, source string, sourceInfo os.FileInfo) error {
	destInfo, destErr := e.syncTree.Lstat(action.Path)
	if destErr == nil {
		if isLocalSpellingOnlyMove(source, action.Path) && os.SameFile(sourceInfo, destInfo) {
			return nil
		}

		return stalePreconditionError("local move destination %s already exists", action.Path)
	}
	if !errors.Is(destErr, os.ErrNotExist) {
		return normalizeSyncTreePathError(destErr)
	}

	return nil
}

func (e *executor) validateLocalMoveSourceAgainstBaseline(
	source string,
	info os.FileInfo,
	baseline *BaselineEntry,
) error {
	return e.validateLocalSourceAgainstBaseline("local move", source, info, baseline)
}

func (e *executor) validateLocalSourceAgainstBaseline(
	op string,
	source string,
	info os.FileInfo,
	baseline *BaselineEntry,
) error {
	if info.IsDir() {
		if baseline.ItemType != ItemTypeFolder {
			return stalePreconditionError("%s source %s changed type", op, source)
		}
	} else {
		if baseline.ItemType != ItemTypeFile {
			return stalePreconditionError("%s source %s changed type", op, source)
		}
		if baseline.LocalSizeKnown && info.Size() != baseline.LocalSize {
			return stalePreconditionError("%s source %s changed size", op, source)
		}
		if baseline.LocalMtime != 0 && info.ModTime().UnixNano() != baseline.LocalMtime {
			return stalePreconditionError("%s source %s changed mtime", op, source)
		}
		if baseline.LocalHash != "" {
			absPath, absErr := e.syncTree.Abs(source)
			if absErr != nil {
				return normalizeSyncTreePathError(absErr)
			}
			currentHash, hashErr := e.hashFunc(absPath)
			if hashErr != nil {
				return fmt.Errorf("hashing %s source %s: %w", op, source, hashErr)
			}
			if currentHash != baseline.LocalHash {
				return stalePreconditionError("%s source %s changed hash", op, source)
			}
		}
	}
	if baseline.LocalHasIdentity {
		identity, identityErr := e.syncTree.IdentityNoFollow(source)
		if identityErr == nil && !synctree.SameIdentity(identity, synctree.FileIdentity{
			Device: baseline.LocalDevice,
			Inode:  baseline.LocalInode,
		}) {
			return stalePreconditionError("%s source %s changed identity", op, source)
		}
	}

	return nil
}

// symlinkBoundaryForPath reports the first component of relPath, the final
// one included, that is a symbolic link, walking one component at a time so
// every Lstat resolves against a fully real prefix. A link as the final
// component is never followed by that Lstat, so the walk itself cannot leave
// the sync root.
//
// An absent component means nothing beneath it exists yet, so there is no
// boundary to cross and the walk stops cleanly.
func (e *executor) symlinkBoundaryForPath(relPath string) (string, bool, error) {
	clean := slashpath.Clean(filepath.ToSlash(relPath))
	if clean == "." || clean == "" {
		return "", false, nil
	}

	current := ""

	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." {
			continue
		}

		current = slashpath.Join(current, part)

		info, err := e.syncTree.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", false, nil
			}

			return "", false, fmt.Errorf("lstat %s: %w", current, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return current, true, nil
		}
	}

	return "", false, nil
}

// validateNoSymlinkBoundary refuses a local mutation whose target sits beneath
// a symlinked ancestor.
//
// R-2.4.6: when follow_symlinks is enabled the sync tree observes content
// through an alias, but the alias is the only thing it may mutate. Writing
// through it would modify — and, for rename, silently replace — content the
// sync tree never owned. synctree fails such calls closed on its own; this
// check exists so the refusal is a stated policy with an actionable message
// and a durable blocked-scope classification, rather than a rooted-descriptor
// error leaking through.
//
// Deletes do not route through here: ExecuteLocalDelete has to special-case
// the alias itself, which it removes without following.
func (e *executor) validateNoSymlinkBoundary(relPath string, verb string) error {
	boundary, ok, err := e.symlinkBoundaryForPath(relPath)
	if err != nil {
		return normalizeSyncTreePathError(err)
	}

	if !ok {
		return nil
	}

	return fmt.Errorf("won't %s %s: %s is a symbolic link, and sync never writes through one", verb, relPath, boundary)
}

// errDatabaseInUse means a download would replace a SQLite database that an
// application has open: its write-ahead log or rollback journal is non-empty.
// Renaming new bytes over the main file while the sidecar holds uncommitted
// pages corrupts the database the next time the application checkpoints
// (P-3, D-084). It is a per-file condition retried later, not a stale plan:
// superseding would replan the same download at once.
var errDatabaseInUse = errors.New("the local database is open in an application (it has a live -wal or -journal file)")

// sqliteLiveSidecarSuffixes are the sidecars whose non-empty presence means
// uncommitted database state. -shm is only an index and is not checked.
func sqliteLiveSidecarSuffixes() []string {
	return []string{"-wal", "-journal"}
}

// refuseLiveDatabaseTarget returns errDatabaseInUse when path has a
// non-empty SQLite sidecar beside it. It runs before the download and again
// just before the rename (ValidateTargetBeforeRename).
func (e *executor) refuseLiveDatabaseTarget(path string) error {
	for _, suffix := range sqliteLiveSidecarSuffixes() {
		info, err := e.syncTree.Lstat(path + suffix)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			return fmt.Errorf("%w: %s%s", errDatabaseInUse, path, suffix)
		}
	}

	return nil
}

// isLocalSpellingOnlyMove reports a move that changes only the spelling of a
// name under OneDrive's equivalence (case, Unicode form): the same OneDrive
// name, a different local one.
func isLocalSpellingOnlyMove(oldPath, newPath string) bool {
	return oldPath != newPath && namekey.EqualPath(oldPath, newPath)
}
