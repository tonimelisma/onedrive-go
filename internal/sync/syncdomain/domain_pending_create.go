package syncdomain

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// PendingCreateIntentTTL is how long an intent without an item ID can claim
// an empty file observed at its path. Past it the intent is pruned.
const PendingCreateIntentTTL = 72 * time.Hour

// PendingCreate is one recorded, not yet confirmed create. ItemID is empty
// while only the intent is recorded.
type PendingCreate struct {
	ItemID      string
	DriveID     driveid.ID
	Path        string
	ParentID    string
	CreatedETag string
	LocalSize   int64
	LocalHash   string
	IntendedAt  int64 // when the create was decided, as StoredTimeNano encodes it
}

// IntentIsFresh reports whether the intent was recorded within
// PendingCreateIntentTTL of now, so that an empty file at its path can still
// be the file its create made. Observation's claim, the executor's claim on a
// 409 and the prune all ask this one question.
func (p *PendingCreate) IntentIsFresh(now time.Time) bool {
	return StoredTimeIsSet(p.IntendedAt) && now.Sub(time.Unix(0, p.IntendedAt)) <= PendingCreateIntentTTL
}
