package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	// sqlLocalAndRemoteMoveCTEs is the shared head of the comparison and
	// reconciliation queries: the path universe plus local and remote move
	// pairing. Both queries must classify moves identically, so the pairing
	// rules live here once.
	//
	// Local move pairing: a baseline row and an unbaselined local_state row
	// pair on filesystem identity (device + inode, same item type). For files
	// identity is necessary but not sufficient, see the corroboration clause.
	// Directories pair on identity alone: a directory has no content of its
	// own, its descendants are paired independently, and a wrong pairing after
	// rmdir + mkdir reuse still converges to the same tree (only the remote
	// folder's item identity differs). Baseline files without identity fall
	// back to unique content-hash pairing.
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
		AND l.local_device = b.local_device
		AND l.local_inode = b.local_inode
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
				AND src.local_device = b.local_device
				AND src.local_inode = b.local_inode
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
local_move_sources AS (
	SELECT
		source_path,
		MIN(target_path) AS target_path,
		COUNT(*) AS candidate_count
	FROM local_move_candidates
	GROUP BY source_path
),
local_move_targets AS (
	SELECT
		target_path,
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
					OR (ls.local_device = b.local_device AND ls.local_inode = b.local_inode)
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
	FROM baseline b
	JOIN remote_state r ON r.item_id = b.item_id AND r.path <> b.path
	JOIN baseline anc ON substr(b.path, 1, length(anc.path) + 1) = anc.path || '/'
	JOIN remote_state ra ON ra.item_id = anc.item_id AND ra.path <> anc.path
	WHERE r.path = ra.path || substr(b.path, length(anc.path) + 1)
		AND NOT EXISTS (SELECT 1 FROM local_state ls WHERE ls.path = b.path)
		AND EXISTS (
			SELECT 1 FROM local_state la
			WHERE la.path = anc.path AND COALESCE(la.item_type, '') = 'folder'
		)
),
`

	// sqlComparisonStateCTEs is the one definition of the comparison facts and
	// the per-path comparison kind. QueryComparisonState and
	// QueryReconciliationState both read it, on the live tables and on the
	// planner-visible scratch tables alike (sqlQueryForStateTables), so a fact
	// such as "local changed" can never mean two things in two queries.
	sqlComparisonStateCTEs = sqlLocalAndRemoteMoveCTEs + `
comparison_flags AS (
	SELECT
		p.path,
		COALESCE(b.item_id, '') AS baseline_item_id,
		COALESCE(r.item_id, '') AS remote_item_id,
		COALESCE(b.item_type, l.item_type, r.item_type, '') AS item_type,
		CASE WHEN b.path IS NOT NULL THEN 1 ELSE 0 END AS baseline_present,
		CASE WHEN l.path IS NOT NULL THEN 1 ELSE 0 END AS local_present,
		CASE WHEN r.path IS NOT NULL THEN 1 ELSE 0 END AS remote_present,
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
				AND (
					COALESCE(b.local_device, 0) <> COALESCE(l.local_device, 0)
					OR COALESCE(b.local_inode, 0) <> COALESCE(l.local_inode, 0)
				) THEN
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
				AND (
					COALESCE(b.local_device, 0) <> COALESCE(l.local_device, 0)
					OR COALESCE(b.local_inode, 0) <> COALESCE(l.local_inode, 0)
				) THEN 1
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
		CASE
			WHEN b.path IS NOT NULL AND l.path IS NOT NULL
				AND COALESCE(b.item_type, '') <> COALESCE(l.item_type, '') THEN 1
			ELSE 0
		END AS local_type_changed,
		CASE
			WHEN b.path IS NOT NULL AND r.path IS NOT NULL
				AND COALESCE(b.item_type, '') <> COALESCE(r.item_type, '') THEN 1
			ELSE 0
		END AS remote_type_changed,
		CASE WHEN rmsh.path IS NOT NULL OR fad_remote.remote_path IS NOT NULL THEN 1 ELSE 0 END AS remote_move_shadowed,
		CASE WHEN fad_baseline.baseline_path IS NOT NULL THEN 1 ELSE 0 END AS local_delete_awaits_ancestor_move,
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
			-- the recorded hash again and a later rename cannot pair.
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND (
					COALESCE(b.local_device, 0) <> COALESCE(l.local_device, 0)
					OR COALESCE(b.local_inode, 0) <> COALESCE(l.local_inode, 0)
				) THEN 1
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
),
comparison_state AS (
	SELECT
		path,
		baseline_item_id,
		remote_item_id,
		item_type,
		baseline_present,
		local_present,
		remote_present,
		local_changed,
		remote_changed,
		current_equal,
		local_move_target,
		local_move_candidate_count,
		local_move_source,
		remote_move_target,
		remote_move_source,
		local_item_type,
		remote_item_type,
		local_type_changed,
		remote_type_changed,
		remote_move_shadowed,
		local_delete_awaits_ancestor_move,
		remote_item_replaced,
		local_item_replaced,
		local_subtree_changed,
		remote_subtree_changed,
		metadata_drift,
		CASE
			-- The synced item moved; a new entry at its old path (rename and
			-- recreate) is planned as a create by move composition.
			WHEN baseline_present = 1 AND local_move_candidate_count = 1 AND local_move_target <> ''
				AND (local_present = 0 OR local_item_replaced = 1) THEN 'local_move_source'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 0
				AND local_delete_awaits_ancestor_move = 1 THEN 'local_delete_awaits_ancestor_move'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 0 THEN 'both_missing'
			WHEN local_present = 1 AND baseline_present = 0 AND local_move_source <> '' AND local_move_candidate_count = 1 THEN 'local_move_dest'
			WHEN baseline_present = 1 AND remote_move_target <> '' THEN 'remote_move_source'
			WHEN remote_present = 1 AND baseline_present = 0 AND remote_move_source <> '' THEN 'remote_move_dest'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 0 THEN 'local_only_create'
			WHEN baseline_present = 0 AND local_present = 0 AND remote_present = 1 AND remote_move_shadowed = 1 THEN 'remote_move_shadowed'
			WHEN baseline_present = 0 AND local_present = 0 AND remote_present = 1 THEN 'remote_only_create'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 1 AND current_equal = 1 THEN 'create_equal'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 1 THEN 'create_conflict'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 1 AND local_changed = 0 AND remote_changed = 0 THEN 'unchanged'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 1 AND current_equal = 1 AND remote_changed = 1 THEN 'equal_again'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 1 THEN 'local_missing'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 0 THEN 'remote_missing'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 1 THEN 'diverged'
			ELSE 'unknown'
		END AS comparison_kind
	FROM comparison_flags
)
`

	sqlQueryComparisonState = sqlComparisonStateCTEs + `
