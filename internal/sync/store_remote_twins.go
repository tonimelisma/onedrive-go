package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
)

// Remote twins are OneDrive siblings that OneDrive keeps as distinct items
// but that this client stores under one path, because stored paths are NFC
// and OneDrive does not compose every canonically equivalent spelling: live
// probe L-33 created U+FB2A next to U+05E9 U+05C1, and U+1F71 next to U+03AC,
// as two items each. remote_state keys rows by path, so the second item hit
// "UNIQUE constraint failed: remote_state.path" and the failed write stopped
// the whole drive on every retry.
//
// The first item to hold a path keeps it. Every other twin is stored under a
// shadow path below it, P + "/:twin:" + itemID, a segment no real stored name
// can contain (':' is forbidden in OneDrive names, and local names with ':'
// are rejected before upload), and so is its subtree. NUL would be the
// obvious marker, but SQLite's length() and substr() stop at a NUL in TEXT. A remote_name_twin issue at P blocks
// remote truth for P and everything below it (truth_status.go), so neither
// twin syncs, nothing under them is planned, and a baselined holder keeps its
// baseline. When the holder is deleted or moves away, the lowest-ID twin is
// promoted to P and syncs normally on the next pass; when a twin is renamed to
// a distinct name, its row simply moves there. The issue set is recomputed
// from the shadow rows at the end of every observation commit.

// remoteTwinShadowMarker starts the stored segment of a shadowed twin.
const remoteTwinShadowMarker = ":twin:"

func remoteTwinShadowPath(holderPath, itemID string) string {
	return holderPath + "/" + remoteTwinShadowMarker + itemID
}

// remoteTwinShadowParent returns the holder path a shadow path hangs from, or
// "" when p is not inside a shadowed twin.
func remoteTwinShadowParent(p string) string {
	idx := strings.Index(p, "/"+remoteTwinShadowMarker)
	if idx < 0 {
		return ""
	}

	return p[:idx]
}

// remoteStateHolderTx returns the item ID that holds exactly path, or "".
func remoteStateHolderTx(ctx context.Context, tx sqlTxRunner, p string) (string, error) {
	var itemID string
	err := tx.QueryRowContext(ctx, `SELECT item_id FROM remote_state WHERE path = ?`, p).Scan(&itemID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("sync: looking up the remote item at %s: %w", p, err)
	default:
		return itemID, nil
	}
}

// storedPathForObservedItem is where an observed item's row belongs: below its
// parent's shadow path when the parent is a shadowed twin, and at a shadow path
// of its own when another item already holds its path.
func (s *SyncStore) storedPathForObservedItem(ctx context.Context, tx sqlTxRunner, item *observedItem) (string, error) {
	target := item.Path
	if item.ParentID != "" {
		parent := s.scanRemoteStateRow(ctx, tx, item.DriveID.String(), item.ParentID)
		if parent != nil && remoteTwinShadowParent(parent.Path) != "" {
			target = parent.Path + "/" + path.Base(item.Path)
		}
	}

	holder, err := remoteStateHolderTx(ctx, tx, target)
	if err != nil || holder == "" || holder == item.ItemID {
		return target, err
	}

	s.logger.Warn("OneDrive has two items that this computer would store under one name; rename one of them on OneDrive",
		slog.String("path", target),
		slog.String("item_id", item.ItemID),
		slog.String("other_item_id", holder),
	)

	return remoteTwinShadowPath(target, item.ItemID), nil
}

