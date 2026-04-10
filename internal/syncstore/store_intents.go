package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// DurableIntentCounts summarizes queued user decisions for control-socket
// status. Counts are read-only diagnostics; mutation still goes through the
// typed store methods.
type DurableIntentCounts struct {
	PendingHeldDeleteApprovals int
	PendingConflictRequests    int
	ResolvingConflictRequests  int
	FailedConflictRequests     int
}

type durableIntentQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const (
	syncMetadataLastStaleHeldDeletePruneCount = "last_stale_held_delete_prune_count"
	syncMetadataLastStaleHeldDeletePruneTime  = "last_stale_held_delete_prune_time"
)

func (m *SyncStore) CountDurableIntents(ctx context.Context) (DurableIntentCounts, error) {
	return countDurableIntents(ctx, m.db)
}

func countDurableIntents(ctx context.Context, db durableIntentQuerier) (DurableIntentCounts, error) {
	var counts DurableIntentCounts

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM held_deletes WHERE state = ?`,
		synctypes.HeldDeleteStateApproved,
	).Scan(&counts.PendingHeldDeleteApprovals); err != nil {
		if isMissingTableErr(err) {
			return counts, nil
		}
		return DurableIntentCounts{}, fmt.Errorf("sync: count approved held deletes: %w", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM conflict_requests
		WHERE state IN (?, ?, ?)
		GROUP BY state`,
		synctypes.ConflictStateResolutionRequested,
		synctypes.ConflictStateResolving,
		synctypes.ConflictStateResolveFailed,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return counts, nil
		}
		return DurableIntentCounts{}, fmt.Errorf("sync: count conflict requests: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			state string
			count int
		)
		if err := rows.Scan(&state, &count); err != nil {
			return DurableIntentCounts{}, fmt.Errorf("sync: scan conflict request count: %w", err)
		}
		switch state {
		case synctypes.ConflictStateResolutionRequested:
			counts.PendingConflictRequests = count
		case synctypes.ConflictStateResolving:
			counts.ResolvingConflictRequests = count
		case synctypes.ConflictStateResolveFailed:
			counts.FailedConflictRequests = count
		}
	}
	if err := rows.Err(); err != nil {
		return DurableIntentCounts{}, fmt.Errorf("sync: iterate conflict request counts: %w", err)
	}

	return counts, nil
}

func (m *SyncStore) RecordStaleHeldDeletePrune(ctx context.Context, count int, at time.Time) error {
	if count <= 0 {
		return nil
	}

	return m.UpsertSyncMetadataEntries(ctx, map[string]string{
		syncMetadataLastStaleHeldDeletePruneCount: strconv.Itoa(count),
		syncMetadataLastStaleHeldDeletePruneTime:  at.UTC().Format(time.RFC3339Nano),
	})
}
