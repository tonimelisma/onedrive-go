package syncexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	slashpath "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// errRemoteMoveAlreadyApplied reports that the item a remote move targets
// already sits at the planned destination: the work is done, not stale.
var errRemoteMoveAlreadyApplied = errors.New("remote move already applied")

// RemoteItemGoneError reports that a live read found a remote item missing:
// positive evidence it was deleted, even when delta never said so (an item
// moved into a folder that was then deleted is not reported again). It wraps
// ErrActionPreconditionChanged, so the action is superseded as before, and
// the engine also drops the item from remote_state so the next plan stops
// targeting it instead of superseding the same action on every pass.
type RemoteItemGoneError struct {
	Op     string
	ItemID string
	Role   string
}

func (e *RemoteItemGoneError) Error() string {
	return fmt.Sprintf("%s: %s remote %s %s is missing", syncdomain.ErrActionPreconditionChanged, e.Op, e.Role, e.ItemID)
}

func (e *RemoteItemGoneError) Unwrap() error { return syncdomain.ErrActionPreconditionChanged }

func stalePreconditionError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", syncdomain.ErrActionPreconditionChanged, fmt.Sprintf(format, args...))
}

func (e *Executor) validateRemoteSourcePrecondition(
	ctx context.Context,
	driveID driveid.ID,
	action *syncdomain.Action,
	op string,
) error {
	_, err := e.remoteSourcePreconditionETag(ctx, driveID, action, op)
	return err
}

func (e *Executor) remoteSourcePreconditionETag(
	ctx context.Context,
	driveID driveid.ID,
	action *syncdomain.Action,
	op string,
) (string, error) {
	live, err := e.remoteSourcePrecondition(ctx, driveID, action, op)
	return live.eTag, err
}

// liveRemoteSource is what a passed remote source preflight learned about the
// item. Both fields are empty when the action names no item.
type liveRemoteSource struct {
	eTag     string
	modified time.Time
}

// remoteSourcePrecondition checks the planned remote source against the live
// item. errRemoteMoveAlreadyApplied still carries the live facts.
func (e *Executor) remoteSourcePrecondition(
	ctx context.Context,
	driveID driveid.ID,
	action *syncdomain.Action,
	op string,
) (liveRemoteSource, error) {
	if action == nil || action.ItemID == "" {
		return liveRemoteSource{}, nil
	}

	item, err := e.items.GetItem(ctx, driveID, action.ItemID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return liveRemoteSource{}, &RemoteItemGoneError{Op: op, ItemID: action.ItemID, Role: "item"}
		}
		return liveRemoteSource{}, fmt.Errorf("checking remote %s precondition for %s: %w", op, action.Path, err)
	}

	live := liveRemoteSource{eTag: item.ETag, modified: item.ModifiedAt}
	row := remoteStateRowFromGraphItem(item)
	planned := plannedRemoteStateForPrecondition(action)
	// Already at the destination with the same identity and content: moving
	// it (or the parent that carried it) churned its eTag, so eTag is not
	// compared here.
	if e.remoteMoveAlreadyApplied(action, item, row, planned) {
		return live, errRemoteMoveAlreadyApplied
	}
	if planned != nil && !remoteRowMatchesLivePreconditionForAction(action, row, planned) {
		return liveRemoteSource{}, stalePreconditionError("%s remote item %s changed since planning", op, action.ItemID)
	}
	if expectedPath := e.expectedRemotePreconditionPath(action); expectedPath != "" {
		if at, known := e.remoteItemIsAt(item, expectedPath); known && !at {
			return liveRemoteSource{}, stalePreconditionError("%s remote item %s moved from %s", op, action.ItemID, expectedPath)
		}
	}

	return live, nil
}

