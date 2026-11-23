# A7 Definition of Ready: fakes answer with the faults seen live

1. PASS **Task intent**: repo-maintenance outcome. The fakes that unit and
   engine tests run against can answer with the faults seen against the live
   service, and each fault is exercised by a test: Graph HTTP fake
   (`sessionUploadRecorder`) refuses a create (`createStatus`) and lets a
   session request land and lose its answer (`sessionLandsThenDrops`); the
   driveops creating uploader scripts placeholder-removal results
   (`removeResults`); the scenario remote refuses creates (`refuseCreate`),
   hides an item from path reads and listings (`invisibleFor`) and serves
   resumable sessions (large-file path); the chaos layer's
   timeout-after-create can move the placeholder's eTag (`churnPlaceholder`).
2. PASS **Non-goals**: any product (non-test) change, including a fix for a
   defect a new fault exposes (reported instead); e2e tests; CI; spec text
   beyond what the repository rules require; the chaos `create:response-lost`
   fault (#871, reused, not duplicated); delta lag (a different family,
   LI-20260405-04); session faults inside the chaos storm.
3. PASS **Governing docs**: repo CLAUDE.md lines 55-236; analysis
   01b-product-and-e2e.md Item 4 (lines 462-532) and PR rows 8 and 10;
   spec/design/drive-transfers.md "New files and their placeholder";
   spec/design/sync-execution.md lines 168-215 (pending creates, 409
   resolution); spec/design/sync-engine.md (D-092 chaos, model tiers);
   spec/requirements/transfers.md R-5.2.1, R-5.2.4, R-5.6.14, R-5.6.15;
   spec/requirements/sync.md R-2.10.71, R-2.11.7.
4. PASS **Authority owner**: no product authority changes. Test fakes own
   only their simulated OneDrive state: `moveScenarioRemote` (items, eTags,
   sessions), `chaosGraph` (fault streams), `sessionUploadRecorder` (scripted
   HTTP answers), `tmCreatingUploader` (scripted removal answers). Truth,
   mutation and output stay with graph.Client, driveops.TransferManager and
   the sync engine, which the tests observe.
5. PASS **Code scope**: `_test.go` files only: internal/graph
   (upload_test.go, item_create_test.go), internal/driveops
   (transfer_create_test.go), internal/sync (scenario_remote_test.go, a new
   scenario_remote_session_test.go, chaos_remote_test.go,
   engine_model_based_test.go, pending_creates_test.go). Must not touch any
   non-test file under internal/, e2e/, CI.
6. PASS **Regression plan**: new tests
   `TestUpload_RefusedCreateSendsNoContentAndDeletesNothing`,
   `TestUpload_SessionRequestThatLandsWithoutAnAnswerStillRemovesTheEmptyItem`,
   `TestUploadFile_FailedPlaceholderRemovalKeepsTheUploadsOwnError`,
   `TestRunOnce_RefusedCreateWaitsOnTheDurableCurveAndClearsItsIntent`,
   `TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt`,
   `TestRunOnce_LargeFileWhoseSessionRequestFailedFinishesIntoItsPlaceholder`,
   `TestChaosGraph_TimeoutAfterCreateCanMoveThePlaceholderETag`; the chaos
   storm of `TestModel_ChaosGraphConverges` gains the churn variant.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`; the unit
   tests of internal/graph and internal/driveops with `-race`; internal/sync
   with `-run` patterns for the touched tests only, with `-race` (plus one
   `TestModel_ChaosGraphConverges` run); targeted golangci-lint of the three
   package trees, one at a time; full `devtool verify` is the reviewer's.
8. PASS **Data safety**: test-only; fakes run in memory and on t.TempDir().
   No local or remote deletes, no state DB outside test temp dirs.
9. PASS **Secret and credential safety**: no tokens, `.env`, account names
   or credential-bearing logs involved.
10. PASS **Live provider and destructive operations**: none. No live call,
    no state reset, no dependency change, no history rewrite.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: what does `sessionLandsThenDrops` land: the session request
      or a fragment's content? Current assumption: the session request (the
      analysis and the 2026-11-10 sequence, LI-20261110-01); content that
      landed without its answer is the final-fragment path already covered
      (`TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain`). Validation
      step: reviewer reads the report's mapping. Owner affected if wrong:
      internal/graph/upload_test.go.
    - Question: which reads does `invisibleFor` hide from? Current
      assumption: path reads and child listings only (analysis), not delta or
      reads by ID. Validation step: engine test exercises the 409 listing.
      Owner affected if wrong: internal/sync/scenario_remote_test.go.
    - Question: can the session uploader live on `engineMockClient` itself?
      Current assumption: no; a method set cannot be optional, and every
      engine test with a file above 4 MiB would change route. An opt-in
      wrapper type carries it. Validation step: existing tests unchanged.
      Owner affected if wrong: internal/sync/engine_test_helpers_test.go.
    - Question: does any new fault expose a product defect? Current
      assumption: unknown until run; if one does, the test states current
      behaviour and the report marks the defect. Owner affected: the product
      file the report names.
