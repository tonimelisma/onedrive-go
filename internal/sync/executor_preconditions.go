package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	slashpath "path"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func stalePreconditionError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrActionPreconditionChanged, fmt.Sprintf(format, args...))
}

func (e *Executor) validateRemoteSourcePrecondition(
	ctx context.Context,
	driveID driveid.ID,
	action *Action,
	op string,
) error {
	_, err := e.remoteSourcePreconditionETag(ctx, driveID, action, op)
	return err
}

func (e *Executor) remoteSourcePreconditionETag(
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
			return "", stalePreconditionError("%s remote item %s is missing", op, action.ItemID)
		}
		return "", fmt.Errorf("checking remote %s precondition for %s: %w", op, action.Path, err)
	}

	row := remoteStateRowFromGraphItem(item)
	planned := plannedRemoteStateForPrecondition(action)
	if planned != nil && !remoteRowMatchesLivePreconditionForAction(action, row, planned) {
		return "", stalePreconditionError("%s remote item %s changed since planning", op, action.ItemID)
	}
	if expectedPath := e.expectedRemotePreconditionPath(action); expectedPath != "" &&
		row.Path != "" && row.Path != expectedPath {
		return "", stalePreconditionError("%s remote item %s moved to %s", op, action.ItemID, row.Path)
	}

	return item.ETag, nil
}

func remoteRowMatchesLivePreconditionForAction(action *Action, row *RemoteStateRow, planned *RemoteState) bool {
	if plannedPostRemoteMoveContentUpload(action) {
		return remoteRowMatchesPostRemoteMoveLivePrecondition(row, planned)
	}

	return remoteRowMatchesLivePrecondition(row, planned, true)
}

func remoteRowMatchesPostRemoteMoveLivePrecondition(row *RemoteStateRow, planned *RemoteState) bool {
	return remoteRowMatchesLivePrecondition(row, planned, false)
}

func remoteRowMatchesLivePrecondition(row *RemoteStateRow, planned *RemoteState, compareETag bool) bool {
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

func remoteLiveIdentityMatches(row *RemoteStateRow, planned *RemoteState, compareETag bool) bool {
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

func remoteLiveContentMatches(row *RemoteStateRow, planned *RemoteState) bool {
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

func (e *Executor) validateRemoteParentPrecondition(
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
			return stalePreconditionError("%s remote parent %s is missing", op, parentID)
		}
		return fmt.Errorf("checking remote %s parent precondition for %s: %w", op, action.Path, err)
	}
	if !item.IsFolder && !item.IsRoot {
		return stalePreconditionError("%s remote parent %s is not a folder", op, parentID)
	}

	return nil
}

func remoteStateRowFromGraphItem(item *graph.Item) *RemoteStateRow {
	if item == nil {
		return nil
	}

	path := ""
	if item.ParentPathKnown {
		path = item.Name
		if item.ParentPath != "" {
			path = slashpath.Join(item.ParentPath, item.Name)
		}
	}

	itemType := ItemTypeFile
	if item.IsFolder || item.IsRoot {
		itemType = ItemTypeFolder
	}

	mtime := int64(0)
	if !item.ModifiedAt.IsZero() {
		mtime = item.ModifiedAt.UnixNano()
	}

	return &RemoteStateRow{
		DriveID:  item.DriveID,
		ItemID:   item.ID,
		Path:     path,
		ItemType: itemType,
		Hash:     driveops.SelectHash(item),
		Size:     item.Size,
		Mtime:    mtime,
		ETag:     item.ETag,
	}
}

func plannedRemoteStateForPrecondition(action *Action) *RemoteState {
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
	return &RemoteState{
		DriveID:  baseline.DriveID,
		ItemID:   baseline.ItemID,
		ItemType: baseline.ItemType,
		Hash:     baseline.RemoteHash,
		Size:     baseline.RemoteSize,
		Mtime:    baseline.RemoteMtime,
		ETag:     baseline.ETag,
	}
}

func (e *Executor) expectedRemotePreconditionPath(action *Action) string {
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

func (e *Executor) validateUploadSourcePrecondition(action *Action) error {
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

	planned := (*LocalState)(nil)
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

func (e *Executor) uploadSourceInfo(path string) (os.FileInfo, error) {
	info, err := e.syncTree.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, stalePreconditionError("upload source %s is missing", path)
		}
		return nil, normalizeSyncTreePathError(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return info, nil
	}

	absPath, err := e.syncTree.Abs(path)
	if err != nil {
		return nil, normalizeSyncTreePathError(err)
	}
	info, err = localpath.Stat(absPath)
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

func (e *Executor) validateDownloadTargetPrecondition(action *Action) error {
	if action == nil {
		return nil
	}

	info, err := e.syncTree.Lstat(action.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if action.View == nil || action.View.Local == nil {
				return nil
			}
			return stalePreconditionError("download target %s disappeared", action.Path)
		}
		return normalizeSyncTreePathError(err)
	}

	planned := plannedLocalState(action)
	if planned == nil {
		return stalePreconditionError("download target %s appeared", action.Path)
	}

	return e.validateExistingDownloadTarget(action, info, planned)
}

func plannedLocalState(action *Action) *LocalState {
	if action == nil || action.View == nil {
		return nil
	}

	return action.View.Local
}

func (e *Executor) validateExistingDownloadTarget(action *Action, info os.FileInfo, planned *LocalState) error {
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

func (e *Executor) validateLocalMovePrecondition(action *Action) error {
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
	if err := e.validateLocalMoveDestinationAbsent(action); err != nil {
		return err
	}

	if action.View == nil || action.View.Baseline == nil {
		return nil
	}

	return e.validateLocalMoveSourceAgainstBaseline(source, info, action.View.Baseline)
}

func (e *Executor) validateLocalDeleteFolderPrecondition(action *Action, relPath string) error {
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

func (e *Executor) validateLocalMoveDestinationAbsent(action *Action) error {
	_, destErr := e.syncTree.Lstat(action.Path)
	if destErr == nil {
		return stalePreconditionError("local move destination %s already exists", action.Path)
	}
	if !errors.Is(destErr, os.ErrNotExist) {
		return normalizeSyncTreePathError(destErr)
	}

	return nil
}

func (e *Executor) validateLocalMoveSourceAgainstBaseline(
	source string,
	info os.FileInfo,
	baseline *BaselineEntry,
) error {
	return e.validateLocalSourceAgainstBaseline("local move", source, info, baseline)
}

func (e *Executor) validateLocalSourceAgainstBaseline(
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
