package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// sqlSameLocalEntry* are the one SQL definition of "the same filesystem
// entry" for a local row and a baseline row b that both carry local identity
// (local_has_identity = 1). Each is the same pattern for one local alias L:
//
//	(L.local_device = b.local_device AND L.local_inode = b.local_inode
//	 AND (L.local_birth_time = 0 OR b.local_birth_time = 0
//	      OR L.local_birth_time = b.local_birth_time))
//
// that is, the same device and inode, and the same birth time unless either
// row lacks one (zero): synctree.SameIdentity in SQL. They are written out
// per alias because the queries are constants;
// TestSQLSameLocalEntry_EveryAliasFollowsThePattern holds them to the
// pattern. Every identity test in the comparison queries uses one. A reused
// inode number keeps device and inode but not the birth time, so where the
// filesystem records birth times a deleted entry and an unrelated new one
// never pair (R-2.17.1).
const (
	sqlSameLocalEntryL = `(l.local_device = b.local_device AND l.local_inode = b.local_inode` +
		` AND (l.local_birth_time = 0 OR b.local_birth_time = 0 OR l.local_birth_time = b.local_birth_time))`
	sqlSameLocalEntryLs = `(ls.local_device = b.local_device AND ls.local_inode = b.local_inode` +
		` AND (ls.local_birth_time = 0 OR b.local_birth_time = 0 OR ls.local_birth_time = b.local_birth_time))`
	sqlSameLocalEntrySrc = `(src.local_device = b.local_device AND src.local_inode = b.local_inode` +
		` AND (src.local_birth_time = 0 OR b.local_birth_time = 0 OR src.local_birth_time = b.local_birth_time))`
)

