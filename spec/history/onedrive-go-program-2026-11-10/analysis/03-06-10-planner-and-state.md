# Planner and state: fail-closed planner, one owner for state, identity-keyed reconciliation

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis, 2026-11-10.

**Nothing was built, run or measured.** Every number about speed is quoted from the repository's own documents. Cross-product sizes are arithmetic on the columns read.

Path abbreviations used in citations:

- `S/` = `~/src/onedrive-go/internal/sync/`
- `M/` = `~/src/onedrive-go/internal/multisync/`
- `D/` = `~/src/onedrive-go/spec/design/`
- `R/` = `~/src/onedrive-go/spec/reference/`
- `G/` = `~/src/onedrive-go/` (repository root)

Marks: **VERIFIED** means I read the code or document that proves the statement. **SUSPECTED** means I did not, and the sentence says what would settle it. Section "Not read" at the end lists what I did not get to.

---

## 0. Findings in short

1. **`ELSE 'noop'` is unreachable today, and that is the problem.** Every reachable input already has an explicit `WHEN`; the four genuine no-ops are explicit (`S/sqlite_compare.go:615, 616, 620, 629`). `ELSE 'noop'` (`:647`) is reached only by the comparison kind `'unknown'` (`:532`, itself unreachable), by a future comparison kind added without a `WHEN`, or by a flag outside {0, 1}. Each of those is a bug, and each would be planned as "nothing to do". VERIFIED.
2. **Two `WHEN` branches are dead**: `S/sqlite_compare.go:592–593` and `:645`. VERIFIED by reading the conditions (derivation in A1.4).
3. **Kinds are unvalidated strings in Go.** They are scanned into `string` fields (`S/sqlite_compare.go:680, 686, 687, 742, 800–801`). The Go `default:` does return an error (`S/planner_actions.go:436–437`), but only for a string Go does not know, and only after an early return that skips the switch (`:381–383`). Move composition compares three kind literals inline (`S/planner_move_composition.go:1174, 1180, 1182`). VERIFIED.
4. **`foreign_keys(ON)` is already set, and the schema declares no foreign key.** DSN at `S/store.go:109–114` (pragma at `:111`), one connection (`:122`); no `REFERENCES` anywhere in `S/schema.go:77–253`. VERIFIED.
5. **Exactly one reference is a real foreign-key candidate**: a blocked `retry_work` row to its `block_scopes` row. Today it is enforced by a test-only panic (`S/engine_runtime_invariants.go:235–237`) and repaired at startup (`S/store_retry_work.go:122–139`). The child row is committed before the parent (`S/engine_runtime_completion.go:324` before `:331/:334`), the parent is written with `INSERT OR REPLACE` (`S/store_write_block_scopes.go:59–62`), `DiscardScope` deletes the parent before the children (`S/store_scope_admin.go:77–78` before `:83`), and the parent's primary key lacks `NOT NULL` (`S/schema.go:226`). VERIFIED.
6. **The baseline cache has one owner already (the store), but three ways to diverge**: a cache step that can fail after the transaction committed (`S/store_write_baseline.go:441, 446–447`; `S/store_write_observation.go:70, 75–76`), shared `*BaselineEntry` pointers protected only by a comment (`S/core_types.go:132–133`), and a consistency check with no production caller that ignores four fields (`S/store_write_baseline.go:948–1052`). VERIFIED.
7. **The decision table as one Go function has 8,388,608 input states** after normalizing to three four-valued side types and 17 booleans (402,653,184 in the raw SQL columns; 131,072 and 417,792 as two raw stages). Derivation in B4.
8. **Planner benchmarks exist at 10,000 and 100,000 items** (`S/sqlite_compare_bench_test.go:36–56`), with results recorded in `D/performance-benchmarking.md:653–753`. VERIFIED.
9. **The engine model test drives only `RunOnce`**, on a wall clock plus an offset (`S/engine_model_based_test.go:469, 557–574`) and a real temporary directory (`:302`). Watch mode, timers and observers are outside it. VERIFIED.
10. **No recorded live incident traces to path keying of the reconciliation.** The evidence for candidate 1 is the candidates document itself and commit history. VERIFIED by search of both incident documents (C6.4).

---

# PART A — Fail-closed planner

## A1. Kinds: explicit no-op, error on anything else

### A1.1 The reconciliation `CASE`, quoted in full

`S/sqlite_compare.go:559–661`. VERIFIED (read verbatim).

```sql
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
			-- A new filesystem identity alone is not a local change
			-- (D-057): a rebuilt or renumbered folder whose subtree is
			-- unchanged gives way like any other.
			WHEN comparison_kind = 'diverged' AND local_item_type <> remote_item_type
				AND (local_changed = 1 OR local_subtree_changed = 1)
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
			WHEN comparison_kind = 'kept_local_folder' THEN 'noop'
			-- Planned as a remote delete next pass, once the ancestor's local
			-- move has re-pathed the baseline (followed_ancestor_deletes,
			-- followed_local_ancestor_deletes).
			WHEN comparison_kind = 'delete_awaits_ancestor_move' THEN 'noop'
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
			-- A folder with a new filesystem identity but nothing changed
			-- below it (rebuilt, restored from a copy, or its disk
			-- renumbered) accepts OneDrive's delete too (D-057). Anything new
			-- or changed below it is kept by the descendants' own rows.
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
```

Line map: `CASE` opens at 569; `WHEN` lines 584–586, 592–593, 594–595, 596–597, 598, 599, 604–605, 606, 607, 608, 609, 610, 611, 612, 615, 616, 620, 621, 622, 623, 624, 628, 629, 630, 635, 636, 641, 642, 643, 644, 645, 646; `ELSE 'noop'` 647; `END` 648.

### A1.2 The comparison `CASE` that feeds it, quoted in full

`S/sqlite_compare.go:505–533`. VERIFIED (read verbatim).

```sql
		CASE
			-- The synced item moved; a new entry at its old path (rename and
			-- recreate) is planned as a create by move composition.
			WHEN baseline_present = 1 AND local_move_candidate_count = 1 AND local_move_target <> ''
				AND (local_present = 0 OR local_item_replaced = 1) THEN 'local_move_source'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 0
				AND delete_awaits_ancestor_move = 1 THEN 'delete_awaits_ancestor_move'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 0 THEN 'both_missing'
			WHEN local_present = 1 AND baseline_present = 0 AND local_move_source <> '' AND local_move_candidate_count = 1 THEN 'local_move_dest'
			WHEN baseline_present = 1 AND remote_move_target <> '' THEN 'remote_move_source'
			WHEN remote_present = 1 AND baseline_present = 0 AND remote_move_source <> '' THEN 'remote_move_dest'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 0
				AND local_copy_awaits_ancestor_move = 1 THEN 'delete_awaits_ancestor_move'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 0
				AND kept_local_folder = 1 THEN 'kept_local_folder'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 0 THEN 'local_only_create'
			WHEN baseline_present = 0 AND local_present = 0 AND remote_present = 1 AND remote_move_shadowed = 1 THEN 'remote_move_shadowed'
			WHEN baseline_present = 0 AND local_present = 0 AND remote_present = 1
				AND remote_create_awaits_ancestor_move = 1 THEN 'delete_awaits_ancestor_move'
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
```

Line map: `WHEN` lines 508–509, 510–511, 512, 513, 514, 515, 516–517, 518–519, 520, 521, 522–523, 524, 525, 526, 527, 528, 529, 530, 531; `ELSE 'unknown'` 532.

### A1.3 Input dimensions and kinds

**Comparison `CASE` — 17 two-valued inputs** (VERIFIED, from the conditions above and the flag definitions at `S/sqlite_compare.go:270–473`):

| # | Input as tested | Defined at |
|---|---|---|
| 1–3 | `baseline_present`, `local_present`, `remote_present` | `:276–278` |
| 4 | `local_move_candidate_count = 1` | `:411` |
| 5 | `local_move_target <> ''` | `:410` |
| 6 | `local_move_source <> ''` | `:412` |
| 7 | `remote_move_target <> ''` | `:413` |
| 8 | `remote_move_source <> ''` | `:414` |
| 9 | `local_item_replaced` | `:329–334` |
| 10 | `delete_awaits_ancestor_move` | `:428–429` |
| 11 | `local_copy_awaits_ancestor_move` | `:430` |
| 12 | `remote_create_awaits_ancestor_move` | `:431` |
| 13 | `kept_local_folder` | `:361–373` |
| 14 | `remote_move_shadowed` | `:427` |
| 15 | `current_equal` | `:390–409` |
| 16 | `local_changed` | `:285–302` |
| 17 | `remote_changed` | `:308–319` |

Output: 17 kinds plus the fallback — `local_move_source`, `delete_awaits_ancestor_move`, `both_missing`, `local_move_dest`, `remote_move_source`, `remote_move_dest`, `kept_local_folder`, `local_only_create`, `remote_move_shadowed`, `remote_only_create`, `create_equal`, `create_conflict`, `unchanged`, `equal_again`, `local_missing`, `remote_missing`, `diverged`; fallback `unknown`.

**Reconciliation `CASE` — inputs** (VERIFIED):

- `comparison_kind`: 18 values (17 plus `unknown`).
- `item_type` (tested `= 'folder'` at `:611, :621`), defined as `COALESCE(b.item_type, l.item_type, r.item_type, '')` (`:275`); values `file`, `folder`, `root` (CHECK at `S/schema.go:85, 175, 186`).
- `local_item_type` (`:584, :593, :595, :599`) and `remote_item_type` (`:584, :593, :595, :604`), each `COALESCE(x.item_type, '')` (`:415–416`): four values each.
- Nine booleans: `local_changed`, `remote_changed`, `current_equal`, `local_subtree_changed` (`:339–354`), `remote_subtree_changed` (`:374–389`), `local_type_changed` (`:417–421`), `remote_type_changed` (`:422–426`), `remote_item_replaced` (`:322–326`), `metadata_drift` (`:435–458`).

Output: 17 kinds — `noop`, `baseline_remove`, `baseline_update`, `folder_create_local`, `folder_create_remote`, `upload`, `download`, `local_delete`, `remote_delete`, `local_move`, `remote_move`, `conflict_edit_edit`, `conflict_edit_delete`, `conflict_create_create`, `conflict_keep_both`, `replace_remote`, `replace_local`.

### A1.4 What reaches each `ELSE` today

**`ELSE 'unknown'` (`:532`) — reached only when all three sides are absent.** VERIFIED by reading. Each other presence combination has an unconditional `WHEN`:

| baseline | local | remote | Unconditional `WHEN` | Kind |
|---|---|---|---|---|
| 0 | 0 | 0 | none | `ELSE 'unknown'` |
| 1 | 0 | 0 | `:512` | `both_missing` |
| 0 | 1 | 0 | `:520` | `local_only_create` |
| 0 | 0 | 1 | `:524` | `remote_only_create` |
| 0 | 1 | 1 | `:526` | `create_conflict` |
| 1 | 0 | 1 | `:529` | `local_missing` |
| 1 | 1 | 0 | `:530` | `remote_missing` |
| 1 | 1 | 1 | `:531` | `diverged` |

The conditional branches that do not test all three presence bits (`:508–509, :513, :514, :515`) each require at least one side present. (0, 0, 0) cannot occur: the row set is `all_paths`, the `UNION` of the three tables' paths (`:53–59`), left-joined to the same three tables (`:460–462`), and `sqlQueryForStateTables` rewrites the table names over the whole query text at once (`:825–830`). VERIFIED.

**`ELSE 'noop'` (`:647`) — reached by no reachable input.** VERIFIED by reading. Every comparison kind except `diverged` and `unknown` has an unconditional `WHEN`:

