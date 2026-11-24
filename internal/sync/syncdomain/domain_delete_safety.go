package syncdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Delete-safety vocabulary shared by the planner, which decides the hold
// (planner_delete_safety.go), the store, which records it for status, and the
// engine report. The policy itself lives with the planner.

// DeleteSafetySide names the side a held delete would remove content from.
type DeleteSafetySide string

const (
	DeleteSafetySideLocal  DeleteSafetySide = "local"
	DeleteSafetySideRemote DeleteSafetySide = "remote"
)

// DeleteSafetyReason records which rule held a direction's deletes.
type DeleteSafetyReason string

const (
	// DeleteSafetyReasonSideVanished: the opposite side has no entries.
	DeleteSafetyReasonSideVanished DeleteSafetyReason = "side_vanished"
	// DeleteSafetyReasonTooMany: the proportional threshold was exceeded.
	DeleteSafetyReasonTooMany DeleteSafetyReason = "too_many"
	// DeleteSafetyReasonMountPointGone: the delete removes a recorded mount
	// point whose folder is gone from the sync folder, or something below
	// it, so it is held however few such deletes there are.
	DeleteSafetyReasonMountPointGone DeleteSafetyReason = "mount_point_gone"
)

// DeleteSafetyWindow is how long an executed, unconfirmed delete keeps
// counting toward the proportional rule. A hold that judged each plan alone
// would never stop a process that removes files below the threshold on every
// pass (a failing disk, a misbehaving app, a cron job) and so empties a drive
// one small batch at a time. Deletes the user confirmed with
// `sync --confirm-deletes` are not counted: the confirmation reviewed them.
const DeleteSafetyWindow = 24 * time.Hour

// HeldDelete is one delete the hold kept out of the executable plan.
type HeldDelete struct {
	Path     string
	Side     DeleteSafetySide
	Reason   DeleteSafetyReason
	ItemType ItemType
	// ItemID binds a confirmation to the item that was held, so the same
	// path holding a different item later is not covered by it.
	ItemID string
}

// DeleteSafetySideOf reports whether an action type deletes a synced item, and
// the side it removes content from: the planner's hold and its confirmation
// matching, and the store's delete ledger, all ask it. A cleanup removes only
// the baseline row of an item both sides already lost, so it is no such
// delete.
func DeleteSafetySideOf(action ActionType) (DeleteSafetySide, bool) {
	switch action {
	case ActionLocalDelete:
		return DeleteSafetySideLocal, true
	case ActionRemoteDelete:
		return DeleteSafetySideRemote, true
	case ActionDownload, ActionUpload, ActionLocalMove, ActionRemoteMove, ActionFolderCreate,
		ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return "", false
	default:
		panic(OutsideTheEnum(action))
	}
}

// heldDeleteTokenLength is how many hex digits of the held set's digest a
// confirmation carries: 64 bits, short enough to type and far beyond what
// two held sets of one drive could share by chance.
const heldDeleteTokenLength = 16

// HeldDeleteConfirmation names one held set the user was shown: how many
// deletes it held and a token derived from exactly those deletes. A running
// watch applies a confirmation only while its held set is still that set, so
// a delete held after the user looked is never confirmed unseen (R-6.2.17).
type HeldDeleteConfirmation struct {
	Count int
	Token string
}

// ConfirmationOf is the confirmation that names held: its count and the token
// of its deletes, whatever their order.
func ConfirmationOf(held []HeldDelete) HeldDeleteConfirmation {
	lines := make([]string, 0, len(held))
	for i := range held {
		lines = append(lines, strings.Join([]string{
			string(held[i].Side), held[i].Path, held[i].ItemID, held[i].ItemType.String(), string(held[i].Reason),
		}, "\x00"))
	}
	slices.Sort(lines)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))

	return HeldDeleteConfirmation{
		Count: len(held),
		Token: hex.EncodeToString(digest[:])[:heldDeleteTokenLength],
	}
}

// Matches reports whether c names exactly held.
func (c HeldDeleteConfirmation) Matches(held []HeldDelete) bool {
	return c == ConfirmationOf(held)
}

// String is the confirmation as the user types it: the count, a dash and the
// token ("215-3fa9c1d2e4b5a6f7").
func (c HeldDeleteConfirmation) String() string {
	return strconv.Itoa(c.Count) + "-" + c.Token
}

// ParseHeldDeleteConfirmation reads a confirmation as String writes it.
func ParseHeldDeleteConfirmation(text string) (HeldDeleteConfirmation, error) {
	countText, token, found := strings.Cut(strings.TrimSpace(text), "-")
	if !found {
		return HeldDeleteConfirmation{}, fmt.Errorf("parse held set %q: expected <count>-<token>", text)
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count < 1 {
		return HeldDeleteConfirmation{}, fmt.Errorf("parse held set %q: the count must be a positive number", text)
	}
	if _, decodeErr := hex.DecodeString(token); decodeErr != nil || len(token) != heldDeleteTokenLength {
		return HeldDeleteConfirmation{}, fmt.Errorf("parse held set %q: the token must be %d hex digits", text, heldDeleteTokenLength)
	}

	return HeldDeleteConfirmation{Count: count, Token: strings.ToLower(token)}, nil
}
