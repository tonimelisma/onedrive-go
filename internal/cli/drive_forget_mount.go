package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func newDriveForgetMountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forget-mount <path>",
		Short: "Forget that a folder of the sync folder was another disk's mount point",
		Long: `Forget that a folder of the selected drive's sync folder was the mount point of
another disk, so the next sync reads it as an ordinary folder.

Sync remembers a folder it crossed into another disk at, so that when the disk
is not mounted its empty folder is "Disk not mounted" rather than every file on
the disk deleted. When the disk's content is now in that folder as a plain
folder of the sync folder's own disk (a whole-tree restore, or the disk copied
in), the folder stays "Disk not mounted" while the record stands. Forgetting
it lets the next sync compare that content with OneDrive like any other: what
is the same stays, nothing is deleted and uploaded again.

<path> is the folder as 'onedrive-go status' names it, relative to the sync
folder, or its absolute path. The command refuses a folder that is not a
recorded mount point, one that is missing or empty (that is a disk that is not
mounted: mount it again instead), and one that is still the mount point of
another disk. It shows what the folder holds; type FORGET to confirm, or use
--yes for non-interactive use. Nothing is deleted, here or on OneDrive.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, err := cmd.Flags().GetBool("yes")
			if err != nil {
				return fmt.Errorf("read --yes flag: %w", err)
			}
			return runDriveForgetMountWithInput(cmd.Context(), mustCLIContext(cmd.Context()), cmd.InOrStdin(), args[0], yes)
		},
	}
	cmd.Flags().Bool("yes", false, "skip the interactive FORGET confirmation prompt")

	return cmd
}

// forgetMountAction completes "cannot ..." in the forget's refusals.
const forgetMountAction = "forget the recorded mount point"

// driveForgetMountJSONOutput is `drive forget-mount --json`.
type driveForgetMountJSONOutput struct {
	Drive         string `json:"drive"`
	Path          string `json:"path"`
	Forgotten     bool   `json:"forgotten"`
	FilesInFolder int    `json:"files_in_folder"`
}

func runDriveForgetMountWithInput(ctx context.Context, cc *CLIContext, stdin io.Reader, folderArg string, yes bool) error {
	cfg, cid, err := resolveDriveForStateChange(ctx, cc, "forget a mount point of", forgetMountAction)
	if err != nil {
		return err
	}

	dbPath := config.DriveStatePath(cid)
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cid)
	}
	syncDir := driveSyncDir(cfg, cid, cc.Logger)
	tree, err := synctree.Open(syncDir)
	if err != nil {
		return fmt.Errorf("open sync folder %s: %w", syncDir, err)
	}
	rel, err := mountPointRelPath(syncDir, folderArg)
	if err != nil {
		return err
	}

	recorded, err := syncstore.ReadMountBoundaries(ctx, dbPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("read the recorded mount points of %s: %w", cid, err)
	}
	if !slices.Contains(recorded, rel) {
		return notRecordedMountPointError(cid.String(), rel, recorded)
	}

	present, err := plainFolderFileCount(tree, syncDir, rel)
	if err != nil {
		return err
	}
	if err := confirmForgetMountIntent(stdin, cc.Status(), cid.String(), rel, present, yes); err != nil {
		return err
	}

	if err := changeDriveStateUnderLock(ctx, syncDir, dbPath, refuseOfflineChange(forgetMountAction), cc.Logger,
		func() error {
			forgotten, forgetErr := syncstore.ForgetMountBoundary(ctx, dbPath, rel, cc.Logger)
			if forgetErr != nil {
				return fmt.Errorf("forget recorded mount point: %w", forgetErr)
			}
			if !forgotten {
				// A sync that ran since the check pruned it: nothing is left
				// to forget, and nothing changed.
				return fmt.Errorf("%s is no longer a recorded mount point of %s; nothing was changed", rel, cid)
			}
			return nil
		}); err != nil {
		return err
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), driveForgetMountJSONOutput{
			Drive: cid.String(), Path: rel, Forgotten: true, FilesInFolder: present,
		})
	}

	return writef(cc.Output(), "The next sync for %s reads %s as an ordinary folder and compares its %s with OneDrive.\n",
		cid.String(), rel, countOf(present, "file"))
}

// mountPointRelPath is the folder folderArg names, as the store records it:
// slash-separated and relative to the sync folder. An absolute path must lie
// inside the sync folder.
func mountPointRelPath(syncDir, folderArg string) (string, error) {
	rel := folderArg
	if filepath.IsAbs(folderArg) {
		inside, err := filepath.Rel(filepath.Clean(syncDir), filepath.Clean(folderArg))
		if err != nil {
			return "", fmt.Errorf("%s is not inside the sync folder: %w", folderArg, err)
		}
		rel = inside
	}
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("%s does not name a folder inside the sync folder", folderArg)
	}

	return rel, nil
}

// notRecordedMountPointError refuses a folder that is not a recorded mount
// point, and lists the ones that are.
func notRecordedMountPointError(drive, rel string, recorded []string) error {
	if len(recorded) == 0 {
		return fmt.Errorf("%s is not a recorded mount point of %s, which records none; nothing was changed", rel, drive)
	}

	return fmt.Errorf("%s is not a recorded mount point of %s (recorded: %s); nothing was changed",
		rel, drive, strings.Join(recorded, ", "))
}

// plainFolderFileCount checks that rel is a folder of the sync folder's own
// disk that holds content, and returns how many files it holds. Only then is
// forgetting its record safe: a missing or empty folder is a disk that is not
// mounted, whose content the next sync would otherwise delete on OneDrive,
// and a folder on another device is still a mount point.
func plainFolderFileCount(tree *synctree.Root, syncDir, rel string) (int, error) {
	folder, err := tree.IdentityNoFollow(filepath.FromSlash(rel))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, fmt.Errorf("%s is missing from the sync folder: the disk is not mounted; mount it again "+
			"instead, which loses nothing. Nothing was changed", rel)
	case err != nil:
		return 0, fmt.Errorf("look at %s: %w", rel, err)
	}
	parentRel := path.Dir(rel)
	parent, err := tree.IdentityNoFollow(filepath.FromSlash(parentRel))
	if err != nil {
		return 0, fmt.Errorf("look at the folder that holds %s: %w", rel, err)
	}
	if folder.Device != parent.Device {
		return 0, fmt.Errorf("%s is still the mount point of another disk: sync keeps its record while it is; "+
			"nothing was changed", rel)
	}

	present, err := syncdomain.CountSyncFolderFiles(filepath.Join(syncDir, filepath.FromSlash(rel)))
	if err != nil {
		return 0, fmt.Errorf("look at %s: %w", rel, err)
	}
	if present == 0 {
		return 0, fmt.Errorf("%s holds no files: that is a disk that is not mounted; mount it again instead, "+
			"which loses nothing. Forgetting it would let sync delete the disk's content on OneDrive. "+
			"Nothing was changed", rel)
	}

	return present, nil
}

// confirmForgetMountIntent shows what forgetting the record accepts and asks
// for FORGET, unless --yes was given.
func confirmForgetMountIntent(stdin io.Reader, output io.Writer, drive, rel string, present int, yes bool) error {
	if err := writef(output, "%s of %s is recorded as the mount point of another disk, and holds %s on the sync "+
		"folder's own disk now. After this, sync compares them with OneDrive as ordinary files; nothing is deleted.\n",
		rel, drive, countOf(present, "file")); err != nil {
		return err
	}
	if yes {
		return nil
	}
	if !isWriterTTY(stdinAsWriter(stdin)) {
		return fmt.Errorf("forget-mount requires confirmation; rerun with --yes or use an interactive terminal")
	}
	if err := writef(output, "Type FORGET to forget this mount point: "); err != nil {
		return err
	}

	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read forget-mount confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "FORGET" {
		return fmt.Errorf("forget-mount canceled: confirmation token did not match FORGET")
	}

	return nil
}