| Comparison kind | Line that catches every remaining row | Result |
|---|---|---|
| `both_missing` | `:606` | `baseline_remove` |
| `local_move_source`, `local_move_dest` | `:607, :608` | `local_move` |
| `remote_move_source`, `remote_move_dest` | `:609, :610` | `remote_move` |
| `local_only_create` | `:612` (after `:611`) | `upload` or `folder_create_remote` |
| `remote_move_shadowed` | `:615` | `noop` |
| `kept_local_folder` | `:616` | `noop` |
| `delete_awaits_ancestor_move` | `:620` | `noop` |
| `remote_only_create` | `:622` (after `:621`) | `download` or `folder_create_local` |
| `create_equal` | `:623` | `baseline_update` |
| `create_conflict` | `:624` | `conflict_create_create` |
| `unchanged` | `:629` (after `:628`) | `noop` or `baseline_update` |
| `equal_again` | `:630` | `baseline_update` |
| `local_missing` | `:636` (after `:604–605, :635`) | `download`, `remote_delete` or `folder_create_local` |
| `remote_missing` | `:641` and `:642` together (`local_changed` is 0 or 1; its `CASE` ends `ELSE 0` at `:301`) | `local_delete` or `conflict_edit_delete` |
| `diverged` | `:643, :644, :646` together, see below | |
| `unknown` | none | `ELSE 'noop'` |

`diverged` is produced only when all three sides are present and neither `:527` (`local_changed = 0 AND remote_changed = 0`) nor `:528` (`current_equal = 1 AND remote_changed = 1`) matched. So a `diverged` row has (`local_changed`, `remote_changed`) in {(1, 0), (0, 1) with `current_equal = 0`, (1, 1) with `current_equal = 0`}. Lines `:643`, `:644` and `:646` cover exactly those.

So the fallback is reached only by:

1. comparison kind `unknown`, which is unreachable today;
2. a comparison kind added to `:505–533` later without a reconciliation `WHEN`;
3. a flag value outside {0, 1} (for example a `NULL` from a future flag expression).

All three are defects, and all three are planned as "no action". The genuine no-ops are already explicit (`:615, :616, :620, :629`). The chain `'unknown'` → `ELSE 'noop'` → Go `case "noop": return nil, nil` (`S/planner_actions.go:386`) fails open twice.

**Dead branches.** VERIFIED by reading the conditions.

- `:645` needs a `diverged` row with `current_equal = 1 AND remote_changed = 1`; `:528` classifies every such row as `equal_again`. Dead.
- `:592–593` needs a `diverged` row with `remote_type_changed = 1 AND current_equal = 1`. `remote_type_changed = 1` means both rows are present with different types (`:422–426`), the same condition that sets `remote_changed = 1` (`:310`); with `current_equal = 1` the row is `equal_again` (`:528`), which maps to `baseline_update` at `:630` — the same result. Dead; removal is behaviour-neutral.

**SUSPECTED, worth one test:** a `diverged` row with `local_changed = 1, remote_changed = 0, current_equal = 1` maps to `upload` (`:643`), an upload of content that already equals OneDrive's. It would arise when `baseline.local_hash` differs from `baseline.remote_hash`. Settle with a `sqlite_compare` test seeding such a baseline row; whether it is intended (to refresh the baseline) is not stated near `:643`.

### A1.5 Where kind strings appear

**SQL** (VERIFIED): comparison kinds as `THEN '<kind>'` at `S/sqlite_compare.go:508–532`; as `comparison_kind = '<kind>'` at `:584–646`; reconciliation kinds as `THEN '<kind>'` at `:586–646` and `ELSE` at `:647`. No other SQL in `S/` names a kind (`S/store_planner_visible.go` has no `CASE`/`ELSE`; VERIFIED by reading the file).

**Go production code — complete list** (VERIFIED by search over `internal/` and `cmd/`):

| Site | What |
|---|---|
| `S/sqlite_compare.go:680` | `sQLiteComparisonRow.ComparisonKind string` |
| `S/sqlite_compare.go:686, 687` | `sQLiteReconciliationRow.ComparisonKind string`, `.ReconciliationKind string` |
| `S/sqlite_compare.go:742` | comparison kind scanned straight into the string field |
| `S/sqlite_compare.go:800–801` | both kinds scanned straight into string fields |
| `S/planner_actions.go:67` | kind logged |
| `S/planner_actions.go:385` | `switch rec.ReconciliationKind` |
| `S/planner_actions.go:386, 388, 390, 392, 394, 399, 410, 415, 419, 427, 429, 431` | twelve bare literals: `"noop"`, `"baseline_remove"`, `"folder_create_local"`, `"folder_create_remote"`, `"upload"`, `"download"`, `"conflict_edit_edit"`, `"conflict_edit_delete"`, `"conflict_create_create"`, `"conflict_keep_both"`, `"replace_remote"`, `"replace_local"` |
| `S/planner_actions.go:404, 406, 408, 433` | five kinds spelled with **action-type wire constants**: `strLocalDelete`, `strRemoteDelete`, `strBaselineUpdate`, `strLocalMove`, `strRemoteMove` (`S/enums.go:23–26, 30`) |
| `S/planner_actions.go:436–437` | `default:` → `fmt.Errorf("sync: unsupported reconciliation kind %q for %s", ...)` |
| `S/planner_move_composition.go:1072` | `const comparisonKindLocalMoveSource = "local_move_source"` — the only named comparison-kind constant |
| `S/planner_move_composition.go:1061, 1128` | comparisons against that constant |
| `S/planner_move_composition.go:1174, 1180, 1182` | bare literals `"remote_move_shadowed"`, `"remote_move_source"`, `"local_move_dest"` in a tagless `switch` |

The 17 `case` labels at `S/planner_actions.go:386–435` equal the 17 reconciliation kinds the SQL emits. VERIFIED by comparing the two lists.

**Tests** (VERIFIED by search): `S/sqlite_compare_test.go` helpers `comparisonKindsByPath` (`:10`) and `reconciliationKindsByPath` (`:19`) and literal assertions at `:40, 44, 85, 126, 164, 168, 199, 205, 245, 310, 384, 436, 466, 518, 530, 594, 662`; `S/engine_current_plan_test.go:101–102`; `S/planner_actions_test.go:1066, 1109`. None in `internal/cli`, `cmd` or `internal/syncverify`.

**What the Go side does with a bad kind today** (VERIFIED):

- A string SQL emits that Go lacks → error from `S/planner_actions.go:436–437`, wrapped as `sync: planning actions: ...` (`S/engine_current_plan.go:605–607`).
- But `buildActionsForReconciliation` returns `nil, nil` before the switch when `plannerSuppressesUnavailableTruth(&view.TruthStatus)` holds (`S/planner_actions.go:381–383`; `S/planner_truth_overlay.go`). A row with an unknown kind under unavailable truth is silently dropped.
- An unknown **comparison** kind is never checked: it flows into string compares that simply do not match (`S/planner_move_composition.go:1061, 1128, 1174–1185`).
- A file-only kind on a folder is logged and dropped: `errFolderTransferPlanned` → `p.logger.Error("planner dropped an impossible folder transfer", ...)` → `continue` (`S/planner_actions.go:61–71`; documented as "a comparison bug (logged, row dropped)" at `D/sync-planning.md:362–364`).

### A1.6 Design

**Precedent to copy.** `ItemType` and `actionType` are typed enums with `String()`, `parse*` that errors on an unknown string, `sql.Scanner` and `driver.Valuer` (`S/enums.go:93–151, 176–291`). The row scans read `item_type` into a string and call `parseItemType`, wrapping the error with the row's path (`S/sqlite_compare.go:746–752, 809–815`). VERIFIED.

**One Go enum per kind, as the single source of truth.** New file `S/planner_kinds.go`; add it to the `GOVERNS:` line of `D/sync-planning.md:3` (each production file is named by exactly one design document, `G/CLAUDE.md:45–50`).

```go
type comparisonKind uint8

const (
	comparisonUnclassified comparisonKind = iota // zero value; never a valid decision
	comparisonLocalMoveSource
	comparisonDeleteAwaitsAncestorMove
	comparisonBothMissing
	comparisonLocalMoveDest
	comparisonRemoteMoveSource
	comparisonRemoteMoveDest
	comparisonKeptLocalFolder
	comparisonLocalOnlyCreate
	comparisonRemoteMoveShadowed
	comparisonRemoteOnlyCreate
	comparisonCreateEqual
	comparisonCreateConflict
	comparisonUnchanged
	comparisonEqualAgain
	comparisonLocalMissing
	comparisonRemoteMissing
	comparisonDiverged
)

type reconciliationKind uint8

const (
	reconcileUnclassified reconciliationKind = iota // zero value; never a valid decision
	reconcileNoop
	reconcileBaselineRemove
	reconcileBaselineUpdate
	reconcileFolderCreateLocal
	reconcileFolderCreateRemote
	reconcileUpload
	reconcileDownload
	reconcileLocalDelete
	reconcileRemoteDelete
	reconcileLocalMove
	reconcileRemoteMove
	reconcileConflictEditEdit
	reconcileConflictEditDelete
	reconcileConflictCreateCreate
	reconcileConflictKeepBoth
	reconcileReplaceRemote
	reconcileReplaceLocal
)

func (k comparisonKind) String() string                       // exhaustive switch
func parseComparisonKind(s string) (comparisonKind, error)    // error on any unknown string
func allComparisonKinds() []comparisonKind                    // for tests
// the same three for reconciliationKind
```

Constraints that shape it (VERIFIED):

- `gochecknoglobals` is enabled (`G/.golangci.yml:43`), so the name table is a `switch` inside `String()`, not a package-level map.
- `exhaustive` is enabled (`G/.golangci.yml:39`) with `default-signifies-exhaustive: false` (`:251–252`): a `switch` over a typed enum must list every member even when it has a `default:`. Typing the kind makes the linter fail the build when a member lacks a `case`. That is the compile-time half of "fail closed".
- The reconciliation enum owns its strings. It must not borrow `strLocalDelete` and the other `actionType` wire constants as `S/planner_actions.go:404–433` does now: those strings are persisted in `retry_work.action_type` (`S/enums.go:282–291`), so the two vocabularies only coincide by accident.

**SQL changes.**

- `ELSE 'unknown'` (`:532`) → `ELSE 'unclassified'`.
- `ELSE 'noop'` (`:647`) → `ELSE 'unclassified'`.
- Delete the dead branches `:592–593` and `:645`.
- No new explicit no-op `WHEN` is needed: after reading every branch, no genuine no-op relies on the fallback (A1.4).
- Split the two `CASE` expressions out of the query text into named constants, `sqlComparisonKindCase` and `sqlReconciliationKindCase`, so tests can evaluate them alone.

**Go changes.**

1. Row structs carry the typed kinds (`S/sqlite_compare.go:680, 686, 687`). The scans (`:742, :800–801`) read into a local string, call `parse*Kind`, and wrap the error with the row path, exactly as `:746–752` does for `item_type`. An unknown string is therefore a load error naming the path: this is the "SQL and enum drifted" failure.
2. `'unclassified'` parses successfully to the zero member; it is the "state outside the decision table" failure and is reported with the state tuple:

   ```go
   var errUnclassifiedPlan = errors.New("sync: planner state is outside the decision table")

   type unclassifiedPlanError struct {
       Path           string
       Comparison     comparisonKind
       Reconciliation reconciliationKind
       State          string // "baseline_present=1 local_present=0 ... metadata_drift=0"
   }
   func (e *unclassifiedPlanError) Error() string
   func (e *unclassifiedPlanError) Is(target error) bool { return target == errUnclassifiedPlan }
   ```

3. **Where the state tuple comes from.** The comparison query returns 16 of the 29 `comparison_state` columns (`S/sqlite_compare.go:538–557` against `:476–504`); the 14 missing ones include most of the reconciliation `CASE` inputs. Two options:
   - *Recommended:* on the error path only, run a diagnostic query inside the same read transaction that produced the rows (`loadCurrentInputsTx`, `S/engine_current_plan.go:516–584`, right after the reconciliation query at `:558–563`): `sqlComparisonStateCTEs` plus a `reconciliation_state` that selects all 29 columns and `sqlReconciliationKindCase`, `WHERE reconciliation_kind = 'unclassified' ORDER BY path LIMIT 20`. It passes through `sqlQueryForStateTables` like the other two. Cost is one more evaluation of the CTE chain, paid only when the plan is already failing.
   - *Rejected for Part A:* widen the hot-path `SELECT` by 14 columns. It taxes every plan and needs `BenchmarkComparisonQueries` before and after (`G/CLAUDE.md:92`). Part B widens it anyway (Go needs the flags), at which point the diagnostic query is deleted.