const (
	// sqlLocalAndRemoteMoveCTEs is the shared head of the comparison and
	// reconciliation queries: the path universe plus local and remote move
	// pairing. Both queries must classify moves identically, so the pairing
	// rules live here once.
	//
	// Local move pairing: a baseline row and an unbaselined local_state row
	// pair on filesystem identity (sqlSameLocalEntry*: device, inode and,
	// where both know it, birth time; same item type). For files
	// identity is necessary but not sufficient, see the corroboration clause.
	// Directories pair on identity alone: a directory has no content of its
	// own, its descendants are paired independently, and a wrong pairing after
	// rmdir + mkdir reuse converges to the same tree, but would give the new
	// folder the old OneDrive item (its sharing, permissions and history),
	// which is why the birth time is part of identity. Baseline files
	// without identity fall back to unique content-hash pairing.
	sqlLocalAndRemoteMoveCTEs = `
WITH
all_paths AS (
	SELECT path FROM baseline
	UNION
	SELECT path FROM local_state
	UNION
	SELECT path FROM remote_state
),
local_move_candidates AS (
	SELECT
		b.path AS source_path,
		l.path AS target_path
	FROM baseline b
	JOIN local_state l
		ON l.path <> b.path
		AND COALESCE(l.item_type, '') = COALESCE(b.item_type, '')
		AND l.local_has_identity = 1
		AND b.local_has_identity = 1
		AND ` + sqlSameLocalEntryL + `
		AND (
			COALESCE(b.item_type, '') = 'folder'
			OR (
				-- Identity alone never proves a file move: after delete +
				-- create in one batch the filesystem may hand the freed inode
				-- to an unrelated file. A rename preserves size, mtime and
				-- content, so the destination must still agree with the
				-- baseline on size and on content (both hashes known) or,
				-- when either hash is unknown, on mtime.
				b.local_size IS NOT NULL
				AND l.size IS NOT NULL
				AND l.size = b.local_size
				AND (
					CASE
						WHEN NULLIF(b.local_hash, '') IS NOT NULL AND NULLIF(l.hash, '') IS NOT NULL
							THEN l.hash = b.local_hash
						ELSE COALESCE(b.local_mtime, 0) <> 0 AND l.mtime = b.local_mtime
					END
				)
			)
		)
	WHERE NOT EXISTS (SELECT 1 FROM baseline target WHERE target.path = l.path)
		AND EXISTS (SELECT 1 FROM remote_state rs WHERE rs.item_id = b.item_id)
		AND NOT EXISTS (SELECT 1 FROM remote_state occupied WHERE occupied.path = l.path AND occupied.item_id <> b.item_id)
		-- The item has not left its baseline path while that path still
		-- holds its identity: a second name for the same inode is a hard
		-- link, which uploads as a new file, never as a move (D-056).
		AND NOT EXISTS (
			SELECT 1 FROM local_state src
			WHERE src.path = b.path
				AND src.local_has_identity = 1
				AND ` + sqlSameLocalEntrySrc + `
		)
	UNION ALL
	SELECT
		b.path AS source_path,
		l.path AS target_path
	FROM baseline b
	JOIN local_state l
		ON l.path <> b.path
		AND COALESCE(l.item_type, '') = 'file'
		AND COALESCE(b.item_type, '') = 'file'
		AND b.local_has_identity = 0
		AND NULLIF(l.hash, '') = NULLIF(b.local_hash, '')
	WHERE b.local_hash IS NOT NULL
		AND b.local_hash <> ''
		-- Without identity, content alone cannot tell a move from a copy: the
		-- source must be gone.
		AND NOT EXISTS (SELECT 1 FROM local_state src WHERE src.path = b.path)
		AND NOT EXISTS (SELECT 1 FROM baseline target WHERE target.path = l.path)
		AND EXISTS (SELECT 1 FROM remote_state rs WHERE rs.item_id = b.item_id)
		AND NOT EXISTS (SELECT 1 FROM remote_state occupied WHERE occupied.path = l.path AND occupied.item_id <> b.item_id)
),
local_move_sources AS MATERIALIZED (
	SELECT
		source_path,
		MIN(target_path) AS target_path,
		COUNT(*) AS candidate_count
	FROM local_move_candidates
	GROUP BY source_path
),
-- The per-path join on target_path must be able to use an index. MIN()
-- strips the column's TEXT affinity, and a join key without affinity gets no
-- automatic index: every path scanned every move, so a local folder rename
-- cost moved items x paths (57 s at 10k items). The CAST restores it.
local_move_targets AS MATERIALIZED (
	SELECT
		CAST(target_path AS TEXT) AS target_path,
		source_path,
		candidate_count
	FROM local_move_sources
),
-- A move pairs only while the other side still holds the item. A local move
-- of an item deleted on OneDrive is a fresh upload at the destination; a
-- remote move of an item deleted or moved locally is a fresh download (or,
-- when the local side moved it too, nothing: the local move wins). Pairing
-- them anyway planned a move whose source was gone, superseded it, and
-- replanned it on every pass.
remote_move_sources AS (
	SELECT
		b.path AS source_path,
		r.path AS target_path
	FROM baseline b
	JOIN remote_state r ON r.item_id = b.item_id AND r.path <> b.path
	-- The local source must still be the item: a type change at the source
	-- is a replacement, not something a rename could carry, and so is an
	-- entry with another filesystem identity unless it is a file that
	-- provably holds the synced content. This is the test the local move's
	-- freshness and executor apply (localRowIsBaselineItem); pairing more
	-- planned a move they refused on every pass.
	WHERE EXISTS (
			SELECT 1 FROM local_state ls
			WHERE ls.path = b.path AND COALESCE(ls.item_type, '') = COALESCE(b.item_type, '')
				AND (
					COALESCE(b.local_has_identity, 0) = 0
					OR COALESCE(ls.local_has_identity, 0) = 0
					OR ` + sqlSameLocalEntryLs + `
					OR (
						COALESCE(ls.item_type, '') = 'file'
						AND COALESCE(ls.hash, '') <> '' AND ls.hash = b.local_hash
						AND (b.local_size IS NULL OR ls.size = b.local_size)
					)
				)
		)
		-- A destination the other side filled with something else in the
		-- meantime is not a move target: the source plays out as a delete
		-- (hash-verified, recoverable) and the destination as a create/create
		-- conflict that keeps both versions. Paired, the move found its
		-- destination taken and was superseded on every pass.
		AND NOT EXISTS (
			SELECT 1 FROM local_state occupied
			WHERE occupied.path = r.path
				AND NOT EXISTS (SELECT 1 FROM baseline bo WHERE bo.path = r.path)
		)
),
-- remote_move_shadowed holds remote paths of items the local side moved: the
-- local move carries the item there, so the remote copy must not also be
-- downloaded as a new file.
remote_move_shadowed AS (
	SELECT r.path AS path
	FROM local_move_sources lms
	JOIN baseline b ON b.path = lms.source_path
	JOIN remote_state r ON r.item_id = b.item_id AND r.path <> b.path
	WHERE lms.candidate_count = 1
),
remote_move_targets AS (
	SELECT
		target_path,
		source_path
	FROM remote_move_sources
),
-- An item deleted locally that OneDrive only carried along with a moved
-- ancestor: relative to that ancestor it sits where the baseline has it. The
-- ancestor is still local, so its local move re-paths the baseline row and
-- the next pass plans the delete like any other. Until then neither path is
-- a fact of its own: the remote one is not a new file to download (that
-- undid the user's delete) and the baseline one is not gone from both sides.
followed_ancestor_deletes AS (
	SELECT
		b.path AS baseline_path,
		r.path AS remote_path
	FROM baseline anc
	JOIN remote_state ra ON ra.item_id = anc.item_id AND ra.path <> anc.path
	-- A path range rather than a prefix expression, so the baseline path
	-- index finds the ancestor's descendants ('0' sorts right after '/').
	JOIN baseline b ON b.path > anc.path || '/' AND b.path < anc.path || '0'
	JOIN remote_state r ON r.item_id = b.item_id AND r.path <> b.path
	WHERE r.path = ra.path || substr(b.path, length(anc.path) + 1)
		AND NOT EXISTS (SELECT 1 FROM local_state ls WHERE ls.path = b.path)
		AND EXISTS (
			SELECT 1 FROM local_state la
			WHERE la.path = anc.path AND COALESCE(la.item_type, '') = 'folder'
		)
),
-- The mirror case: an item OneDrive deleted whose local copy only followed a
-- folder the user renamed. The folder's move reaches OneDrive and re-paths
-- the baseline row, and the next pass deletes the local copy (or, if it was
-- edited, keeps it) like any other. Until then the local copy is not a new
-- file to upload (that undid OneDrive's delete) and the baseline path is not
-- gone from both sides.
followed_local_ancestor_deletes AS MATERIALIZED (
	SELECT
		b.path AS baseline_path,
		l.path AS local_path,
		-- Where the item would sit online under the ancestor: whatever
		-- OneDrive created there waits too, or its writes are rebased into
		-- the moving folder onto the local copy still in the way.
		ra.path || substr(b.path, length(lms.source_path) + 1) AS remote_equivalent
	FROM baseline b
	JOIN local_move_sources lms
		ON lms.candidate_count = 1
		AND b.path > lms.source_path || '/' AND b.path < lms.source_path || '0'
	JOIN baseline anc ON anc.path = lms.source_path AND COALESCE(anc.item_type, '') = 'folder'
	JOIN local_state l
		ON l.path = lms.target_path || substr(b.path, length(lms.source_path) + 1)
		AND COALESCE(l.item_type, '') = COALESCE(b.item_type, '')
	JOIN remote_state ra ON ra.item_id = anc.item_id
	WHERE NOT EXISTS (SELECT 1 FROM remote_state rs WHERE rs.item_id = b.item_id)
		AND NOT EXISTS (SELECT 1 FROM local_state ls WHERE ls.path = b.path)
),
-- The OneDrive paths at or under a followed local ancestor delete's remote
-- equivalent: what OneDrive created there waits for the ancestor's move.
-- Computed once as a set (a path range per row, on the remote path index);
-- asked per path as a correlated prefix test it re-ran the whole CTE for
-- every path.
followed_local_ancestor_remote_paths AS MATERIALIZED (
	SELECT DISTINCT r.path
	FROM followed_local_ancestor_deletes f
	JOIN remote_state r
		ON r.path = f.remote_equivalent
		OR (r.path > f.remote_equivalent || '/' AND r.path < f.remote_equivalent || '0')
),
`
	// sqlComparisonStateCTEs is the one definition of the comparison facts:
	// the decision table's inputs for every path (syncdomain.DecisionState).
	// The table itself is Go (syncdomain.Decide); SQL keeps what is set-valued
	// (the path universe, move pairing, the ancestor-move sets and the
	// descendant probes) and the per-path flags.
	sqlComparisonStateCTEs = sqlLocalAndRemoteMoveCTEs + `
comparison_flags AS (
	SELECT
		p.path,
		COALESCE(b.item_id, '') AS baseline_item_id,
		COALESCE(r.item_id, '') AS remote_item_id,
		-- A side's presence and the path's item type are read from the three
		-- side types: item_type is NOT NULL and one of the enum's names in
		-- every table this query reads (ComparisonRow).
		COALESCE(b.item_type, '') AS baseline_item_type,
		-- Filesystem identity is a pairing hint, never a change signal
		-- (D-057). A folder has no content, so a new inode at its path (rm -rf
		-- and restore, a device renumbered after reboot) changes nothing; a
		-- file with a new inode changed only if its content did, and content
		-- without a hash on both sides is not proof of equality. Either way
		-- metadata_drift republishes the new identity.
		CASE
			WHEN b.path IS NULL OR l.path IS NULL THEN 0
			WHEN COALESCE(b.item_type, '') <> COALESCE(l.item_type, '') THEN 1
			WHEN COALESCE(b.item_type, '') = 'folder' THEN 0
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND NOT ` + sqlSameLocalEntryL + ` THEN
				CASE
					WHEN COALESCE(b.local_hash, '') <> '' AND COALESCE(l.hash, '') <> ''
						AND b.local_hash = l.hash
						AND COALESCE(b.local_size, 0) = COALESCE(l.size, 0) THEN 0
					ELSE 1
				END
			WHEN COALESCE(b.local_hash, '') <> '' OR COALESCE(l.hash, '') <> '' THEN
				CASE WHEN COALESCE(b.local_hash, '') <> COALESCE(l.hash, '') THEN 1 ELSE 0 END
			WHEN COALESCE(b.local_size, 0) <> COALESCE(l.size, 0) THEN 1
			WHEN COALESCE(b.local_mtime, 0) <> COALESCE(l.mtime, 0) THEN 1
			ELSE 0
		END AS local_changed,
		-- Remote content is compared by hash only when both sides carry one.
		-- OneDrive omits the hash for some items (zero-byte files on
		-- Business, some Office-for-web saves), and a hash that is known on
		-- one side only is no evidence of an edit (D-059b): size and eTag
		-- decide, so a lost hash is unknown rather than changed.
		CASE
			WHEN b.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(b.item_type, '') <> COALESCE(r.item_type, '') THEN 1
			WHEN COALESCE(b.item_type, '') = 'folder' THEN 0
			WHEN COALESCE(b.remote_hash, '') <> '' AND COALESCE(r.hash, '') <> '' THEN
				CASE WHEN b.remote_hash <> r.hash THEN 1 ELSE 0 END
			WHEN COALESCE(b.remote_size, 0) <> COALESCE(r.size, 0) THEN 1
			WHEN COALESCE(b.remote_hash, '') = '' AND COALESCE(r.hash, '') = ''
				AND COALESCE(b.remote_mtime, 0) <> COALESCE(r.mtime, 0) THEN 1
			WHEN COALESCE(b.etag, '') <> COALESCE(r.etag, '') THEN 1
			ELSE 0
		END AS remote_changed,
		-- The item at the remote path is not the item the baseline recorded
		-- there: deleted and re-created online, or restored under a new ID.
		CASE
			WHEN b.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(b.item_id, '') <> COALESCE(r.item_id, '') THEN 1
			ELSE 0
		END AS remote_item_replaced,
		-- The entry at the baseline path is not the synced item: renamed away
		-- and a new entry created in its place (D-056).
		CASE
			WHEN b.path IS NULL OR l.path IS NULL THEN 0
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND NOT ` + sqlSameLocalEntryL + ` THEN 1
			ELSE 0
		END AS local_item_replaced,
		-- A folder has no content of its own, so whether it "changed" on a
		-- side is whether anything below it did: a new entry, or one that
		-- differs from its baseline (D-059). Only asked where it decides
		-- something, a folder the other side replaced with a file.
		CASE
			WHEN b.path IS NULL OR COALESCE(b.item_type, '') <> 'folder'
				OR COALESCE(l.item_type, '') <> 'folder'
				OR r.path IS NULL OR COALESCE(r.item_type, '') = 'folder' THEN 0
			WHEN EXISTS (
				SELECT 1 FROM local_state ld
				WHERE ld.path > b.path || '/' AND ld.path < b.path || '0'
					AND NOT EXISTS (
						SELECT 1 FROM baseline bd
						WHERE bd.path = ld.path
							AND COALESCE(bd.item_type, '') = COALESCE(ld.item_type, '')
							AND (COALESCE(ld.item_type, '') = 'folder' OR COALESCE(bd.local_hash, '') = COALESCE(ld.hash, ''))
					)
			) THEN 1
			ELSE 0
		END AS local_subtree_changed,
		-- A folder deleted on OneDrive that the local delete kept because it
		-- holds only content sync ignores (kept_local_folders, D-012). While
		-- it is that same folder and nothing sync sees is inside it, it is
		-- not new local content to create online (R-2.17.2). The kept row
		-- is aliased b inside the subquery so the one identity rule,
		-- sqlSameLocalEntryL, decides "the same folder".
		CASE
			WHEN b.path IS NOT NULL OR r.path IS NOT NULL OR l.path IS NULL
				OR COALESCE(l.item_type, '') <> 'folder' OR COALESCE(l.local_has_identity, 0) = 0 THEN 0
			WHEN NOT EXISTS (
				SELECT 1 FROM kept_local_folders b
				WHERE b.path = l.path AND ` + sqlSameLocalEntryL + `
			) THEN 0
			WHEN EXISTS (
				SELECT 1 FROM local_state ld
				WHERE ld.path > l.path || '/' AND ld.path < l.path || '0'
			) THEN 0
			ELSE 1
		END AS kept_local_folder,
		CASE
			WHEN b.path IS NULL OR COALESCE(b.item_type, '') <> 'folder'
				OR COALESCE(r.item_type, '') <> 'folder'
				OR l.path IS NULL OR COALESCE(l.item_type, '') = 'folder' THEN 0
			WHEN EXISTS (
				SELECT 1 FROM remote_state rd
				WHERE rd.path > b.path || '/' AND rd.path < b.path || '0'
					AND NOT EXISTS (
						SELECT 1 FROM baseline bd
						WHERE bd.path = rd.path
							AND bd.item_id = rd.item_id
							AND (COALESCE(rd.item_type, '') = 'folder' OR COALESCE(bd.remote_hash, '') = COALESCE(rd.hash, ''))
					)
			) THEN 1
			ELSE 0
		END AS remote_subtree_changed,
		CASE
			WHEN l.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(l.item_type, '') <> COALESCE(r.item_type, '') THEN 0
			WHEN COALESCE(l.item_type, '') = 'folder' THEN 1
			-- A zero-byte file has no content to disagree about, and OneDrive
			-- may carry no hash for one (D-059b): two known empty sizes prove
			-- equality where hashes cannot.
			WHEN l.size = 0 AND r.size = 0 THEN 1
			-- Equality must be proven, never assumed. A missing hash on either
			-- side makes matching sizes meaningless: two different files of the
			-- same length would compare equal. Both halves are reachable --
			-- the scanner stores an empty hash when hashing fails, and
			-- driveops.SelectHash returns "" when Graph supplies no hash at all
			-- (B-021) -- and the result was a baseline update recording an
			-- agreement that never existed.
			WHEN COALESCE(l.hash, '') = '' OR COALESCE(r.hash, '') = '' THEN 0
			WHEN COALESCE(l.hash, '') <> COALESCE(r.hash, '') THEN 0
			WHEN COALESCE(l.size, 0) <> COALESCE(r.size, 0) THEN 0
			ELSE 1
		END AS current_equal,
		COALESCE(lms.target_path, '') AS local_move_target,
		COALESCE(lms.candidate_count, lmt.candidate_count, 0) AS local_move_candidate_count,
		COALESCE(lmt.source_path, '') AS local_move_source,
		COALESCE(rms.target_path, '') AS remote_move_target,
		COALESCE(rmt.source_path, '') AS remote_move_source,
		COALESCE(l.item_type, '') AS local_item_type,
		COALESCE(r.item_type, '') AS remote_item_type,
		-- The type-changed flags and the path's item type for the decision
		-- are derived from the three side types (DecisionState).
		CASE WHEN rmsh.path IS NOT NULL OR fad_remote.remote_path IS NOT NULL THEN 1 ELSE 0 END AS remote_move_shadowed,
		CASE WHEN fad_baseline.baseline_path IS NOT NULL OR flad_baseline.baseline_path IS NOT NULL
			THEN 1 ELSE 0 END AS delete_awaits_ancestor_move,
		CASE WHEN flad_local.local_path IS NOT NULL THEN 1 ELSE 0 END AS local_copy_awaits_ancestor_move,
		CASE WHEN flad_remote.path IS NOT NULL THEN 1 ELSE 0 END AS remote_create_awaits_ancestor_move,
		-- File content the change tests call unchanged can still carry new
		-- metadata (touch, tar, rsync, chmod: same bytes, new mtime or ctime; or a new remote
		-- mtime or eTag over the same hash).
		CASE
			WHEN b.path IS NULL OR l.path IS NULL OR r.path IS NULL THEN 0
			-- A folder deleted and re-created online (or restored with a new
			-- ID) at the same path is a new item: the baseline must rebind to
			-- it, or uploads into it target the dead ID forever (R-2.10.49).
			WHEN COALESCE(b.item_type, '') = 'folder' AND COALESCE(r.item_type, '') = 'folder'
				AND COALESCE(b.item_id, '') <> COALESCE(r.item_id, '') THEN 1
			-- A new filesystem identity over unchanged content (D-057): the
			-- baseline must learn it, or the scanner's fast path never trusts
			-- the recorded hash again and a later rename cannot pair. So must
			-- a birth time the baseline was written without.
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND NOT ` + sqlSameLocalEntryL + ` THEN 1
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND b.local_birth_time = 0 AND l.local_birth_time <> 0 THEN 1
			WHEN COALESCE(b.item_type, '') <> 'file' THEN 0
			WHEN COALESCE(b.local_mtime, 0) <> COALESCE(l.mtime, 0)
				OR COALESCE(b.local_ctime, 0) <> COALESCE(l.ctime, 0)
				OR COALESCE(b.local_size, 0) <> COALESCE(l.size, 0)
				OR COALESCE(b.remote_mtime, 0) <> COALESCE(r.mtime, 0)
				OR COALESCE(b.remote_size, 0) <> COALESCE(r.size, 0)
				OR COALESCE(b.etag, '') <> COALESCE(r.etag, '') THEN 1
			ELSE 0
		END AS metadata_drift
	FROM all_paths p
	LEFT JOIN baseline b ON b.path = p.path
	LEFT JOIN local_state l ON l.path = p.path
	LEFT JOIN remote_state r ON r.path = p.path
	LEFT JOIN local_move_sources lms ON lms.source_path = p.path
	LEFT JOIN local_move_targets lmt ON lmt.target_path = p.path
	LEFT JOIN remote_move_sources rms ON rms.source_path = p.path
	LEFT JOIN remote_move_targets rmt ON rmt.target_path = p.path
	LEFT JOIN remote_move_shadowed rmsh ON rmsh.path = p.path
	LEFT JOIN followed_ancestor_deletes fad_remote ON fad_remote.remote_path = p.path
	LEFT JOIN followed_ancestor_deletes fad_baseline ON fad_baseline.baseline_path = p.path
	LEFT JOIN followed_local_ancestor_deletes flad_baseline ON flad_baseline.baseline_path = p.path
	LEFT JOIN followed_local_ancestor_deletes flad_local ON flad_local.local_path = p.path
	LEFT JOIN followed_local_ancestor_remote_paths flad_remote ON flad_remote.path = p.path
)
`

	// sqlQueryPlannerState reads the decision table's inputs of every path,
	// once per plan.
	sqlQueryPlannerState = sqlComparisonStateCTEs + `
SELECT
	path,
	baseline_item_id,
	remote_item_id,
	local_changed,
	remote_changed,
	current_equal,
	local_move_target,
	local_move_source,
	local_move_candidate_count,
	remote_move_target,
	remote_move_source,
	baseline_item_type,
	local_item_type,
	remote_item_type,
	local_item_replaced,
	remote_item_replaced,
	delete_awaits_ancestor_move,
	local_copy_awaits_ancestor_move,
	remote_create_awaits_ancestor_move,
	kept_local_folder,
	remote_move_shadowed,
	local_subtree_changed,
	remote_subtree_changed,
	metadata_drift
FROM comparison_flags
ORDER BY path`
)

