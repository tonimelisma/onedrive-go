package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// heldSetFlag names the held set a `sync --confirm-deletes` confirms to a
// running watch, as `sync --confirm-deletes` printed it.
const heldSetFlag = "held-set"

// heldDeleteListLimit is how many held paths the confirmation prompt lists
// per drive before it says how many more there are.
const heldDeleteListLimit = 20

// errHeldDeletesNotConfirmed ends a `sync --confirm-deletes` that showed a
// running watch's held sets without naming one: nothing was confirmed.
var errHeldDeletesNotConfirmed = errors.New("held deletes not confirmed: run the command above to confirm exactly " +
	"the set it shows; a set held later is not confirmed by it")

// errHeldSetWithoutWatch is --held-set with no running watch to confirm to.
var errHeldSetWithoutWatch = errors.New("--held-set confirms held deletes to a running 'sync --watch', and none " +
	"is running; run 'onedrive-go sync --confirm-deletes' to apply the held deletes in a one-time sync")

// drivesHeldSet is one drive's held deletes as its state store records them.
type drivesHeldSet struct {
	drive        *config.ResolvedDrive
	held         []syncdomain.HeldDelete
	confirmation syncdomain.HeldDeleteConfirmation
}

// readDrivesHeldSets reads the held deletes of each drive that holds any.
func readDrivesHeldSets(ctx context.Context, cc *CLIContext, drives []*config.ResolvedDrive) ([]drivesHeldSet, error) {
	var sets []drivesHeldSet
	for _, drive := range drives {
		snapshot, err := readDriveStatusSnapshot(ctx, drive.StatePath(), cc.Logger, syncdomain.ContentFilterConfig{})
		if err != nil {
			return nil, fmt.Errorf("read the held deletes of %s: %w", drive.CanonicalID, err)
		}
		held := syncstore.HeldDeletesFromRows(snapshot.HeldDeletes)
		if len(held) == 0 {
			continue
		}
		sets = append(sets, drivesHeldSet{drive: drive, held: held, confirmation: syncdomain.ConfirmationOf(held)})
	}

	return sets, nil
}

// confirmDeletesToRunningWatch is `sync --confirm-deletes` while a watch owns
// the sync: the watch keeps running and takes the confirmation through its
// control socket (R-6.2.17). Without heldSet it shows each drive's held set
// and the command that confirms exactly that set, and confirms nothing. With
// heldSet it confirms the one drive whose held set it names; a set that is no
// longer held, here or in the watch when it takes it, is refused, so a delete
// held after the user looked is never confirmed unseen.
func confirmDeletesToRunningWatch(
	ctx context.Context,
	cc *CLIContext,
	client *controlSocketClient,
	drives []*config.ResolvedDrive,
	heldSet string,
) error {
	sets, err := readDrivesHeldSets(ctx, cc, drives)
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return writef(cc.Output(), "No deletes are held for confirmation.\n")
	}
	if heldSet == "" {
		if err := writeHeldSets(cc, sets); err != nil {
			return err
		}
		return errHeldDeletesNotConfirmed
	}

	named, err := syncdomain.ParseHeldDeleteConfirmation(heldSet)
	if err != nil {
		return fmt.Errorf("read --%s: %w", heldSetFlag, err)
	}
	target, found := heldSetNamed(sets, named)
	if !found {
		if err := writeHeldSets(cc, sets); err != nil {
			return err
		}
		return fmt.Errorf("confirm held deletes: the held deletes changed since they were shown; " +
			"nothing was confirmed. The sets held now are listed above")
	}

	if err := client.confirmDeletes(ctx, synccontrol.ConfirmDeletesRequest{
		Drive: target.drive.CanonicalID.String(),
		Count: named.Count,
		Token: named.Token,
	}); err != nil {
		return confirmDeletesAnswerError(err)
	}

	return writef(cc.Output(), "The running 'sync --watch' applies the %s of %s in its next pass.\n",
		countOf(named.Count, "confirmed delete"), target.drive.CanonicalID)
}

// heldSetNamed is the drive whose held set confirmation names.
func heldSetNamed(sets []drivesHeldSet, confirmation syncdomain.HeldDeleteConfirmation) (drivesHeldSet, bool) {
	for i := range sets {
		if sets[i].confirmation == confirmation {
			return sets[i], true
		}
	}

	return drivesHeldSet{}, false
}

// writeHeldSets shows each drive's held set and the command that confirms it.
func writeHeldSets(cc *CLIContext, sets []drivesHeldSet) error {
	for i := range sets {
		set := &sets[i]
		if err := writef(cc.Output(), "%s holds %s for confirmation:\n", set.drive.CanonicalID, countOf(len(set.held), "delete")); err != nil {
			return err
		}
		for j := range set.held {
			if j == heldDeleteListLimit {
				if err := writef(cc.Output(), "  ... and %d more\n", len(set.held)-heldDeleteListLimit); err != nil {
					return err
				}
				break
			}
			if err := writef(cc.Output(), "  %s (%s)\n", set.held[j].Path, heldDeleteSideText(set.held[j].Side)); err != nil {
				return err
			}
		}
		if err := writef(cc.Output(),
			"To apply exactly these while 'sync --watch' keeps running:\n  onedrive-go sync --confirm-deletes --%s %s\n",
			heldSetFlag, set.confirmation); err != nil {
			return err
		}
	}

	return nil
}

// heldDeleteSideText says where a held delete would remove content.
func heldDeleteSideText(side syncdomain.DeleteSafetySide) string {
	if side == syncdomain.DeleteSafetySideLocal {
		return "delete on this computer"
	}

	return "delete on OneDrive"
}

// confirmDeletesAnswerError is the error a confirmation the running watch
// refused ends the command with.
func confirmDeletesAnswerError(err error) error {
	var daemonErr *controlDaemonError
	switch {
	case errors.As(err, &daemonErr) && daemonErr.code == synccontrol.ErrorHeldDeletesChanged:
		return fmt.Errorf("confirm held deletes: %s; nothing was confirmed. Run 'onedrive-go sync --confirm-deletes' "+
			"to see the set held now", daemonErr.message)
	case errors.As(err, &daemonErr) && daemonErr.code == synccontrol.ErrorNoHeldDeletes:
		return fmt.Errorf("confirm held deletes: the running sync holds no deletes for this drive now; nothing was confirmed")
	case controlOwnerDidNotAnswer(err):
		return fmt.Errorf("confirm held deletes: the running 'sync --watch' is busy and confirmed nothing; try again: %w", err)
	default:
		return fmt.Errorf("confirm held deletes: %w", err)
	}
}