4. `PlanCurrentState` gets a validation pre-pass before its loop (`S/planner_actions.go:53`), so it runs before the unavailable-truth early return (`:381–383`): any row whose comparison or reconciliation kind is the unclassified member returns `*unclassifiedPlanError` (state built from the 16 columns it holds). This is the backstop for callers that hand rows in directly (tests, the planner model through `planCurrentStateForStore`, `S/planner_model_test.go:204`).
5. The switch at `S/planner_actions.go:385–437` switches on the typed kind, lists `reconcileUnclassified` explicitly (returning the typed error), and keeps `default:` as an error for a value outside the enum.
6. `S/planner_move_composition.go:1061, 1072, 1128, 1174, 1180, 1182` use the typed constants; the string constant at `:1072` is deleted.
7. `errFolderTransferPlanned` (`S/planner_actions.go:61–71`) becomes a returned plan error instead of log-and-continue. A dropped row is a path that silently stops syncing; `G/CLAUDE.md:136` forbids swallowing errors. Risk: one such row stops the mount, so the PR runs the matrix golden and both model tests first to show it is never hit.

**How SQL gets the strings.** Recommended: the SQL keeps literals and tests prove agreement with the enum. Not recommended: generating the `CASE` text from the enum at statement-build time. It is possible (the text already passes through `sqlQueryForStateTables`, `:825–830`), but the query is a `const` concatenation today (`:269`), generation would turn a readable decision table into string assembly, and Part B deletes both `CASE` expressions. The enum is the source of truth; the SQL literals are a checked copy that lives until Part B.

**Tests that prove SQL and enum agree.**

- *Token test* (cheap, always on), in the style of `TestSQLSameLocalEntry_EveryAliasFollowsThePattern` (`S/sqlite_compare_test.go:667–682`): extract every `THEN '<x>'` and `ELSE '<x>'` from `sqlComparisonKindCase` and assert the set equals `allComparisonKinds()` rendered with `String()`; extract every `comparison_kind = '<x>'`, `THEN '<x>'` and `ELSE '<x>'` from `sqlReconciliationKindCase` and assert set equality with the two enums (the `comparison_kind =` set equals all comparison kinds except `unclassified`).
- *SQL-evaluated exhaustive test*, two stages, on an in-memory store connection:
  - Stage 1: a recursive CTE yields all 2^17 = 131,072 raw comparison states; evaluate `sqlComparisonKindCase` over it; assert the result is `unclassified` **iff** all three presence bits are 0, and that no `diverged` row has (`local_changed = 0 AND remote_changed = 0`) or (`current_equal = 1 AND remote_changed = 1`).
  - Stage 2: 18 kinds × 48 type combinations (3 `item_type` × 4 `local_item_type` × 4 `remote_item_type`) × 512 flag combinations = 442,368 rows; evaluate `sqlReconciliationKindCase`; assert that every row whose result is `unclassified` has input kind `unclassified`, or is `diverged` with `local_changed = 0 AND remote_changed = 0` (which stage 1 proves cannot be produced). The assertion is one-directional on purpose: in the synthetic product an earlier `WHEN` (`:584–586`, `:598`) can still match such a row.
  - Run times were not measured. If either stage is slow, guard it with `skipInShort(t)` (`S/test_helpers_test.go:350–362`; rule at `G/CLAUDE.md:224`).

**What a failed plan looks like to the user.**

- One-shot: the error is wrapped as `sync: planning actions: ...` (`S/engine_current_plan.go:605–607`). VERIFIED. If the check runs inside `loadCurrentInputsTx` it is wrapped by whatever wraps load errors; the PR should make both paths read the same. The CLI's presentation of that error was not read (SUSPECTED; see "Not read").
- Watch mode: `sync: watch replan build_current_plan: ...` (`S/engine_watch_replan.go:52–56, 75–89`) → returned through `runPendingWatchReplan` (`S/engine_watch_loop.go:184–187`), `runRunningLoopStep` (`:157–160`), `runWatchLoop` (`:55–61`), `RunWatch` (`S/engine_watch.go:109–114`) → the mount's runner ends (`M/orchestrator_watch.go:508`) → `handleWatchRunnerEvent` closes the engine and calls `reportWatchRunnerExit` (`M/orchestrator_watch.go:679, 690–692`), which "tells the user once: the first time, and again only when the error changes" and restarts after a backoff (`:697–699`) that doubles from 5 s to 5 min (`M/watch_reporting.go:11–17, 201–208`). VERIFIED up to the start of `reportWatchRunnerExit`; the message it prints for a generic error is SUSPECTED (the `switch` at `:704` was read only to its first case).
- Consequence to decide deliberately: an unclassified row stops the whole mount and it retries every five minutes until the state changes. That is fail-closed as asked. Because the state is unreachable today, any occurrence is a bug the typed error fully describes. Quarantining just the path as an observation issue is the alternative; it lets the rest sync but hides the bug behind a per-file issue. Not recommended as the first step.

### A1.7 Other `ELSE` and `default:` branches

**SQL `ELSE` lines in `S/sqlite_compare.go`** (VERIFIED, complete list): 87, 276, 277, 278, 295, 298, 301, 313, 318, 325, 333, 353, 372, 388, 408, 420, 425, 427, 429, 430, 431, 457, 532, 647. All except 532 and 647 are the residual value of a boolean flag expression (`... THEN 1 ELSE 0`); they are not decisions. They become explicit `return false` lines when Part B moves the flags to Go.

**Go `default:` branches in planner and consumers:**

| Site | What it does | Verdict |
|---|---|---|
| `S/planner_actions.go:436–437` | error on unknown reconciliation kind | Fail-closed already, but see `:381–383` above. VERIFIED |
| `S/planner.go:55–56` (`actionAllowedInMode`) | `default: return true` after all ten `actionType` members | Unreachable; permissive value. Change to `false` (an action type nobody listed must not be allowed in any mode). VERIFIED |
| `S/enums.go:231–232` (`Direction()`) | `default: return directionDownload` | Unreachable; arbitrary value. Make the zero direction or panic-in-test invariant. VERIFIED |
| `S/planner_actions.go:218–219` (`writesLocally`) | `default: return false` | Unreachable. VERIFIED |
| `S/planner_dependencies.go:287–288, 519–520` | `default: return deps` / `return false` | Unreachable. VERIFIED |
| `S/planner_dependencies.go:316–317, 344–345, 487–488` | tagless switches | Real cases, not defects. VERIFIED |
| `S/planner_move_composition.go:1184–1185` (`composedRemotePosition`) | `default: return ""` in a tagless switch | A real "no remote position" case. The risk is the three kind literals above it (`:1174, 1180, 1182`), fixed by typing. VERIFIED |
| `S/planner_move_composition.go:289–291` | default with a notice | Real case. VERIFIED |
| `S/planner_move_composition.go:1496–1497` (`createSide`) | silent default | SUSPECTED harmless; settle by reading its callers' enum |
| **`S/truth_status.go:112–126`** (`observationIssueBlocksLocalTruth(issueType string)`) | lists eight issue types → `true`; `default: return false` over an **untyped string** | **Fail-open.** A new issue type that should block local truth is silently treated as not blocking. Propose a typed issue-type enum with an exhaustive switch. Whether an existing issue type is wrongly missing from the list is SUSPECTED; settle by listing every `issue*` constant against `:113–122`. VERIFIED that the switch is over a string |
| `S/planner.go:273–298` (`isolateDependencyCycles`) | drops the actions in a dependency cycle, logs at Error (D-059c) | Deliberate, documented drop; outside this item, but it is the other place the planner discards work. VERIFIED |
| `S/store_delete_ledger.go:28–29` | `default: return nil` with `//nolint:exhaustive` | Documented. VERIFIED |
| `S/store_pending_creates.go:250–252` | documented default | VERIFIED |
| `S/store_write_baseline.go:537–550` (`classifyBaselineMutation`) | `default:` → error | Fail-closed already. VERIFIED |
| `S/store_write_baseline.go:596–608` (`updateBaselineCache`) | classification failure → Debug log and full reload | Self-healing; unreachable after a successful `applySingleMutation`. See B3. VERIFIED |

---

## A2. Foreign keys

### A2.1 Connection, driver, pragma

- Driver: `modernc.org/sqlite v1.61.0` (`G/go.mod:23`). VERIFIED.
- The read-write DSN already carries `_pragma=foreign_keys(ON)` (`S/store.go:109–114`, pragma at `:111`), and the pool is one connection (`db.SetMaxOpenConns(1)`, `:122`). VERIFIED. So the first `REFERENCES` clause added to the schema is enforced with no connection change.
- The read-only inspector DSN has no such pragma (`S/store_inspect.go:277`); it never writes. VERIFIED.
- The test invariant library already runs `PRAGMA foreign_key_check` (`S/sync_invariants_test.go:162–172`, query at `:165`). With no foreign key declared it passes trivially today. VERIFIED.
- `PRAGMA quick_check` is the only integrity pragma at open (`S/store_compatibility.go:134–162`). VERIFIED.
- modernc-specific behaviour was **not read in the module source**. The following are standard SQLite rules and are SUSPECTED for this driver until the two tests in the PR breakdown pass: the pragma is per connection and is a no-op inside a transaction; a parent key must be `PRIMARY KEY` or `UNIQUE`; a non-`INTEGER` `PRIMARY KEY` column admits `NULL` unless declared `NOT NULL`; an immediate violation fails the statement, a deferred one fails `COMMIT`; `INSERT OR REPLACE` on a parent row is a delete followed by an insert.

### A2.2 Tables

Fifteen tables, `S/schema.go:77–253`. VERIFIED.

| Table | Key | Lines |
|---|---|---|
| `store_metadata` | none (one row, `schema_generation`) | 77–79 |
| `baseline` | `item_id` PK, `path` UNIQUE; `parent_id TEXT` nullable | 81–98; index `idx_baseline_parent` 100 |
| `observation_state` | none (one row) | 102–116 |
| `pending_creates` | `path` PK; `item_id`, `parent_id` `NOT NULL DEFAULT ''` | 118–127; index 129 |
| `mount_boundaries` | `path` PK | 131–133 |
| `kept_local_folders` | `path` PK | 135–140 |
| `held_deletes` | (`path`, `side`) PK; `item_id DEFAULT ''` | 142–149 |
| `delete_ledger` | (`side`, `path`, `committed_at`) PK; `item_id DEFAULT ''` | 151–157; index 158 |
| `local_hash_cache` | `path` PK | 160–169 |
| `remote_state` | `item_id` PK, `path` UNIQUE | 171–180; index 182 |
| `local_state` | `path` PK | 184–195 |
| `retry_work` | (`path`, `old_path`, `action_type`) PK; `scope_key NOT NULL DEFAULT ''`; `blocked` CHECK IN (0, 1) | 197–207; indexes 209–212 |
| `observation_issues` | `path` PK; `scope_key DEFAULT ''` | 214–218; indexes 220–223 |
| `block_scopes` | `scope_key TEXT PRIMARY KEY` — **no `NOT NULL`**, unlike every other primary key (82, 119, 132, 136, 161, 173, 185, 215, 233) | 225–230 |
| `shortcut_roots` | `binding_item_id` PK; `remote_drive_id`, `remote_item_id`, ... | 232–253 |

Plus two per-plan `TEMP` tables, `planner_visible_local_state` and `planner_visible_remote_state` (`S/store_planner_visible.go:15–36`).

### A2.3 Every reference between tables, with a verdict