// queryComparisonRowsForTables reads every path's comparison row, the
// decision table's inputs, over the given current-state tables in one query,
// in path order. It decides nothing: the planner decides each row
// (syncplan.DecideComparisons).
func queryComparisonRowsForTables(
	ctx context.Context,
	runner sqlTxRunner,
	localStateTable string,
	remoteStateTable string,
) ([]syncdomain.ComparisonRow, error) {
	return queryComparisonRows(ctx, runner, sqlQueryForStateTables(sqlQueryPlannerState, localStateTable, remoteStateTable))
}

// queryComparisonRows runs a query shaped like sqlQueryPlannerState and
// scans its rows. An item type the enum does not know fails the scan,
// naming the path.
func queryComparisonRows(ctx context.Context, runner sqlTxRunner, query string) ([]syncdomain.ComparisonRow, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("syncstore: querying comparison state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var results []syncdomain.ComparisonRow
	for rows.Next() {
		row, scanErr := scanComparisonRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncstore: iterating comparison state rows: %w", err)
	}

	return results, nil
}

func scanComparisonRow(rows *sql.Rows) (syncdomain.ComparisonRow, error) {
	var (
		row                         syncdomain.ComparisonRow
		localChanged, remoteChanged int
		currentEqual                int
		sideTypes                   [3]string
	)
	err := rows.Scan(
		&row.Path,
		&row.BaselineItemID,
		&row.RemoteItemID,
		&localChanged,
		&remoteChanged,
		&currentEqual,
		&row.LocalMoveTarget,
		&row.LocalMoveSource,
		&row.LocalMoveCandidateCount,
		&row.RemoteMoveTarget,
		&row.RemoteMoveSource,
		&sideTypes[0],
		&sideTypes[1],
		&sideTypes[2],
		&row.LocalItemReplaced,
		&row.RemoteItemReplaced,
		&row.DeleteAwaitsAncestorMove,
		&row.LocalCopyAwaitsAncestorMove,
		&row.RemoteCreateAwaitsAncestorMove,
		&row.KeptLocalFolder,
		&row.RemoteMoveShadowed,
		&row.LocalSubtreeChanged,
		&row.RemoteSubtreeChanged,
		&row.MetadataDrift,
	)
	if err != nil {
		return syncdomain.ComparisonRow{}, fmt.Errorf("syncstore: scanning comparison state row: %w", err)
	}
	for i, target := range []*syncdomain.SideType{&row.BaselineType, &row.LocalType, &row.RemoteType} {
		*target, err = syncdomain.ParseSideType(sideTypes[i])
		if err != nil {
			return syncdomain.ComparisonRow{}, fmt.Errorf("syncstore: reading the side types of %q: %w", row.Path, err)
		}
	}
	row.LocalChanged = localChanged != 0
	row.RemoteChanged = remoteChanged != 0
	row.CurrentEqual = currentEqual != 0

	return row, nil
}

func sqlQueryForStateTables(query string, localStateTable string, remoteStateTable string) string {
	return strings.NewReplacer(
		"local_state", localStateTable,
		"remote_state", remoteStateTable,
	).Replace(query)
}