func (e *Executor) remoteMoveAlreadyApplied(
	action *syncdomain.Action, item *graph.Item, row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState,
) bool {
	if action.Type != syncdomain.ActionRemoteMove {
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
func (e *Executor) remoteItemIsAt(item *graph.Item, relPath string) (at, known bool) {
	if item == nil || item.Name == "" {
		return false, false
	}
	if namekey.Stored(item.Name) != namekey.Stored(slashpath.Base(relPath)) {
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

	return namekey.StoredPath(item.ParentPath) == namekey.StoredPath(parentDir), true
}

func remoteRowMatchesLivePreconditionForAction(
	action *syncdomain.Action, row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState,
) bool {
	if plannedPostRemoteMoveContentUpload(action) {
		return remoteRowMatchesPostRemoteMoveLivePrecondition(row, planned)
	}

	// A folder's eTag changes whenever anything beneath it changes, so a
	// folder eTag recorded at planning time (often before its own children
	// were uploaded) is not evidence that the folder itself changed. Folders
	// are checked on identity, type and path; comparing their eTag made every
	// folder move and delete supersede forever.
	return remoteRowMatchesLivePrecondition(row, planned, planned.ItemType != syncdomain.ItemTypeFolder)
}

func remoteRowMatchesPostRemoteMoveLivePrecondition(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
	return remoteRowMatchesLivePrecondition(row, planned, false)
}

func remoteRowMatchesLivePrecondition(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState, compareETag bool) bool {
	if row == nil || planned == nil {
		return row == nil && planned == nil
	}
	if !remoteLiveIdentityMatches(row, planned, compareETag) {
		return false
	}
	if planned.ItemType == syncdomain.ItemTypeFolder {
		return true
	}

	return remoteLiveContentMatches(row, planned)
}

func remoteLiveIdentityMatches(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState, compareETag bool) bool {
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

func remoteLiveContentMatches(row *syncdomain.RemoteStateRow, planned *syncdomain.RemoteState) bool {
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
	if syncdomain.StoredTimeIsSet(row.Mtime) && syncdomain.StoredTimeIsSet(planned.Mtime) && row.Mtime != planned.Mtime {
		return false
	}

	return true
}

func (e *Executor) validateRemoteParentPrecondition(
	ctx context.Context,
	driveID driveid.ID,
	parentID string,
	action *syncdomain.Action,
	op string,
) error {
	if parentID == "" {
		return nil
	}
	if parentID == GraphRootID {
		return nil
	}
	if e.parentChecks.folderRead(driveID, parentID) {
		// Read live as a folder earlier in this plan; a create into it
		// that answers 404 is reported as the same parent-gone fact.
		return nil
	}
	item, err := e.items.GetItem(ctx, driveID, parentID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return &RemoteItemGoneError{Op: op, ItemID: parentID, Role: "parent"}
		}
		return fmt.Errorf("checking remote %s parent precondition for %s: %w", op, action.Path, err)
	}
	if !item.IsFolder && !item.IsRoot {
		return stalePreconditionError("%s remote parent %s is not a folder", op, parentID)
	}
	e.parentChecks.markFolderRead(driveID, parentID)

	return nil
}

// remoteCreateParentGone turns a create's own not-found answer into the
// parent-gone fact the live parent read would have returned: the create
// addresses the parent by ID, so a 404 that is not about an item the create
// already made names that parent. Nil for every other error.
func remoteCreateParentGone(err error, op, parentID string) error {
	if err == nil || parentID == "" || !errors.Is(err, graph.ErrNotFound) {
		return nil
	}
	var created *graph.CreatedItemError
	if errors.As(err, &created) {
		return nil
	}

	return &RemoteItemGoneError{Op: op, ItemID: parentID, Role: "parent"}
}

func remoteStateRowFromGraphItem(item *graph.Item) *syncdomain.RemoteStateRow {
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
		path = namekey.StoredPath(path)
	}

	resolvedType := syncdomain.ItemTypeFile
	if item.IsFolder || item.IsRoot {
		resolvedType = syncdomain.ItemTypeFolder
	}

	// A negative size is not a size (R-2.4.16).
	size, sizeKnown := item.Size, item.Size >= 0
	if !sizeKnown {
		size = 0
	}

	return &syncdomain.RemoteStateRow{
		DriveID:   item.DriveID,
		ItemID:    item.ID,
		Path:      path,
		ItemType:  resolvedType,
		Hash:      driveops.SelectHash(item),
		Size:      size,
		SizeKnown: sizeKnown,
		// Compared with the planned remote time, which the item converter
		// stored: the same encoding.
		Mtime: syncdomain.StoredTimeNano(item.ModifiedAt),
		ETag:  item.ETag,
	}
}

func plannedRemoteStateForPrecondition(action *syncdomain.Action) *syncdomain.RemoteState {
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
	return &syncdomain.RemoteState{
		DriveID:  baseline.DriveID,
		ItemID:   baseline.ItemID,
		ItemType: baseline.ItemType,
		Hash:     baseline.RemoteHash,
		Size:     baseline.RemoteSize,
		Mtime:    baseline.RemoteMtime,
		ETag:     baseline.ETag,
	}
}

func (e *Executor) expectedRemotePreconditionPath(action *syncdomain.Action) string {
	// Graph reports GetItem parentReference paths relative to the drive root.
	// Mount-root engines plan actions relative to the mounted item, so that path
	// is a different coordinate system unless observation rematerializes it.
	if e != nil && e.remoteRootItemID != "" {
		return ""
	}
	if action == nil {
		return ""
	}
	if action.Type == syncdomain.ActionRemoteMove && action.OldPath != "" {
		return action.OldPath
	}

	return syncdomain.ActionViewPath(action)
}

// validateUploadSourcePrecondition requires the file at source, the rooted
// spelling on disk of action.Path, to still be the file the plan observed.
func (e *Executor) validateUploadSourcePrecondition(action *syncdomain.Action, source string) error {
	if action == nil {
		return nil
	}

	info, err := e.uploadSourceInfo(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return stalePreconditionError("upload source %s is no longer a regular file", action.Path)
	}

	planned := (*syncdomain.LocalState)(nil)
	if action.View != nil {
		planned = action.View.Local
	}
	if planned == nil {
		return nil
	}
	if planned.ItemType != syncdomain.ItemTypeFile {
		return stalePreconditionError("upload source %s changed type", action.Path)
	}
	if planned.Size != 0 && info.Size() != planned.Size {
		return stalePreconditionError("upload source %s changed size", action.Path)
	}
	if planned.Mtime != 0 && info.ModTime().UnixNano() != planned.Mtime {
		return stalePreconditionError("upload source %s changed mtime", action.Path)
	}
	if plannedCtimeChanged(planned, info) {
		return stalePreconditionError("upload source %s changed since it was observed", action.Path)
	}
	if planned.LocalHasIdentity {
		identity, identityOK := synctree.IdentityFromFileInfo(info)
		if identityOK && !synctree.SameIdentity(identity, synctree.FileIdentity{
			Device:    planned.LocalDevice,
			Inode:     planned.LocalInode,
			BirthTime: planned.LocalBirthTime,
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
func (e *Executor) uploadSourceInfo(path string) (os.FileInfo, error) {
	absPath, err := e.syncTree.Abs(path)
	if err != nil {
		return nil, normalizeSyncTreePathError(err)
	}

	_, readsThroughLink, err := e.SymlinkBoundaryForPath(path)
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

func expectedUploadHash(action *syncdomain.Action) string {
	if action == nil || action.View == nil || action.View.Local == nil {
		return ""
	}

	return action.View.Local.Hash
}

// validateDownloadTargetPrecondition requires target, the rooted path the
// download writes (downloadTargetSpelling), to still hold what the plan saw.
func (e *Executor) validateDownloadTargetPrecondition(ctx context.Context, action *syncdomain.Action, target string) error {
	if action == nil {
		return nil
	}

	info, err := e.syncTree.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// A live -wal or -journal with no main file is a database the
			// application is mid-way through creating or replacing: writing
			// a downloaded main file beside it corrupts it just the same.
			if dbErr := e.refuseLiveDatabaseTarget(target); dbErr != nil {
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

	if err := e.refuseLiveDatabaseTarget(target); err != nil {
		return err
	}

	planned := plannedLocalState(action)
	if planned == nil {
		return stalePreconditionError("download target %s appeared", action.Path)
	}

	return e.validateExistingDownloadTarget(ctx, action, target, info, planned)
}

func actionClearsLocalBeforeDownload(action *syncdomain.Action) bool {
	return action != nil && action.Type == syncdomain.ActionDownload && action.RequireMissingLocalTarget
}

func plannedLocalState(action *syncdomain.Action) *syncdomain.LocalState {
	if action == nil || action.View == nil {
		return nil
	}

	return action.View.Local
}

// Names of the existing file an action is about to replace, for the
// stale-precondition messages of the check that it is still the version the
// plan saw.
const (
	downloadTargetNoun       = "download target"
	localMoveDestinationNoun = "local move destination"
)

func (e *Executor) validateExistingDownloadTarget(
	ctx context.Context, action *syncdomain.Action, target string, info os.FileInfo, planned *syncdomain.LocalState,
) error {
	if err := validateExistingTargetMetadata(action, info, planned, downloadTargetNoun); err != nil {
		return err
	}
	if expected := expectedDownloadTargetHash(action, planned); expected != "" {
		absPath, absErr := e.syncTree.Abs(target)
		if absErr != nil {
			return normalizeSyncTreePathError(absErr)
		}
		currentHash, hashErr := e.hashFile(ctx, absPath)
		if hashErr != nil {
			return fmt.Errorf("hashing download target %s: %w", action.Path, hashErr)
		}
		if currentHash != expected {
			return stalePreconditionError("download target %s changed hash", action.Path)
		}
	}

	return nil
}

// expectedDownloadTargetHash is the content a download may replace: the
// version the plan observed, or, when the plan carries no hash for it, the
// synced version in the baseline (R-2.2.5). Metadata alone never proves a
// file is the synced version: an in-place edit can keep size and mtime.
func expectedDownloadTargetHash(action *syncdomain.Action, planned *syncdomain.LocalState) string {
	if planned != nil && planned.Hash != "" {
		return planned.Hash
	}
	if action == nil || action.View == nil || action.View.Baseline == nil {
		return ""
	}
	if action.View.Baseline.ItemType != syncdomain.ItemTypeFile {
		return ""
	}

	return action.View.Baseline.LocalHash
}

// validateExistingTargetMetadata is the cheap half of the overwrite check:
// the file the planner saw must still be a file of the same size and mtime.
// noun names the file in the refusal (downloadTargetNoun,
// localMoveDestinationNoun).
func validateExistingTargetMetadata(action *syncdomain.Action, info os.FileInfo, planned *syncdomain.LocalState, noun string) error {
	if info.IsDir() || planned.ItemType != syncdomain.ItemTypeFile {
		return stalePreconditionError("%s %s changed type", noun, action.Path)
	}
	if planned.Size != 0 && info.Size() != planned.Size {
		return stalePreconditionError("%s %s changed size", noun, action.Path)
	}
	if planned.Mtime != 0 && info.ModTime().UnixNano() != planned.Mtime {
		return stalePreconditionError("%s %s changed mtime", noun, action.Path)
	}
	if plannedCtimeChanged(planned, info) {
		return stalePreconditionError("%s %s changed since it was observed", noun, action.Path)
	}

	return nil
}

// plannedCtimeChanged reports whether the file's inode change time moved since
// the observation the plan used. An in-place edit that restored size and mtime
// still moves ctime (R-2.2.4). An unknown planned ctime proves nothing.
func plannedCtimeChanged(planned *syncdomain.LocalState, info os.FileInfo) bool {
	if planned == nil || planned.LocalCtime == 0 {
		return false
	}
	current := syncdomain.FileChangeTime(info)

	return current != 0 && current != planned.LocalCtime
}

// validateDownloadTargetBeforeReplace runs just before a finished download is
// renamed over its target. With a trash, an existing target is verified
// through the descriptor of the inode that is then preserved in the trash, so
// the version kept is exactly the version checked (D-010, P-4); the rename
// that follows can no longer lose the content it replaces.
func (e *Executor) validateDownloadTargetBeforeReplace(ctx context.Context, action *syncdomain.Action, target string) error {
	if e.trash == nil {
		return e.validateDownloadTargetPrecondition(ctx, action, target)
	}
	info, err := e.syncTree.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || actionClearsLocalBeforeDownload(action) {
		return e.validateDownloadTargetPrecondition(ctx, action, target)
	}
	if dbErr := e.refuseLiveDatabaseTarget(target); dbErr != nil {
		return dbErr
	}
	planned := plannedLocalState(action)
	if planned == nil {
		return stalePreconditionError("download target %s appeared", action.Path)
	}

	return e.preserveReplacedDownloadTarget(action, target, e.existingTargetVerifier(ctx, action, planned, downloadTargetNoun))
}

// existingTargetVerifier checks, through the open descriptor, that the file a
// download or a replacing local move is about to replace is still the
// version the plan saw; its hash stops when ctx ends. noun names the file in
// the refusal.
func (e *Executor) existingTargetVerifier(
	ctx context.Context, action *syncdomain.Action, planned *syncdomain.LocalState, noun string,
) synctree.VerifyFunc {
	return func(f *os.File, current os.FileInfo) error {
		if metaErr := validateExistingTargetMetadata(action, current, planned, noun); metaErr != nil {
			return metaErr
		}
		expected := expectedDownloadTargetHash(action, planned)
		if expected == "" {
			return nil
		}
		currentHash, hashErr := e.hashOpenFile(ctx, f)
		if hashErr != nil {
			return fmt.Errorf("hashing %s %s: %w", noun, action.Path, hashErr)
		}
		if currentHash != expected {
			return stalePreconditionError("%s %s changed hash", noun, action.Path)
		}
		return nil
	}
}

// preserveReplacedDownloadTarget keeps the verified version of the file at
// target (rooted, spelled as on disk) in the trash before a download renames
// over it; the file stays at target until then.
func (e *Executor) preserveReplacedDownloadTarget(action *syncdomain.Action, target string, verify synctree.VerifyFunc) error {
	placement, err := e.trash.Reserve(e.syncTree.Path(), target, localtrash.ReasonReplaced)
	if err != nil {
		// Named as text, as a local delete names it: a reservation whose
		// entry folder went missing is the trash failing, never the target
		// already gone.
		return fmt.Errorf("keeping the replaced version of %s: reserving a trash entry: %s", action.Path, err.Error())
	}
	if err := e.syncTree.PreserveRegularFile(target, verify, placement.ItemPath); err != nil {
		if abandonErr := placement.Abandon(); abandonErr != nil {
			e.logger.Debug("could not remove unused trash entry",
				slog.String("path", action.Path), slog.Any("error", abandonErr))
		}
		if errors.Is(err, synctree.ErrDetachRaced) {
			return stalePreconditionError("download target %s changed while it was checked", action.Path)
		}
		return fmt.Errorf("keeping the replaced version of %s: %w", action.Path, err)
	}
	e.commitTrashPlacement(target, placement)

	return nil
}

// validateLocalMovePrecondition checks a local move between source and dest,
// the rooted spellings on disk of action.OldPath and action.Path.
func (e *Executor) validateLocalMovePrecondition(ctx context.Context, action *syncdomain.Action, source, dest string) error {
	if action == nil {
		return nil
	}
	info, err := e.syncTree.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stalePreconditionError("local move source %s is missing", localMoveSourcePath(action))
		}
		return normalizeSyncTreePathError(err)
	}
	if err := e.validateLocalMoveDestinationAbsent(action, dest, info); err != nil {
		return err
	}

	if action.View == nil || action.View.Baseline == nil {
		return nil
	}

	return e.validateLocalMoveSourceAgainstBaseline(ctx, source, info, action.View.Baseline)
}

// validateLocalMoveParentExists requires the parent directory of dest (rooted,
// spelled as on disk) to exist already.
func (e *Executor) validateLocalMoveParentExists(dest string) error {
	parent := filepath.Dir(dest)
	if parent == "." || parent == "" {
		return nil
	}
	info, err := e.syncTree.Lstat(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stalePreconditionError("local move destination parent %s is missing", parent)
		}
		return normalizeSyncTreePathError(err)
	}
	if !info.IsDir() {
		return stalePreconditionError("local move destination parent %s is not a directory", parent)
	}

	return nil
}

func (e *Executor) validateLocalDeleteFolderPrecondition(ctx context.Context, action *syncdomain.Action, relPath string) error {
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

	if !info.IsDir() || action.View.Baseline.ItemType != syncdomain.ItemTypeFolder || action.View.Local == nil {
		return e.validateLocalSourceAgainstBaseline(ctx, "local delete", relPath, info, action.View.Baseline)
	}

	// A folder is checked against the folder the plan saw, not the
	// baseline's identity: the planner already decided, by content, that
	// a folder with a new inode (rebuilt, restored, its disk renumbered)
	// is the synced one with nothing changed below it (D-057). Checking
	// the baseline identity refused that decision on every pass. What
	// R-2.8.10 guards against, a different folder swapped in after
	// planning, still fails here.
	return e.validateLocalFolderIdentityAgainstPlan("local delete", relPath, action.View.Local)
}

func (e *Executor) validateLocalFolderIdentityAgainstPlan(op, source string, planned *syncdomain.LocalState) error {
	if !planned.LocalHasIdentity {
		return nil
	}
	identity, err := e.syncTree.IdentityNoFollow(source)
	if err == nil && !synctree.SameIdentity(identity, synctree.FileIdentity{
		Device:    planned.LocalDevice,
		Inode:     planned.LocalInode,
		BirthTime: planned.LocalBirthTime,
	}) {
		return stalePreconditionError("%s source %s changed identity since planning", op, source)
	}

	return nil
}

func localMoveSourcePath(action *syncdomain.Action) string {
	if action.OldPath != "" {
		return action.OldPath
	}

	return action.Path
}

// validateLocalMoveDestinationAbsent refuses a move onto an existing entry,
// except the source itself: on a case-insensitive or normalization-insensitive
// volume (macOS by default) a remote case-only or Unicode-form-only rename
// finds its destination already "present" as the same inode, and refusing it
// left the rename stale on every pass forever (D-031). dest is the rooted
// spelling on disk of action.Path.
func (e *Executor) validateLocalMoveDestinationAbsent(action *syncdomain.Action, dest string, sourceInfo os.FileInfo) error {
	destInfo, destErr := e.syncTree.Lstat(dest)
	if destErr == nil {
		if isLocalSpellingOnlyMove(localMoveSourcePath(action), action.Path) && synctree.SameFile(sourceInfo, destInfo) {
			return nil
		}
		if action.ReplaceLocalTarget {
			// Cleared last, by clearLocalMoveDestination, once every other
			// precondition held: a move superseded after its destination left
			// would have displaced the file for nothing.
			return nil
		}

		return stalePreconditionError("local move destination %s already exists", action.Path)
	}
	if !errors.Is(destErr, os.ErrNotExist) {
		return normalizeSyncTreePathError(destErr)
	}

	return nil
}

// clearLocalMoveDestination makes way for a replacing local move immediately
// before its rename, after the source and the parent were validated.
func (e *Executor) clearLocalMoveDestination(ctx context.Context, action *syncdomain.Action, dest string) error {
	if !action.ReplaceLocalTarget {
		return nil
	}
	destInfo, err := e.syncTree.Lstat(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return normalizeSyncTreePathError(err)
	}

	return e.clearReplacedLocalMoveTarget(ctx, action, dest, destInfo)
}

// clearReplacedLocalMoveTarget makes way for a move that replaces the file at
// its destination (D-058) by taking the file out of the sync folder the way a
// local delete does: only while it is still exactly the version the plan saw
// (a regular file with its observed hash, metadata and ctime), verified
// through the descriptor of the inode that leaves, into the trash when the
// drive has one and removed otherwise. The hash stops when ctx ends. The file
// is moved once, or copied through its verified descriptor when the trash is
// on another filesystem; the move's rename then finds the name free and
// replaces nothing. A file that takes the name between the two is kept, the
// rename refuses, and the move is superseded with the displaced file in the
// trash. A file that left the name on its own leaves nothing to make way for.
func (e *Executor) clearReplacedLocalMoveTarget(ctx context.Context, action *syncdomain.Action, dest string, destInfo os.FileInfo) error {
	planned := plannedLocalState(action)
	if !destInfo.Mode().IsRegular() || planned == nil || planned.Hash == "" {
		return stalePreconditionError("local move destination %s already exists", action.Path)
	}
	if err := e.refuseLiveDatabaseTarget(dest); err != nil {
		return err
	}

	err := e.takeOutVerifiedLocalFile(dest, e.existingTargetVerifier(ctx, action, planned, localMoveDestinationNoun),
		localtrash.ReasonReplaced, syncdomain.CrashPointLocalMoveDestinationTrashed)
	switch {
	case err == nil, errors.Is(err, os.ErrNotExist):
		return nil
	case errors.Is(err, syncdomain.ErrActionPreconditionChanged):
		return err
	case errors.Is(err, synctree.ErrDetachRaced):
		return stalePreconditionError("local move destination %s changed while it was checked", action.Path)
	default:
		return fmt.Errorf("making way for the move to %s: %w", action.Path, normalizeSyncTreePathError(err))
	}
}

// validateLocalMoveSourceAgainstBaseline requires the source to still be the
// synced item: the same type and, when known, the same filesystem identity.
// Its content may have changed (D-053): a rename carries the bytes unchanged,
// the move outcome keeps the baseline's content facts, and the next pass
// uploads the local edit at the destination. Requiring unchanged content
// would supersede the remote move on every pass and strand the edit.
func (e *Executor) validateLocalMoveSourceAgainstBaseline(
	ctx context.Context,
	source string,
	info os.FileInfo,
	baseline *syncdomain.BaselineEntry,
) error {
	if info.IsDir() != (baseline.ItemType == syncdomain.ItemTypeFolder) {
		return stalePreconditionError("local move source %s changed type", source)
	}
	identityErr := e.validateLocalSourceIdentity("local move", source, baseline)
	if identityErr == nil || info.IsDir() || !e.localFileHoldsBaselineContent(ctx, source, info, baseline) {
		return identityErr
	}

	// A file with a new inode that still holds the synced content is the
	// synced item (D-057): a folder rebuilt with the same files gives each
	// one a new inode, and refusing them wedged every move of them.
	return nil
}

// localFileHoldsBaselineContent reports whether the file at source provably
// holds the synced content: the baseline's size and local hash.
func (e *Executor) localFileHoldsBaselineContent(
	ctx context.Context, source string, info os.FileInfo, baseline *syncdomain.BaselineEntry,
) bool {
	if baseline.LocalHash == "" || (baseline.LocalSizeKnown && info.Size() != baseline.LocalSize) {
		return false
	}
	absPath, err := e.syncTree.Abs(source)
	if err != nil {
		return false
	}
	currentHash, err := e.hashFile(ctx, absPath)

	return err == nil && currentHash == baseline.LocalHash
}

func (e *Executor) validateLocalSourceIdentity(op string, source string, baseline *syncdomain.BaselineEntry) error {
	if !baseline.LocalHasIdentity {
		return nil
	}
	identity, err := e.syncTree.IdentityNoFollow(source)
	if err == nil && !synctree.SameIdentity(identity, synctree.FileIdentity{
		Device:    baseline.LocalDevice,
		Inode:     baseline.LocalInode,
		BirthTime: baseline.LocalBirthTime,
	}) {
		return stalePreconditionError("%s source %s changed identity", op, source)
	}

	return nil
}

func (e *Executor) validateLocalSourceAgainstBaseline(
	ctx context.Context,
	op string,
	source string,
	info os.FileInfo,
	baseline *syncdomain.BaselineEntry,
) error {
	if info.IsDir() {
		if baseline.ItemType != syncdomain.ItemTypeFolder {
			return stalePreconditionError("%s source %s changed type", op, source)
		}
	} else {
		if baseline.ItemType != syncdomain.ItemTypeFile {
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
			currentHash, hashErr := e.hashFile(ctx, absPath)
			if hashErr != nil {
				return fmt.Errorf("hashing %s source %s: %w", op, source, hashErr)
			}
			if currentHash != baseline.LocalHash {
				return stalePreconditionError("%s source %s changed hash", op, source)
			}
		}
	}
	return e.validateLocalSourceIdentity(op, source, baseline)
}

// SymlinkBoundaryForPath reports the first component of relPath, the final
// one included, that is a symbolic link, walking one component at a time so
// every Lstat resolves against a fully real prefix. A link as the final
// component is never followed by that Lstat, so the walk itself cannot leave
// the sync root.
//
// An absent component means nothing beneath it exists yet, so there is no
// boundary to cross and the walk stops cleanly.
func (e *Executor) SymlinkBoundaryForPath(relPath string) (string, bool, error) {
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
func (e *Executor) validateNoSymlinkBoundary(relPath string, verb string) error {
	boundary, ok, err := e.SymlinkBoundaryForPath(relPath)
	if err != nil {
		return normalizeSyncTreePathError(err)
	}

	if !ok {
		return nil
	}

	return fmt.Errorf("won't %s %s: %s is a symbolic link, and sync never writes through one", verb, relPath, boundary)
}

// ErrDatabaseInUse means a download would replace a SQLite database that an
// application has open: its write-ahead log or rollback journal is non-empty.
// Renaming new bytes over the main file while the sidecar holds uncommitted
// pages corrupts the database the next time the application checkpoints
// (P-3, D-084). It is a per-file condition retried later, not a stale plan:
// superseding would replan the same download at once.
var ErrDatabaseInUse = errors.New("the local database is open in an application (it has a live -wal or -journal file)")

// sqliteLiveSidecarSuffixes are the sidecars whose non-empty presence means
// uncommitted database state. -shm is only an index and is not checked.
func sqliteLiveSidecarSuffixes() []string {
	return []string{"-wal", "-journal"}
}

// refuseLiveDatabaseTarget returns ErrDatabaseInUse when path has a
// non-empty SQLite sidecar beside it. It runs before the download and again
// just before the rename (ValidateTargetBeforeRename). Only a sidecar that
// does not exist is "no sidecar": a sidecar that cannot be checked (EACCES,
// EIO, a name too long) refuses the replacement too, because the download
// cannot prove the database is closed.
func (e *Executor) refuseLiveDatabaseTarget(path string) error {
	for _, suffix := range sqliteLiveSidecarSuffixes() {
		info, err := e.syncTree.Lstat(path + suffix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return fmt.Errorf("won't replace %s: cannot check %s%s for an open database: %w",
				path, path, suffix, normalizeSyncTreePathError(err))
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			return fmt.Errorf("%w: %s%s", ErrDatabaseInUse, path, suffix)
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