| # | Reference | Foreign key? | Why |
|---|---|---|---|
| 1 | `retry_work.scope_key` → `block_scopes.scope_key`, **only when `blocked = 1`** | **Yes, on a new column** (A2.4) | The one referential invariant checked at run time (`S/engine_runtime_invariants.go:235–237`). VERIFIED |
| 2 | `retry_work.scope_key` when `blocked = 0` | No | A timed (non-blocked) row carries the key as evidence only (`S/store_retry_work.go:191`), and a released row keeps its key (`:106–108`) while the scope row is deleted in the same transaction (`S/store_scope_admin.go:42–43`). VERIFIED |
| 3 | `observation_issues.scope_key` → `block_scopes` | No | Read-boundary keys are tags on `observation_issues` and are refused as `block_scopes` rows ("read boundaries belong in observation_issues, not block_scopes", `S/store_write_block_scopes.go:28–30`; `D/data-model.md:176–207`). VERIFIED |
| 4 | `baseline.parent_id` → `baseline.item_id` | No, not today | (a) A top-level item's parent is the mount's remote root ID or the Graph root alias (`S/executor.go:763–768`); whether a baseline row with that ID exists is SUSPECTED — settle by adding `SELECT COUNT(*) FROM baseline c WHERE c.parent_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM baseline p WHERE p.item_id = c.parent_id)` to `assertStoreIntegrity` (`S/sync_invariants_test.go:129–160`) and running the model test. (b) `commitDelete` deletes one row by path and leaves its children's `parent_id` (`S/store_write_baseline.go:799–806`). (c) The folder-rebind path re-points children to the new parent ID (`:741–749`) before the new parent row exists (`:762`); that alone would be solved by `DEFERRABLE INITIALLY DEFERRED`, (a) and (b) would not. Nothing reads the relation except that one `UPDATE` (`:743–744`), so the constraint would buy little. VERIFIED except (a) |
| 5 | `baseline.item_id` ↔ `remote_state.item_id` | No | One-sided rows are the signal: a `remote_state` row with no baseline row is a remote create, a baseline row with no `remote_state` row is a remote delete (`S/sqlite_compare.go:276–278, 524, 530`). VERIFIED |
| 6 | `pending_creates.item_id`, `.parent_id` → `baseline`/`remote_state` | No | Intent rows written before the item exists; both default to `''` (`S/schema.go:118–127`; writers `S/store_pending_creates.go:82–88, 104–110`). VERIFIED |
| 7 | `held_deletes.item_id` → `baseline.item_id` | No | Defaults to `''`; the table is emptied and rewritten on every plan (`S/store_held_deletes.go:24–50`, `DELETE` at `:33`). VERIFIED |
| 8 | `delete_ledger.item_id` → `baseline.item_id` | No | The ledger records a delete whose baseline row is removed by the same outcome (`S/store_delete_ledger.go:18–47`; `S/store_write_baseline.go:433`). VERIFIED |
| 9 | `shortcut_roots.remote_item_id` / `binding_item_id` → `remote_state`/`baseline` | SUSPECTED no | No join between `shortcut_roots` and another table was found; the shortcut code was not read. Settle by reading the `shortcut_root*` store files |
| 10 | Path-keyed tables with no parent table: `local_state`, `local_hash_cache`, `kept_local_folders`, `mount_boundaries`, `retry_work.path`, `observation_issues.path`, `held_deletes.path` | No | A path is not a key of any other table that must exist; a retry row can name a path absent from all three state tables (after a delete). VERIFIED for the schema; whether `local_hash_cache` rows may outlive `local_state` rows is SUSPECTED (its prune statement was not read) |

### A2.4 The one foreign key: blocked retry work → block scope

**Why not a plain `REFERENCES` on the existing column.** `scope_key` defaults to `''` (`S/schema.go:197–207`), carries evidence keys on non-blocked rows (row 2 above), and SQLite has no conditional foreign key. VERIFIED for the first two; the third is standard SQLite.

**Schema (generation 29).**

```sql
CREATE TABLE IF NOT EXISTS block_scopes (
    scope_key TEXT NOT NULL PRIMARY KEY CHECK(scope_key <> ''),
    ...                                        -- other columns unchanged
);

CREATE TABLE IF NOT EXISTS retry_work (
    ...
    scope_key         TEXT    NOT NULL DEFAULT '',                -- evidence; unchanged meaning
    blocked           INTEGER NOT NULL DEFAULT 0 CHECK(blocked IN (0, 1)),
    blocked_scope_key TEXT    REFERENCES block_scopes(scope_key), -- NULL unless blocked
    ...
    CHECK ((blocked = 1) = (blocked_scope_key IS NOT NULL)),
    PRIMARY KEY (path, old_path, action_type)
);

CREATE INDEX IF NOT EXISTS idx_retry_work_blocked_scope
    ON retry_work(blocked_scope_key) WHERE blocked_scope_key IS NOT NULL;
```

- **`ON DELETE`: the default (NO ACTION), immediate.** Not `CASCADE`: `DiscardScope` deletes *every* retry row carrying the key, blocked or not (`deleteRetryWorkByScopeTx`, `S/store_retry_work.go:86–92`: `DELETE FROM retry_work WHERE scope_key = ?`), and non-blocked rows are not foreign-key children, so a cascade would not reproduce it; and a cascade would silently delete retry work when some other code path deletes a scope by mistake. Deleting a scope that still has blocked rows should fail the statement.
- **Not `DEFERRABLE INITIALLY DEFERRED`.** Deferral does not help the real ordering problem, which is that parent and child are written in *separate transactions* today (below). Once they share a transaction, parent-first order is trivial, and an immediate key names the failing statement where a deferred one only fails `COMMIT`.
- End-state alternative: drop the `blocked` column and derive it from `blocked_scope_key IS NOT NULL`. Not costed: the SQL sites reading `blocked` beyond `S/store_retry_work.go:26–43, 106–127` were not enumerated (SUSPECTED size).
- `canonicalSyncStoreColumns()` (`S/schema.go:265–324`) gains the new column.

**Code that must change with it** (each VERIFIED as the current behaviour):

1. **Write order.** `persistFailureWithScopeEffects` persists the blocked child row at `S/engine_runtime_completion.go:324` and the parent scope at `:331/:334`; `RecordBlockedRetryWork` runs its own transaction with no `block_scopes` check (`S/store_retry_work.go:210–258`), and the scope is upserted separately (`S/engine_runtime_lifecycle.go:113`). Replace with one store method that, in one transaction, upserts the scope row and then the blocked row. Other `RecordBlockedRetryWork` callers: `S/engine_runtime_lifecycle.go:431`, `S/engine_runtime_permissions.go:333`, `S/engine_runtime_completion.go:606` — whether the scope row already exists at each is SUSPECTED; the immediate key makes any wrong order fail in the model tests.
2. **`INSERT OR REPLACE INTO block_scopes`** (`S/store_write_block_scopes.go:59–62`) → `INSERT ... ON CONFLICT(scope_key) DO UPDATE SET ...`. `REPLACE` deletes the parent row first; with children present that is a foreign-key violation or a cascade, depending on the action. The upsert avoids the question.
3. **`DiscardScope`** deletes the parent (`S/store_scope_admin.go:77–78`) before the children (`:83`). Swap the two statements.
4. **`markRetryWorkReadyTx`** (`S/store_retry_work.go:97–116`, `UPDATE` at `:106–108`) also sets `blocked_scope_key = NULL`. `ReleaseScopeWork` already deletes the scope row after releasing the rows, in the same transaction (`S/store_scope_admin.go:34–43`), so its order is right.
5. **`sqlUpsertRetryWork`** (`S/store_retry_work.go:12–20`) writes the new column.
6. **`PruneBlockScopesWithoutBlockedWork`** (`S/store_scope_admin.go:94–120`) reads, then deletes in separate transactions (`:95, :106, :114`). With the key, a blocked row written in between makes the delete fail instead of leaving an orphan; the method should become one statement: `DELETE FROM block_scopes WHERE scope_key NOT IN (SELECT blocked_scope_key FROM retry_work WHERE blocked_scope_key IS NOT NULL) AND <timed-scope keep rule>`. The keep rule (`shouldKeepTimedBlockScope`, `:110`) was not read; SUSPECTED whether it is expressible in SQL.
7. **`ReleaseOrphanBlockedRetryWork`** (`S/store_retry_work.go:122–139`) and its startup call (`S/engine_runtime_lifecycle.go:61`) are deleted: an orphan blocked row cannot exist. Its own comment cites a live case of exactly that orphan ("live on 2026-11-03, where two quota rows sat blocked across a restart with no scope", `S/engine_runtime_completion.go:310–322`).
8. The dry-run scratch store is unaffected: it writes only `remote_state`, `kept_local_folders` and `baseline` (`S/store_scratch.go:19, 25, 203`). VERIFIED by search.

### A2.5 Runtime invariant checks: what goes, what stays

`S/engine_runtime_invariants.go` runs only when `engine.assertInvariants` is set, and panics (`:10–21`). VERIFIED.

**Made redundant by the schema (delete):**

| Check | Lines | Replaced by |
|---|---|---|
| blocked row's scope is missing | 235–237 | the foreign key |
| blocked row has a zero scope key | 232–234 | `CHECK ((blocked = 1) = (blocked_scope_key IS NOT NULL))` plus `CHECK(scope_key <> '')` on the parent |
| persisted scope has a zero key | 185–187 | `NOT NULL ... CHECK(scope_key <> '')` |
| duplicate persisted scope | 188–190 | already impossible: primary key |

**Candidates for `CHECK` constraints in the same generation bump** (each needs the model tests to show no writer violates it):

| Check | Lines | Constraint |
|---|---|---|
| `AttemptCount <= 0` | 213–215 | `CHECK(attempt_count > 0)` — conflicts with the column default `0` (`S/schema.go:197–207`); drop the default |
| blocked row has `NextRetryAt != 0` | 238–240 | `CHECK(blocked = 0 OR next_retry_at = 0)` |
| timed row has `NextRetryAt <= 0` | 224–226 | `CHECK(blocked = 1 OR next_retry_at > 0)` |
| empty path | 207–209 | `CHECK(path <> '')` |

**Must stay at run time:**

- A persisted scope has at least one blocked row (`:192–194`): a foreign key cannot require children.
- Invalid action type on a retry row (`:210–212`): could be a `CHECK(action_type IN (...))`, but that would be a second copy of the `actionType` strings (`S/enums.go:191–216`); keep it in Go.
- Released / discarded scope checks against in-memory runtime state (`:268–294, :296–322`), including "no retry row with that key at all" after a discard (`:315–318`).
- All watch-runtime checks (`:104–166`): in-memory, not expressible in a schema.

### A2.6 Generation bump, mismatch, schema tests

