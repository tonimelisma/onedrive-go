package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// asideNamePrefix marks an existing destination set aside while --force
// replaces it. It starts with a dot so it sorts out of the way, and never
// with "~$", which work and school drives reject.
const asideNamePrefix = ".onedrive-go-replacing-"

// maxRemoteNameBytes keeps the aside name within the 255-byte name limit the
// local side shares with OneDrive's practical limit.
const maxRemoteNameBytes = 255

// replaceDestination runs perform, which writes the source to dest's name,
// with any existing destination file renamed aside first. On success the
// aside copy is deleted (to the recycle bin); on any failure, including a
// copy the monitor reports as failed, it is renamed back.
//
// --force used to delete the destination and then copy or move. A copy that
// failed afterwards (quota, a monitor "failed", a timeout, Ctrl-C) left the
// user with neither file outside the recycle bin (D-082). Graph has no
// atomic replace for copy or move, and the rename-aside keeps the original
// one undo away at every step.
func replaceDestination(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.MountSession,
	destPath string,
	dest destInfo,
	now time.Time,
	perform func() error,
) error {
	if dest.existingID == "" {
		return perform()
	}

	asideName := asideNameFor(dest.newName, now)
	if _, err := session.MoveItem(ctx, dest.existingID, "", asideName); err != nil {
		return newRemoteOpError("setting aside existing", destPath, err)
	}

	if err := perform(); err != nil {
		// Restore even when the caller's context was canceled: leaving the
		// original under its aside name is the failure this exists to avoid.
		// Bounded, so a dead network cannot hang the exit.
		timeout := restoreAsideTimeout
		if cc.seams.asideRestoreTimeout > 0 {
			timeout = cc.seams.asideRestoreTimeout
		}
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if _, restoreErr := session.MoveItem(restoreCtx, dest.existingID, "", dest.newName); restoreErr != nil {
			return errors.Join(err, fmt.Errorf(
				"the original %s is kept on OneDrive as %q; restoring its name failed: %w",
				destPath, asideName, restoreErr))
		}
		return err
	}

	// The replacement landed, so the command succeeded whatever happens to
	// the aside copy now. DeleteResolvedPath rides out Graph's brief
	// itemNotFound after a rename; an aside already gone is the goal.
	asidePath := joinDisplayPath("/"+parentRemotePath(destPath), asideName)
	if err := session.DeleteResolvedPath(ctx, asidePath, dest.existingID); err != nil && !errors.Is(err, graph.ErrNotFound) {
		cc.Logger.Warn("replaced "+destPath+", but the previous version is left on OneDrive as "+asidePath+
			"; delete it when you no longer need it",
			slog.String("error", err.Error()),
		)
	}

	return nil
}

// restoreAsideTimeout bounds putting the original back after a failed
// replacement.
const restoreAsideTimeout = 30 * time.Second

// parentRemotePath is the drive-relative parent folder of remotePath.
func parentRemotePath(remotePath string) string {
	parent, _ := driveops.SplitParentAndName(remotePath)
	return parent
}

// hintAboutLeftoverAside tells the user about a previous version an
// interrupted --force left behind (a crash or kill between the rename aside
// and the cleanup), so it can be recovered. Listing failures are not
// errors: the hint is a courtesy, and the command itself goes on.
func hintAboutLeftoverAside(ctx context.Context, cc *CLIContext, session *driveops.MountSession, destPath string, dest destInfo) {
	folder := parentRemotePath(destPath)
	if dest.destIsDir {
		folder = driveops.CleanRemotePath(destPath)
	}

	children, err := session.ListChildren(ctx, folder)
	if err != nil {
		cc.Logger.Debug("list destination folder for leftover --force copies", slog.String("error", err.Error()))
		return
	}

	for i := range children {
		if isAsideNameFor(children[i].Name, dest.newName) {
			cc.Statusf("Note: %s is a previous version of %s left by an interrupted --force; rename or delete it.\n",
				joinDisplayPath("/"+folder, children[i].Name), dest.newName)
		}
	}
}

// isAsideNameFor reports whether name is an aside copy made for target.
func isAsideNameFor(name, target string) bool {
	rest, ok := strings.CutPrefix(name, asideNamePrefix)
	if !ok {
		return false
	}
	stamp, original, ok := strings.Cut(rest, "-")
	if !ok || stamp == "" || strings.Trim(stamp, "0123456789") != "" {
		return false
	}

	// asideNameFor trims long names on a rune boundary to fit 255 bytes, so
	// a trimmed one ends within a rune's width of the limit.
	trimmed := len(name) > maxRemoteNameBytes-utf8.UTFMax && strings.HasPrefix(target, original)
	return original == target || trimmed
}

// asideNameFor builds a sibling name unique to this run that fits the name
// limit, trimming the original name on a rune boundary when needed.
func asideNameFor(name string, now time.Time) string {
	prefix := asideNamePrefix + strconv.FormatInt(now.UnixNano(), 10) + "-"
	budget := maxRemoteNameBytes - len(prefix)
	for len(name) > budget {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}

	return prefix + name
}
