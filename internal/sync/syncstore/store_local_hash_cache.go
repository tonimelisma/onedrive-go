package syncstore

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The local hash cache makes a first scan's work durable as it goes. A file
// the baseline does not know is hashed in the scan's hash phase, and that
// hash becomes durable only when the pass commits the file's baseline row:
// a kill, a restart or an OOM during the first scan of a large tree repeated
// every read. The scan now records each hash it computes for such a file,
// with the metadata the fast path (R-2.2.4) requires to still match, and the
// next scan trusts a recorded hash under exactly those conditions (R-2.2.8).
// Rows leave the cache once the file has a baseline row or is gone.

// storeLocalHashCache is the store's cache: the rows loaded once at scan
// start, and writes going straight to the table.
type storeLocalHashCache struct {
	store   *SyncStore
	entries map[string]syncdomain.LocalHashCacheEntry
}

// NewLocalHashCache loads the cache for a scan.
func (s *SyncStore) NewLocalHashCache(ctx context.Context) (*storeLocalHashCache, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, hash, size, mtime, ctime, local_device, local_inode, local_birth_time FROM local_hash_cache`)
	if err != nil {
		return nil, fmt.Errorf("syncstore: loading the local hash cache: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	entries := map[string]syncdomain.LocalHashCacheEntry{}
	for rows.Next() {
		var e syncdomain.LocalHashCacheEntry
		if err := rows.Scan(&e.Path, &e.Hash, &e.Size, &e.Mtime, &e.Ctime, &e.Device, &e.Inode, &e.BirthTime); err != nil {
			return nil, fmt.Errorf("syncstore: scanning the local hash cache: %w", err)
		}
		entries[e.Path] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncstore: iterating the local hash cache: %w", err)
	}

	return &storeLocalHashCache{store: s, entries: entries}, nil
}

func (c *storeLocalHashCache) Lookup(path string) (syncdomain.LocalHashCacheEntry, bool) {
	e, ok := c.entries[path]
	return e, ok
}

// record writes a batch of hashes in one transaction. It is called from the
// scan's hash workers while the scan runs, so it is the one store write that
// happens during a scan; the single-writer store serializes it.
func (c *storeLocalHashCache) Record(ctx context.Context, entries []syncdomain.LocalHashCacheEntry) (err error) {
	if len(entries) == 0 {
		return nil
	}
	tx, err := beginPerfTx(ctx, c.store.db)
	if err != nil {
		return fmt.Errorf("syncstore: begin local hash cache transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rollback local hash cache transaction")
	}()
	for i := range entries {
		e := &entries[i]
		if _, err = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO local_hash_cache
			 (path, hash, size, mtime, ctime, local_device, local_inode, local_birth_time)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			e.Path, e.Hash, e.Size, e.Mtime, e.Ctime, e.Device, e.Inode, e.BirthTime,
		); err != nil {
			return fmt.Errorf("syncstore: recording the hash of %s: %w", e.Path, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: commit local hash cache transaction: %w", err)
	}

	return nil
}

// pruneLocalHashCacheTx drops cached hashes the baseline now owns and those
// of files the committed local snapshot no longer has. It runs inside the
// snapshot's own transaction.
func pruneLocalHashCacheTx(ctx context.Context, tx sqlTxRunner) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM local_hash_cache
		 WHERE path IN (SELECT path FROM baseline)
		    OR path NOT IN (SELECT path FROM local_state)`); err != nil {
		return fmt.Errorf("syncstore: pruning the local hash cache: %w", err)
	}

	return nil
}