SELECT
	path,
	baseline_item_id,
	remote_item_id,
	item_type,
	baseline_present,
	local_present,
	remote_present,
	local_changed,
	remote_changed,
	current_equal,
	local_move_target,
	local_move_source,
	local_move_candidate_count,
	remote_move_target,
	remote_move_source,
	comparison_kind
FROM comparison_state
ORDER BY path`

	sqlQueryReconciliationState = sqlComparisonStateCTEs + `,
reconciliation_state AS (
	SELECT
		path,
		item_type,
		comparison_kind,
		local_move_target,
		local_move_source,
		remote_move_target,
		remote_move_source,
		CASE
			-- A type change (file <-> folder) replaces the item: the old one is
			-- deleted and the new one created, never "uploaded" or
			-- "downloaded" as the baseline's type. When the other side also
			-- changed, local wins: the replaced OneDrive item goes to the
			-- recycle bin, whereas replacing local truth would destroy it.
			-- Different types with changes on both sides (a local edit of
			-- the file OneDrive replaced with a folder, a remote edit inside
			-- the folder the user replaced with a file, ...) keep both: the
			-- local entry becomes a conflict copy and OneDrive's item takes
			-- the path. Replacing either side would delete the other's
			-- content (D-059).
			-- An entry re-created at the path (a new filesystem identity)
			-- counts as a local change here: replacing it would need a local
			-- delete, which refuses an entry whose identity changed
			-- (R-2.8.10) and was superseded on every pass.
			WHEN comparison_kind = 'diverged' AND local_item_type <> remote_item_type
				AND (local_changed = 1 OR local_subtree_changed = 1 OR local_item_replaced = 1)
				AND (remote_changed = 1 OR remote_subtree_changed = 1) THEN 'conflict_keep_both'
			-- Both sides replaced the item with the same new type: two
			-- creates at one path, not a type change one side wins. Planned
			-- as a local win it deleted the OneDrive item, a delete that
			-- refuses while the path exists locally, and was superseded on
			-- every pass.
			WHEN comparison_kind = 'diverged' AND local_type_changed = 1 AND remote_type_changed = 1
				AND local_item_type = remote_item_type AND current_equal = 1 THEN 'baseline_update'
			WHEN comparison_kind = 'diverged' AND local_type_changed = 1 AND remote_type_changed = 1
				AND local_item_type = remote_item_type THEN 'conflict_create_create'
			WHEN comparison_kind = 'diverged' AND local_changed = 1
				AND (local_type_changed = 1 OR remote_type_changed = 1) THEN 'replace_remote'
			WHEN comparison_kind = 'diverged' AND remote_type_changed = 1 THEN 'replace_local'
			WHEN comparison_kind = 'remote_missing' AND local_type_changed = 1 AND local_item_type = 'folder' THEN 'folder_create_remote'
			-- A folder deleted locally while OneDrive replaced it (a new item
			-- at the same path, or a new type) is not the folder the user
			-- deleted: re-create it locally. Planned as a delete it would
			-- remove the new item and its content online.
			WHEN comparison_kind = 'local_missing' AND remote_item_type = 'folder'
				AND (remote_type_changed = 1 OR remote_item_replaced = 1) THEN 'folder_create_local'
			WHEN comparison_kind = 'both_missing' THEN 'baseline_remove'
			WHEN comparison_kind = 'local_move_source' THEN 'local_move'
			WHEN comparison_kind = 'local_move_dest' THEN 'local_move'
			WHEN comparison_kind = 'remote_move_source' THEN 'remote_move'
			WHEN comparison_kind = 'remote_move_dest' THEN 'remote_move'
			WHEN comparison_kind = 'local_only_create' AND item_type = 'folder' THEN 'folder_create_remote'
			WHEN comparison_kind = 'local_only_create' THEN 'upload'
			-- The local side moved this item too; its local move wins and
			-- carries the item, so the remote copy is not a new download.
			WHEN comparison_kind = 'remote_move_shadowed' THEN 'noop'
			-- Planned as a remote delete next pass, once the ancestor's local
			-- move has re-pathed the baseline (followed_ancestor_deletes).
			WHEN comparison_kind = 'local_delete_awaits_ancestor_move' THEN 'noop'
			WHEN comparison_kind = 'remote_only_create' AND item_type = 'folder' THEN 'folder_create_local'
			WHEN comparison_kind = 'remote_only_create' THEN 'download'
			WHEN comparison_kind = 'create_equal' THEN 'baseline_update'
			WHEN comparison_kind = 'create_conflict' THEN 'conflict_create_create'
			-- Nothing transfers, but the baseline must learn the new metadata:
			-- the scanner's size+mtime fast path compares against it, so a
			-- stale mtime would re-hash the file on every scan (R-2.2.3).
			WHEN comparison_kind = 'unchanged' AND metadata_drift = 1 THEN 'baseline_update'
			WHEN comparison_kind = 'unchanged' THEN 'noop'
			WHEN comparison_kind = 'equal_again' THEN 'baseline_update'
			-- A folder the user deleted (or moved without a provable identity
			-- pairing) is deleted on OneDrive exactly like a file. Remote-only
			-- descendants that still need the folder are protected by action
			-- normalization, which rewrites this delete into a local re-create.
			WHEN comparison_kind = 'local_missing' AND remote_changed = 0 AND remote_item_replaced = 0 THEN 'remote_delete'
			WHEN comparison_kind = 'local_missing' THEN 'download'
			WHEN comparison_kind = 'remote_missing' AND local_changed = 0 THEN 'local_delete'
			WHEN comparison_kind = 'remote_missing' AND local_changed = 1 THEN 'conflict_edit_delete'
			WHEN comparison_kind = 'diverged' AND local_changed = 1 AND remote_changed = 0 THEN 'upload'
			WHEN comparison_kind = 'diverged' AND local_changed = 0 AND remote_changed = 1 THEN 'download'
			WHEN comparison_kind = 'diverged' AND local_changed = 1 AND remote_changed = 1 AND current_equal = 1 THEN 'baseline_update'
			WHEN comparison_kind = 'diverged' AND local_changed = 1 AND remote_changed = 1 THEN 'conflict_edit_edit'
			ELSE 'noop'
		END AS reconciliation_kind
	FROM comparison_state
)
SELECT
	path,
	item_type,
	comparison_kind,
	reconciliation_kind,
	local_move_target,
	local_move_source,
	remote_move_target,
	remote_move_source
