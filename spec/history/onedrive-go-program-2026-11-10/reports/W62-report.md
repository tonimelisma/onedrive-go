# W62 report

Commit `0c9052dd` on `f82a5f84`, branch `test/e2e-helper-tests-untagged`, one commit, not pushed. 16 files.

## What changed
- `e2e/shared_items_types_test.go` (new, untagged): shared --json, stat and drive list JSON types, the resolved fixture types, shortcutFixtureKind and its constants.
- `e2e/shared_items_decisions_test.go` (new, untagged): listing matches, the provider-skip reasons and decisions, the shortcut root placeholder decisions and their 6 tests; shortcutFixtureProviderOmissionProofDelay (from `e2e_test.go`).
- `e2e/status_json_test.go` (new, untagged): the status JSON types (UnmarshalJSON with them), validateStatusJSONContract, the counts and their 2 tests.
- `e2e/fixture_command_failures_test.go` (new, untagged): the recurrence reasons (from `quirk_helpers_test.go`), isRetryableGraphGatewayFailure (from `e2e_test.go`), the put/mkdir failure classifiers and their 4 tests.
- `e2e/shared_items_helpers_selection_test.go`: build tag dropped; sharedFileCandidate, selectSharedFileFixture, sharedFileCandidatesShareRemoteIdentity moved in.
- `e2e/shared_items_helpers_test.go`, `status_helpers_test.go`, `sync_e2e_test.go`, `quirk_helpers_test.go`, `e2e_test.go`: the declarations above cut out. Pure move: every added line was a removed line, imports and blank lines aside (`w62/purecheck.py`).
- `e2e/printed_value_rule_test.go`: t.Run's name is a printing call; a testutil function hands on its arguments and an Error() method its receiver; header updated, with one paragraph naming the hand-masked sites the rule cannot see and why each group is accepted; constant errorMethodName.
- `e2e/printed_value_flow_test.go`: planted rows for the three extensions.
- `e2e/sync_e2e_test.go` copyTokenFileForDrive, `e2e/testenv_test.go` copyRotatedTokenBack: four raw prints of token paths (the account's name) the extension found now print the role or the masked text.
- `testutil/idmask_source_test.go` (new): 9 tests holding the masks to the product source. `testutil/idmask.go`: key `ctag` removed.
- `spec/design/system.md` line 657: one sentence naming the mask source tests (every name checked).

## Tests added (each `Validates: R-6.10.27`, each seen failing on a reworded product text, the text then restored)
- TestSelectorMaskFollowsTheProductsSelector: sharedref.Ref.String masked (":" to "/" failed it).
- TestStateFileNameMaskFollowsTheProductsStatePath: DriveStatePathForDataDir's replace, prefix, suffix and driveid's shared type, read from source (":"->"-" failed it).
- TestRouteMaskFollowsEveryProductRoute: every route literal of internal/graph rendered with invented IDs, masked in text, JSON and URL form ("/sites/" -> "/site/" failed it).
- TestIdentifierKeysFollowTheProductsKeys: every key isIdentifierKey names (read from idmask.go) is a product JSON tag or slog key (owner_name renamed failed it; `ctag` failed it before its removal).
- TestThrottleKeyMaskFollowsTheProductsScopeKey: wireThrottleTarget and throttleDrivePrefix from source ("drv:" failed it).
- TestStatIDLineMaskFollowsStatsOutput: printStatText's ID format ("Item ID:" failed it).
- TestSharedByMaskFollowsTheProductsLabels: the shared-by labels of three functions, counted (one reworded label failed it).
- TestRestoreArgumentMaskFollowsTheProductsCommand: the Use strings of recycle-bin and restore ("undelete" failed it).
- TestShareTokenFollowsTheProductsEncoding: graphSharingURLPrefix and the encoding expression ("s!" failed it).
- Planted rows in TestPrintedValueRuleFollowsValues: t.Run(drive), an os error of a TokenFileName path, its Error() text; each row missing with its extension switched off.

## Moves: 15 moved, 20 stayed
Stayed because they read TestMain state or the account: timing (3, suiteDriveRoles through redactTimingEvent/debugLogFileName/appendDebugLogChunk; TestE2ERecordersRecordRolesNotDrives also reads `drive`), quirk (6, suiteDriveRoles, suiteQuirkRecorder), TestResolveDriveSelection (`drive`), TestIsolation_* (9, isolation globals, env, `drive`, makeCmd).

## Blind-spot sites (54 found by unwrapping each of 223 masker calls, `w62/overlay/blind_test.go`)
- Rule extension (12 sites now seen): t.Run name (auth_preflight_e2e_test.go:37); testutil call (testenv_test.go tokenPath x3, copyFixtureFile src/dst x3); Error() (copyFixtureFile x3, testenv_test.go:61, requireNoLocalError).
- Accepted in the header (42): decoded/returned values (status issues, mounts, poll state, CandidateDriveIDs, helper errors), message builders, assertSamePath paths, product/CLI errors, artifact writes (held by TestE2ERecordersRecordRolesNotDrives).

## Checks
build ./... ok; vet ./..., -tags=e2e, e2e,e2e_full, e2e,e2e_full,integration,stress ok; gofumpt/goimports clean; go test ./e2e/ ./testutil/... ok, the 15 moved names pass in -v and -short runs; same with -race behind the lock ok; RealRepoIsClean 4 PASS; lint ./e2e/... ./testutil/... and --build-tags e2e,e2e_full ./e2e/... 0 issues (W62-lint.log; a first run had 3, fixed). Print rule over the moved files read as tagged: 0 findings. No live, tagged or integration test run.

## Deviations
- The plan's example TestE2ERecordersRecordRolesNotDrives cannot move: it reads `drive`.
- `ctag` removed from isIdentifierKey: no product key spells it (Graph's is `cTag`, never matched).
- Third rule extension (Error() receiver), and four live sites fixed that the extensions found.

## Seen and not fixed
- internal/cli/shared_discovery.go:420-424: shared display names "<name>'s <folder> (<email>)" print a sharer's name and address; no mask follows them (unlearned sharers stay readable). Fix: a mask for the display-name forms, held like the shared-by labels.
- testutil/idmask.go:70: route segment `users` follows no product route (maybe Graph's nextLinks). Fix: drop it, or hold it to a recorded nextLink.
- e2e/timing_helpers_test.go:345,372,409 and quirk_helpers_test.go:433-555: the recorders redact with the suite-wide roles. Fix: give newE2ETimingRecorder/newE2EQuirkRecorder the roles, then 8 tests can move.
- e2e/printed_value_rule_test.go printedValueClass: plural identity names (driveIDs, CandidateDriveIDs()) are not classed; adding "Drives" flags counts (TotalDrives). Fix: plural IDs/Emails only, checked on the live suite.
- e2e/printed_value_rule_test.go: a suite function's result is not followed (30 accepted sites). Fix: result classes to a fixed point, with an exemption for counts.

## Packages for the reviewer
None beyond e2e and testutil; the tagged e2e jobs compile the new untagged files (vet with tags ok).

## Review fixes
Commit `86a834a7` on `41919e44` (amended, one commit, 18 files).

- Finding 1: `e2e/printed_value_rule_test.go:172` scannedFuncs (live functions, then each non-Test function of another file a live one reaches, via new `reachedFrom` at :803); findPrintedValues uses it; header says so. Planted row: an untagged helper a live test calls (:859), and the unreached one still passes. Proof ("To run" 3, RecipientEmail Errorf in selectSharedFileFixture): before PASS, after FAIL. The wider scan also found two raw prints of a decoded time in untagged helpers live code calls, `e2e/remote_read_rules_test.go:416` and `:451` (they quoted the text, and so does time.Parse's error): they now print its length; new TestTimeFieldReadersPrintNoOutput (:476, seen failing on the old print).
- Finding 2: `testutil/idmask_source_test.go:348` counts the writers of each key and holds them in one map (id 26, email 6, org_name 2, site_name 3, library_name 3, the rest 1); split into two literal-led patterns (one alternation took 12 s). Proof ("To run" 2, site_name renamed in drive_search.go only): before PASS, after FAIL.
- Finding 3: `testutil/idmask_source_test.go:235` renderRoute returns an error on a verb other than %s or %d; the test requires none. Proof ("To run" 1, "/groups/%v/drive" in internal/graph): before PASS, after FAIL.
- Finding 4: `e2e/auth_preflight_e2e_test.go:49` requireNoLocalError with the role; the header's product-call group names a drive's token source.
- Note 5: sharerLabels (:408) returns masked labels and the format of every other sharer label; SharedByName joins the sharer fields; the test holds deriveSharedDisplayName's two display-name formats (:470) and none elsewhere.
- Item iv: names ending in IDs, Emails, Selectors or Links are identities (:776), not Drives; planted row (:853, four values, a TotalDrives count not flagged). Live sites it fails: 0, so 0 added to the accepted list.
- Note 8: all nine new tests carry the Validates line directly before func.
- system.md:665 now says "a key renamed at any one of its writers"; the commit message is updated to match.

Checks: build, vet (untagged and all tags), gofumpt, goimports clean; go test ./e2e/ ./testutil/... ok, with -race ok; RealRepoIsClean 4 PASS; lint 0 issues (W62-lint.log; a first run had 2 dupl, fixed) and tagged 0 issues (W62-lint-tagged.log). Proof files restored; status shows only .testdata.

Not done: nothing. Seen, not fixed: the first-name display form (internal/cli/shared_discovery.go:420) is built from a variable, so sharerLabels does not see it, and its format equals that of :421.


## Merged as #936

- Merged 2026-11-16 06:20 PST as `ca287a32` (squash); installed 06:26.
- The tagged lint and the pre-PR gate passed on `85f97d06` (base
  `e248aae5`); the PR's CI was green; pre-merge passed on its first
  run. The gate waited about 70 minutes for the heavy lock behind one
  builder's whole-package race run of `internal/sync`.
- No live run: test code and one spec sentence only.
- Opened by this merge: W83, W84, W85 and W71 (W86 after W85).
