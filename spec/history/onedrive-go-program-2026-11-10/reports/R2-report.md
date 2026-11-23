# R2 report: names (G1, G2, G8, G3, G12)

Branch `refactor/names-one-rule`, one commit `995c1e19` on origin/main `a5af3b04` (not based on R1: no R1 code needed). Pushed; no PR. 54 files, +1543/-436. **Store generation: unchanged** (no schema, store or config change).

## G1 typed keys
- `namekey.Key` type + `Of` (old `Key` func body, no alias); `PathKey` + `OfPath` (per segment) + `HasPrefix` (at or below on a segment boundary). Empty key is NOT a universal root: it is the key of an all-ignored name (emoji alone); the old planner prefix test already meant that, and `isWithinRemotePath` keeps its own `CleanRemotePath(root)=="" => true` drive-root rule. Uncleaned paths compare as written (callers clean).
- Retyped: `DirNameKey.NameKey`, `caseGroupKey.nameKey`, `DirNameCache` (map[string]map[namekey.Key][]string, 6 files), planner `movesByOldNameKey`/`vacatedByNameKey`, move composition `ownLandings`, `foldedByPath`, `foldedOccupied`, `byTarget`, `takenPathsFolded`, `atOrBelowOwnLanding`; `pathAncestors` made generic over `~string`.
- Deleted `pathNameKey` (`foldMovePath` no longer existed on main); `movesIntoVacatedPath` uses `belowPathKey` (exactly the old `HasPrefix(k, root+"/")`). `isWithinRemotePath` kept (two callers, does cleaning), body is `OfPath(...).HasPrefix(...)`; its old test passes unchanged.
- P5 copy found: `exRelated` in planner_exhaustive_test.go (hand prefix test) now `HasPrefix`; `untouchedDeletes` no longer mixes keys and paths in one map.
- `TestKey_*` renamed `TestOf_*` (2; cited in system.md, updated; not in live-incidents). `TestPathNameKey_EqualPathsShareAKey` moved to namekey as `TestOfPath_EqualPathsShareAKey`. New `TestOfPath_HasPrefix_SegmentBoundary`, `TestOfPath_IsOfPerSegment`.
- R-2.8.12 renumbering: already done (one `- R-2.8.12:` line in spec/requirements/sync.md).
- Remaining `string(key)`: only inside namekey (HasPrefix, OfPath).

