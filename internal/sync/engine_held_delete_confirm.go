package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// ErrHeldDeletesChanged is a confirmation that names a held set other than
// the one held now: a delete was held, or released, after the user was shown
// the set. Nothing is confirmed; the user looks at the set again (R-6.2.17).
var ErrHeldDeletesChanged = errors.New("the held deletes changed since they were shown")

// ErrNoHeldDeletes is a confirmation that reached a drive holding no deletes.
var ErrNoHeldDeletes = errors.New("no deletes are held for confirmation")

// ErrConfirmationNotTaken is a confirmation no running watch of the engine
// took before its context ended: the engine is not watching, or its watch is
// not running steadily yet (bootstrap, drain). Nothing was confirmed.
var ErrConfirmationNotTaken = errors.New("the running sync did not take the confirmation")

// heldDeleteConfirmRequest is one confirmation on its way to the watch loop,
// with the buffered channel the loop answers on.
type heldDeleteConfirmRequest struct {
	confirmation syncdomain.HeldDeleteConfirmation
	answer       chan error
}

// ConfirmHeldDeletes hands confirmation to the engine's running watch, on any
// goroutine, and waits for the watch loop's answer: nil once the loop has
// taken exactly the held set the confirmation names, which its next plan
// releases (and no other delete held since); ErrHeldDeletesChanged when the
// set held now is another one; ErrNoHeldDeletes when none is held; and
// ErrConfirmationNotTaken when no running watch took it before ctx ended.
func (e *Engine) ConfirmHeldDeletes(ctx context.Context, confirmation syncdomain.HeldDeleteConfirmation) error {
	request := heldDeleteConfirmRequest{confirmation: confirmation, answer: make(chan error, 1)}
	select {
	case e.heldDeleteConfirms <- request:
	case <-ctx.Done():
		return fmt.Errorf("sync: confirm held deletes: %w: %w", ErrConfirmationNotTaken, ctx.Err())
	}
	select {
	case err := <-request.answer:
		return err
	case <-ctx.Done():
		// The loop answers on a buffered channel, so it never blocks on a
		// caller that left; it took the confirmation, which stands.
		return fmt.Errorf("sync: confirm held deletes: waiting for the answer: %w", ctx.Err())
	}
}

// acceptHeldDeleteConfirmation checks confirmation against the held set the
// store records now and, when it names exactly that set, keeps the set for the
// run's next plan to release (approvedHeldDeletes). Any other set is refused,
// so a delete held after the user looked is judged by the hold again rather
// than confirmed unseen.
func (flow *engineFlow) acceptHeldDeleteConfirmation(
	ctx context.Context,
	confirmation syncdomain.HeldDeleteConfirmation,
) error {
	held, err := flow.deps.store.ListHeldDeletes(ctx)
	if err != nil {
		return fmt.Errorf("sync: reading the held deletes to confirm: %w", err)
	}
	if len(held) == 0 {
		return ErrNoHeldDeletes
	}
	if !confirmation.Matches(held) {
		return fmt.Errorf("%w: %d were shown, %d are held now", ErrHeldDeletesChanged, confirmation.Count, len(held))
	}
	flow.confirmedHeldDeletes = held
	flow.deps.logger.Info("held deletes confirmed; the next pass applies them",
		slog.Int("count", len(held)),
		slog.String("held_set", confirmation.String()),
	)

	return nil
}

// approvedHeldDeletes is the held set this plan releases: the set a running
// watch's user confirmed (acceptHeldDeleteConfirmation), taken once, or the
// recorded set a `sync --confirm-deletes` run confirms; nil releases nothing.
func (flow *engineFlow) approvedHeldDeletes(ctx context.Context, opts RunOptions) ([]syncdomain.HeldDelete, error) {
	if flow.confirmedHeldDeletes != nil {
		approved := flow.confirmedHeldDeletes
		flow.confirmedHeldDeletes = nil
		return approved, nil
	}
	if !opts.ConfirmDeletes {
		return nil, nil
	}
	approved, err := flow.deps.store.ListHeldDeletes(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: reading the held deletes to confirm: %w", err)
	}

	return approved, nil
}

// handleHeldDeleteConfirm answers one confirmation on the watch loop and, when
// it was taken, asks for the replan that releases the confirmed set.
func (rt *watchRuntime) handleHeldDeleteConfirm(ctx context.Context, request heldDeleteConfirmRequest) {
	err := rt.acceptHeldDeleteConfirmation(ctx, request.confirmation)
	if err == nil && rt.resources.dirtyBuf != nil {
		rt.resources.dirtyBuf.MarkDirty()
	}
	request.answer <- err
}