FROM reconciliation_state
ORDER BY path`
)

type sQLiteComparisonRow struct {
	Path                    string
	BaselineItemID          string
	RemoteItemID            string
	ItemType                ItemType
	BaselinePresent         bool
	LocalPresent            bool
	RemotePresent           bool
	LocalChanged            bool
	RemoteChanged           bool
	CurrentEqual            bool
	LocalMoveTarget         string
	LocalMoveSource         string
	LocalMoveCandidateCount int
	RemoteMoveTarget        string
	RemoteMoveSource        string
	ComparisonKind          string
}

type sQLiteReconciliationRow struct {
	Path               string
	ItemType           ItemType
	ComparisonKind     string
	ReconciliationKind string
	LocalMoveTarget    string
	LocalMoveSource    string
	RemoteMoveTarget   string
	RemoteMoveSource   string
}

func (s *SyncStore) QueryComparisonState(ctx context.Context) ([]sQLiteComparisonRow, error) {
	return queryComparisonStateWithRunner(ctx, s.db)
}

func queryComparisonStateWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
) ([]sQLiteComparisonRow, error) {
	return queryComparisonStateWithRunnerForTables(ctx, runner, "local_state", "remote_state")
}

func queryComparisonStateWithRunnerForTables(
	ctx context.Context,
	runner sqlTxRunner,
	localStateTable string,
	remoteStateTable string,
) ([]sQLiteComparisonRow, error) {
	rows, err := runner.QueryContext(ctx, sqlQueryForStateTables(sqlQueryComparisonState, localStateTable, remoteStateTable))
	if err != nil {
		return nil, fmt.Errorf("sync: querying comparison state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var results []sQLiteComparisonRow
	for rows.Next() {
		var (
			row                                        sQLiteComparisonRow
			itemType                                   sql.NullString
			baselinePresent, localPresent              int
			remotePresent, localChanged, remoteChanged int
			currentEqual                               int
		)
		if err := rows.Scan(
			&row.Path,
			&row.BaselineItemID,
			&row.RemoteItemID,
			&itemType,
			&baselinePresent,
			&localPresent,
			&remotePresent,
			&localChanged,
			&remoteChanged,
			&currentEqual,
			&row.LocalMoveTarget,
			&row.LocalMoveSource,
			&row.LocalMoveCandidateCount,
			&row.RemoteMoveTarget,
			&row.RemoteMoveSource,
			&row.ComparisonKind,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning comparison state row: %w", err)
		}
		if itemType.Valid {
			parsed, err := parseItemType(itemType.String)
			if err != nil {
				return nil, fmt.Errorf("sync: parsing comparison state item type %q: %w", itemType.String, err)
			}
			row.ItemType = parsed
		}
		row.BaselinePresent = baselinePresent != 0
		row.LocalPresent = localPresent != 0
		row.RemotePresent = remotePresent != 0
		row.LocalChanged = localChanged != 0
		row.RemoteChanged = remoteChanged != 0
		row.CurrentEqual = currentEqual != 0
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating comparison state rows: %w", err)
	}

	return results, nil
}

func (s *SyncStore) QueryReconciliationState(ctx context.Context) ([]sQLiteReconciliationRow, error) {
	return queryReconciliationStateWithRunner(ctx, s.db)
}

func queryReconciliationStateWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
) ([]sQLiteReconciliationRow, error) {
	return queryReconciliationStateWithRunnerForTables(ctx, runner, "local_state", "remote_state")
}

func queryReconciliationStateWithRunnerForTables(
	ctx context.Context,
	runner sqlTxRunner,
	localStateTable string,
	remoteStateTable string,
) ([]sQLiteReconciliationRow, error) {
	rows, err := runner.QueryContext(ctx, sqlQueryForStateTables(sqlQueryReconciliationState, localStateTable, remoteStateTable))
	if err != nil {
		return nil, fmt.Errorf("sync: querying reconciliation state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var results []sQLiteReconciliationRow
	for rows.Next() {
		var (
			row      sQLiteReconciliationRow
			itemType sql.NullString
		)
		if err := rows.Scan(
			&row.Path,
			&itemType,
			&row.ComparisonKind,
			&row.ReconciliationKind,
			&row.LocalMoveTarget,
			&row.LocalMoveSource,
			&row.RemoteMoveTarget,
			&row.RemoteMoveSource,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning reconciliation state row: %w", err)
		}
		if itemType.Valid {
			parsed, err := parseItemType(itemType.String)
			if err != nil {
				return nil, fmt.Errorf("sync: parsing reconciliation state item type %q: %w", itemType.String, err)
			}
			row.ItemType = parsed
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating reconciliation state rows: %w", err)
	}

	return results, nil
}

func sqlQueryForStateTables(query string, localStateTable string, remoteStateTable string) string {
	return strings.NewReplacer(
		"local_state", localStateTable,
		"remote_state", remoteStateTable,
	).Replace(query)
}