- `currentSyncStoreGeneration = 28` (`S/schema.go:72`), history comment at `:12–71`. VERIFIED.
- `validateCanonicalSchema` compares **table names and column names only** (`S/schema.go:377–420`: tables `:386–393`, columns `:395–413`), then the generation (`:415`, `validateStoreGeneration` `:433–448`). A constraint-only change (a `CHECK`, `NOT NULL`, `REFERENCES`) is invisible to it, so the generation must go to 29 for an old store to be refused. VERIFIED.
- On mismatch the store returns `errIncompatibleSchema` (`S/schema.go:263`); `openEngineSyncStore` never mutates or recreates a store at startup and reports the incompatible-schema reason (`S/store_compatibility.go:82–117`, `:107–108`). The user runs `drive reset-sync-state` (`G/internal/cli/drive_reset_sync_state.go:20`), which calls `ResetStateDB` (`S/store_reset.go:15–35`). VERIFIED. Zero backward compatibility is the rule (`G/CLAUDE.md:62`): no migration.
- Operational note for this host (from the host's own instructions, not the repository): install the new binary before running the reset, or the reset recreates a generation-28 store.
- Schema tests (VERIFIED): `TestNewSyncStore_CreatesCanonicalSchema` (`S/schema_test.go:15–63`), `TestNewSyncStore_RejectsNonCanonicalSchema` (`:66–90`), `TestApplySchema_SeedsObservationStateRowForCurrentSchema` (`:93–114`), `TestApplySchema_RejectsGeneration6RemoteStateDriveOwnership` (`:117–143`). **There is no golden schema file**: `S/testdata` holds only `reconcile/matrix.golden` and `watch_capture/`.

---

# PART B — One owner for state, planner in Go

## B3. Baseline ownership

### B3.1 The copies

| # | Copy | Where | Lifetime |
|---|---|---|---|
| 1 | SQLite table `baseline` | `S/schema.go:81–98` | durable |
| 2 | The store's cache, one `*Baseline` object | field `S/store.go:67`; created in `Load` (`S/store_write_baseline.go:136–158`, assignment `:154`) | the store's life; rebuilt in place by `replaceContents` (`S/core_types.go:254–261`) |
| 3 | Per-plan snapshot `currentInputs.baseline` | `S/engine_current_plan.go:171–181` (field `:180`), built by `loadBaselineWithRunner` inside the plan's read transaction (`:572`; comment at `S/store_write_baseline.go:160–162`) | one plan |
| 4 | Dry-run scratch store's baseline | seeded **from the cache, not the database** (`S/store_scratch.go:139`), written at `:203` | one dry run |

Copy 2 is shared by pointer, not copied. Holders (VERIFIED by search): executor (`S/executor.go:95`; `newExecution(cfg, bl)` `:270`, called from `S/worker.go:208`), local observer (`S/observer_local.go:470, 570`), remote observer (`S/observer_remote.go:119`), item converter (`S/item_converter.go:43, 124`), watch pipeline (`S/engine_watch.go:148`, also `:338, :499`), delete safety (`S/planner_delete_safety.go:44`), plus parameters through scanner, root and permission code. `Load` call sites: `S/engine_startup.go:38`, `S/engine_run_once.go:85`, `S/engine_watch_loop.go:336`, `S/store_write_baseline.go:406, 458`. Watch mode keeps its startup pointer for the whole run, which is why a reload must mutate the object in place (comment at `S/store_write_baseline.go:646–650`).

### B3.2 The writers

**Database writers — complete production list** (VERIFIED by search):

- Outcome commits: `S/store_write_baseline.go:512` (`RefreshLocalBaseline`), `:743` (children re-pointed), `:756`, `:762` (`commitUpsert`), `:800` (`commitDelete`), `:862–865` (`repathSubtreeTx`).
- Local observation: `S/store_local_state.go:148` (identity backfill).
- Remote observation: `S/store_write_observation.go:452, 460`.
- Pending creates: `S/store_pending_creates.go:202, 277`.
- Scratch store only: `S/store_scratch.go:203`.

**Cache writers — complete production list** (VERIFIED by search): `S/store_write_baseline.go:154` (`Load`), `:520` (`RefreshLocalBaseline`), `:624`, `:626`, `:640` (`updateBaselineCache`), `:664/:667` (`reloadBaselineCache`), `S/store_write_observation.go:377` (`cacheAdoptedPlaceholders`). **No production code outside `S/core_types.go` and the store files touches the exported maps** (`ByPath`, `ByID`, `ByDirNameKey` appear only at `S/store_write_baseline.go:155, 182, 183, 186` and inside `S/core_types.go`).

So ownership is already single in practice: the store mutates, everyone else reads. What is missing is that the type system does not say so, and that cache and database are updated in two steps.

### B3.3 Where the copies can diverge

1. **A cache step that fails after the commit.** `CommitMutation` commits at `S/store_write_baseline.go:441`, then calls `updateBaselineCache` at `:446` and returns its error at `:447`. `updateBaselineCache` can call `reloadBaselineCache` (`:596–608, :615–622, :628–634`), which reads through `s.db` (`:656`) and can fail. The caller is told a committed mutation failed, and the cache is stale. Same shape in the observation commit: `tx.Commit()` at `S/store_write_observation.go:70`, `reloadBaselineCache` at `:75`, error returned at `:76`; and in `S/store_local_state.go:136–140`. VERIFIED. There is a fault test for the first (`TestFault_CommitMutationFailureLeavesCacheAndDatabaseAgreeing`, `S/store_fault_seam_test.go:246`), which shows the concern is known; I did not read which failure point it injects (SUSPECTED whether it covers a failure *after* `tx.Commit()`).
2. **The cache patch re-implements the SQL in Go.** `updateBaselineCache` (`:595–644`) decides per mutation whether a `Put`/`Delete` reproduces what the statement did, and falls back to a full reload for folder rebind (`:615–622`) and folder move (`:628–634`). Any future SQL-side effect not mirrored there diverges silently.
3. **Shared mutable entries.** `GetByPath` and `GetByID` return `*BaselineEntry` (`S/core_types.go:134–141, 146–153`) with only a comment forbidding mutation (`:132–133, :144–145`). The maps are exported "for test setup convenience" (`:116–121`).
4. **Unlocked reads of `s.baseline`**: `S/store_write_baseline.go:405`, `:457`, `:464`, `:956`, while `Load` guards the pointer with `s.baselineMu` (`:137`). `RefreshLocalBaseline` also calls `Put` without `baselineMu` (`:520`). With one writer connection this may be benign; it is not proven by a lock.
5. **Two baselines visible to the planner.** The plan uses `inputs.baseline` if non-nil, else the cache (`S/engine_current_plan.go:726–727, 737–738`): a runtime choice between two values of the same type.
6. **The dry run starts from the cache** (`S/store_scratch.go:139`), so a stale cache plans a dry run on stale data.
7. **The detector is switched off.** `CheckCacheConsistency` (`S/store_write_baseline.go:948–1035`) says it is "Intended for periodic verification in watch mode (B-198)" (`:948–954`) but has **no production caller**: callers are tests only (`S/cache_consistency_race_test.go:50`, `S/baseline_test.go:1686, 1696`, `S/store_fault_seam_test.go:268, 278, 305`), and it is not part of `S/sync_invariants_test.go`. It logs at Debug only (`:999, 1009, 1020, 1029`). `baselineEntriesEqual` (`:1037–1052`) **omits `ParentID`, `ItemType`, `ETag`, `DriveID`**. VERIFIED.

### B3.4 Proposal: the store owns; everyone else gets a read-only view

1. **Read interface.**

   ```go
   // baselineView is the only thing code outside the store holds.
   type baselineView interface {
       GetByPath(path string) (BaselineEntry, bool)   // by value
       GetByID(itemID string) (BaselineEntry, bool)   // by value
       GetCaseVariants(...) []BaselineEntry           // same parameters as today (S/core_types.go:164–174)
       Len() int
       ForEachPath(...)                                // same parameters as today (:275–282), entries by value
       FindOrphans(...)                                // same parameters as today (:295–326)
   }
   ```

   Maps become unexported; `Put`, `Delete`, `replaceContents` become unexported and are called only from the store files. Tests build baselines through `NewBaselineForTest` (`S/core_types.go:330`). Executor, observers, converter, watch pipeline and delete safety take `baselineView`. Returning values removes divergence channel 3; the cost is one struct copy per lookup, to be measured with the existing benchmarks before and after (`G/CLAUDE.md:92`). The number of call sites to convert was not counted.
2. **No fallible step after `COMMIT`.** Move the reload read inside the transaction: `loadBaselineWithRunner` already accepts a `sqlTxRunner` (`S/store_write_baseline.go:163–194`). Sequence: apply mutation → if a reload is needed, read the new contents through `tx` into a fresh map set → `tx.Commit()` → swap contents in memory under the lock (cannot fail). For the patch path (`Put`/`Delete`), build the patch before commit and apply after. `CommitMutation` then never returns an error for a committed mutation.
3. **One lock discipline.** Every read of `s.baseline` goes through `cachedBaseline()` (`S/store.go:197–202`) or holds `baselineMu`; fix `:405, :457, :464, :520, :956`.
4. **A distinct snapshot type.** `type planBaseline struct{ ... }` (immutable, built in the read transaction) for copy 3, so the planner signature cannot accept the live view and the `if inputs.baseline != nil` choice at `S/engine_current_plan.go:726–738` disappears.
5. **Dry run reads the database**, in a read transaction, not the cache (`S/store_scratch.go:139`).
6. **Turn the detector on in tests.** `baselineEntriesEqual` compares every field; `CheckCacheConsistency` returns its mismatches as an error and is called from `assertStoreIntegrity` (`S/sync_invariants_test.go:129–160`), so every model seed, crash point and chaos run checks cache = database. Fix its stale comment.

---

## B4. The decision table in Go

### B4.1 The pipeline today

**Inputs.** Tables `baseline`, `local_state`, `remote_state`, plus `kept_local_folders` (`S/sqlite_compare.go:364–367`). `loadCurrentInputsTx` (`S/engine_current_plan.go:516–584`) runs in one read transaction: local rows (`:522`) → observation state (`:526`) → `remote_state` (`:534–539`) → content filters (`:543–544`) → `replacePlannerVisibleStateTx` (`:545`), which fills the two `TEMP` tables (`S/store_planner_visible.go:154–201`) → comparison query (`:549–554`) → reconciliation query (`:558–563`) → observation issues (`:567/:571`) → baseline snapshot (`:572`). The queries read the `TEMP` tables through a text rewrite (`S/sqlite_compare.go:825–830`). **The comparison CTE chain is evaluated twice per plan**, once per query. VERIFIED.

**SQL stages** (`S/sqlite_compare.go`): `all_paths` (`:53–59`) → local move pairing (`local_move_candidates` `:60–123`, `local_move_sources` `:124–131`, `local_move_targets` `:136–142`) → remote move pairing (`remote_move_sources` `:149–185`, `remote_move_shadowed` `:189–195`, `remote_move_targets` `:196–201`) → ancestor-delete sets (`:208–262`) → `comparison_flags` (`:270–473`) → `comparison_state` with the comparison kind (`:474–533`) → `reconciliation_state` with the reconciliation kind (`:559–649`).

**Outputs.** `sQLiteComparisonRow` (16 columns, `S/sqlite_compare.go:538–557, 664–681`) and `sQLiteReconciliationRow` (8 columns, `:651–661, 683–692`), both ordered by path.

**Go stages** (`PlanCurrentState`, `S/planner_actions.go:21–105`): `buildSQLitePathViews` (`:43`) → `moveCompositionInputs` (`:48`) and `composeMoves` (`:49`), keyed by baseline item ID (`S/planner_move_composition.go:1053`) → per-row `buildActionsForReconciliation` (`:53–77`, body `:368–439`) → `dropReconciliationOfComposedMoves` (`:78`) → `buildComposedMoveActions` (`:79`) → `normalizeCurrentPlanActions` (`:81`) → `dropCreatesOntoComposedItems` (`:83`) → `partitionCurrentActionsForMode` (`:85`) → `bindMountContext` (`:86`) → `isolateDependencyCycles` (`:89`) → `actionPlan` (`:91–100`). Then the engine applies the delete-safety hold (`S/engine_current_plan.go:619–627`) and records the plan (`:636`). VERIFIED.

### B4.2 `decide(state) → kind`

**The struct.** Three side types replace the three presence bits, `item_type`, `local_item_type`, `remote_item_type`, and the two type-changed flags (all derivable: `item_type` is the first non-absent of baseline, local, remote (`:275`); `local_type_changed` is "baseline and local both present, types differ" (`:417–421`)).

```go
type sideType uint8 // sideAbsent, sideFile, sideFolder, sideRoot

type decisionState struct {
	Baseline, Local, Remote sideType

	LocalMoveSource  bool // local_move_candidate_count = 1 AND local_move_target <> ''
	LocalMoveDest    bool // local_move_candidate_count = 1 AND local_move_source <> ''
	RemoteMoveSource bool // remote_move_target <> ''
	RemoteMoveDest   bool // remote_move_source <> ''

	LocalItemReplaced, RemoteItemReplaced bool
	DeleteAwaitsAncestorMove              bool
	LocalCopyAwaitsAncestorMove           bool
	RemoteCreateAwaitsAncestorMove        bool
	KeptLocalFolder, RemoteMoveShadowed   bool
	CurrentEqual                          bool
	LocalChanged, RemoteChanged           bool
	LocalSubtreeChanged                   bool
	RemoteSubtreeChanged                  bool
	MetadataDrift                         bool
}

type decision struct {
	Comparison     comparisonKind     // move composition still consumes it
	Reconciliation reconciliationKind
}

func decide(s decisionState) decision // pure; = reconcile(classify(s), s)
```

All fields are comparable, so the struct is a map key and a table-test key. `LocalMoveSource`/`LocalMoveDest` must be derived from the *coalesced* count exactly as SQL does (`COALESCE(lms.candidate_count, lmt.candidate_count, 0)`, `:411`); the comparison `CASE` uses the count only in conjunction with target (`:508`) or source (`:513`), so the two booleans lose nothing. VERIFIED by reading the two branches.

**Cross-product sizes** (arithmetic):

| Space | Size |
|---|---|
| Raw comparison stage: 17 two-valued inputs | 2^17 = 131,072 (114,688 with at least one side present) |
| Raw reconciliation stage: 17 kinds × 48 type combinations × 2^9 flags | 417,792 (442,368 with `unclassified` as an eighteenth kind) |
| Raw, both stages composed: 23 distinct booleans (17 + 9 − 3 shared) × 48 | 2^23 × 48 = 402,653,184 |
| **Normalized `decisionState`: 4^3 side types × 2^17 booleans** | **64 × 131,072 = 8,388,608** (8,257,536 with at least one side present) |
| Normalized classify stage alone: 64 × 2^13 | 524,288 |
| Normalized reconcile stage alone: 17 kinds × 64 × 2^7 | 139,264 (147,456 with 18 kinds) |

**Exhaustive test over all 8,388,608 states** (loop in Go; run time not measured; `skipInShort` if needed):

1. `decide(s)` is unclassified **iff** all three sides are absent — for states that satisfy the derivation rules below.
2. Derivation rules as a pure `func (s decisionState) valid() error`: a flag about an absent side is false; a type mismatch between baseline and a present side implies that side's `Changed` (`:287, :310`); a same-type folder is never `Changed` (`:288, :311`); and so on, one rule per line of the flag definitions. For an invalid state `decide` returns unclassified. This replaces the runtime guards at `S/planner_actions.go:395–397, 400–402` with a table property: **no valid state yields `upload` with a local folder or `download` with a remote folder.** Over the unconstrained product the current SQL table does not have that property (for example baseline folder, remote folder, `remote_changed = 1`, local absent → `download` at `:636`), which is why the constraint has to be explicit. The count of valid states was not computed.
3. One cell needs a decision: (absent, **root**, absent) yields `upload` at `:612` because `item_type` is not `'folder'`. Whether a root-typed row can reach `local_state` or `baseline` is SUSPECTED (the CHECK allows it, `S/schema.go:85, 186`; remote roots are filtered at `S/item_converter.go:489–497`). Settle by reading the scanner's row construction; then either make the cell unclassified or delete `root` from the CHECK.

**SQL reduced to joining.** Go already loads all three tables in the plan transaction (`S/engine_current_plan.go:522, 534–539, 572`). The scalar flags (`local_changed`, `remote_changed`, the replaced and type-changed flags, `current_equal`, `metadata_drift`: `:285–334, :390–409, :417–426, :435–458`) become a pure `deriveFlags(baselineRow, localRow, remoteRow)`. The set-valued parts stay in SQL at first: `all_paths`, move pairing (`:60–201`), ancestor-delete sets (`:208–262`), and the three descendant probes (`local_subtree_changed` `:343–352`, `remote_subtree_changed` `:378–387`, `kept_local_folder` `:364–371`); those can later become one pass over path-sorted rows in Go. The end-state query returns one row per path with the three sides' columns and the set-valued booleans, once per plan instead of twice.

**Oracle phase before deleting the SQL.** With Part A's `sqlComparisonKindCase` and `sqlReconciliationKindCase` constants in place, a test evaluates the SQL `CASE` over the stage tables and asserts equality with `decide` for every state; the engine also runs both on real rows in the planner model and the matrix. Only when they agree are the SQL `CASE` expressions deleted.

### B4.3 Existing planner tests, and what becomes obsolete

Present (VERIFIED by listing and, where a line is given, by reading):

- Golden: `TestReconcileMatrix_Golden` (`S/reconcile_matrix_test.go:131–195`), 99 cells (75 + 24), golden at `S/testdata/reconcile/matrix.golden`, regenerated with `-update` (`:16–21`). It prints **actions, not kinds**.
- Model with seeds: `TestPlannerModel_RandomTriplesConverge` (`S/planner_model_test.go:669–679`), 8 default seeds (`:33`), up to 10 rounds (`:34`), 3 edits a side (`:35`), `ONEDRIVE_GO_PLANNER_SEED` / `_SEEDS` (`:36–37`); it runs the real comparison SQL and planner against a pure model with no executor, filesystem or Graph (`:20–30`, `plan()` at `:147–205`).
- Unit and scenario files: `planner_actions_test.go` (35 tests), `sqlite_compare_test.go` (13), `comparison_hashless_test.go` (6), `engine_current_plan_test.go` (7), `planner_move_composition_test.go` (4), `store_planner_visible_test.go` (4), `planner_dependencies_test.go` (2), `planner_remote_delete_safety_test.go` (1), `planner_delete_safety_test.go`, `engine_comparison_facts_test.go`, `engine_move_composition_test.go`, `engine_move_scenario_test.go`, `engine_folder_rebind_test.go` (2).

After the move:

- **Survive unchanged:** the matrix golden (action level), the planner model, the engine model, move-composition and dependency tests.
- **Rewritten as table tests on `decide`/`deriveFlags`:** the kind-literal assertions in `S/sqlite_compare_test.go` (helpers `:10, :19`; lines listed in A1.5), `S/engine_current_plan_test.go:101–102`, `S/planner_actions_test.go:1066, 1109`.
- **Deleted with the SQL `CASE`s:** Part A's token test and SQL-evaluated tests.

### B4.4 Measure first

- `BenchmarkComparisonQueries` (`S/sqlite_compare_bench_test.go:36–56`): items ∈ {10,000, 100,000} × shapes `steady`, `remote-rename`, `local-rename`.
- `BenchmarkBuildDependencies` (`S/planner_dependencies_bench_test.go:39–58`): 10,000 and 100,000 actions; `BenchmarkBuildDependenciesScanningOracle` (`:60–72`).
- `BenchmarkReplacePlannerVisibleState` (`S/store_planner_visible_bench_test.go:45–65`): 9,910 rows a side.
- Recorded results: `D/performance-benchmarking.md:653–753`. Comparison table at `:663–668`: 10,000 items 2.0 s steady / 2.1 s remote rename / 2.8 s local rename; 100,000 items 23 s / 23 s / 24 s; "the quiet pass itself (about 11 s per query at 100k items) is the next target" (`:677–678`); host and date at `:683–689`; dependency table `:699–702`; planner-visible table `:726–729`. VERIFIED (read; not re-measured).

The Go-side decision is compared on the same benchmark and shapes, and the result is added next to `:663–668`. Evaluating the chain once instead of twice is the first expected gain; that is a hypothesis until the benchmark is run.

---

## B5. Deterministic simulation as the primary test

### B5.1 What exists (VERIFIED unless marked)

| Piece | Where | What it does |
|---|---|---|
| Engine model test | `S/engine_model_based_test.go:23–54`; `TestRunOnce_RandomizedBidirectionalEditsConverge` `:93` | Random edits on a local tree and a fake OneDrive, `RunOnce` until quiet, four invariants (convergence, no content loss, untouched untouched, quiescence). 12 default seeds (`:63`), 6 batches (`:57`), ≤3 operations a side (`:61`), ≤8 passes (`:62`); `ONEDRIVE_GO_MODEL_SEED`, `_SEEDS`, `_SEED_START`, `_TRACE` (`:69–72`) |
| Seeded dispatch, one worker | `eng.serializeWorkers = true` (`:356`); D-090 (`:44–48`); `S/dispatch_order_test.go:22–60` | The ready frontier is dispatched in an order shuffled by the seed |
| Replay equality | `TestModel_SameSeedReplaysIdentically` (`:109`) | Same seed, same trace |
| Crash and restart | nine named crash points (`S/crash_points.go:13–40`), `reachCrashPoint` (`S/store.go:234–240`); `TestModel_CrashAtSeededPointsRecovers` (`:142`, 4 seeds `:64`); `TestCrashRecovery_EveryCrashPointConverges` (`S/crash_points_test.go:390`) | Kill at a store boundary, reopen, converge |
| Chaos Graph | `S/chaos_remote_test.go`; `TestModel_ChaosGraphConverges` (`:476`), 2 default seeds (`:55`), `ONEDRIVE_GO_CHAOS_SEEDS` (`:54`) | Faulty remote in the model |
| Store fault seam | `openSyncStoreWithDriver` (`D/sync-store.md:133`); `S/store_fault_seam_test.go:246, 288, 321` | Fault-injecting SQL driver |
| Filesystem shims | `S/../synctest/fsshim.go:74, 101, 239`, `identityshim.go:32, 55`; `S/fs_shim_scenario_test.go:47, 119, 144, 168` | errno injection, case-insensitive view, identity reuse — each wraps a **real** directory |
| Clock seam | `syncClock` (`S/domain_clock.go:20–26`); the engine wires one clock into store and executor (`S/engine.go:245–249`) | |
| Virtual time | `testing/synctest` bubble (`S/virtual_clock_test.go:20–42`; tests `:45, 74, 111, 160, 192`) | For buffers and observers, not the model |
| Planner model | `S/planner_model_test.go` | See B4.3 |
| Invariant library | `S/sync_invariants_test.go:16–35, 129–160, 216–254` | Store integrity, tree comparison |

### B5.2 What is outside their reach

1. **Watch mode.** The model drives `RunOnce` only (`S/engine_model_based_test.go:469`). The watch loop, replan debounce, retry/trial timers, maintenance tickers, Socket.IO and the watch observers are not in any seeded model. VERIFIED.
2. **Time.** The model's clock is the wall clock plus an offset (`modelClock`, `:557–574`; `time.Now().Add(c.offset)` at `:567`). Eleven direct wall-clock calls remain outside `S/domain_clock.go`: `S/tx.go:39, 96`; `S/buffer.go:48`; `S/observer_remote.go:221, 251, 257, 617, 695, 720`; `S/observer_local.go:890`; `S/engine_run_once.go:215`. Jitter is unseeded (`realJitter`, `rand.N`, `S/domain_clock.go:128`). VERIFIED by search.
3. **Filesystem.** The model runs on a real temporary directory (`:302, :689–716`); inode numbers, mtime granularity and case sensitivity come from the host. There is no in-memory filesystem. At least 25 direct filesystem calls in 13 files bypass the `synctree` handle (lower bound from search): `symlink_observation.go` 6, `engine_root_lock.go` 4, `store_scratch.go` 2, `executor_conflict.go` 2, `observation_root_identity.go` 2, `inotify_linux.go` 2, and one each in `scanner.go`, `observer_local_handlers.go`, `store_compatibility.go`, `observer_local_collisions.go`, `debug_event_sink.go`, `executor_preconditions.go`, `store_reset.go`. The candidates document adds that "uploads, hashing and followed-symlink reads bypass the filesystem seam via `localpath`" (`R/rearchitecture-candidates.md:193–196`).
4. **Concurrency.** Ten goroutine launch sites: `S/engine_watch_maintenance.go:131`; `S/buffer.go:143`; `S/socketio_conn.go:107`; `S/engine_watch.go:426, 532, 537`; `S/engine_primary_root_watch.go:20, 60`; `S/worker.go:108, 111`. Only worker dispatch is seeded. VERIFIED by search.
5. **Combined faults.** Store faults and filesystem errno shims have their own scenario tests; whether `runModelScenarioWith` (`:286–345`) can combine them with chaos and crashes in one seed is SUSPECTED no (its options were not re-read for this report).
6. **Crash between a side effect and its commit in the executor.** The nine crash points were counted, not classified one by one; which executor boundaries they cover is SUSPECTED (read `S/crash_points.go:13–40` against the executor's commit calls).

### B5.3 Seams that exist, seams that are missing

| Seam | State |
|---|---|
| Worker dispatch order | Exists (D-090) |
| Clock interface | Exists, but 11 call sites bypass it and the model does not inject a fake |
| Jitter | Missing: needs a seeded source behind `syncClock` or a sibling interface |
| Remote (Graph) | Exists (fake plus chaos) |
| Store I/O faults | Exists (driver seam) |
| Crash points | Exist in the store; executor coverage unclear |
| Filesystem | Half: handle and shims exist; no in-memory implementation; 25+ bypassing calls |
| Goroutine spawn and timers | **Missing.** `testing/synctest` gives virtual time and a quiescence wait but does not seed interleaving. A single-threaded simulation needs every one of the ten launch sites and every timer to go through one injected scheduler (`Go(func())`, `After(d)`, `Now()`), with a production implementation (goroutines, real timers) and a simulated one (a run queue driven by the seed) |
| Watch event sources | **Missing.** inotify and Socket.IO need fake sources fed by the scheduler |

### B5.4 What it takes

1. Inject a fake `syncClock` into the engine model (or run the model inside a `synctest` bubble); route the 11 direct sites through the clock; seed the jitter. Proof: `TestModel_SameSeedReplaysIdentically` passes with the wall clock frozen, and a lint rule (forbidigo-style) bans `time.Now` outside `S/domain_clock.go`. Whether such a rule is already configured was not checked.
2. An in-memory `synctree` implementation with capability flags (case-insensitive, NFD normalizing, coarse mtime, ENOSPC), and the 25+ bypassing calls routed through the handle. Proof: the engine model runs with no temporary directory; a host-dependent incident such as LI-20261029-01 (NFD names; `R/live-incidents.md:88–118`) replays on Linux.
3. The scheduler seam for goroutines and timers; watch mode brought into the model (edits arrive as fake watcher events while actions are in flight).
4. One seeded fault plan across the existing seams (Graph chaos, store driver faults, errno shims, crash points).
5. The invariant stays the one the model already states (`S/engine_model_based_test.go:26–34`), checked by the invariant library after every step, with the cache-consistency check from B3.4 added.
6. "Primary" then means: a new behaviour is first expressed as a model operation or fault, and a seed that finds a bug is kept as a named scenario test, which is already the practice (`:40–42`).

The candidates document's own words on the remaining work: "Item 7's harness landed in WS9; what remains is routing the last direct I/O through the seams." (`R/rearchitecture-candidates.md:251–252`).

---

# PART C — Identity-keyed reconciliation

## C6.1 The candidate, quoted

`R/rearchitecture-candidates.md:43–78`, "### 1. Identity-keyed reconciliation instead of path-universe comparison". VERIFIED (read verbatim).

> **Today:** the comparison joins baseline, local and remote rows on path (`sqlite_compare.go`). Moves are recovered afterwards by pairing deletes with creates, behind gates such as "destination not occupied" and "source still exists". (`:45–48`)

> **Evidence:** most of WS5 (D-050..D-059e) and part of WS4 (D-060, D-061). Every one of the 14 move-composition fixes in #799 is a rule that recovers identity from paths, and the model test's remaining non-converging seeds are more of the same (cross-move cycles, divergent renames, a move of an edited file). (`:50–54`)

> **Landed:** #808 applies each delta window as a set, keyed by item ID, and rebinds a folder recreated online at the same path (D-060, D-061), which removes the path-ordering wedges on the remote side. #810 composes moves per item by (parent item, name) with SQLite keeping only structural pairing; #819 and #822 add the entry's birth time to local identity, the generation guard this item asks for; the model test converges 80/80 seeds (#810) and the planner-level model 500/500 (#812). (`:56–62`)

> **Proposal:** reconcile three trees whose nodes are keyed by stable identity: the remote item ID, the local (device, inode) with a generation guard, and the baseline row. A change is an attribute delta on a node (name, parent, content, type), so a move and an edit compose trivially. Paths become a derived view for addressing the filesystem and for display. This is Nucleus's design. (`:64–69`)

> **Risk:** the largest change. Local identity on Linux is weaker than Dropbox's (inode reuse, FAT, network filesystems), so it needs a "local identity confidence" with a fallback to today's path pairing. (`:71–73`)

> **How to research it:** write the planner as a pure function over the existing `baseline`, `local_state` and `remote_state` rows (they already carry item IDs and inodes); run it in shadow mode beside the current planner in the model test; diff the plans and count converging seeds. (`:75–78`)

Sequencing (`:245–255`): "Item 1 is the big one: do it last, in shadow mode, measured by the model test." (`:253–254`).

Candidate 7 (`:188–205`) is the simulation of B5; its proposal: "one seeded scheduler owns worker dispatch, watcher events and timers; a fake clock; the fake Graph with chaos (429, 5xx, timeouts, reordered pages, lost replies); a fake filesystem with capability flags (case-insensitive, NFD, 2-second mtime, ENOSPC). Every failure replays from its seed." (`:198–202`).

## C6.2 How moves and renames are detected today

Design text: `D/sync-planning.md:399–409` (case-only rename, R-2.12.1), `:411–493` (Move Composition), `:495–510` (Dependency Build). Code (VERIFIED):

- **Local move pairing, in SQL.** `local_move_candidates` (`S/sqlite_compare.go:60–123`) pairs a baseline row with a `local_state` row at another path by local identity (`:64–103`, the `sqlSameLocalEntry*` rule defined at `:14–33`), with file corroboration (`:71–91`) and gates: destination not in baseline (`:92`), item still in `remote_state` (`:93`), destination not occupied remotely by another item (`:94`), hard-link guard (`:98–103`, D-056). Entries without identity fall back to hash pairing where "the source must be gone" (`:104–122`). Ambiguity is counted (`candidate_count`, `:124–131`) and only a count of 1 is a move (`:508, :513`).
- **Remote move pairing, in SQL.** `remote_move_sources` joins `remote_state` to baseline **on `item_id`** with a different path (`:154`), requires that the local source is still the item (`:161–174`) and that the destination is free (`:175–184`).
- **When a gate fails, the move is replayed as delete plus create by design**: "A local move of an item deleted on OneDrive is a fresh upload at the destination; a remote move of an item deleted or moved locally is a fresh download" (`:143–148`); "the source plays out as a delete (hash-verified, recoverable) and the destination as a create/create conflict that keeps both versions" (`:175–179`); a folder "moved without a provable identity pairing" is deleted remotely (`:631–634`).
- **Composition, in Go, by item.** `moveCompositionInputs` builds nodes keyed by `entry.ItemID` (`S/planner_move_composition.go:1046–1065`, key at `:1053`); positions are (parent item, name) (`composedLocalPosition` `:1127–1136`, `composedRemotePosition` `:1155–1187`).

## C6.3 What is path-keyed and what is already identity-keyed

**Path-keyed** (VERIFIED):

- The row universe: `all_paths` is a union of paths (`S/sqlite_compare.go:53–59`), and the three sides are joined **on path** (`:460–462`). Every flag (`:270–473`) and both kind `CASE`s are per path.
- `local_state` primary key is `path` (`S/schema.go:185`); there is no index on local identity (indexes in `S/schema.go` are at `:100, 129, 158, 182, 209–212, 220–223` only).
- `remote_state.path` is `UNIQUE` (`S/schema.go:174`), which forces synthetic paths for two remote items at one path: remote twins `P/:twin:<itemID>` (`D/sync-store.md:434–453`) and the observation window's `:park:<itemID>` (`:455–483`). `remote_state` "intentionally does not persist remote parent ancestry" (`D/sync-store.md:501–504`).
- `retry_work` (key `path, old_path, action_type`), `held_deletes` (`path, side`), `observation_issues` (`path`), `kept_local_folders`, `mount_boundaries`, `local_hash_cache`.
- Subtree probes by path range (`S/sqlite_compare.go:216, 345, 370`).
- The executor resolves a parent by path (`ResolveParentID`, `S/executor.go:756–780`: the parent folder's baseline row by path, `:775–776`).

**Already identity-keyed** (VERIFIED):

- `baseline` primary key `item_id` (`S/schema.go:82`), "so remote moves are atomic UPDATEs" (`D/data-model.md:81–82`); `remote_state` primary key `item_id` (`S/schema.go:173`).
- Remote move pairing on `item_id` (`S/sqlite_compare.go:154`); `remote_subtree_changed` matches descendants by `bd.item_id = rd.item_id` (`:384`).
- Local identity (device, inode, birth time, has-identity flag) is stored on both `baseline` (`S/schema.go:96`; fields listed at `S/store_write_baseline.go:1037–1052`) and `local_state` (`S/schema.go:191–194`).
- Move composition nodes (`S/planner_move_composition.go:1053`).
- Delta windows applied as a set keyed by item ID (#808, quoted above).

So identity is present in the data and is used to *repair* a path-keyed comparison. Candidate 1 inverts that: compare by identity, derive paths.

## C6.4 Incidents that trace to path keying

**None found in the incident documents.** VERIFIED by search:

- `R/live-recurring-families.md`: no line matches rename, moved, item id, identity, tombstone, path-keyed, or delete + create.
- `R/live-incidents.md`: four lines mention rename / tombstone / case-only (`:29, :30, :37` are renamed test names; `:478` is a conflict copy renaming a local file away). Headings matching move/rename/identity: `:681` (LI-20260422-01, a test bug), `:2104` (LI-20260407-02), `:2177` (LI-20260405-05, crash recovery).

The two nearest:

- **LI-20260407-02** (`R/live-incidents.md:2104–2119`): "sync upload execution still recreated files through the parent-path upload route even when the conflict record already carried the authoritative remote `itemID`" (`:2113`); fixed so that "`ExecuteUpload` now overwrites by item ID whenever the action carries a non-empty `ItemID`" (`:2118`). Path-versus-identity, but in **execution**, not reconciliation.
- **LI-20261029-01** (`R/live-incidents.md:88–118`): NFD-named files; "moves and conflict copies saw the file as already absent" (`:112–113`), "NFD-named folders got an NFC twin for remote additions" (`:113–114`). A path **spelling** defect; identity keying would have made the lookups immune, but the incident is not recorded as a reconciliation defect.

The evidence for the cost of path keying is therefore the candidates document (`R/rearchitecture-candidates.md:50–54`) and the commit history, not incidents: `59f1ee6f` "Place a nameless tombstone by its remote_state path (#857)" (touches `S/item_converter.go`, `S/engine_remote_path_lookup.go`, `S/observer_remote.go`; R-2.4.17), `36286daf` (#819) and `1db0fcce` (#822) adding birth time to local identity, `26b3bc8b` (#825). The case-only rename fix in `8a68337d` (#856) is in `internal/cli/mv.go`, not in sync. VERIFIED from `git log`/`git show`.

Of the examples the brief names: case-only renames are handled by a designed rule (`D/sync-planning.md:399–409`), not an incident; folder moves replayed as delete + create are the designed fallback when a pairing gate fails (C6.2); nameless tombstones are the #857 fix, which resolves a tombstone *through* `remote_state`'s path because the comparison needs a path.

## C6.5 Outline of an identity-keyed design

**Keys.**

- Remote: `remote_state.item_id` (already the primary key).
- Baseline: `baseline.item_id`.
- Local: (`local_device`, `local_inode`, `local_birth_time`) when `local_has_identity = 1`, with a **confidence**: `strong` (identity present, birth time non-zero), `weak` (identity present, birth time 0), `none`. `weak` and `none` fall back to today's path pairing, as the candidate's risk paragraph requires (`R/rearchitecture-candidates.md:71–73`). The hard-link guard (`S/sqlite_compare.go:98–103`) becomes "identity seen at two paths → confidence `none` for both".

**Join.** One node per identity: baseline ⟕ remote on `item_id`; baseline ⟕ local on local identity (or on path when confidence is not `strong`). A remote row with no baseline is a remote create; a local row matching no baseline identity is a local create; a baseline with neither is `both_missing`.

**Node state and decision.** Per side, relative to the baseline: presence, parent (as the parent's node key), name, type, content. `decide` runs per node on enums of those deltas, so "moved and edited" is two independent attribute deltas rather than a move pairing plus a content compare at a new path. A second pure step resolves **position conflicts** (two nodes claiming the same (parent, name)), which replaces the "destination not occupied" gates (`:92–94, :175–184`) and the shadowing rule (`:189–195`). Paths are computed last, for the executor and for display.

**Tables affected at cut-over** (not in the shadow stage):

- `remote_state`: add `parent_id`; drop `UNIQUE` on `path`; delete the synthetic `:twin:` / `:park:` paths and their code.
- `local_state`: index on (`local_device`, `local_inode`, `local_birth_time`).
- `retry_work`, `held_deletes`, `observation_issues` stay path-keyed (they address the filesystem and the user); `held_deletes` already carries `item_id`.
- A generation bump and reset, as in A2.6.

**The shadow stage needs no schema change.** A remote row's parent identity can be derived inside one snapshot from `path` → parent path → `remote_state.path` (unique), and the candidate says the rows "already carry item IDs and inodes" (`R/rearchitecture-candidates.md:75–78`).

## C6.6 Shadow mode

- `planIdentityKeyed(inputs) (*actionPlan, error)`: a pure function over the same per-plan snapshot the current planner uses (`currentInputs`, `S/engine_current_plan.go:171–181`), written after Part B so it reuses `deriveFlags`, the kind enums and the action builders.
- `diffPlans(current, shadow *actionPlan) []planDifference`: pure; compares the two action sets after normalizing each action to (type, path, old path, item ID); ordering and dependency edges are ignored at first.
- **Never executed.** The shadow plan is not recorded with `RecordPlan` (`S/engine_current_plan.go:636`) and never reaches the dispatcher.
- **Where differences go.**
  - In tests (the main place): `pmState.plan()` (`S/planner_model_test.go:147–205`) and the engine model compute both plans; each seed's differences go into the failure log; a summary line counts seeds with differences. A second test runs the planner model with the shadow planner *driving* the model executor and asserts the same convergence and no-content-loss oracle (`:716–726`), giving "count converging seeds" for both planners.
  - A second golden, `S/testdata/reconcile/matrix.identity.golden`, for the same 99 cells of `TestReconcileMatrix_Golden`, so every intended difference is a reviewed diff.
  - In a live watch run on the test drive (optional): one debug event per plan with the difference count, through the existing debug event sink, which appends JSON lines to a file (`S/debug_event_sink.go:23–56`). Not stored in the state database, so no schema change. The `DebugEvent` type's fields were not read (SUSPECTED that a new event type fits).
- **Cost guard.** The shadow planner runs only in tests and when the debug event sink is configured; `BenchmarkComparisonQueries` shapes are run with it on and off before it is enabled anywhere outside tests.
- **Exit criterion** for cut-over: zero unexplained differences on the matrix; the shadow-driven model converges on at least as many seeds as the current planner at a wide seed count (the candidates document reports 80/80 and 500/500 for the current one, `R/rearchitecture-candidates.md:60–62`).

---

# PR breakdown

Each PR follows the repository's rules: testify only, `// Validates: R-X.Y.Z` on tests, no `time.Sleep`, golden files regenerated with `-update` (`G/CLAUDE.md:206–220`), exhaustive tests behind `skipInShort` (`:224`), design document and `GOVERNS:` line updated in the same PR (`:45–50`). The requirement IDs for new tests were not looked up.

## Part A (small, first)

**A-1. Typed kinds.** New `S/planner_kinds.go` (two enums, `String`, `parse*`, `all*`); typed row fields; scans parse with the path in the error; `S/planner_actions.go:385–437` and `S/planner_move_composition.go:1061–1182` use the constants; `CASE` expressions split into `sqlComparisonKindCase` / `sqlReconciliationKindCase`. No behaviour change for valid states.
- Tests: round-trip `parse(String(k)) == k` for every member; `parse` of an unknown string errors; the **token test** (SQL literals = enum members); a scan test where a doctored query returns `'bogus'` and the load fails naming the path; existing matrix golden and both models unchanged.

**A-2. Fail closed.** Both `ELSE` branches → `'unclassified'`; delete dead `WHEN`s `:592–593`, `:645`; `unclassifiedPlanError` with the diagnostic query; validation pre-pass ahead of `S/planner_actions.go:381–383`; `errFolderTransferPlanned` returned, not dropped; permissive `default:` values fixed (`S/planner.go:55–56`, `S/enums.go:231–232`). Depends on A-1.
- Tests: **SQL-evaluated exhaustive stages 1 and 2** (A1.6); a planner test that feeds an unclassified row under unavailable truth and gets the typed error (proves the pre-pass runs before the early return); an engine test that plants a state yielding `unclassified` (via a test-only query override) and asserts `errors.Is(err, errUnclassifiedPlan)`, the path and the tuple in the message; one-shot and watch error-path tests asserting the wrap text; matrix golden unchanged (proves the dead branches were dead); both model tests green; one test settling the SUSPECTED (`diverged`, 1, 0, equal) → `upload` cell.

**A-3. Typed observation issue types.** Enum for issue types; `observationIssueBlocksLocalTruth` (`S/truth_status.go:112–126`) becomes an exhaustive switch. Independent of A-1/A-2.
- Tests: table test over every member; the `exhaustive` linter is the proof that a new member cannot be forgotten.

**A-4. Foreign key and CHECKs, generation 29.** Schema of A2.4 plus the CHECKs of A2.5; one store method writing scope then blocked row in one transaction; upsert instead of `INSERT OR REPLACE`; `DiscardScope` order; `markRetryWorkReadyTx` clears `blocked_scope_key`; single-statement prune; delete `ReleaseOrphanBlockedRetryWork`, its startup call, and the four redundant runtime checks. Independent of A-1..A-3; one reset of the test drive (install the new binary first).
- Tests: `PRAGMA foreign_keys` returns 1 on the store connection; `PRAGMA foreign_key_list(retry_work)` lists the key (pins the schema, since there is no golden schema); inserting a blocked row with no scope fails; deleting a scope with blocked rows fails; `DiscardScope` and `ReleaseScopeWork` leave `foreign_key_check` clean; each CHECK rejects its bad row; a generation-28 store is refused with `errIncompatibleSchema` (extend `S/schema_test.go:66–90`); the crash-point suite and the engine model (which run `assertForeignKeysClean`, `S/sync_invariants_test.go:162–172`) stay green — now a non-trivial check; a crash-point test between "scope written" and "blocked row written" shows no orphan after restart.

## Part B (large, later)

**B-1. Baseline is read-only outside the store.** `baselineView`, value returns, unexported maps and mutators. Depends on nothing in A.
- Tests: compile-time (holders take the interface); `TestCommitMutation_FolderMoveKeepsHeldBaselinePointerCurrent` (`S/engine_baseline_snapshot_test.go:47`) still passes through the view; race test `S/cache_consistency_race_test.go:19`; benchmarks before/after recorded.

**B-2. No fallible cache step after commit; detector on.** In-transaction reload, post-commit swap; locked reads; `baselineEntriesEqual` covers every field; `CheckCacheConsistency` returns an error and joins `assertStoreIntegrity`. Depends on B-1.
- Tests: fault-seam tests (`S/store_fault_seam_test.go:246, 288`) extended with a fault on the read that used to follow the commit — `CommitMutation` returns an error **and** nothing was committed, or returns nil and cache equals database; every model, chaos and crash test now asserts cache = database through the invariant library.

**B-3. Distinct plan snapshot type; dry run seeded from the database.** Depends on B-1.
- Tests: `TestPlanning_UsesBaselineFromTheComparisonSnapshot` (`S/engine_baseline_snapshot_test.go:13`) adapted; a dry-run test with a deliberately stale cache that still plans from database truth.

**B-4. `decide` and `deriveFlags` in Go, SQL still deciding (oracle phase).** `decisionState`, `valid()`, `decide`; the comparison query widened to return all flag columns once. Depends on A-1, A-2.
- Tests: **exhaustive test over 8,388,608 states** (properties 1–3 of B4.2); **oracle test** `decide` = SQL `CASE` over the stage tables; an engine-level assertion in test builds that Go and SQL kinds agree on every row of the matrix, the planner model and the engine model; property test that `deriveFlags` only produces `valid()` states.

**B-5. Go decides; SQL `CASE`s deleted.** SQL reduced to the join and the set-valued CTEs; one query per plan. Depends on B-4.
- Tests: matrix golden byte-identical; planner model and engine model at a wide seed count; kind-literal SQL tests replaced by `decide` table tests; **`BenchmarkComparisonQueries` before/after at 10,000 and 100,000 items in the three shapes, recorded next to `D/performance-benchmarking.md:663–668`**.

**B-6. Simulation: time.** Fake `syncClock` in the engine model, the 11 direct sites routed, seeded jitter. Independent of B-1..B-5.
- Tests: `TestModel_SameSeedReplaysIdentically` under a frozen clock; a check (lint or test) that no wall-clock call remains outside `S/domain_clock.go`.

**B-7. Simulation: in-memory filesystem.** Capability flags; bypassing calls routed through the handle. Depends on B-6 only for ordering.
- Tests: engine model with no temporary directory; the fs-shim scenarios (`S/fs_shim_scenario_test.go`) re-run on the in-memory tree; an NFD scenario reproducing LI-20261029-01 on Linux.

**B-8. Simulation: scheduler seam and watch mode in the model.** One injected scheduler for the ten goroutine sites and all timers; fake watcher and Socket.IO sources. Depends on B-6, B-7.
- Tests: a watch-mode model test (edits arrive while actions are in flight) with the four invariants; replay equality for watch mode; a combined-fault seed (Graph chaos + store fault + errno + crash).

## Part C (last)

**C-1. `planIdentityKeyed` as a pure function, not wired.** Depends on B-5.
- Tests: unit tables per attribute delta; `matrix.identity.golden` for the 99 cells with every difference from `matrix.golden` explained in the PR.

**C-2. `diffPlans` and shadow runs in the models.** Depends on C-1 (and benefits from B-8).
- Tests: `diffPlans` unit tests; planner model and engine model report differences per seed; planner model driven by the shadow planner asserts convergence and no content loss; seed counts for both planners in the PR.

**C-3. Shadow in live watch on the test drive (optional).** Debug event per plan. Depends on C-2.
- Tests: sink unit test for the new event; benchmark with shadow on/off.

**C-4. Cut-over.** Identity planner becomes the planner; path pairing remains only as the low-confidence fallback; `remote_state` gains `parent_id` and loses `UNIQUE(path)`; twin/park paths deleted; generation bump. Depends on C-2 meeting the exit criterion.
- Tests: all of the above with the roles swapped for one release of the test suite (old planner as shadow), then the old planner and `diffPlans` are deleted; `BenchmarkComparisonQueries` recorded again.

---

# SUSPECTED items and what settles each

| # | Statement | Settled by |
|---|---|---|
| 1 | (`diverged`, local changed, remote unchanged, current equal) → `upload` is reachable and intended | a `sqlite_compare` test seeding `baseline.local_hash ≠ baseline.remote_hash` |
| 2 | A root-typed row can reach `baseline` or `local_state` | reading the scanner's and committer's row construction; or a CHECK without `'root'` under the model tests |
| 3 | Top-level baseline rows have a `parent_id` with no baseline row | the orphan-count query in `assertStoreIntegrity` under the model test |
| 4 | Each `RecordBlockedRetryWork` caller runs after its scope row exists | the immediate foreign key under the model and crash tests |
| 5 | `shouldKeepTimedBlockScope` is expressible in SQL | reading `S/store_scope_admin.go:110` and its helper |
| 6 | `shortcut_roots` references nothing enforceable | reading the `shortcut_root*` store files |
| 7 | `local_hash_cache` rows may outlive `local_state` rows | reading the hash cache's prune statement |
| 8 | modernc.org/sqlite follows standard SQLite foreign-key rules (A2.1) | the two pragma/violation tests in A-4 |
| 9 | The fault test at `S/store_fault_seam_test.go:246` does not cover a failure after `tx.Commit()` | reading which statement it fails |
| 10 | `createSide`'s silent default (`S/planner_move_composition.go:1496–1497`) is harmless | reading its callers |
| 11 | An existing issue type is missing from `observationIssueBlocksLocalTruth` | listing all `issue*` constants against `S/truth_status.go:113–122` |
| 12 | The model cannot combine store faults and errno shims with chaos and crashes | reading `runModelScenarioWith`'s options (`S/engine_model_based_test.go:286–345`) |
| 13 | Which executor boundaries the nine crash points cover | reading `S/crash_points.go:13–40` against the executor |
| 14 | The message a user sees for a mount stopped by a plan error, one-shot and watch | reading `M/orchestrator_watch.go:704` onward and the CLI's sync error path |
| 15 | A new `DebugEvent` type fits the sink | reading the `DebugEvent` type |

# Not read

- The `modernc.org/sqlite` module source.
- `S/store_scratch.go` beyond the lines cited; the `shortcut_root*` files; `S/planner_delete_safety.go` in detail; `S/crash_points.go` names one by one.
- Every SQL site that reads `retry_work.blocked`, and the definitions of the `retry_work` indexes (`S/schema.go:209–212`) and the non-key columns of `block_scopes` (`:227–229`), beyond what is cited.
- `M/orchestrator_watch.go` beyond `:478–509` and `:647–709`; the CLI's handling of a one-shot sync error.
- Bodies of most planner test files (counted by search, not read end to end); the content of `S/testdata/reconcile/matrix.golden`.
- `R/live-incidents.md` end to end (searched, and read at `:74–118` and `:2100–2123`); `D/system.md` (cited by candidate 7 for the `localpath` bypass).
- The requirement IDs (`R-X.Y.Z`) that new tests should carry.
- The number of call sites affected by value-returning baseline lookups.
- The count of `valid()` states in the 8,388,608-state product.
- Whether a lint rule already bans `time.Now` outside the clock file.
- Nothing was built, run or benchmarked; run times of the proposed exhaustive tests are unknown.
