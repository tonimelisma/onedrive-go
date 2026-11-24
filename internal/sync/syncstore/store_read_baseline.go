// Baseline reads of the sync store.
//
// Contents:
//   - Load:                  read baseline table into memory
//   - scanBaselineRow:       scan single baseline row with nullable handling
//   - CheckCacheConsistency: verify in-memory cache matches DB

package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Load returns the store's baseline cache, reading the baseline table on the
// first call. Every later call returns the same cache: the store keeps it
// current itself (CommitMutation patches it after each transaction), which
// is safe because SyncStore exclusively owns the database (sole-writer
// pattern with SetMaxOpenConns(1)). Callers get the read-only view; only the
// store writes the cache.
func (s *SyncStore) Load(ctx context.Context) (syncdomain.BaselineView, error) {
	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()

	if s.baseline != nil {
		return s.baseline, nil
	}

	contentDriveID, err := s.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("syncstore: loading content drive for baseline: %w", err)
	}

	entries, err := loadBaselineEntries(ctx, s.db, contentDriveID)
	if err != nil {
		return nil, err
	}

	s.baseline = newBaselineCache(indexBaselineEntries(entries))
	s.logger.Debug("baseline loaded", slog.Int("entries", len(entries)))

	return s.baseline, nil
}

// loadBaselineWithRunner returns the baseline rows runner sees as an
// immutable snapshot. Inside a read transaction it is the baseline of that
// transaction's snapshot, which is what planning must use alongside SQL
// comparison rows from the same snapshot.
func loadBaselineWithRunner(ctx context.Context, runner sqlTxRunner, contentDriveID driveid.ID) (*syncdomain.BaselineSnapshot, error) {
	entries, err := loadBaselineEntries(ctx, runner, contentDriveID)
	if err != nil {
		return nil, err
	}

	return syncdomain.NewBaselineSnapshot(entries), nil
}

// loadBaselineEntries reads every baseline row runner sees.
func loadBaselineEntries(ctx context.Context, runner sqlTxRunner, contentDriveID driveid.ID) ([]*syncdomain.BaselineEntry, error) {
	rows, err := runner.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return nil, fmt.Errorf("syncstore: loading baseline: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var entries []*syncdomain.BaselineEntry
	for rows.Next() {
		entry, err := scanBaselineRow(rows, contentDriveID)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncstore: iterating baseline rows: %w", err)
	}

	return entries, nil
}

// scanBaselineRow scans a single row from the baseline table, handling
// nullable columns with sql.Null* types.
func scanBaselineRow(rows *sql.Rows, contentDriveID driveid.ID) (*syncdomain.BaselineEntry, error) {
	var (
		e                syncdomain.BaselineEntry
		parentID         sql.NullString
		localHash        sql.NullString
		remoteHash       sql.NullString
		localSize        sql.NullInt64
		remoteSize       sql.NullInt64
		localMtime       sql.NullInt64
		remoteMtime      sql.NullInt64
		localDevice      int64
		localInode       int64
		localHasIdentity int
		etag             sql.NullString
	)

	err := rows.Scan(
		&e.ItemID, &e.Path, &parentID, &e.ItemType,
		&localHash, &remoteHash, &localSize, &remoteSize, &localMtime, &remoteMtime,
		&e.LocalCtime, &localDevice, &localInode, &e.LocalBirthTime, &localHasIdentity, &etag,
	)
	if err != nil {
		return nil, fmt.Errorf("syncstore: scanning baseline row: %w", err)
	}

	e.DriveID = contentDriveID
	e.ParentID = parentID.String
	e.LocalHash = localHash.String
	e.RemoteHash = remoteHash.String
	e.ETag = etag.String

	if localSize.Valid {
		e.LocalSize = localSize.Int64
		e.LocalSizeKnown = true
	}

	if remoteSize.Valid {
		e.RemoteSize = remoteSize.Int64
		e.RemoteSizeKnown = true
	}

	if localMtime.Valid {
		e.LocalMtime = localMtime.Int64
	}

	if remoteMtime.Valid {
		e.RemoteMtime = remoteMtime.Int64
	}
	e.LocalDevice = uint64(localDevice)
	e.LocalInode = uint64(localInode)
	e.LocalHasIdentity = localHasIdentity != 0

	return &e, nil
}

// ErrBaselineCacheDiverged reports a baseline cache that differs from the
// baseline table.
var ErrBaselineCacheDiverged = errors.New("syncstore: the baseline cache differs from the database")

// maxReportedCacheDivergences bounds how many paths a divergence error names.
const maxReportedCacheDivergences = 5

// CheckCacheConsistency compares the baseline cache with the baseline table,
// every field of every entry, and returns ErrBaselineCacheDiverged naming
// the paths that differ. Without a cache there is nothing to compare. The
// sync invariant suite runs it after every scenario, model round and crash
// recovery (R-6.5.2).
func (s *SyncStore) CheckCacheConsistency(ctx context.Context) error {
	cache := s.cachedBaseline()
	if cache == nil {
		return nil
	}

	contentDriveID, err := s.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return fmt.Errorf("syncstore: loading content drive for consistency check: %w", err)
	}
	entries, err := loadBaselineEntries(ctx, s.db, contentDriveID)
	if err != nil {
		return fmt.Errorf("syncstore: reading the baseline for consistency check: %w", err)
	}
	stored := make(map[string]*syncdomain.BaselineEntry, len(entries))
	for _, entry := range entries {
		stored[entry.Path] = entry
	}

	var diverged []string
	// The callback reads only the local map: ForEachPath holds the cache's
	// read lock for its duration.
	cache.ForEachPath(func(path string, cached syncdomain.BaselineEntry) {
		if row, ok := stored[path]; !ok || *row != cached {
			diverged = append(diverged, path)
		}
	})
	for path := range stored {
		if _, ok := cache.GetByPath(path); !ok {
			diverged = append(diverged, path)
		}
	}
	if len(diverged) == 0 {
		return nil
	}
	slices.Sort(diverged)
	shown := diverged[:min(len(diverged), maxReportedCacheDivergences)]

	return fmt.Errorf("%w: %d paths, first %s", ErrBaselineCacheDiverged, len(diverged), strings.Join(shown, ", "))
}