// promoteRemoteTwinTx gives a freed path to the lowest-ID twin shadowed below
// it, together with that twin's subtree.
func promoteRemoteTwinTx(ctx context.Context, tx sqlTxRunner, freedPath string) error {
	if freedPath == "" || remoteTwinShadowParent(freedPath) != "" {
		return nil
	}
	if holder, err := remoteStateHolderTx(ctx, tx, freedPath); err != nil || holder != "" {
		return err
	}

	prefix := freedPath + "/" + remoteTwinShadowMarker
	rows, err := queryRemoteStatePairsTx(ctx, tx,
		`SELECT item_id, path FROM remote_state WHERE substr(path, 1, length(?)) = ? COLLATE BINARY`,
		prefix, prefix)
	if err != nil {
		return fmt.Errorf("sync: listing remote twins below %s: %w", freedPath, err)
	}

	var candidates []string
	for _, row := range rows {
		if row[1] == remoteTwinShadowPath(freedPath, row[0]) {
			candidates = append(candidates, row[0])
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Strings(candidates)
	shadow := remoteTwinShadowPath(freedPath, candidates[0])
	if _, err := tx.ExecContext(ctx, `UPDATE remote_state SET path = ? WHERE item_id = ?`, freedPath, candidates[0]); err != nil {
		return fmt.Errorf("sync: promoting remote twin %s to %s: %w", candidates[0], freedPath, err)
	}

	return repathSubtreeTx(ctx, tx, "remote_state", shadow, freedPath)
}

// keepRemoteTwinsAtHolderPathTx undoes the part of a holder folder's subtree
// move that carried twins shadowed at its old path: they are siblings of the
// holder at the old path, not its children.
func keepRemoteTwinsAtHolderPathTx(ctx context.Context, tx sqlTxRunner, oldPath, newPath string) error {
	from := newPath + "/" + remoteTwinShadowMarker
	to := oldPath + "/" + remoteTwinShadowMarker
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_state SET path = ? || substr(path, length(?) + 1)
		WHERE substr(path, 1, length(?)) = ? COLLATE BINARY`,
		to, from, from, from); err != nil {
		return fmt.Errorf("sync: keeping remote twins at %s: %w", oldPath, err)
	}

	return nil
}

// reconcileRemoteTwinIssuesTx makes the remote_name_twin issues exactly the
// holder paths that currently have shadowed twins.
func (s *SyncStore) reconcileRemoteTwinIssuesTx(ctx context.Context, tx sqlTxRunner) error {
	shadowRows, err := queryRemoteStatePairsTx(ctx, tx,
		`SELECT path, '' FROM remote_state WHERE instr(path, ?) > 0`, "/"+remoteTwinShadowMarker)
	if err != nil {
		return fmt.Errorf("sync: listing remote twins: %w", err)
	}

	desired := make(map[string]struct{})
	for _, row := range shadowRows {
		if holder := remoteTwinShadowParent(row[0]); holder != "" {
			desired[holder] = struct{}{}
		}
	}

	issueRows, err := queryRemoteStatePairsTx(ctx, tx,
		`SELECT path, '' FROM observation_issues WHERE issue_type = ?`, issueRemoteNameTwin)
	if err != nil {
		return fmt.Errorf("sync: listing remote twin issues: %w", err)
	}
	var stale []managedObservationIssueKey
	for _, row := range issueRows {
		if _, ok := desired[row[0]]; ok {
			delete(desired, row[0])
			continue
		}
		stale = append(stale, managedObservationIssueKey{path: row[0], issueType: issueRemoteNameTwin})
	}

	if err := deleteObservationIssuesTx(ctx, tx, stale); err != nil {
		return err
	}

	upserts := make([]ObservationIssue, 0, len(desired))
	for p := range desired {
		upserts = append(upserts, ObservationIssue{Path: p, IssueType: issueRemoteNameTwin})
	}
	sort.Slice(upserts, func(i, j int) bool { return upserts[i].Path < upserts[j].Path })

	return s.upsertObservationIssuesTx(ctx, tx, upserts)
}

// queryRemoteStatePairsTx runs a two-column TEXT query and returns its rows.
func queryRemoteStatePairsTx(ctx context.Context, tx sqlTxRunner, query string, args ...any) (_ [][2]string, err error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close rows: %w", closeErr)
		}
	}()

	var out [][2]string
	for rows.Next() {
		var row [2]string
		if scanErr := rows.Scan(&row[0], &row[1]); scanErr != nil {
			return nil, fmt.Errorf("scan: %w", scanErr)
		}
		out = append(out, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate: %w", rowsErr)
	}

	return out, nil
}