## G2 one lookup rule
- `namekey.Stored`, `Relation` (Unrelated/Twin/SameName/SameStored/Identical; no fifth case: two booleans + identity cover every pair, property test proves it), `Relate`, `Match` (+`Found`), `Find`, `FindStored`.
- Matchers moved: `findCreatedFolder` (graph), `listedChildNamed` (graph, via new `graph.NameSeek`), driveops `nameOnPage` (deleted, replaced by `graph.NameSeek`; `matchChildByName` no longer exists on main: the matcher is `FindExistingChild`/`findInParentListing`), put's lookup (via `FindExistingChild`), synctree `uniqueEquivalentEntry` (FindStored; synctree no longer imports norm). New `graph.WithoutRepeats`: listings do not dedupe by ID, so `NameSeek` and `findCreatedFolder` (and G3) count a repeated item once.
- `FindExistingChild` now returns `ChildByName{Item, Ambiguous}`; callers take their not-found path on Ambiguous, and their message gains ": more than one item in the folder holds that name" only then (one-candidate texts unchanged; an earlier draft changed EnsureFolder's text and `TestEnsureFolder_ConflictNotFound` caught it). put returns the 409.
- Inputs whose answer changed: two key-equal candidates without the exact spelling (was: first/last listed; now: none); exact spelling after two folded folders in findCreatedFolder (was missed); one SameStored beside one SameName in findCreatedFolder (was ambiguous; now SameStored); repeated items count once.
- Tests: `TestRelate_ProbeCorpus`, `TestOf_StoredComposition_KelvinClass`, `TestFind_TieBreak`, `TestFindStored_TwoEntriesAreAmbiguous`, `TestFindExistingChild_TwoFoldedCandidatesIsNotFound` (deviation: named for the existing function, not the gone `matchChildByName`), `TestReadCreatedFileByName_PrefersExactSpelling`, `TestLookupUploadTarget_PrefersExactSpelling`, `TestPutFile_PrefersExactSpellingOverCaseVariant`, `TestFindCreatedFolder_TwoFoldedFoldersAreNotFound`, `TestWithoutRepeats_KeepsEachIDOnce`. Graph two tests seen failing against old product code (stub for WithoutRepeats); put/driveops ones fail on old code by construction (old picked the first spelling).
- Live proof commands for the caller (one-candidate decides): `put` over an existing file by exact and by case-variant spelling; `stat`/`get` of a path by case-variant spelling; `mkdir` of an existing folder by case variant (EnsureFolder).

## G8 one stored form
- `namekey.StoredPath` (NFC over the whole string) with `TestStoredPath_EqualsPerSegment` (~1,100 paths: both probe corpora joined both ways plus combining-mark/slash/empty-segment cases): passes.
- `nfcNormalize` deleted; sites replaced: single name 21 (engine_shortcut_release 3, scanner 1, observer_local_handlers 5, shortcut_root_lifecycle 1, executor_transfer 1, executor_preconditions 2, observer_local_collisions 6, symlink_observation 3, item_converter 3 — counted by edit), joined path 2 (executor_preconditions ParentPath compare and path), the executor matchers went to Find/FindStored (G3). Inline local-path copies replaced by `storedPathOf`: 3 (scanner x2, observer_local_handlers x1). `storedPathOf` moved from permission_probe_local.go (sync-engine, LAYER 4) to core_types.go (sync-domain, LAYER 0) so observation files do not read up the layers.
- Comparison sites left as they are for later (written with namekey.Stored): observer_local_handlers `localPathStillPresent`-style loop (~line 1246), observer_local_collisions (4), executor_transfer `placeholderIsAtCreate`, executor_preconditions (2).
- Guard: depguard rule `norm-only-namekey` in .golangci.yml (namekey and the synctest shim allowed, tests exempt). Proved: with main's spelling.go restored temporarily, `golangci-lint --enable-only depguard ./internal/synctree/` reports the import; tree restored (`git status` clean).
- Production files importing norm now: internal/namekey/namekey.go, internal/synctest/fsshim.go only.

## G3 create conflicts never adopt a twin
- `classifyCreateConflict(children, name) (*graph.Item, createConflictFound)` in executor_transfer.go: Nothing / Wanted (Identical, SameStored) / Rival (SameName); no I/O; ID-less children are no candidates; repeats removed by ID. Two rivals -> Rival naming the first listed; two of the wanted tier -> Nothing.
- `adoptExistingRemoteFolder` and `resolveCreateConflict` use it (pending-create branch untouched; five cases hold for one candidate). `storedNameSpelling` uses Find (ambiguous -> plain error naming the folder: classified as an ordinary local failure, resultSkip with persisted retry work, so the user sees it with backoff; not errActionPreconditionChanged, which would be a silent replan every pass). `exactLocalNameExists` uses FindStored, keeps "exists" on two entries (the caller then refuses the remote delete: safe).
- Changed outcomes: folder create beside a twin alone (was adopted; now stale); twin + key-equal sibling (was adopted twin; now name taken naming the sibling); wanted name + SameName listed first (was rival; now wanted); folder create with a file of the wanted name + folder of another spelling (was name taken; now stale); respell with two key-equal entries none exact (was rename of the last listed; now error).
- Tests: `TestClassifyCreateConflict_NeverAdoptsATwin` (12 rows), `TestExecutor_CreateRemoteFolder_ConflictBesideATwinAdoptsNothing`, `TestExecutor_StoredNameSpelling_TwoKeyEqualEntriesAreAnError`, `TestExecutor_ExactLocalNameExists_TwoEntriesOfOneStoredForm` (both run on this host's ext4, not skipped).

## G12 the 255-byte name check deleted
- `maxComponentLength` and the check deleted from `validateOneDriveNameFor`. Callers: only `validateObservedName` <- `shouldObserveWithFilter`, which always runs the path check after it, so DoR (iii): every validated name is path-checked; (iv) a 380-unit root name: was invalid_filename "longer than 255 characters", now passes locally and the server's refusal maps to path_too_long.
- Other length sites (no change): driveops `maxLocalNameBytes` (download, R-2.11.8), sync executor_conflict `maxLocalNameBytes` (R-2.11.11), planner `moveNameMaxBytes`, cli `maxRemoteNameBytes` (parked comment).
- New R-2.11.12; new probe row L-36 (numbers from the brief verbatim, with the not-measured list); L-05 gained a pointer; graph-api-quirks clause corrected; comments on `maxOneDrivePathLength` and in `classifyServerAnswer` cite L-36 and no longer claim to know why 365.
- Tests: `TestValidateOneDriveName_MaxLength` replaced by `TestValidateOneDriveName_NoLimitOnASingleName` (3 rows, all fail before); `TestShouldObserve_RootLevelNameMeetsOnlyThePathLimit` (4 rows: 400/401 ASCII, 400 BMP, 401 with emoji); the "too long" row of `TestValidateOneDriveName` flipped to valid (the validator's own test). Old name was cited nowhere in spec.

## Docs
system.md (namekey: keys, stored form, relation, lookups, depguard rule), graph-client.md, drive-transfers.md (two places, incl. a stale "until the WS2 namespace key replaces it"), sync-execution.md (classifyCreateConflict, folder adoption, respell ambiguity), sync-observation.md, requirements R-2.12.3 (one lookup rule) and R-2.11.12, live-probes L-36, graph-api-quirks. Hand check: every cited test exists once, every cited requirement exists, no old name (TestKey_, pathNameKey, nfcNormalize, nameOnPage, maxComponentLength, TestValidateOneDriveName_MaxLength) left in spec/.

## Checks
- go build ./...: pass. go vet ./...: pass. go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...: pass.
- go test -count=1 namekey, graph, driveops, synctree, cli, devtool, syncverify (behind lock): all pass after the EnsureFolder text fix (first run failed TestEnsureFolder_ConflictNotFound; driveops rerun pass).
- go test -count=1 ./internal/sync/... (behind lock): first run hit the default 10m deadline in TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge (a long test on 2 cores); rerun with -timeout 90m: pass (1256 s).
- go run ./cmd/devtool verify lint: first run 9 findings (goconst, goimports local prefix, misspell, testifylint, all in tests), fixed; second run pass. Log: reports/R2-lint.log.
- No -race run (ship step).

## PARKED lines added
- R2-p1: `maxRemoteNameBytes` comment in internal/cli/replace_destination.go claims a OneDrive 255-byte practical limit; L-36 found none.

## Notes
- `.testdata` (the devtool symlink) shows as untracked in the worktree; not committed.
