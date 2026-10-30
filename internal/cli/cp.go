package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newCpCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "cp <source> <dest>",
		Short: "Copy a file or folder (server-side)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCp(cmd, args, force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing file at destination")

	return cmd
}

// cpJSONOutput is the JSON output schema for the cp command.
type cpJSONOutput struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ID          string `json:"id"`
}

// copyPollInterval is the polling interval for async copy status.
const copyPollInterval = 1 * time.Second

// copyTimeout is the maximum time to wait for an async copy to complete.
const copyTimeout = 5 * time.Minute

// Copy monitor statuses (asyncJobStatus). Only completed and the
// in-progress ones are named; everything else ends the poll as a failure.
const (
	copyStatusCompleted  = "completed"
	copyStatusInProgress = "inProgress"
	copyStatusNotStarted = "notStarted"
	copyStatusWaiting    = "waiting"
	copyStatusUpdating   = "updating"
)

// copyStatusOutcome reports whether a monitor status ends the poll, and the
// error when it ended in failure. Every status other than the documented
// in-progress ones ("failed", "deletePending", "deleteFailed", or one
// Microsoft adds later) is terminal: polling it until the five-minute timeout only
// delayed the failure (RCL-081).
func copyStatusOutcome(status string) (bool, error) {
	switch status {
	case copyStatusCompleted:
		return true, nil
	case copyStatusInProgress, copyStatusNotStarted, copyStatusWaiting, copyStatusUpdating:
		return false, nil
	default:
		return true, fmt.Errorf("copy %s", status)
	}
}

// applySourceMtime gives the copy its source's modification time. A
// server-side copy is stamped with the copy time, so a later sync saw every
// copied file as changed (RCL-028). The copy itself already succeeded, so a
// failure here is a warning, not a failed command.
func applySourceMtime(
	ctx context.Context, cc *CLIContext, session *driveops.MountSession, source *graph.Item, copyID string,
) {
	if source == nil || source.IsFolder || source.ModifiedAt.IsZero() || copyID == "" {
		return
	}

	if _, err := session.Meta.UpdateFileSystemInfo(ctx, session.DriveID, copyID, source.ModifiedAt); err != nil {
		cc.Logger.Warn("the copy keeps the time it was made, not the source's modification time",
			slog.Any("error", err),
		)
	}
}

// checkSelfCopy returns an error if the copy would overwrite the source item.
func checkSelfCopy(sourceID string, dest destInfo) error {
	if isSelfReference(sourceID, dest) {
		return fmt.Errorf("cannot copy a file over itself")
	}

	return nil
}

func runCp(cmd *cobra.Command, args []string, force bool) error {
	sourcePath := args[0]
	destPath := args[1]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	cc.Logger.Debug("cp", "source", sourcePath, "dest", destPath)

	sourceItem, err := session.ResolveItem(ctx, sourcePath)
	if err != nil {
		return newRemoteOpError("resolving source", sourcePath, err)
	}

	dest, err := resolveDest(ctx, session, destPath, sourceItem.Name, force)
	if err != nil {
		return err
	}

	// Bail early if copying a file over itself — prevents both data loss
	// (delete then fail) and silent duplicates (API auto-rename).
	if selfErr := checkSelfCopy(sourceItem.ID, dest); selfErr != nil {
		return selfErr
	}

	wait := awaitCopy
	if cc.seams.cpAwaitCopy != nil {
		// Copy monitor URLs must be HTTPS on Microsoft hosts, so a fake
		// Graph server cannot serve one; tests stand in for the poll here.
		wait = cc.seams.cpAwaitCopy
	}

	// With --force, an existing destination is set aside, not deleted, until
	// the copy has completed (D-082).
	var resourceID string
	if force {
		hintAboutLeftoverAside(ctx, cc, session, destPath, dest)
	}

	err = replaceDestination(ctx, cc, session, destPath, dest, time.Now(), func() error {
		copyResult, copyErr := session.CopyItem(ctx, sourceItem.ID, dest.parentID, dest.newName)
		if copyErr != nil {
			return newRemoteOpError("copying", sourcePath, copyErr)
		}

		resourceID, copyErr = wait(ctx, cc, session.Meta, copyResult.MonitorURL)
		return copyErr
	})
	if err != nil {
		return err
	}

	applySourceMtime(ctx, cc, session, sourceItem, resourceID)

	displayDest := destPath
	if dest.destIsDir {
		displayDest = joinDisplayPath(destPath, dest.newName)
	}

	if cc.Flags.JSON {
		return printCpJSON(cc.Output(), cpJSONOutput{
			Source:      sourcePath,
			Destination: displayDest,
			ID:          resourceID,
		})
	}

	cc.Statusf("Copied %s → %s\n", sourcePath, displayDest)

	return nil
}

// printCpJSON writes the cp command's JSON output to w.
func printCpJSON(w io.Writer, out cpJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode copy output: %w", err)
	}

	return nil
}

// awaitCopy polls the monitor URL until the copy completes, fails, or times out.
func awaitCopy(ctx context.Context, cc *CLIContext, meta *graph.Client, monitorURL string) (string, error) {
	deadline := time.Now().Add(copyTimeout)

	for time.Now().Before(deadline) {
		status, pollErr := meta.PollCopyStatus(ctx, monitorURL)
		if pollErr != nil {
			return "", fmt.Errorf("polling copy status: %w", pollErr)
		}

		if done, err := copyStatusOutcome(status.Status); done {
			return status.ResourceID, err
		}

		cc.Progressf("Copying: %.0f%%", status.PercentageComplete)

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("await copy: %w", ctx.Err())
		case <-time.After(copyPollInterval):
		}
	}

	return "", fmt.Errorf("copy timed out after %v", copyTimeout)
}
