# onedrive-go improvement program: plan

Written 2026-11-10 against onedrive-go `70301c0e` (`main`). This is the working
plan for the ten-increment program the owner approved on 2026-11-10 ("Do all of
this"). It is a working document: when the program closes, what is still useful
moves to `SYSTEM.md` and `CHANGELOG.md` and this directory is deleted.

- Evidence for each item is in `analysis/` (one report per area, each written
  read-only at `70301c0e`). A report's claim is checked against the code again
  when its PR is implemented; reports carry no authority of their own, and
  several of their statements are marked SUSPECTED by the report itself.
- The split of `internal/sync` has its own design and evidence in
  `../onedrive-go-sync-split-2026-11-10/`.
- The plan was completed on 2026-11-10 after all ten reports were read. The
  reports corrected the program in four places; see section 3 (D1, D17, D21)
  and the notes on waves C and G.

## 1. Status

Updated as each PR merges. "PR" is the onedrive-go pull request number. Lane:
`s` is the sync lane, `o` the outer lane, `x` crosses both and runs alone.

| ID | Title | Branch | Lane | State | PR | Merged as |
|---|---|---|---|---|---|---|
| A1 | Empty file left by a failed upload is removed; PauseResume waits for content | `fix/upload-placeholder-retry` | s | merged | 860 | `fc21d1aa` |
| A2 | devtool gates that work on a 2-core host; CI wait that survives a cancelled shard | `fix/devtool-host-gates` | o | merged | 863 | `90c60a8f` |
| A3 | Created-item record survives failure and shutdown; clean shutdown is quiet | `fix/created-item-record` | s | merged | 864 | `68ca8f3b` |
| A4 | Mount perf collectors receive context-recorded metrics | `fix/mount-perf-collector` | s | merged | 862 | `1c2cd155` |
| A5 | e2e waits check content, not names | `test/e2e-content-waits` | s | merged | 867 | `31fd2a59` |
| A6 | e2e single-pass syncs use the converge helper | `test/e2e-converge` | s | merged | 877 | `835daf61` |
| A7 | Fakes answer with the faults seen live | `test/fake-graph-live-faults` | s | merged | 879 | `640e27ac` |
| A8 | One coverage number; stress lane covers the sync tree | `ci/one-coverage-number` | o | merged | 872 | `cd11f79c` |
| A9 | Every e2e bucket runs; gate riders | `ci/e2e-every-bucket` | o | merged | 878 | `34fb1a66` |
| A10 | Nightly: sound verdicts, visible result | `ci/nightly-report` | o | merged 907 2e465121 |  |  |
| A11 | Race on every push; nightly seed rotation | `ci/race-and-seeds` | o | merged 917 04983d12 |  |  |
| A12 | Lint moves; devtool imports no product package | `chore/lint-moves` | o | merged 946 4886b38c |  | **Rider (2026-11-14, from W43's report and its review, note 12)**: `internal/devtool/bench_live.go` reads a cleanup's missing item from the words "not found" anywhere in a command's stderr (`isRemoteNotFoundCleanup`, used near lines 433 and 541), where a retried 404 in a debug log can stand before a different failure. By reading only: the DoR says whether the benchmark's commands can print such a line and shows the misreading on the unchanged tree with a unit test. Fix: match the CLI's own error line for the item, the rule of the e2e suite's `cliReportsRemoteItemMissing` (`e2e/remote_read_rules_test.go`; the rule is copied, the e2e package is not imported). |
| A13 | e2e buckets derived from test names | `test/e2e-family-buckets` | x | merged | 943 | `238bea94` |
| A14 | A create whose answer was lost keeps its claim | `fix/create-answer-lost` | s | merged | 871 | `3e4c21a7` |
| A15 | A stale precondition is superseded, whatever status it carried | `fix/stale-precondition-first` | s | merged | 881 | `e686ae2e` |
| A16 | A create intent nobody will retry is dropped; expired intents are pruned | `fix/orphan-create-intent` | s | merged | 886 | `27e76cc3` |
| A17 | A failed verify step prints one marker line, and the nightly keys the step | `ci/step-failure-marker` | o | merged 930 c0792e98 |  |  |
| A18 | The short tier's go test deadline scales with the cores | `ci/short-tier-deadline-by-cores` | o | merged 902 83fe1f82 |  |  |
| A19 | The linter's deadline scales with the cores | `ci/lint-timeout-by-cores` | o | merged 904 7d7bc06b |  |  |
| A20 | The stress lane's race runs of the sync tree take their deadline from the race profile | `ci/stress-race-deadline` | o | merged 922 b8b72df8 |  |  |
| A21 | The stress lane's fifty-times step has declared members and a budget | `ci/stress-x50-members` | o | merged 939 52053271 |  |  |
| A22 | The nightly issue reads each red job's log, and a go test alarm keys only the test it names | `ci/nightly-reads-logs-and-alarms` | o | parked |  |  |
| B1 | Split `orchestrator_watch.go` (pure moves) | `refactor/split-orchestrator-watch` | s | merged | 869 | `472e50ba` |
| B2 | One-shot `sync` exits 1 when work is left | `feat/sync-exit-left-work` | x | merged 898 8d6f7d46 |  |  |
| B3 | Control socket: deadlines and honest client messages | `fix/control-socket-deadlines` | s | merged | 876 | `05336a1a` |
| B4 | Config and catalog locks: bounded wait, catalog updates locked | `fix/config-lock-bounded` | o | merged | 870 | `06a217d7` |
| B5 | One token source per sign-in file, on a tuned HTTP client | `fix/auth-bootstrap-client` | o | merged | 873 | `56a396e2` |
| B6 | oauth2 fork removed; a failed token save is an error | `fix/token-persist` | o | merged 957 ed2f2441 |  |  |
| B7 | Token refresh coordinated across processes | `fix/token-refresh-lock` | o | merged 957 ed2f2441 |  |  |
| B8 | Watch runners: identity, bounded stops, fail closed | `fix/watch-runner-stop` | s | merged | 882 | `af259a57` |
| B9 | Panic boundaries everywhere | `fix/panic-boundaries` | x | merged 897 b8991f4b | 897 | `22e88c0b` |
| B10 | Loop heartbeat (observe only) | `feat/loop-heartbeat` | s | merged | 966 | `93271278` |
| B11 | Watchdog ping follows the heartbeat | `feat/watchdog-follows-loop` | s | gated on a soak (D14) | | |
| C1 | Planner kinds are typed; an unclassified row fails the plan | `fix/planner-fail-closed` | s | merged | 865 | `b02f4acc` |
| C2 | Observation issue types are typed | `refactor/typed-issue-types` | s | merged | 875 | `3618a8aa` |
| C4 | An interrupted removal's condition clears when the leftover is gone | `fix/interrupted-removal-clears` | s | merged | 880 | `aed97bce` |
| E1 | Dead code, stale comments, guarded protected-roots read | `fix/engine-guarded-read` | s | merged 908 fa601d30 |  |  |
| E2 | Observers: start order and lifetime | `fix/observer-start-lifetime` | s | merged | 955 | `14906f0a` |
| E3 | Vestigial locks and atomics deleted | `refactor/engine-vestigial-locks` | s | merged | 955 | `14906f0a` |
| E4 | Retry work has one write path | `refactor/retry-work-write-path` | s | merged | 955 | `14906f0a` |
| C3 | Foreign key and CHECKs in the store (generation 29) | `feat/store-foreign-key` | s | merged | 965 | `30c8b3e6` |
| G1 | Typed name keys | `refactor/namekey-typed` | x | merged | 956 | `97f45626` |
| G2 | One sibling-lookup rule | `refactor/namekey-find` | o | merged | 956 | `97f45626` |
| G3 | Create conflicts never adopt a twin | `fix/create-conflict-classify` | s | merged | 956 | `97f45626` |
| G4 | Twin of a synced file is a collision; watch deletes remove the subtree | `fix/local-twin-subtree-delete` | s | merged 958 f2250310 |  |  |
| G5 | Dead local event channel deleted | `refactor/local-events-dead` | s | merged 960 9b16a5c0 |  |  |
| G6 | Local observation is one sum type that carries its findings | `refactor/local-observation-type` | s | merged 958 f2250310 |  |  |
| G7 | Remote changes without `changeEvent` | `refactor/remote-change-type` | s | merged 960 9b16a5c0 |  |  |
| G8 | One stored-form function | `refactor/namekey-stored` | x | merged | 956 | `97f45626` |
| S1 | Split: export identifiers | `refactor/sync-split-exports` | s | merged 961 197f4d24 |  |  |
| S2 | Split: declarations to their target files | `refactor/sync-split-files` | s | merged 961 197f4d24 |  |  |
| S3 | Split: fields, methods, interfaces, errors | `refactor/sync-split-seams` | s | merged 961 197f4d24 |  |  |
| S4 | Split: tests and helpers | `refactor/sync-split-tests` | s | merged 961 197f4d24 |  |  |
| S5 | Split: the move | `refactor/sync-split-move` | x | merged 962 f0ac23e5 |  |  |
| S6 | Split: layering by depguard; un-export | `refactor/sync-split-cleanup` | x | merged 962 f0ac23e5 |  |  |
| E5 | Watch phase table | `refactor/watch-phase-table` | s | merged | 966 | `93271278` |
| E6 | Engine test hooks in one struct | `refactor/engine-hooks` | s | merged | 963 | `f2392533` |
| E7 | The flow reads its inputs, not the engine | `refactor/flow-inputs` | s | merged | 963 | `f2392533` |
| E8 | `protectedRootSet` and `housekeeper` | `refactor/engine-owners` | s | merged | 963 | `f2392533` |
| E9 | `observation` type | `refactor/engine-observation` | s | merged | 966 | `93271278` |
| E10 | `shortcutRoots`, start-up inversion, back-pointer deleted | `refactor/engine-no-backpointer` | s | planned | | |
| E11 | Per-package hook structs | `refactor/package-hooks` | s | merged | 963 | `f2392533` |
| E12 | Scheduler package | `refactor/syncsched` | s | planned | | |
| E13 | Executor and worker-pool constructors | `refactor/executor-constructors` | s | merged | 963 | `f2392533` |
| F1 | Baseline is read-only outside the store | `refactor/baseline-view` | s | merged | 965 | `30c8b3e6` |
| F2 | No fallible cache step after commit; plan snapshot type; dry run reads the database | `fix/baseline-cache-commit` | s | merged | 965 | `30c8b3e6` |
| F3 | `decide` in Go, checked against the SQL | `feat/planner-decide-oracle` | s | merged | 965 | `30c8b3e6` |
| F4 | Go decides; the SQL `CASE`s are deleted | `feat/planner-decide-go` | s | merged | 965 | `30c8b3e6` |
| F5 | Simulation: time | `test/sim-clock` | s | merged | 966 | `93271278` |
| F6 | Simulation: in-memory filesystem | `test/sim-filesystem` | s | planned | | |
| F7 | Simulation: scheduler seam and watch mode | `test/sim-watch` | s | planned | | |
| G9 | One local collision detector | `refactor/one-collision-detector` | s | merged | 966 | `93271278` |
| G10 | Scan reuses committed hashes | `perf/scan-hash-reuse` | s | dropped |  |  |
| G11 | Watch-side collision detection deleted | `refactor/watch-collisions-gone` | s | merged | 966 | `93271278` |
| G12 | Single-name length limit | `fix/name-length-limit` | s | merged | 956 | `97f45626` |
| H1 | `fsroot` mutations through `os.Root` | `fix/fsroot-os-root` | o | merged | 874 | `43ceff31` |
| H2 | One Retry-After parser | `fix/retry-after-parser` | o | merged | 866 | `e85498a9` |
| H3 | A request body that cannot be rewound fails loudly | `fix/rewind-body-loud` | o | merged | 868 | `89616d61` |
| H4 | `drive add` is one transaction in `config` | `refactor/config-add-drive` | o | merged 896 21b62d3e | 896 | `b073989e` |
| H5 | Upload-session shim and dead code deleted | `chore/outer-dead-code` | o | merged 899 bb0d76fd |  |  |
| H6 | One JSON encoder | `refactor/cli-json-encoder` | o | merged 909 dd0932f4 |  |  |
| H7 | Recursive `get`/`put` engines in `driveops` | `refactor/tree-transfers` | o | merged 918 341d9820 |  |  |
| H8 | Remaining silent failures; output leftovers | `fix/outer-silent-failures` | o | merged 947 b00e6da3 |  | **Rider (2026-11-14, from H7's report, "Not fixed" 4)**: `internal/cli/format.go` drops the write error in `writeWarningf` (near `:31-35`), `Progressf` (near `:307-309`) and `ProgressDone` (near `:326-328`). Fix: each records its failure through `recordStatusError`, called outside `statusMu`; one test per function with a writer that fails. **Rider (2026-11-15, from the review of H7's second version, note 16)**: some texts the CLI prints write a count and a plural noun straight from a format (`1 files`, `1 items`) where `countOf` and `countItems` in `internal/cli/format.go` exist for it. Sites by reading (the DoR confirms each and searches for more): `internal/cli/trash.go` near `:258` (test `trash_test.go` near `:75`), `drive_rebind_root.go` near `:125` (tests near `:60` and `:161` of its test file), `recycle_bin_flow.go` near `:123` and `:135`, `sync_notify.go` near `:140`, `:166` and `:172`. Fix: every count the CLI prints goes through `countOf` or `countItems`; a source check fails on a format in non-test `internal/cli` that holds a number verb directly before a plural noun, with its exceptions named in the check. **Rider (2026-11-15, seen after the install of #918)**: the summary of a one-shot sync counted folders as files: a confirm run that removed a tree of files and folders printed `deleted 215 files locally` for 215 items. The site is not found yet; the DoR finds it and decides whether the summary says items or counts files and folders apart. **Rider (2026-11-16, from the check of every listed site against main `ca287a32`)**: the first two riders of this row are row W93 now and the third is W91's. Main already holds: `CleanStale` returns every failure joined; an account with no canonical ID is an error in the purge; the dead `status` plumbing is gone; a folder `get` and `put` show progress (#918); a client bootstrap failure in drive discovery is an auth requirement and a failed drive listing a degraded notice; `graph.NewClient` refuses a nil HTTP client; the SharePoint site failures are W88's. Open, and this row's whole content: (1) `internal/cli/auth_flow.go` near `:174`: a failure to clear the sign-in requirement after a successful login is only logged; (2) `internal/perf/http.go:14-15`: a nil inner transport falls back to `http.DefaultTransport`; (3) `discoverAvailableDrives` (`internal/cli/drive_catalog.go:152-181`) starts one goroutine per token with no bound, where `shared_discovery.go:268-282` has the bounded shape; (4) `runPeriodic` (`internal/perf/logging.go:102`) stops only on `Complete`, not when the session's context ends; (5) the panic cancel cause of the rider in the wave table. Not with W88 (same file `drive_catalog.go`). |
| H9 | Conflict copies and trash restore through a rooted descriptor | `fix/rooted-noreplace-rename` | s | merged 914 9d08ad70 |  |  |
| I1 | `planIdentityKeyed`, pure and not wired | `feat/identity-planner` | s | planned | | |
| I2 | Plan diff and shadow runs in the models | `test/identity-shadow-models` | s | planned | | |
| I3 | Shadow comparison in live watch (debug event) | `feat/identity-shadow-watch` | s | planned | | |
| W1 | DoD and nightly survive GitHub errors; CI logs name no drive | `ci/dod-nightly-hardening` | x | merged | 883 | `ab89da4f` |
| W2 | Verify profiles stream every step and refuse an empty test tree | `ci/verify-profile-leftovers` | x | merged 888 b5d6723a | 888 | `1e568496` |
| W3 | e2e tests wait for what they read; no name-only waits | `test/e2e-wait-leftovers` | x | merged 911 0ff7c754 |  |  |
| W4 | Graph client: retries rebuild bodies, create marks are honest, quirks recorded | `fix/graph-client-leftovers` | o | merged 893 b32514f1 | 893 | `9502ab2e` |
| W5 | Pending-create records end once their file is settled | `fix/pending-create-lifetime` | x | merged 906 3e5c1bd4 |  |  |
| W6 | Control client: one probe, honest errors, one runtime per sync | `fix/control-client-leftovers` | o | parked |  |  |
| W7 | The control server's closing goroutine gets an owner | `fix/control-server-closer` | o | done by B8 (#882) |  |  |
| W8 | Engine entry points carry their collector and wrap load errors once | `fix/engine-entry-leftovers` | s | merged | 955 | `14906f0a` |
| W9 | Planner: unreachable branches refuse, equal-content edit decided, dead code removed | `fix/planner-leftovers` | s | merged 895 d28158e8 | 895 | `d4865523` |
| W10 | Issue types: exhaustive verdicts and keys, names that say what they hold | `fix/issue-type-leftovers` | s | merged | 884 | `142b194b` |
| W11 | Stored retry and trial times saturate instead of wrapping past 2262 | `fix/store-time-saturation` | s | merged 901 a6f6fbf3 |  |  |
| W12 | Sync test harness: load-proof waits, chaos summaries, session route covered | `test/sync-harness-leftovers` | s | merged 892 50e82dbb | 892 | `d0ef3c95` |
| W13 | Config writers: one write per expired pause, a context, portable locks | `fix/config-writer-leftovers` | o | parked |  |  |
| W14 | fsroot: precise absence errors and directory sync on FUSE and SMB | `fix/fsroot-leftovers` | o | merged | 887 | `191a1588` |
| W15 | A live test loses a create's answer on the wire | `test/e2e-lost-create-answer` | x | parked |  |  |
| W16 | A state store has its own lock: engines, reset, rebind and purge take it | `fix/state-store-lock` | x | merged | 885 | `5de4ff7f` |
| W17 | An account email change survives an interruption | `fix/email-rename-resumable` | o | merged 890 04b8e0e2 | 890 | `eb2a9970` |
| W18 | One rule for the modes of folders the client creates in a user's tree | `fix/user-tree-folder-modes` | x | merged 894 6891bbdf | 894 | `14c89097` |
| W19 | `status` answers within a bound when OneDrive is slow | `fix/status-live-budget` | o | merged 891 d766d7d7 | 891 | `404a9909` |
| W20 | The account identity check never fails or stalls a command | `fix/identity-check-not-a-precondition` | o | merged 959 899617bc |  |  |
| W21 | The stall watchdog's attempt check and timer change are one step | `fix/stall-watchdog-race` | o | merged 889 2614515e | 889 | `8c9c4769` |
| W22 | An account email change reaches everything that names the old email | `fix/email-change-leftovers` | x | parked |  |  |
| W23 | Watch tests prove absence by a signal, and the chaos model takes the session route | `test/sync-absence-and-session-chaos` | s | merged 903 b5d2ccf4 |  |  |
| W24 | A request's deadline reaches the token refresh and the retry wait | `fix/deadline-reaches-refresh-and-backoff` | o | merged 905 2b1c5afa |  |  |
| W25 | The status model has one field per fact | `refactor/status-model-one-field-per-fact` | o | parked |  |  |
| W26 | Human units are rendered by one package | `fix/one-human-units-package` | x | parked |  |  |
| W27 | No product code that only its tests call | `fix/no-test-only-product-code` | x | parked |  |  |
| W28 | An error without a format verb is `errors.New`, held by the linter | `chore/errors-new-without-verbs` | x | parked |  |  |
| W29 | Principle labels (P-n) that nothing defines are replaced by requirement IDs, held by a check | `docs/drop-undefined-principle-labels` | x | parked |  |  |
| W30 | A removed shared folder's local copy leaves by the rule of any folder deleted on OneDrive | `fix/shared-folder-release-by-delete-rule` | x | merged 900 bd2711b0 |  |  |
| W31 | A request the transport timed out is retried, not read as shutdown | `fix/transport-timeout-is-not-shutdown` | s | merged 915 3a80f900 |  |  |
| W32 | A shortcut child's trash is reachable, purged after retention, and says why a released file is there | `fix/shortcut-child-trash` | x | parked |  |  |
| W33 | An upload sync gives up drops its session record and cancels the server session | `fix/given-up-upload-drops-session` | x | parked |  |  |
| W34 | `status --perf` counts a committed transaction once | `fix/perf-tx-counted-once` | s | merged 959 899617bc |  |  |
| W35 | Multisync and cli tests prove absence by a signal, and long event waits have one name | `test/multisync-absence-by-signal` | x | merged 931 41919e44 |  |  |
| W36 | Two local moves that swap through a recreated folder plan without a dependency cycle | `fix/planner-move-recreate-cycle` | s | merged 912 ec6b0417 |  |  |
| W37 | A chaos run is replayable from its seed | `test/chaos-replay-from-seed` | s | merged 913 51e554fa |  |  |
| W38 | A shared folder that is gone or no longer shared says so, and a run counts shared folders as shared folders | `fix/shared-folder-gone-says-so` | s | merged 910 36f2e2b6 |  |  |
| W39 | A shortcut the user's filters exclude leaves no folder and no work behind | `fix/excluded-shortcut-leaves-nothing` | s | parked |  |  |
| W40 | A sync whose output cannot be written says so and exits non-zero, in text and in JSON | `fix/sync-output-write-failure` | s | parked |  |  |
| W41 | A run reports a condition the way `status` does: the four cosmetic leftovers | `fix/run-report-leftovers` | s | merged 945 89c388f6 |  |  |
| W42 | No managed folder lies inside a sync folder, and the local move does not replace | `fix/managed-folders-outside-sync-dirs` | x | merged 934 abfe625d |  |  |
| W43 | Every e2e read of a rewritten file waits for its content, and no poll predicate asserts | `test/e2e-content-reads-and-predicates` | x | merged 916 cf345986 |  |  |
| W44 | An entry left under the temporary spelling name is finished or made visible at the next start | `fix/spelling-rename-leftover` | x | parked |  |  |
| W45 | The chaos model runs in virtual time | `test/chaos-virtual-time` | x | parked |  |  |
| W46 | Jitter comes from a source the caller passes; a chaos seed replays identically | `refactor/injected-jitter` | x | parked |  |  |
| W47 | No e2e failure message or log prints a drive ID, an item ID or an address | `test/e2e-prints-no-ids` | x | merged 927 2afdb376 |  |  |
| W48 | Requests that time out count as outage evidence | `fix/timeouts-are-outage-evidence` | s | merged 920 29e28200 |  |  |
| W49 | A file put back at its old path after its folder moved stays there | `fix/move-back-to-old-path` | s | merged 926 a0dbe10b |  |  |
| W50 | A crash during a conflict copy leaves nothing empty behind (crash model seed 318012) | `fix/crash-conflict-copy-leftover` | s | merged 928 f82a5f84 |  |  |
| W51 | Crash recovery converges within its pass limit (crash model seed 318011) | `fix/crash-recovery-converges` | s | merged 935 e248aae5 |  |  |
| W52 | A subfolder moved onto its moved parent's old path on this computer converges | `fix/local-move-onto-vacated-path` | s | parked |  |  |
| W53 | A folder moved into its own former child converges | `fix/folder-into-former-child` | s | parked |  |  |
| W54 | An existing name after a 409 is found with one lookup | `fix/one-lookup-after-conflict` | o | merged 923 af2a6cf5 |  |  |
| W55 | Folder listings are read a page at a time | `refactor/paged-folder-listing` | o | merged 929 9798e318 |  |  |
| W56 | A failed Graph answer names its error code | `fix/graph-error-names-its-code` | o | merged 921 12a48bb2 |  |  |
| W57 | The crash model draws the crash point of the replacing move | `test/crash-model-replacing-move` | s | merged 959 899617bc |  |  |
| W58 | Every metadata body read is bounded, and a lost connection has a type | `fix/bound-metadata-body-reads` | x | merged 940 9e5498a5 |  |  |
| W59 | The trash side of a detach goes through a descriptor of its folder | `fix/detach-trash-side-rooted` | s | merged #953 a5af3b04 |  |  |
| W60 | The engine treats every failure to reach OneDrive as one family | `fix/unreachable-is-one-family` | s | parked |  |  |
| W61 | The tree names no fixture folder and no live item ID | `test/tree-names-no-fixture` | x | parked |  |  |
| W62 | Pure tests of e2e helpers run in the quick tier | `test/e2e-helper-tests-untagged` | x | merged 936 ca287a32 |  |  |
| W63 | A watch user can confirm held deletes | `fix/watch-confirms-held-deletes` | s | parked |  |  |
| W64 | A local read that times out is retried as busy | `fix/local-read-timeout-is-busy` | x | parked |  |  |
| W65 | A stopping run records its completions on a detached, bounded context | `fix/stop-records-completions` | s | merged 941 d339dd23 |  |  |
| W66 | An error names a local path once | `fix/error-names-path-once` | x | merged 948 99c582d4 |  |  |
| W67 | A replacing local move that finds a new file at the freed name still moves | `fix/replacing-move-freed-name` | s | parked |  |  |
| W68 | A file moved out of a subfolder into the folder re-made at the old path, with the emptied subfolder removed, converges | `fix/recreate-with-removed-subfolder` | s | parked |  |  |
| W69 | A read by path returns only the item at that path | `fix/path-read-is-exact` | x | merged 949 2686d99d |  |  |
| W70 | Status warns about a default sync folder that meets a managed folder | `fix/lenient-load-checks-default-sync-dir` | x | merged 950 5ce3ad30 |  |  |
| W71 | An e2e assertion on what a sync did reads every attempt of its converge loop | `test/e2e-assert-over-every-attempt` | x | parked |  |  |
| W72 | A seeded model of move steps holds that every plan converges | `test/move-steps-model` | s | parked |  |  |
| W73 | A name the path route does not resolve is found through its parent's listing in every command | `fix/names-the-path-route-misses` | o | parked |  |  |
| W74 | The crash model checks every empty file of a converged tree against a declared allowance | `test/empty-files-are-declared` | x | parked |  |  |
| W75 | Each test that cites the executor-returns-results requirement tests it | `test/executor-results-citations` | x | parked |  |  |
| W76 | A paged listing that gathers a slice fails on a next link it already followed | `fix/slice-listings-refuse-a-repeated-link` | x | merged 933 ca8ecac6 |  |  |
| W77 | Multisync and cli tests start and stop a watch only through bounded helpers | `test/multisync-watch-helpers` | x | merged 938 0e31e63c |  |  |
| W78 | A source check holds the test wait rules | `test/wait-rules-source-check` | x | parked |  |  |
| W79 | A move sent to OneDrive into a folder made here waits while OneDrive keeps a synced item at that path | `fix/remote-move-into-recreated-folder` | s | merged 952 5122e6a4 |  |  |
| W80 | A folder moved into a new folder at its own path converges | `fix/folder-into-new-folder-at-own-path` | s | parked |  |  |
| W81 | A publication's item type has one rule | `fix/publication-item-type-one-rule` | s | parked |  |  |
| W82 | The dispatch-order record sees every pass | `test/order-record-sees-every-pass` | s | parked |  |  |
| W83 | Test masks cover a sharer's name and any address | `test/masks-cover-display-names` | x | parked |  |  |
| W84 | The test environment prints no account path | `test/testenv-prints-no-account-path` | x | parked |  |  |
| W85 | The e2e recorders take the roles they redact with | `test/e2e-recorders-take-roles` | x | parked |  |  |
| W86 | The print rule follows what a suite function returns | `test/print-rule-follows-results` | x | parked |  |  |
| W87 | A delta listing fails on a next link it already followed | `fix/delta-refuses-a-repeated-link` | x | merged 937 ec5a0efe |  |  |
| W88 | A site whose libraries cannot be listed is named by drive list and drive search | `fix/unlisted-site-libraries-are-said` | x | merged 942 17564167 |  |  |
| W89 | The multisync panic-boundary tests that rerun the test binary run side by side | `test/panic-boundary-children-parallel` | x | parked |  |  |
| W90 | Two sync tests that take 8 to 25 seconds are profiled and made cheap | `test/two-slow-sync-tests` | s | parked |  |  |
| W91 | The sync summary counts a deleted folder as a folder, not as a file | `fix/summary-counts-deleted-folders` | x | parked |  |  |
| W92 | A folder made below a path whose old holder has not left waits for it | `fix/create-below-a-waiting-folder` | s | parked |  |  |
| W93 | A status line that fails to write is recorded; every printed count is worded by one helper | `fix/cli-counts-and-dropped-writes` | o | merged 944 6421bf98 |  |  |
| W94 | Lint reads every Go file, also one built only on another platform or with the race tag | `ci/lint-reads-every-file` | o | parked |  |  |
| W95 | A change list that leads back is refused and recovered wherever it is read | `fix/delta-loop-every-reader` | s | parked |  |  |
| W96 | Every store wait after a run's context ended goes through one helper and the stop's deadline | `fix/store-waits-after-cancel` | s | parked |  |  |
| W97 | The scanner's entries are their own type; `changeEvent` and `changeType` are gone | `refactor/scanner-entry-type` | s | parked |  |  |
| W98 | The CLI names a stalled answer and a lost connection by their type | `fix/cli-stall-and-lost-by-type` | o | parked |  |  |
| W99 | `drive list`'s site-search warning log line says a plain reason | `fix/site-search-warning-plain-reason` | o | parked |  |  |
| W100 | A refused stderr line of the sync console and of main's last lines is recorded | `fix/record-console-stderr-refusals` | o | parked |  |  |
| W101 | A rebind after a mount vanished deletes no unmounted disk's content unconfirmed | `fix/rebind-keeps-unmounted-disks` | s | merged | 954 | `a8ad2f27` |
| W102 | devtool links no product package, also through testutil | `refactor/devtool-no-product-via-testutil` | o | parked |  |  |
| W103 | Every repository-rule walk of devtool skips the same folders | `refactor/devtool-one-walk-skip` | o | parked |  |  |
| W104 | The benchmark's retries wait on an injected clock and read the CLI's server-error line | `fix/bench-retry-clock-and-5xx` | o | parked |  |  |
| W105 | Each account at a shared address has its own view, status row and selector | `fix/one-view-per-account` | o | parked |  |  |
| W106 | A panic on main's goroutine completes the performance summary; a failed watch start keeps a keepalive panic | `fix/panic-summary-and-startup-panic` | o | parked |  |  |
| W107 | A state reset, a relative path's sentence and a shared item's not-found line name their path rightly | `fix/error-path-leftovers` | x | parked |  |  |
| W108 | The CLI says in a sentence of its own that the answers for a path disagree | `fix/cli-disagreeing-answers-sentence` | o | parked |  |  |
| W109 | A folder create's empty answer and sync's 409s read the one name before listing the folder | `fix/one-name-reads-after-create` | s | parked |  |  |
| W110 | Drive and site IDs go into request paths by one rule | `fix/drive-and-site-ids-in-routes` | x | parked |  |  |
| W111 | Logout warns once, the missing-drive refusal names the config file given, and a default folder is computed one way | `fix/config-warning-leftovers` | x | parked |  |  |
| W112 | Creates at a waiting item's own landing below a folder moved here wait until it has left | `fix/own-landing-below-a-local-move` | s | parked |  |  |
| W113 | A folder moved into different new folders at its own path on each side converges | `fix/divergent-moves-into-own-landing` | s | parked |  |  |
| W114 | Three folders moved into each other's new folders converge | `fix/nested-own-landings-converge` | s | parked |  |  |
| W115 | Every run the exhaustive planner check records as broken has a row | `test/exhaustive-pairs-triage` | s | parked |  |  |

76 PRs at the start; 202 with A14, H9, A15, C4, A16, A17, A18, A19, A20, A21, A22 and the sweep PRs W1 to W115 (W112 to W114 added on 2026-11-18 from the leftovers #952 names under "Not in this change": a waiting item's own landing below a folder moved here is not held on this computer, a folder moved into different new folders at its own path on each side and three folders moved into each other's new folders never converge; W115 added on 2026-11-18 for the runs #952's exhaustive planner check still records as broken, the same on main; W111 added on 2026-11-18 from the leftovers #950 names under "Not in this change": logout's recovery prints the config's warnings twice, the refusal for a drive missing from the catalog says "config.toml" whatever file `--config` names, a shared drive's default folder and the other drives' collision list read its name from different places, and a relative `XDG_DATA_HOME` is named below the home folder; W109 and W110 added on 2026-11-18 from the two leftovers #949 names under "Not in this change" that no row covered: three lookups of one name still list the whole folder (a folder create's empty answer, sync's folder and file 409s), and drive and site IDs go into request paths as they are; W108 added on 2026-11-18 from the leftover #949 names under "Not in this change": a command whose path's answers disagree prints the raw error chain, which names folders by item ID; W107 added on 2026-11-18 from the leftovers #948 names under "Not in this change": `drive reset-sync-state` names the state file twice, the plain sentence of a local failure names the absolute path for a relative one, and a shared put or folder get that OneDrive answers with not found suggests `onedrive-go ls .`; W106 added on 2026-11-17 from the two leftovers #947 names as unchanged from main: a panic on the command's own goroutine writes no performance summary, and a keepalive panic is dropped from a failed watch-start error; W105 added on 2026-11-17 from H8's review fixes and its second review, the leftover #947 declared: account views are one per address, so with two accounts at one address the second has no status row and `--account` cannot pick it; W102 to W104 added on 2026-11-17 from A12's report and its review, items its merge left: devtool still links two product packages through testutil; the repository-rule walks skip different folders; the benchmark's retries sleep on the wall clock and miss the CLI's server-error line; W101 added on 2026-11-17 from the second review of W41: after a mount vanished, `drive rebind-root` makes the next sync delete the OneDrive content of every disk that is not mounted, with no confirmation below the hold's threshold (3 files deleted, 51 held); main behaves so already; W100 added on 2026-11-17 from the review of W93: `syncConsole.write` still drops a refused stderr line, and main's own last lines are written after the log file closed, so a refusal of theirs leaves no record; W99 added on 2026-11-17, found while building W88: `drive list`'s existing warning log line for a failed site search prints the Graph request id and response body at the default level; W98 added on 2026-11-16 from the review of W58: the CLI words a stalled answer by no rule of its own and still knows the lost connection by its text; W97 added on 2026-11-16 from the fact check for G7: the remote change type can replace `changeEvent` only on the remote side, and the scanner keeps the type as its collision input after G5; W96 added on 2026-11-16 from the review of W65: three store waits after a run's context ended still detach by hand, outside the stop's one deadline, and a completion in flight at the cancel is cut; W95 added on 2026-11-16 from the review of W87: the sync observer's own loop guard misses a tokenless window's self-link, a shared folder's looping change list is not recovered, and a token that is not a link restarts silently; W94 added on 2026-11-16 from the fact check for A12's brief: the per-PR lint run never reads a file built only on another platform or with the race tag, and the source checks A12 moves into lint read every file today; W93 added on 2026-11-16 from two riders on H8, split off when H8's sites were checked against main: a warning or progress line that fails to write is dropped, and counts are printed with a plural noun straight from a format (`1 files`); W92 added on 2026-11-16 from the writer's report and the review of the OneDrive-side move order: a folder made below a path that a waiting item still holds is created inside that item and carried back later; W91 added on 2026-11-16 from a live proof of a move fix: the summary line of a sync that removed one empty folder on this computer read "deleted 1 file locally"; W89 and W90 added on 2026-11-16 from the writer's report and the review of the stress lane's declared members: eight multisync tests that each rerun the test binary serially, and two sync tests that take 8 to 25 seconds; W88 added on 2026-11-16 from the review of the repeated-link fix for the non-delta listings: a site whose libraries cannot be listed vanishes from `drive list` and `drive search` without a word; W87 added on 2026-11-15 from the writer's report of the repeated-link fix for the non-delta listings: the delta loop has the same hole; W83 to W86 added on 2026-11-15 from the review of the move of the e2e helper tests into the quick tier: a shared folder's display name and a sharer's address that the test masks leave readable, account paths the test environment prints to stderr, recorders that redact with suite-wide roles, and suite function results the print rule does not follow; W81 and W82 added on 2026-11-15 from the review of the folder-replaced-by-a-file fix: the item type read as unknown when it is a file, which no user can reach today, and the order record that misses publication-only passes; W79 and W80 added on 2026-11-15 from the review of the follow-up to #926: the same commit-order defect on the OneDrive side when the holder's move waits, and a folder moved into a new folder at its own path, both on main before the follow-up and each needing its own rule; W77 and W78 added on 2026-11-15 from the review of W35: about 25 multisync tests start a watch on a bare goroutine and about 70 select blocks repeat one wait, and no check reads the test sources for the wait rules; W76 added on 2026-11-15 from the review fixes of W55: the paged loops that gather a slice have no check for a repeated next link, and a site search can return one site more than once; A22 added on 2026-11-15 from the review of A17: that change left its second rider out; W74 and W75 added on 2026-11-15 from the review of W50: an empty file at an ordinary name passes every invariant, and citations of one requirement that do not test it; A21 added on 2026-11-15 from the review of A20: the step before the failed one had passed 37 seconds short of its own deadline; W73 added on 2026-11-15 from the live checks made for W54's review; A20 added on 2026-11-15 from that night's stress failure; W71 and W72 added later on 2026-11-15: W71 from the nightly of that day (an e2e assertion that reads only the last attempt's output), W72 from the third review of W49 (move steps that stop converging are found by reading, not by machine); W64 to W70 added on 2026-11-15 from what the second passes and second reviews saw and did not fix: W64 and W65 from W48 (a local read that times out; a stopping run's completions), W66 and W69 from W54 (an error names a local path twice; a read by path can return an item elsewhere), W67 and W70 from W42 (a replacing local move that finds a new file at the freed name; the lenient config load), W68 from the second review of W49 (a cycle main has too); W61 to W63 added on 2026-11-15: W61 and W62 from the review of W47 (the tree still names fixture folders and item IDs; pure tests of e2e helpers run only behind the live suite), W63 from the install of #918 (a watch user cannot confirm held deletes without stopping the watch); W60 added on 2026-11-15 from the review of W48: the failures to reach the service are not one family in the engine; W59 added on 2026-11-15 from the review of W42: the trash side of a detach is reached by path; W57 and W58 added on 2026-11-15 from what two hand-backs named and did not fix: the crash model never draws the new crash point between the two steps of a replacing local move, and a metadata response body that stalls after its headers has no bound; W56 added on 2026-11-15 from a full-tier run of the branch of #916: a folder move answered HTTP 400 `Invalid request`, the second such answer in four days, and no log of either holds the answer's error code; W52 to W55 added on 2026-11-14 from the reviews of W49 and H7: two move shapes that never converge, and two places where a recursive transfer still reads a whole folder at once; W50 and W51 added on 2026-11-14 from the first measured run of the rotating seed range in A11: two seeds of the crash model fail, each replaying exactly by the writer's account; W49 added on 2026-11-14 from the review of W36: a file put back at its old path after its folder moved is moved away again by the sync, proven on main with a scenario; W48 added on 2026-11-14 from the review of W31: a timed-out request is retry work after W31, and this row makes it outage evidence too; W47 split from W43 on 2026-11-14, before W43's brief: W43 is what a test waits for, W47 what the suite prints; W45 and W46 split from W37 on 2026-11-14, when its first step showed that a chaos run is not a function of its seed; W18 to W30 added 2026-11-12, W31 to W40 on 2026-11-13, W41 split from H8 the same day, A17 from the review of A10, A18 from a pre-PR gate that ran into the short tier's fixed deadline, A19 from one that ran into the linter's, W42 and W44 from the reviews of H9 and W43 from the review of W3, all six on 2026-11-14). S0 (re-pin the split model) changes only this repo's notes.

## 2. Ground rules

These hold for every PR. They repeat the standing instructions so a subagent
brief can point here.

1. **Test account only.** All testing uses `test-account@example.com` and
   synthetic data under `~/sync-test-root`. Never the production account, never
   `<production sync folder>` in any mode. The production client in a terminal session is
   never touched. The production cutover is the owner's decision and is not part of
   this program.
2. **No account names or secrets in onedrive-go.** Not in commits, PR text,
   docs, test data or logs. `.testdata/` and `.env` stay git-ignored and are
   never copied into a worktree.
3. **onedrive-go's own process** (`CLAUDE.md` there): every increment starts
   with `verify --dod --stage start` (which checks and claims the nightly),
   has a Definition of Ready report, runs in its own worktree under
   `~/src/onedrive-go-wt/` made by `devtool worktree add`, is written tests
   first, updates the governing design doc and requirement lines, and goes
   through the `pre-merge` and `post-merge` stages. PR bodies carry
   `Review-thread carryover` and, when the branch holds claims,
   `Nightly failures`.
4. **This host has 2 cores.** One `devtool verify` at a time. Subagents run
   only targeted `go test` on the packages they touch.
5. **Install after every merge**: `onedrive-go-install <checkout>`, then the
   commit in the agent repo's `CLAUDE.md` and an entry in `CHANGELOG.md`,
   committed and pushed. A store-generation bump follows the reset procedure
   in section 7.
6. **No chained shell commands**; one command per call.
7. **No model identifiers** in commits, PRs or code. Commits end with the
   co-author line, PR bodies with the generated-with line.

## 3. Decisions taken under "Do all of this"

The owner asked for all of it without stopping, so open choices were taken on the
recommendation each analysis made. Each is reversible; all are listed in the
final report. The ids are decision ids; the split's steps are S0 to S6.

| # | Decision | Why |
|---|---|---|
| D1 | A one-shot `sync` exits **1** when it leaves work unsynced: (1) an action of this run failed, (2) work is queued to retry or left for the next sync, or (3) a condition from an earlier run still keeps a file unsynced (retrying, blocked or actionable work, or an observation issue that leaves a file unsynced). Not counted: work deferred by the chosen mode, deletes held for `--confirm-deletes`, conflict copies, a paused drive beside a synced one, `--dry-run`. JSON reports the drive as `incomplete`. No new exit code. | The exit status then says what `status` says, on every run. Without clause 3 a permanently refused file exits 1 once and 0 afterwards while it is still not on OneDrive. This reverses the rationale now written in R-6.6.28: a script sees 1 until the user deals with the file. (My first draft chose a new code, 75; the analysis showed 1 is enough.) |
| D2 | One coverage gate measured with `-coverpkg=./...`. The number is written only in requirement R-6.10.4 and read from there by devtool. It is set from a `workflow_dispatch` run of the branch, about 0.7 below the measured value (84.2 without `-race` at `70301c0e`, so about 83.5). | Today devtool says 75.5 and CI says 76.0, measured per package, so the two disagree and neither sees cross-package coverage. |
| D3 | Retry-After keeps "no cap, the server is ground truth" (R-2.10.7, R-2.10.14). Seconds overflow saturates; an HTTP-date is accepted and measured against the response's `Date` header. | The owner's written rule stays. A ceiling remains available as an owner decision. |
| D4 | JSON output has HTML escaping off, and a nil list prints `[]`. | One encoder; `&` and `<` in file names print as themselves. |
| D5 | Recursive `get` and `put` use one bounded pool of 4 workers; progress shows a running count, not a total. | The total needed a full listing before the first transfer. |
| D6 | Bounded waits: token endpoint 30 s for response headers and 60 s per request; token-file lock 90 s; config and catalog lock 10 s (polling every 50 ms). | Nothing external waits forever. A config write is short, so 10 s means another process is stuck. |
| D7 | `verify --dod --stage pre-pr` runs a new `pr` profile (lint, then quick); `--profile default` stays available. The unit-test timeout scales with cores. | The full race tier cannot finish in 20 minutes on 2 cores, so the stage was unusable here. CI stays the full gate. |
| D8 | Renamed tests get rows in `live-incidents.md` "Historical test names"; old incident text is not rewritten. | The existing convention. |
| D9 | A token that was refreshed but could not be saved does not fail the request. A one-shot command finishes its work and then exits 1 with the save error. The daemon logs it once per distinct failure, shows it in its systemd status, retries the save at every later token use, and exits 1 if it is stopped while the save is still failing. | A token that cannot be saved is lost at the next restart, so it must be loud; failing every request for it would stop a sync that still works. |
| D10 | Not done, on value: a split by one-shot versus watch mode, moving shortcuts into `multisync`, a re-export alias layer, a symbol-accurate layering check. | Stated in the program. |
| D11 | Shutdown keeps cancelling work in flight; there is no grace period. The three texts that say "drain" are corrected to say "cancel". | It is the written rule (R-6.3.6) and what the code does. The nightly failure came from a test that assumed a drain. |
| D12 | A watch runner that does not stop within its bound fails closed: it is logged with goroutine stacks, never replaced or closed under itself, and the watch ends with exit 1 so systemd restarts it. | A second engine on the same drive is worse than a restart. |
| D13 | A panic outside a drive's own runtime is logged with its stack and ends the process with exit 1 (never Go's 2). The daemon is not kept alive after a loop panic. | State after a loop panic is unknown; systemd restarts on 1. |
| D14 | The watchdog ping is tied to loop progress in two PRs: first the heartbeat with an error log and unconditional pings (B10); the conditional ping (B11) only after 72 hours of watch running on the test drive, across installs, without a false "no progress" line. | A wrong bound on one long step would make systemd kill a healthy daemon. |
| D15 | An unexpected return of the control server ends the watch with exit 1. `pause` and `resume` say what happened (`busy`, `reload_in_progress`). A perf capture is at most 5 minutes. | A daemon without its control socket cannot be paused, reloaded or queried, and said so only in one warning. |
| D16 | A planner row outside the decision table stops that drive's sync (the watch runner ends and retries with backoff 5 s to 5 min) instead of skipping the path. A planned folder transfer becomes a returned plan error. | "Skip and continue" is how a silent no-op happens. |
| D17 | One foreign key: a new nullable column `retry_work.blocked_scope_key` references `block_scopes(scope_key)`, non-NULL exactly when the row is blocked. Store generation 28 to 29; no migration (reset). | It is the only reference the code already asserts. The existing `scope_key` column also holds evidence for unblocked rows, so it cannot carry the key itself. |
| D18 | `DiscardScope` deletes only the blocked rows of the scope. Unblocked rows that name the scope as evidence keep their retry count and backoff. | Comments, test name and call sites say "blocked"; the SQL deleted every row with the key, which reset the backoff of work that then ran anyway. Confirmed or reversed by the test written first in E4. |
| D19 | Which of two colliding local names gets the issue: a name is blocked when it has something to upload. An unchanged synced file whose rivals are all unchanged gets no finding. Written into R-2.12.2. | Today's effective rule. Blocking every collider would stop an unchanged synced file from receiving remote edits. |
| D20 | The replan's own full scan is local truth; watch events only wake it. Written into the design; what it makes redundant is deleted. Dropping the replan scan in favour of watch patches is not pursued. | Production already passes no event channel. No measurement supports the alternative. |
| D21 | The stored form of a name (NFC) lives in `internal/namekey` (`Stored`, `StoredPath`). There is no `Name` wrapper type; keys are typed (`namekey.Key`, `PathKey`) and one function finds a sibling by name. | Name equivalence was already central in `namekey`; a wrapper would add a type without authority. This amends the split design, which put the NFC helper in `syncdomain`. |
| D22 | Engine work E1 to E4 lands before the split, E5 to E13 after it. | E1, E2 and E4 fix defects and C3 needs E4. The rest is easier and faster to test in the smaller engine package, and does not change what crosses a package boundary. |
| D23 | The reports' PR lists (about 120 PRs) are batched into 76 where PRs share files and risk class. A behaviour change and a pure move are never in one PR. | Each PR costs a CI round on a shared live-account queue. |

## 4. How the work is done

**Two lanes.** The live-account CI jobs share one concurrency group (one
running, one pending), so at most two PRs are in CI at once, on areas that do
not overlap:

- *sync lane* (`s`): `internal/sync`, `internal/multisync`, `e2e`
- *outer lane* (`o`): `internal/cli`, `config`, `graph`, `driveops`, `retry`,
  `fsroot`, `namekey`, `internal/devtool`, CI
- A PR marked `x` touches both and runs with the other lane idle. The outer
  lane is held while S5 is open, because that PR rewrites import lines in
  `cli` and `multisync`.

**Order.**

- Sync lane: A1, A3, A4, A5, A6, A7, B1, B2, B3, B8, B9, B10, C1, C2, E1, E2,
  E3, E4, C3, G3, G4, G5, G6, G7, G8, then S0 to S6, then E5 to E13, F1 to F7,
  G9 to G11, I1 to I3. B11 and G12 when their gates open.
- Outer lane: A2, A8, A9, A10, A11, A12, A13, B4, B5, B6, B7, G1, G2, H1 to
  H8.
- After each wave the full tier runs (the scheduled nightly, or
  `gh workflow run ci.yml` at a pause), and the next start stage reads it.

**Per PR.**

1. `git -C ~/src/onedrive-go fetch origin`, then
   `go -C ~/src/onedrive-go run ./cmd/devtool worktree add --path ~/src/onedrive-go-wt/<name> --branch <branch>`.
2. In the worktree: `go run ./cmd/devtool verify --dod --stage start`.
   Definition of Ready report (it goes into the PR body).
3. Check the analysis claims the PR rests on against the code.
4. Failing tests first, each citing its requirement (`// Validates: R-…`).
   Then the change, then the governing design doc and requirement lines.
5. `go test -race` on the touched packages.
6. `GOTOOLCHAIN=go1.26.10 go run ./cmd/devtool verify lint`, then
   `PATH=~/go/bin:$PATH go run ./cmd/devtool verify quick` (after A2:
   `verify --dod --stage pre-pr`).
7. Where the change touches a live path: the matching live e2e tests, one at a
   time, against the test account.
8. Push, PR, `verify --dod --stage pre-merge --pr N`, then
   `verify --dod --stage post-merge --pr N --worktree <path> --branch <b>`.
9. Install, the watch client restarts on the new binary, `onedrive-go status`
   on the test drive reads "Up to date", and a manual check on `~/sync-test-root` where
   the change is user-visible. Agent repo: `CLAUDE.md` commit, `CHANGELOG.md`,
   the status table above, commit and push.

**Who writes what.** A PR whose scope is fully specified by its analysis
section is implemented by a subagent in its own worktree, briefed with: the
analysis section, the worktree path, the ground rules above, "check the
report's claims against the code, tests first, targeted package tests only,
never `devtool verify`, never e2e, never the CLI against any account, commit
locally, do not push". I read every diff, run steps 5 to 9 myself. PRs that
need design judgement (S5, F3, F4, E7 to E10, I1) I write myself. A subagent
that hits the usage limit is not replaced by a duplicate; its worktree is
finished directly.

**Nightly failures.** Every start stage claims failing nightly tests for its
branch. A claimed failure is fixed in that PR and recorded in
`spec/reference/live-incidents.md`, or released with `devtool nightly release`
when it belongs to a later PR of this plan (the PR is named in the release).

## 5. What I cannot do

- Merge dependabot PR #858 (the permission layer refused a merge without
  review; it is green and left for the owner).
- The production cutover.
- Verify macOS behaviour other than through CI's macOS shards.
- Make increment 10 authoritative. I1 to I3 land the identity-keyed planner in
  shadow mode with a comparison report. The cut-over (new columns, the path
  constraint dropped, a generation bump) is a decision for after a soak.
- B11, if the 72-hour gate of D14 has not opened by the time everything else
  is done. It is then reported as open with the hours reached.
- Name equivalence on a Business account. Only a Personal test account exists;
  the probe corpus in `namekey` is Personal.

## 6. The PRs

One section per wave. Each entry names its source in `analysis/`; the source
has the file and line references, the proposed signatures and the test names.

### Wave A: signals and tooling (increments 1 and 9)

A1 fixes the test the nightly is red on. A2 makes every later PR's gates usable
on this host. A8 to A11 make a red nightly visible before the larger waves.

#### A1. Empty file left by a failed upload is removed; PauseResume waits for content

- **Source**: `01b` section 1 (fix design 1d); nightly run `42865435904`.
- **Defect**: an upload whose session request fails removes the empty file it
  created with one conditional delete. When the cancelled request still
  reached the server, the eTag moved after the cleanup read, the delete got a
  412 and the empty file stayed. Separately, `TestE2E_SyncWatch_PauseResume`
  stopped the daemon as soon as the file name was visible.
- **Change**: `graph.Client.RemoveEmptyItem(ctx, driveID, itemID) (removed
  bool, err error)`: read the item, delete it conditional on the eTag just
  read, answer a 412 by reading again, at most 3 attempts inside a 10 s
  detached timeout, stop as soon as the item is not empty.
  `removeCreatedPlaceholder` uses it. `driveops.PlaceholderRemover` changes
  from `DeleteItemIfMatch` to `RemoveEmptyItem`; `removePlaceholder` and
  `removeEmptyPlaceholder` become one. e2e: new `waitForRemoteFileContent`;
  PauseResume waits for the content before SIGTERM.
- **Tests first**: graph, beside
  `TestUpload_FailedContentStepRemovesItsEmptyItemByItsCurrentETag`: 412 then
  204 sends both eTags in order; second read shows size 7, exactly one DELETE;
  412 every time, exactly three DELETEs and the upload still returns
  `*CreatedItemError`; second read 404, no further DELETE. driveops: through
  `tmCreatingUploader` with `removedEmpty`.
- **Docs**: R-5.6.14, `drive-transfers.md`, `graph-api-quirks.md`,
  `LI-20261110-01` (also the 2026-10-30 `FileModification` occurrence).
- **Validation**: the live test `TestE2E_SyncWatch_PauseResume`, three runs.
- **Nightly**: the start stage claims `TestE2E_SyncWatch_PauseResume`.

#### A2. devtool gates that work on a 2-core host; CI wait that survives a cancelled shard

- **Source**: `09` sections 3 and 6 (its PRs 1, 5, 6); decision D7.
- **Change**: devtool reads `toolchain` from `go.mod` and sets `GOTOOLCHAIN`
  for the commands it runs when the variable is unset; the lint version check
  also compares the Go version the linter was built with and prints the
  rebuild command. `pre-pr` defaults to a new `pr` profile (lint, then quick).
  The unit-test timeout is `20m × max(1, ceil(4 / GOMAXPROCS))` with
  `--unit-timeout`. `worktree add` fetches `origin/main` first. Post-merge
  cleanup removes exactly what `.worktreeinclude` put into the worktree.
  `waitForPRCI` and the post-merge wait end on a stall window (longest job
  timeout plus 10 minutes without a state change) instead of a fixed 30
  minutes; a cancelled job of the head commit is rerun with
  `gh run rerun <id> --failed`, at most twice (`--ci-max-reruns`).
- **Tests first**: toolchain parsed and passed; an explicit `GOTOOLCHAIN`
  wins; lint check fails on a built-with mismatch; pre-pr runs lint then
  quick; timeout arithmetic for 2, 4 and 8 cores; fetch before `worktree add`;
  cleanup removes the bootstrap entries only; with an injected clock: progress
  extends the wait, a stall ends it naming the pending checks, a cancelled
  shard is rerun once, the third cancellation fails, a cancelled run of an
  older commit is not rerun.
- **Docs**: onedrive-go `CLAUDE.md` (DoD steps), `system.md`, R-6.10.16,
  R-6.10.22. Agent repo `CLAUDE.md`: the toolchain and pre-pr workarounds go.
- **Validation**: here, `verify lint` with no `GOTOOLCHAIN` set, and
  `verify --dod --stage pre-pr` end to end.

#### A3. Created-item record survives failure and shutdown; clean shutdown is quiet

- **Source**: `01b` section 1 (gaps G1 to G3; its PRs 6 and 7); D11.
- **Defects**: G1, the large-file path returns the created item inside an
  error before the record hook runs, so the record of the item is lost. G2, an
  adopted placeholder without a hash whose eTag moved again plans an edit-edit
  conflict. G3, the record is written on the action's context and is lost to
  the cancel. A clean shutdown logs a warning from housekeeping (against
  R-6.6.21).
- **Change**: `recordPendingCreate` writes on a detached context with the
  outcome-commit timeout; `openSessionForUpload` calls the created hook when
  the error carries an item; `clearPendingCreateIntent` skips its write when
  the context is cancelled; the comparison treats the G2 case as not a
  conflict; `refreshProtectedRootsForHousekeeping` logs at debug when the
  context is done. Wording: "cancel", not "drain", in `CLAUDE.md`,
  `sync-engine.md`, the watch log text, R-2.8.3 and the signal comment.
- **Tests first**: driveops beside
  `TestSessionUpload_RetryOfAnInterruptedCreateReusesItsItem`; sync beside
  `TestPendingCreateIntent_ClearedWhenNothingWasCreatedAndKeepsAKnownItem`;
  `TestCommitObservation_AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict`;
  `TestPostSyncHousekeeping_CanceledContextIsQuiet`.
- **Docs**: R-5.6.15, `drive-transfers.md`, `sync-execution.md`,
  `sync-engine.md`.

#### A4. Mount perf collectors receive context-recorded metrics

- **Source**: `01b` (`status --perf`; its PR 9).
- **Defect**: recorders read the collector from the context; the per-mount
  collector is never put there, so a watch reports zero HTTP and database
  counts.
- **Change**: in `multisync`, wrap the mount context with
  `perf.WithCollector` where the watch runner's context is made and at the top
  of the one-shot mount function.
- **Tests first**: `TestOrchestrator_MountPerfIncludesContextRecordedMetrics`
  (R-6.6.15); an engine pass asserts `Snapshot().DBTransactionCount > 0`.
- **Validation**: `onedrive-go status --perf` against the running watch
  client after a file change in `~/sync-test-root`.

#### A5. e2e waits check content, not names

- **Source**: `01b` and its list `01d` (18 sites; one is done in A1).
- **Change**: `waitForRemoteStatSizeForDrive` and the content helper replace
  the remaining 17 name-only waits and two inline loops; `pollForDrive2File`
  goes if nothing else calls it. New recurring-family entry "Name-only wait
  stops the daemon mid-create and leaves an empty placeholder".
- **Validation**: each touched test once, live, one at a time.

#### A6. e2e single-pass syncs use the converge helper

- **Source**: `01b` and its list `01c` (104 sites: 66 converge, 37 stay
  single-pass by design, 1 unclear).
- **Change**: converge and transient-failure helpers take a runner
  (`syncRunner`), with per-drive and all-drives variants; the 66 sites move;
  each of the 37 gets a one-line comment saying why it is one pass; the
  unclear site (`shared_items_e2e_test.go:212`) is settled by reading the
  test; three hand-written `require.Eventually` loops use the helper.
- **Validation**: the flake candidates named in `01b` live, one at a time; the
  rest through the nightly.
- **Interaction**: B2 changes what a pass that left work exits with; B2
  updates these helpers.

#### A7. Fakes answer with the faults seen live

- **Source**: `01b` (fake Graph layers; its PRs 8 and 10).
- **Change**: per layer: HTTP handlers get `createStatus`, `deleteStatuses`,
  `readBacks`, `sessionLandsThenDrops`; driveops mocks get `removeResults`;
  the scenario remote gets `refuseCreate` (400 `invalidRequest`),
  `invisibleFor` (visibility lag) and a session uploader; the chaos remote
  gets a placeholder-churn variant of its timeout-after-create fault.
- **Tests first**: `TestUpload_RefusedCreateSendsNoContentAndDeletesNothing`;
  one engine-level test per fault.

#### A8. One coverage number; stress lane covers the sync tree

- **Source**: `01a` sections 5 and 6 (its PRs 1 and 2); D2.
- **Change**: `-coverpkg=./...`; `requiredCoverage(repoRoot)` reads the number
  from R-6.10.4; both constants and the number in `ci.yml` go; stress targets
  `./internal/sync/...` with a tree-aware "pattern matched nothing" guard and
  slow-test summaries keyed by package.
- **Tests first**: `TestRequiredCoverageReadsRequirement`,
  `TestRunVerifyZeroCoverageThresholdUsesRequirement`,
  `TestAssertTestsMatchedInTree_*`,
  `TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing`,
  `TestSummarizeStressSlowTestsKeysByPackage`.
- **Validation**: a `workflow_dispatch` full run sets the number and passes.

#### A9. Every e2e bucket runs; gate riders

- **Source**: `01a` sections 2 and 7 (its PR 3 and riders a, b, d, e, f, h, l,
  m, t).
- **Change**: `runE2EFull` runs every bucket and joins the failures; buckets
  not run after a preflight failure are recorded as not run; failed tests are
  named in the summary; output is streamed; per-bucket Go deadlines sum to
  less than the job timeout (tested). Riders: the format step can fail
  (`--check-format`); `public` and `stress` run every step; the classified
  rerun keeps `-race` and the seed; a skipped auth preflight is not recorded
  as a pass; `verify-full` writes a summary; three CI steps stop printing
  drive identifiers and secret names into public logs.
- **Tests first**: `TestRunVerifyE2EFull{RunsEveryBucketAfterOneFails,
  ReportsEveryFailingBucket,RecordsBucketsNotRunAfterPreflightFailure}` and
  the three existing tests the report names.
- **Docs**: new R-6.10.26, `system.md`.

#### A10. Nightly: sound verdicts, visible result

- **Source**: `01a` section 1 (its PRs 4 and 5) and `09` section 5 (its PR 7).
- **Change**: `nightly check` reads the newest completed scheduled run; a run
  without a verdict, a stale run and a red run without attributed tests are
  findings; a timed-out job is a failure; a "fixed" claim is deleted only when
  its job passed in a run that contains the fix; keys are package-qualified;
  leases renew in the pre-pr and pre-merge stages; the PR-body gate matches on
  a word boundary. A `nightly-report` job (schedule and dispatch only) posts
  commit status `nightly` and opens, updates or closes one issue labelled
  `nightly-failure`. Post-merge reads push runs only; the CI concurrency group
  is per event; the README badge shows the scheduled run.
- **Tests first**: `TestNightlyReport*` (seven), 
  `TestNightlyRequiredJobsExistInWorkflow`, `TestNightlyCheck*` (four), and
  the extended tests the report names.
- **Docs**: R-6.10.23, new R-6.10.24, `system.md`, `CLAUDE.md`.
- **Precondition**: the key format changes only when
  `git ls-remote origin 'refs/nightly-claims/*'` is empty.
- **Validation**: a `workflow_dispatch` run in dry-run mode prints the status
  and issue it would write; the first scheduled run after the merge writes
  them.
- **Rider (2026-11-11)**: W1 lands first and owns the retrying `gh` call (one
  helper, 5xx and dropped connections, bounded backoff); this PR builds on it
  and owns the stage rule that follows. Every GitHub read of `nightly check`
  (run list, job list, job log) goes through that helper. When GitHub still
  fails, the start
  stage fails with one plain sentence that names GitHub and the endpoint (no
  usage text), and the pre-pr, pre-merge and post-merge stages do not fail on
  it: they print the same sentence as a warning. Seen on 2026-11-11: the jobs
  endpoint of one nightly run answered HTTP 502 for more than 15 minutes and
  no increment could pass its start stage. Tests first: a fake `gh` that
  fails twice then answers; one that always fails (start fails with the
  sentence, post-merge passes with the warning). Also here: a post-merge
  stage must not exit 1 because of a failed nightly that the merged PR did
  not touch (B1's post-merge did).

#### A11. Race on every push; nightly seed rotation

- **Source**: `01a` sections 3 and 4 (its PRs 6, 7, 8).
- **Change**: `verify race` profile and a `verify-race` job on push
  (`go test -race ./internal/sync/...`). One seed helper for the four model
  families, a replay line printed on failure, a `seeds` profile and a nightly
  `model-seeds` job that rotates the start seed by date; a failing seed opens
  a sticky issue labelled `seed-failure` (never closed automatically). The
  issue code merges before the rotation is switched on.
- **Docs**: R-6.10.22, new R-6.10.25.

#### A12. Lint moves; devtool imports no product package

- **Source**: `09` sections 2 and 4 (its PRs 2 and 3).
- **Change**: forbidigo rules replace two hand-written checks (test failure
  calls through a struct field; direct `slog`, `os.Stdout`, `log.Print`); the
  README status check, the retired-backlog-ids check and
  `verify_test_conventions.go` are deleted with their docs (R-6.10.18). The
  benchmark's cursor reader goes and a depguard rule `devtool-no-product`
  keeps product packages out of devtool. Lands before the split.

- **Decision (mine, 2026-11-13)**: the retired-backlog-ids check, its registry and R-6.10.18 stay. W29 builds its label check on that step, and the `B-NNN` ids still cited in Go files would lose their only definition. W29 decides what becomes of both kinds of label. A12 deletes the README status check and `verify_test_conventions.go` only. Base: A11's accepted tip or main after A11 (both change the devtool verify profiles), and after the JSON-encoder change (H6), which also edits `.golangci.yml`.
- Rider (2026-11-16, from checking the analysis against main `ca287a32` while writing the brief): (1) `verify_test_conventions.go` holds three rules today, not two: since #888 also the rule that every test in a file built with the integration tag carries the integration prefix (`integrationTestNameViolations`), which lint cannot state. Decision (mine): the file and the step `test-conventions` stay with that rule alone; only its selector half (direct `testing` failure calls, `time.Sleep`) goes to forbidigo, and the Sleep rule is already there. (2) Only `readme-status` leaves the closed set of verify step names (32 to 31); `output-boundaries` stays with the flattened-error-attribute rule. (3) `bench_live_bidirectional.go` imports three product packages (`internal/config`, `internal/driveid`, `internal/sync`), and all three serve only the cursor read, so the depguard rule needs no exception. (4) `analyze-types: true` is already set for forbidigo. (5) The exclusion of the three filesystem packages has no `text:` filter and would hide the new rules there: it gets one. (6) Decision (mine): the hand-written checks read every `.go` file whatever its build constraint, the per-PR lint run does not build files for another platform or with the race tag; A12 accepts lint's coverage for the moved rules, lists those files in its DoR, fixes by hand what a moved rule would report in them, and says in the spec which files lint reads. W94 closes the gap for all rules.

#### A13. e2e buckets derived from test names

- **Source**: `09` section 1 (its PR 4).
- **Change**: bucket = family token of the test name; 20 renames; an unknown
  family fails before the first live call; the three hard-coded lists go.
  Renamed tests get "Historical test names" rows (D8).
- **Precondition**: no open nightly claim on a renamed test.
- Rider (2026-11-16, from the fact check on main `ca287a32`): the fast tier holds eight `TestE2E_` tests, not seven (`TestE2E_TrackedFilesNameNoTestAccount` is the eighth); the no-tests guard is already on main (bucket runs go through `runGoTestMatching`), so it is not part of this row. Decided: discovery asks `go/build` `MatchFile` which files a tag set builds and never compares the text of a build line; a bucket pattern stays an explicit name list; one exact family table, no exception list; per D8 the incidents log gets one "Historical test names" row per rename and its old entry text stays. The next free requirement id on main is R-6.10.28. Precondition not met on 11-16: one nightly claim ref names `e2e.TestE2E_Sync_IncrementalDeltaToken`, one of the 20 renames; list the claim refs at dispatch and again before the merge. Not with A21, A22 or A12 (same devtool files); one after the other with W71 (`e2e/sync_full_test.go`, `sync_recovery_e2e_test.go`). Live runs after the hand-back: the renamed tests that run on this host, one at a time.

#### A14. A create whose answer was lost keeps its claim

- **Source**: found live on 2026-11-11 by A5's content wait
  (`TestE2E_SyncWatch_BasicRoundTrip`, during a 75 s network outage on this
  host). The create POST landed, its answer was lost ("http2: client
  connection lost"), `uploadNewFile` cleared the intent because the hook
  never ran, the retry got 409 and cleared the intent again. The empty item
  was then a stranger's file: the retry got 409 on every attempt until the
  next remote observation, which reads it as a create conflict (the user's
  file renamed to a conflict copy, the empty file downloaded in its place).
  The chaos layer models a timeout after the hook ran, never a lost create
  answer.
- **Change**: `graph.CreateFile` marks a failure that does not say whether
  the create landed (no response, 5xx, 408, cancel) with
  `ErrCreateOutcomeUnknown`. `uploadNewFile` reads the pending row before it
  records its intent (`prior`); it keeps the intent after an unknown outcome;
  `resolveCreateConflict` with a prior intent and no item ID claims a
  same-spelling empty file from the listing it already makes (records the
  item, uploads by ID with If-Match), keeps the intent when the listing
  shows nothing yet, and clears it for anything else. A 409 without a prior
  intent clears as today.
- **Tests first**: executor test (create lands, answer lost, retry claims
  and uploads, one remote file, no conflict copy); `TestRunOnce_` scenario
  (answer lost, next pass observes the empty item and uploads into it; listed
  in `runOnceScenarios()`); 409 without a prior intent does not claim a
  stranger's empty file; chaos layer fault `create:response-lost`.
- **Docs**: R-5.6.15; `sync-execution.md`, `drive-transfers.md`,
  `sync-store.md`; the comment at the top of `store_pending_creates.go`.
- **Order**: after A3 merges (same files); before A7 (same chaos file).

#### A15. A stale precondition is superseded, whatever status it carried

- **Source**: exposed by A7's listing-lag test (`reports/A7-report.md`,
  "Product defect exposed"), 2026-11-11.
  `internal/sync/engine_result_classify.go:67` classifies by HTTP status
  (`classifyHTTPResult`) before the stale-precondition check at :134, and
  `executor_transfer.go:369-372` (`uploadTransferError`) wraps the Graph
  error with `%w`, so `worker_result.go:98` records its status. A create 409
  that `resolveCreateConflict` returns as stale (:694 same spelling, :697
  nothing listed yet) completes as HTTP 409, `serverAnswerOther`: retry_work
  on the durable curve (30 s doubling), logged "Not syncing <path>: file
  changed while it was being synced: … 409". It should be superseded and
  replanned in the same run (`sync-execution.md` 211–215;
  `uploadTransferError`'s own comment). By the same order a 412 on a
  conditional overwrite is requeued as `transient_conflict`.
- **Change**: the stale-precondition check (`errActionPreconditionChanged`)
  runs before the HTTP branch in the result classifier (verify first that no
  other error class depends on the present order; the alternative is keeping
  the Graph status out of the stale error).
- **Tests first**: flip A7's
  `TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt`
  (superseded, replanned in the same run, no retry_work row); a classifier
  table row for stale-with-409 and stale-with-412; an engine test for the 412
  on a conditional overwrite.
- **Docs**: `sync-execution.md` only if its text does not already say this.
- **Order**: after A7 merges (it holds the test to flip). Opus writer.

#### A16. A create intent nobody will retry is dropped; expired intents are pruned

- **Source**: `reports/A7-seeds.md`, 2026-11-11. `TestModel_ChaosGraphConverges`
  fails on seeds 9 and 11 (the nightly runs seeds 1–2 only) on the invariant
  "no pending creates after convergence": a create ends "outcome unknown"
  (reset before it reached OneDrive), its intent is kept by design
  (`store_pending_creates.go:58-64`), the user renames the local file away, no
  create is ever retried at that path, and the intent-only row stays. Checked
  in code: `pendingCreateIntentTTL` (72 h) only stops a stale intent from
  claiming (`store_pending_creates.go:387`, `executor_transfer.go:549`);
  nothing deletes an expired row, so such rows stay for good. While it is
  fresh, an orphaned intent can still claim an empty file a stranger makes at
  that path, and with the local file gone the plan deletes that file.
- **Decision (D24, mine)**: an intent-only row is dropped when the plan is
  built from a complete local observation that has no file at its path and the
  remote observation of the same pass did not claim a placeholder for it
  (a row that names an item is never dropped this way). Intent-only rows past
  the TTL are deleted at the same point. A create that lands later than that
  shows up as a remote empty file, which is the lesser harm. The writer checks
  this against the code and the kill-mid-create case (process dies, the item
  lands, the user deletes the local file before the restart: observation
  claims first, so the placeholder is still deleted) and says so if the
  evidence points elsewhere.
- **Decision (D30, mine, 2026-11-11; review of 14de692b)**: the first pass
  ran the "file gone" rule after every plan, but a watch steady-state replan
  refreshes local truth only (`runSteadyStateCurrentPlan`), so there the
  remote observation D24 relies on has not happened: with the create's answer
  lost and the file renamed or deleted before the next remote poll, the
  intent was dropped and the landed empty item then came down as an empty
  file. The rule now runs only in a pass that observed the remote side before
  planning (one-shot, watch bootstrap) and only with remote truth available
  at the path; a steady-state replan applies the expiry rule only. Weighed:
  in steady state a failed create with an unknown outcome has often landed,
  while a stranger's empty file at the same path within 72 h is very rare,
  so keeping the intent is right far more often. Closing the remaining
  window (drop after the next remote poll) is a rider on W5. Second pass also
  fixes report entries 3 to 5 (`store_write_observation.go`: stacked doc
  comment, `scanRemoteStateRow` swallowing scan errors, second package
  comment); entries 1 and 2 are riders on W5.
- **Tests first**: seeds 9 and 11 reproduce on main, then pass; a store test
  for the prune; an engine test for rename-after-unknown-outcome (row gone
  after the next pass) and for the kill-mid-create case above (placeholder
  deleted); the model's invariant stays as strict as it is.
- **Docs**: the comment block at the top of `store_pending_creates.go`;
  `sync-execution.md` where pending creates are described.
- **Live**: `TestE2E_Sync_KillMid…` from the worktree, one test.
- **Order**: after A7 merges (its harness) and after A15 (same test file).
  Opus writer; I review the planner change line by line.

#### C4. An interrupted removal's condition clears when the leftover is gone

- **Source**: `reports/C2-report.md`, recommendation 2; checked in code
  2026-11-11: `scanner.go:498` reports `issueLocalInterruptedRemoval` as a
  skipped item on every scan, but `localObservationManagedIssueTypes()`
  (`observation_findings.go:10`) does not list it, so a full scan that no
  longer finds the leftover does not clear the stored condition.
- **Change**: verify the clearing rule in the store (what "managed" means for
  a batch), then add the type to the managed list, or name in a comment why
  it must stay unmanaged if the check shows a reason.
- **Tests first**: scan with a leftover (condition present), remove the
  leftover, full scan (condition gone); status no longer shows it.
- **Docs**: `sync-observation.md` or the issue-type table, where the managed
  types are listed.
- **Order**: any time; small. Opus writer.

### Wave B: the ways the daemon hangs or fails silently (increment 2)

Source: `02` (its section numbers below) and `08` items 2, 9 and 11.

#### B1. Split `orchestrator_watch.go` (pure moves)

`08` item 6. 1,373 lines into the kept file plus `watch_wake.go`,
`watch_runner.go`, `watch_reload.go`, `watch_reconcile.go`,
`watch_runner_stop.go`, `watch_mount_equivalence.go`. No other change, so B3,
B8, B9 and B10 edit small files.

#### B2. One-shot `sync` exits 1 when work is left

- **Source**: `02` section 8; D1. After A6.
- **Change**: the engine report gains `Unresolved` (retrying, blocked,
  actionable, issues), zero on a dry run. In `cli`, one `leftWork` function
  feeds the text summary, the JSON summary and the exit status. A table
  classifies every issue type as "leaves a file unsynced" or not, with a test
  that fails on an unclassified constant. A plan-invariant violation and a
  throttled run that left everything no longer exit 0.
- **Tests first**: the eight `TestRunSyncCommand_*` cases,
  `TestRunOnce_ReportCountsUnresolvedWork`,
  `TestRunOnce_DryRunLeavesUnresolvedZero`,
  `TestIssueTypes_EveryTypeIsClassifiedForOneShotExit`.
- **e2e**: `runSyncThroughTransientFailures` runs through the allow-error
  runner and asserts "left-work text if and only if non-zero exit"; the tests
  in `02` 8.5 change (the read-only shared folder test expects 1 on the first
  and on the second run; the delete-hold test is pinned to 0); the remaining
  direct `sync` invocations are audited file by file.
- **Docs**: R-6.6.28, R-6.6.31, R-6.8.15, R-6.6.19, R-6.6.33, `cli.md`,
  `docs/scripting.md`, `docs/running-as-a-service.md`.
- **Validation**: live, one at a time: the read-only shared folder test, the
  delete-hold test, the status-issue test, the remote-delete-to-trash test.
- **Decisions (mine, 2026-11-12)**: (1) the end state is the analysis's 8.3 as written; (2) `Unresolved` is read from the same store snapshot `status` reads, through the same function, so the exit status and `status` cannot count differently; (3) the issue-type classification is one more exhaustive switch in the family #884 built, with its source-reading test, not a map; a row answers "leaves a file unsynced" unless the type records only a deliberate hold or a fact that keeps no file from syncing, and an unsure row answers "unsynced"; I review the table; (4) watch mode is untouched; (5) the e2e changes of 8.5 are in the same commit, compiled under the e2e tags by the writer and run live by me once Graph's `GET /me` answers (the incident of 2026-11-12 blocks live tests); (6) a user-facing change log entry if the repository keeps one. The writer keeps its edits in `sync_runtime.go`, `failure_class.go` and `main.go` small, because B9 is changing them at the same time.
- **Review decisions (mine, 2026-11-12)**: (a) **the count has no subtraction**: every live pass prunes retry work to the plan's actions (`reconcileRuntimeState`), so each retry row the store holds after the last pass belongs to an action already in `Failed` or `LeftForNextSync`; still open is therefore the observation issues that leave a file unsynced, and the total is failed + will retry + still open. The retry-row counters the first pass added go. If the writer finds a retry row that can sit outside the run's counters, it stops and reports. (b) **Follow-up work the run ends on is left work**: `Deferred` and `Dropped` always, `Renames` only at the pass bound; the zero-action branch keeps the plan's follow-up. (c) A file that fails and is then synced by a later pass of the same run is not left, and a file that fails in two passes is one file (failed files keyed by path). (d) The writer's "Not fixed" 2, 3, 4, 6 and 7 are fixed in this change (one read transaction for the status snapshot, one "Retrying" filter, the exported function with the unexported type, the bench sample that left work, the e2e single-pass sites); 5 (the unused `history` parameter in `status_snapshot.go`) rides on W25. (e) That a download-only pass prunes a standing write block is existing behaviour and stays. Message: `scratchpad/B2-msg.txt`.
- **Second-pass review (mine, 2026-11-12, on `77e771f7`)**: the ten items are done and accepted, with the writer's two deviations: the follow-up work a run ends on is counted with the mode in mind (`Unfinished`), and the status snapshot's one-transaction test uses a test hook field (`betweenSnapshotReads`), which is this package's convention. Two defects remain, both for a third pass. (f) **What a run left is counted on its last pass, by the engine. This replaces (c).** After the second pass the console's path sets and failure-event count span every pass and `Report.Failed` is summed over passes, while `LeftForNextSync` and the plan are the last pass's. So a file that reported "will retry" in pass 1 and is held without an event in pass 2 counts twice; a file that failed in pass 1 and has no action in pass 2 still makes the run exit 1; and a failed bookkeeping action of an earlier pass that a later pass redid still counts. Rule: each pass counts, beside its failures, the failures kept for retry, by one predicate over the result class that the activity's `WillRetry` uses too; the report carries the last pass's two counts as `withPass` already carries `LeftForNextSync` (work adds up, a state is the last pass's); `Failed` stays the sum of failed actions over the passes and is read only by the log; one exported method on `Report` returns failed, will retry and still open (zero for nil and for a dry run), and no other code adds those fields up. The console tally loses its path sets and its failure-event count and is display only. The plan-invariant branch counts as failed. If the writer finds a retry row that can sit outside the last pass's counters (the rows a pause scope holds are the case to check), it stops and reports. (g) **A removed shared folder's final drain is clean only when its run left nothing.** `classifyShortcutChildDrainReport` reads clean from `Failed == 0` and no errors. A clean drain is acknowledged by the parent, which releases the shortcut root and removes its whole local tree (`removeShortcutRootProjection` calls `RemoveTreeNoFollow`), and then the child's state is purged. A child run that left work without a failure (follow-up work, work a pause holds, a local file that cannot be uploaded) therefore loses the local files that were never uploaded. This is on main and is closed here, because the rule is the exit status's rule read by a second consumer and the unsynced-issue count is new in this change. The classifier reads the method of (f); a dry-run report is never clean (today a dry run starts no child, because the parent publishes no child work; the classifier does not depend on that); a drain whose last pass holds deletions of local files is blocked too, because the hold exists so that the user's local files are not removed unasked and the release removes exactly those; held deletions of remote files do not block. The writer lists every other way a local-only file can exist after the last pass while the method returns zero (a conflict copy made in the last pass is the case to check) and makes each block the drain. The final drain runs bidirectional with a full reconcile, in one-shot sync and in watch, so nothing there is deferred by mode. Message: `scratchpad/B2-msg3.txt`.
- **Third-pass review decisions (mine, 2026-11-12, on `f5b735a4`)**: the writer asked whether the release of a removed shared folder should check the tree before it removes it, and whether files the filter hides may go. (h) Yes, it must check: the release walk removes every regular file with no check against the baseline, no verification and no trash, against the rule for any folder deleted on OneDrive (the trace and the rule are in W30). (i) No: a file the filter hides may not go, nor any other file sync has not verified as synced. Neither is done in this PR: its sentence in R-2.4.8 says when a drain is clean, not what the release removes. W30 follows B2 directly, and B2's PR body lists the release under what is not in the change. The writer's list is wrong on one point: symbolic links and special files are not removed; the walk stops on them with an error, after a partial removal.

#### B3. Control socket: deadlines and honest client messages

- **Source**: `02` section 7; D15.
- **Change**: read, idle and write deadlines and a header limit on the server;
  a request the loop does not accept within 5 s answers `busy` (503); a reload
  not finished within 60 s answers `reload_in_progress` (202); perf capture at
  most 5 minutes; an unexpected `Serve` return ends the watch through a fatal
  channel. Client: messages by outcome; `status` says when the running sync
  did not answer. Four wrong `// Validates:` ids are corrected.
- **Docs**: R-2.9.3, new R-2.9.7, `sync-control-plane.md`, `cli.md`.

#### B4. Config and catalog locks: bounded wait, catalog updates locked

- **Source**: `02` section 6; `08` items 2 (catalog lock part) and 11.3; D6.
- **Change**: `withConfigMutationLock` polls a non-blocking `flock` and fails
  after 10 s with `ErrConfigLocked` naming the directory. Catalog
  read-modify-write (daemon and CLI writers) runs under the same lock.
- **Tests first**: `TestWithConfigMutationLock_HeldLockTimesOutWithTheDirectoryNamed`,
  `…AcquiresWhenTheHolderReleases`,
  `TestSetDriveKey_CrossProcessHolderIsNotWaitedForForever`,
  `TestUpdateCatalog_CrossProcessUpdatesAreNotLost`.
- **Docs**: new R-4.4.4, `config.md`.

#### B5. One token source per sign-in file, on a tuned HTTP client

- **Source**: `02` sections 5 and 4(a); `08` items 11.1 and 11.5; D6.
- **Change**: `SessionRuntime.BootstrapClient(ctx, tokenPath)`; the seven
  direct `TokenSourceFromPath` calls in `cli` go through it; a source-scan
  test keeps it so. `graphtransport.AuthClient()` (30 s headers, 60 s
  request) is passed to the token source and both login flows; a nil auth
  client is an error; `graph.NewClient` refuses a nil HTTP client instead of
  falling back to `http.DefaultClient`.
- **Tests first**: `TestSessionRuntime_BootstrapClient*`,
  `TestTokenSourceFromPath_{RefreshUsesTheInjectedClient,StalledTokenEndpointTimesOut,NilAuthClientIsAnError}`,
  `TestNewClient_NilHTTPClientIsAnError`.
- **Docs**: new R-6.8.27, `graph-client.md`, `sync-control-plane.md`.

#### B6. oauth2 fork removed; a failed token save is an error

- **Source**: `02` section 4(b), 4(c); D9. After B5.
- **Change**: the `replace` directive and `OnTokenChange` go; a
  `persistingTokenSource` around the upstream source saves each new token
  atomically, never writes an older one, keeps and retries a failed save.
  `tokenBridge.SaveError()` and `SessionRuntime.TokenSaveErrors()` carry it to
  the command (exit 1) and to the daemon (log once, status line, exit 1 when
  stopped with the error set).
- **Tests first**: `TestPersistingTokenSource_*` (three),
  `TestTokenBridge_ForceRefreshIsSaved`, `TestSessionRuntime_TokenSaveErrors`,
  a CLI exit-1 case.
- **Docs**: new R-6.8.26, `graph-client.md`.

#### B7. Token refresh coordinated across processes

- **Source**: `08` item 9 (verify, then fix). After B6.
- **First**: a test with a token endpoint that rotates refresh tokens and two
  sources on one file shows the loser's refresh being refused. If it cannot be
  made to fail, the PR is the test and a note, nothing else.
- **Change**: `tokenfile.AcquireLock(ctx, tokenPath)` (90 s); before a refresh
  the source re-reads the file under the lock and adopts a newer token; login
  installs its token under the lock; logout takes the lock and tells a running
  daemon.
- **Tests first**: `TestRefresh_*`, `TestForceRefresh_AdoptsNewerTokenFromDisk`,
  `TestLogout_NotifiesDaemon`, `TestLogin_InstallsTokenUnderLock`.

#### B8. Watch runners: identity, bounded stops, fail closed

- **Source**: `02` sections 1, 9.1, 9.2; D12.
- **Defect**: a runner event carries only the mount id, so an event of a
  replaced runner is applied to its successor; five places wait on
  `<-wr.done` with no bound; `closeEngine` has no timeout; the run mode is
  missing from runner equivalence; the owner lock is released before the
  runners are drained, and `drive reset-sync-state` does not take the root
  lock.
- **Change**: runners and events carry a generation, stale events are ignored;
  one `retireWatchRunner` waits with a bound (30 s stop, 15 s close, checked
  against the engine's own shutdown bound first); on timeout the runner is
  marked stuck, left alone, logged with stacks, and `RunWatch` returns
  `ErrWatchRunnerStuck`; runner sends also select on their context; the owner
  lock is released after the drain; reset takes the root lock.
- **Tests first**: the eight tests named in `02` 1.
- **Docs**: new R-6.3.9, `sync-control-plane.md`,
  `docs/running-as-a-service.md`.
- **Validation**: live watch on `~/sync-test-root`: reload with a drive paused and
  resumed, SIGTERM during a transfer; the daemon exits inside the bound.

#### B9. Panic boundaries everywhere

- **Source**: `02` section 2; D13. After B8.
- **Change**: leaf package `internal/panicguard` (`ErrPanic`, `Recovered`);
  recover at `RunWatch`, at daemon helper goroutines (reported on the fatal
  channel), at CLI goroutines, in one-shot helpers and in `mainWithWriters`
  (exit 1); the control server logs handler panics through slog.
- **Tests first**: `TestEveryGoroutineHasAPanicBoundary` (source scan),
  `TestRunWatch_LoopPanicIsLoggedWithStackAndReturnsAnError`,
  `TestMainWithWriters_PanicExitsOne`; the panic-site test is rewritten.
- **Docs**: new R-6.3.10.
- **Decisions (mine, 2026-11-12)**: (1) the goroutine test finds its packages by walking `main.go`, `internal/` and `pkg/`, so a new package is covered without editing it; what it leaves out is a named list with a reason each; its allowlist has a reason per entry and fails on an unused entry. (2) The last defence in `mainWithWriters` writes the panic value and stack to stderr as well as to the log (a one-shot command may have no log file). (3) The documented systemd unit gets no start limit: an unattended sync client that stays down until someone notices loses more than one that retries every 30 s and logs each crash with its stack; `docs/running-as-a-service.md` says what a repeating crash does and how to make the unit give up. (4) Existing recover sites keep what they do and log through the one helper. (5) What #876 and #882 already built (a watch whose control server stops exits; bounded runner stops) is reused, not repeated.
- **Review decisions (mine, 2026-11-12)**: no defect in the delivered paths of the first pass (3866d726); all eight deviations accepted (the one-shot control-server exception is written into R-6.3.10). (a) **Exit status**: a recovered panic anywhere in the command's final error exits 1, ahead of the typed 77 and 78: a crash is never reported to a script as "sign in again" or "fix the config". (b) **Timer callbacks are goroutine starts**: the scan counts `time.AfterFunc`; in `internal/sync` the real clock's one callback boundary fails the engine run the timer belongs to (the run's context is canceled with the panic as its cause; `RunWatch` and `RunOnce` return it), so the drive fails alone and restarts with backoff; the engine builds one real clock and hands it to the observers and the socket.io source (four constructions become one); graph's transfer watchdog cancels the transfer with the panic as the cause. Rejected: a log-only wrapper (a drive that stops retrying or refreshing while its status reads healthy) and turning the timers into channel timers (too large here). (c) **One report per drive**: a panic after a drive's report exists is joined into that report, never dropped and never a second report. (d) **Startup order**: `RunWatch` registers the drain and the socket close before anything starts. (e) Found in review: `startChildrenForParent` holds the coordinator's mutex without `defer`, so a panic there deadlocks the boundary's own `appendReports`; fixed first in the second pass, with a list of every lock a recover block takes. (f) The four "Seen and not fixed" entries, the scan's two fail-open cases (a non-literal `Go` argument, a callee from an imported package), the history comments, the five copies of a test log handler and the package doc go into the second pass. Message: `scratchpad/B9-msg.txt`. **Second-pass review (mine, 2026-11-12)**: items 1 to 4, 5a to 5c and 6 to 9 are done in f493dca3 and accepted; the writer stopped on (b) and was right to: `internal/retry` and `internal/graph` wrap `context.Cause` of the request context, and sync reads a shutdown only as `context.Canceled` or `context.DeadlineExceeded`, so a run canceled with the panic as its cause would record every in-flight action as a failure. **(b) as revised**: the real clock's timer boundary records the recovered panic in a sink the engine owns and cancels the run's context plainly (no cause); `RunWatch` and `RunOnce` each run under a context derived for the run, read the sink as they return and return the `panicguard.ErrPanic` error (joined with the run's own error when that is not a shutdown), so the drive fails alone and restarts with backoff (R-6.3.8) and in-flight actions read a shutdown. With no run bound the boundary's log is the whole effect. The engine hands its clock to the observers too (found in review: they kept their own `realClock{}`), and a source-reading test holds that product code builds a real clock in one place. In graph the watchdog's context is one transfer's, so there the panic is the cancel cause and `explain` returns it. Also in the third pass: the `readerReturned` test hook becomes `readerDone` on the connection; the history comment in `scanner.go`; multisync's own `logCapture` replaced by `synctest.LogCapture`; the two exit-status join rows seen red. Message: `scratchpad/B9-msg3.txt`.
- **Live proof**: none (unit tests and the source scan are the proof).

#### B10. Loop heartbeat (observe only)

- **Source**: `02` section 3; D14. After B3, B4, B8, B9.
- **Change**: `loop_heartbeat.go`: the loop beats at every select iteration
  and on a 15 s tick; each long step declares itself with the bound that
  holds it; the watchdog goroutine reads `LoopAlive()` and logs one error when
  the loop has been silent past its limit, but still pings.
- **Tests first**: `TestRunWatch_BeatsOnEveryIterationAndWhileIdle`,
  `TestLoopHeartbeat_DeclaredStepExtendsTheLimit`.
- **Before**: every long loop step has a real bound (the list is in `02` 3).

#### B11. Watchdog ping follows the heartbeat

- **Source**: `02` section 3. Gate: D14.
- **Change**: `WATCHDOG=1` is sent only while the loop is alive; the first
  withheld ping is logged and sent as the status line.
- **Tests first**: `TestRunWatchdog_PingsOnlyWhileTheLoopIsAlive`.
- **Docs**: R-6.6.32, `cli.md` (also the false "there is no SIGHUP reload"),
  `docs/running-as-a-service.md`, the unit file comment.

### Wave C: fail-closed planner and the store's foreign key (increment 3)

Source: `03-06-10` Part A. The report corrected two things in my first draft:
the SQL keeps its literals (a token test and two SQL-evaluated exhaustive tests
prove SQL and Go agree; nothing is generated), and the foreign key needs its
own column (D17).

#### C1. Planner kinds are typed; an unclassified row fails the plan

- **Source**: `03-06-10` A1 (its PRs A-1 and A-2); D16.
- **Finding**: `ELSE 'noop'` is unreachable today, so this is a guard for the
  next change to the table, not a live bug. Kinds are untyped strings in Go.
  Two `WHEN` branches are dead.
- **Change**: `planner_kinds.go` with `comparisonKind` and
  `reconciliationKind` (zero value = unclassified, `String`, `parse…`,
  `all…Kinds`); row scans parse and fail with the path; both `ELSE`s become
  `'unclassified'`; the dead branches go; the two `CASE`s become named SQL
  constants; `unclassifiedPlanError` carries the path and its 17 input flags
  from a diagnostic query run only on the error path; a validation pass runs
  before the planner's early return; `errFolderTransferPlanned` is returned;
  `actionAllowedInMode` and `Direction()` lose their permissive defaults.
- **Tests first**: token test (every Go kind appears as a SQL literal and the
  reverse); scan test with a doctored query returning `'bogus'`; stage 1 over
  all 131,072 comparison states; stage 2 over 442,368 reconciliation inputs
  (behind `skipInShort` if slow); an engine test asserting
  `errors.Is(err, errUnclassifiedPlan)`; the wrap text of one-shot and watch;
  the `diverged` cell with a local change, no remote change and equal content.
  The matrix golden stays byte-identical.
- **Docs**: `sync-planning.md` (GOVERNS gains the new file).

#### C2. Observation issue types are typed

- **Source**: `03-06-10` A1.7 (its PR A-3).
- **Change**: `observationIssueBlocksLocalTruth` switches on a typed issue
  type with the `exhaustive` linter instead of an untyped string with a
  fall-through. If the switch shows a type that was missing, that is fixed
  with its own test in this PR.

#### C3. Foreign key and CHECKs in the store (generation 29)

- **Source**: `03-06-10` A2 (its PR A-4); D17. After E4.
- **Change**: the schema of D17 plus CHECKs the runtime invariants assert
  today (`attempt_count > 0`, the blocked and next-retry pair, non-empty
  path); one store method writes the scope row and the blocked row in one
  transaction; `block_scopes` upserts instead of `INSERT OR REPLACE`;
  `DiscardScope` deletes children before the parent; releasing a row clears
  its key; the orphan repair at start-up and four runtime invariants are
  deleted because the database now refuses the state.
- **Tests first**: `foreign_keys` is on; `foreign_key_list(retry_work)` lists
  the key; a blocked row without its scope fails; deleting a scope that still
  has blocked rows fails; discard and release leave `foreign_key_check` clean;
  each CHECK rejects its bad row; a generation-28 store is refused; a crash
  between "scope written" and "blocked row written".
- **Rider from W10 (D27)**: the stored issue type `hash_panic` is renamed to
  `hash_error` here (constant in `internal/sync/issue_types.go`, the two
  scanner sites, the condition-key switch, `spec/design/sync-domain.md` and
  `sync-store.md`), because this PR already bumps the generation and a
  rename of a stored value needs one. Test first: a generation-28 store is
  refused (already listed); no source file names `hash_panic` afterwards.
- **Install**: the reset procedure of section 7.

### Wave E: the engine (increment 5)

Source: `05` (its PR ids in brackets). E1 to E4 before the split, E5 to E13
after it (D22).

| ID | What | Tests first | Notes |
|---|---|---|---|
| E1 | Delete `trackedAction.Cancel`, `depGraph.MarkTrial`, `Engine.diskAvailableFn`, the stale `depGraph` capture; fix nine stale comments; read `protectedRoots` under its lock in housekeeping [E0, E1] | `TestPostSyncHousekeeping_ReadsProtectedRootsUnderTheLock` (race) | F5 is a real data race. Decision (mine, 2026-11-13): seven of the nine comments are fixed here; the two that say "blocked retry work" stay for E4, which makes the SQL match them (D18). E1 lands before E3 (`05` 10.1: E3 depends on the dead-code deletion). |
| E2 | Start no observer before the fallible local set-up has succeeded; `RunWatch` returns only after its observers have exited (bounded, logged) [E2] | `TestStartObservers_LocalSetupFailureStartsNoObserver`, `TestRunWatch_ReturnsOnlyAfterObserversHaveExited`, `TestAckHandle_ConcurrentWithFullRefresh_IsRaceFree` | The wait is a behaviour change; its bound is set against B8's. |
| E3 | Delete `scopeLedger.mu`, `watchTimerState.timerMu`, `depGraph.mu` and four atomics on `Engine`; a test-only owner check asserts single-goroutine use [E3] | `TestWatchRuntime_TimerCallbacksTouchOnlyChannels`; three concurrent dep-graph tests go | Proof: `-race` on the engine tests and the stress lane, here and in CI. |
| E4 | `retryWork`: the only engine code that calls a `retry_work` mutator; its cache is written only from a store result; one `discardScope`; three store methods return what they changed [E4] | `TestEngineFlow_RetryCacheMatchesStoreAfterEveryCompletion`, `…DiscardScope_LeavesNoCachedRowTheStoreDeleted`, `…ReleaseScopeBatch_CacheEqualsStore`, `…ResolveWithoutStoreRow_ClearsTheCachedRow` | D18 is decided by the discard test. |
| E5 | Watch lifecycle as a transition table of six phases and five events; the select reads the inputs enabled in the phase [E5] | `TestWatchPhase_TransitionTableIsTotal`, `…NothingLeavesDrainingExceptDrained`, `TestWatchLoop_ServicesOnlyThePhasesEnabledInputs`, `TestRunWatch_CancelDuringBootstrapStopsWithoutStartingObservers` | |
| E6 | `engineHooks`: the ten test hooks of `Engine` in one struct whose zero value is production [E6] | `TestEngineHooks_ZeroValueIsProduction` | About 44 assignment sites. |
| E7 | `runConfig` and `flowInputs`; the 35 ambient uses go through `runtimeDeps`; config and hook reads stop reaching the engine [E7, E8] | the pinning tests named in `05` 1 stay green; `TestEngine_EachRunGetsADistinctRunID` | Rider (2026-11-13): receiver names made consistent on the way (`flow` for `engineFlow`, `e` for `Engine`; `05` 9.3), since this change touches nearly every engine file anyway. |
| E8 | `protectedRootSet` and `housekeeper` own their state [E9] | `TestRunWatch_HousekeepingRunsBeforeTheFirstRefreshTimerIsArmed` | |
| E9 | `observation`: the methods that run off the loop goroutine live on their own type with send-only outputs [E10] | the refresh and panic-recovery tests named in `05` 2 | Shapes follow G6 and G7. |
| E10 | `shortcutRoots`; start-up takes the flow; `engineFlow.engine` is deleted [E11] | architecture tests in `engine_runtime_architecture_test.go` assert the field is gone | |
| E11 | `syncexec.Hooks`, `syncstore.Hooks`, `syncobserve.Hooks`; a devtool check that hook literals appear only in tests [E12, X2] | per-package zero-value tests | |
| E12 | `syncsched`: dependency graph, held work, scopes and admission as one package with the API in `05` 3 [X1] | `TestScheduler_InstallReplacesEveryPerPlanStructure`, an `Admit` table, characterisation tests through the flow first | Needs E3 and E4. |
| E13 | `newExecutorConfig(executorInputs)` and `newWorkerPool(workerPoolInputs)` replace 14 setters [X3] | `TestNewExecutorConfig_RejectsEachMissingRequiredField`, `TestWorkerPool_ParentChecksAreResetPerPlanAndSharedByWorkers` | About 45 call sites. |

Docs for all: `sync-engine.md` (ownership contract, construction, run state,
verified-by list).

### Wave G: names and local observation (increment 7)

Source: `07` (its PR ids in brackets). The report corrected the premise of this
increment. Name equivalence is already central in `internal/namekey`, so there
is no scattered folding to gather; what is missing is typed keys, one lookup
rule (ten matchers use five tie-breaks) and one stored-form function. And local
change events are not even wake hints: production passes no event channel and
every replan scans the tree again. G1 to G8 land before the split, G9 to G11
after it.

| ID | What | Tests first | Notes |
|---|---|---|---|
| G1 | `namekey.Key`/`Of`, `PathKey`/`OfPath`/`HasPrefix`; 25 sites retyped; the two identical planner helpers and `isWithinRemotePath` use them; the second R-2.8.12 is renumbered [P0, P1] | `TestOfPath_HasPrefix_SegmentBoundary` | No behaviour change. |
| G2 | `namekey.Relate`, `Find`, `FindStored`; the matchers in `graph`, `driveops`, `cli`, `synctree` use them, one per commit [P2] | `TestRelate_ProbeCorpus`, `TestFind_TieBreak`, `TestMatchChildByName_TwoFoldedCandidatesIsNotFound`, `TestReadCreatedFileByName_PrefersExactSpelling`, `TestPutFile_PrefersExactSpellingOverCaseVariant` | Behaviour changes where a listing holds two key-equal children: `put` then reports the conflict instead of possibly overwriting the wrong case variant. |
| G3 | `classifyCreateConflict`: a folder create adopts an identical or same-stored-form name only; a same-name rival is "name taken"; a twin is never adopted [P8] | `TestClassifyCreateConflict_NeverAdoptsATwin` | Defect N4: adoption on NFC equality alone. Rider (2026-11-13): the two executor matchers G2 leaves in the sync packages move here: `storedNameSpelling` to `namekey.Find` and `exactLocalNameExists` to `namekey.FindStored`, each with a two-candidate test. |
| G4 | A Unicode-form twin of a synced file is a collision instead of silently replacing its row; a watch delete always removes the path and everything below it [F4, F3] | `TestFullScan_UnicodeFormTwinOfSyncedFileIsACollision`, `TestRunOnce_UnicodeFormTwinOfSyncedFileUploadsNothing`, `TestHandleDelete_UnwatchedNewDirectoryRemovesSubtreeRows` | Defects C1 and L6. The subtree delete is measured on a large table first. |
| G5 | Delete the local event channel, create-versus-modify classification, `detectDeletions`, the move arm, `LastActivity`, the delete type guess; fix five stale comments [P3] | `TestFullScan_DeletedSyncedFileHasNoRow`; about 85 test sites move to the batch channel | D20 is written into `sync-engine.md` here. |
| G6 | `localObservation` = `localPatch`, `localSnapshot`, `localTruthSuspect`; a scan's findings and crossed mounts travel with its rows; `skippedCh` and its handler go; one `commitLocalSnapshot` for the watch and the replan [P4, F1] | `TestRunSafetyScan_ClearedLastFindingReplans`, `TestRunFallbackScan_PersistsNewFinding`, `TestWatch_CreateSendsOneUpsertPatch`, `TestWatch_DeleteSendsSubtreeRemoval` | Defects L1, L2, L3, L7: a finding that cleared could stay listed for up to a day. Rider (2026-11-16, from a fact check against main `ca287a32`, kept in the row's brief): L1 and L2 are fixed on main since 2026-11-11 (every safety and fallback scan sends its finding set, the empty one included; R-2.8.22), so the sentence before this rider no longer describes main. What is left is the mechanism: L3 (a scan's rows and its findings travel on two channels and commit in two transactions; a dropped finding set asks for no recovery scan) and L7 (a watch snapshot carries no crossed mounts, the observer's mount flags are loaded once per watch). `commitLocalSnapshot` already exists (`engine.go`, rows and crossed mounts): the row extends it to findings and makes the watch use it. None of the four named tests exists; the brief names tests for what is left. Decided: rows, findings and mounts of one scan commit in one transaction, in the watch and in the replan alike; the one commit function binds the root as the replan's does; the dry run's scratch commit uses it only if it fits without a second code path. |
| G7 | `remoteChange{Path, OldPath, ItemType}` replaces `changeEvent` on the remote side; `changeEvent`, `changeType`, `changeSource` are deleted; the content filter is built once per batch [P5] | `TestRemoteBatch_PlannerVisibleEffect_BuildsFilterOnce` | Rider (2026-11-16, from a fact check against main `ca287a32`, kept in the row's brief): `changeSource` is already gone. Building the content filter is a struct copy, so "built once per batch" has no value: that item and its named test are dropped. The row's scope is the remote side whole: the converter and the orphan finder build the stored row form and a `remoteChange` directly, no remote file names `changeEvent` (12 product files, 57 lines on main), the write-only `Type`, `CTag` and `Name` go, the return value the one caller drops goes, and the product functions only tests call (`LastActivity` of the remote observer, `observeRemoteFull`, `projectObservedItems`) go. The change list keeps exactly the entries it holds today, those with an empty item ID included, because the cursor decision reads its length. The type itself and `changeType` are deleted by W97, once the scanner no longer uses them. |
| G8 | `namekey.Stored`/`StoredPath`; one `storedPathOf`; `nfcNormalize` deleted [P6] | `TestStoredPath_EqualsPerSegment`, `TestOf_StoredComposition_KelvinClass` | D21. `namekey.Stored` itself is added by G2 (its lookup needs it, decided 2026-11-13); G8 adds `StoredPath`, the per-segment property and the removal of `nfcNormalize`. If the per-segment property fails, joined paths are normalised per segment. |
| G9 | `dirNames` and `localNameCollisions`: one detector for scan and watch, on-disk rivals only; the baseline name index and `RecentLocalDeletes` are deleted [P7] | `TestHashAndEmit_BaselineVariantNotOnDiskIsNotACollision`, `TestFullScan_ModifiedSyncedSiblingOfNewColliderBlocksBoth`, `…UnchangedSyncedSiblingKeepsSyncing`, `TestDirNames_ProbeCorpus`, `TestLocalCollisions_ScanAndWatchAgree` | D19. Defect C2. A followed symlink visiting one stored path twice needs a case first. F1's `baselineView` has no `GetCaseVariants` after this. Rider (2026-11-13): the local-disk matchers `onDiskSiblingName` and `localPathStillPresent` use `namekey.FindStored` here, or go with the detector they serve. |
| G10 | A scan reuses the hash in the committed local row when size, times and identity match and the file is outside the racily-clean window [P9] | `TestFullScan_ReusesCommittedLocalHash` (counting hash function) | Only if a replan of a large synthetic tree under `~/sync-test-root` shows the saving; otherwise closed with the measurement. |
| G11 | Watch-side collision detection (3 fields, 9 functions) deleted; R-2.12.2 reworded to "the survivor syncs on the next pass" [P10] | `TestLocalCollisions_ScanAndWatchAgree` becomes a scan-only test | After G9 has shown the scan detector complete. |
| G12 | The single-name limit counts UTF-16 units against OneDrive's limit; the message is correct; a requirement beside R-2.11.10 [F2] | `TestValidateOneDriveName_MaxLength` | The limit lies between 300 and 400; a live probe on the test account fixes it first. |

### Wave S: split `internal/sync` into seven packages (increment 4)

Design: `../onedrive-go-sync-split-2026-11-10/end-state.md`. The model there is
pinned to `70301c0e`; everything before this wave changes `internal/sync`, so
the first step re-runs it.

The method is "prepare in place, then move by tool". Every preparation PR
leaves one package that compiles and passes. The move itself is generated, so
it is reviewed as a tool plus a verdict, not as 360 hand-edited files.

| ID | What | Notes |
|---|---|---|
| S0 | Re-pin the model to HEAD | `tools/run.sh` on `conf/production.txt`, `mkconf.py`, `run.sh` on `conf/final.txt`; fix the configuration until the verdict is 0 violations, 0 cycles on Linux and macOS. Inventories in the design are regenerated. No onedrive-go change. |
| S1 | Export identifiers | The capitalisations in the regenerated `exports.tsv`, by a rename tool over the type-checked package. Pure rename. |
| S2 | Declarations to their target file | Declarations that leave their file family move to a file that belongs wholly to one target package; mixed files are split. Pure moves. |
| S3 | Fields, methods, interfaces, errors | The new methods, the `syncexec.Store` and hash-cache interfaces, the sentinel errors with `Err` prefix (design sections 4 to 6). |
| S4 | Tests and helpers | The `…ForTest` functions, the two helper packages' contents gathered into their files, test files split so each belongs to one target package; a devtool check that `ForTest` identifiers are used only from tests and helper packages. |
| S5 | The move | A purpose-built tool (go/packages and AST rewrite) changes package clauses, qualifies cross-package uses, rewrites imports in `cli`, `multisync`, `syncverify`, `e2e`, moves files and `testdata`. Same PR, by hand: `doc.go` per package, GOVERNS lines and the design docs, depguard rules, the wrapcheck exclusion, lint path exclusions for the helper packages, the panic-site test's roots, the routing table in `CLAUDE.md`. |
| S6 | Layering by depguard; un-export | `verify_layering.go` is replaced by depguard rules (`09` PR 8). Anything S1 exported that ended up used only inside its package goes back to unexported. |

Amendments to the design, from the later analyses:

- The NFC helper does not move to `syncdomain`; G8 deleted it for
  `namekey.Stored` (D21).
- `changeEvent`, `changeCreate`, `dirNameKeyFromPath` and the observation
  batch types have changed shape or are gone (G5 to G7), so their export rows
  go.
- `dep_graph.go`, `active_scopes.go` and `scope.go` move as the design says in
  S5; E12 then gathers them into `internal/sync/internal/syncsched`.
- The pool setter `SetRuntimeOptions` and the executor `…ForTest` setters are
  exported by S1 as designed and removed by E11 and E13.

Other notes:

- **Fallback**: if leaf-first moves are not possible because store and observe
  tests import each other, S5 is one PR. That is the expected case.
- **Validation of S5**: the model's verdict on the result (it must report the
  split as the identity configuration), `go build ./...` for Linux and macOS,
  `go vet`, the whole unit tier with `-race` (CI), the stress lane, and a
  `workflow_dispatch` full tier including live e2e before the merge.
- **Measured here, not in the design**: stale `//nolint` lines after the move,
  wall time of seven test binaries against one, coverage under the A8 gate.

### Wave F: one owner for state, the planner in Go (increment 6)

Source: `03-06-10` Part B (its PR ids in brackets). After the split.

| ID | What | Tests first | Notes |
|---|---|---|---|
| F1 | Code outside the store holds a `baselineView` that returns entries by value; maps and mutators are unexported [B-1] | `TestCommitMutation_FolderMoveKeepsHeldBaselinePointerCurrent` is rewritten for values; the cache race test stays green | Planner and scan benchmarks before and after. |
| F2 | The cache reload is read inside the transaction and swapped after commit, so nothing can fail after `COMMIT`; `CheckCacheConsistency` compares every field and joins the test invariants; the plan holds a distinct snapshot type; a dry run reads the database [B-2, B-3] | a store-fault test that fails the reload read and finds the cache unchanged; `TestPlanning_UsesBaselineFromTheComparisonSnapshot` | |
| F3 | `decisionState`, `decide`, `deriveFlags` in Go; the SQL still decides and every plan compares the two [B-4] | exhaustive test over the 8,388,608 states with `valid()`; "no upload of a local folder, no download onto a remote folder" as a property; the (absent, root, absent) cell decided | Needs C1. |
| F4 | Go decides; the two SQL `CASE`s and their second evaluation of the comparison chain are deleted; set-valued parts (move pairing, ancestor sets) stay in SQL [B-5] | matrix golden byte-identical; planner and engine model seeds; benchmarks recorded in `performance-benchmarking.md` | Reverted if the 100,000-item plan is slower than the 23 s it takes today. |
| F5 | A fake `syncClock`; the eleven direct wall-clock calls go through it; seeded jitter; a lint ban on `time.Now` outside the clock file [B-6] | the model runs with a fake clock | |
| F6 | In-memory `synctree` with capability flags (case folding, normalisation); the direct filesystem calls go through `synctree` [B-7] | an NFD scenario that reproduces `LI-20261029-01` on Linux | |
| F7 | A scheduler seam (`Go`, `After`, `Now`) at the ten goroutine launch sites; watch mode in the model; one seeded fault plan; the cache-consistency check in the invariant library [B-8] | the watch model converges on the nightly seed set | Becomes the primary test: A11's seed rotation runs it. |

### Wave H: outer packages (increment 8)

Source: `08`. Runs in the outer lane alongside waves B to G. Items 2 (lock
part), 6, 9 and 11.1, 11.3, 11.5 of the report are in wave B.

| ID | What | Tests first | Notes |
|---|---|---|---|
| H1 | `fsroot` remove, rename, mkdir and temp-file through `os.Root` | `TestRoot_*_RefusesSymlinkedParent` | Item 7. Verify first: a test that shows the escape. Also the two sync-side callers the report names. |
| H2 | `retry.ParseRetryAfter` and `ResponseRetryAfter`; both callers use them | table: seconds, overflow saturates, HTTP-date against `Date`, spaces, garbage | Item 3; D3. |
| H3 | A 401 retry whose body cannot be rewound returns an error instead of sending an empty body | `TestDo_401WithNonSeekableBodyFailsLoudly` | Item 8. |
| H4 | `config.AddDrive`: config lock, then catalog lock, the config write is the commit point; both `drive add` paths use it | `TestAddDrive_*` | Item 2; R-3.3.5, R-3.3.1. After B4. **Decisions (mine, 2026-11-12)**: login is the same class and is in this change (it installs the token, runs the same transaction with the account record in the catalog step, and puts the previous token back on failure; a catalog failure is an error, not a warning); a half-done enrollment is completed and reported as completed in text and JSON; the rollback helpers, their seam and their tests are deleted; the lock and the wait bound are the ones #870 made, no second copy. Live proof is mine, in an isolated tree. Brief: `scratchpad/H4-brief.md`. **Review decisions (mine, 2026-11-12)**: no defect in the first pass (4f4e7204); accepted: one rule for both `drive add` paths (the login is checked first, so a configured shared drive without a login is an error), the collision-free `sync_dir` for shared folders, refusing a config that would not load and writing nothing when complete, `completed` in text and JSON, `Completed` = the section exists and the section, the record or the folder was lacking. Of the seven "Not fixed" entries six go into the second pass (guidance names `drive add <id>`; value kind per key; section headers round-trip; the test file named after a plan label; and the two catalog entries as one decision: **the catalog is refused whole on an ID that does not parse**, error naming file, key and recovery, the silent skips deleted) and one rides on W27 (`AppendDriveSection`). Mine in the second pass: `validate()`'s error states the fault; the spec says a failure leaves the empty sync directory; two test comments. On the rebase after W17 and W14: no path takes the catalog lock and then the config lock. **Second-pass review (mine, 2026-11-12)**: the eight items are done in a72ff6d2 and accepted, with `canonical_id` = the record's key as part of the refusal. Decided for the third pass: (a) **the encoder runs the decoder's check**, so a writer bug fails its own write and leaves the file as it was, instead of leaving a catalog every command refuses; (b) the key must be the ID's own rendering if any other spelling parses; (c) **the child-mount catalog prune step is removed** from the shortcut child purge (nothing writes such a record and the decoder refuses one; the writer checks the history for a version that wrote one and stops if it finds it); (d) one accessor per catalog reference (`OwnerAccountCanonical`, `PrimaryDriveCanonical`), the six parse sites go through it and their unreachable branches are deleted; (e) the "loading config:" wrap in `account_view_snapshot.go` names what the call loads. The dead skip branch in `email_reconcile.go` rides on W22 (W17 rewrote that file). Message: `scratchpad/H4-msg3.txt`. **Third-pass review (mine, 2026-11-12)**: the five items are done in e31337ec and accepted; no defect; my one fix (the `purge` doc comment) amended as b073989e. The exact-rendering rule only bites degenerate SharePoint spellings, which the parse accepts and the render drops: that is a `driveid` fault and rides on W13. |
| H5 | The upload-session `UnmarshalJSON` shim and the dead code the report lists are deleted | strict-load test for a session file in the old shape | Item 4. |
| H6 | One JSON encoder constructor; 17 own copies use it; the NDJSON path returns its marshal error; forbidigo keeps it so | golden test for escaping and empty lists | Item 5; D4. |
| H7 | `driveops.DownloadTree` and `UploadTree` with a pool of 4; a remote child name that is not local is refused | `TestDownloadTree_*`, `TestUploadTree_*` | Item 1; D5; R-6.6.29. Live: `get -r` and `put -r` of a 200-file tree under `~/sync-test-root`. W18 already merged the two `get` walks into `cli.downloadTree`: start from it. Rider (from W18's report): a command-level test of a shared single-file `get`, which needs a download seam for cli tests because a download URL may only name a Microsoft host (R-6.8.22). |
| H8 | The remaining silent failures the report lists; the drive-catalog fan-out is bounded; the output leftovers of Item 10 that H6 and H7 do not take (`put` folder progress, dead output plumbing in `status`) | one test per site | Items 10 and 11.4, and the 11.5 rows B5 did not take. The four "cosmetic leftovers" of `BACKLOG.md` are in the sync packages and are W41 (split 2026-11-13). **Rider (2026-11-14, from the review of W31's second pass, its note 6)**: `internal/cli/signal.go` (the recover of the signal handler) and `internal/cli/sync_runtime.go` (`failWatch`, when it is given a panic) cancel the run with a panic error as the cause. By reading only, that cause does not answer to `context.Canceled`, so a request the cancel interrupts carries it in place of `context.Canceled` and reads as a failure during the stop instead of the stop's echo. DoR: a test that shows it. Fix: the cause answers to `context.Canceled` and still to `panicguard.ErrPanic` (a type like the sync package's `cancelCauseError`), so the stop is quiet and the command still exits 1. |
| H9 | Split from H1 (2026-11-11): the directory conflict copy (`internal/sync/executor_conflict.go`, `RenameNoReplace` on absolute paths) and `localtrash.Restore` (check, then act on absolute paths) mutate through a rooted descriptor in `synctree` | a symlink swapped in below the sync root does not redirect the rename or the restore | Item 7, table "Mutations inside the sync tree that bypass the rooted capability". Needs a no-replace rename on a rooted parent (`renameat2`, `renamex_np` on macOS). Session model. Also settle the SUSPECTED `detach_unix.go` rows. **Riders from the W14 report (2026-11-11)**: `synctree.normalizeNotExist` (`internal/synctree/synctree.go:1160`) lets a failure without an errno reach the absence check and maps ENOTDIR on a parent only when the unrooted lstat says ENOENT: give it fsroot's two-answer rule (the failure and the lstat both say not-exist or not-a-directory); the missing-name errno set is defined twice (`synctree.go:1173`, fsroot's `isMissingName`): one exported predicate in `localpath` that both use, and `internal/synctree/spelling.go:63` holds a third copy that uses it too (W14 second-pass report). |

### Wave I: identity-keyed reconciliation in shadow mode (increment 10)

Source: `03-06-10` Part C. After F4. No schema change in this wave.

| ID | What | Tests first | Notes |
|---|---|---|---|
| I1 | `planIdentityKeyed(inputs)`: one node per identity (remote and baseline by item id; local by device, inode and birth time, with a confidence that falls back to path pairing), `decide` per node, a second pure step for position conflicts [C-1] | a second golden, `matrix.identity.golden`, with every difference from the path-keyed matrix explained in the test | Not wired into any run. |
| I2 | `diffPlans(current, shadow)`; the planner model and the engine model run both planners per seed and report differences [C-2] | the models fail on an unexplained difference | |
| I3 | The live watch computes the shadow plan and reports differences as a debug event; never executed, never recorded [C-3] | a watch test that sees the event for a seeded difference | Soaks on the test drive. |

Exit criterion for the later cut-over decision: zero unexplained matrix
differences, and the shadow-driven model converges on at least as many seeds.

### Wave W: the recommendations parked in the first 17 reports (2026-11-11)

Source: `reports/SWEEP.md` (every recommendation, deviation and doubt of
the reports A1 to C2, each marked DONE, COVERED or TODO) and
`reports/SWEEP-plan.md`. Opus writers. Each entry's **Order** line says
where it sits relative to the planned PRs that share its files. From
2026-11-11 on a writer fixes what it finds in the files it touches, so
this wave should not grow.

All paths are relative to the repository root and were checked at main `05336a1a` on 2026-11-11. No two sweep PRs change the same file. Every line of `SWEEP.md` marked TODO is in exactly one entry below.

#### W1. DoD and nightly survive GitHub errors; CI logs name no drive

- **Source**:
  - Reviewer: one transient HTTP 5xx from GitHub (502 four times in a row on 2026-11-11, a 503 earlier) ends the DoD start stage. The job-log fetch in `internal/devtool/nightly.go:644-650` calls `gh` once and returns its error; so do the run list (`:590-601`) and the run view (`:620-624`).
  - A2 report, recommendation 2: `nightlyJob` (`internal/devtool/nightly.go:85-89`) and `ciRunJob` (`internal/devtool/verify_dod_ci_wait.go:163`) are two private types for the same GitHub job JSON.
  - A2 report, recommendation 5: `internal/devtool/worktree.go:90` spells "origin/main" as a literal, and `internal/devtool/cleanup_audit.go:33` declares a second constant beside `originMainRef` (`internal/devtool/verify_dod.go:24`).
  - A8 report, recommendation 1: `verify failed-test` reruns with plain `runner.Run` (`internal/devtool/verify_failed.go:56`), so a rerun whose `-run` pattern matches nothing passes.
  - A9 report, recommendation 4, and the reviewer: two hand-written go test log parsers exist. `lastFailedGoTestTarget` (`internal/devtool/verify_failed.go:63-149`) needs raw lines without a CI timestamp prefix (`:75-81`) and guesses JSON from the first `{` anywhere in a line (`:106-108`); `failedTopLevelTests` (`internal/devtool/nightly.go:668-690`) searches with `strings.Index` instead.
  - A9 report, recommendation 3: neither parser reads the "running tests:" block of a test-timeout panic, so a hung test is reported under its job's name, with no test named.
  - A9 report, recommendation 5 and its deviation, and the reviewer: `ciStallWindow` (`internal/devtool/verify_dod_ci_wait.go:99-121`) takes the longest `timeout-minutes` anywhere in `ci.yml`. With #878 that is the 180-minute e2e job, so every wait, post-merge included, waits 190 minutes for a stall, although a push to main runs only the 15- and 20-minute jobs.
  - A9 report, recommendation 1, and the reviewer: `.github/workflows/ci.yml:262-263` and `:369-371` set `ONEDRIVE_TEST_DRIVE` and `ONEDRIVE_TEST_DRIVE_2` in job-level `env`, so GitHub prints their values in every step's environment header in public logs.
  - A2 report, recommendation 3: nothing in `ci.yml` (`:88`, `:131`) logs the runner's core count, which the unit-test deadline formula (20 minutes times ceil(3 / GOMAXPROCS)) assumes.
- **Change**:
  - Every `gh` call of the nightly check and the CI waits goes through one helper that retries an HTTP 5xx or a dropped connection with backoff, at most four attempts, and then fails with a message that says GitHub answered with that status and names the call.
  - One `githubJob` type replaces `nightlyJob` and `ciRunJob`.
  - `AddWorktree` and the cleanup audit use `originMainRef`; the literal and the second constant go.
  - `verify failed-test` reruns through `runGoTestMatching`, so a rerun that selects no test fails.
  - One go test log parser replaces both: it strips a CI timestamp prefix, decodes only lines that are go test JSON events, reads raw `--- FAIL` and package `FAIL` lines, and names the test listed in a timeout panic's "running tests:" block. The nightly check, `verify failed-test` and the per-bucket failed-test lists #878 adds all use it.
  - The stall window is the longest timeout among the jobs that run for the event being waited on (pull request, or push for post-merge), read from each job's `if:` condition; the PR's e2e job gets a timeout that fits the quick tier it runs on a PR (see Order).
  - The drive identifiers leave job-level `env`: a first step reads the repository variables, masks each with `::add-mask::`, and exports it through `$GITHUB_ENV`.
  - Each verify job prints the runner's core count once (`nproc`, or `sysctl -n hw.ncpu` on macOS).
- **Tests first**: `TestNightlyCheck_RetriesAJobLog5xxThenSucceeds`, `TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound`, `TestRunFailedTestVerification_PatternThatSelectsNothingFails`, `TestParseGoTestLog_TimestampedRawLines`, `TestParseGoTestLog_BraceInARawLineIsNotJSON`, `TestParseGoTestLog_TimeoutPanicNamesTheRunningTest`, `TestCIStallWindow_PostMergeUsesOnlyPushJobs`, `TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs`, and `TestCIWorkflowSetsNoTestDriveInJobEnv`, which reads `ci.yml` as `TestCIJobTimeoutsCoverEveryGoTestDeadline` does. The shared type and the constant are refactors with no failing test.
- **Docs**: `spec/design/system.md` (nightly check, failed-test, the CI wait and its stall window, the credential steps); the requirement lines in `spec/requirements/non-functional.md` that state the stall window and the nightly check; the repository's `CLAUDE.md` where it states the stall window.
- **Order**: after A9 (#878 changes `verify_failed.go`, adds the failed-test lists in `verify_e2e.go`, and edits the credential steps and job timeouts of `ci.yml`). Before A10, which rewrites `nightly.go` and the post-merge run selection in `verify_dod_ci_wait.go`, so that A10 builds on the retrying call and the one parser; before A11, which adds jobs to `ci.yml`. Size M. Design decisions: how the e2e job's timeout fits the PR tier (an expression on `timeout-minutes`, or a separate quick-tier job), without which a PR's stall window stays 190 minutes; whether the drive identifiers stay masked repository variables or become secrets.
- **Decisions (D25 and D26, mine, 2026-11-11)**: D25: one e2e job; its `timeout-minutes` is `${{ github.event_name == 'pull_request' && N || 180 }}` with N worked out from the quick tier's go test deadlines, and the code that reads `ci.yml` understands exactly that shape and fails on any other expression. D26: the drive identifiers and the account allowlist stay repository variables, read as `${{ secrets.NAME || vars.NAME }}` in the first step of each live job, which masks every spelling and exports them. A plain variable still shows once per job in that step's header; only a secret hides it fully. Creating the secrets (Actions and Dependabot stores, four names each) was refused to me by the permission rules (secret-store writes), so it is the owner's step: report it at the end. Dependabot pull requests run the live jobs through OIDC, which is why both stores are needed.
- **Decision (D29, mine, 2026-11-11; third pass)**: the tree names no test account. The two default recipient addresses in `testutil/testenv.go` are deleted, and so is the guessing through `DriveIDForEmail`; the shortcut parent drives come only from `ONEDRIVE_TEST_SHORTCUT_WRITABLE_PARENT_DRIVE` and `ONEDRIVE_TEST_SHORTCUT_READONLY_PARENT_DRIVE`, which the masking script exports in the e2e job by role. Checked by variable name, never by value: `ONEDRIVE_TEST_DRIVE_E2E` (the e2e job's primary drive) holds the read-only shortcut and `ONEDRIVE_TEST_DRIVE_E2E_2` the writable one; `ONEDRIVE_TEST_DRIVE_INTEGRATION` is the same account as `E2E_2`. No vault change is needed (process environment wins over `fixtures.env`). The folder-name defaults stay: they are fixture folder names, not account identifiers. The eight tracked files that hold account words are rewritten with role names and placeholder addresses, and a quick-tier e2e test fails, naming file and line number and never the value, when a tracked file holds a spelling of a configured drive's or allowlist entry's address. Git history is not rewritten: that is the owner's decision (final report). The same pass shares one `.env` reader between testutil and devtool and takes the drive out of test names, artifact file names and e2e messages.
- **Reviewer's step in the gate**: the rollup status of a check queued in its concurrency group is unconfirmed. Push the branch, dispatch the full tier on it first, open the PR once the dispatch run's e2e job is in progress, and read the queued e2e check's status from the PR's `statusCheckRollup`; record it in the spec and narrow the "not started" rule only if the status proves a narrower one safe.

#### W2. Verify profiles stream every step and refuse an empty test tree

- **Source**:
  - A9 report, recommendation 2: the stress profile's x3 step still runs with `CombinedOutput` (`internal/devtool/verify_stress.go:195`), so a hang shows no output until the step ends.
  - A8 report, recommendation 3: `assertTestsMatchedInTree` (`internal/devtool/verify_test_match.go:102-105`) passes any output that lacks the "no tests to run" marker, including a tree whose packages all print "[no test files]".
  - A2 report, recommendation 1: the benchmark builds its subject with plain `os.Environ()` (`internal/devtool/bench.go:621-624`), so with the host's Go 1.27 instead of the toolchain pinned in `go.mod`.
  - A8 report, recommendation 2: the unit tier always runs with `-coverpkg=./...` and `-coverprofile` (`internal/devtool/verify.go:577-599`), also on the macOS leg, where the threshold is -1 and coverage is only reported.
  - A8 report, recommendation 4: nobody measured how long a local `verify default` takes on this two-core host since `-coverpkg=./...` instruments every package in every test binary.
  - C4 report (2026-11-11), "Not fixed" 1: two requirements shared the ID R-2.8.12 in `spec/requirements/sync.md` and the "requirement traceability" check passed. C4 renumbers the one it found; nothing stops the next duplicate.
- **Change**:
  - The requirement traceability check fails when one requirement ID is defined by two requirement lines in `spec/requirements/*.md`, naming the ID and both places. Test first: `TestRequirementTraceability_RefusesAnIDDefinedTwice`.
  - The x3 step streams through the same tee as the other stress steps and keeps its slow-test summary.
  - The tree guard fails when no package ran any test, including a tree with no test files; the fake runners print realistic go test output.
  - The benchmark builds its subject with the pinned toolchain (`withPinnedToolchain`).
  - Read the macOS full leg's time from the next full-tier run: if it is within five minutes of its deadline, a negative threshold skips `-coverprofile` and `-coverpkg`; otherwise record the time in `spec/design/system.md` and change nothing. Measured on run 43103299254 (main at 34fb1a66, 2026-11-11): the macOS `verify-full` job took 19 min 6 s and the Ubuntu one 14 min 56 s, both of a 45-minute job timeout (job times; read the unit step's own time from the job log before deciding).
  - Measure one local `verify default` and record it in the repository's `CLAUDE.md` verification tiers.
  - **Rider (D28, mine, 2026-11-11; W1 report, "Not fixed" 4)**: `verify lint` lints the workflow files, so an invalid `ci.yml` cannot pass every Go test (W1's writer found an invalid one-line `run:` only through a manual YAML parse). Preferred: a pinned `actionlint` run by the lint profile, with its shellcheck integration off (shellcheck is not installed here or assumed in CI). Minimum, if a new tool proves unworkable: a strict YAML parse of every file under `.github/workflows/` with the YAML library `go.mod` already has. The writer checks `go.mod` and chooses within that, and says which in the report. Test first: `TestVerifyLint_RefusesAnInvalidWorkflowFile` (a workflow whose `run:` is a plain scalar containing a colon and a space fails the profile, naming file and line).
  - **Rider (observed 2026-11-11 on #883, mine)**: `spec/design/system.md` says no pull request has shown which status the rollup gives a job waiting on its concurrency group. #883 showed it: with a dispatch run's live jobs in progress on the same branch, the pull request's `integration` and `e2e` checks read `PENDING` with no conclusion, the checks waiting only for a runner (`verify-lint`, the six `verify-unit` shards) read `QUEUED`, and jobs skipped by their `if:` read `COMPLETED`/`SKIPPED`. Replace that sentence with the observation, keep the rule as written (every status but `IN_PROGRESS` and `COMPLETED`, and a check not yet reported, counts as not started: both observed statuses fall under it), and add a rollup fixture with a `PENDING` group-held check to `TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup` if its fixture uses another status.
- **Tests first**: `TestRunVerifyStressStreamsTheRepeatStep` (output reaches stdout before the step returns), `TestAssertTestsMatchedInTree_RejectsATreeWithNoTestFiles`, `TestPrepareBenchSubject_BuildsWithThePinnedToolchain`; only if the measurement calls for it, `TestRunVerifyReportOnlyCoverageWritesNoProfile`.
- **Docs**: `spec/design/system.md` (stress lane, the tree guard, the benchmark, the macOS measurement); the repository's `CLAUDE.md` (the measured local time).
- **Order**: after A9 (#878 changes `verify.go`, `verify_stress.go` and `verify_test_match.go`). A11 (new `race` and `seeds` profiles) and A12 (deleted checks) also edit `verify.go`; land W2 before whichever of them starts later, or rebase it. Size S. No design decision; the coverage-profile change depends on a measurement.

#### W3. e2e tests wait for what they read; no name-only waits

- **Source**:
  - A5 report, recommendation 1: `TestE2E_SyncWatch_RapidChurn` (`e2e/sync_watch_full_test.go:465`) and `TestE2E_SyncWatch_GracefulShutdown` (`:528`) keep name-only waits that the content and size waits beside them now cover.
  - A5 report, recommendation 2: `e2e/sync_full_test.go:593` and `:647` wait on a name for files that were modified, not created; `e2e/cli_commands_e2e_test.go:654` runs a `--force` overwrite, and its callers read the destination without waiting for the new content.
  - A5 report, recommendation 3: on timeout, `pollRemoteEventually` (`e2e/remote_read_helpers_test.go:176-178`) prints `get`'s stdout, which is not the downloaded file, so the last content seen is unknown.
  - A5 report, recommendation 4: no check stops a new name-only wait in a test that starts a daemon; the rule is prose in `spec/design/system.md`.
  - A5 report, recommendation 5: sixteen of the seventeen live tests #867 changed run only in the full tier, and nobody has read their full-tier result.
  - A6 report, recommendation 1: `TestE2E_Sync_NamingCorpusRoundtrip` (`e2e/sync_hardening_full_e2e_test.go:235`) checks content unpolled after a best-effort `--download-only` pass.
  - A6 report, recommendation 2: `findActiveShortcutChild` (`e2e/shared_items_helpers_test.go:541-542`) asserts the record's state inside a poll predicate, so one transient non-active state fails the test.
  - A6 report, recommendation 3: `TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict` (`e2e/sync_full_test.go:819`) and `TestE2E_Sync_DryRunNonDestructive` (`:937`) run their single pass straight after a remote put, before delta shows it.
  - A4 report, recommendation 3: no end-to-end test asserts that `status --perf` shows requests for a syncing mount; `e2e/status_helpers_test.go:74` parses the field and nothing reads it.
- **Change**:
  - Delete the two redundant name waits.
  - Modified files wait for their new content with `waitForRemoteFileContent`; the `--force` callers wait for the overwritten content before reading it.
  - On timeout, the content wait prints the bytes it last downloaded.
  - A source-scan test in `e2e` fails when a test that starts a daemon (`cmd.Start`, `startDaemon`, `startFastDaemon`, `withShortcutWatchDaemon`) calls a name-only wait helper.
  - NamingCorpus's download pass becomes a converge whose predicate is the content it then checks.
  - `findActiveShortcutChild` returns the record, and its callers assert the state after convergence.
  - The two single-pass tests wait until delta shows the remote put before their one pass.
  - A watch test runs `status --perf --json` while the daemon syncs one file and asserts a non-zero request count for that mount.
  - Before starting, read the full-tier e2e results for the sixteen tests in the A6 dispatch run 43092588842 (it contains #867) or the next nightly; fix here any that failed.
- **Tests first**: `TestE2EDaemonTestsUseNoNameOnlyWait` (fails today on the two waits above); `TestE2E_SyncWatch_StatusPerfCountsRequests` (live; it would have failed before #862). The other changes are proven by one live run of each touched test, one at a time.
- **Docs**: `spec/design/system.md` Verification Policy (the rule is now enforced, with the test's name); `spec/reference/live-recurring-families.md:137` (the family's policy lines).
- **Order**: after A6 (#877 rewrites the converge helpers and touches `sync_full_test.go`, `shared_items_helpers_test.go` and `sync_hardening_full_e2e_test.go`) and after B2 (it changes the helpers' exit-code handling and audits every direct `sync` call file by file); before A13, which renames tests across `e2e`. Size M. No design decision.

#### W4. Graph client: retries rebuild bodies, create marks are honest, quirks recorded

- **Source**:
  - H3 report, recommendation 1: `do` (`internal/graph/client_auth.go:18`) takes an `io.Reader`, and the 401 retry rewinds that reader (`rewindBody`, `:313-323`). Since #868 a body that cannot be rewound fails loudly, but the signature still accepts one.
  - H3 report, recommendation 2: `Seek(0, io.SeekStart)` (`internal/graph/client_auth.go:323`) rewinds to the start of the slice, not to where the reader stood when the call began.
  - A14 report, recommendation 3: `createOutcomeUnknown` (`internal/graph/item_create.go:118-122`) marks every error that is not a `*GraphError` as "create outcome unknown", including token-source and request-building failures where nothing was sent; the intent is then kept until it expires.
  - A1 report, recommendation 4: `internal/graph/upload_transfer.go:330` bounds a session cancel with `shutdownTimeout`, the sign-in callback server's constant (`internal/graph/auth.go:87-88`), and `internal/graph/auth_browser.go:105` uses that constant as a header read timeout.
  - A1 report, recommendation 2 and deviation 10: `RemoveEmptyItem` (`internal/graph/item_create.go:246-248`, `:294-297`) makes three immediate attempts. Whether an orphaned upload session keeps moving the empty item's eTag, and whether the canceled request reached the server, was inferred and never probed.
  - A3 report, recommendation 4: whether opening and canceling a session moves an empty item's mtime and hash on OneDrive for Business was never probed; `spec/reference/graph-api-quirks.md:1146` describes Personal only.
  - A7 report, recommendation 2, and the reviewer: no quirk or incident entry records the HTTP 400 that Graph gave a folder create in the 2026-11-11 nightly (`TestE2E_Sync_IdempotentReSync`).
  - B5 report, recommendation 3: `spec/design/graph-client.md:22-24` lists the profile builders without the `notices` parameter each one takes (`internal/graphtransport/profiles.go:47-59`).
- **Change**:
  - `do` and its siblings take the request body as bytes (nil for none), never a reader. Every attempt, the 401 retry included, builds its request from those bytes, so a body that cannot be replayed can no longer be passed; `rewindBody` and its Seek go.
  - A failure before the request leaves (token source, request building) carries a not-sent mark and is never "create outcome unknown".
  - Named constants for the session-cancel bound and for the callback server's header timeout.
  - A live probe on the test account: create an empty item, open and cancel an upload session client side, read the eTag at once and over one minute, try the conditional delete. Record the result in the quirk entry. If the eTag keeps moving, `RemoveEmptyItem` waits briefly between attempts.
  - The same probe on Business only if a Business test drive exists; otherwise the quirk entry says it was not probed and why.
  - A quirk entry and a ledger entry for the folder-create 400: the request, the answer, and why it is classed as transient.
  - The profile list in `graph-client.md` gets the `notices` parameter.
- **Tests first**: `TestDo_401RetrySendsTheBytesOfTheFirstRequest` (a reader advanced before the call), `TestCreateFile_TokenFailureIsNotOutcomeUnknown`, `TestCreateFile_RequestBuildFailureIsNotOutcomeUnknown`; only if the probe shows a moving eTag, `TestRemoveEmptyItem_WaitsBetweenRefusedDeletes`.
- **Docs**: `spec/design/graph-client.md` (profiles, 401 Token Refresh, Transfers); R-6.8.14 in `spec/requirements/non-functional.md`; `spec/reference/graph-api-quirks.md`; `spec/reference/live-incidents.md`.
- **Order**: before B6 (it changes the token source and `graph-client.md`) and before G2 (it changes `readCreatedFileByName` in `item_create.go`). Size M.
- **Decision (mine, 2026-11-12)**: `do` takes bytes (the stronger option: the type rules out a body that cannot be replayed, where a `GetBody`-based retry would still accept any reader and fail at run time). Streamed bodies stay on the pre-authenticated path, whose builder already makes a fresh body per attempt. The backoff in `RemoveEmptyItem` is added only if the probe shows a moving eTag. No Business test drive exists, so the Business quirk entry says it was not probed and why. The probe uses drive endpoints only (on 2026-11-12 Graph was failing `GET /me`).
- **Review decisions (mine, 2026-11-12)**: (a) one bound for cancelling an upload session, 10 s, owned by `internal/graph` (`graph.SessionCancelTimeout`) and used by driveops' session cancel too; driveops' removal of the empty item keeps its own bound (`placeholderRemovalTimeout`); the cancel stays best effort. (b) The create-by-body quirk and policy are named for what they are: `file-create-transient-404`, `QuirkFileCreateTransient404`, `retry.FileCreatePolicy`. (c) The writer's "Not fixed" entries 1 to 9 were fixed in this PR; entry 10 is W23's rider; entry 11 became ledger entry LI-20261112-01. (d) Verb-less `fmt.Errorf` is fixed in `internal/graph` here and in the rest of the tree by W28. (e) The Socket.IO subscription GET sends no Content-Type (it has no body; a live call answered 200 without one). (f) `ResumeUpload` reports an expired session only on a 404 and wraps the answer; the consumer in driveops tests `ErrUploadSessionExpired` before `ErrNotFound`, so the wrapped error keeps its path.

#### W5. Pending-create records end once their file is settled

- **Source**:
  - A3 report, recommendation 1: `SyncStore.ClearPendingCreate` (`internal/sync/store_pending_creates.go:138-141`) has no caller.
  - A3 report, recommendation 2: when Graph's cleanup reports the empty item removed, `removePlaceholder` (`internal/driveops/transfer_session.go:471-483`) only logs it, and the pending record of that item stays until observation sees the deletion.
  - A3 report, recommendation 3, and A14 report, recommendation 2: a row naming an item is dropped only when observation sees the item gain content or disappear (`internal/sync/store_pending_creates.go:26-36`). A zero-byte file never gains content, so its row stays for good, and a later mtime-only remote change is absorbed as the placeholder following its eTag.
  - A14 report, recommendation 1: in `settleUploadedItem` (`internal/driveops/transfer_verify.go:186-198`), a failed lookup after `ErrUploadCompletedWithoutItem` wraps only the lookup error and drops the upload's own. Checked today: no create mark is lost, because a create never returns that error (`internal/graph/item_create.go:127-146`); the drop would hide any future mark.
  - A16 report, "Not fixed" entry 1: `followAdoptedPlaceholderTx` (`internal/sync/store_pending_creates.go`) keeps the record of an adopted placeholder at its old path when the item is renamed online while still empty (`adoptPlaceholderTx` drops it in the same case). A later create at the old path that meets 409 would upload into the item at its new path.
  - A16 report, "Not fixed" entry 2: in watch mode the remote observer can commit a new zero-byte file between OneDrive making it and `clearPendingCreateIntent` in `uploadNewFile` (`internal/sync/executor_transfer.go`); the claim names the item and the clear (`item_id = ''` only) leaves that row for good.
  - A16 review (D30): a watch steady-state replan refreshes local truth only, so there the "file gone" rule for a fresh intent does not run and the intent stays until the next start or the 72 h expiry. In that window an empty file another device makes at the same path would be claimed and removed.
- **Change**:
  - An adopted placeholder that has left its record's path while still empty loses the record (after checking the stale-echo protection is not needed for an upload in flight).
  - A successful zero-byte create clears the row at its path whether it names nothing or the created item, on a context detached from the action's and bounded by `outcomeCommitTimeout`.
  - Watch steady state drops a fresh intent whose file is gone once a remote poll that began after the last create attempt ended has been applied without claiming it. This needs the poll's start time on `remoteObservationBatch` and a signal for a zero-event poll (`remoteObserver.handleZeroEventPoll` emits nothing today); the loop goroutine then runs the prune with "remote observed". Decide in the DoR whether the signal reuses the batch channel.
  - Once a create's outcome is committed to the baseline, the row naming that item is dropped; `ClearPendingCreate` becomes that call, or is deleted if the drop lives inside the commit transaction.
  - A removal the cleanup reports done drops the record of that item.
  - A zero-byte file's row goes once its committed outcome is in the baseline, so a later remote change reads as a change.
  - The lookup failure joins the upload error with the lookup error.
- **Tests first**: `TestUploadNewFile_ZeroByteCreateLeavesNoPendingRow`, `TestRunOnce_ClaimedZeroByteFileThenRemoteMtimeChangeIsARemoteChange`, `TestSessionUpload_RemovedPlaceholderClearsItsRecord`, `TestSettleUploadedItem_LookupFailureKeepsTheUploadError`, `TestCommitObservation_AdoptedPlaceholderRenamedWhileEmptyLosesItsRecord`, `TestUploadNewFile_ZeroByteCreateClaimedByTheObserverLeavesNoPendingRow` (an uploader that commits the claiming observation before it returns), `TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll` (zero-event poll and a poll with other events; kept while the only poll began before the attempt ended).
- **Docs**: the comment block at the top of `internal/sync/store_pending_creates.go`; `spec/design/sync-store.md` (Pending creates); `spec/design/drive-transfers.md` (New files and their placeholder); R-5.6.15 in `spec/requirements/transfers.md`.
- **Order**: after A15 and A16 (A16 rewrites this store file's comment block and adds the expiry prune, and both use `pending_creates_test.go`) and after B8 (it changes `executor_transfer.go`). H5 edits another section of `drive-transfers.md` (its lines 313-323) and does not conflict. If W11 has landed, `intended_at` uses its time helper. Size M. Design decisions: how a done removal reaches sync (a driveops callback beside the create hook, or a typed result sync reads); that dropping the row at outcome commit keeps the kill-mid-create recovery A16 checks.
- **Decision (mine, 2026-11-12, after reading main at 27e76cc3)**. The rule for the whole change: a record ends at the first moment its file can no longer receive this client's upload, and no remote change is ever swallowed, and no item adopted, because a record exists. Corrections to the bullets above come first.
  - Stale premises: `ClearPendingCreate` no longer exists on main; rows are deleted only by observation (resolved; adopt with a moved path), by the intent clear, by the intent prune and by a claim that is refused.
  - (a) Change 4 is narrowed. The stale-echo rule (`placeholderStale`) needs the row after an upload with content has committed, so that row stays until observation sees content or a delete, as today. Inside the `CommitMutation` transaction a committed outcome drops the row at its path and the row naming its item when the committed remote size is zero, and the row naming an item the outcome removes from the baseline. That is changes 2 and 6, A16's "Not fixed" 2 and the empty file `claimLostCreate` finishes. The drop is in the same transaction as the outcome, after the create hook's crash point and before the commit's, so kill-mid-create recovery is untouched; A16's crash-point tests pass unchanged. After a create that succeeded, the intent clear runs on a context detached from the action's and bounded by `OutcomeCommitTimeout`; only an attempt that did not succeed keeps its intent on a cancel.
  - (b) A done removal reaches sync as a typed result. `graph.CreatedItemError` gains the fact "this item was removed": `removeCreatedPlaceholder` returns what `RemoveEmptyItem` reported, driveops' `removePlaceholder` returns it too, and its two callers (the failed create hook in `openSessionForUpload`, which returns a plain error today, and `settleUncertainFinalFragment`) return the same typed error without wrapping one inside another. `recordItemOfFailedSession` does not record an item the error says is removed (today it records a row naming an item graph may just have deleted). `uploadNewFile` reads the fact with `errors.As` and deletes the row naming that item, detached and bounded as above. `remoteCreateParentGone` keeps its meaning. A second hook was rejected: graph's session create has no hook to carry it, and the fact belongs to the failure that already names the item.
  - (c) An adopted placeholder whose local file is empty too plans nothing and commits nothing, so its row stays for good and `followAdoptedPlaceholderTx` absorbs every later mtime-only change. The prune that follows planning also drops a row naming an item when that item's baseline row and the local state at its path hold the same empty content: nothing is left to upload, and an echo of an empty file is no change.
  - (d) Unknown size. `normalizeRemoteFacts` hands an item of unknown size on as size 0. `classifyObservedPlaceholderTx` then reads it as still empty: with content in the baseline it is dropped as a stale echo, and with no baseline row `adoptPlaceholderTx` adopts it as known empty on both sides. Stale and adopt need a known size of zero; an observation of unknown size is processed like any item and the record stays, as `followAdoptedPlaceholderTx` already does.
  - (e) Change 1: the writer first shows whether an upload by ID in flight into the renamed item still needs the row (the echo is matched by item ID, the row's path is the old one). If it does not, `followAdoptedPlaceholderTx` drops the record on a moved path as `adoptPlaceholderTx` does; if it does, the row follows the item to its new path. Unknown size keeps the row either way.
  - (f) Change 7 is restated. Joining the upload's error is wrong: the only upload error on that path is `ErrUploadCompletedWithoutItem`, which nothing outside driveops reads, and #881's rule is that a failed lookup is classified by its own answer. The defect is the lost create mark, and "Checked today: no create mark is lost" no longer holds: both `UploadCreating` and `persistedSessionUpload` wrap a failure after their create in `CreatedItemError`, a final fragment answered 200 without a body is `ErrUploadCompletedWithoutItem` inside it, `settleUploadedItem` looks the item up by ID, and a failed lookup returns a plain error. A 404 there reaches `remoteCreateParentGone` unmarked, reads as "the parent is gone", and `forgetRemoteItemProvenGone` deletes the parent folder and its subtree from remote truth, so the next plan reads the folder as deleted online. Fix: with a create mark the lookup failure returns `CreatedItemError` around `outcomeUnknownAfter` (the answer as text, the lookup's error in the chain), as `settleUncertainFinalFragment` does; without one it returns the unknown-outcome error. The test is `TestSettleUploadedItem_LookupFailureKeepsTheCreateMark`, with a sync test that such a 404 neither fails as a gone parent nor forgets it.
  - (g) Change 3. A zero-event poll sends an empty batch that carries only the poll's start time on the batch channel: no items and no token, so nothing is committed and the token rule of `handleZeroEventPoll` holds; it costs no replan and no store write unless an intent is dropped. "The last create attempt at that path ended before this poll began" is held in memory by the loop goroutine (no create at that path is in flight, and the last one this process ran had completed before the poll began); the row gets no new column, because only C3 bumps the store generation. After a restart the start pass covers older intents. Accepted: a create whose answer was lost and which the change log shows only after that poll comes back as an empty remote file where the user deleted the local one, the same exposure the start rule of #886 has; a wait of a fixed length was rejected because nothing measured grounds its length.
  - (h) Smell in the same file: `finishIntoPlaceholder` tests `item.Size == 0` where `isEmptyLiveFile` is the rule (a folder or a deleted item of size 0 passes today).
  - Tests first, added to the list above: `TestCommitObservation_UnknownSizeOfAPendingItemIsNotAStaleEcho`, `TestCommitObservation_UnknownSizeOfAPendingItemIsNotAdopted`, `TestRunOnce_AdoptedPlaceholderWithAnEmptyLocalFileLosesItsRecord`, `TestUploadNewFile_RemovedPlaceholderLeavesNoRecord` (graph's path and the session path), `TestRecordItemOfFailedSession_SkipsARemovedItem`, `TestCommitMutation_DeleteOutcomeDropsTheRecordOfItsItem`, `TestCommitMutation_UploadWithContentKeepsTheRecordForItsEcho` (a pin), `TestExecuteUpload_LookupFailureAfterACreateIsNotAGoneParent`, `TestFinishIntoPlaceholder_RefusesAnItemThatIsNotAnEmptyLiveFile`. `TestSettleUploadedItem_LookupFailureKeepsTheUploadError` is replaced by the test named in (f), and `TestUploadNewFile_ZeroByteCreateLeavesNoPendingRow` holds through the outcome commit.

#### W6. Control client: one probe, honest errors, one runtime per sync

- **Source**:
  - B3 report, recommendation 1, and the reviewer: the one-shot `sync` refuses early only for a watch or one-shot owner (`internal/cli/sync_flow.go:96-99`); an unresponsive owner gets the "Checking ..." banner and then fails on the lock.
  - B3 report, recommendation 2, and the reviewer: `status --perf` probes the control socket twice (`internal/cli/status_runtime.go:27` and `internal/cli/status_perf.go:34`), so a busy watch costs two 2-second probe timeouts.
  - B3 report, recommendation 3, and the reviewer: `decodeControlJSONResponse` (`internal/cli/control_client.go:316-326`) turns every decode failure into `internal_error` and flattens its cause to text, so a body-read timeout on a 2xx answer never reads as "did not answer".
  - B5 report, recommendation 1 and its deviation: `sync` builds a second `SessionRuntime` for its engine (`internal/cli/sync_flow.go:186`, `internal/cli/sync_runtime.go:187`), so one process keeps two token-source caches; `spec/design/sync-control-plane.md` records the unification as planned.
  - B5 report, recommendation 2: `sharedBootstrapMetaClient` and `sharedTargetClients` (`internal/cli/shared_target.go:146`, `:175`) do not flush the token cache after a probe renames the account, unlike `cc.Session` (`internal/cli/root.go:366-371`).
  - A4 report, recommendation 4: `status --perf` reads only the per-mount snapshots (`internal/cli/status_perf.go:136-160`); the owner aggregate that `GET /v1/perf` returns is never shown.
- **Change**:
  - The early refusal also refuses for an unresponsive owner, with the existing "already running" error.
  - `status` probes once and gives the result to both overlays.
  - A decode failure caused by a timeout or deadline is classified as "did not answer"; every decode error keeps its cause with `%w`.
  - `sync` uses one session runtime for its probe and its engine; the daemon receives the command's runtime with the timestamped logger.
  - The shared-target clients flush the token cache when the probe renames the account (or `probeAccountIdentity` flushes on any rename).
  - Decide whether `status --perf` shows the owner aggregate; implement it, or write the decision into `cli.md`.
- **Tests first**: `TestRunSyncCommand_UnresponsiveOwnerRefusesBeforeTheBanner`, `TestRunStatusCommand_PerfProbesTheSocketOnce`, `TestDecodeControlJSONResponse_BodyTimeoutReadsAsDidNotAnswer`, `TestRunSync_ProbeAndEngineShareOneSessionRuntime`, `TestSharedTargetClients_FlushTheTokenCacheAfterARename`; a status test for the aggregate if it is shown.
- **Docs**: `spec/design/cli.md` (Control Socket client, status); `spec/design/sync-control-plane.md` (the planned one-runtime row); R-6.8.27 in `spec/requirements/non-functional.md` can then say "within a process".
- **Order**: after B2 (it changes the summary and exit status in `sync_flow.go`); before B6, which carries token-save errors out of `SessionRuntime` and should find one runtime per process. Size M. Design decisions: whether `status --perf` shows the aggregate (a product decision); merging the runtimes changes the daemon's runtime logger and eight `runSyncDaemonWithFactory` test calls.

#### W7. The control server's closing goroutine gets an owner

- **Source**: B3 report, recommendation 4: `startControlSocketServer` (`internal/multisync/control_socket.go:178-190`) starts a goroutine that waits for the context to end and then closes the server; nothing waits for that goroutine.
- **Change**: remove the goroutine if every owner already closes the server after the drain (B8's readiness notes assume they do), or make `Close` wait for it.
- **Tests first**: `TestStartControlSocketServer_CancelLeavesNoRunningGoroutine`.
- **Docs**: `spec/design/sync-control-plane.md` (Control Socket).
- **Order**: after B8 and B9, which both change `control_socket.go`. If B8 removes the goroutine, record this item as done by B8 and drop W7. Size S. No design decision.

#### W8. Engine entry points carry their collector and wrap load errors once

- **Source**:
  - A4 report, recommendation 1: `RunOnce` (`internal/sync/engine_run_once.go:31`) and `RunWatch` (`internal/sync/engine_watch.go:58`) do not put the engine's own perf collector on their context; only the multisync owners do (`internal/multisync/watch_runner.go:88`, `internal/multisync/orchestrator_run_once.go:249`), so an owner that forgets loses every count.
  - A4 report, recommendation 2: `Engine.Close` (`internal/sync/engine.go:429`) runs under the orchestrator's context, so anything it records counts on the command collector only; it records nothing today.
  - B4 report, recommendation 2: `normalizePersistedAccountAuthRequirement` (`internal/sync/engine.go:530-532`) fails the mount's start-up when another process holds the catalog lock (`config.ErrConfigLocked`), where the fatal-auth path only warns.
  - C1 report, recommendation 3: planning-state load errors are wrapped three ways by entry point (bare in one-shot, "reading sync state:" at `internal/sync/engine_current_plan.go:76`, "sync: reading sync state:" at `:95`), and a comparison query error is wrapped twice (`internal/sync/engine_current_plan.go:589` around `internal/sync/sqlite_compare.go:798`).
  - A3 report, recommendation 5: `internal/sync/doc.go:24` says the watch loop owns "graceful drain", the wording #864 retired elsewhere.
- **Change**:
  - `RunOnce`, `RunWatch` and `Close` wrap their context with the engine's collector when one is set; a repeated wrap with the same collector changes nothing.
  - A locked catalog at start-up logs one Warn, keeps the requirement and lets the engine start; the next start clears it.
  - One wrap for planning-state load errors in every entry point; the outer comparison wrap goes (the change stays in `engine_current_plan.go`).
  - `doc.go` says the loop owns shutdown: cancel in-flight actions, then drain their completions.
- **Tests first**: `TestRunOnce_CountsOnTheEngineCollectorWithoutAnOwnerWrap`, `TestRunWatch_CountsOnTheEngineCollectorWithoutAnOwnerWrap`, `TestEngineStartup_LockedCatalogWarnsAndStarts`, `TestPlanningLoadError_SameTextInOneShotAndWatch`.
- **Docs**: `spec/design/sync-engine.md` (start-up, the perf context); `spec/design/sync-control-plane.md` (the list #862 wrote of work outside the mount collector).
- **Order**: before E1 (it edits `engine.go` and `engine_watch.go`), E2 (`RunWatch`) and E7 (`engine_current_plan.go`); before W13, which changes the signature of `config.ClearAccountAuthRequirement` that this start-up path calls. B8 does not touch these files. Size S. No design decision.

#### W9. Planner: unreachable branches refuse, equal-content edit decided, dead code removed

- **Source**:
  - C1 report, recommendation 4: `writesLocally` (`internal/sync/planner_actions.go:215`), two switches in `internal/sync/planner_dependencies.go` (`:287`, `:519`) and `createSide` (`internal/sync/planner_move_composition.go:1492`) return a quiet value for an action type no case lists.
  - C1 report, recommendation 1: the reconciliation cell for a diverged row whose local side changed, remote side did not, and whose content is equal is `upload` (the CASE at `internal/sync/sqlite_compare.go:606`, pinned by `TestQueryReconciliationState_LocalEditEqualToUnchangedRemoteIsAnUpload`, `internal/sync/sqlite_compare_test.go:692`), so sync uploads bytes OneDrive already holds.
  - C1 report, recommendation 2: `actionType.Direction()` and the `direction` type (`internal/sync/enums.go:37`, `:221`) have no production caller; only `internal/sync/core_types_test.go:121` uses them.
- **Change**:
  - An unlisted action type is refused: the plan's validation rejects any action type outside the enum before these functions run, and each default branch says it is unreachable instead of returning a value that looks like a decision.
  - The equal-content cell becomes `baseline_update` (decision below): change the comparison, flip the pinning test and rename it for what it now proves.
  - Delete `Direction()`, the `direction` type and their test row.
- **Decision (mine, 2026-11-12)**: the cell becomes `baseline_update`. A row whose three sides are present and whose content is proven equal (`current_equal = 1`) never transfers: whichever side changed, the baseline learns both sides' current facts.
  - Why: it is what the sibling cells already do (`equal_again` when the remote changed, `create_equal`, `unchanged` with metadata drift). The local file holds exactly the bytes the baseline's remote side recorded, so a later remote change is a plain fast-forward, not a loss. The upload makes a new version on OneDrive for no new bytes, and on a library that transforms uploads it can make the two sides differ again.
  - Shape: the comparison kind decides it. A three-present row with equal content and a change on either side is `equal_again`, so `diverged` always means the content differs. The writer may choose another shape if a consumer of the kind needs the old meaning, and says why.
  - Bound: the golden matrix may change only in cells with all three sides present, `current_equal = 1`, `local_changed = 1` and `remote_changed = 0`; the report lists each changed cell. A local mtime newer than OneDrive's is not sent on its own, as with metadata drift today.
- **Tests first**: `TestPlanCurrentState_UnlistedActionTypeFailsThePlan`; the flipped pinning test; an updated `TestReconcileMatrix_Golden`; a test that the baseline update for this cell records the new local hash, size, mtime and identity beside the unchanged remote facts, and that the next plan of the same state is a no-op.
- **Live proof**: none. The cell needs a baseline whose two sides recorded different bytes, which the test account's server does not produce.
- **Docs**: `spec/design/sync-planning.md` (Decision Table Kinds); `spec/design/sync-domain.md` (the Direction row).
- **Order**: before G1 (it retypes planner helpers), before the S wave (it moves these files) and before F3 (its Go oracle must mirror the decided cell). Size S.
- **Review (mine, 2026-11-12)**: read the report, the whole product diff, the test diff and the spec diff; no defect. The wider `equal_again` rule is right: three sides present with equal current content and a change on one side can only be a file of one type on all three sides whose content now matches the other side; the rename-and-recreate and type-change rules sit before it or are untouched, and no Go code reads the two kinds. Accepted: (a) the plan test calls `planBuiltActions` directly, because the planner builds actions only from constants; (b) the folder create side is held to its enum too; (c) tagless switches and `==` predicates became exhaustive switches; (d) store and dispatch defaults return an error where the planner's panic; (e) `underMoveSource` answers false for a move that is neither kind; (f) R-2.2.3 and R-6.2.16 are extended. **Second pass (writer)**: one exhaustive switch for the delete-safety side that the planner and the delete ledger share; the change event's source field goes with its type, once the writer has shown that only remote events reach the one guard that reads it; a source-reading test that every switch on the action type or the folder create side has a default that refuses, so the 26 hand-listed decisions become a held property. **Second pass reviewed (eb442d96)**: all three done, no defect; the source-reading test walks function declarations only (accepted; no switch lives elsewhere). Mine, amended in as d4865523: the shared switch's parameter no longer shadows its type, two comments no longer tell history, `deleteSafetyMinFiles` is `deleteSafetyMinItems`. **Planned elsewhere**: the functions only tests call are a rider on W27.

#### W10. Issue types: exhaustive verdicts and keys, names that say what they hold

- **Source**:
  - C2 report, recommendation 1, and the reviewer: `observationIssueBlocksRemoteTruth` (`internal/sync/truth_status.go:172-174`) is two equality tests, so an unknown issue type does not block remote truth; the local verdict has failed closed since #875.
  - C2 report, recommendation 3, and the reviewer: `conditionKeyForIssueType` (`internal/sync/condition_keys.go:153-178`) is a lookup table, so a new issue type silently maps to "unexpected condition".
  - C2 report, recommendation 4, and the reviewer: `hash_panic` (`internal/sync/issue_types.go:30`) is recorded for ordinary read errors too (`internal/sync/scanner.go:289`, `:324`), and its condition key is `ConditionHashError` (`internal/sync/condition_keys.go:175`). The value is stored in `observation_issues`, and since #875 an unknown stored value fails the read, so a rename needs a new store generation.
  - C2 report, recommendation 5, and the reviewer: `fallbackFailureSummaryIssueType` (`internal/sync/engine_results.go:15`, used at `:101` and `:135`) is a summary and log key, not an issue type.
- **Change**:
  - Both functions become exhaustive switches over `IssueType`; a non-member blocks remote truth and maps to a condition chosen on purpose.
  - Rename `hash_panic` to a name for "the file could not be read or hashed" (for example `hash_error`, matching its condition key) in code, store and docs.
  - Rename the summary key so its name says what it is.
- **Tests first**: `TestObservationIssueBlocksRemoteTruth_ValueOutsideTheEnumIsUnavailable`; `TestConditionKeyForIssueType_EveryMemberIsDecided`, which reads the members from source as #875's verdict test does; a store test that a store of the previous generation is refused after the rename.
- **Docs**: `spec/design/sync-domain.md` (issue types); `spec/design/sync-observation.md` (it already calls it a hash error); `spec/design/sync-store.md` (the generation).
- **Order**: after C4, which edits the managed issue set in `internal/sync/observation_findings.go`, a file the rename also touches. B2 adds a table that classifies every issue type for the one-shot exit status; whichever lands second carries the renamed constant. Size M.
- **Decision (D27, mine, 2026-11-11)**: the `hash_panic` rename is not done here. A stored value's rename needs a new store generation, and one bump with one reset is better than two; it moves into C3 (generation 29) as a rider. W10 keeps the two exhaustive switches and the summary-key rename, does not touch the generation, and drops the "previous generation is refused" test. Until C3, W10 adds a comment at the constant saying the name is wider than its use and where the condition key comes from; it does not rename the Go constant alone (a Go name that disagrees with the stored value is worse than either).

#### W11. Stored retry and trial times saturate instead of wrapping past 2262

- **Source**:
  - H2 report, recommendation 1: `internal/sync/store_write_block_scopes.go:56` stores `NextTrialAt.UnixNano()`. For a time past 2262, which a saturated `Retry-After` produces since #866, that value is undefined, so after a restart the trial is due at once.
  - H2 report, recommendation 2: the same conversion writes other stored times (`internal/sync/store_retry_work.go:202`, `internal/sync/store_scope_admin.go:24`). Checked today: only the block-scope trial takes a server-given wait (`internal/sync/engine_runtime_lifecycle.go:279` through `computeTrialInterval`, `internal/sync/scope_lifecycle_policy.go:88-93`).
- **Change**: one helper converts a time to stored nanoseconds and saturates at `math.MaxInt64`; the block-scope, retry-work and scope-admin writes use it, and the read side maps the saturated value back to "not before the far future".
- **Tests first**: `TestUpsertBlockScope_SaturatedTrialSurvivesAReload` (write, reopen the store, the trial is not due).
- **Docs**: `spec/design/sync-store.md` (time columns); `spec/design/retry.md`, section "Reading `Retry-After`" (saturation survives a restart).
- **Order**: before E4 (one write path for retry work in `store_retry_work.go`) and before C3 (it rewrites the block-scope write). W5 can adopt the helper for `pending_creates.intended_at`. Size S. Design decision: saturate, which keeps decision D3's "no cap", or refuse such a time at the store boundary.
- **Decision (mine, 2026-11-12)**: saturate. One helper converts a time to stored nanoseconds: a time past the end of the int64 nanosecond range stores `math.MaxInt64`, a time before its start stores `math.MinInt64`, and the zero time keeps what each site stores for it today. It is used by every write of a stored time that is compared by order (a deadline or a not-before time: trial, retry, refresh, expiry, ledger window), not only the three named sites; the report lists each such column with where its time comes from and whether it is bounded. `time.Unix(0, math.MaxInt64)` already reads back as the far future, so the read side changes only where a reader does arithmetic that could overflow. File and item modification times are compared for equality and keep `UnixNano()`: saturating them would make two different far-future mtimes equal. The writer traces what a file mtime outside 1678 to 2262 does on the way to OneDrive and reports it; a wrong time sent to the server is fixed here when it is in a touched file and planned otherwise. Refusing at the store boundary was rejected: it would turn a server's long `Retry-After` into a failed write of the block that honours it.
- **Rider from the W14 report (2026-11-11)**: `internal/sync/store.go:110-115` builds the SQLite `file:` URI by interpolating the database path unescaped, so a `?` or `#` in the data path changes the URI, and `busy_timeout 5000` and `journal_size_limit 67108864` are unnamed. Build the URI with `net/url`, name the two constants. Test first: a store opens, writes and reopens in a temp folder whose name holds `?` and `#`.
- **Review decisions (mine, 2026-11-12)**: the first pass (`af77ed72`) is accepted as it stands. Its report lists ten things it saw and did not fix; nine are fixed in this change and one needs nothing.
  - (a) **Remote modification times saturate too; local ones do not.** This amends the decision above for the remote side. A remote time outside 1678 to 2262 is stored wrapped (`toUnixNano`, used by the item converter) and is then turned back into a file time at two places: the file download (`driveops` `publishPartial`) and the local folder create (`executor.go`, `Chtimes`). An item dated 2300 gets a local time in 1716, and one dated 1601, the Windows file-time epoch that real items carry, gets one in 2185. Rule: every write of a remote modification time goes through the saturating encoder (the item converter and the upload outcome must agree, because the two are compared with each other); local modification, change and birth times keep `UnixNano()`, because they are compared only with themselves and are never turned back into a time (every comparison of a modification time in sync product code is same-side: checked, sixteen sites). sync decodes a stored remote modification time in one function, used by the download and the folder create; `DownloadOpts.RemoteMtime` becomes a `time.Time`, so driveops decodes no stored integer. Two source-reading tests hold it: every `time.Unix(0, …)` in the product files of `internal/sync` and `internal/driveops` is declared with what it decodes, and every plain `UnixNano()` conversion in `internal/sync` product files is declared with its kind, so a new stored time has to choose its encoder. Cost, accepted and written in the spec: two remote times past the same end of the range are equal as stored, so a change of only the time between them is not seen (a content change still is). Rejected: saturating local times (a same-size change between two far-future times could hide); skipping `Chtimes` at a bound (the file would carry the download time); carrying the true time in the store (a column change is a generation bump, which only C3 makes).
  - (b) **The racily-clean window is computed on times.** `canReuseBaselineHash` and `cachedHashForNewFile` subtract stored integers, so a file dated 1601 wraps to 2185 and is re-hashed on every scan. One helper answers "was this file modified within the window before the scan began" from `info.ModTime()` (`Sub` saturates), used by both.
  - (c) **Only zero is "no time".** One named predicate says a stored ordered time is set; every Go reader uses it (the two fixed in the first pass, `intentIsFresh`, `applyPersistedRetryAdmission`) and the SQL says `<> 0` (the `RecordPendingCreate` CASE).
  - (d) **The zero sentinel for modification times is traced.** A file dated exactly 1970-01-01T00:00:00Z stores 0, which every reader takes for "unknown". The writer traces what that skips for a local file and a remote item and fixes what is wrong, test first; if the fix needs a schema change it stops and says what it needs.
  - (e) Fixed in this change: the `readObservationStateTx` pass-through is removed; test DSNs go through `sqliteFileURI` in `internal/sync` and through one builder in `internal/synctest` elsewhere, held equal to the product builder by a test and by a source-reading test that no other Go file builds a SQLite DSN by concatenation; `scopeHasBlockedRetryWork` asks the store's count; `PruneRetryWorkToCurrentActions` runs in one transaction. No action: the literal 3 in `CountRetryingWork` (B2 deletes that query).
  - Tests added to the list: a remote file dated 2300 downloads with the last storable time and the next scan and observation plan nothing; the same for 1601 (the first storable time, or what the filesystem clamps it to); a folder created for a remote folder dated 2300 gets the same time; a local file dated 2300 uploads and the outcome's remote time equals what the next observation stores; the converter and the upload outcome agree at both ends; a file dated 1601 is not racy and its hash is reused; a pending create recorded with a clock before 1970 keeps its first `intended_at` and is fresh; a retry row with a negative time is delayed, not blocked; a failed delete in the middle of the prune removes no row.

#### W12. Sync test harness: load-proof waits, chaos summaries, session route covered

- **Source**:
  - Reviewer: watch tests in `internal/sync` wait ten seconds for an event and fail under host load alone: `debugEventTimeout` (`internal/sync/engine_test_helpers_test.go:586`) and literal `time.After(10 * time.Second)` waits in `internal/sync/engine_watch_test.go:83`, `:92`, `:303`, `:355`, `:530`, `:583`, `:593`, `internal/sync/engine_watch_panic_recovery_test.go:145` and `internal/sync/observer_local_watch_health_test.go:139`, `:572`.
  - A7-seeds and the reviewer: `TestModel_ChaosGraphConverges` logs the fault-kind summary only after the run returns (`internal/sync/chaos_remote_test.go:535-537`), so a failing seed never prints it.
  - A7 report, recommendation 3, and the reviewer: A7's fake upload-session client `scenarioSessionClient` (`internal/sync/scenario_remote_session_test.go`, on A7's branch, not yet on main) does not go through the chaos layer, so the storm never covers the large-file route.
  - A7 report, recommendation 4, and the reviewer: under `-race` the chaos model runs seed 1 only (`internal/sync/chaos_remote_test.go:551-553`), and that seed draws no create or session fault.
- **Change**:
  - One named wait for event-driven watch tests, long enough for a loaded two-core host (for example 60 seconds; a wait returns as soon as its event arrives), replaces the ten-second literals.
  - The fault summary is logged through `t.Cleanup`, so it prints for a failing seed too.
  - The session client's requests pass through the chaos layer.
  - The race run uses a seed that draws a create fault and a session fault, and asserts that it drew them.
- **Tests first**: the race run's new assertion (it fails today with seed 1); `TestChaosGraph_SessionRouteDrawsFaults`. The wait change has no failing test; it is proven by the watch tests passing under `-race -cpu 1` beside a parallel load.
- **Docs**: `spec/design/system.md` Verification Policy (the wait rule for event-driven tests).
- **Order**: after A7 (it adds the session client and edits `chaos_remote_test.go`); before A11, whose seed helper replaces `chaosSeeds` and should keep the summary and the race-seed rule. Size M. No design decision.

#### W13. Config writers: one write per expired pause, a context, portable locks

- **Source**:
  - B4 report, recommendation 1: `ClearExpiredPauses` (`internal/config/resolver.go:178-179`) deletes `paused` and `paused_until` in two separately locked writes, so a stuck lock holder stalls a reload up to 20 seconds per expired drive.
  - B4 report, recommendation 3: config and catalog writers take no context (`internal/config/mutation_lock.go:61-71`), so Ctrl-C cannot end the ten-second lock wait.
  - B4 report, recommendation 4: the lock is a flock on the directory's descriptor (`internal/config/mutation_lock.go:82-100`); NFS (EBADF) and FUSE (ENOSYS) may refuse it, and then every config change fails. Unconfirmed.
- **Change**:
  - Both pause keys go in one locked write.
  - Every config and catalog writer takes a context, and the lock wait ends on `ctx.Done()`.
  - Probe a directory flock on a FUSE mount on this host (for example sshfs). If it is refused, fall back to a lock file beside the config, with an unsupported-errno rule like the sync root lock's (`isUnsupportedLockErrno`, `internal/sync/engine_root_lock.go:77`, which `config` cannot import); if it is not, record the result in `spec/design/config.md` and change nothing.
- **Rider (2026-11-12, from my review of H4)**: `driveid.NewCanonicalID` (`internal/driveid/canonical.go`) accepts SharePoint spellings that `CanonicalID.String` does not render: an empty site before a library (`sharepoint:a@example.com::Docs` parses with site "" and library `Docs` and renders as `sharepoint:a@example.com:Docs`, a different ID whose site is `Docs`), an empty library (`sharepoint:a@example.com:site:`) and a trailing colon (`sharepoint:a@example.com:`). Refuse them at parse, so parse then render is the identity for every accepted string; check `ConstructSharePoint` for the same (a library without a site) and the unreachable `default` arm of `String`. Then `checkRenderedCanonicalID` in `internal/config/catalog.go` reduces to the parse (its "as onedrive-go writes it" arm can no longer fire: delete it and its test rows move to the driveid tests), and a `config.toml` section named with such a spelling is refused at load with the parse error. Tests first: `TestNewCanonicalID_RendersEveryAcceptedStringUnchanged` (table plus a fuzz or quick check over colon placements), `TestNewCanonicalID_RefusesAnEmptySharePointPart`. Docs: the canonical ID grammar wherever `spec/design` states it. If H4 has not merged when W13 starts, the driveid part still goes in and the catalog part moves to the next change that touches `catalog.go`.
- **Tests first**: `TestClearExpiredPauses_OneLockedWritePerDrive`, `TestWithConfigMutationLock_CanceledContextEndsTheWait`; only if the probe shows the failure, `TestWithConfigMutationLock_UnsupportedDirectoryLockUsesALockFile`.
- **Docs**: `spec/design/config.md` (the lock); R-4.4.4 in `spec/requirements/configuration.md`; `docs/troubleshooting.md` (the lock section).
- **Order**: after H4 (`config.AddDrive` takes both locks through `mutation_lock.go`) and after W8 (it changes the start-up path that calls `config.ClearAccountAuthRequirement`, whose signature W13 changes). The context reaches callers in `internal/cli`, `internal/multisync` and `internal/sync`; if one of them is in W6's files, land W6 first. Size M. Design decision: how far the context reaches in one PR.

#### W14. fsroot: precise absence errors and directory sync on FUSE and SMB

- **Source**:
  - H1 report, recommendation 1: `normalizeNotExist` (`internal/fsroot/fsroot.go:598-604`) answers `os.ErrNotExist` for any failure on an absent name, so a permission or disk-full error on a create reads as "missing".
  - H1 report, recommendation 2 and its deviation: `syncDirectory` (`internal/fsroot/fsroot.go:544-549`) reports every directory-sync error, so a filesystem that refuses it (EINVAL or ENOTSUP on FUSE or SMB home folders) fails every config and token write.
  - H1 report, recommendation 3: `Root.CreateTemp` (`internal/fsroot/fsroot.go:391`) is exported, but nothing outside the package calls it.
  - H1 report, recommendation 4: the rooted-boundary evidence row in `spec/design/threat-model.md:81` cites none of the containment tests #874 added.
  - H1 report, recommendation 5: R-6.4.10 (`spec/requirements/non-functional.md:58`) leaves the state database out, because SQLite opens it by path, without saying so.
- **Change**:
  - `normalizeNotExist` maps only the errors that mean absence (not-exist and not-a-directory, as `synctree` does) and returns every other error as it is.
  - The directory sync tolerates EINVAL and ENOTSUP, using the same test as `localpath.IsDirSyncUnsupported` (`internal/localpath/localpath.go:377`).
  - `CreateTemp` becomes unexported.
  - The threat-model row cites the `TestRoot_*_RefusesSymlinkedParent` tests.
  - R-6.4.10 and the managed-state bullet in `spec/design/system.md` say that the state database is opened by path and why.
- **Tests first**: `TestRoot_AtomicWrite_PermissionDeniedIsNotNotExist`, `TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync` (a fake handle whose directory `Sync` returns EINVAL).
- **Docs**: `spec/design/threat-model.md:81`; `spec/design/system.md` (the managed-state bullet at `:450` and after); R-6.4.10.
- **Order**: any time; no planned PR changes `internal/fsroot` (H9 changes `synctree`). Size S. Design decision: `fsroot` imports no internal package today, so either it may import `localpath` for the errno test or that test moves to a leaf package.
- **First pass (bca5be85, 2026-11-11 20:25) reviewed and accepted**: `fsroot` imports `localpath`; `CreateTemp` is deleted, not unexported (no production caller); `Remove` normalizes absence too; ENOTDIR on a parent reads as absence; the directory sync has its own `syncDir` op.
- **Second pass, same commit (the writer's own "Not fixed" entries in the callers of fsroot)**: `localpath.ReadFile` names the file on a read or close failure, and its dead `clean == ""` check goes; fsroot classifies a root that is a file and `internal/cli/cleanup.go` drops `managedParentIsNotADirectory`; `logfile.cleanOwnOldLogs` reports its listing, info and removal errors once and names the day; `driveops` session store: `Load` reuses the open root, `CleanStale` collects an entry whose info fails into its returned error; `config/write.go:770` and `tokenfile.go:87` drop the `MkdirAll` that `AtomicWrite` already does; the leaf-internal depguard rule in `.golangci.yml:128-136` is made to bite (its glob does not match today) and its list made true (`tokenfile` imports `fsroot`, `fsroot` imports `localpath`).
- **Second pass (9f7dcf24, 21:05) reviewed and accepted in outline**: a root that is a file reads as absent beneath it and as not-a-directory for a listing of it; `logfile.RemoveExpired` is separate from `Open` and the CLI logs its failure once; `CleanStale` returns its failures; the extra `MkdirAll` calls are gone. Its finding: **every path-scoped depguard rule was dead**, because depguard matches file patterns against absolute paths; the patterns now start with `**/` and a contract test holds that.
- **Third pass ordered 21:15, same commit (the second pass's own "seen and not fixed" entries plus one review finding)**: `copyExclusive` (`internal/localpath/rename_noreplace.go:106-111`) joins a failed removal of the partial copy into its error; fsroot stats the root before `os.OpenRoot`, so a root that is a FIFO cannot block; a `CleanStale` failure logs at Warn with the count in `internal/sync/engine.go:434-442` and `internal/driveops/cleanup.go:44` (the second pass had lowered it to Debug on engine close); **new managed-state folders are created 0o700** (`internal/config/write.go:22` was 0o755; my decision, the XDG base-directory rule; existing folders are not changed); depguard: the leaf list holds every true leaf, `tokenfile`, `retry` and `fsroot` each get a lax rule that denies `internal/` and allows their one dependency (the second pass had dropped the first two from every rule), and the contract test checks that each pattern names a folder with Go files.
- **Riders elsewhere**: the three `synctree` entries ride on H9; the SQLite URI rides on W11; `renameManagedPathIfPresent` (`internal/config/email_reconcile.go:611`) renaming to the target's base name inside the source's folder goes to W17's writer, who is in that file.

#### W15. A live test loses a create's answer on the wire

- **Source**: A14 report, recommendation 4: no live test loses a create's answer on the wire. The rule that a retry finishes into the file its lost create made (R-5.6.15) is proven only by fakes: the chaos fault `create:response-lost` in `internal/sync/chaos_remote_test.go` and the tests in `internal/sync/create_answer_lost_test.go`.
- **Change**: a test-only way to drop the answer to the first file create for one name in a live run (a fault-injecting HTTPS forward proxy started by the e2e harness, or a transport hook compiled only under the e2e build tag); then a one-shot sync and a second pass, asserting one remote file with the user's content and no conflict copy.
- **Tests first**: `TestE2E_Sync_CreateWhoseAnswerWasLostFinishesIntoItsOwnFile` (live; fails if the claim rule regresses).
- **Docs**: `spec/design/system.md` Verification Policy (the fault injection); the e2e family table if A13 has landed.
- **Order**: after W3 and A13 (the new test needs a family name). Size L. Design decision: the injection point, and whether a test-only hook in product code is acceptable. Doubt: the harness may cost more than the fakes already prove; the owner's rule keeps it planned.

#### W16. A state store has its own lock: engines, reset, rebind and purge take it

- **Source**: B8 report (second pass, 2026-11-11), "Not fixed", three entries that are one defect: what excludes two writers of one state store is a lock on the sync folder, not on the store.
  1. `internal/cli/drive_remove.go:48`, `drive_cleanup.go:75` (`drive remove --purge`) and `auth_logout.go:172-175` (`logout --purge`) delete a drive's state DB family with neither the owner probe nor a lock, before the watch is told to reload.
  2. `internal/sync/engine_root_lock.go:46`: the lock keys on the sync folder, the state DB on the drive; two configurations sharing a data dir with different `sync_dir`s for one drive open one store under different locks.
  3. `LockDriveForOfflineChange` keys the recorded folder's lock by the recorded device number, which can change across a reboot, so the key can name another inode or none.
- **Decision (mine)**: an engine takes a second `flock`, on a lock file named for its state DB path (in the data dir's lock folder), after the sync-root lock and before it opens the store, and holds it until the store has closed. `LockDriveForOfflineChange` takes that store lock and the lock of the folder now at the sync path; the recorded-folder lock and `readRecordedSyncRoot` go (the store lock covers an engine started before the folder moved, with no device number involved). Purge: the config section is removed and the watch told to reload first (its reload retires the runner inside the stop bound), then the DB family is deleted under `LockDriveForOfflineChange`; when the lock is refused the DB is kept and the command says so and how to finish (`drive reset-sync-state` or rerun). The sync-root lock keeps its own job (two engines on one folder).
- **Tests first**: `TestEngine_SecondEngineOnSameStoreRefused` (two sync folders, one DB path); `TestLockDriveForOfflineChange_ContendsWithAnEngineWhoseFolderMoved` (no recorded device needed); `TestDriveRemovePurge_KeepsTheStateWhileAnEngineHoldsIt` and the same for `logout --purge`; `TestDriveRemovePurge_NotifiesTheWatchBeforeDeleting`; the lock-release-after-close test extended to the store lock.
- **Docs**: `sync-engine.md` (root lock section, offline-change lock), `sync-store.md`, `cli.md` (the two purge commands lose their [planned] mark), R-2.9.5 and R-6.3.9 text.
- **Live**: on `~/sync-test-root` with an isolated branch binary: `drive reset-sync-state` refused while a watch runs; a purge of a second, throwaway drive section while its watch runs ends with the runner retired and the DB gone.
- **Order**: after B8 (it builds on `LockDriveForOfflineChange`). Cross lane (`internal/sync` and `internal/cli`); before C3 (same store-open path). Size M. No generation bump: the lock file lives beside the root locks.

#### W17. An account email change survives an interruption

- **Source**: W16 report (2026-11-11), "Not fixed" entry 3, and one finding from the review of W16.
  1. `internal/config/email_reconcile.go` `apply` runs four steps (state DB moves, token rename, config section rewrite, catalog) with nothing that records how far it got. A failure after the state DB move leaves the account half renamed: the stores carry the new name, the config and the token the old one. An engine started then creates a fresh store at the old name, and every later rerun of the rename refuses with "already exists".
  2. `internal/sync/store_reset.go` `moveStateDBFamily` renames the sidecars first and the database last, and removes the target's sidecar when the source has none. A rerun after a move that was interrupted between the two renames finds no source `-wal` and deletes the `-wal` it had already moved: the commits not yet checkpointed are lost.
- **Decision (mine)**: the change is journaled and rolled forward. Before the first step a journal file (old email, new email, the list of moves) is written atomically into the data dir. Every step is idempotent: "source missing and target present" means done, "both present" is the only refusal. The journal is deleted after the last step. Every command that loads the config completes a journal it finds before it reads anything else, and an engine does not start on a drive a journal names. A resumed family move never deletes a file at the target. Where the store can be opened, the log is folded into the database (a truncating checkpoint under the store lock) before the move, so the family is one file and the move is one rename.
- **Tests first** (each seen red): a failure injected after each of the four steps, then a rerun that completes with every name new; `TestMoveStateDBFamily_ResumeAfterSidecarsMovedKeepsTheLog`; `TestEngine_RefusesToStartWhileAnEmailChangeIsUnfinished` (or completes it first, whichever the design takes); a rerun with a fresh store at the old name says which file to remove and why.
- **Docs**: `spec/design/config.md` (email reconciliation; W16 wrote a paragraph there, "An email change cut short between two renames of one store … falls back to its last checkpoint", which this PR replaces with the journaled behavior), `sync-store.md` (family move), the requirement the rename traces to.
- **Facts from the W16 review** (pinned by `TestStateDBFamily_SQLitePairsALogWithADatabaseByNameAlone`): SQLite deletes a log left alone when it creates a database at that name, and replays a log beside an existing database whoever wrote it. `MoveStateDBs` already deletes sidecars left without a database at a source (under the store lock), so this PR has no lone-sidecar work. The database moves last, so a move cut short leaves the database at the source and already-moved sidecars at the target: the resume rule must tell those (a journal entry exists: keep them, finish the move) from a stranger's sidecars at the target (no journal entry: clear them, as today).
- **Order**: after W16 (it builds on `MoveStateDBs` and the store lock). Outer lane with one function in `internal/sync`; not tied to C3. Size M.
- **Decision (mine, 2026-11-11)**: the journal is completed at the CLI's entry, by one call before the command resolves its config, never inside `config.Load`. Reason: completing needs the state DB mover, which lives in `internal/sync` and reaches `internal/config` only as an injected function (as today), and a config read that renames files is a side effect a reader does not expect. An engine neither waits nor completes: a runner refuses to start on a drive while a journal exists, with a named error that says any command finishes the change. `sync --watch` passes through the CLI's entry, so a watch started after a crash completes the journal before it starts a drive; a reload during another process's change hits the refusal and the next reload starts the drive.

#### W18. One rule for the modes of folders the client creates in a user's tree

- **Source**: my review of W14's third pass (2026-11-12), which settled the mode of managed-state folders (0o700) and found that folders created in user space follow three rules.
  1. `internal/sync/executor.go:28` `localDirPerms = 0o755`: a folder sync creates inside the sync tree (line 302).
  2. `internal/cli/get.go:21` `defaultDirPerm = 0o755`: a folder a recursive `get` creates (`get.go:273`, `get_shared.go:155`).
  3. `internal/driveops/transfer_target.go:54` `newDirPerms = 0o777`: a missing parent of a `get` target, with the comment that the kernel applies the umask "as for any other program" (P-8). Files already follow that rule: `newFilePerms = 0o666` minus the umask.
  So under a umask of 002 a downloaded file is group-writable (0664) and the folder sync or a recursive `get` made around it is not (0755), while the parent `get` made for a single file is (0775).
- **Decision (mine)**: one rule, the one P-8 already states for files. A folder the client creates inside a user's tree (the sync tree below its root, a `get` target tree) requests 0o777 and the kernel applies the umask. The sync root the client creates stays 0o700 (`cli/sync_dir.go:11`: it is the container the client makes on its own initiative, and what it holds is personal). Managed state stays 0o700 folders and 0o600 files (W14). One constant in `localpath` (or `driveops`) for the user-tree folder mode, used by all three sites; `localDirPerms` and `defaultDirPerm` go.
- **Behavior change**: none under the common umasks 022 and 077. Under a umask that leaves group or other write (002, 000) new folders get those bits, as the files inside them already do. Existing folders are never chmodded.
- **Tests first**: `TestExecutor_CreatedFolderFollowsTheUmask` and the same for recursive `get` (set the umask in a subprocess or use the probe seam `TransferManager` already has; a test may not change the process umask of a parallel suite); a contract test that no product file outside the one constant's home requests a folder mode in a user's tree.
- **Docs**: the principle P-8 text; `sync-execution.md` or `drive-transfers.md` where folder creation is described; `cli.md` for `get`.
- **Order**: none. Cross lane (one line in `internal/sync`, two in `internal/cli`). Size S.
- **Review (mine, 2026-11-12)**: read the report and the product diff. Accepted: (a) three more user-tree sites than this entry named (a shortcut folder, the parents a moved one needs, the missing parents of a restored file): under umask 022 a new one is 0755 where it was 0700, stated as a behavior change; (b) `trash restore` refuses a sync folder that is gone (a re-created one fails the root identity check, and the client makes sync folders only at login and drive add); (c) one `get` walk for a drive path and a shared folder, `folders_created` counts only what the download made, and an unreadable `min_free_space` is an error instead of a silent off; (d) the umask tests run in a child test binary. **Second pass (writer)**: the sync tree's `MkdirAll` and `MkdirAllNoFollow` lose the mode parameter and apply the constant themselves; the unused `rootOps.mkdirAll` seam; `maxListEntries` in the trash; the driveops umask test under `RunUnderUmask`; the rewritten comment cites R-5.10.2, not P-8. **Mine on the rebase after W14 merges** (W14 edits the same lines): doc comments on `localpath`'s exported functions; R-6.8.23 (state modes) moves from R-6.8 Network Resilience to R-6.4 Safety with every citation; `sync-control-plane.md` line 31 (the data directory's mode). **Planned elsewhere**: the undefined P-n labels are W29; a command-level test of a shared single-file `get` is a rider on H7.

#### W19. `status` answers within a bound when OneDrive is slow

- **Source**: seen live on 2026-11-12 at 10:35 Pacific, while Graph answered `GET /me` with a server error after about 21 s each time. `onedrive-go status` took **177 s** (six requests and the retry ladder between them) to print what it knew locally from the start: the account, the folder, "Up to date, watching for changes", the file count. Its only output about the wait was the retry layer's "OneDrive is busy, waiting before retrying" and, at the end, a Warn "status: live account probe failed".
- **Cause**: `loadStatusLiveOverlay` (`internal/cli/status_live.go:27`) asks Graph for each account in turn (`Me`, then `PrimaryDrive`/`Drive` per configured drive) on the command's own context, through a client with the full retry policy, before anything is printed (`status_snapshot.go:214`). The overlay only decorates local state (display name, storage, sign-in health). Nothing bounds it, and N accounts wait N times.
- **Decision (mine)**: the live overlay has one budget, 10 s for all accounts together, and the accounts are asked at the same time. An account whose answer does not arrive in the budget, or whose request fails for a reason other than sign-in, shows its local state, and `status` says so in one stderr line per account (stderr, so `--json` stays one document), with the plain reason ("OneDrive did not answer within 10 s" or the failure's own words). Exit code stays 0, as a failed probe is today. A sign-in failure keeps its present handling (it is an answer). The budget is a named constant behind the existing `statusLiveOverlay` seam, so a test does not wait 10 s.
- **Also in the entry**: the writer lists every other command that decorates local state with a live request before printing (`drive list`, `whoami`, `issues`, `status --perf` if any) and gives each the same bound, or says why it needs none (a command whose whole answer is the live request keeps the retry ladder).
- **Tests first**: `TestStatus_PrintsLocalStateWhenOneDriveDoesNotAnswerInTheBudget` (a fake client that blocks until its context ends; the output holds the local lines and the stderr note; the test's budget is milliseconds); `TestStatus_AsksAccountsAtTheSameTime` (two blocked accounts cost one budget); `TestStatus_ASignInFailureStillMarksTheAccount` (pin).
- **Docs**: `cli.md` (the `status` section), one requirement line under the CLI's responsiveness rules, the output guidelines if they list `status` notes.
- **Live proof**: `onedrive-go status` on the test account still shows the display name and storage; time it.
- **Order**: not at the same time as W6 (both edit `status_snapshot.go` around the overlays). Outer lane (`internal/cli` only). Size S.

#### W20. The account identity check never fails or stalls a command

- **Source**: the nightly of 2026-11-12 (run 43274485305). Graph answered `GET /me` with 502 and 504 while the drive endpoints worked (the running watch kept syncing). The e2e sign-in preflight's plain `onedrive-go ls` failed on it, and so would every file command have.
- **Cause**: `CLIContext.Session` (`internal/cli/root.go:366`) calls `probeAccountIdentity` (`email_reconcile.go:13`), which asks `GET /me` to learn whether the account's email changed, and returns the probe's failure as the command's failure. `ls`, `get`, `put`, `rm`, `mkdir`, `mv`, `cp`, `stat` and the recycle bin commands all start there; `sharedBootstrapMetaClient` and `sharedTargetClients` (`shared_target.go:146`, `:175`) do the same. The three best-effort callers (`reconcileSyncAccounts` in `sync_flow.go:253`, `loadAccountViewSnapshotWithBestEffortIdentityRefresh` in `account_view_snapshot.go:86`, used by `drive search`, `drive add`, `shared` and the account views) log at Debug and carry on, but each waits the whole retry ladder, one account after another. And every file command pays one extra request for an event that happens once in years.
- **Decision (mine)**: the identity check is a freshness step, never a precondition. Three rules, all inside `probeAccountIdentity`, so every caller gets them:
  1. **It cannot fail a command.** A failure other than a sign-in failure is logged at Debug and the command goes on with the stored identity. A sign-in failure keeps its present handling: it is an answer, and the drive requests would fail the same way.
  2. **It is bounded**: one budget of 5 s per probe (a named constant; the request's retry ladder ends with it). Callers that loop over accounts ask them at the same time under that one budget.
  3. **It runs when due, not on every command.** The catalog account records when its identity is next checked: 24 hours after a check that answered, 15 minutes after one that did not. A probe that is not due returns at once without a request. `login` (which has just read the user) records the 24 hours too. A catalog write that fails does not fail the command either (Debug).
  An email change is only a local label change: the sign-in and the drive IDs stay valid, so learning of it up to a day late breaks nothing. `sync` start follows the same rule.
- **Tests first**: `TestSession_AFailedIdentityCheckDoesNotFailTheCommand` (a fake `/me` that answers 503; `ls` lists); `TestSession_AnIdentityCheckThatDoesNotAnswerEndsAtItsBudget` (blocks until the context ends; a millisecond budget); `TestSession_ASignInFailureFromTheIdentityCheckStillFailsTheCommand` (pin); `TestProbeAccountIdentity_SkipsWhileNotDue`, `TestProbeAccountIdentity_AFailureIsRetriedAfterFifteenMinutes`, `TestProbeAccountIdentity_AnEmailChangeIsStillAppliedWhenDue`; `TestSharedTarget_AFailedIdentityCheckDoesNotFailTheCommand`; `TestLogin_RecordsTheIdentityCheck`.
- **Docs**: the design note that owns email reconciliation, one requirement line (a file command needs only its drive's endpoints), `cli.md` if it mentions the per-command check.
- **Live proof**: time `onedrive-go ls /` twice on the test account with `--debug`: the second run sends no `GET /me`.
- **Order**: after W17 (same file and the email-change journal) and after W19 (same budget idea; share the helper if one exists). Outer lane. Size M.
  - Rider (mine, 2026-11-12, seen during the Graph incident of that day): `drive list` ran past 60 s while `GET /me` answered 502 after about 21 s. Its listing goes through `loadAccountViewSnapshotWithBestEffortIdentityRefresh` (`internal/cli/account_view_snapshot.go`), which asks each account's identity one after another and waits the whole retry ladder for each; rules 2 and 3 bound that, and the proof is a test: `TestDriveList_ASlowIdentityAnswerDoesNotDelayTheListing` (a `/me` that never answers; the listing returns within the probe budget with every configured and discovered drive). In the same function, `loadDriveListSnapshot`, the two live discoveries run one after the other and `discoverSharedTargets` (`shared_discovery.go`) asks accounts one at a time, while `discoverAvailableDrives` asks them together: the shared search runs beside the drive discovery and per account at the same time, each goroutine behind the panic boundary the fan-out in `drive_catalog.go` has by then. The discovery calls themselves keep the retry ladder: they are what the command was asked for, and a failed one already reads as a degraded notice. Decision: no budget on discovery.

#### W21. The stall watchdog's attempt check and timer change are one step

- **Source**: the pre-pr gate of the verify-profile branch on 2026-11-12 failed on `TestUploadChunk_RetryAfterLongerThanStallTimeoutIsNotAStall` (load 9.7): the upload was reported stalled 1.05 s into a 3 s retry wait, with a 1 s stall timeout. Not test timing: a race in the product.
- **Cause**: `progressReader.Read` (`internal/graph/transfer_watchdog.go`) loaded the watchdog's attempt number, and armed the timer in a second step. The transport reads a request body on its own goroutine; `RetryTransport` calls the backoff observer (`waitStarts`) on the caller's goroutine before it sleeps. With `waitStarts` between the two steps, the read armed the timer just after the wait had stopped it, and the timer ran through the wait. In production (2-minute stall timeout) a throttle wait longer than 2 minutes failed the upload fragment as stalled whenever that interleaving happened. Two more directions found by reading: an abandoned attempt's read that ends after the next attempt started disarmed the next attempt's watchdog (a real stall would then hold its worker for good), and a read after `stop` left a timer running.
- **Decision (mine)**: one mutex in `stallWatchdog` makes "is this reader's attempt current" and the timer change one step, for arm and for disarm. `stop` retires the readers too, so a late read by the transport cannot start the timer after the transfer ended. No test seam in product code.
- **Tests first** (all seen failing on the old code): `TestStallWatchdog_AWaitThatStartsDuringAReadLeavesTheTimerStopped` (two goroutines, 20000 rounds; failed 5 of 5 runs before the fix), `TestStallWatchdog_AnAbandonedAttemptsReadCannotDisarmTheNextAttempt` (deterministic, through a gated inner reader), `TestStallWatchdog_NothingArmsItOnceTheTransferHasEnded`.
- **Docs**: `spec/design/graph-client.md`, the stall watchdog paragraph. R-6.8.20 is unchanged.
- **Live proof**: none possible (it needs a throttle answer at a chosen instant); the race detector run and the 20-fold repeat stand in.
- **Order**: free of every open branch. Outer lane. Size S.

#### W22. An account email change reaches everything that names the old email

- **Source**: the report of the email-change journal work (W17, 2026-11-12), "Seen and not fixed" entries 1 to 4, and my review of it (item 5).
  1. `internal/config/email_reconcile.go` `buildEmailReconcilePlan` with `internal/config/mount_state_path.go` `ChildMountID`: a shortcut child's state DB is `state_mount_<digest of the mount ID>.db`, and the mount ID is the parent's ID (which holds the email) plus the binding. The digest changes with the email and nothing moves the file: the child starts on a fresh store (an empty baseline over a tree that is already there) and the old store is orphaned.
  2. A running `sync --watch` is not told about a change another process made or finished. It rereads its config file by itself within about 10 s (`internal/multisync/watch_config_file.go`), but until then a runner of a paused or backed-off drive can start an engine under the old name, which makes a fresh store there and plans against an empty baseline.
  3. `collectConfiguredDriveRenames` rewrites only the config file the command runs with. A second config file that shares the data dir keeps sections under the old email.
  4. `collectStateRenames`: a shared drive's state DB whose catalog record is missing is skipped with a Warn in the log and stays under the old email.
  5. (Review of W17, 2026-11-12.) A config section can store a `display_name`. `drive add` writes the default derived from the drive's ID when it cannot resolve the shared name (`internal/cli/drive_add.go`, the two fallback sites; "Shared from <email>"), and `renameJournalDriveSections` renames the section header only, so that stored default keeps the old address and is what `status` and `drive list` show. W17 fixed the same thing for the catalog drive record (`renameCatalogAccount`), which nothing shows.
- **Decision (mine)**:
  1. **Child stores move with the parent, inside the same journal.** The mount ID scheme stays (an email-free scheme would need a one-time rename of every existing child store, which is this same machinery, and the parents would still be named by email). While planning, the CLI asks the sync package for the child mounts of each parent store the change moves (read under the parent's store lock), and the plan names each child move (`ChildMountID` of the old parent ID to that of the new one) before the first rename. The writer lists every other data-dir name that derives from a mount ID or a canonical ID (locks excepted: they are keyed by path and die with the process) and moves or justifies each.
  2. **A watch runner starts an engine only for an account the catalog still holds.** It rereads the catalog at each engine start; a drive whose ID names an account the catalog no longer has does not start and waits for the reload. The catalog step comes before the journal is deleted, so with the journal's refusal there is no instant at which a stale runner can open the old name. This is a general rule (it covers a logout racing a stale runner too). And **the change tells a running watch at once** (`notifyDaemonIfRunning`, as login does) after a command applied or finished one, except when the command is the watch itself.
  3. **A second config file is caught up.** The catalog account keeps its former emails. At the CLI's entry, after an unfinished change is finished, a config file with a section under a former email of an account (and no account now has that email) gets the same idempotent section rename, under the config lock, with one line to the user. A login that brings an email takes it off every other account's former list.
  4. **A shared store's name is the evidence when one account had the email.** The catalog lookup is needed only when two accounts of different types shared the old address. With one, the store moves. With two and no record, the store stays, and the result says so: one line to the user naming the file, not a Warn in the log.
  5. **A stored default name follows the address; a name the user or Graph supplied stays.** In the same section rename, a `display_name` equal to `DefaultDisplayName` of the old ID becomes that of the new ID. The comparison is exact, so a name the user typed that merely contains the address is left alone. The rule is the one the catalog step already applies, kept in one function both steps call.
- **Rider (2026-11-12, from the review of H4)**: once H4 is merged the catalog decoder and encoder refuse a record whose ID does not parse, so the `continue` after `driveid.NewCanonicalID(drive.CanonicalID)` in the catalog step of the email change (`internal/config/email_reconcile.go`, the loop over `catalog.SortedDriveKeys()`, wherever W17 left it) can never run. Delete the branch here; if the compiler still needs the parse result, the failure is an error that stops the step, not a skip. If H4 has not merged when W22 starts, leave it and move the rider to the next change that touches the file.
- **Tests first**: `TestReconcileAccountEmail_MovesAShortcutChildsStore`; the step table of `TestReconcileAccountEmail_AFailureAfterAnyStepIsFinishedByTheNextCommand` gains a row between a parent move and its child's; `TestRunner_DoesNotStartAnEngineForAnAccountTheCatalogNoLongerHolds` (multisync); `TestReconcileGraphUser_TellsARunningWatch`, `TestFinishInterruptedEmailChange_TellsARunningWatch`, `TestWatch_AChangeItFinishedItselfSendsNoReloadToItself`; `TestInitializeCLIContext_ASecondConfigFileIsCaughtUpFromAFormerEmail`, `TestLogin_ANewAccountTakesItsEmailOffAnotherAccountsFormerList`; `TestReconcileAccountEmail_MovesASharedStoreWithNoCatalogRecordWhenOneAccountHadTheEmail`, `TestReconcileAccountEmail_ReportsASharedStoreItCannotAssign`; `TestReconcileAccountEmail_AConfigSectionsDefaultNameFollowsTheAddress`, `TestReconcileAccountEmail_AConfigSectionsOwnNameStays`.
- **Docs**: `spec/design/config.md` (Email Reconciliation: the plan's contents, former emails, the second config file), the multisync runner start rule where runner lifecycle is described, `cli.md` (entry call), one requirement line beside R-3.7.3.
- **Live proof**: in an isolated data dir copied from the test bundle, rename the token, the config section, the catalog account and a state DB to an invented old address (same user ID), with a second config file and a child mount store; run `status`; every name is new, the child store kept its content, the second config is caught up on its first use. The test account only.
- **Order**: after W17 (it builds on the journal) and after W20 (same probe function). Not together with W6 (the daemon notifier in `internal/cli/pause.go`). Cross lane: `internal/config`, `internal/cli`, `internal/multisync`, one read function in `internal/sync`. Size L.

#### W23. Watch tests prove absence by a signal, and the chaos model takes the session route

- **Source**: the report of the sync test harness work (W12, 2026-11-12), "Seen and not fixed" entries 3, 4 and 5. (Entries 1, 2, 6 and 7 went back to that writer and are fixed in W12 itself.)
  1. Four tests prove that nothing happened by waiting a short fixed time: `internal/sync/observer_local_collisions_test.go:248`, `internal/sync/observer_local_write_test.go:438`, `internal/sync/engine_watch_test.go:207`, `internal/sync/observer_remote_test.go:1526` (line numbers as on W12's branch). On a loaded host the event the test rules out can arrive after the window, so the test passes whether the product is right or wrong.
  2. The model never writes a file above the simple-upload limit (`internal/sync/engine_model_based_test.go:433`), so `TestModel_ChaosGraphConverges` never takes the upload-session route. W12 put the session route through the chaos layer and proved it with one direct test; the model still does not draw from it.
  3. The scenario fake's session upload hands over every byte in one call (`internal/sync/scenario_remote_session_test.go:223`), so a fault in the middle of a session never leaves a part-uploaded session for the engine to resume.
  4. (Rider, from the graph client leftovers report, W4, 2026-11-12.) The chaos remote's create fault (`internal/sync/chaos_remote_test.go`, `chaosCreateFailure`) re-derives by hand which create failures carry `graph.ErrCreateOutcomeUnknown`, and it cannot express a create that never left the client (token failure, ended context, unsent retry), which since W4 is known to have created nothing.
- **Cause**: the harness has no signal for "the observer saw this and decided to do nothing", so absence can only be waited out; the model's operation set and the fake session were sized for the small-file route.
- **Decision (mine)**:
  1. **Absence is asserted after a positive signal.** Each of the four tests waits (within `eventWait`) for the signal that the product handled the input, then asserts that nothing was emitted. Where the engine's debug-event hook already reports the decision, the test uses it. Where the observer drops an input without a trace, it gains one debug event at the drop site through the hook the tests already use (no new seam type, no sleep, no test-only branch in the decision). A test for which no handled point exists says why in a comment and keeps its window; the PR lists it.
  2. **The model writes large files under chaos.** The model's operation set gains a write above the simple-upload limit, the chaos runs wrap the session route, and the run asserts that it drew a session fault for the seeds that are listed as drawing one. Seeds are re-picked if the new operation changes the draws; the race seed keeps its create-fault and session-fault assertion.
  3. **The fake session takes fragments.** It accepts ranges, answers with the next expected ranges, and keeps what it has across a fault, so a mid-session fault is followed by a resume that sends only the rest. The model's oracle checks the final content, and one direct test checks that the resume did not resend accepted bytes.
  4. **The chaos create fault follows the graph client's rule.** The fault set gains a not-sent create (no item appears, no mark, the intent is cleared), and the choice of which faults carry the mark is written once beside the fault kinds with a test that pins it against `graph`'s own classification (a status table the test feeds through `graph`), so a change of the rule in `graph` fails the chaos layer's test instead of silently diverging.
- **Tests first**: `TestScenarioSession_ResumeSendsOnlyTheMissingRanges`; `TestChaosCreateFault_MarksWhatTheGraphClientMarks`; `TestModel_ChaosGraphConverges` gains the large-file operation and the session-fault assertion (it fails today: no session fault is ever drawn by the model); each of the four absence tests is first made to fail by reordering the product decision (kept as a note in the PR, not in the tree) to show that the new form can fail.
- **Docs**: `spec/design/system.md` Verification Policy (absence is asserted after a handled signal, never by a window alone; the model covers the session route).
- **Live proof**: none needed; test files and at most debug events change. `go test -race -cpu 1` of the touched tests beside a parallel load, as W12 did, and the full non-race `./internal/sync/...`.
- **Order**: after W12 (same files). Before A11, whose seed helper must keep the session-fault assertion. Sync lane. Size M.

#### W24. A request's deadline reaches the token refresh and the retry wait

- **Source**: the report of the status budget work (W19, 2026-11-12), "Not fixed" entries 2 and 3, and my review of it.
  1. `internal/graph/client.go:68`: `TokenSource.Token()` takes no context. A refresh runs under the context the source was built with (the command's), so it is bounded only by the sign-in client's 60 s and can outlast a caller's budget: `status` promises an answer in about 10 s and can take 70 when the token endpoint is slow. For the same reason `status` must build its clients on the command context, because a source built under a budget context would stay dead after the budget ended.
  2. `internal/retry/transport.go:303` and `:305`: when the wait before the next attempt would end after the request's deadline, the transport still announces it at WARN ("OneDrive is busy, waiting before retrying") and sleeps until the context ends. The caller then gets a context error in place of the answer OneDrive gave, later than it could have had that answer.
- **Decision (mine)**:
  1. **`Token(ctx)`.** The token source takes the caller's context for each call. A refresh is one request under that context and the sign-in client's own timeout, whichever ends first. A refresh a caller's context ended leaves the source usable: the next call refreshes again, and a refresh already under way for another caller is not canceled by this caller's context (one refresh at a time, waiters each bounded by their own context). The saved token file is written only by a refresh that finished.
  2. **A wait that cannot finish before the deadline ends the retries.** The transport compares the wait with the request context's deadline before it drains the answer. If the wait would end after the deadline, it returns the answer it has, exactly as it does when the attempts are used up: no wait, no long-wait notice. Callers then classify the real answer (throttled, server problem) and get it sooner. A context with no deadline behaves as today. The writer lists every caller that treats a deadline error from a retried request specially and shows each still does the right thing with the answer.
  3. `status` says what happened: with decision 2 its note for a server error still failing at the budget's end becomes "OneDrive had a temporary server problem", and with decision 1 the budget covers the refresh, so the sentence in `spec/design/cli.md` about the 60 s residual goes.
- **Tests first**: `TestTokenSource_ARefreshEndsWithTheCallersContext`, `TestTokenSource_ARefreshOneCallerAbandonedStillServesTheNext`, `TestTokenSource_AnAbandonedRefreshWritesNoTokenFile`; `TestRetryTransport_AWaitPastTheDeadlineReturnsTheAnswerAtOnce` (429 with a long Retry-After and a 503), `TestRetryTransport_AWaitPastTheDeadlineIsNotAnnounced`, `TestRetryTransport_NoDeadlineWaitsAsBefore`; `TestStatus_ASlowTokenRefreshIsCutByTheBudget`; the 503 row of `TestStatus_SaysWhyOneDriveGaveNoDetails` changes its expected reason.
- **Docs**: `spec/design/graph-client.md` (token source, 401 Token Refresh), `spec/design/retry.md` or the retry section of `graph-client.md` (the deadline rule), `spec/design/cli.md` (status budget), R-6.8.28 in `spec/requirements/non-functional.md`.
- **Live proof**: `status` and one file command on the test account with the branch binary in an isolated tree; a forced refresh (an expired access token in the copied token file) works and is saved.
- **Order**: after W19 (it edits the status overlay and its tests) and after W4 (same files in `internal/graph`); before B6 (it changes the token source). Outer lane: `internal/graph`, `internal/retry`, `internal/cli`, every `TokenSource` implementer and fake. Size M.

#### W25. The status model has one field per fact

- **Source**: my review of the status budget work (W19, 2026-11-12).
- **Cause**: `internal/cli/status_snapshot.go` carries every drive twice. `statusAccount` has `Drives` (rendered, in JSON) and `Mounts` (`json:"-"`, line 74); `statusDrive` has the public `InternalID`, `Name`, `Folder`, `Kind`, `SharedFolders` and the internal `MountID`, `ProjectionKind`, `CanonicalID`, `DisplayName`, `SyncDir`, `ChildMounts` (line 129). The builder fills the internal set (`status_snapshot.go:445`, `:448`), the overlays write to it (`status_runtime.go:66-67`, `:95-96`; `status_perf.go:85-86`, `:108-109`), `dropStatusDriveDebugDetail` clears it (`status_snapshot.go:239`, `:247`), and rendering copies internal to public in `normalizeStatusAccounts` (`status_render.go:115`) and `normalizeStatusDrives` (`:136`) and once more in `printAccountStatus` (`status_render.go:203-207`). The alias `statusMount = statusDrive` (`status_snapshot.go:57`) has no product use and 41 test mentions. A reader cannot tell which field holds the truth at a given point, and a new overlay has to know to write to the internal one.
- **Decision (mine)**: one field per fact. The builder fills `Drives`, `SharedFolders` and the public fields directly; overlays and the debug-detail drop work on them. `Mounts`, `ChildMounts`, the internal fields whose only job is to feed normalization, the alias `statusMount` and the normalize functions as a bridge go. Whatever normalization derives (a label, a default name) is derived once in the builder. A field the product reads but JSON must not show stays, tagged `json:"-"`, with a comment that says who reads it. The JSON and text output do not change by a byte.
- **Rider (2026-11-12, from my review of B2)**: `internal/cli/status_snapshot.go` takes a `history` parameter it never reads (`_ = history`, about line 901), threaded through `querySyncStateWithOptions` and `liveSyncStateQuerier`. Remove the parameter and every argument passed for it; if a caller computed the value only to pass it, that computation goes too.
- **Tests first**: the golden-file and render tests are the oracle and must pass unchanged; add `TestStatus_JSONIsUnchangedByTheModel` only if no existing test pins the whole `--json` document for an account with a child folder and a running sync (the writer checks and says which test does). About 41 test sites move from `statusMount` to `statusDrive`; the test helpers `printMountStatus` and `statusMountLabel` in `status_test.go` go with them.
- **Docs**: `spec/design/cli.md` (status model), only where it names the removed fields.
- **Live proof**: `status` and `status --json` on the test account with the branch binary, compared byte for byte with the installed binary's output for the same state (run both within a few seconds with the watch idle).
- **Order**: after W19 (same files). Before W27, which removes test-only names in the same files. Outer lane: `internal/cli` status files only. Size M.

#### W26. Human units are rendered by one package

- **Source**: the report of the status budget work (W19, 2026-11-12), "Not fixed" entry 3, and my review of it.
- **Cause**: the rule is one sentence, `spec/reference/cli-output-guidelines.md:88-89` ("6 s", "2 min 10 s", "300 MB"). Five product functions render durations, each with its own rounding:
  1. `retry.HumanDuration` (`internal/retry/notices.go:79`): whole seconds, never below "1 s", "1 min 30 s", no hours. Callers `internal/retry/transport.go:229`, `:376`, `internal/cli/status_live.go:137`.
  2. `humanDuration` (`internal/cli/sync_console.go:586`): tenths below a minute, minutes and seconds truncated ("2 min 0 s"), hours as "%d h %d min". Callers `sync_console.go:337`, `:340`.
  3. `humanDelay` (`internal/multisync/watch_reporting.go:256`): can print "0 s".
  4. `waitText` (`internal/sync/observer_remote.go:886`): "5 min" rounded to the minute, "1 h" rounded to the hour.
  5. `formatPerfElapsed` (`internal/cli/status_perf.go:244`): Go's own format ("1.5s", "0s"), against the rule.
  Also `internal/cli/auth_flow.go:434` ("in %d minutes"), and two copies of `formatSize` (`internal/cli/format.go:113`, `internal/driveops/transfer_manager.go:118`). devtool's `formatDurationMS` (`internal/devtool/verify_summary.go:260`) is developer output and is left.
- **Decision (mine)**:
  1. One leaf package (no imports from this repository; not `internal/retry`, which is about retries) with one duration function and one size function. Every product formatter above is replaced by a call to it and deleted.
  2. The duration function takes the precision as an explicit argument, because the two uses differ in what is true: a **wait or a limit** is rendered in whole seconds and never below "1 s"; a **measured time** is rendered in tenths of a second below a minute. The value is rounded first, then split, so 59.96 s prints "1 min" and never "60.0 s".
  3. From a minute up: whole seconds, zero parts dropped ("2 min", not "2 min 0 s"). From an hour up: hours and whole minutes ("1 h 15 min", "1 h"), no seconds.
  4. `waitText`'s coarse rounding (a wait of 4 min 40 s reads "5 min") is not kept: the poll wait is a real duration and reads as one. If a caller wants a round number it rounds the value before the call.
  5. `formatPerfElapsed` uses the measured form. `--json` output keeps its numeric fields.
  6. The auth flow sentence uses the function too.
- **Behavior changes** (texts only; the PR lists each with before and after): an exact-minute elapsed time reads "2 min" instead of "2 min 0 s"; seconds above a minute are rounded instead of truncated; an hour or more of retry wait reads "1 h 15 min" instead of "75 min"; a zero delay reads "1 s" where it read "0 s"; the remote poll notice shows the real wait; `status --perf` elapsed reads "1.5 s" instead of "1.5s".
- **Tests first**: one table test in the new package covers both precisions and every boundary (0, 0.04 s, 0.96 s, 59.94 s, 59.96 s, 60 s, 119.5 s, 3599.5 s, 3600 s, 4500 s, a negative value); one for sizes. The five old table tests are replaced by it; each caller keeps one test of its own sentence with the new text. A devtool check or a `forbidigo` rule refuses `time.Duration` formatted with `%v`, `%s` or `.String()` in user-facing output packages if it can be done without false positives; if not, the PR says why.
- **Docs**: `spec/reference/cli-output-guidelines.md` (the rule names the package and the two precisions), `spec/design/system.md` package list, any spec text that quotes a changed sentence.
- **Live proof**: one sync on the test account with the branch binary shows the "Done in" line; `status --perf` shows the elapsed form.
- **Order**: after W19 (`status_live.go` calls the formatter). Not at the same time as W24 (both edit `internal/retry/transport.go`). Cross lane: `internal/retry`, `internal/cli`, `internal/multisync`, `internal/sync`, `internal/driveops`. Size M.

#### W27. No product code that only its tests call

- **Source**: my review of the status budget work (W19, 2026-11-12), which found ten functions and two interfaces in the status files that only tests called, and a sweep of the whole tree afterwards; the report of the sync test harness work (W12), which names a branch and three accessors only tests reach.
- **Cause**: golangci-lint's `unused` counts a test file as a user, so a function whose last product caller went away stays as long as it has a test. The test then proves something the product does not do, and the next reader takes the function for the product's path. The sweep found 39 unexported names that no non-test file of their package mentions (some are false positives; the writer decides each):
  - `e2e`: `formatAuthPreflightFailure`, `ensureCatalogDriveRecords` (helpers of a test package: likely fine, check).
  - `internal/cli`: `clearAccountAuthRequirement`, `hasPersistedAccountAuthRequirement` (`auth_health.go`); `resolveLogoutAccount` (`auth_logout.go`); `buildConfiguredAuthRequirements` (`drive_catalog.go`); `resumeSingleDrive` (`resume.go`); `buildHandler`, `buildLogger`, `buildLoggerDual`, `newRootCmd` (`root.go`); `printStatJSON`, `printStatText` (`stat.go`); `statusMount` (`status_snapshot.go`; goes in W25); `mountReportsError` (`sync_render.go`).
  - `internal/config`: `decodeDriveSections`, `decodeDriveSectionsLenient` (`decoder.go`); `tokenAccountCID` (`token_resolution.go`).
  - `internal/devtool`: `lookupSyncPartialLocalCatchup100MScenario` (`bench_live.go`); `hitDeadline` (`go_test_log.go`); `discoverTaggedE2ETests`, `fullE2EStandaloneTests` (`verify_e2e.go`).
  - `internal/graph`: `buildDeltaPath`, `buildFolderDeltaPath` (`delta.go`).
  - `internal/multisync`: `buildRuntimeWork`, `expectedChildRootIdentity` (`mount_spec.go`); `shortcutChildArtifactCleanupClass` (`shortcut_child_artifacts.go`).
  - `internal/sync`: `dueTrials`, `scopeKeys` (`active_scopes.go`); `observeRemoteFull` (`engine_run_once.go`); `recordBlockedRetryWork` (`engine_runtime_lifecycle.go`); `conflictCopyPath` (`executor_conflict.go`); `validateOneDriveName` (`scanner.go`); `shortcutChildRunModeForRoot` (`shortcut_root_publication.go`); `allComparisonKinds`, `allReconciliationKinds` (`sqlite_compare_kinds.go`; registry lists a test reads on purpose: likely kept, moved to a test file if nothing else reads them); `cachedBaseline` (`store.go`, 29 test mentions); `readPathTruthStatus` (`store_inspect.go`).
  - From W12's report: the path-prefix branch of the orphan finder that no caller uses (`TestFindOrphans_WithPathPrefix`), and the remote observer's `LastActivity`, `Stats` and hash counter accessors that no production code reads.
  - Rider from W9's report (2026-11-12; deadcode from `main`), names the sweep above missed in `internal/sync`: `extendScopeTrial` and `earliestTrialAt` (`active_scopes.go`), `kickDueHeldRelease` (`engine_watch_state.go`), `watchRegistry.paths` (`observer_local.go`), `projectObservedItems` (`observed_items.go`; tests go through `projectRemoteObservations`, which the product calls). The sweep looked only at unexported package-level names no non-test file mentions; methods and names a dead function mentions escaped it. The writer runs `deadcode` from `main` first and works from its list, and the gate must catch methods too. W9's report ("Not fixed", entries 2 and 3) lists more, all in scope: `dueTrials`, `scopeKeys`, `observeRemoteFull`, `recordBlockedRetryWork`, `conflictCopyPath`, `validateOneDriveName`, `shortcutChildRunModeForRoot`, `cachedBaseline`, `readPathTruthStatus`, `allComparisonKinds`, `allReconciliationKinds`, and whatever else that list names.
  - Rider from H4's report (2026-11-12): `config.AppendDriveSection` (`internal/config/write.go`) is called only by tests since enrollment became one transaction (28 test files in config, cli, multisync). One shared test-support writer under a test-only package, or `AddDrive` in the fixtures; the product function goes.
  - Rider from my review of B9 (2026-11-12): three clock defaults in `internal/sync` are reached only by tests once the engine hands its clock to everything it builds: the `realClock{}` default in `newLocalObserver` (`observer_local.go`) and `newRemoteObserver` (`observer_remote.go`), and the nil `opts.Clock` fallback of the socket.io wake source (`socketio.go`). The constructors take the clock as a parameter (about 200 test call sites, mechanical) and the fallback goes; the source test that holds "product code builds a real clock in one place" then needs no exception for a default.
- **Decision (mine)**:
  1. Each name gets one of three ends. **Dead**: nothing but its own test needs it; the function and the test go. **Test helper**: tests of other behavior use it to set up or read state; it moves into a `_test.go` file (or `export_test.go` style helper) so the product binary does not carry it. **Wrong entry point**: tests call an inner function while the product calls an outer one; the tests are repointed at what the product calls, and the inner function is inlined or unexported further if that leaves it with one caller.
  2. No coverage is lost silently: for every test deleted or repointed the PR says what still covers the behavior, as W19's table did.
  3. **A gate keeps it out.** `verify lint` gains one more run: `unused` alone with test files excluded (`--tests=false`), so a function only tests call fails the lint. If the extra run costs more than about a minute on this host, the gate is a devtool check that does the same with `go/packages` instead. Exceptions are named in one list with a reason each (for example a registry list a test reads from source).
  4. The same sweep is run for exported names inside `internal/` that only test files of other packages use; they are listed and handled the same way, or named as out of scope with the reason if the list is long.
- **Tests first**: the gate itself is the test: it fails on today's tree with the 39 names, and passes at the end. A devtool test proves the gate fails on a planted test-only function and passes when a non-test caller is added.
- **Docs**: `spec/design/system.md` Verification Policy (the rule and the gate), the repository's `CLAUDE.md` gate list.
- **Live proof**: none needed for deletions; one sync and `status` on the test account with the branch binary, because `root.go` and the auth helpers are touched.
- **Order**: after W19 and W25 (status files), and after the open branches that touch `internal/cli/root.go`, `auth_health.go` and `internal/sync` have merged, or it will conflict with each. Cross lane. Size M; split by package (cli and config; sync and multisync; graph and devtool with the gate last) if the diff passes about 1,500 lines.

#### W28. An error without a format verb is `errors.New`, held by the linter

- **Source**: the report of the graph client leftovers work (W4, 2026-11-12), which named three `fmt.Errorf` calls without a format verb, and my count of the whole tree afterwards.
- **Cause**: `fmt.Errorf("fixed text")` formats nothing. It reads as if the message carried a value or wrapped a cause, it cannot be a package-level sentinel, and nothing stops the next one: `.golangci.yml` has no rule for it. The count on 2026-11-12 (pattern `fmt\.Errorf\("[^"%]*"\)`): 277 in 111 Go files, 213 of them in product code. The most are in `internal/driveops/transfer_manager_test.go` (15), `internal/devtool/bench.go` (14), `internal/sync/executor_test.go` (9), and seven each in `internal/devtool/verify_dod.go`, `internal/sync/mock_item_client_test.go`, `internal/sync/engine_runtime_invariants.go` and `internal/driveid/canonical.go`. `internal/graph` is cleared by W4.
- **Decision (mine)**:
  1. One mechanical PR. Every such call becomes `errors.New` with the message unchanged byte for byte; no behavior changes and no message is reworded.
  2. **A linter rule keeps it out**: the `perfsprint` check for it (`errorf`), enabled alone if the linter's other checks would widen the diff, or the nearest equivalent golangci-lint 2.11.3 offers. If none fits, one devtool repo check with the same pattern. The rule covers test files too.
  3. Where the same fixed message is built in more than one place of a package and callers compare it with `errors.Is` or by text, it becomes one package-level sentinel; that is the only non-mechanical edit allowed, and the PR lists each.
- **Tests first**: the rule itself is the test: it fails on today's tree, and passes at the end. No unit test changes behavior.
- **Docs**: the repository's `CLAUDE.md` or `spec/design/system.md` wherever lint rules are listed.
- **Live proof**: none (no behavior change).
- **Order**: late, when few branches are open, because it touches most packages and would conflict with every one of them. Cross lane. Size M.

#### W29. Principle labels (P-n) that nothing defines are replaced by requirement IDs, held by a check

- **Source**: the report of the folder-mode work (W18, 2026-11-12) and my count afterwards.
- **Cause**: comments and spec text cite labels `P-1` to `P-15` ("(P-4)", "policy P-6", "product decision P-1") as if a reader could look them up. No file in the repository defines them, and git history holds no file that did after the spec migration (#221): they came from working notes outside the repository. Count on 2026-11-12: 33 lines in Go files (`internal/sync` 21, `internal/driveops` 4, `internal/config` 3, `internal/cli` 2, `internal/localtrash` 1 and tests) and about 20 lines in `spec/` (`requirements/sync.md`, `transfers.md`, `non-functional.md`; `design/sync-observation.md`, `sync-planning.md`, `sync-execution.md`, `drive-transfers.md`, `config.md`, `cli.md`). Labels seen: P-1 to P-12 and P-15.
- **Decision (mine)**:
  1. The labels are dropped, not defined. A second ID system beside the requirements would need its own registry and its own check; the requirements already hold each rule. Each citation becomes the requirement ID that states the rule (most citing lines sit next to one), or plain words where the label marked something not built (`P-12`, `P-15` in `sync-observation.md`): those sentences say "not implemented" and nothing else.
  2. Where a label is the only statement of a rule the code keeps (to be found per label: `P-1` in `sync-planning.md`, `P-6` "OneDrive's location wins"), the rule gains or is matched to a requirement first, and the citation uses that ID.
  3. **A repository check keeps them out**: the devtool step that already holds the retired backlog IDs also fails on a `P-<number>` label in Go files, `spec/` and `docs/`.
  4. No behavior change; no code but comments and the check.
- **Tests first**: the check's unit test (a file with a label fails, with file and line; today's tree fails before the edit and passes after).
- **Docs**: wherever the repository lists its checks (`spec/design/system.md`, the repository's `CLAUDE.md`).
- **Live proof**: none.
- **Order**: late, with W28: about 35 files change by a line or two each. Cross lane. Size S.
- **Rider (2026-11-12 23:46, mine): the file-mode requirement leaves Network Resilience.** `R-6.8.23` states that the client's own files and folders are private (state folders `0700`, files `0600`). It is filed under "R-6.8 Network Resilience" in `spec/requirements/non-functional.md`; it belongs under "R-6.4 Safety". It becomes `R-6.4.11` (R-6.4 holds 1 to 4 and 7 to 10 once the folder-mode work and the stack below it have merged; no branch uses 11), with every citation: `internal/config/write_test.go`, `internal/logfile/logfile_test.go`, `spec/design/cli.md`, `spec/design/config.md`, `spec/requirements/non-functional.md` and, from the folder-mode work, `spec/requirements/transfers.md`; grep the tree again when the work starts. The old ID is not reused and gets no tombstone line unless the traceability check asks for one. This was first listed as a rebase item of the folder-mode PR (W18). It moved here because, done mid-stack, it rewrites lines (`config.md` line 3, the `cli.md` table rows, `non-functional.md`) that every branch above it also edits; done late it conflicts with nothing. Same end state.
- **Rider (2026-11-15, from a collision caught at a rebase)**: live incident ids (`LI-<date>-<nn>`) are allocated by hand, and nothing reads `spec/reference/live-incidents.md`. Two branches that each recorded an incident on 2026-11-15 both took `-01`; only the rebase conflict in the index showed it. Add to this row's check: every id in the index is unique, every index row has exactly one entry heading with the same id and title and every entry heading has its index row, and every `live-incidents.md#li-…` link in the spec tree points at an existing heading.

#### W30. A removed shared folder's local copy leaves by the rule of any folder deleted on OneDrive

- **Source**: the third-pass review of the exit-status work (B2, 2026-11-12): its writer's list of files that exist only locally and that the release still removes, and my trace of the release path afterwards.
- **Cause**: when a shared folder's shortcut is removed and its final drain is clean, the parent engine releases the folder's local tree with `synctree.RemoveTreeNoFollow` (`removeShortcutRootProjection` in `internal/sync/shortcut_root_lifecycle.go`, reached from `acknowledgeChildFinalDrain` and again from the local-state reconcile). The walk removes every regular file it meets: no check against the child's baseline, no content verification, no trash. So it removes files sync never uploaded: files the content filter hides, a file written after the drain's last scan, entries under names sync reserves. It stops with an error at the first symbolic link or special file, after it has removed everything sorted before it, and the binding sits in `removed_cleanup_blocked` over a half-removed tree (that state publishes no child run; the parent's local-state reconcile retries the walk each run). The product's own rule for a folder deleted on OneDrive (`executor_delete.go`) is the opposite on every point: a file leaves only when its content still matches the baseline, through one descriptor, to the trash; a folder leaves only when empty (bundled junk is disposable under the configured rule); what remains is named ("folder still contains files that are not on OneDrive").
- **Unverified, to establish first**: `removed_cleanup_blocked`, `removed_final_drain` and `removed_child_cleanup_pending` all take `RemoteUpsert → Active`. If the shortcut comes back after a partial removal (or after a full one, before the child's state is purged), the child runs with a baseline that still holds rows for files the walk removed: local absent, baseline row present, remote present reads as "deleted locally" and plans deletes in the sharer's folder. The first test builds that state and asserts that no remote delete is planned. If it passes on today's code (the root identity check or the delete hold may already stop it), it stays as a pin and the report says which guard held.
- **Decision (mine, 2026-11-12)**: one rule, *sync removes a local file only when it has verified it as synced*, and the release is no exception.
  1. The release is the last step of the child's final drain, in the child engine, through the executor's ordinary local delete: each file is verified against the baseline and goes where a file deleted on OneDrive goes (the trash under the configured rule), and its baseline row leaves in the same commit as for any local delete, so a later run can never read "local absent, baseline row present" from a release. Folders leave bottom-up by the ordinary folder rule. It runs only after the drain itself is clean (`Report.LocalTreeHolds` holds nothing).
  2. What the rule does not remove stays, and the drain is then not clean. (a) A visible file that changed or is new since the scan is ordinary left work: the next final drain uploads it and releases again. (b) Content sync does not manage (hidden by the filter, a symbolic link, a special file, a reserved name) is reported by the engine as a count with at most three names; the drain result is `blocked` with the folder-not-empty wording and what to do (move or delete them), and status shows it. No kept-and-forgotten folder at the parent: that would need the parent to learn the child's kept folders across the store boundary, and a wrong step there uploads the leftovers into the user's own drive. A blocked drain is already retried on every child pass and reported once.
  3. After the acknowledgement the parent removes the root only as an empty directory, never by a tree walk. A root that is not empty at that moment goes back to the final drain (the child runs again); `removed_cleanup_blocked` stays only for a root no drain can fix (the path is not a directory, the parent folder refuses the removal). The state-table change is shown in the report, transition by transition. `RemoveTreeNoFollow` has no other product caller: it is deleted with its tests if nothing else needs it.
  4. The delete-safety hold does not apply to the release's removals: they are not a user's deletes and nothing is deleted on OneDrive. The design doc says so.
  5. A drain whose shared folder can no longer be reached (`root_unavailable`, or access revoked): the writer first states what happens to the local tree today. The same rule holds: files that match the baseline may leave; a file with unsynced changes stays and is named, with wording that says it could not be uploaded.
  6. A crash in the middle of the release converges without a remote delete (test). Files already released may be downloaded again by the next drain and released again; that cost is accepted and stated.
  7. Whether a blocked release counts as left work for the exit status follows the exit rule as it is (an issue that keeps a file unsynced); this change does not alter that rule, and the report says what a one-shot sync exits with while a release is blocked.
  8. If the executor's local delete cannot be driven for a file whose remote item still exists without a new action kind, the writer stops on that and reports what collides before inventing one.
- **Tests first**: the re-add test above; under a released folder each of these stays, and the drain is blocked or left work by kind: a file the filter hides, a file with no baseline row, a file whose content changed after the scan, a symbolic link, a FIFO; a tree of synced files leaves completely, to the trash, with no baseline row left; bundled junk leaves under the configured rule; a symbolic link sorted after regular files leaves no half-removed tree with baseline rows; the parent refuses to remove a non-empty root and the binding returns to the final drain; a crash point between two removals, then a run: no remote delete; multisync: the blocked detail names the leftovers.
- **Docs**: R-2.4.8 (what the release removes and what stays), the shortcut lifecycle in `spec/design/sync-engine.md` and `sync-control-plane.md`, the state wording in `spec/design/cli.md` if status text changes.
- **Live proof**: none on this host (it cannot run shortcut live tests); the fake-Graph multisync tests carry it. The PR says so.
- **Order**: right after B2 (it builds on `Report.LocalTreeHolds` and the final-drain classifier), before W3 touches the shortcut e2e waits. Cross lane (sync and multisync). Size L.

#### W31. A request the transport timed out is retried, not read as shutdown

- **Source**: the hand-back of W24 (2026-11-13), which noted that a token refresh ended by the sign-in client's 60 s timeout reaches sync as a `context.DeadlineExceeded`. My review confirmed it and found it is wider than the refresh. A probe on this host (a real `http.Client` against a stalled `httptest` server) shows that **every** transport timeout answers true to `errors.Is(err, context.DeadlineExceeded)`: the response-header timeout ("net/http: timeout awaiting response headers"), a dial timeout ("i/o timeout"), a client's whole-request `Timeout`, and a real context deadline. None answers true to `context.Canceled`.
- **Cause**: `classifyResult` (`internal/sync/engine_result_classify.go:60`) reads any error whose chain holds `context.Canceled` or `context.DeadlineExceeded` as shutdown (`resultShutdown`). Shutdown is neither retried nor recorded and drops the action's dependents, which is right only when the run is ending. So when the retrying transport has used its attempts on a request that timed out, the action is dropped while the engine runs on: in watch mode the file waits for the next change or scan that plans it again, and a one-shot run can end without it. The branch above line 60, for a transfer the stall watchdog abandoned, names exactly this failure for one error; the rule it works around is the fault. Eight more sites in product code read the same two errors as "the run is ending": `observer_remote.go:665` and `:1010` (a retry loop gives up at once on a timed-out page), `engine_watch.go:141`, `engine_watch_loop.go:508`, `engine_runtime_shutdown.go:58`, `engine_mount_root.go:437`, `observer_local.go:1076` and `internal/multisync/engine_close.go:52`.
- **Decision (mine, 2026-11-13)**: shutdown is a fact about the run, never about the shape of an error.
  1. One helper in `internal/sync` answers "did this fail because the run is ending": it takes the context the work ran under and the error, and says yes only when that context has ended. Every site that asks the question uses it; no site reads the two errors directly for that purpose.
  2. A worker records on its completion whether its own run context had ended when the action returned, and the classifier reads that field. With a live run context, a deadline-shaped error is a request timeout: it requeues as retry work with `issueRequestTimeout`, exactly as the stalled-transfer branch does, and that branch then becomes one case of the rule instead of an exception placed before it.
  3. `context.Canceled` with a live run context: the writer lists every place that cancels a context narrower than the run (a per-action or per-transfer context, a superseded plan, a pause) with its cause, and each gets its own class by `context.Cause`. A cancel nobody can name is requeued and logged at Warn; it is never dropped.
  4. Each of the nine sites is judged in the report in one table: which context is in play, what a transport timeout does there today, what it does after. The two observer retry loops retry a timed-out page like any other network error until the policy's attempts are used. A mount-root check that timed out is not "the run is ending".
  5. A source-reading test holds the rule: it lists every `errors.Is` against the two context errors in `internal/sync` and `internal/multisync` product files, and each must be the helper or a named exception with its reason, in the manner of the detached-context and exhaustive-switch tests.
  6. One-shot runs: an action that timed out counts as left work under the exit rule (will retry), so the run exits 1 and says so. That is checked by a test, not assumed.
- **Tests first**: the error comes from a real `http.Client` against a stalled `httptest` server, never a hand-built error, one case each for the response-header timeout, the dial timeout and a client `Timeout`: (a) the classifier requeues each as a request timeout while the run context is live, and classifies each as shutdown once it has ended; (b) a watch-mode engine test where one upload's request times out and the action is retried and its dependent still runs; (c) a one-shot test with the same fault that reports the action as will-retry and leaves work; (d) the remote observer retries a timed-out delta page up to its policy; (e) the source-reading test, failing on today's nine sites.
- **Docs**: the sync design document that governs `engine_result_classify.go` (result classes, and the stalled-transfer paragraph that becomes a case of the rule), the observer document for the two retry loops, and a Network Resilience requirement in `spec/requirements/non-functional.md` if none says "a request that timed out is retried while the run continues".
- **Live proof**: none. It needs a request that times out, and this host takes no network fault injection.
- **Order**: after B2, W30 and W23 are stacked (the classifier, `engine_run_once` and the exit rule's left-work counts; W23's absence tests), on the stack tip; before W8, which reshapes the engine entry points and should build on the one helper, and before B10. Sync lane (one file in multisync). Size M.

#### W32. A shortcut child's trash is reachable, purged after retention, and says why a released file is there

- **Source**: three "Not fixed" entries of W30's hand-back (2026-11-13), each checked against main `27e76cc3` in review. They existed before W30; W30 makes them matter, because a released shared folder's synced files now go to the child's trash instead of being removed outright.
- **Cause**:
  1. `internal/cli/trash.go` `openDriveTrash` resolves `--drive` with `resolveExactDriveSelector` and refuses a selector that is not in the config, then opens `localtrash.DirForStatePath(config.DriveStatePath(cid))`. A shortcut child is a mount, not a configured drive, so no `trash list`, `restore` or `empty` can open the trash its deletes (and its release) fill.
  2. `internal/multisync/shortcut_child_artifacts.go` `purge` removes the child's state database family, its catalog record and its upload sessions, and leaves the trash directory beside the state path. The only caller of `Trash.Purge` is an engine's housekeeping (`internal/sync/engine_housekeeping.go`), and no engine runs for a purged child, so those entries outlive `localtrash.Retention` for good and nothing lists them.
  3. `internal/localtrash` has two reasons, `deleted` and `replaced`. The release trashes with `deleted`, which `trash list` prints as deleted on OneDrive. The file was not deleted on OneDrive: the shared folder was removed from this account and the sharer's copy still exists.
- **Decision (mine, 2026-11-13)**: one rule: every trash sync writes stays reachable and bounded for as long as it holds an entry, whether or not the mount that wrote it still exists, and an entry says why its file left the sync folder.
  1. A shortcut child's trash belongs to its parent drive. `trash list --drive <parent>`, `restore` and `empty` cover the parent's own trash and the trash of every child mount of that drive; each entry already carries its sync root, which tells them apart. No new selector for a child.
  2. The child artifact purge hands the child's trash over to the parent's trash before it removes the child's state: each entry keeps its id, time, reason, state and original location. After that the parent engine's housekeeping expires the entries after the retention like its own. The purge never deletes a trash entry and never leaves a trash directory that no command and no engine reaches. A hand-over that fails is a cleanup failure of the existing kind (the parent publishes the cleanup again), not a silent skip.
  3. The hand-over is crash-safe by the trash's own protocol: at every point an entry is in exactly one trash, or in both under the same id with the next attempt finishing the move; never in neither. A pending entry and an orphan are carried over as what they are.
  4. A third reason, `released`: the release's delete trashes with it, `trash list` says the shared folder was removed from this account, and `--json` carries `released`. How `trash list` words a reason is an exhaustive switch over the declared reasons, held by a test that reads the member list from the source; an entry with a reason outside the set is still listed, with the stored word.
  5. `trash restore` of an entry whose sync root no longer exists (the released folder) says so and names where the file is in the trash, as it does today for a missing parent directory; it does not recreate a removed shared folder's root. Stop and tell me if today's restore already creates missing parents, so that the rule for a removed root can be decided on the code as it is.
  6. Stop if the hand-over needs a change to the entry record's format: that is a compatibility decision.
- **Tests first**: (a) `trash list --drive <parent>` shows an entry a child of that drive trashed, text and JSON; `restore` puts it back; `empty` empties both trashes; (b) a child artifact purge with committed, pending and orphan entries: afterwards every entry is in the parent's trash with its id, time, reason and state, the child's trash directory is gone, and the parent's purge removes the committed ones once the clock passes the retention; (c) the purge stopped at each step of the hand-over (the trash's crash points or an injected failure) and run again: each entry exactly once at the end; (d) a hand-over that fails reports the cleanup failure and leaves the child's trash as it was; (e) a released file's entry has reason `released` and its list line; (f) the source-reading test of the reason switch; (g) `restore` into a sync root that is gone.
- **Docs**: `spec/design/sync-execution.md` (the local trash: reasons, whose trash a child's is, the hand-over), `spec/design/sync-engine.md` (the child artifact purge and the release), `spec/design/cli.md` (`trash`), the requirement the trash cites, and `docs/` where `trash` is described to users.
- **Live proof**: none on this host: shortcut live tests cannot run here. The PR says so and names the tests that stand in.
- **Order**: after W30 is stacked (the release's delete and its trash call), on the stack tip; W29 takes the principle label in `internal/cli/trash.go` separately. Cross lane (cli, multisync, localtrash, sync). Size M.

#### W33. An upload sync gives up drops its session record and cancels the server session

- **Source**: W23's hand-back (2026-11-13), deviation 1: with large writes in the chaos model, 4 of 24 seeds at two batches and seeds 2 and 6 at six batches fail the invariant "no upload session outlives its upload". The reviewer of W23 traced it to the product (`scratchpad/R-W23-findings.md`, section C); the cited lines are the reviewer's reading and the writer's first step is the failing test below, which proves or disproves it.
- **Cause**: an interrupted large upload keeps its persisted session record so that the next attempt can resume (`internal/driveops/transfer_session.go:326`, also `:307-310` and `:346-349`). When sync then stops wanting that upload (a conflict takes the path, the local file moves, the local file is deleted) nothing touches the record again: `CleanStale` removes it after 7 days and the server session is never canceled, although R-5.6.13 says every dropped record cancels its server session. `spec/design/system.md:320` documents the defect as a limit of the chaos model.
- **Decision (mine, 2026-11-13)**: one rule: when sync gives up an upload, for any reason, its session record is dropped and its server session canceled, in the same step in which the runtime ends the upload's retry work. Not the smallest fix (drop in `ExecuteConflictCopy` after the rename, `executor_conflict.go:103-116`): that covers the conflict and leaves the move and the delete.
  1. The place is where the runtime prunes the upload's retry row (the caller of `SyncStore.PruneRetryWorkToCurrentActions`, `internal/sync/store_retry_work.go:417`): a pruned upload row means sync no longer wants that upload.
  2. `internal/driveops` gets one entry point for it (for example `TransferManager.DropUploadSessions(ctx, driveID, absPath, itemID)`) that runs the existing `dropSession` (`transfer_session.go:491`) for both keys an upload can hold: the create key (the absolute path, `transfer_upload.go:159`) and the overwrite key (`item::<itemID>::<abs path>`, `:429`). `dropSession` already cancels best-effort on a detached context, which is what R-5.6.13 asks.
  3. An upload that is still wanted keeps its record: a retry of the same upload must still resume. The test for that exists (`TestChaosGraph_FragmentFaultLeavesAPartUploadedSessionToResume`) and must stay green unchanged.
  4. Stop and tell me if the prune cannot tell an upload row from other retry work without a schema change, or if the drop would have to run inside the store transaction (a Graph call under a store lock is not acceptable).
- **Tests first**: `TestRunOnce_ConflictOverAnInterruptedLargeUploadDropsItsSession` in `runOnceScenarios()` (`// Validates: R-5.6.13, R-5.2.4`; engine from `newScenarioSessionEngine` as `pending_creates_test.go:526` does): sync `big.bin`; write local content A of `scenarioLargeFileSize`; set `remote.interruptFragmentAt = scenarioSessionFragmentSize` and run once, expecting one error, one file under `<dataDir>/upload-sessions` and `remote.holdsSession(url)` true; edit `big.bin` remotely to B, `expireRetryWork`, `runScenarioPass` until quiet; then `upload-sessions` is empty, `remote.holdsSession(url)` is false, the conflict copy holds A on both sides and `big.bin` holds B. It fails on the tip before the fix. The same shape for a new file (create key), for a local move of the file after the interruption, and for a local delete. A driveops test that the entry point drops both keys and cancels each session once, and that a failed cancel still drops the record.
- **Docs**: R-5.6.13 in `spec/requirements/transfers.md` names the new trigger (the upload was given up); `spec/design/drive-transfers.md` (session records: who ends them); `spec/design/system.md:320` loses the limit; GOVERNS entries for any new file.
- **Rider (from the same review)**: `internal/driveops/transfer_session.go:402` reads the wall clock for a session's expiry, which no test can move. Not a fault. If the file is open anyway, the expiry check takes the manager's clock; otherwise it stays.
- **After it**: W23's second part lands in the same PR or directly above it: the chaos model's large writes (`scratchpad/W23-model-large-write.patch`, kept until then) and the seed assertion, with every seed W23's report names green.
- **Live proof**: one live run against the test account when the service answers: interrupt a large upload (kill the client mid-fragment), change the file online, sync until quiet, and read `upload-sessions` and the conflict copy. Otherwise the PR goes under "Owed a live rerun".
- **Order**: after W23's first part and W5 are stacked (both touch the scenario fakes and `transfer_session.go`'s tests), on the stack tip. Cross lane (driveops, sync). Size M.

#### W34. `status --perf` counts a committed transaction once

- **Source**: the reviewer of W5 (2026-11-13), verdict on W5's "Not fixed" entry for `internal/sync/tx.go:57`. The reviewer's reading; the writer's first step is the failing test.
- **Cause**: the transaction helper defers `finalizeTxRollback`, which calls `Rollback` after a successful `Commit`; `Rollback` then returns `sql.ErrTxDone` and `recordPerfTx` runs a second time. Every committed transaction is counted twice in `status --perf` (#862 added the count).
- **Decision (mine, 2026-11-13)**: a transaction is recorded exactly once, by what ended it: the commit, or a rollback that returned nil. A rollback that returns `sql.ErrTxDone` ended nothing and records nothing. If the helper has more than one exit that records, they become one place.
- **Tests first**: a store test that runs one committed transaction and one rolled-back transaction through the helper and reads the perf counters: one commit, one rollback, two transactions in all. It fails on the tip (three). A sweep of every other caller of `recordPerfTx` for the same double count, each fixed or shown single with a test.
- **Docs**: the design doc that describes `status --perf` (the transaction count means ended transactions); the CLI doc only if its wording said otherwise.
- **Live proof**: none needed: the count is local. `status --perf` on the test mount before and after, in the PR, when the watch client runs the new build.
- **Order**: independent; on the stack tip. Sync lane. Size S.

#### W35. Multisync and cli tests prove absence by a signal, and long event waits have one name

- **Source**: the reviewer of W23 (2026-11-13), verdicts on W23's "Seen and not fixed" 1 to 4, and W23's own second pass, which converts the sites that need no product seam and leaves these.
- **Cause**: W23 made the rule (`spec/design/system.md:289`: a test proves that something did not happen by a signal, not by a time window) and applied it to `internal/sync`. Two multisync sites cannot follow it without a seam the product does not have: `watch_runner_stop_test.go:734` (it must know that the drain started waiting on the runners; `retireAllWatchRunners` gives no signal) and `watch_config_file_test.go:25` (used at `:47`, `:51`, `:61`; `watchConfigReloadTriggers` owns its ticker and reports no finished poll). Separately, positive waits of 5 s in multisync tests and of 2 s in `internal/cli/signal_test.go:43`, `:184`, `:364` fail under load on two cores for no reason but the number.
- **Decision (mine, 2026-11-13)**: (1) the two seams, as small as they can be: a "waiting on the runners" hook on the drain that the test reads, and an injected ticker (or a poll-done signal) for the config watch; both follow the pattern W23 used in `internal/sync` and neither changes behavior. (2) Every positive wait for an event in multisync and cli tests uses one named long wait (the one W9 and W23 introduced, or its twin in the package's test helpers), so a slow host waits longer and a fast one does not. (3) After this PR the sentence in `system.md` holds for the whole tree, and the check W23 added (if it scans by package) covers multisync and cli. Stop if a seam would have to change how the drain or the reload behaves.
- **Tests first**: the two tests rewritten on their signals, each shown to fail when the product is broken the way the old window was meant to catch (the drain returns before the runners stop; a reload fires without a config change); the remaining window sites listed with zero left.
- **Docs**: `spec/design/system.md` (the absence rule's scope), `spec/design/sync-control-plane.md` where the drain and the config watch are described, if the hooks are named there.
- **Live proof**: none: test code and two test seams.
- **Order**: after W23 is stacked (the rule, the helper names) and after B9 (which touched the watch runner stop); on the stack tip. Cross lane (multisync, cli). Size M.

#### W36. Two local moves that swap through a recreated folder plan without a dependency cycle

- **Source**: W23's hand-back (2026-11-13), "Seen and not fixed" 6, verbatim: "dependency cycle seen with an intermediate variant of this change (chaos seed 3, two batches): local "move docs -> photos/photos, recreate docs" then "move photos/photos/50%.txt -> docs/renamed-50%.txt"; folder_create docs and two remote_moves were dropped as a cycle on every pass, never converging. Not reproducible by seed in the final tree."
- **Cause**: unknown. The planner logs and drops a cycle at `internal/sync/planner.go:294`; if the fault is real it is in `buildDependencies`. It was seen once, in a tree that no longer exists.
- **Decision (mine, 2026-11-13)**: the two operations become a scenario test first. If the test passes on the tip, it is kept as a regression test, the PR says the cycle was not reproduced, and nothing in the planner changes. If it fails, the planner orders these actions (a folder create at a path a move vacated depends on that move; a move into the recreated folder depends on the create) so that no cycle forms, and the general rule is stated in the design doc; dropping a cycle stays as the last resort it is, and a dropped cycle becomes left work the run reports (check against B2's left-work rule) rather than a log line only. Stop if the fix needs a planner decision-table row: that is #865's table and I decide the row.
- **Tests first**: `runOnceScenarios()` case: synced `docs/` with files and `photos/`; locally move `docs` to `photos/photos`, recreate `docs`, run once; locally move `photos/photos/50%.txt` to `docs/renamed-50%.txt`, run until quiet; both sides hold the same tree and no pass logs a cycle. A planner unit test of the same action set that asserts an order exists. If W37 has landed, a replay of chaos seed 3 at two batches as well.
- **Docs**: `spec/design/sync-planning.md` (dependencies between a move and a create at the vacated path), only if the planner changes.
- **Live proof**: only if the planner changes: the same two operations under `~/sync-test-root` against the test account.
- **Order**: after W23 is stacked; W37 first if both are open, since a replayable seed is the cheapest reproduction. Sync lane. Size S, or M if the planner changes.

#### W37. A chaos run is replayable from its seed

- **Source**: W23's hand-back (2026-11-13), "Seen and not fixed" 7, and the reviewer's verdict on it.
- **Cause**: not located by a run. By the reviewer's reading, a chaos draw depends on the seed, the operation and key, the n-th call on that key and the storm flag at call time (`internal/sync/chaos_remote_test.go:146-160`); the n-th call and the moment a concurrent write lands depend on how the workers interleave, so a seed does not fix a run, and the race seed's fault assertion (`:675-677`) depends on it. The writer's first step is the test that shows it: the same seed twice, two fault logs compared. Whether W36's cycle failed to replay for this reason or because the tree had changed is unknown.
- **Decision (mine, 2026-11-13)**: a chaos run is a function of its seed and of what the client did, never of scheduling. Each draw is derived from the seed and from the request's own identity (operation, key, and the count of that same request, which the client decides), with no state shared between keys and none that depends on arrival order; the storm window is counted in the model's own steps, not in wall time or arrival order. A failing seed prints one line that reruns it (the test name with the seed and the batch count, in whatever form the chaos tests already take them). Where a draw cannot be made independent of scheduling, the run records its draws and a failing run can be replayed from the record; the test says which draws those are and why. If the first-step test shows a seed already fixes a run, the PR is that test and the rerun line. Test code only; no product change.
- **Tests first**: the same seed run twice in one process gives the same fault log, with and without `-race` and with `GOMAXPROCS=1` and `2`; the seed assertion at `:675-677` then rests on that; the rerun line is asserted by a test that parses it.
- **Docs**: `spec/design/system.md` (the chaos model: replay), the test file's header comment.
- **Live proof**: none: test code.
- **Order**: after W23's first part is stacked (it owns the chaos model's files); before W36 if both are open. Sync lane. Size S.

#### W38. A shared folder that is gone or no longer shared says so, and a run counts shared folders as shared folders

- **Source**: the e2e job of #898 (2026-11-13): in the CI account one shared-folder shortcut's target answered HTTP 404 to the folder delta and to the children listing, and every one-shot sync then exited 1 with "1 of 3 selected drives left files unsynced (see above)" for a configuration that selects one drive.
- **Read in the code (mine, 2026-11-13)**: `executeMountRootObservation` records a root `remote_read_denied` issue for 403 and for 404 alike (`isObservationRemoteReadDenied`); `status` describes it as "Remote read blocked", "This remote content can no longer be downloaded with your current permissions.", "Restore access to the shared item, or remove the blocked content from this sync scope." (`internal/cli/status_condition_descriptors.go`, the `ConditionRemoteReadDenied` case). `runOnceLeftWorkError` (`internal/cli/sync_render.go`) and the drives error beside it count `result.Startup.SelectedCount()`, which counted the two shared folders of that account as selected drives. Not checked: whether `SelectedCount` counts shortcut children by design or by accident, and what `status` prints as the heading of such a child.
- **Decision (mine, 2026-11-13)**: the exit rule stays: a shared folder that cannot be read keeps files unsynced and fails a one-shot run until the user deals with it. What changes is what the user is told. (1) At a shared folder's root, "gone" (404) and "not allowed" (403) are told apart in the issue's text, with one next step each: for 404, that the folder was deleted or is no longer shared and that removing the shortcut from OneDrive ends the report; for 403, to ask the owner for access or remove the shortcut. One issue type stays (no store generation bump) unless the writer shows the text cannot be chosen from what the store already holds; if it cannot, stop and report. (2) The exit error and the summary count what the user selected: a drive and its shared folders are named as what they are ("1 of 3 selected drives" for one drive with two shared folders is wrong); the writer reads `SelectedCount` and its callers first and reports which wording each caller needs.
- **Tests first**: a root observation that answers 404 and one that answers 403 give the two texts (status text and JSON); a one-drive run with two shortcut children, one unreadable, prints an exit error that does not call the children drives. Each seen failing first.
- **Docs**: `docs/scripting.md` (exit status examples), the status section of the user docs, `spec/design/sync-engine.md` where the root observation is described; the requirement is cited, never invented.
- **Live proof**: none possible locally (the test account has no shared folders); the CI account's fixture is the proof if it still answers 404.
- **Order**: after B2 (#898) merges; CLI lane with a small sync part. Size S.

#### W39. A shortcut the user's filters exclude leaves no folder and no work behind

- **Source**: a code search made while #898's e2e failure was read (2026-11-13). Model output, not yet proven by a run: every line below is a claim for the writer's first test.
- **Claims to prove or refute first**: (a) a shortcut whose path the parent's filters exclude gets no child run (`shortcut_root_publication.go`, `projectShortcutChildContentFilter` returns not visible, read by me), but its record is still written and `reconcileShortcutRootLocalState` still reaches `materializeShortcutRoot`, which creates its local folder (`shortcut_root_lifecycle.go`, read by me: no filter check on that path in the excerpt); (b) `SyncStore.ShortcutChildWorkSnapshot` passes an empty filter (`shortcut_root_store.go:87`), read by `acknowledgeChildArtifactsPurged` and `multisync/shortcut_child_artifacts.go`, so a count it gives can include work the user's filters exclude.
- **Decision (mine, 2026-11-13)**: a path the user's filters exclude is not touched: no folder is created for an excluded shortcut, an existing folder there is left alone, and no count or issue reports work under it. If the first tests show both claims false, the PR is the two tests.
- **Tests first**: an engine test with `ignored_dirs` (and one with `ignored_paths`) naming a shortcut's path: after a pass the path does not exist locally and no child run command is published; a test of the snapshot with a filter that excludes part of a child's tree.
- **Docs**: `spec/design/sync-engine.md` (shortcut roots and the content filter).
- **Live proof**: none: no shared folder in the test account.
- **Order**: after W30 and W32 are stacked (they own the shortcut release files). Sync lane. Size S.

#### W40. A sync whose output cannot be written says so and exits non-zero, in text and in JSON

- **Source**: the review of the JSON-encoder change (H6, 2026-11-13): `internal/cli/sync_console.go` (`write`, about line 577) drops a failed write to stdout through an `if` that does nothing; in JSON mode the first failed write is reported once on stderr and the exit status stays 0, while every document command (`ls --json` and the like) fails on the same error. Two policies in one command, and neither is written down.
- **Not checked (the writer's first step)**: whether the process handles `SIGPIPE`. Go ends a program with that signal on a write to a closed pipe on stdout unless the program asked to be notified of it, so a closed pipe may never reach this code and the cases that do are a full disk or an I/O error on a redirected stdout. Say which in the report, from the code and from one run with stdout redirected to `/dev/full`.
- **Decision (mine, 2026-11-13)**: one policy for both modes, one-shot and watch. The sync finishes its work (a lost report is no reason to stop syncing, and a watch keeps running). The first failed write is reported once on stderr. When the command ends with no other error, it returns the write error, so the exit status is 1: a script that reads the output cannot take a cut-off report for a whole one. When the run also left work or failed, that error is the one returned and the write failure stays the stderr line.
- **Tests first**: a console with a failing writer in text mode and in JSON mode: one stderr line each, the run's work unaffected, and the command's error is the write error when nothing else failed; a one-shot run through the command with stdout on `/dev/full` (if a test of that shape exists for a document command, the same shape). Each seen failing first.
- **Docs**: `docs/scripting.md` (exit status and output), `spec/design/cli.md` (the sync console), the requirement that states exit statuses; cited, never invented.
- **Live proof**: one run of `onedrive-go sync` against the test account with stdout redirected to `/dev/full`, when OneDrive answers.
- **Order**: after B2 (#898) and H6 merge (both change the files this touches). CLI lane. Size S.

#### W41. A run reports a condition the way `status` does: the four cosmetic leftovers

- **Source**: this repo's `BACKLOG.md`, "onedrive-go: cosmetic leftovers from the edge-case round (2026-11-04)", and `CHANGELOG.md` 2026-11-04T11:00 (each seen live on the test account that day). Split from H8 on 2026-11-13: these four are in `internal/sync` and the sync console, H8 keeps the outer packages.
- **The four**: (1) a retried condition the user must clear (a path too long, R-2.10.71) prints "Failed to create folder …: graph: bad request" and counts as "1 failed" on every retry of the durable curve, although `status` already lists it under its condition; (2) the one-shot summary counts folders as "files" in "deleted N files locally"; (3) the condition of a crossed mount that has vanished shares the plain mount boundary's `status` descriptor, so the text suggests setting `cross_filesystems = true` when it is set (the sync warning itself is right); (4) a watch logs "sync conditions changed" at debug every 10 s while nothing changed.
- **Decision (mine, 2026-11-13)**: (1) a retry whose failure is already recorded as a condition the user must clear is reported by the run as that condition, in the words `status` uses, once per run, and is counted with what waits for the user, not with what failed; the exit status of such a run does not change. (2) A count names what it counts: files and folders separately where the summary already does so anywhere, otherwise "items". (3) The vanished crossed mount gets its own issue type with its own descriptor (what happened, that the content is kept and nothing is deleted on OneDrive, what clears it); the declared-set tests and the exhaustive switches hold the new member; a row an older client stored under the shared type is re-recorded by the next full scan. A new issue type is no store-generation bump unless the code says it is: if it is, stop and report. (4) The watch logs a change of conditions only when the set changed.
- **Tests first**: one per item, each seen failing: the run's output and counts for a retried recorded condition; the summary's wording for a deleted folder beside a deleted file; the descriptor of the new issue type and the scan that records and clears it; two watch ticks with the same conditions log one line.
- **Docs**: `spec/design/cli.md` (the sync console and the status descriptors), `spec/design/sync-engine.md` where issue types are listed; requirements cited, never invented.
- **Live proof**: (1) and (2) on the test account when OneDrive answers (a path over the limit under `~/sync-test-root`, synced twice; a folder deleted on OneDrive). None for (3) and (4): a mount inside the test folder is a change to this host, and the unit tests hold them.
- **Order**: after W38 merges (both change status descriptors and what a run's summary counts). Sync lane. Size M. When it merges, the `BACKLOG.md` entry moves to "Completed".
- **Rider (2026-11-16)**: item (2) of this row (the summary words a deleted folder "file") is row W91's, which was written the same day from a live run before I saw the overlap. W41 keeps items (1), (3) and (4) and their tests; whichever of the two rows merges second takes the `BACKLOG.md` entry to "Completed".

#### A17. A failed verify step prints one marker line, and the nightly keys the step

- **Source**: the review of A10 (2026-11-14), its finding on failures outside `go test`. The nightly check names a failing test from the `go test` output of a failed job, and since A10 it names the job when a package failed without naming a test. A job that fails outside `go test` (the coverage gate, lint, `govulncheck`, the format check, a build of the tool itself) leaves nothing a reader can key but the job, and one job runs several such steps.
- **Change**: every step a `devtool verify` profile runs is started through one runner (it already is; check). When a step fails, that runner prints one line in a fixed form on standard error after the step's own output: a fixed prefix, the profile, the step's stable name (a constant declared with the step, not its command line). The nightly reader recognises the line in a failed job's log and keys the failure `step-<job>-<step>`; the report and the issue name the step. The line holds no path, no command line and no output of the step.
- **Decision (mine, 2026-11-14)**: the marker is printed for every failed step, `go test` steps included (the reader prefers a test key when the log names one and keeps the step key only when none does). Step names are a closed set held by a test that reads the profiles, so a new step without a name fails the test. The claim-ref encoding of A10 carries the new key unchanged.
- **Tests first**: a failed step prints exactly one marker line and a passing one none; the reader keys a failed job whose log holds a marker and no failed test; a log with both a failed test and a marker yields the test key only; every step of every profile has a distinct name.
- **Docs**: the requirement A10 added for the nightly verdict (amend it: a failure outside the tests is keyed by its step) and `spec/design/system.md` where the nightly check is described; requirements cited, never invented.
- **Live proof**: none on the host. On GitHub: the next red nightly whose failure is outside `go test` shows the step in the issue; not forced.
- **Order**: after A10 and A11 merge (A11 changes which steps the jobs run). CI lane, light on the host. Size S.

- **Rider (2026-11-14, from A10's first pre-PR gate)**: the spec-governs step's routing-table check (`internal/devtool/verify_governs.go`, `routingPathPattern` and `findDeadRoutingTablePaths`) runs over the whole of the repository guide, so any backticked text in that file that ends in a slash, or in `.go` or `.md`, must match a file wherever it stands; a git ref prefix written in prose failed a gate with a message that names no line. Tests first: either a path-like text outside the routing table is not checked, or the failure names the line and says what the check wants; the DoR decides which. No rule is loosened for references inside the table.
- **Rider (2026-11-15, from the first nightly that posted its verdict)**: the `nightly-failure` issue said of each red job "failed; its log could not be read". Why the report job could not read the logs is not checked: find it first (a missing permission, a log not yet available when the job runs, or the reader's own error path) and hold the fix with a test. Second, the reader that keys failing tests takes a test that was merely running when a `go test` alarm fired as failed: the stress lane's timeout produced two such keys. Key only the test the alarm names as the one that hung, or key the alarm itself, and say which in the issue.

#### A18. The short tier's go test deadline scales with the cores

- **Source**: W11's pre-PR gate on 2026-11-14 at about 02:00. `go test -short ./internal/sync/` reached go test's fixed 10-minute package deadline with no test hung. Measured afterwards: the package took 120, 209, 256 and 306 s inside the last four passing gates; alone behind the heavy-run lock 269 s on main and 476 s on the W11 branch at different host loads; the four slowest tests back to back took 48 s on main and 38 s on the branch. The branch was not the cause. The same tests vary about 2.5 times with what else runs on this 2-core host, and the fixed limit sits inside that spread. The same gate, unchanged, passed at 03:14 with the tier at 9 min 39 s.
- **Change**: the short tier's deadline follows the rule the race tier has had since #863: 10 minutes on a host with at least the 3 cores of the CI runners, and 60 core-minutes divided by the cores below that (30 minutes on 2 cores, 60 on one). `--short-unit-timeout` (`VerifyOptions.ShortUnitTimeout`) overrides it. Both tiers call one core-scaling function. The test that holds every CI job's timeout above its go test deadlines passes both tiers' CI-runner deadlines explicitly, so it reads the same on any host. CI's own deadlines and job timeouts do not change.
- **Decision (mine, 2026-11-14)**: 60 core-minutes, three times the run that failed, not a number derived from the race tier's 180. A flag, not an environment variable, to match `--unit-timeout`.
- **Tests first**: the deadline per core count; the explicit value wins; with no option the three profiles that run the short tier pass the deadline of the host's cores; the flag reaches the options.
- **Docs**: `spec/design/system.md` (verify profiles), the repository guide, R-6.10.22.
- **Live proof**: none needed; its own pre-PR gate runs with the new deadline.
- **Order**: now, ahead of the rest of the merge pipeline; I wrote it myself. A10's branch edits `cmd/devtool/main.go` and the same docs and is rebased after this merges. Size S.

#### W42. No managed folder lies inside a sync folder, and the local move does not replace

- **Source**: the review of H9 on 2026-11-14 (`reports/H9-report.md`, "Not fixed" entries 1, 2 and 4, and the reviewer's notes 11 and 13). All five lie in files H9 does not touch, or need a decision H9 was not given, so they are a row of their own and not a rider on W32 (the shortcut child's trash).
- **Change**:
  1. `internal/config/validate_drive.go:194` `checkSyncDirOverlap` also refuses a `sync_dir` equal to or above the data, config and state directories. The trash and the detach destination then never lie inside a sync folder. H9 makes this more pressing: `MoveInNoReplace` refuses a source inside the root, so a trash inside a sync folder can no longer be restored from.
  2. `internal/synctree/detach_unix.go:103` and `:153` refuse a destination that `Rel` places inside the root.
  3. `internal/sync/executor.go:599-601`: the local move uses `RenameNoReplace` for files everywhere and for folders where the platform has the flag. A rename that changes only the spelling keeps the plain or temporary-sibling path, because on a case-insensitive volume the no-replace call finds the source itself at the destination. The exception H9 records in R-6.4.8 is then removed.
  4. `detach_unix.go:211`: the parent is opened with `O_DIRECTORY|O_NONBLOCK`.
  5. `MoveInNoReplace` decides "outside the root" lexically (`Rel`). A trash path that reaches the sync folder through a symlink, or through another spelling on a case-insensitive volume, gets through. Decide by what the two paths resolve to (device and inode of the opened folders), not by their text.
- **Tests first**: one per item, each failing before its fix; for item 3 a destination that appears between the plan and the move is kept and the move is reported, for a file and for a folder; for item 5 a trash reached through a symlink into the sync folder.
- **DoR questions**: what does the executor do with a local move that finds its destination taken (today it replaces): is the action retried after a new observation, or failed? Does any test or the planner rely on a local move replacing a file the planner knows about (a move onto a path whose delete is planned in the same pass)?
- **Docs**: R-6.4.8, `spec/design/system.md` (rooted mutation), `spec/design/sync-execution.md` (local move), `docs/` where the config's `sync_dir` rules are listed.
- **Live proof**: none for items 1, 2, 4 and 5 (local only). Item 3: one local rename synced on the test account, to show the ordinary move still works.
- **Order**: after H9 merges. Size M.

#### W43. Every e2e read of a rewritten file waits for its content, and no poll predicate asserts

- **Source**: the review of W3 on 2026-11-14 (`reports/W3-report.md`, "Seen and not fixed" entries 3, 7 and 9, and the reviewer's notes 12 and 15). All lie in e2e files W3 does not touch, or are suite-wide, so they are a row of their own.
- **Change**:
  1. Every caller of `getRemoteFile` that reads a file that existed before (an edit or an overwrite, whatever wrote it) moves to `waitForRemoteFileContent`. A read of a new file that an exited `put` created stays. The rule is the one W3 wrote into `spec/reference/live-recurring-families.md` and `spec/design/system.md`.
  2. The poll predicates in the e2e files W3 does not touch are audited for an asserting call inside the predicate (a predicate reports, the wait fails). The first pass's report lists seven files a file-name-only grep suggested; whether they hold faults is unknown.
  3. The three reads after the daemon exited in `e2e/sync_watch_e2e_test.go` (near lines 121, 272 and 384) repeat a content wait for the same path and bytes. Drop the reads and their three exceptions in the scan, so the exception list holds only the shortcut waits.
  4. A source-reading test for item 2 if a rule can be stated without false findings (a call of a testify `require`/`assert` function, or of `t.Fatal`/`t.Error`, inside a function literal passed to one of the suite's poll primitives); if not, say why in the report.
  5. After the merge, read from the next nightly whether the bucket `full-serial-watch-shared` still fits its 40-minute go test deadline with the test W3 added (the CI-deadline test compares deadlines with job timeouts, not durations).
- **Tests first**: for item 4, the planted source of the scan's own test gains one asserting predicate and the test reports it.
- **Docs**: the Verification Policy sentence that names the scan, if item 4 adds a rule.
- **Live proof**: every test whose read or predicate changed, one at a time on the test account; the report ends with the run list.
- **Order**: after W3 merges (same files). Size M.
- **Riders (added 2026-11-14, from W3's second and third step reviews and its third pass; the brief takes each as an item or says why not). MOVED TO W47 on 2026-11-14: rider 4 whole; from rider 6 the whole-output `assert.Contains` calls, the raw stdout and stderr in failure messages and the debug log's `--json` records; from rider 7 the addresses and the socket.io messages. W43 keeps riders 1, 2, 3 and 5, from rider 6 the predicate tests' move and the named constant, from rider 7 the two tests that repeat one body and the check of the fixed timeout message. Rider 8 is mine, read from the nightlies after W3 merges. Decision (mine) on rider 3: the second reload goes, so the test proves the daemon's own expiry wake; if the file then does not upload live, that is a product fault with its own change**:
  1. The e2e suite's copy of the watch debounce is held by a comment only. An untagged source-reading test compares it with `defaultDebounce` in `internal/sync/engine_watch.go`.
  2. The timed-pause test does not see an upload that happens after its window and before the pause ends. After the file is on OneDrive, compare `stat --json` `created_at` with the pause's end: a creation time before the end fails the test.
  3. The timed-pause test reloads the daemon after the pause command, so the reload, not the daemon's own expiry wake, may be what resumes the drive. Either wait for the file without a reload in between, or say in the test which of the two it proves.
  4. What `Redact` leaves in failure messages and logs: Graph drive and item IDs in redacted `stat` stdout (`e2e/sync_full_test.go` near 841-851), in `ls --json` stdout (`e2e/shared_items_helpers_test.go` near 774 and 836), in `--debug` stderr and in status JSON `display_name`; one message near `e2e/sync_full_test.go:419` prints the sharer's address through `Redact` and relies on it. Seen live on 2026-11-14: the debug log's command line holds a shared-root `--drive` selector as given, while the same log shows a role name for the suite's own drive. **Decision (mine)**: drive and item IDs of the CI and test accounts count as names the suite must not print (R-6.10.27); `Redact` learns them from the fixtures it already holds, and the debug log's command line goes through it.
  5. `isRemoteNotFoundCleanup` scans the whole `--debug` stderr for "not found". It should match the CLI's final sentence ("was not found on OneDrive"), so a debug line that holds the words cannot make a failed stat read as missing.
  6. From the third pass's "seen and not fixed": whole-output `assert.Contains` calls; raw stdout and `--debug` stderr in failure messages in `e2e/e2e_test.go` (near 510, 516, 685, 720, 745, 798, 883); the debug log records `shared --json` and `drive list --json` output through `Redact` only; the pure predicate tests that still sit in the tagged `remote_read_helpers_test.go` move to an untagged file; `120*time.Second` repeated in `sync_full_test.go` becomes one named constant.
  7. From the second pass's "seen and not fixed" (`reports/W3-report.md`, the four entries under "Second pass (review)"): `assert.Equal` on addresses in the two preflight and caller-proof files; the socket.io helpers' failure messages that print whole debug events; the two file-discovery tests that repeat one body. The fourth entry (`pollRemoteEventually`'s timeout message) was fixed in W3's third pass; check it and drop it.
  8. Unknown and to be measured by the DoR: how often a shared-root listing lags twice in a row (W3 marks the second lag as a failed read; if it is common, the recovery needs a second round instead).

#### A19. The linter's deadline scales with the cores

- **Source**: W23's first pre-PR gate on 2026-11-14 at about 05:20. `golangci-lint run` ended with exit status 4 and "Timeout exceeded" after the 15 minutes `.golangci.yml` fixes, while three writers were building on this 2-core host. Nothing in the branch was the cause as far as the log shows: the linter reported no finding, only the timeout. It is the smell #902 fixed for the short tier's go test deadline, one step earlier in the same gate.
- **Change**: devtool passes `--timeout` to golangci-lint: 15 minutes on a host with at least the 3 cores of the CI runners (the value the configuration file holds, so CI does not change), and a core-time budget divided by the cores below that. The deadline comes from the same core-scaling function the two go test tiers use. `.golangci.yml` keeps its 15 minutes for a linter started by hand.
- **Decision (mine, 2026-11-14)**: 90 core-minutes, three times the run that failed, as with #902: 45 minutes on 2 cores, 90 on one. No flag: nobody has needed to override the linter's deadline, and the configuration file still serves a manual run.
- **Tests first**: the deadline per core count; every profile that lints passes the deadline of the host's cores on the linter's command line.
- **Docs**: `spec/design/system.md` (verify profiles), the repository guide, the requirement that names the short tier's deadline if it also names the linter.
- **Live proof**: none needed; its own pre-PR gate runs with the new deadline.
- **Order**: now; I write it myself. It goes ahead of W23 only if W23's second gate fails on the same timeout. Size S.
- **As merged (#904, `7d7bc06b`)**: beyond the row, the CI lint job's `timeout-minutes` went from 15 to 20 with a test that holds it above the linter deadline plus a 5-minute setup allowance; a test holds `.golangci.yml`'s `run.timeout` equal to devtool's base deadline; and devtool's gh client repeats gh's own "error connecting to <host>" message, the one that failed #903's post-merge stage after its merge.

#### W44. An entry left under the temporary spelling name is finished or made visible at the next start

- **Source**: the second review of H9 on 2026-11-14 (`reports/H9-report.md`, and the reviewer's findings on the put-back of a spelling-only rename). H9 itself only makes the failure say where the entry is; what happens afterwards is this row.
- **The fault**: a spelling-only rename (`a.txt` to `A.txt` on a filesystem that folds case) goes through a temporary sibling `.onedrive-go.spelling.<name>`. A crash between its two renames, or a put-back that fails, leaves the entry under that name. Observation always excludes the name (`content_filter.go:139`) and cleanup never deletes it (`spec/design/sync-execution.md:256-258`), so nothing the client does afterwards sees the entry.
- **Unknown, and the DoR's first job**: what the planner does at the next run. Either the path's absence is read as a local delete, which deletes the remote item while the only local copy sits hidden; or it is read as a missing file to download again, which leaves a hidden duplicate for ever. Prove which with a test that plants the leftover and runs a plan; do not reason it out.
- **Change** (to be fixed after the DoR, in this order of preference): at engine start and before the first plan, a leftover under the temporary spelling name whose plain name is free is moved back to its plain name through the rooted no-replace rename (the rename is then planned again from truth); a leftover whose plain name is taken is reported as an issue that names both paths and is never deleted. No leftover is ever read as a delete of the path it came from.
- **Tests first**: the planted-leftover test above, seen failing; a leftover whose plain name is taken; a leftover in a folder the content filter excludes; a start with no leftover touches nothing.
- **Docs**: `spec/design/sync-execution.md` (the cleanup paragraph), `spec/design/system.md` (the rename paragraph H9 rewrites), a requirement of its own.
- **Live proof**: none needed if the DoR test is a real-filesystem test; otherwise one kill between the two renames on the test account's folder.
- **Order**: after H9 merges (same files in `internal/synctree`), and after W32 and W42 (the rider below reads W42's move). Size M.
- **Rider (2026-11-15, from the review of W42, note 7)**: a second leftover of a local move. Where the system refuses the no-replace flag, the entry is linked or copied to its new name and the old name is then removed; when that removal fails the move reports `synctree.ErrSourceNotRemoved` and counts as done (kept so in W42, whose second pass adds a test of what is left at the old name). From reading, not run: the next observation sees the old name as a new local file and uploads it, so OneDrive ends with the file twice. The DoR proves what the next plan does with a test that plants both names. If a second item is uploaded: before the first plan after such a move, an old name that is the same file as the moved entry (same device and inode) is removed through the rooted remove; an old name that is a copy, or cannot be removed, is reported as an issue that names both paths and is never deleted or uploaded silently. If the fix needs a record of the move that outlives the run (a store change), stop and report.
- **Rider (2026-11-15, from W42's second review, note 4)**: since W42 a local move renames on parent descriptors, which the case-insensitive shim (`internal/synctest/fsshim.go`, `Interpose`) does not wrap. On Linux `TestCaseInsensitiveShim_RemoteCaseOnlyRenameConverges` therefore renames for real on ext4 and no longer reaches the emulated spelling-only rename or the respell step, and still passes. Only the hard-link model test (`executor_local_spelling_rename_test.go`) reaches the plain-rename-then-respell path on Linux; real APFS does on the macOS shards. This row works on that path: make a Linux test reach the respell step (the shim wraps the descriptor renames, or a seam of `synctree` does), and check that the shim's doc says which renames see the volume beneath.

#### W45. The chaos model runs in virtual time

- **Source**: W37's first step on 2026-11-14 (`reports/W37-report.md`): two runs of one seed sent different requests in the same step. The log shows that much. The writer's reading of the cause, consistent with the log and not proven by it: a one-shot pass releases held retry work by elapsed wall time (`drainDueHeldWorkNow`), the model clock is the wall clock plus an offset, the one-shot loop waits for ramp admission instants in real time, and the observers sleep a throttled or failed delta page's wait on the wall clock (up to 30 s in a storm).
- **The rule**: inside the chaos model no goroutine waits on real time; time moves only when every goroutine of the run is blocked.
- **DoR's first job (prove, do not reason)**: whether the model can run inside a `testing/synctest` bubble (the toolchain is go1.26.11, so the package is there). Name every goroutine that is not durably blocked in a bubble: the SQLite store, the fake Graph server if it uses real sockets, the observers, file I/O. If a bubble cannot hold the run, the fallback is a virtual model clock that advances on `AfterFunc` and `Sleep`; W37's writer found that a clock frozen within a pass hangs `runResultsLoopWithOutbox`, so a virtual clock must advance, not stand still.
- **Product changes this needs**: the observers take their waits from the engine clock, not the wall clock (`internal/sync/engine.go` near the observer construction, `observer_remote.go` retry waits). That is a fix in its own right: one clock for the engine. The conflict copy's name takes its stamp from the engine clock too (`executor_conflict.go`), so a stream's identity does not hold wall time.
- **Tests first**: a test that a storm's 30-second Retry-After costs no real time in the model (a wall-clock bound on one seed, failing today); a test that an observer's retry wait is taken from the clock the engine was given.
- **Measure and report**: the real time of each chaos seed before and after (78 s and 153 to 205 s for seeds 1 and 2 at host load 5 to 10 before). No claim about the saving is made here.
- **Unknown**: whether the divergence between two runs of one seed is gone after this row alone. Measure it with W37's fault log (per-stream comparison) and write the answer down; W46 owns the assertion.
- **Docs**: `spec/design/system.md` (the chaos model), `spec/design/sync-engine.md` (one clock).
- **Riders from the review of W37's second pass (2026-11-14, `scratchpad/R-W37b-findings.md` items 1, 2, 7, 8)**: (1) the names the fake hands out are counted in the order requests arrive, so a stream keyed by one depends on other streams' traffic: item IDs from `nextID` (`internal/sync/scenario_remote_test.go:160-163`), upload URLs from `sessionSeq` (`scenario_remote_session_test.go:296-297`), delta tokens `seq-<changeSeq>` (`scenario_remote_test.go:455`). Derive each from what it names (an item ID from its path and a per-path count, an upload URL from its item, a delta token not from the global count), then drop the matching sentence from the chaos header comment and `spec/design/system.md`. (2) Another client's write draws its file with `intN(len(files))` and draws nothing on an empty tree, so the draw's range follows the tree. Tried in W37's review and taken back: a draw from a fixed range before the tree is read made seed 2 lose its session-timeout fault (the race seed's own assertion failed) and its plain run took 360 s instead of 134 s; why the run got longer is unknown. Make that change here, where the seeds are chosen again anyway. (3) Not checked: whether a seed that hangs until `-timeout` prints the fault summary and the rerun line (both are logged from a cleanup); check it and make a hanging seed print both. (4) The fault log is neither printed nor saved when a model run fails; save it beside the test's other output (a file under `t.ArtifactDir()` or the like) and name the file in the failure.

- **Order**: after W37's second pass merges (it needs the fault log). Before W46. Size L.

#### W46. Jitter comes from a source the caller passes; a chaos seed replays identically

- **Source**: W37's "Seen and not fixed", entries 1 and 2: `retry.Policy.Delay` draws its jitter from the global `math/rand` source (`internal/retry/policy.go`), and the engine clock's `Jitter` does the same (`internal/sync/domain_clock.go`), so no test can seed either.
- **The rule**: no package under `internal/` draws from the global random source; every random choice comes from a source its caller passes.
- **Change**: `retry.Policy.Delay` takes its jitter from a source the caller passes; the engine clock's `Jitter` is the one source for the engine and the transport's retry waits; production seeds it once at start. In the chaos model the source is derived from the seed and the key the wait belongs to, so the order in which waits are asked for cannot move a draw. A source scan (the kind the repository already has for bare goroutines) fails a call of a global `math/rand` function in product code.
- **Then the assertion W37 was written for**: `TestModel_ChaosSameSeedReplaysIdentically` at the model level, over every seed the chaos tests use, 20 runs each, plain and under the race detector, compared per stream. If runs still differ, the first differing entry names what is left; that is reported, not sorted away.
- **Tests first**: the scan test; a test that two policies given equal sources give equal delays; the replay test, seen failing on the tree before this row.
- **Docs**: `spec/design/system.md` (what a seed fixes: after this row, the run), `spec/design/retry.md` if there is one, a requirement for the scan.
- **Order**: after W45, and after W31 and W8, which change the retry loops. Size M.

#### W47. No e2e failure message or log prints a drive ID, an item ID or an address

- **Source**: riders 4, 6 (in part) and 7 (in part) of W43, split off on 2026-11-14 before W43's brief was written, so that each row is one writer's change: W43 is about what a test waits for, this row about what the suite prints. The evidence is in `reports/W3-report.md` (the second and third passes' "Seen and not fixed") and the live run of 2026-11-14.
- **The rule**: nothing the e2e suite writes to a test log, a failure message or its debug log names an account, a drive, an item or a fixture of the CI or test account (R-6.10.27). A value is named by its role. CI logs are public.
- **Decision (mine)**: drive and item IDs of the CI and test accounts count as names the suite must not print; `Redact` learns them from the fixtures it already holds, and the debug log's command line goes through it.
- **Change**:
  1. `Redact` masks the drive and item IDs of the fixtures wherever they stand: `stat` stdout (`e2e/sync_full_test.go` near 841-851), `ls --json` stdout (`e2e/shared_items_helpers_test.go` near 774 and 836), `--debug` stderr, status JSON `display_name`.
  2. The debug log's command line goes through `Redact` (a shared-root `--drive` selector was seen there as given on 2026-11-14, while the same log shows a role name for the suite's own drive).
  3. The debug log records `shared --json` and `drive list --json` output through `Redact` only: the DoR says what `Redact` leaves there (IDs of items that are no fixture) and whether a mask by shape is needed.
  4. Raw stdout and `--debug` stderr in failure messages in `e2e/e2e_test.go` (near 510, 516, 685, 720, 745, 798, 883) go through `Redact`, bounded.
  5. `assert.Equal` on addresses in the two preflight and caller-proof files becomes `assertSameIdentity`; the message near `e2e/sync_full_test.go:419` no longer relies on `Redact` for the sharer's address.
  6. The socket.io helpers' failure messages print a bounded, masked summary instead of whole debug events.
  7. Whole-output `assert.Contains` calls go through one helper that prints the output masked and bounded when it fails.
  8. A source-reading test in the untagged quick lane, if a rule can be stated without false findings (no failure message takes a raw stdout, stderr or ID variable outside the masking helpers); if not, the report says why.
- **Tests first**: untagged tests of `Redact` with planted values (a drive ID, an item ID, a selector, an address) in each of the shapes above; the planted cases of the source-reading test.
- **Docs**: R-6.10.27 and the Verification Policy sentence on what the suite prints.
- **Live proof**: every changed test one at a time on the test account; then one debug log read with the masked reader, which must show role names only.
- **Order**: after W43 merges (same files). Size M.
- **Rider (2026-11-14, from the review of W43, its note 11)**: the poll-predicate scan (`e2e/poll_predicate_rule_test.go`, `e2e/daemon_wait_rule_test.go`) leaves two shapes out, and its header says so: a predicate a suite function builds and returns (`dryRunPlanHasLineStarting`), and one a function forwards from a variable or a parameter (`ready` in `requireSyncWithModeEventuallyConverges`). Close both, tests first with planted sources: record the function literals a suite function returns, read a call of such a function in a predicate position as that literal (its failing call, not the builder's own), and follow a predicate handed on through a parameter to the poll primitive. Plant an asserting builder, a pure builder, a forwarded asserting predicate and a forwarded pure one. A shape that cannot be followed without false findings stays out, and the header names it with the reason.

#### W48. Requests that time out count as outage evidence

- **Source**: the review of W31 on 2026-11-14 (its note 7; `reports/W31-report.md` holds the pass that followed). After W31 a request that times out while the run goes on is retry work with the request-timeout issue. The classifier gives it no scope evidence (`internal/sync/engine_result_classify.go`, the request-timeout branch), while an answered 504 or 408 gets `ScopeEvidence` and `RunScopeDetection` (`classifyHTTPResult` in the same file). So many requests timing out against one stalled service never open an outage scope: every action retries on its own ladder and the user sees one issue per file instead of one outage.
- **The rule**: a service that does not answer is an outage like a service that answers 503. The same evidence, the same threshold, the same scope, the same one line to the user.
- **Not known before the DoR**: which scope a timeout belongs to (the service scope a 504 opens, or a narrower one per host), whether a dial timeout and a response-header timeout are the same evidence, and what the scope's trial request does when it times out too.
- **Change**:
  1. The request-timeout branch of the classifier returns the scope evidence a 504 returns, by the same function, not by a copy.
  2. The scope's trial request that times out keeps the scope open (test first; if it already does, the test stays as the pin).
  3. `status` and the watch condition log name the outage once, not once per file.
- **Tests first**: the classifier table gains the rows; one engine test with the stalled-server helper W31 added, in which several actions time out against one server and one outage scope opens after the threshold; one in which a single timeout opens none.
- **Docs**: the outage-scope part of `spec/design/sync-engine.md` and the requirement that names what counts as outage evidence.
- **Live proof**: none possible here (no network fault injection on this host); the in-process stalled server is the proof.
- **Order**: after W31 merges (same files). Size S.
- **Rider (2026-11-14, from the review of W31's second pass, its note 7)**: the detached bound on an action's commit (`internal/sync/worker.go`: the context made for the commit once the action's own context has ended) expires as `context.DeadlineExceeded`. The completion carries that error to the classifier, and `isRequestTimeout` (`engine_result_classify.go`) gives it `request_timeout`: a wedged store is shown as a request that timed out, and with this row's change it would count as outage evidence. First step of this PR, test first: one sentinel joined as the cause where the bound's context is made, and a classifier branch ahead of `isRequestTimeout` that reads it as a store failure. Test: an action whose commit outlasts the bound is not `request_timeout` and opens no outage scope.

#### W49. A file put back at its old path after its folder moved stays there

- **Source**: the review of W36 on 2026-11-14 (its notes 3 and 4 and its "seen and not fixed 1"; `reports/W36-report.md`, section "Review fixes"). Proven by me with a scenario on main `36f2e2b6` and on the W36 tip alike: with no sync in between, `mv docs photos/photos; mkdir docs; mv photos/photos/50%.txt docs/50%.txt` (the file is back at the path and name it had), then one sync. Both sides end with the file at `photos/photos/50%.txt`: the sync undoes the user's move of the file. No data is lost. The same three steps with a sync after the first two end right. The case was written as "one run same name" in `TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges` (`internal/sync/engine_move_recreate_test.go`, fields `oneRun` and `keepName`) and taken out of W36 because it fails for a reason W36 does not touch.
- **The rule**: after a sync both sides hold the tree the user left. An entry whose path equals its baseline path is not thereby unchanged: its parent may be a different folder now.
- **Not known before the DoR**: the mechanism. The reviewer's reading, not checked: `planMove` in `internal/sync/planner_move_composition.go` (near line 935) treats a move whose old and new paths are equal as nothing to do, so the file follows the folder's move. Whether the mirror case (the three steps made on OneDrive, one sync) fails too is unknown: run it first. Whether a folder put back the same way behaves alike is unknown.
- **Change**:
  1. Fix what item 1's failing test shows, at the place the DoR finds. The file stays at `docs/50%.txt` on both sides, moved on the other side by one move and not sent again.
  2. A child folder that takes the path its parent vacated: `mv docs archive; mv archive/sub docs`, one sync. By reading (`movePrecedes` and the children-leave-first edge in `internal/sync/planner_dependencies.go`) this is a dependency cycle on main, with no folder create in the plan; not run. Prove it first with a scenario case. If it is a cycle, the exception W36 made for a move into a folder re-created at the vacated path is the model: the child's move does not hold the parent's.
  3. `planMove` (`internal/sync/planner_move_composition.go`, near lines 914 to 917): a move that leaves a moving folder for a path below that folder's old path keeps its old path as it was, so the executor finds it stale once and the next pass of the same run moves it (one extra pass, inside the four-pass bound, no wrong data). Rebase its old path as the code a few lines below already does for the other case, so it moves in the first pass. Then check whether W36's exception in `movesIntoRecreatedSource` is still needed; if it is not, remove it with its tests rewritten to the new order, and say so in the spec.
- **Tests first**: the scenario case "one run same name" and its mirror on OneDrive, both failing before the fix; a scenario case for item 2 (one sync, both trees equal, no "dependency cycle" in the log); for item 3 the existing cases assert zero superseded actions in the single sync where they assert one today. Unit rows in `planner_move_composition_test.go` for each plan the scenarios build.
- **Docs**: the move-ordering part of `spec/design/sync-planning.md`.
- **Live proof**: mine, on the test account: the three steps of item 1 in an isolated tree, one sync, both sides listed.
- **Order**: after W36 merges (same files, and it extends W36's scenario test). Size M.

#### W50. A crash during a conflict copy leaves nothing empty behind (crash model seed 318012)

- **Source**: the first measured run of the rotating seed range (2026-11-14, `reports/A11-report.md`, "Finding first" and "Not fixed"), on A11's tree (base `0ff7c754`, before #911 to #913 and before H9). `ONEDRIVE_GO_MODEL_SEED=318012 go test -run '^TestModel_CrashAtSeededPointsRecovers$' ./internal/sync` fails in batch 2: an empty conflict-copy reservation is left (`sync_invariants_test.go:181`, reached from `engine_model_based_test.go:331`). The writer says it replays exactly; I have not rerun it.
- **Not known**: the cause. Not checked: whether the seed still fails on main once H9 is in. H9 creates nothing before the conflict copy's rename and deletes the reservation, so the invariant may have nothing left to find; and #913 changed the model's files, so the same seed may draw another run on main. Neither is a fact until the replay is run.
- **First step**: replay the seed on main with H9 and A11 merged, through the quiet runner, never with `-v`.
- **Change**: if it still fails, find the cause from the replay, fix it in the product, and keep the case as a named scenario test (the steps written out, not a seed number). If it no longer fails, say which merge ended it and show the replay output; then check that the invariant at `sync_invariants_test.go:181` still means something (an invariant about a reservation no code makes any more is rewritten to what can go wrong now, a conflict copy at a name with no source beside it, or deleted with the reason), and close the seed's issue with that.
- **Tests first**: the replay failing, or the proof that it passes; then the named scenario.
- **Docs**: the crash-recovery part of `spec/design/sync-execution.md` if the fix changes what a restart finds.
- **Order**: after H9 and A11 merge. Sync lane. Size unknown until the replay.

#### W51. Crash recovery converges within its pass limit (crash model seed 318011)

- **Source**: the same run. `ONEDRIVE_GO_MODEL_SEED=318011 go test -run '^TestModel_CrashAtSeededPointsRecovers$' ./internal/sync` fails: batch 4 after the crash is still changing after 8 passes (`engine_model_based_test.go:502`). The writer says it replays exactly; I have not rerun it.
- **Not known**: the cause; whether a pass undoes what an earlier pass did (a product livelock), whether the run needs more passes for a reason that can be stated, or whether the model is wrong; whether the seed fails on main after #913 and H9.
- **First step**: replay on main with A11 merged; record what each of the 8 passes planned (action kind and path per pass, from the debug events or a test-only print) and name the cycle.
- **Change**: if a pass undoes another, fix the product and keep the case as a named scenario test. If the limit is too low for a legitimate case, set it from the stated reason (how many passes that case needs and why), never by raising it until the seed passes. If the model is wrong, fix the model and say what it got wrong.
- **Tests first**: the replay failing; then the named scenario with the pass count asserted.
- **Docs**: the pass bound in `spec/design/sync-engine.md` if it changes.
- **Order**: after A11 merges; after W50 if both touch the same recovery code. Sync lane. Size unknown until the replay.

#### W52. A subfolder moved onto its moved parent's old path on this computer converges

- **Source**: W49's report ("Not fixed 1") and its review (finding 7), 2026-11-14. W49 makes the case converge when the steps are made on OneDrive; made on this computer the same steps still plan a cycle on every sync.
- **The case**: `mv docs archive; mv archive/sub docs` on this computer between two syncs: a folder moves away and one of its subfolders is moved onto the folder's old path. The comparison leaves the subfolder unpaired (`docs/sub` reads as missing locally; the local `docs` row holds the parent's baseline row), so the plan is the folder move, a remote create of `docs`, a remote move of the file inside, and a remote delete of `docs/sub`: a four-action cycle dropped on every pass, every sync exits 1, nothing is lost. Both forms (made on OneDrive, made here) are written out as scenario code in `reports/W49-item2-both-cases.go.txt` (corrected by W49's second pass so that it compiles).
- **Where, by reading**: `internal/sync/sqlite_compare.go` refuses the pairing in three places: near `:92` a local move target may not be a path the baseline holds, near `:120` the same for files without a hash, near `:525` `local_move_dest` needs `baseline_present = 0`. The row of the old folder path is already the moved folder's `local_move_source` (near `:520`), and one comparison row gets one kind.
- **Decision at the DoR (mine)**: the fix changes the comparison model, probably with a new comparison kind or a second role for one row. The DoR lays out the alternatives with what each does to the planner's decision table, and stops for the decision before any product code.
- **Tests first**: the scenario failing on the unchanged tree (sync never settles, the planner logs a dropped cycle each pass); then a comparison-table row and a composition row.
- **Docs**: `spec/design/sync-planning.md` (the comparison kinds and the move composition).
- **Rider (2026-11-14, from W49's report, "Not fixed" 2, and its review)**: `underMoveDestination` in `internal/sync/planner_dependencies.go` (near `:492`) reads deletes and cleanups below a move's destination as post-move paths, though the comparison addresses them at baseline paths. For a folder moved into the path its moving parent vacates the two readings coincide, so W49 makes such a folder wait one more pass instead of rebasing it. By reading only: the code makes no mistake today, it costs one pass inside the bound. The DoR writes the concrete plan shape (the W49 case made on OneDrive with a delete below the moved subfolder), shows the extra pass with a scenario, and says whether ordering such a delete by the other move's source removes it without a new cycle. If it does, do it here; if not, the row's report says why and the wait stays.
- **Rider (2026-11-15, from W49's second pass and my live proof)**: the scenario of `internal/sync/engine_move_recreate_test.go` for the recreate made on OneDrive asserts sync counts, not what each sync plans, and W49's second report says the per-sync plans were not established. My live run of that case on W49's second version converged in ONE sync that did: a local move of the folder, a folder create below the moved folder, a folder create at the old path, a local move of the file. The DoR establishes the real plan of each sync of that scenario (by the planner's own output in the test, not by reading) and the test gets one assertion per real plan, so a later change that reaches the same end by a longer or riskier route fails it.
- **Order**: after W49 merges (same files). Sync lane. Size L.
- **Rider (2026-11-15, from W49's fourth pass, "Seen and not fixed" 3)**: a local move into a folder OneDrive made at the path a local folder move leaves (on OneDrive: `mv x x2; mkdir x; mv y x/y`). The first plan holds no local create of the new x (none is planned while the old x holds the path) and no order between y's move and x's, so y's move either lands in the old x and is moved again next pass (main's timing) or fails once as stale (W49's commit; 58 runs of the differential in `reports/W49-differential.md` show one more superseded action than main, none needs more syncs). The writer's candidate: plan that local create in the same pass, waiting for the local move that leaves the path, as `recreatedSourceCreates` does for OneDrive, so the move into it waits for it. Assert what each sync plans, not only the sync count.

#### W53. A folder moved into its own former child converges

- **Source**: the review of W49 (note 12), 2026-11-14, by reading only: not run. Present before W49 too.
- **The case**, steps on this computer between two syncs: `mv docs tmp; mkdir docs; mv tmp/sub docs/sub; mv tmp docs/sub/docs2`. The child put back at the folder's old path waits for the folder (before W49: was deferred past it), and the folder waits for its target parent, which is that child. No occupancy cycle exists to park, so both are deferred on every pass.
- **Not known**: whether it reproduces; whether the same shape made on OneDrive behaves the same.
- **First step**: the scenario as an engine test on the unchanged tree, for both sides. If neither form fails, close the row with the test kept and say so.
- **Change**: decided at the DoR from what the scenario shows. The candidates the review saw: emit the child's move first when its target parent is a folder created at a vacated path (the create does not need the moving folder), or park one of the two at a temporary name as an occupancy cycle is parked.
- **Tests first**: the two scenarios; a `TestBuildDependencies` row for the plan the fix yields.
- **Docs**: `spec/design/sync-planning.md`.
- **Order**: after W49 and W52 (same files). Sync lane. Size M.
- **Rider (2026-11-15, from W49's fourth pass, "Seen and not fixed" 2)**: 29 of the 39 sequences that fail on main and on W49's commit alike in `reports/W49-differential.md` are one more cycle of this kind (`mv y x/y; mkdir y; mv x/a.txt y/a.txt; mv x x2` on this computer, and kin): an entry leaves a moving folder for a folder made at a path that another folder's move leaves; children leave first (`internal/sync/planner_dependencies.go` near 472), and that other folder's move, composed after the moving folder's, waits for the moving folder. The smallest fix is not determined; the writer's candidate is that composition holds the entry's move a pass when its target lies at or below a path another folder move of the plan vacates and its source lies below a folder move's source. Acceptance: the differential harness (`reports/W49-differential-test.go.txt`), these 29 converge and nothing that converged stops.

#### W54. An existing name after a 409 is found with one lookup

- **Source**: the review of H7 (note 8) and H7's report ("Not fixed" 2, 3 and 10), 2026-11-14.
- **What is wrong**: `putFile` in `internal/cli/put.go` (near `:233-252`) lists the whole parent folder on every 409 to find the item that holds the name, so a re-upload of a folder with n files costs n listings of n children. `EnsureFolder` in `internal/driveops/session.go` lists the whole parent likewise for a folder that exists.
- **Change**: one lookup of the one name (a path-addressed read of `parent:/name`), keeping the comparison OneDrive itself makes (`namekey.Equal`, R-1.3.8), in one function both callers use. The DoR says what the path-addressed read returns for a name that differs only in case or Unicode form, from the Graph quirks notes and a live read.
- **Riders**: (a) a folder `put` prints its paths with `--json` without the leading slash (`Backup/a.txt`) where a single `put` prints `/a.txt`: one form, the single put's; the JSON contract test holds it. (b) `internal/driveops/transfer_upload.go` near `:361-364` and `:401-405` holds the mtime choice and the perf record twice (`uploadMtime`, `recordUploadPerf`): one copy each. (c) (2026-11-15, from the review of H7's second version, should-fix 6) an upload failure names the local path twice: `internal/driveops/transfer_upload.go` near `:209`, `:243` and `:274` and `internal/driveops/hash.go` near `:43` wrap an error that already holds the path, and the folder form then prints `Failed: <path>: ...<path>...`. One naming: the layer that knows the path names it once; the tests that hold the failure text say so.
- **Tests first**: a fake that counts listings and fails the test on a second one; the case-different name; the JSON path form.
- **Live proof (mine)**: `put -r` of a 200-file tree twice under the test account, the second run's request count from `status --perf` or the debug log.
- **Docs**: `spec/design/drive-transfers.md`, `spec/design/cli.md` (the JSON path form).
- **Order**: after H7 merges (same files). Outer lane. Size M.

#### W55. Folder listings are read a page at a time

- **Source**: H7's report ("Not fixed" 5) and its review, 2026-11-14.
- **What is wrong**: `ListChildren` in `internal/graph/items_fetch.go` (near `:175`) reads every page of a folder and returns one slice. H7 bounds the transfers and the walk of a recursive `get`, but a folder with a million children is still held whole while it is listed.
- **Change**: a paged form (a callback per page, or an iterator) beside the slice form; `DownloadTree` and every other caller that only walks the children use it; callers that need the whole list keep the slice form, each named in the DoR with its reason. The retry of a failed page restarts that page, never the folder.
- **Tests first**: a fake Graph server with three pages that fails the test when page 2 is asked for before page 1's children were handed on; a page that fails once and is retried; cancellation between pages.
- **Docs**: `spec/design/drive-transfers.md` (the memory bound of a recursive get), `spec/design/graph-client.md` if it names the listing.
- **Order**: after H7 merges; after W54 if both touch `session.go`. Outer lane. Size M.

#### W56. A failed Graph answer names its error code

- **Source**: the full-tier run `43687851252` of the branch of #916, 2026-11-15 07:09 UTC, job `e2e`. `TestE2E_Mv_Folder` (`e2e_full`) failed once: OneDrive answered the CLI's one folder move (the `PATCH` of the item with its new parent) with HTTP 400 and the message `Invalid request`. In the same command the destination path had just answered 404, as it should, and the new parent 200. The test passed alone on the development host about an hour later, and #916 touches neither the test nor the move. The cause is unknown.
- **Why a row**: it is the second such answer in four days (LI-20261111-01 was a folder create), and `spec/reference/graph-api-quirks.md` says a recurrence can be reconsidered. Both records say the log holds no error code. By reading: the client parses the code chain into `GraphError` (`Code`, `InnerCodes`), but only a quirk-retry record reads it (`internal/graph/quirks.go` near `:111`). `GraphError.Error()` (`internal/graph/errors.go` near `:281`) prints the status, the request id and the message, and no debug line names the code. So neither incident shows whether the answer carried a code: the payload evidence the design asks of a quirk rule (`spec/design/graph-client.md`, Design Constraints) was not captured, twice.
- **Decision (mine, 2026-11-15)**: no retry rule on this evidence. This change makes the evidence exist. The retry rule is decided from the first occurrence whose code is on record, and gets a row of its own then. Until then: a one-shot command that fails this way is rerun by its user, sync keeps a bare 400 as per-item retry work as it does today, and a full-tier e2e failure of this shape is rerun with `verify failed-test` and recorded as a recurrence, not worked around in the test.
- **Change**: (1) `GraphError.Error()` names the most specific code after the status when the answer has one; the text stays sanitized. (2) The debug line of a failed answer records the code and the inner codes, and says so when the body holds no `error` object, so that "no code" is a fact of the answer and not a gap in the log. (3) Every reader of the error text follows.
- **DoR first**: list every reader of the error text (CLI output tests, e2e assertions, examples under `docs/` and `spec/`) and say which change; say what the debug log holds today for a failed answer, by reading and by one fake-server test; check that the sanitizing of `Message` and `RawBody` covers the new text.
- **Tests first**: an error with a code chain prints its most specific code; one without a code prints as today; the debug line for both; the redaction test extended to the new field.
- **Docs**: a new entry LI-20261115-01 in `spec/reference/live-incidents.md` (index line too; account, drive, item and request ids omitted), with the earlier entry's "Last seen" and "Recurring" brought up to date; the quirk section "A Folder Create Can Be Refused With A Bare 400" in `graph-api-quirks.md` renamed and extended to a write in general, with both occurrences and what each log could not show; the family added to `spec/reference/live-recurring-families.md` with the rule of the decision above; `spec/design/graph-client.md` (what the error text and the debug line hold).
- **No live proof is possible**: the answer cannot be provoked. Fake-server tests only.
- **Order**: none. Outer lane. Size S. By the branches' file lists of 2026-11-15, no branch in flight changes `internal/graph/errors.go`.

#### W57. The crash model draws the crash point of the replacing move

- **Source**: the hand-back of W42 on 2026-11-15 ("Seen and not fixed"). W42 adds a crash point between the two steps of the replacing local move (the verified destination is taken out of the tree, then the no-replace rename). `allCrashPoints` (`internal/sync/crash_points_test.go` near `:319`) does not hold it, because `crashRecoveryScenario` (near `:278`) has no replacing move. So the seeded crash family `TestModel_CrashAtSeededPointsRecovers` never draws it. One targeted test of W42 holds the state that crash leaves; the model does not.
- **Why a row and not part of W42**: a point added to the list, or a move added to the scenario, changes what every seed replays. Seeds 318011 and 318012 are open as W50 and W51 and must replay unchanged until their fixes merge.
- **Change**: (1) the crash scenario gains a replacing move, so the point is reached; (2) `allCrashPoints` holds the point; (3) a test reads the crash point constants from `crash_points.go` and fails when one is in no list, unless such a test exists (the DoR says); (4) the crash family is run over the seeds of three UTC days on the development host, behind the heavy-run lock, and a failing seed becomes a row of its own.
- **DoR first**: is there already a test that fails on an unlisted crash point; what the scenario's remote and local trees hold and where a replacing move fits; whether the change alters what an open seed issue replays (then the issue says so).
- **No live proof is possible**: test files only.
- **Order**: after W50, W51 and W42. Sync lane, test files only. Size S.
- **Rider (2026-11-15, from W42's second review, note 9)**: `spec/design/sync-engine.md` near line 110 lists the crash point of the replacing move among points of which the row says a fresh engine on a snapshot taken at each converges, "and so does one whose database lost every commit since the last checkpoint". For this point only its own test runs (`crash_replacing_move_test.go`), without power loss, and `allCrashPoints` leaves it out. When this row adds the point to the model's draw, it also runs the power-loss half for it, or the spec sentence is narrowed to what is run.

#### W58. Every metadata body read is bounded, and a lost connection has a type

- **Source**: the hand-back of W48 on 2026-11-15 ("Seen and not fixed", two entries). Both are claims from reading, not proven. (a) The transports bound dialing, TLS and the wait for response headers, and the stall watchdog runs only around transfer bodies (`internal/graph/transfer_watchdog.go` near `:16`, D-045). A metadata response body (a delta page, a listing, the answer to a create) that stalls after its headers has no bound: a live but silent server holds the request until the run ends, and a watch has no end. (b) An HTTP/2 connection closed by an unanswered ping fails its streams with an error of no timeout type (`http2: client connection lost`), which the sync classifier reads as an actionable `unexpected_condition` per file instead of a request that was never answered.
- **Why it matters**: after W48 a request OneDrive never answers is outage evidence. These are the two shapes of silence that W48 cannot see: one never returns, the other returns untyped. D-045 closed the same hole for transfer bodies only.
- **Change**: (1) every response body read of a metadata request is bounded, by the stall watchdog or by a read deadline (the DoR picks one by reading), and fails with a typed, retryable error that `isRequestTimeout` reads; (2) the lost connection gets a type in `internal/graph`: the text is matched in one place at the transport boundary, and a test pins the standard library's text, so a Go release that changes it fails that test; the sync classifier reads the type as a request never answered; (3) the design text of the graph client and of the sync engine says so.
- **DoR first**: prove (a) with a fake server that sends headers and then nothing: does the request return, and when; list every body read in `internal/graph` (a decoded answer, an error body, a delta page) with what bounds it today; for (b) say whether an HTTP/2 test server can produce the error without a sleep, and if not, how the type is tested; say what a one-shot command and `status` do with each today (a command has its own limit since #905).
- **Tests first**: the stalled body returns within the bound with the typed error; the classifier reads both as a request never answered; a slow but moving body is not failed.
- **No live proof is possible**: neither can be provoked. Fake-server tests only.
- **Order**: after W48 (it reads `isRequestTimeout`) and W56 (it changes `internal/graph/errors.go` and the debug line of a failed answer). Size M.
- **Rider (2026-11-15, from the review of W48, must-fix 1)**: W48's second pass makes the sync classifier accept the lost HTTP/2 connection by its text, in one named constant of `internal/sync/domain_graph_error.go` (`isNetworkTimeout`), because the health check of the production transport closes a silent connection with an untyped error and W48's rule would otherwise miss it. When this row gives the lost connection its type, that text match and its constant leave `internal/sync`: the classifier reads the type, and the table row of the timed-out requests is built from it.
- **Rider (2026-11-15, from W48's second pass)**: the text of the lost HTTP/2 connection also reaches a response body that is being read when the connection closes (`h2_bundle.go` near `:9138` of go1.26.11), outside any `*url.Error`. W48 does not match it there. From the writer's reading; not run. The DoR proves or refutes it with a fake server, and the type this row gives the lost connection covers the body read too.
- **Riders (2026-11-15, from W56's second pass, "Seen and not fixed" 2 to 4; all from reading)**: (1) no requirement states that an inner `serviceReadOnly` is left out of the drives token-propagation retry or that it reads as `ErrProviderReadOnly` (`spec/requirements/non-functional.md` near R-6.7.13); three tests of `internal/graph/service_read_only_test.go` cite none, and the Verified By row near `spec/design/system.md:25` names no requirement. One row, cited by them. (2) R-6.7.30 says an oversized body is reported as an error and not silently cut; an error body is cut at 64 KiB without one (the record's `body_bytes` shows the bound). The row names the bounded error-body read, or the read changes; this row bounds body reads, so it decides. (3) `internal/graph/quirks_test.go` near `:324-327`: the comment of `TestHandleChunkResponse_Unexpected2xx` says the request path cannot produce a 204; a fake server can. Drive the case through `UploadChunk`, or drop the comment.

#### W59. The trash side of a detach goes through a descriptor of its folder

- **Source**: the review of W42 on 2026-11-15 (its note 16 and its answer on the trash side) and W42's report ("Seen and not fixed"). A detach takes a verified file out of the sync tree through descriptors, but reaches the trash entry folder by path: `internal/synctree/detach_unix.go` near `:179-186`, `:397` and `:440-482` (`linkatOut`, `moveDetachedOut`, `copyThroughDescriptor`, `PreserveRegularFile`). The check that the destination is outside the tree (near `:213-219`) compares text.
- **What can go wrong**: no loss was found by reading: the entry folder is reserved new, so nothing is replaced, and a purge that removes it makes the move-out fail and the file is linked back. Misplacement only: if another process of the same user swaps a folder of the data folder for a link while a detach runs, the file lands where the link points, the sync tree included. Managed-state writes elsewhere already go through a descriptor rooted at their folder, so a swapped link cannot redirect them; the trash side of a detach is the one that does not.
- **Change**: the entry folder is opened once (`O_DIRECTORY|O_NOFOLLOW`) and the link, rename, create and remove are made on its descriptor; the outside-the-tree check compares device and inode of the opened folder, not text. About 50 to 80 lines by the reviewer's reading.
- **DoR first**: confirm each site and whether `internal/localtrash` reserves the entry folder through a descriptor it could hand over; say what macOS offers for each call; show the misplacement on the unchanged tree with a test that swaps the entry folder for a link between the reservation and the move-out (a hook, no sleep), or say why it cannot be shown.
- **Tests first**: the swapped folder: the file is not moved through the link and the detach fails with the file back in place; the plain case and the other-filesystem copy still work.
- **No live proof is possible**: real-filesystem unit tests only.
- **Order**: after W42 merges (same files) and after W44 if W44 touches `internal/synctree`. Sync lane. Size S.
- **Rider (2026-11-15, from W42's fourth pass, "Seen and not fixed" 1 to 3; from the writer's reading, nothing run)**: (1) `takeOutVerifiedLocalFile` in `internal/sync/executor_delete.go` wraps a `Trash.Reserve` failure with `%w`, so an ENOENT from it (the entry folder removed between its `MkdirAll` and the entry write) still reads as "already absent" in the delete and as "left on its own" in `clearReplacedLocalMoveTarget`: name the Reserve cause as text, as `synctree.ErrDestinationFailed` does, with a seam in `localtrash` for the test. (2) `heldFileIn` in `internal/localtrash/localtrash.go` reports an entry whose folder is gone as holding its item, so `Placement.Abandon` logs "it holds a file" for an entry that no longer exists: a `ReadDir` that fails with not-exist returns nothing held. (3) `linkBackNoReplace` in `internal/synctree/detach_unix.go`: on a filesystem without hard links the put-back of a raced set-aside file always fails, so the file stays under its temporary detach name (reported by every full scan, never lost): when the link is `localpath.IsLinkUnsupported`, put it back with the checked rename the no-replace rename falls back to, with a seam and a test.

#### W60. The engine treats every failure to reach OneDrive as one family

- **Source**: the review of W48 on 2026-11-15 (its note 8 and its answer on the neighbouring errors). All claims are from reading; nothing was run. After W48 the timeouts are outage evidence: a dial, name-resolver, response-header, TLS handshake, keepalive or whole-request timeout, a stalled transfer and the lost HTTP/2 connection. The other ways a request fails without an answer are not: connection refused, no route to host, network unreachable, connection reset and a name that does not resolve are a per-file `unexpected_condition` a user is asked to act on, before W48 and after it (their `Timeout()` is false; `internal/driveops/local_errors.go` near `:46-66` maps none of them). The retry transport already treats all of them as one family, "can't reach OneDrive" (`internal/retry/transport.go` near `:219-276`).
- **What can go wrong**: a dead local link, a captive portal or a laptop waking from sleep gives a mix: some files in flight count toward the outage scope and wait for its trial, the others become issues that name a file though nothing is wrong with the file.
- **Change**: one predicate for "the request did not reach the service, or got no answer", owned by one package (the DoR says which: the retry transport's classification exported, or `internal/graph`), read by the retry transport and by the sync classifier. Every member is retry work with `service` scope evidence; none is an actionable per-file issue. No new scope kind, issue type or store change: if the DoR shows that the trial or the blocking must differ for a failure that is plainly local, it stops and reports.
- **DoR first**: a table of each error shape (how it arrives, by which type or errno), what the retry transport does with it, what the engine classifies it as today, what `status` and the watch log show, whether the file is retried at all today and when, and what a one-shot command prints. Say whether the line a user reads for the scope ("OneDrive is temporarily unavailable") should differ when the cause is plainly local (no route, network unreachable), as a text choice only, and propose the text.
- **Tests first**: a table row per shape, built as it really arrives (a `*url.Error` around a `*net.OpError` around an `*os.SyscallError` with the errno; a `*net.DNSError` that says not found), failing first; a one-shot run where five files fail with connection refused opens the scope and lists no per-file issue; one such failure followed by a success opens nothing.
- **No live proof is possible**: no network fault is injected on this host. Fake-transport tests only.
- **Order**: after W48 (the same classifier) and after W58 (the lost connection's type lands there). Sync lane. Size M.

#### W61. The tree names no fixture folder and no live item ID

- **Source**: the review of W47 on 2026-11-15 and my reading of `testutil/testenv.go` and `.github/workflows/ci.yml` the same day. Nothing was run.
- **What is wrong**: `testutil/testenv.go` lines 28 to 31 hold default names for the shortcut and shared-folder fixtures of the test accounts, and `applyShortcutFixtureDefaults` (near lines 188 to 205) applies them when the variables are unset. Three reference files name the same folders (`spec/reference/graph-api-quirks.md`, `live-incidents.md`, `output-journeys-vs-abraunegg.md`). `spec/reference/live-incidents.md` holds an item-ID shape of 50 characters on 17 lines (539, 1114, 1116, 1353, 1587, 1678, 1680, 1691, 1692, 1699, 1700, 1864, 1950, 2006, 2184, 2185, 2354); whether those are real IDs or invented ones is not checked. The repository is public. W47 masks the names in what the suite prints by learning them at start; the tree itself still holds them. Never copy a name or an ID from those lines anywhere, this plan included.
- **Change**: the defaults and `applyShortcutFixtureDefaults` go. The five variables (`ONEDRIVE_TEST_SHORTCUT_WRITABLE_NAME`, `ONEDRIVE_TEST_SHORTCUT_READONLY_NAME`, `ONEDRIVE_TEST_SHORTCUT_WRITABLE_SENTINEL_PATH`, `ONEDRIVE_TEST_SHORTCUT_READONLY_SENTINEL_PATH`, `ONEDRIVE_TEST_STANDALONE_SHARED_FOLDER_NAME`) are required by the tests that use them: a missing one fails or skips those tests with a message that names the variable (the DoR decides which, by the rule the suite has today for a missing drive variable). The default tests in `testutil/testenv_test.go` change with it. The reference files name the fixtures by role, and every ID of the 50-character shape is replaced by an invented ID of the same shape. A source check that the tree holds no default for those variables, if one can be written without naming the names.
- **Precondition, unknown today**: the fixture file CI restores (`.testdata/fixtures.env` from the repository secret) and the local one set the five variables. The local file can be checked by counting variable names, never printing a value. The CI secret store is not mine to read or write. W47's second pass makes the suite print which fixture variables were unset (names only), so the first nightly after W47 answers this for CI. If CI lacks them, this row needs the owner to add them to the secret before it merges; that goes in the final report.
- **Order**: after W47 and W56 (both touch the same e2e and reference files), and after one nightly has shown which variables CI sets. Lane x. Size S to M.
- **Live proof**: the shortcut and shared-folder tests, one at a time on this host, and the next nightly.
- **Rider (2026-11-15, from W56's second pass, "Seen and not fixed" 1)**: `spec/reference/graph-api-quirks.md` and `spec/reference/live-incidents.md` still quote the live test drive's ID and further item IDs on it (about 36 occurrences by the writer's count; W56 replaces only the one ID its own text quotes). Each becomes an invented ID of the same shape, and the source check of this row covers the drive-ID shape too. Never copy one of those IDs anywhere, this plan included.
- **Rider (2026-11-15, from the second pass of W47, "Seen and not fixed")**: two places the e2e masking does not reach yet. (1) `e2e/shared_items_helpers_test.go` near `:1381`: a shared folder fixture's name is resolved at run time from `shared --json` and is no role, so the `drive add` command line in the debug log and listings print it. `suiteDriveRoles` is set once in `TestMain` and read by parallel tests, so the fix is to resolve the shared folder fixtures' names in `TestMain` and add them to the roles. (2) `e2e/testenv_test.go` near `:311` and the other `testutil.TokenFileName` callers: the printed-value flow does not follow a call that builds a name from an identity, so such a value is unseen by the rule (the one print found is fixed by hand). Fix: the rule treats a `testutil` function given an identity as returning one.
- **Rider (2026-11-15, from the third pass of W47, "Seen and not fixed" 3)**: the cost of the e2e masks is not measured on a real debug log. `maskIdentifierPositions` in `testutil/idmask.go` (the regex masks for the selector, the throttle drive and the restore argument, through `maskGroups` and `ReplaceAllStringFunc`) took about 1.1 s per megabyte of text in which every line holds a marker, by the writer's measurement on this host; the shape scans added about 0.06 s. Measure one full debug log of a nightly e2e job (size, and the time the mask takes on it); if it is more than a few seconds per job, replace the three regex masks with scans of the kind `maskURLs` uses, held by the existing mask tests. No product change.
- **Rider (2026-11-15, from the third review of W47, "Seen and not fixed" b)**: when the ID-shaped lines of `spec/reference/graph-api-quirks.md` and `spec/reference/live-incidents.md` are replaced by invented ones, also make `TestE2E_TrackedFilesNameNoTestAccount` fail on a Graph drive ID of the test catalog in any tracked file, in every spelling (raw, lower case, percent-encoded), so the two files cannot regress. The reviewer counted 7 and 17 such lines and did not check whether they are live.

#### W62. Pure tests of e2e helpers run in the quick tier

- **Source**: the review of W47 on 2026-11-15.
- **What is wrong**: tests that need no live account sit in files tagged `e2e`: the unit tests in `e2e/shared_items_helpers_selection_test.go` and `e2e/timing_helpers_test.go` (for example `TestE2ERecordersRecordRolesNotDrives`). They run only in the PR's e2e job and in the nightly, behind a `TestMain` that touches the live account, so a broken helper shows late and costs a live run; a local writer who must not run tagged tests cannot run them at all.
- **Change**: those tests and the pure helpers they call move to untagged files of the same package, beside the rule tests (`e2e/printed_value_rule_test.go`), so `go test ./e2e/` and the quick tier run them. No helper changes behavior. The DoR lists every test in a tagged file that calls no CLI runner and reads no `TestMain` state, and moves all of them.
- **Order**: after W47 (it rewrites the same files). Lane x. Size S.
- **Rider (2026-11-15, from the second review of W47, its notes 9 and 13)**: every mask in `testutil/idmask.go` that follows a product text is held to the product's source by a test that reads that source, as the error-format list is, so a reworded product message fails a test instead of printing an identifier. And the e2e sites that were fixed by hand because the printed-value rule cannot see them (the rule's header lists its blind spots) get a test or a rule extension each, or are named as accepted in the header.

#### W63. A watch user can confirm held deletes

- **Source**: seen on 2026-11-15 after the install of #918. The watch client held 215 deletes (the tree a live proof had uploaded and removed). `status` said: run `onedrive-go sync --confirm-deletes`. That command answered that another sync is already running. The help of the flag says "one-shot only". The watch had to be stopped by a signal before the confirm run could work.
- **What is wrong**: the action the issue names cannot be taken in the state the issue is shown in. A user who runs the watch as a service has to stop the service, run a one-shot sync (which also does everything else a sync does) and start the service again, and nothing tells them so.
- **Change** (the DoR writes down the alternatives and chooses): the target is that the confirm reaches the running watch through the control socket, as `pause` and `resume` do, and the watch applies the held deletes. The confirmation names what it confirms (a count or a token of the held set), so a delete that arrived after the user looked is not confirmed unseen. Changing only the issue text and the refusal so that they say what a watch user does is the least acceptable outcome and is not the target.
- **DoR first**: how held deletes are stored and released today (files and functions); what the one-shot flag does with them; what the control protocol offers (`internal/cli/control_request.go` and its server); whether a confirm from a stale view can delete more than the user saw, since the held set can grow between `status` and the confirm; what `status --json` reports about the held set.
- **Tests first**: a watch with held deletes takes the confirm through the socket and deletes exactly the held set; a confirm that names an older set than is held now follows the DoR's rule (refused, or applies only what it named); the texts of the issue and of the refusal. One tagged e2e test with a watch: hold, confirm, converge.
- **Live proof (mine)**: with the installed watch and the test account, upload and remove a tree larger than the threshold, confirm through the new path, and read `status` as up to date without stopping the watch.
- **Order**: no row must come first. Not at the same time as another branch that changes the control protocol. Sync lane. Size M.

#### W64. A local read that times out is retried as busy

- **Source**: W48's second-pass report ("Seen and not fixed" 1), 2026-11-15. From the writer's reading; nothing was run.
- **What is wrong**: `internal/driveops/local_errors.go` near `:58` classes ETIMEDOUT and EAGAIN of a local read as `LocalErrorOther`. A file on a network mount or a FUSE folder whose read times out is then failed as an unknown local error, while ESTALE from the same mount is `LocalErrorBusy` and retried.
- **Change**: both map as ESTALE does (`LocalErrorBusy`, retried).
- **DoR first**: the table of every errno the classifier names and its class; where a local read's errno enters (hashing, the upload read, the scan) and wrapped in what; what the engine does with a busy local error and with an unknown one (retry bound, issue text a user sees); whether EAGAIN can come from a regular file at all (`localpath.OpenRegular` opens with `O_NONBLOCK`) and, if it can, whether a retry at once is right or the read should wait.
- **Tests first**: a table row per errno, wrapped as it arrives (`*fs.PathError`); the engine's decision for each.
- **No live proof is possible**: no fault-injecting mount on this host.
- **Order**: no row must come first. Not at the same time as W66 (same file). Lane x. Size S.
- **Rider (2026-11-15, from the second review of W48, note 4; from reading, not proven)**: a local disk that hangs, rather than failing, ends a transfer as `graph.ErrTransferStalled`: the stall watchdog runs while a request body is being written and while a response body is being read (`internal/graph/transfer_watchdog.go` near `:28-33` and `:166-176`), so an upload whose local file read blocks, or a download whose local write blocks between body reads, stalls the same way as a silent service. Since W48 that error counts in the service window (`internal/sync/domain_graph_error.go` near `:182`), against the rule that a failure of the local disk is never outage evidence. Fix in `internal/graph`: the watchdog records whether the stall fired inside the body's local read or write, the error says so, and the sync classifier keeps such a stall out of the window (per-file retry, as on main before W48). Test: a body whose local side blocks past the stall bound leaves no evidence; one whose network side blocks does.

#### W65. A stopping run records its completions on a detached, bounded context

- **Source**: W48's second-pass report ("Seen and not fixed" 2), 2026-11-15. From the writer's reading; nothing was run.
- **What is wrong**: `internal/sync/engine_runtime_shutdown.go` near `:17` (callers `engine_run_once_results.go` near `:208` and `engine_watch_drain.go` near `:92`): a stopping run applies every completion it drains on the context of the run that has just ended. By reading, each store write for a result that really happened (an upload finished, a file moved) can then fail at once with a canceled context. What the next run has to repair after that is not checked. W48 gave the executor's own records a detached, bounded context (`runDetachedStoreAccess`, `OutcomeCommitTimeout`); the drain does not use it.
- **Change**: completions drained during a stop are applied on a context detached from the run's cancel and bounded by the same helper, so a stop loses no result that already happened and a store that does not answer ends the drain at the bound with the store-failure error. A drive's stop stays inside its 45 s limit: the DoR adds up the bounds.
- **DoR first**: what each caller applies and on which context; a test with a store that honors its context, showing what is lost today; how many completions a drain can hold and the worst-case time; how that fits the stop limit; what the next run does today with a result the store never got (so the report can say whether this was a loss of work or only a repeat of it).
- **Tests first**: a run canceled with completions in flight records each of them; a store that never answers ends the drain at the bound; the stop limit still holds.
- **Live check (mine)**: the kill-mid-transfer e2e test, once.
- **Order**: after W48 merges (it adds the helper and the bound). Sync lane. Size M.
- **Rider (2026-11-15, from the second review of W48, note 10)**: `TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid` (`internal/sync/request_timeout_outage_test.go` near `:640-648`) rests on the stop's write failing on the ended context. Once the stop writes on a detached, bounded context, that write succeeds and no Warn is emitted: the test must change with this row (hold instead that the completion IS recorded, and keep one case in which the detached write itself outlasts its bound and the loss is said).

#### W66. An error names a local path once

- **Source**: W54's brief (its rider c) and W54's first report ("Seen and not fixed" 5), 2026-11-15. The first writer found the brief's pointer to `transfer_upload.go:209` wrong and that the doubled path comes from `internal/localpath`, which W54 does not touch.
- **What is wrong**: `internal/localpath` returns errors that already hold the path (the `*fs.PathError` of the failed call). `internal/driveops` wraps them with the path again (`transfer_upload.go` near `:243` and `:274`, `hash.go` near `:43`; the DoR finds every site), and the folder form of `put` prints `Failed: <path>: ` in front of a cause that names the same path once or twice more.
- **Change**: the path is named once in what a user sees: by the layer that prints it for a folder transfer, by the error itself for a single file. The cause keeps its operation word (`opening`, `hashing`, `reading`). One rule for where the path enters an error, written in `spec/design/drive-transfers.md`, and the wrap sites follow it. Sync's failure text is unchanged unless it too names the path twice.
- **DoR first**: every wrap site and every caller that prints the error (single `put`, folder `put`, `get`, sync's upload and download), each with the text a user sees today; which layer owns the path in the error; whether a new exported `localpath` function is needed (if so it goes on the declared lists in `internal/sync/executor_rooted_mutation_test.go`).
- **Tests first**: the tests that hold a failure text say the new text; one test per printing caller that the path occurs once.
- **Live proof (mine)**: a folder `put` with one unreadable file in a small tree: the failure line names the file once.
- **Order**: after W54 merges (same files). Not at the same time as W64. Lane x. Size S to M.
- **Rider (2026-11-15, from W54's second pass, "Seen and not fixed" 5; from reading)**: `internal/cli/put.go` near `:134` (`runPut`, the `Uploaded %s` line): the text line of a single put names the remote path as typed, while `--json` prints `putJSONPath` (one leading slash, cleaned). Decide one form for both and hold it with a test; the spec sentence in `spec/design/cli.md` on the JSON path form then covers the text line too.

#### W67. A replacing local move that finds a new file at the freed name still moves

- **Source**: W42's second pass (its item 5, where the writer stopped as the brief said, and "Seen and not fixed" 1), 2026-11-15. The writer's failing scenario test is kept as `reports/W67-freed-name-test.go.txt`. Its run is the writer's; I have not run it. Whether main fails the same way is checked before W42's gate and noted in W42's report.
- **The case**: OneDrive moved item a onto b's name, replacing b. During the local replacing move, after b went to the trash and before a moved, a new file was saved at b's name. That pass supersedes the move and retries, which is right: local a.txt keeps its content, b.txt is the saved file, the old b.txt is in the trash once. The next pass makes no move: it keeps the saved file as a conflict copy and downloads item a into b.txt. The pass after uploads two files: the conflict copy, and a.txt as a new item. Nothing is lost and nothing is deleted on OneDrive. But OneDrive holds a.txt again after it moved that item to b.txt, and the moved item was downloaded beside its old copy. Both contradict R-2.17.9, which is marked verified.
- **Where, by the writer's reading**: `composedDisplacement` in `internal/sync/planner_move_composition.go` (near `:1148-1163` on W42's base) answers never, because item b's local position no longer matches its baseline path (the file at b.txt is not the baseline item). No move is composed and the remote move is planned as a download.
- **Change**: when the file at the destination is not the displaced baseline item, it is kept as a conflict copy and the local move of the source is still composed, so nothing is downloaded or uploaded again.
- **DoR first**: confirm the plans of each pass with the scenario; say what main does (the replacing move itself is older than W42); check whether W52 or W53 changes the same function, and if one does, say which goes first; the comparison row or rows this adds, laid out as a table.
- **Tests first**: the kept scenario, failing on the unchanged tree; a composition-table row; R-2.17.9 cited by the scenario.
- **No live proof is possible**: the window between the two steps of a local move cannot be hit on purpose without a hook. The scenario test is the proof.
- **Order**: after W49 and W42 merge (same files). Sync lane. Size M.
- **Rider (2026-11-15, from W42's second review, note 8, and its item 8 result)**: the check owed above is done, by the writer, on an export of main `341d9820` with the kept test: main cannot reach the state at all. It has no crash point between clearing the destination and the rename, and the name is never free (with a trash main links or copies the old b.txt into the trash and then renames over it; without one it renames over it). By reading, not by running: on main a file saved at b.txt between the preserve and the rename is replaced by the moved file and its content is lost. So the fault of this row exists only since W42, which keeps that file. R-2.17.9 in `spec/requirements/sync.md` is marked [verified] and its clauses "rather than downloaded beside its old copy" and "shall not be uploaded again" are not true of this case until this row lands: this row sets the requirement's status honestly in its own change (down while the fault stands, or verified with the test that holds the fix).

#### W68. A file moved out of a subfolder into the folder re-made at the old path, with the emptied subfolder removed, converges

- **Source**: the second review of W49 (its note 4), 2026-11-15. The reviewer's scenario runs, kept as `reports/W68-review-scenarios-1.go.txt` and `reports/W68-review-scenarios-2.go.txt`; I have not run them.
- **The case**, all on this computer between two syncs: `mv docs archive; mkdir docs; mv archive/sub/inner.txt docs/inner.txt; rmdir archive/sub`. On main and on W49's commit alike the plan is a dependency cycle: three syncs each leave work, the planner logs a dropped cycle (12 lines over the three), nothing moves on OneDrive, nothing is deleted. With a new file added on OneDrive below the subfolder the same steps converge on main in one sync (W49's third pass holds that with a scenario).
- **Where, by the reviewer's reading**: the delete of a folder that exists on one side only, below a folder that is moving, is addressed at its baseline path while the move's dependencies read it at the post-move path (the same double reading W52's rider names for `underMoveDestination`).
- **Change**: the delete of a one-sided folder below a moving folder is addressed, or ordered, where the folder move puts it, so the plan has no cycle and one sync converges.
- **DoR first**: the plan of the case as the planner prints it, with the cycle's edges; whether W52's rider fixes this as a side effect (then this row closes into W52 with its scenario); the bound on syncs.
- **Tests first**: the scenario, failing on the unchanged tree, registered with the other recreate scenarios; a dependency-table row for the cycle's edges.
- **Live proof (mine)**: the four steps in a private sync folder with the test account, one sync, both trees equal.
- **Order**: after W49 merges (same files); before or with W52. Sync lane. Size M.
- **Rider (2026-11-15, from the third pass of W49, "Seen and not fixed" 1)**: `internal/sync/planner_move_composition.go`, `vacatingFolder` (the return at a relocated folder). Made on OneDrive, `mv docs/sub docs/sub2; mv docs archive; mv archive/sub2/inner.txt docs` (a file moved ONTO the vacated path itself, through a renamed subfolder) is a dependency cycle on every pass, on main `341d9820` and on the W49 commit alike (3 syncs, each leaves work; cycle of `local_move docs->archive`, `local_move docs/sub/inner.txt->docs`, `local_move docs/sub->docs/sub2`; nothing lost). The dependency exception needs a folder created at the vacated path, so it cannot order this. Fix proposed by the writer: let the walk go on past a relocated folder the target does not land below, so composition holds the local move one pass. Hold it with a scenario that takes one sync. The same steps made on this computer already converge.
- **Rider (2026-11-15, from the third review of W49, note 6; from reading, not run)**: the class is wider than the first rider says. A file moved on OneDrive into a folder OneDrive moved onto the vacated path, through a renamed subfolder (`mv docs/sub docs/sub2; mv docs archive; mv photos docs; mv archive/sub2/inner.txt docs/inner.txt`), has no folder create at `docs`, so the dependency exception does not apply: the local move of `docs` waits for the file's move, which waits for the move of `photos` onto `docs`, which waits for the move of `docs`: a cycle on main and after W49. The walk-on change the first rider names covers it; this row's scenario holds both shapes.
- **Rider (2026-11-15, from W49's fourth pass, "Seen and not fixed" 1)**: the same function (`vacatingFolder` in `internal/sync/planner_move_composition.go`, the return at a relocated folder) leaves three more shapes as a cycle on every pass, on main and on W49's commit alike: the "onto" case through a renamed subfolder, the shape of the third review's note 6, and a subfolder that moves out of the moving folder (`mv x x2; mkdir x; mv x2/s y/s; mv y/s/t.txt x/t.txt` on this computer; 10 of the 39 sequences that fail on both in `reports/W49-differential.md`). The writer's candidate: walk on past a relocated folder the target does not land below, and hold the move a pass when the target lies at or below a farther folder's vacated path. The differential harness (`reports/W49-differential-test.go.txt`) is the acceptance test: these 10 must converge and no sequence that converges before may stop converging.

#### W69. A read by path returns only the item at that path

- **Source**: W54's first report ("Seen and not fixed" 6, 7 and 8), 2026-11-15. From the writer's reading; nothing was run.
- **What is wrong**: (1) `internal/graph/items_fetch.go` near `:159` escapes a path segment with `url.PathEscape`, which leaves `:` as it is; in a path-addressed Graph URL a colon ends the path, so a name that holds one is cut. (2) `GetItemByPath`'s fallback near `:113-121` accepts an answer by its leaf name alone, so an item of the same name in another folder passes. (3) `internal/graph/item_create.go` near `:151-164` (`readCreatedFileByName`) lists the whole parent folder to find one name.
- **Change**: (1) a colon in a segment is escaped, or such a name takes the listing route, as W54 decides for its one-name read: one rule for both. (2) the fallback compares the answer's parent, by ID where the caller has one and by path otherwise. (3) `readCreatedFileByName` uses the one-name read W54 adds.
- **DoR first**: what Graph answers for a name with a colon on each drive kind the test accounts have (my live question, asked through the CLI; the answer is written without any ID); every caller of `GetItemByPath` and what a wrong item would make it do (the worst case decides the size of this row); the largest folder `readCreatedFileByName` lists today and how often.
- **Tests first**: fake-server tests for the three: the colon name, the same leaf under another parent refused, one request for the created file's read.
- **Live proof (mine)**: a `stat` and a `put` of a name with a colon if the drive allows one; a file create whose answer is lost cannot be provoked.
- **Order**: after W54 and W56 merge (same files). Lane x. Size S to M.
- **Rider (2026-11-15, from W54's second pass, "Seen and not fixed" 4; from reading)**: `internal/graph/items_fetch.go` near `:78` (`GetItem`) and `:240` (`ListChildren`): the item routes put the item ID into the route unescaped, while the path routes escape it (the parent ID goes out with `!` as `%21`). One rule for an item ID in a route, held by a test that reads the request line for an ID holding `!`.
- Rider (2026-11-15, from the paged-listing writer's "Not fixed (seen)" 1 and 2; from reading, nothing was run): two more lookups list a folder whole to find one name. (a) `lookupUploadTarget` in `internal/driveops/transfer_verify.go` (near `:282`) lists the parent for a first-match lookup after an upload. (b) `resolveItemFromMountRoot` in `internal/driveops/session.go` (near `:836`) lists every folder on the path whole to find one name per segment. Change: both use the one-name read (`GetChildByName`), with the paged listing, stopped at the first match, as the fallback where the read cannot settle it, the way `FindExistingChild` does. Tests first: per lookup, a fake that counts requests shows one read per name and no listing when the read answers; the fallback case stops at the page that holds the name. DoR first: every other caller in `internal/driveops` and `internal/cli` that lists a folder to find one name (file-name-only grep for the slice forms, then read each), with file and line.
- **Rider (2026-11-16, the live question of the DoR, answered by me)**: on the personal test account, with the installed client (main at #933), in a private folder removed afterwards: a `stat` and a `put` of a path whose last name holds a colon each exit 1 with HTTP 400 BadRequest "Resource not found for the segment" (the path is cut at the colon and the rest is read as a route segment); nothing is created; a `mkdir` of a name with a colon exits 1 with HTTP 400 invalidRequest "The provided name contains invalid character ':'". So a personal drive holds no name with a colon. Decision for change (1): the by-path read takes the one-name read's rule, `ErrNameNotPathAddressable` before any request, and gets no listing fallback. Not known: a Business or SharePoint drive (the test accounts on this host are personal).

#### W70. Status warns about a default sync folder that meets a managed folder

- **Source**: W42's second pass ("Seen and not fixed" 3), 2026-11-15. From the writer's reading; nothing was run.
- **What is wrong**: after W42, `sync` refuses a drive whose sync folder holds, or lies inside, one of the client's own folders, whether the folder is configured or the default. The lenient load (`internal/config/load.go` near `:153`), which `status`, `drive list` and `shared` use, warns only about a configured `sync_dir`, because only `ValidateResolvedForSync` sees defaults. A user whose default sync folder meets a managed folder sees no warning until `sync` exits 78.
- **Change**: the lenient load runs the same one-path check on each drive's effective sync folder when it collects warnings. One check, two callers.
- **DoR first**: where the effective sync folder is resolved and whether the lenient load has it; the warning text beside the refusal text (same words for the same fact).
- **Tests first**: a config with no `sync_dir` whose default meets a managed folder: the lenient load returns the warning, `status` prints it; the configured case still warns once, not twice.
- **Live proof (mine)**: `status` in a private config tree whose data directory lies under the default sync folder.
- **Order**: after W42 merges (same files). Lane x. Size S.

#### W71. An e2e assertion on what a sync did reads every attempt of its converge loop

- **Source**: the nightly of 2026-11-15. `TestE2E_Sync_IncrementalDeltaToken` failed at `e2e/sync_recovery_e2e_test.go:88`. Read from the job log: the first download-only attempt downloaded and named the new file but ended with an error from unrelated churn on the drive; the second attempt was clean and had nothing left to name; the assertion reads only the converged attempt's output. That one test is fixed in W47's PR.
- **What is wrong**: the same shape (a converge loop, then an assertion that the last attempt's output names an action) is at other sites. Counted by pattern on main `341d9820`, not each read: `sync_recovery_e2e_test.go` 7, `sync_full_test.go` 6, and 1 each in `sync_shortcut_e2e_test.go`, `sync_e2e_test.go`, `sync_edge_cases_full_test.go`, `sync_shared_e2e_test.go`.
- **Change**: the converge helper keeps the output of every attempt; an assertion about what the sync did reads all of them, an assertion about the end state reads the last. A source check in the quick lane (beside the one #911 added) fails an action assertion on a converge result's last output.
- **DoR first**: every site, what it asserts, and whether an earlier attempt can have done the work; the helper's new signature.
- **Tests first**: the source check's own test with one fixture it must fail and one it must pass.
- **Live proof (mine)**: the touched tests, one at a time.
- **Order**: after W47 merges (same files). Lane x. Size M.

#### W72. A seeded model of move steps holds that every plan converges

- **Source**: W49. Three reviews each found, by reading, a set of renames, folder moves, re-made folders and file moves that converged on main and no longer converged on the commit. The fourth pass ran a differential enumeration in scratch; its test file is `reports/W49-differential-test.go.txt` and its counts are in `reports/W49-differential.md`.
- **What is wrong**: nothing holds by machine that a set of move steps made on one side converges; the recreate scenarios are picked by hand, and each new planner rule is checked against them only.
- **Change**: a fifth seeded model family beside the four `verify seeds` runs: a small tree, a seeded sequence of steps on this computer, on OneDrive or both, then syncs up to a bound; both trees are equal, or the sequence matches one of the shapes the spec names as still dropped. Nightly start seed from the date like the others; a failing seed gets its issue and its replay line.
- **DoR first**: the differential file and its counts; what a predicate for the named shapes needs; the run time per seed on 2 cores.
- **Tests first**: the family with a short fixed seed list in the quick lane; one deliberately broken planner rule makes a seed fail (seen once, then put back).
- **Order**: after W49 merges; W52, W53 and W68 each remove a named shape from its list. Lane s. Size M.

#### A20. The stress lane's race runs of the sync tree take their deadline from the race profile

- **Source**: the nightly of 2026-11-15. The `stress` job failed in its step `sync race x3`: `go test -json -race -count=3 -shuffle=on -timeout=40m ./internal/sync/...` ended at go test's 40-minute alarm. The tests running at that moment were 3 and 4 seconds old, so nothing hung. Measured: the same step took 31.3 minutes on 2026-11-14 (main `2b1c5afa`); the `verify-race` job (one race run of the tree, plus setup) took 12.2 minutes on `04983d12` and 15.8 minutes on `341d9820`. Why one run grew: not measured.
- **What is wrong**: the step's deadline is the literal `40m` beside a count of 3, chosen when three runs took 31 minutes. The race profile has its own measured deadline for one run (`raceSyncBaseTimeout`, by cores), and nothing ties the two. A suite that grows by a quarter fails the night by budget, and the nightly reader names the two tests that happened to be running as the failures.
- **Change**: one constant for the number of race runs of the tree; the step's `-count`, its name and its deadline (the runs times the race profile's deadline for the host's cores) come from it. The comments' measurements are corrected. The `stress` job's `timeout-minutes` covers the new sum, held by `TestCIJobTimeoutsCoverEveryGoTestDeadline`.
- **Tests first**: the step's deadline is the runs times the race profile's deadline; the job-timeout test fails before the workflow file changes.
- **Order**: at once: the night stays red until it merges. Lane o. Size S.

#### W73. A name the path route does not resolve is found through its parent's listing in every command

- **Source**: live checks of 2026-11-15 on the test account (a Personal drive) with the installed main `341d9820`, made for the review of W54 (logs `W54-live1.log` to `W54-live4.log` in the scratchpad; helper `w54live.py`, modes `names` and `files`).
- **Measured, lowercase `v1.0` prefix**: a file or folder whose name starts with `v1.0` (`v1.0`, `v1.0x`, `v1.0.zip`, `v1.0.3`) exists and is listed, yet every path-addressed read of it or through it answers 404. `stat`, `get` and `ls` exit 1 with "was not found on OneDrive". `put`, `rm` and `mkdir` exit 0 because each falls back to the parent's listing. A one-shot sync of a folder holding such names exits 0 (it addresses by ID).
- **Measured, the name `$value`**: a path-addressed read of a file or folder named exactly `$value` answers HTTP 400 "Invalid request". `put`, `stat`, `get` and `rm` exit 1 with the raw Graph error. `mkdir '$value'` creates the folder (201) and then exits 1 in its visibility wait. A one-shot sync of a folder holding a file of that name exits 0.
- **Measured fine (200)**: `V1.0`, `v2.0`, `v2.0.zip`, `beta`, `$plain`, `$count`, `$ref`, `$batch`, `$value.txt`, `$value.d`, `it's`, `50% off`, `a#b`, `children`, `content`, `delta`, `root`, `items`, `me`, `drive`.
- **Unknown**: why Graph answers so; whether a Business drive does the same; whether other names share the fault.
- **What is wrong**: path resolution trusts the path route's 404 and gives up on its 400. The fallback to the parent's listing exists in separate places (the lookup after a 409, the visibility wait on a 404, `rm`), each with its own rule, and `stat`, `get` and `ls` have none.
- **Change**: one rule in one place for "resolve this remote path". A segment the route is known not to resolve (the two measured shapes, beside the existing rule for a colon and for length) is never path-addressed: its parent is resolved, listed once, and the walk goes on by ID. A 400 from a path read falls back the same way. Every command that resolves a remote path goes through it, and so does the visibility wait. Sync's own path reads (the sync root, `included_dirs`, a shared folder's root) are read and use it where they path-address. The quirk enters `spec/reference/graph-api-quirks.md` with the measured table and its date.
- **Tests first**: a fake Graph that answers 404 for a path with a `v1.0` prefixed segment and 400 for `$value`, as the live service does. Per command (`stat`, `get`, `ls`, `rm`, `put`, `mkdir`): such a name at the leaf and in the middle of the path succeeds. A genuine miss still costs one request and says "not found".
- **Live proof (mine)**: `w54live.py BINARY LABEL files` and `names`: every line exits 0, and `mkdir '$value'` exits 0.
- **Order**: after W54 (the same files: `internal/driveops/session.go`, `internal/graph/items_fetch.go`). Lane o. Size M.
- **Rider (2026-11-15)**: not at the same time as W69: both change the same function in `internal/driveops/session.go`. Whichever starts second begins on the other's merged commit.
- Rider (2026-11-15, from the second review fixes of W54): the CLI Graph fake (`internal/cli/json_contract_fake_graph_test.go`, the root item near line 88) gives the drive root the alias `root` as its item ID, so no CLI test passes through the rule that places an answer for the root alias by its parent path (held only in graph, by `TestGetChildByName_PlacesAnAnswerForTheRootAliasByItsParentPath`). Give the fake root an ID of its own and answer the alias for it, so every CLI test of a name at the top of the drive goes through that rule; fix what the change shows.

#### A21. The stress lane's fifty-times step has declared members and a budget

- **Source**: the review of A20 on 2026-11-15, from the step times of five CI runs. The stress step `sync concurrency race x50` passed that night in 39.4 minutes against a fixed 40-minute deadline; A20 raised the deadline to 80 minutes as a measured number and left the cause open.
- **What is wrong**: the step picks its tests by name substrings (`stressSyncConcurrencyPattern` = `TestRunWatch|TestWatch|TestOneShot|TestWorkerPool|Shutdown|Drain|Concurren|Cancel`), so any new test whose name holds "Drain", "Shutdown" or "Cancel" joins a fifty-times race run without anyone deciding it, and nothing bounds what one pass may cost. The step's time grew 3.4 times in two days while the selection grew from 214 to 244 top-level tests.
- **Evidence** (step times from the public CI runs; ids are run ids of this repository):

  | Run | head | multisync x50 | sync concurrency x50 | sync race x3 | total |
  |---|---|---|---|---|---|
  | 11-13 scheduled `43473280688` | 27e76cc3 | 4m43 | 11m32 | 26m17 | 42m32 |
  | 11-14 scheduled `43615207680` | 2b1c5afa | 11m33 | 23m58 | 31m17 | 1h06m48 |
  | 11-14 dispatch `43644281830` | dd0932f4 | 10m19 | 20m05 | 27m23 | 57m47 |
  | 11-15 dispatch `43687851252` | c867b645 | 11m45 | 29m32 | 34m03 | 1h15m20 |
  | 11-15 scheduled `43739908483` | 341d9820 | 11m57 | 39m23 | fail at 40m02 | 1h31m22 |

  Selected top-level sync tests: 214 at 27e76cc3, then 229, 235, 244, 244 at 341d9820, 245 at 12a48bb2. One local race pass of the selection at 12a48bb2: 245 tests, 739.0 s summed, package wall 593.8 s; the 31 tests selected now and not on 11-13 take 277.3 s (37.5%), many of them picked up by the substrings alone (the `TestRunOnce_FinalDrain...` family for one); 88 tests of 1 s or more make 693.2 s. `multisync x50` took 76.7 s in April 2026 (`LI-20260412-04`), 4m43 on 11-13 and about 11.5 minutes since 11-14.
- **Unknown, to measure and not to guess**: why `multisync x50` more than doubled between 11-13 and 11-14; why the same 244 selected tests took 29.5 and 39.4 minutes on the two runs of 11-15.
- **Change**: the step's members are declared (a list or a marker a test checks against the source, in the way the other verify profiles hold their membership), not matched by substring; a test fails when a member is slow enough to break a stated budget for one pass, or the deadline is derived from that budget and the repetition count as A20 did for the whole-tree step; `multisync x50` gets the same treatment once its growth is measured. The 80-minute literal goes when the budget exists.
- **Tests first**: a new sync test named with "Drain" does not join the step unless declared; a member over the budget fails the check; the job-timeout test still covers the derived deadlines.
- **Order**: after A20 (it edits the same constants and tests). Lane o. Size M.

#### A22. The nightly issue reads each red job's log, and a go test alarm keys only the test it names

- **Source**: the second rider of A17 (2026-11-15, from the first nightly that posted its verdict), which the A17 change left out; the review of A17 named the gap.
- **What is wrong**: (1) the `nightly-failure` issue said of each red job "failed; its log could not be read". Why the report job could not read the logs is unknown: find it first (a missing permission, a log not yet available when the job runs, or the reader's own error path). (2) The reader that keys failing tests takes a test that was merely running when a `go test` alarm fired as failed: the stress lane's timeout produced two such keys.
- **Change**: the report job reads the logs it judges (or says exactly why it could not, per job); for an alarm, key only the test the alarm names as the one that hung, or key the alarm itself, and say which in the issue.
- **Tests first**: a fixture of a timed-out `go test` log with several running tests yields one key; the report job's read failure is held by a test of the cause found.
- **Order**: after A17 and A21 (the same reader and verify files). Lane o. Size M.
- Rider of 2026-11-16 (from the review of the stress lane's declared members): the nightly reader gives a member over its budget its own key. Today `nightly_verdict.go:161-163` keys a go test step by its marker only when the job's log names no failed test, so an over-budget member on a night with a test failure gets no key and nobody claims it. Smallest correct fix: a printer and reader pair for the budget line, like `failedStepMarker` and `readFailedStepMarker` (`verify_summary.go:24-51`), printed on stderr by the step; the reader keys it as that member's test key whatever else failed. Test first: a job log with one failed test and one budget line yields two keys.

#### W74. The crash model checks every empty file of a converged tree against a declared allowance

- **Source**: the review of W50 on 2026-11-15 (its note 9) and W50's own "seen and not fixed" list.
- **What is wrong**: the stray-artifact invariant (`internal/sync/sync_invariants_test.go`) looks only at conflict-copy names. A lost claim at an ordinary name, an empty file plus a conflict copy after a plain crash, passes every invariant; the every-crash-point test asserts nothing beyond the invariants.
- **Change**: every empty file of a converged tree, on either side, is checked against an explicit allowance: the scenarios that sync zero-byte files on purpose declare theirs; the lost-WAL allowance of W50 stays keyed by item.
- **Tests first**: a planted recovery that leaves an empty file at an ordinary name fails the invariant; the zero-byte scenarios pass with their declared files and fail without the declaration.
- **Order**: after W50 and W51 (the same test files). Lane x. Size M.

#### W75. Each test that cites the executor-returns-results requirement tests it

- **Source**: the review of W50 on 2026-11-15 (its note 10) and W50's "seen and not fixed" list.
- **What is wrong**: several tests cite R-6.8.9 without testing either of its sentences; the review found two in `dispatch_order_test.go` (reverse dispatch order) and did not check the citations outside `worker_test.go` and `engine_config_test.go`.
- **Change**: read every citation of R-6.8.9; move each that tests another requirement to that requirement, and leave the ones that test the executor's results or the engine's classification and retry scheduling.
- **Tests first**: none new; the requirement-traceability rule passes before and after.
- **Order**: any time; test comments only. Lane x. Size S.

#### W76. A paged listing that gathers a slice fails on a next link it already followed

- **Source**: the review fixes of W55 on 2026-11-15 (its report, "Other paged loops in this commit").
- **What is wrong**: the children listing fails on a next link back to a page it already asked for, but the four loops that gather a slice (`fetchAllDrives`, `SearchDriveItems`, `ListItemPermissions`, `SearchSites`) do not: a circular link costs the page cap's requests and up to the cap's pages in memory before the first three fail, and `SearchSites`, which stops at the caller's limit, returns the same site more than once, up to the limit (`TestSearchSites_StopsFollowingAtTheLimit` serves exactly such a link and passes).
- **Change**: one shared repeat check for every non-delta paged loop of `internal/graph` (the children loop's check moves into it, no second pattern), with the same error text; `SearchSites` returns each site once. Say in `spec/design/graph-client.md` that the rule holds for every non-delta listing. The delta loop is out of scope unless the DoR shows the same hole there: then say so and stop.
- **DoR first**: every paged loop of `internal/graph` with file and line, what each does with a repeated link today, and whether the delta loop can be handed a link it already followed.
- **Tests first**: one table over the four loops, a self-link and a two-page cycle each, seen failing on the base with the cap's error or a repeated result; the site search case with a repeating link returns each site once or fails, by the decision taken in the DoR.
- **Order**: after W55 (it owns the children loop). Not with W58 or W69 (same package files). Lane x. Size S.

#### W77. Multisync and cli tests start and stop a watch only through bounded helpers

- **Source**: the review of W35 on 2026-11-15 (its findings, answer 9 item 2, and finding 4).
- **What is wrong**: `internal/sync` tests start a watch only through `runWatchInBackground` and read results with `receiveWithin` or `stopWatch`, so a failing test never leaves a watch running into the next. `internal/multisync` has no such helpers: about 25 tests start `RunWatch` on a bare goroutine and about 70 select blocks repeat the same bounded wait by hand. `internal/cli` has the same shape at a few sites. After W35 every such wait is bounded, but by hand, and a test that fails early leaves its watch running.
- **Change**: twins of `runWatchInBackground`, `receiveWithin`, `stopWatch` and `runWithin` in the test helpers of `internal/multisync` and `internal/cli` (same names and messages as the `internal/sync` ones; no shared test package unless the DoR shows one already exists); every test that starts a watch or loop on a goroutine moved onto them; the repeated select blocks folded into `receiveWithin`. Then `spec/design/system.md` says the helper rule for all three packages in one sentence again.
- **DoR first**: every test in the two packages that starts a goroutine, with file and line, and which helper it moves to; every hand-written select with a timer arm; the sites that stay as they are, with the reason.
- **Tests first**: test code only. For each helper one test that it fails naming what it waited for (as the `internal/sync` helpers are tested, if they are; otherwise for all three packages).
- **Order**: after W35. Not with W63 or another branch in `internal/multisync` tests. Lane x. Size M.
- Rider from the W35 review fixes (2026-11-15), all three fixed in this row: (1) direct own calls of a watch that are not bounded: `internal/multisync/panic_boundary_test.go` near 210 and `internal/cli/sync_notify_test.go` near 251 (both in a child process), `internal/multisync/orchestrator_test.go` near 2958, 3048 and 3678, `internal/multisync/watch_lifecycle_test.go` near 123 (check for each whether it returns at once; the rest go through the `runWithin` twin); (2) the `eventWait` arms of `internal/multisync/orchestrator_test.go` fail with `require.Fail`, which does not stop the test, so a failed wait runs on into the next one (near 1870, 2602, 2827, 3151; the last then blocks on a channel read): they become `FailNow` or go through `receiveWithin`; (3) `internal/multisync/owner_lock_test.go` near 52: `wg.Wait()` for two control-server starts is unbounded: a done channel read within `eventWait`. When all direct own calls are bounded, `spec/design/system.md` drops the "In `internal/sync`" limit from the own-call sentence.

#### W78. A source check holds the test wait rules

- **Source**: the review of W35 on 2026-11-15 (its answer 6): no test in the tree holds the wait rules by reading source, for `internal/sync` either.
- **What is wrong**: the rules of `spec/design/system.md` (no absence proved by a fixed wait; one named bound for an event that must come; no unbounded wait for a watch) are held by review alone. A new test with `time.Sleep`, a testify `Never` or `Consistently`, or a literal in a wait passes every gate.
- **Change**: a check in `internal/devtool/verify_test_conventions.go`, run by the quick lane, that reads the test files: testify `Never*` and `Consistently*` are refused tree-wide; in `internal/sync`, `internal/multisync` and `internal/cli` a literal duration as the wait argument of `time.After`, `context.WithTimeout`, `SetReadDeadline` or `Eventually` is refused, and so is `time.Sleep`, each with a declared-exception list in the check (file and reason) that a test holds against the tree; if it stays small, also a select whose timer arm does not fail the test. An aliased import of `time` or testify is resolved, not matched by text.
- **DoR first**: what the tree holds today per rule and package (counts and the sites that would be exceptions); whether the e2e suite's own source check can share the reader.
- **Tests first**: planted cases for each refused form and for an aliased import, each seen passing the check before the rule exists; the real tree passes.
- **Order**: after W35 and W77 (they remove what it would refuse). Not with A21 or A22 (same devtool files). Lane x. Size M.

#### W79. A move sent to OneDrive into a folder made here waits while OneDrive keeps a synced item at that path

- **Source**: the writer's "Seen and not fixed" 1 and the review of the follow-up to #926 on 2026-11-15 (its Seen and not fixed 1 and note 9).
- **What is wrong**: here, x is renamed x2, a new x is made and f is moved into it; before the sync something takes x2 on OneDrive. By reading, `findTargetCollision` undoes x's local relocation, x's local move back is blocked by the new local x, the create of the new x is dropped, and f's remote move lands in the old x on OneDrive; afterwards f waits on every pass for as long as the collision lasts. Second shape (note 9): two folders each moved here into a new folder made at the other's path plan a cycle of two remote moves and two remote creates that `isolateDependencyCycles` drops on every pass; it never converges. Both are the same on main before the follow-up.
- **Change**: in `planner_move_composition.go` (`recreatedSourceCreates`, `resolve`): hold a remote move into a folder made here while OneDrive keeps a synced item at that path in this pass. A plain mirror of the local rule does not catch it, because after the undo x is already at its target online. For the two-folder shape: park one folder beside its path so the cycle opens. About 10 to 20 lines plus rows for the first; the second is sized in the DoR.
- **DoR first**: both shapes reproduced as composition rows and as RunOnce scenarios on the base, with what each pass plans; whether one rule serves both.
- **Tests first**: composition rows and RunOnce scenarios for both shapes, each seen failing on the base; the property and model tests stay green.
- **Order**: after the follow-up to #926 merges (same functions). Not with W52, W53, W68 or W80 (same file). Sync lane. Size M.
- Rider (2026-11-15, from the fix pass of the follow-up to #926; its finding 3, dependency half, and note 10): (1) `moveDepCandidates` (`planner_dependencies.go`, near 438 to 461) looks up a move's partners by exact path, so on a volume that folds case the pair x -> x2, f -> X/f gets no edge from `moveOrderDependency`. Composition already holds that pair (`occupantAt`), so the edge is a second guard. Index the moves by the `pathNameKey` of their source (beside `vacatedByNameKey`), add the moves under each ancestor's name key, fold the compare, and add the dependency row "into a new X" expecting `{nil, {0}}`. (2) A move into a deeper new chain (on OneDrive: x to x2, folders x and x/sub made, f moved to x/sub/f) is not held by composition: `localFolderHolder` looks only at the direct parent x/sub, where nothing synced sits, so f's move is planned, ordered after x's move, and goes stale on the first pass, every time. Walk up the unsynced ancestors to the first path a synced item holds; one composition row and one RunOnce scenario, each seen needing a second plan on the base. `TestComposeMoves_FolderAMoveLandsInIsNotParkedToBreakASwap` plans `m->Q/z/m`, which is of this shape: change its expectation only if the rule changes what it plans, and say so.

#### W80. A folder moved into a new folder at its own path converges

- **Source**: the writer's "Seen and not fixed" 2 and the review of the follow-up to #926 on 2026-11-15 (its Seen and not fixed 2).
- **What is wrong**: on OneDrive, a folder k is wrapped in a new folder of the same name (k becomes k/k). `localFolderHolder` returns k as its own holder and the plan still renames k into its own subtree: rename(2) refuses with EINVAL, a failure and not a stale precondition, on every pass. Made here, the twin is a create and move cycle that is dropped on every pass. Both are the same on main before the follow-up.
- **Change**: in `planMove`: park k beside its path and defer it, as `parkOneOccupancyCycle` does, on both sides. About 20 to 40 lines with composition and RunOnce rows.
- **DoR first**: both sides reproduced on the base with what each pass plans and what the executor answers; whether W53's rule (a folder moved into its own former child) already covers one side.
- **Tests first**: a composition row and a RunOnce scenario per side, each seen failing on the base.
- **Order**: after the follow-up to #926 merges. Not with W52, W53, W68 or W79 (same file); read W53's change first if it has merged. Sync lane. Size M.
- Rider (2026-11-16, from the writer of the OneDrive-side move order, its "Not fixed" 4): made here, the shape now meets `remoteLandingWaits` in `planner_move_composition.go`: k is its own `folderHolder`, so the move of k and the create of the new k wait for each other. The fix for the made-here side goes through that function; its composition row asserts what `remoteLandingWaits` answers for a holder that is the moved item itself.
- Rider (2026-11-16, from the review of the OneDrive-side move order, its notes 9 and 13): (1) the same shape with a file: a file k moved here into a new folder k at its own path. The holder is the key itself, a pending file, so the move is deferred on every pass; before that fix it was a create and move cycle dropped on every pass. Neither converges: the rows and scenarios of this change name a file as well as a folder. (2) The park of an occupancy cycle on OneDrive: if a parked item held the path of a folder made here, the park vacates the path, no create follows because the item is deferred, and the move into the folder fails once for want of a parent. The review found no user steps that reach it. The DoR of this change either gives the steps (then a row) or the reason there are none (then a comment at the park).

#### W81. A publication's item type has one rule

- **Source**: the writer's "Seen and not fixed" 1 and 2 and the review (answers 5 and 10, note 7) of the folder-replaced-by-a-file fix, 2026-11-15. From reading; the reviewer found no way for a user to reach it.
- **What is wrong**: `actionItemType` (`internal/sync/store_write_baseline.go` near `:273`) takes the first non-file type among remote, baseline and local, so the zero value (file) reads as "unknown". After the fix it decides a publication's type only for a cleanup, whose type `commitDelete` does not use; its other users are `movedItemIsFolder` (`planner_dependencies.go` near `:597`, which reads the baseline first) and `DeleteOutcome` (`executor_delete.go` near `:420`, which feeds no stored value). `TestPublicationMutation_SyncedUpdate_BaselineFallback` (`executor_test.go` near `:3212`) holds that reading with an input the planner never builds. `resolveItemType` (`planner.go` near `:85-94`) reads a tombstone's file type the same way, and no planner view carries a tombstone (`planner_actions.go` near `:762`).
- **Change**: decide per user whether a type is needed at all; where it is, read it from the side known to be observed, in a stated order, with no "file means unknown"; remove the tombstone guard if the DoR confirms that no view can carry one; the test holds an input the planner builds (a folder local with no hash).
- **DoR first**: every caller of `actionItemType` and `resolveItemType` with the views it can be handed, from the producers (`planner_actions.go`, `planner_move_composition.go`); for each, whether the result reaches a stored value or a decision.
- **Tests first**: a table over the producers' real shapes. No scenario unless the DoR finds a case a user can reach (then that case first, seen failing).
- **Order**: after the follow-up to #926 and after W42 (they hold `planner_dependencies.go` and `executor_delete.go`). Sync lane. Size S.

#### W82. The dispatch-order record sees every pass

- **Source**: the writer's "Seen and not fixed" 4 and the review (answer 10) of the folder-replaced-by-a-file fix, 2026-11-15. Diagnostics only; no user can reach it.
- **What is wrong**: the order hook at `internal/sync/engine_runtime_start.go` near `:55` is called only for a ready frontier of two or more actions, after the publication drain. A pass that holds only publications, or one action at a time, leaves no record, so the kept log of a failing model seed shows nothing for exactly the passes that repeated (passes 2 to 8 of the seed that found the folder-replaced-by-a-file defect).
- **Change**: the test-only record names every action a pass dispatches or publishes, in order, publication-only passes included (about 10 to 20 lines; the hook stays nil outside tests and changes no order).
- **DoR first**: who reads the hook today (reverse dispatch, the model's kept log) and what each needs; whether publications can be recorded without the hook being able to reorder them.
- **Tests first**: a publication-only pass and a one-action pass each leave their record; the reverse-dispatch tests still pass.
- **Order**: any time; not with a branch that edits `engine_runtime_start.go` or `engine_runtime_completion.go`. Sync lane. Size S.

#### W83. Test masks cover a sharer's name and any address

- **Source**: the writer's "Seen and not fixed" 1 and the review (answer 10, item i) of the move of the e2e helper tests, 2026-11-15. A path into public CI output on a failure; whether today's fixtures reach it is not known (fixture values are not read).
- **What is wrong**: a shared folder's display name is built from the sharer's first name, and from the full name with the address when names collide (`internal/cli/shared_discovery.go` near `:420` to `:424`). `drive add`, `drive list` and `status` print it. The test masks learn only a configured account's full display name (`e2e/printed_output_rules_test.go` near `:165`), so the first-name form prints a configured sharer's first name and a sharer outside the configured accounts prints with name and address. A failing test's output and the debug logs CI uploads on a failed e2e job (`.github/workflows/ci.yml` near `:650`) carry these; the CI masking step masks no display names.
- **Change**: the masks learn each catalog account's first name (at least four characters) beside its display name, and mask an address-shaped word wherever it stands unless it is a role. About 30 to 40 lines in `testutil` plus tests.
- **DoR first**: every form the product builds a shared display name in, read from source; what a four-character floor leaves readable; whether masking any address-shaped word hides text a failure message needs (roles, example addresses in fixtures of pure tests).
- **Tests first**: one test per learned form that reads the product's format from source, as the shared-by label tests do; a planted address outside the catalog is masked; a role is not.
- **Order**: after the move of the e2e helper tests merges (same `testutil/idmask` files). Lane x. Size S.
- Rider (2026-11-16, from the review fixes of the e2e helper tests): the first-name display form at `internal/cli/shared_discovery.go:420` is built from a variable, so the source test's `sharerLabels` does not see it; its format equals that of line 421. Make the label a literal the test can read, or hold the variable's value in the test, in the same change that adds the display-name mask.

#### W84. The test environment prints no account path

- **Source**: the review (note 6) of the move of the e2e helper tests, 2026-11-15.
- **What is wrong**: `testutil/testenv.go` near `:410`, `:441`, `:450` and `:476` to `:503` prints drive and token paths raw to stderr from `TestMain`. A token path holds the account's address in its file name, and stderr of a live job is public CI output.
- **Change**: those prints name the role (or pass the text through the masks) instead of the raw path. Small.
- **DoR first**: every write to stderr or stdout in `testutil/testenv.go` outside lines 25 to 35 (never read or print those lines), and which of them can hold a drive, an address or a token path; whether the masks are built at that point of `TestMain`.
- **Tests first**: a source test that fails a raw path or drive argument in a print of that file, with a planted row.
- **Order**: after the move of the e2e helper tests merges; not with W61 (same file). Lane x. Size S.

#### W85. The e2e recorders take the roles they redact with

- **Source**: the writer's "Seen and not fixed" 3 and the review (answer 10, item iii) of the move of the e2e helper tests, 2026-11-15. No leak; a limit on what can be tested without an account.
- **What is wrong**: the timing and quirk recorders redact with the suite-wide roles, a package variable only the tagged `TestMain` sets, so eight pure tests of them stay behind the e2e build tag and run only with an account.
- **Change**: `newE2ETimingRecorder`, `newE2EQuirkRecorder` and their redact functions take the roles as an argument; the eight tests move to untagged files and run in the quick tier. Tens of lines across the two helpers and their callers.
- **DoR first**: every reader of the suite-wide roles in the recorder files; which of the eight tests then read nothing else that only the tagged build sets.
- **Tests first**: the moved tests run untagged with planted roles; the tagged build compiles every moved declaration once.
- **Order**: after the move of the e2e helper tests merges. Lane x. Size M.

#### W86. The print rule follows what a suite function returns

- **Source**: the writer's "Seen and not fixed" 5 and the review (answer 10, item v) of the move of the e2e helper tests, 2026-11-15. A weakness of the rule, not a leak: the 30 accepted sites are masked by hand.
- **What is wrong**: the source rule in `e2e/printed_value_rule_test.go` classes a value by where it comes from, but does not follow the result of a suite function, so an identity returned by a helper and printed by its caller is accepted by a hand-kept list instead of being checked.
- **Change**: result classes are computed to a fixed point over the suite's functions, with an exemption for counts; the hand-kept list shrinks to what the rule still cannot see. Larger than the other three.
- **DoR first**: the 30 accepted sites by kind; which a fixed point over results decides and which need another class; the cost of the fixed point on the quick lane.
- **Tests first**: planted rows for a helper that returns an identity through one and through two calls, and for a count that must stay printable.
- **Order**: after W85 (same e2e helper files). Lane x. Size M.

#### W87. A delta listing fails on a next link it already followed

- **Source**: the writer's "Not fixed" 1 and 2 of the repeated-link fix for the non-delta listings, 2026-11-15. Whether OneDrive ever sends such a link is not known.
- **What is wrong**: `deltaAllPages` (`internal/graph/delta.go` near `:181`) asks again for a next link it already followed. Only `maxDeltaPages` (10000) stops it, and every page's items are held until then, so a link that repeats costs up to 10000 requests and their memory before the listing fails. `TestDeltaAll_MaxPages` (`delta_test.go` near `:313`) serves exactly such a link and has no Validates line.
- **Change**: the delta loop asks through the same shared check the other listings use (the tokens or paths asked for; a repeat fails before the request). The sync observers that page delta one page at a time are read too: the same check or a stated reason why not.
- **DoR first**: every caller that follows delta next links (the all-pages loop and the page-at-a-time callers in `internal/sync`); what a repeat means for a delta token that OneDrive may legitimately hand back unchanged (an empty page whose next link equals the one asked is a known Graph behavior for some drives: check the Graph documentation and the recorded quirks before deciding that a repeat is always a fault).
- **Tests first**: a self-link and a two-page cycle fail the listing before the repeated request; `TestDeltaAll_MaxPages` serves distinct links and gets its Validates line.
- **Order**: after the repeated-link fix for the non-delta listings merges (it brings the shared check). Lane x. Size S.

#### W88. A site whose libraries cannot be listed is named by drive list and drive search

- **Source**: reviewer note 9 of the repeated-link fix for the non-delta listings, 2026-11-16; checked in the code the same day.
- **What is wrong**: `internal/cli/drive_catalog.go` near `:383` and `drive_search.go` near `:139` log a failed `SiteDrives` call at Debug and go on to the next site, so that site's libraries are missing from `drive list` and `drive search` and nothing says so: the output reads as complete. In `drive_catalog.go` near `:373` a failed site search is logged at Warn and the command lists no SharePoint drives, while `drive search` near `:132` fails on the same error.
- **Change**: both commands name each site whose libraries could not be listed, in the text output and in `--json` (one field, a list that is `[]` when every site answered), and say that the listing is incomplete. A failed site search in `drive list` is said the same way. The exit code is decided in the DoR and stated in the spec.
- **DoR first**: what each command prints and exits with today for each of the three failures (read the code and its tests; do not guess from the log level); whether a Warn line reaches the user at the default log level; how `status` says a lookup that failed for one account, as the model for the wording; every other loop in `internal/cli` that skips an entry on an error with a Debug line (one narrow grep; each is fixed here when it is the same fault or listed with its reason).
- **Tests first**: a fake whose `SiteDrives` fails for one site of two: the other site's libraries are listed, the failed site is named in text and in JSON; the same for `drive search`; a failed site search in `drive list` is said.
- **Order**: any time. Not with a branch that edits `drive_catalog.go` or `drive_search.go`. Lane x. Size S.

#### W89. The multisync panic-boundary tests that rerun the test binary run side by side

- **Source**: the writer's report of the stress lane's declared members, 2026-11-16; the eight call sites checked in the code the same day.
- **What is wrong**: eight tests of `internal/multisync/panic_boundary_test.go` (the callers of `synctest.RunInChild`, the first at `:185`) each rerun the test binary in a child process and run one after another. The writer measured about 1 s each, which is about 7 minutes of a night that repeats the multisync tests fifty times.
- **Change**: each of the eight runs in parallel with the others where its hooks allow it (a test that sets a package-level hook or an environment variable in the parent cannot; the child is its own process). A test that cannot run in parallel says why in one comment line.
- **DoR first**: for each of the eight, what the parent process touches before `RunInChild` and what the child touches (package-level hooks, `t.Setenv`, fixed paths, ports); whether `synctest.RunInChild` itself is safe to call from parallel tests; the measured wall time of the eight before the change, plain and under the race detector.
- **Tests first**: none new; the eight tests are the check. The report gives the wall time of `go test -count=5 -run` over the eight before and after, plain and with `-race`.
- **Order**: after W77 and W78 (they edit multisync test helpers). Lane x. Size S.
- Rider (2026-11-16, from the writer's report of the multisync and cli watch helpers): `internal/synctest/child.go` bounds a child test binary by its own constant `childTestTimeout` (2 minutes, near `:23`) and, when it kills the child (near `:65`), reports only the child's output so far. The bound is taken from the caller (the package's `eventWait`) or stated once as a named rule, and a killed child's failure says that the child was killed at the bound and what it was waiting to see. DoR: list every caller of `RunInChild` (files only first) and say what a shorter bound does to the slowest child under the race detector on a 2-core host. Tests first: one test that starts a child that never ends and requires the failure text to name the bound; seen failing first.
- Rider (2026-11-16, from the review of the multisync and cli watch helpers; it narrows the rider above): the bound is NOT taken from the caller. A child that uses `runWithin` fails at `eventWait` and its cleanup then waits up to `eventWait` more, so a kill at `eventWait` would come at the moment of the child's own message. Keep `childTestTimeout` and have `runInChild` say that the child was killed at that bound when its context has ended (about 4 lines, no caller changes); today `requireChildFailedWith` then prints "did not fail its test" and `RequireChildPassed` prints "failed". Callers are in `internal/synctest`, `perf`, `localtrash`, `synctree`, `driveops`, `sync`, `multisync` and `cli` (18 files by the reviewer's count; the DoR lists them).

#### W90. Two sync tests that take 8 to 25 seconds are profiled and made cheap

- **Source**: the writer's report and the review of the stress lane's declared members, 2026-11-16.
- **What is wrong**: `TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile` (`internal/sync/engine_pending_create_intent_test.go:126`) was measured at 8.4 to 21.0 s and `TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain` (`internal/sync/engine_shortcut_release_test.go:437`) at 10.6 to 25.3 s on this host. Why they take that long is not known. Each is far above the other tests of its file, and each costs the repeated stress step its time fifty times over.
- **Change**: decided by the DoR. If a test waits on a real timer or a retry delay, the test drives that clock or shortens the delay through the option the engine already has; if the product waits where it need not, that is a product fix with its own failing test.
- **DoR first**: a CPU profile and a block profile of each test alone (`-cpuprofile`, `-blockprofile`), and the time each spends per phase; whether the time is a wait or work; whether the same wait exists in production and what it costs a user there.
- **Tests first**: the two tests keep what they assert. If the cause is in product code, one new test fails on it first.
- **Order**: any time; not with another sync-lane branch that edits either test file. Lane s. Size S.

#### W91. The sync summary counts a deleted folder as a folder, not as a file

- **Source**: a live run on the test account on 2026-11-16 (the proof of a move fix): a sync that removed one empty folder on this computer printed the line "Deleted locally" for the folder and the summary "deleted 1 file locally".
- **What is wrong**: `activityTally.parts` in `internal/cli/sync_console.go` (near `:380`) words every `ActivityLocalDelete` and `ActivityRemoteDelete` as "files", whatever the item was. Moves are worded "items" and folder creates "folders". A user who deleted a folder of 40 files reads a number that is neither the files nor the folders.
- **Change**: the summary says what was deleted: files and folders counted apart ("deleted 3 files and 1 folder locally"), or one honest noun if the activity event cannot tell them apart. The DoR decides which from what the event carries; the JSON summary of `sync --json` gets the same truth (a new field, never a changed meaning of an old one). The dry-run wording follows from the same function.
- **DoR first**: what an activity event carries about the item type on each of the two delete paths, and for a folder deleted with its children, how many events the engine sends (one per item or one for the folder); every reader of `activityTally` (console, dry run, JSON); whether the per-line text ("Deleted locally") needs the same care.
- **Tests first**: table rows for the summary of a sync that deleted one file, one folder, and both, on each side, plain and dry run, and the JSON twin; seen failing first.
- **Order**: any time; not with a branch that edits `internal/cli/sync_console.go`. Lane x. Size S.

#### W92. A folder made below a path whose old holder has not left waits for it

- **Source**: the writer's "Not fixed" 1 and 2 of the OneDrive-side move order and its review of 2026-11-16 (finding 4, notes 5, 7 and 13).
- **What is wrong**: (1) x renamed x2 here meets X2 made on OneDrive, so x stays on OneDrive; x and x/sub are made here and f is moved into x/sub. The create of sub is sent although the old x still holds the path: sub is made inside the old x, carried to x2/sub once the collision clears, and moved back by the next pass (`dropCreatesOntoComposedItems`, `planner_move_composition.go`). (2) The twin on this computer: a create of x/sub is under x's local move source, so x's move waits for it (`underMoveSource`, `planner_dependencies.go`); the folder is made inside the old x, carried, and moved back. Both converge; neither is new with the move-order fix. (3) `movesIntoVacatedPath` folds the source compare while `underMoveSource` compares exactly, so a pair whose source differs only in spelling runs unordered (no user steps found). (4) The walk of `folderHolder` stops at the first synced item even when an item above it holds the path (two contrived shapes in the review's note 7, no row).
- **Change**: a create below a path whose occupant on that side this computer (or OneDrive) has elsewhere, and that has no move on that side this pass, waits a pass like the move into it does: about 10 to 15 lines per side. One name comparison (as OneDrive compares) in every dependency rule of `planner_dependencies.go`. The walk checks every made level.
- **DoR first**: both shapes reproduced on main after the move-order fix with what each pass plans; whether the take-back of a move whose holder ends deferred (if that fix carries it) already holds the create too; user steps for (3) and (4) or the proof that there are none.
- **Tests first**: a composition row and a RunOnce scenario per side that assert no carry (no move of the made folder in any pass); the chain scenario of the move-order fix asserts it too; a dependency row for a source spelled differently; rows for the two shapes of (4).
- **Order**: after the OneDrive-side move order merges. Sync planner rows run one at a time (not with W80, W52, W53, W68 or W67). Sync lane. Size M.

#### W93. A status line that fails to write is recorded; every printed count is worded by one helper

- **Source**: two riders on H8 (2026-11-14 from H7's report, "Not fixed" 4; 2026-11-15 from the review of H7's second version, note 16), each site read on main `ca287a32` on 2026-11-16. Split from H8 so that H8 keeps the silent failures outside the output code.
- **What is wrong**: (1) `internal/cli/format.go` drops the write error in `writeWarningf` (`:31`, a free function with no command context), `Progressf` (`:303`) and `ProgressDone` (`:322`), while `Statusf` records its failure through `recordStatusError`. (2) Seven formats print a count and a plural noun directly, so one reads `1 files`, `1 items`, `1 drives`: `trash.go:258`, `drive_rebind_root.go:125`, `recycle_bin_flow.go:123` and `:135`, `sync_notify.go:140`, `:166` and `:172`. The helper exists: `countOf` (`folder_transfer.go:71`) and `countItems` (`format.go:271`).
- **Change**: each of the three writers records its failure the way `Statusf` does, with `recordStatusError` called outside `statusMu` (the DoR decides how `writeWarningf` reaches the command context: a method, or an error its callers record). Every count the CLI prints goes through `countOf`, which moves beside `countItems`. A source check in the cli tests (model: `json_lists_test.go`) fails on a format in non-test `internal/cli` that holds a number verb directly before a plural noun, with its exceptions named in the check.
- **DoR first**: every caller of `writeWarningf` and what a recorded status error does to the exit status of each command that calls it; a search of non-test `internal/cli` for more count formats than the seven; what `sd_notify` status text and the tests near `trash_test.go:75` and `drive_rebind_root_test.go:60` and `:161` expect.
- **Tests first**: one test per writer with a writer that fails (the command reports the failure); one test per count site with the count 1; the source check, seen failing on one restored site.
- **Order**: any time. Lane o. Not with a branch that edits `internal/cli/format.go`, `folder_transfer.go`, `trash.go`, `drive_rebind_root.go`, `recycle_bin_flow.go` or `sync_notify.go` (check W26 and W40 at dispatch). Size S.
- Rider (2026-11-16, from writing the brief): on main `ca287a32` the record itself is never read. `StatusError()` (`format.go:81`) has one caller, a test (`format_test.go:120`); no product code reads `statusErr`, so a failed status write changes nothing today and "records like `Statusf`" alone would change nothing either. Decided: the record gets one reader in `mainWithWriters`: a command that worked and whose status stream failed a write ends with exit 1 and the failure goes once to the command logger; a command that failed keeps its own status (the GNU tools treat a failed standard error the same way). Live proof mine: one command of the test account that prints a status line, with standard error on `/dev/full`, installed binary against the branch binary (exit 0 before, 1 after), in a private tree.

#### W94. Lint reads every Go file, also one built only on another platform or with the race tag

- **Source**: the fact check for A12's brief on main `ca287a32` (2026-11-16), by reading `.github/workflows/ci.yml` and `.golangci.yml`.
- **What is wrong**: the per-PR lint job (`verify-lint`, ubuntu only) builds linux files with the tags e2e, e2e_full and integration, so golangci-lint never reads a file constrained to darwin, freebsd, netbsd, `!linux`, `!unix` or `race`: two test files (`internal/graph/http_request_darwin_test.go`, `internal/sync/race_enabled_test.go`) and about ten product files (A12's DoR lists them). The nightly job `verify-full` runs `verify public` on ubuntu and macOS; whether that lints on macOS is in A12's DoR. A file with the race tag is linted nowhere. Every lint rule has this gap. The hand-written source checks read every `.go` file whatever its constraint, so the two rules A12 moves into lint take the gap over.
- **Change**: every Go file in the tree is read by a lint pass on every PR. The DoR chooses by cost between further passes of the linter (one with `GOOS=darwin`, one with the `race` tag, one more if a file is built by neither linux nor darwin) and a smaller set that still reads every file. A devtool source check lists the build constraints in the tree and fails on a file no lint pass builds, so a new constraint cannot go unread.
- **DoR first**: the list of files and constraints the per-PR lint run does not build, from A12's DoR, checked again on the base; what a `GOOS=darwin` pass and a race-tag pass report on main today and what each costs on a CI runner and on 2 cores (whether the darwin pass type-checks on linux at all: cgo, platform packages); whether the lint job's timeout and devtool's lint deadline hold with the passes added.
- **Tests first**: the source check, seen failing on a fixture file whose constraint no pass covers; the test that each CI job timeout covers the deadlines it can start, extended to the new passes.
- **Order**: after A12 merges (same lint steps in `internal/devtool/verify.go`). Not with a branch that edits the lint steps of devtool or the `verify-lint` job. Lane o. Size M.
- Rider (2026-11-17, from the review of A12, note 2(b)): golangci-lint turns forbidigo's `exclude-godoc-examples` on by default, so forbidigo skips every `Example` function in a test file; the hand-written Sleep check A12 deleted read them. No `func Example` exists today. This row also sets `linters.settings.forbidigo.exclude-godoc-examples: false` (or says why not) and pins it in `internal/devtool/lint_config_contract_test.go`.

#### W95. A change list that leads back is refused and recovered wherever it is read

- **Source**: the review of W87 (2026-11-16), should fix 4 and notes 5, 6, 7, 9 and 11, on W87's commit over main `ca287a32`.
- **What is wrong**: (a) the sync observer's own guard (`deltaLoopGuard`, `internal/sync/observer_remote.go` near `:802`, walked near `:235` and `:290`) seeds only a non-empty saved token and keys on raw token strings, so a window read without a saved token whose next link is its own first request is asked once more before the guard sees it, and two spellings of one page count as two; `observer_remote_cursor_test.go` near `:55` holds only the self-link on a later page. (b) R-2.15.2 (`spec/requirements/sync.md` near `:234`) says a change list that leads back on itself is recovered like an expired one; for a shared folder (`engine_mount_root.go` near `:58`) the graph client's loop error is detected but not recovered: the saved cursor is kept and every watch poll fails and backs off (near `:392`) until the periodic full refresh (`engine_watch_maintenance.go` near `:176`). (c) a non-empty token that is not a link (`internal/graph/delta.go`, `resolveDeltaToken`) silently restarts from the initial path, and the caller takes a full enumeration as an incremental one. (d) a page's key is the exact path-and-query spelling; a next link with an upper-case scheme or a relative next link resolves to the initial path, and `spec/design/graph-client.md` says every continuation goes through `stripBaseURL`, which is untrue for such a delta link. (e) no test holds that a repeat at exactly the page cap reports the cap error (`items_fetch.go` near `:490`), for any listing. (f) the `TestDelta*` tests carry no Validates line where a requirement applies.
- **Change**: `DeltaPage` carries the path it was asked at and the path its next link resolves to (set in `fetchDeltaPath`); the page check of the graph client gets an exported form; the observer's guard records the first request's path and asks with the next path before each further page, and maps the graph error to the unusable-cursor error it already has. The graph loop error gets a sentinel, and the shared-folder observation recovers on it beside `ErrGone` (this reopens W87's decision that neither error wraps a sentinel: the decision is this row's). A non-empty token that is not a link is refused as an unusable cursor. A relative or upper-case-scheme next link is resolved or refused, never read as the initial path, and the design sentence is made true. Sync fakes that serve delta pages fill the two paths.
- **DoR first**: every reader of `Delta`, `DeltaFolder`, `DeltaAll` and `DeltaFolderAll` on the base with file and line; what each does today on the loop error, on the cap error and on a non-link token; whether a stored cursor in a state database can be a non-link token (how tokens are written); what OneDrive documents about the form of a next link; the fake fetchers in `internal/sync` tests that build `DeltaPage` values, counted.
- **Tests first**: for the observer, the link back to the saved token's page, the self-link on the first request of a window without a token and a two-page cycle, each seen failing; a shared folder whose change list leads back recovers like an expired cursor, seen failing; a non-link token refused; a repeat at exactly the cap, for every paged listing, in the link-back table; a relative and an upper-case-scheme next link.
- **Order**: after W87 merges. Not with W58, W69 or W26 (`internal/graph/delta.go`, `items_fetch.go`, `internal/sync/observer_remote.go`). Lane s. Size M.

#### W96. Every store wait after a run's context ended goes through one helper and the stop's deadline

- **Source**: the review of W65 (2026-11-16), note 6, and the report of W65's writer, "Not fixed" items 3, 4 and 5.
- **What is wrong**: W65 gives a stopping run one bound for the completions it drains (`runDetachedStoreAccess`, the stop's deadline). Three store waits that can run after the run's context ended still detach by hand, each with its own bound and outside that deadline: the findings write after a cancel in `internal/sync/engine_watch_loop.go` near `:385` (its own 30 s detach), one in `engine_shortcut_release.go` near `:668`, and two in `executor_transfer.go` near `:681` and `:700`. And a completion that is being applied on the live context at the instant the cancel lands is cut by the cancel instead of finishing under the stop's deadline.
- **Change**: every store wait that can run after the run's context ended goes through `runDetachedStoreAccess` and, once the run has ended, under the stop's one deadline. A completion in flight at the cancel is finished under that deadline or counted in the stop's one warning; none is lost silently. A source-reading test names every detach in `internal/sync` product code and fails a new one outside the helper.
- **DoR first**: every `context.WithoutCancel`, `context.Background()` and hand-made detach in `internal/sync` product code on the base, with file and line, and for each whether it can run after the run's context ended; what a store timeout in the findings write does today and what it would do under the stop's deadline (the review names the risk: it would then end the watch with an error; decide and say); how a completion in flight at the cancel ends today, shown by a test.
- **Tests first**: for each of the three sites, a stop whose store wait outlasts the stop's deadline is counted in the one warning, seen failing; a completion in flight at the cancel is recorded, seen failing; the source-reading test, seen failing on a fixture.
- **Order**: after W65 merges. Not with a branch that edits `engine_watch_loop.go`, `engine_shortcut_release.go` or `executor_transfer.go` (check at dispatch). Lane s. Size M.

#### W97. The scanner's entries are their own type; `changeEvent` and `changeType` are gone

- **Source**: the fact check for G7 against main `ca287a32` (2026-11-16). After G5 the local side still names `changeEvent` in `scanner.go` (`scanResult.Events`, the collision check's input), `symlink_observation.go`, `observer_local_handlers.go`, `observer_local.go` and `local_observation_batch.go`; G7 removes the type from the remote side only.
- **What is wrong**: one struct serves two producers that fill different halves of it. A local entry carries remote identity fields nothing sets; a reader cannot tell from the type which fields hold a value.
- **Change**: a local type with exactly the fields a local reader reads (G5's DoR lists them); `changeEvent`, `changeType` and its constants are deleted with their enum strings; tests follow. No behavior change.
- **DoR first**: every product reader of each field of a local entry, by file and line; every test file that builds a `changeEvent` for the local side (15 files, 111 lines on main before G5); whether any spec sentence names the type (R-6.7.28 ends with it).
- **Tests first**: none new; the existing scanner, collision and local observer tests hold the behavior and are migrated in the same commit. A source check that no file under `internal/sync` names the deleted types.
- **Order**: after G5, G6 and G7 (same files; it deletes what they leave).

#### W98. The CLI names a stalled answer and a lost connection by their type

- **Source**: the review of W58 (2026-11-16), answer 5, items (a) and (b), on W58's commit over main `0e31e63c`. Both are in `internal/cli`, which W58's brief put out of scope; neither blocks W58. From reading, and the reviewer's HTTP/2 run of a stalled answer.
- **What is wrong**: (a) `graph.ErrResponseStalled` has no `Timeout()`, so `transportErrorReason` (`internal/cli/error_presentation.go` near `:124-126`) never says "did not answer in time" for a stalled answer: over HTTP/2 its text holds "(http2: response body closed)" and the text match near `:172` says "the connection to OneDrive was interrupted"; over HTTP/1 nothing matches and the generic text is shown. Before W58 the command waited for ever, so this is no regression. (b) the match on `"http2: "` in `isDroppedConnection` (near `:170-172`) also matches the text of `graph.ErrConnectionLost`, so the lost connection is still known by its text in a second product place; it prints the same "interrupted" as before W58.
- **Change**: `transportErrorReason` reads `graph.ErrResponseStalled` in its timeout case, before `isDroppedConnection`, and `graph.ErrConnectionLost` by type; the `"http2: "` text match stays only for the HTTP/2 stream and connection errors net/http does not export.
- **DoR first**: every branch of the error presentation that a stalled answer, a lost connection, a stalled transfer and a dropped connection reach today, with file and line, and the line each prints over HTTP/1 and HTTP/2; which HTTP/2 errors the text match must still catch once the two types are read first; the wording for a stalled answer (propose the text).
- **Tests first**: a stalled answer, in its HTTP/1 and HTTP/2 shapes, reads as not answered in time, seen failing; a lost connection reads as interrupted by its type, seen failing on a match that names only the text; an HTTP/2 stream error still reads as interrupted.
- **No live proof is possible**: neither silence can be provoked on OneDrive. Fake-server tests only.
- **Order**: after W58 merges (the two types land there). Not with a branch that edits `internal/cli/error_presentation.go` (check at dispatch). Lane o. Size S.

#### W99. `drive list`'s site-search warning log line says a plain reason

- **Source**: found while building W88 (2026-11-17); the author proposed it as a separate fix, and the second review's note N8 confirmed it on W88's final commit. From reading.
- **What is wrong**: `drive list`'s existing warning log line for a failed SharePoint site search (`logger.Warn("SharePoint site search failed", "error", err)` in `discoverSharePointDrives`, `internal/cli/drive_catalog.go`; unchanged by W88) prints the Graph request id and response body at the default level.
- **Change**: make its reason plain (`plainErrorReason`); the Debug level may keep the whole error.
- **DoR first**: how the console handler renders that line's `error` attribute today (file and line), what it prints for a refused site search that carries a request id and an HTML body, and the plain text it prints instead.
- **Tests first**: a refused site search whose answer carries a made-up request id and page: the warning line at the default level holds neither, seen failing.
- **No live proof is possible**: a failed site search cannot be provoked on the test account. Fake-server tests only.
- **Order**: after W88 merges (same file). Not with H8, which edits `internal/cli/drive_catalog.go`; check W93 and the other branches in flight at dispatch. Lane o. Size S.

#### W100. A refused stderr line of the sync console and of main's last lines is recorded

- **Source**: the review of W93 (2026-11-17), should-fix 4, and W93's fix round, item 2; both were left out of W93 and named in its spec paragraph and commit message. From reading W93's commit.
- **What is wrong**: (1) `syncConsole.write` (`internal/cli/sync_console.go`, near `:578-587`) drops the error of every write. Its stderr half (a failed or retried file action, a drive's error, held deletes) is not recorded, so a `sync` whose stderr refused such a line ends as if the user had read it. (2) `mainWithWriters` (`internal/cli/root.go`, near `:783-834`) writes its own last lines, the final error and the warning that the log file did not close, after the log file closed and discards a refused write: the exit status is already non-zero, but nothing records that the user never saw why.
- **Change**: the console's stderr writes record a refusal through the command context the way `warnf` does, so a sync that worked ends with exit 1 and one warn record (the stdout half is W40's). For main's last lines the DoR decides whether the final error can be written before the log file closes, so that a refused write reaches the log; what still cannot be recorded is named in `spec/design/cli.md`.
- **DoR first**: every stderr line `syncConsole` writes (file and line) and the modes that write it (one-shot, watch, `--json`, `--quiet`); what a recorded refusal does to the exit status of a one-shot sync and of a watch stopped cleanly; the order of main's last writes against the log file's close, and whether moving the final error before the close changes any output.
- **Tests first**: a sync console whose stderr refuses a held-deletes line and a failed-action line: the record holds the failure and a sync that worked exits 1, seen failing; main with a stderr that refuses the final error of a failed command: the status stays the command's and the log file holds one warn record (or the spec says why it cannot), seen failing.
- **Live proof**: one `sync` of the test account that holds deletes, with stderr on `/dev/full`, in a private tree, main's binary against the branch's.
- **Order**: after W40 or together with it (W40 records the stdout half of the same `write`); not with W91, W26 or W41 (all edit `sync_console.go`). Lane o. Size S.

#### W101. A rebind after a mount vanished deletes no unmounted disk's content unconfirmed

- **Source**: the second review of W41 (2026-11-17), should-fix 1 and its probe of two unmounted disks; W41's report, "Candidate for a later change". Main behaves the same (checked by test on 17564167 in W41's fix round). W41 changed only the texts; this row changes the behaviour.
- **What is wrong**: `drive rebind-root` calls `ClearSyncRootIdentity` (`internal/sync/store_observation_state.go`, near `:526`), which runs `DELETE FROM mount_boundaries` with no `WHERE`: it forgets every recorded mount point, not only the sync folder's fingerprint. The next sync then no longer sees a crossed mount whose disk is not mounted as unavailable and takes its files as deleted. It deletes the OneDrive content of every such disk, with no confirmation while the deletes stay at or under the hold's threshold, max(50, 30 % of the synced items plus the last 24 h of deletes): in `TestRunOnce_ARebindAfterADiskIsGoneDeletesItsContentOnOneDrive` 3 files are deleted and 51 held, and with two unmounted disks all four files of both are deleted. A user who rebinds for one dead disk while another listed disk is only unplugged loses that disk's OneDrive copies; changes made only on OneDrive there go to the recycle bin, and sharing and version history go with the items.
- **Change**: the DoR decides between (a) the rebind keeps the recorded mount points and clears only the fingerprint, so a vanished mount stays "Disk not mounted" until the disk is back or its folder is removed; (b) deletes under a mount point the rebind forgot are held until `sync --confirm-deletes`, whatever their number; (c) `drive rebind-root` lists the disks that are not mounted and asks before it forgets them. Then the status action of "Disk not mounted", the scan's warning (`vanishedMountSkippedItem`), `drive rebind-root --help`, `docs/safety.md`, `docs/troubleshooting.md` and the rebind row of `spec/design/cli.md` say what the rebind does after the change.
- **DoR first**: every reader of `mount_boundaries` and why `ClearSyncRootIdentity` clears it (the history of that line); what a rebind of a folder that really is a new disk needs from the recorded mount points; which option keeps the rebind's purpose (accept a folder moved or restored on purpose) and never deletes an unplugged disk's content unconfirmed; whether a watch that learns of an unmount through inotify plans deletes before a scan marks the folder.
- **Tests first**: the rebind test's cases change: after a rebind with one or two vanished mounts, the next sync deletes nothing on OneDrive without confirmation, seen failing on main; a disk mounted again after the rebind loses nothing (`TestRunOnce_ADiskMountedAgainLosesNothing` as the model).
- **Order**: any time. Lane s. Not with a branch that edits `internal/sync/store_observation_state.go`, `store_reset.go`, `scanner_unavailable.go` or `internal/cli/drive_rebind_root.go`. Size S.

#### W102. devtool links no product package, also through testutil

- **Source**: the report of A12's writer, "Seen and not fixed" item 1, confirmed by the review of A12, note 9 (`go list -deps ./cmd/devtool` on A12's commit). It existed before A12.
- **What is wrong**: depguard's rule `devtool-no-product` (A12) reads direct imports only. `internal/devtool` imports `testutil` (`bench_live.go` for `CheckDriveAllowed`, `live_env.go` for `LoadTestEnvFiles`), and `testutil` imports `internal/localpath` and `internal/sharedref` (`testutil/testenv.go`, `testutil/driveroles.go`), so the verification tool still links two product packages. The rule's description says it "must not link product packages".
- **Change**: devtool gets what it needs from `testutil` without linking a product package (the DoR chooses: the two helpers' product-free part in a package devtool may import, or devtool's use rewritten). A test fails when devtool links a product package through any import, not only a direct one.
- **DoR first**: every `testutil` function devtool calls, with file and line; which of them reach `localpath` or `sharedref`, and for what; every product package `go list -deps ./cmd/devtool` prints on the base; the form of the link test (`go list -deps` from a test, or a depguard rule on the package devtool imports) and its cost.
- **Tests first**: the link test, seen failing on the base.
- **Order**: any time. Not with a branch that edits `testutil/testenv.go` or `testutil/driveroles.go` (W83, W84, W61: check at dispatch). Lane o. Size S.

#### W103. Every repository-rule walk of devtool skips the same folders

- **Source**: the report of A12's writer, "Seen and not fixed" items 2 and 3; the review of A12, note 9 (both existed before A12).
- **What is wrong**: each repository-rule walk of `internal/devtool` keeps its own list of folders to skip. `shouldSkipOutputBoundaryDir` (`verify_output_boundaries.go`) does not skip `.worktrees`, unlike `shouldSkipTestConventionDir` (`verify_test_conventions.go`); the walks in `verify_requirements.go`, `verify_backlog_ids.go` and `verify_mutex_docs.go` keep their own lists; `verify_governs.go` calls the test-convention helper under its name. A tree nested in a skipped folder is read by some rules and not by others.
- **Change**: one skip helper with a neutral name serves every repository-rule walk; the per-walk lists and the borrowed name go. A difference that is deliberate stays, with its reason in a comment.
- **DoR first**: every walk in `internal/devtool` with its skip list, file and line; which differences are deliberate and which are drift; what creates `.worktrees` today, if anything.
- **Tests first**: a fixture tree holding each skipped folder: every walk reads the same files, seen failing on the output-boundary walk.
- **Order**: any time. Not with a branch that edits those walk files. Lane o. Size S.

#### W104. The benchmark's retries wait on an injected clock and read the CLI's server-error line

- **Source**: the report of A12's writer, "Seen and not fixed" item 4, and the review of A12, note 7 (both existed before A12). From reading.
- **What is wrong**: (a) the remote reset and the visibility probe (`internal/devtool/bench_live.go`) and the delta wait (`bench_live_bidirectional.go`) sleep on the wall clock between attempts, so `TestResetBenchRemoteScopeRetriesUntilSuccessOrNotFound` waits 500 ms of real time. (b) `isRetryableBenchGraphReadFailure` looks for "graph: HTTP 50x", which the CLI prints only in its `Details:` line under `--verbose` or `--debug`; the benchmark passes only `--config` and `--drive`, so a 5xx during the visibility probe is fatal instead of retried, and the test fixture "graph: HTTP 503" is not a line the CLI prints.
- **Change**: the wait between attempts is injected and the tests use a fake that records the waits; the retry reads the CLI's own server-error line ("Error: OneDrive had a temporary server problem.", `internal/cli/error_presentation.go`) the way `cliReportsRemoteItemMissing` reads the missing-item line; the fixtures use lines the CLI prints.
- **DoR first**: every wait and every stderr classification in the benchmark, with file and line; every line the CLI prints for a 5xx without `--verbose`, from `error_presentation.go`.
- **Tests first**: a visibility probe that sees the CLI's server-error line retries, seen failing; the retry tests run without a wall-clock wait.
- **No live proof is possible**: a 5xx cannot be provoked on the test account. Fake tests only.
- **Order**: any time. Not with a branch that edits `bench_live.go` or `bench_live_bidirectional.go`. Lane o. Size S.

#### W105. Each account at a shared address has its own view, status row and selector

- **Source**: H8's review fixes ("Stays wrong" and "Seen and not fixed" items 1 to 3) and the second review of H8 (declared leftovers), declared in #947. From reading; main behaved so before #947.
- **What is wrong**: account views are one per address (`internal/cli/account_view.go`, grouped by email). A personal and a work or school account can share an address. Since #947 the second account's sign-in requirement is written and cleared on its own record, but no account surface shows it: no `status` row, no `drive list` `accounts_requiring_auth` entry, not logout's choice. `--account` takes an email (R-3.5.2), so it cannot pick the second account. Logout deletes the token of either account, depending on the map order `drivesForAccount` yields to `canonicalIDForToken` (`internal/cli/auth_logout.go`), while it removes both accounts' drive sections. The live `status` overlay asks for every configured drive at the address with the first account's token, so the second account's primary drive shows the first account's quota (`fetchConfiguredStatusDrive` calls `PrimaryDrive`).
- **Change**: one account view per canonical ID, which gives one status row and one auth-requirement entry per account; configured drives grouped by owner account rather than by email; state databases counted per account; logout and `--account` take a canonical-ID selector when an address has two accounts, and logout deletes the token of the account it names; the live overlay asks each drive with its owner's token. Status text and JSON, `drive list` and logout change: the spec, R-3.5.2 and the user docs change in the same PR.
- **DoR first**: every reader of the account view and every grouping by email in `internal/cli` (status, drive list, drive search, login, logout, `--account` resolution), with file and line; the status text and JSON fields this changes; R-3.5.2's wording; whether any other surface reads `RepresentativeTokenID`.
- **Tests first**: two accounts at one address, each needing a sign-in: two status rows (text and JSON) and two `accounts_requiring_auth` entries; logout with the canonical-ID selector deletes only that account's token and drive sections, whatever the map order; the overlay asks each drive with its owner's token; each seen failing.
- **Live proof**: needs two signed-in accounts at one address; whether the test setup has them is unknown. Otherwise the unit tests are the proof.
- **Order**: after #947. Not with a branch that edits `internal/cli/account_view.go`, `auth_logout.go`, `status_live.go` or `drive_catalog.go` (W88). Lane o. Size M.

#### W106. A panic on main's goroutine completes the performance summary; a failed watch start keeps a keepalive panic

- **Source**: the two leftovers of #947 (merged 2026-11-17), its body's "Not in this change", second bullet; first seen in H8's report ("Seen and not fixed" 2) and its fix report ("Seen and not fixed" 4). From reading main at `b00e6da3`; both behave so before #947 too.
- **What is wrong**: (a) `mainWithWriters` (`internal/cli/root.go`, the deferred recover near line 800) hands a panic on the command's own goroutine to `reportMainPanic` (near line 886), which logs it, closes the log file and prints the error, but never calls `completeRootCommandPerf` (near line 920, `cc.PerfSession.Complete`): the call near line 809 runs only when `cmd.ExecuteContextC` returns. So a command ended by such a panic logs no performance summary, although a panic of a helper goroutine gives one that reads "error". (b) `runSyncDaemonWithFactory` (`internal/cli/sync_runtime.go` near line 248) joins a keepalive panic to the watch's error with `withCommandPanic`, then returns `formatWatchStartupError(watchErr)` (`internal/cli/sync_startup_messages.go` near line 98). When the joined error holds a `multisync.WatchStartupError` with startup results, the formatter rebuilds the error from the drives' failures alone (`startupFailureError`), so the panic is dropped: the message and stack are lost and the exit status can be the sign-in or config one (R-6.6.31) instead of 1 (R-6.3.10).
- **Change**: (a) `reportMainPanic` completes the command's performance session with the panic's error before it closes the log file, once (a session already completed is not completed again); (b) `formatWatchStartupError` keeps a panic the error carries: the presented startup message stays, the error still answers to `panicguard.ErrPanic` with its stack, and the exit status is 1.
- **DoR first**: where the session's context and `Complete` stand when `reportMainPanic` runs (is the session's context already canceled; can `Complete` block); every caller of `formatWatchStartupError` and `startupFailureError`; how `commandExitCode` ranks a panic against the startup causes.
- **Tests first**: (a) a command whose own goroutine panics through `mainWithWriters` (in a child process if needed, as the existing panic tests do) writes exactly one performance summary with result "error" to its log, exit 1; (b) a watch whose start fails for every drive with a sign-in cause while the keepalive panics returns an error that answers to `panicguard.ErrPanic`, prints the startup message and exits 1; each seen failing on main. Tests near `root_test.go`, `perf_test.go` and `sync_startup_messages_test.go`.
- **No live proof is possible**: a panic cannot be provoked in the product on the test account. Unit tests are the proof.
- **Order**: any time. Not with a branch that edits `internal/cli/root.go` `mainWithWriters` or `reportMainPanic` (W93, W100) or `sync_runtime.go` / `sync_startup_messages.go`. Lane o. Size S.

#### W107. A state reset, a relative path's sentence and a shared item's not-found line name their path rightly

- **Source**: the leftovers of #948 (W66), its body's "Not in this change"; first seen in W66's report ("Seen and not fixed" 1 and 2) and in the second review of W66 (the wider case of its second should-fix). From reading and the review's probe, on the branch at `6416bd6b`.
- **What is wrong**: (a) `internal/sync/store_reset.go` wraps a `localpath` failure with the path the failure already names (near lines 45, 58, 93, 108 and 148: `remove state DB file %s`, `move state DB file %s`, `check state DB file %s`, `open state DB %s`), so `drive reset-sync-state` prints `remove state DB file P: removing P: <reason>`. (b) `localFailureText` (`internal/cli/error_presentation.go`) builds its sentence from `fs.PathError.Path`, which is always absolute: a single `get` into `ro/b.txt` whose folder cannot be written says `Permission denied for <abs>/ro` while its details name `ro/.onedrive-go.b.txt…partial` as typed (pinned today by `TestRunGet_AnUnwritableFileIsNamedOnce`). (c) `remoteFailureSubject` (same file) adds "Run 'onedrive-go ls <parent>'" whenever `path.Dir` differs from the path; a shared selector holds no `/`, so a shared `put` (`internal/cli/put_shared.go` near line 52) and a shared folder `get` (`internal/cli/get.go` near line 236, the listing) that OneDrive answers with not found suggest `onedrive-go ls .`, which does not show the shared item. A single shared `get` already gives no hint, through the `noParent` field `downloadOneFile` sets.
- **Change**: (a) the wraps keep their verbs and drop the path the failure names; (b) the sentence names the path as the user gave it, through an exported `localpath` accessor for the given path (it joins the declared lists in `internal/sync/executor_rooted_mutation_test.go`); the hidden-partial rules of `localFailureText` stay; (c) `remoteFailureSubject` gives no list hint for a path that starts with `sharedref.Prefix`, and the `noParent` field goes.
- **DoR first**: every wrap in `internal/sync` over a `localpath` call that names the path again (not only `store_reset.go`), with the text a user sees; every caller of `localFailureText` and whether its path arrives typed or absolute; every `newRemoteOpError` caller whose path can be a selector.
- **Tests first**: (a) a reset whose state file cannot be removed names it once; (b) a single `get` and a single `put` given a relative path name it as typed in the sentence; (c) a shared `put` and a shared folder `get` answered with not found print the sentence with no hint; each seen failing.
- **Live proof**: (b) on the test account with a relative path in a scratch tree outside any synced folder; (a) and (c) need faults the test account cannot give, so unit tests are their proof.
- **Order**: any time. Not with W101 (`store_reset.go`) or a branch that edits `internal/cli/error_presentation.go` (W40). Lane x. Size S.

#### W108. The CLI says in a sentence of its own that the answers for a path disagree

- **Source**: the leftover of #949 (W69), its body's "Not in this change"; first named in W69's second fix report ("Still open") and in its last review (should-fix 2, the reporting problem). From reading, and (b) from W69's live probe on the test account.
- **What is wrong**: (a) `graph.ErrPathAnswersDisagree` (`internal/graph/items_fetch.go`) has no case in `remoteFailureText` (`internal/cli/error_presentation.go`), so a command whose path's answers disagree prints `Error: ` and the raw chain, which names both folders by item ID (`graph: "a/b/c" was read in folder <id>, and the read of "a/b" answered folder <id>: graph: answers for one path disagree`). The same holds at the end of a visibility wait, where `driveops.PathNotVisibleError` carries the disagreement as its cause (put's wait for its parent, sync's parent gate; rm's note for a parent not visible yet is unaffected). (b) A read by path of a name with a colon on a drive prints `Error: resolving "/P": resolve item path "P": graph: reading "P": graph: name cannot be carried in a path-addressed request`: the path three times and no sentence (`graph.ErrNameNotPathAddressable`).
- **Change**: (a) one sentence, recognized with `errors.Is` so the wait's error is caught too: it names the path once, says OneDrive's answers for it disagree, and gives the next step; the item IDs only in `--verbose`'s details. (b) one sentence for a name a path cannot carry, naming the path once and the character.
- **DoR first**: every command that can reach each error (stat, get, ls, put, mkdir, mv, cp, rm; sync's parent gate) and whether it has already changed something when it does (a disagreement in a confirmation wait comes after the mutation landed: the sentence must not say nothing changed there); how `mkdir` of a name with a colon answers today (OneDrive's own 400 "invalid character").
- **Tests first**: per error, a command test on the CLI's fake drive that prints the sentence once, without an item ID, and the chain under `--verbose`; each seen failing.
- **Live proof**: (b) on the test account, a name with a colon in a private folder removed afterwards; (a) needs answers the test account cannot give, so unit tests are its proof.
- **Order**: any time. Not with W40, W98 or W107 (each edits `internal/cli/error_presentation.go`). Lane o. Size S.

#### W109. A folder create's empty answer and sync's 409s read the one name before listing the folder

- **Source**: the leftovers of #949 (W69), its body's "Not in this change"; first seen in W69's first report ("Seen and not fixed" 1 to 3). From reading, on main `2686d99d`.
- **What is wrong**: three lookups of one name still list the whole folder, every page, where #949 made the others read the one name first. (a) `readCreatedFolderAfterEmptySuccessBody` (`internal/graph/items_mutation.go` near `:76`), after a folder create answered 2xx with an empty body, lists the parent whole on each read-back attempt and picks the folder with `findCreatedFolder` (near `:128`: the exact spelling, else one folded match; two folded matches are none). (b) `adoptExistingRemoteFolder` (`internal/sync/executor.go` near `:366`), after a folder create's 409 in `createRemoteFolder`, lists the parent whole to adopt a folder of the name or to report a differently spelled sibling (`remoteNameTakenError`). (c) `resolveCreateConflict` (`internal/sync/executor_transfer.go` near `:737`, the listing near `:756`), after a file create's 409, lists the folder whole to find the holder of the name (name taken, a lost create of its own to claim, or the conflict). (b) and (c) can run once per created item of a sync, each costing every page of a large folder. The sync item client (`itemClient`, `internal/sync/domain_io_seams.go`) has no one-name read.
- **Change**: each reads the one name (`GetChildByName`) and lists the folder only when that read cannot settle it, a page at a time, stopping at the page that holds the name as spelled, as `FindExistingChild` and the created file's read-back do since #949. (a) keeps `findCreatedFolder`'s rule for its listing fallback; (b) and (c) keep every outcome they have today (adopt, name taken, claim, conflict) for every answer. `itemClient` gains the one-name read and the paged listing, and the sync test fakes gain both, pinned to the interface.
- **DoR first**: what the one-name read answers for a name that differs from its holder's only in case or Unicode form (from `spec/reference/graph-api-quirks.md` and the tests #949 added for the spelling), and so whether (b)'s and (c)'s name-taken outcome can come from the read alone; every caller of the three lookups and its cost today in requests per created item; whether `readCannotSettleName`, now in both `internal/graph/item_create.go` and `internal/driveops/session.go`, can become one function without a graph export the program rules out.
- **Tests first**: per lookup, a fake that counts requests: one read and no listing when the read answers; the fallback stops at the page that holds the name; a differently spelled sibling still reports name taken; a file of the name is not an adopted folder and a folder is not a file's holder to claim; each seen failing.
- **Live proof**: (a) none: an empty create answer cannot be provoked. (b) and (c) need a create that meets an item OneDrive holds and this computer has not observed yet; the DoR says whether that can be staged on the test account in a private folder removed afterwards (one read per name in the debug log); if not, unit tests are the proof.
- **Order**: lane s. Not with W96 (`executor_transfer.go`) or G2 (the sibling matchers in `internal/graph`); check the sync-lane branches that edit `executor.go`, `executor_transfer.go` or the sync test fakes (`engine_test_helpers_test.go`) at dispatch. Size M.

#### W110. Drive and site IDs go into request paths by one rule

- **Source**: the leftovers of #949 (W69), its body's "Not in this change"; first seen in W69's first report ("Seen and not fixed" 5 and 6). From reading, on main `2686d99d`.
- **What is wrong**: since #949 every item ID goes into its route escaped as one path segment (`itemRoute`, `internal/graph/items.go` near `:61`, whose comment says the drive ID goes in as it is), while a drive ID or a site ID goes in raw: `itemRoute` itself; the first chunk of a read by path (`fetchItemByPathChunks`) and the route of `ListChildrenByPathPages` (`internal/graph/items_fetch.go` near `:448` and `:550`); the delta route (`delta.go` near `:32`); the socket.io subscription (`socketio.go` near `:27`); the drive read (`drives_identity.go` near `:220`); the recycle bin listing (`items_permissions.go` near `:309`); and `SiteDrives` (`drives_sites.go` near `:84`). A Business drive ID holds `!`, which the item routes send as `%21`; a site ID holds commas; a value OneDrive answered that held `/`, `?`, `#` or `%` would change the request. No failure is known: this is one rule for the IDs in a route, not a seen fault.
- **Change**: one helper builds the `/drives/{id}` prefix of every route and one the `/sites/{id}` prefix, and every route above goes through them; each puts its ID in by the one rule the DoR decides (escaped as one path segment, as item IDs are, or as it is, with a refusal of `/`, `?`, `#` and `%`); the helpers' comments and `spec/design/graph-client.md` state the rule.
- **DoR first**: what Graph's documentation shows for a drive ID with `!` and a site ID with commas in a path, and what is documented or known for each escaped (`%21`, `%2C`); the test accounts on this host are personal, whose drive IDs hold neither character, so if a Business answer stays unknown the decision is the raw form with the refusal; every route built from a drive or site ID in product code (`"/drives/` and `/sites/` in `internal/graph`), with file and line.
- **Tests first**: a request-line test in the form of `TestItemRoutes_EscapeTheItemIDAsOnePathSegment` for every drive and site route, with a made-up drive ID holding `!` and a site ID holding commas, holding the chosen rule; seen failing on one route put back.
- **Live proof**: on the test account, `ls`, `stat` and a one-shot `sync` of a private folder removed afterwards still work. A personal drive ID holds no character the rule changes, so this shows only that nothing broke; the escaped forms need a Business drive this host does not have.
- **Order**: not with W95 (`internal/graph/delta.go`) or a branch that edits `internal/graph/items.go`, `items_fetch.go`, `socketio.go`, `drives_identity.go`, `items_permissions.go` or `drives_sites.go` (check at dispatch). Lane x. Size S.

#### W111. Logout warns once, the missing-drive refusal names the config file given, and a default folder is computed one way

- **Source**: the leftovers of #950 (W70), its body's "Not in this change": the first report's "Not fixed" 1 and 3, and the second review's "Seen outside this delta" (two items, same on both commits). From reading, on main `5ce3ad30`.
- **What is wrong**: (1) `loadLogoutRecoveryState` (`internal/cli/auth_flow.go` near `:279`): when the catalog fails after the config loads, the config's warnings print twice, once as the `validatedWarnings` it is given and again from its second lenient load of the same file. (2) `validateConfiguredDrivesInCatalog` (`internal/config/validated_state.go` near `:73`): the refusal for a configured drive missing from the catalog says "is in config.toml" even when `--config` names another file. (3) A shared drive whose section sets `display_name` and no `sync_dir`: its default (`defaultSyncDir`, `internal/config/drive.go`) takes the display name from the catalog account, which a shared drive does not have, and is `~/OneDrive-Shared`, while `otherSyncDirs` takes the section's `display_name` and counts the same drive as `~/OneDrive-Shared/<display name>` for the other drives' collisions. Whether such a section occurs: unknown. (4) With a relative `XDG_DATA_HOME` and a home folder set, the warning `LoadValidatedState` gives names the data folder below the home folder, while `DefaultDataDir` returns the path relative to the working folder. Which folder the client writes then: unknown.
- **Change**: (1) logout's recovery prints only its second load's warnings, and the `validatedWarnings` parameter goes. (2) The refusal names the config path the load was given. (3) The default and the collision list read a shared drive's name from one place, the one the DoR finds the writers of a shared section use. (4) The warning names the data folder the client uses, or the client ignores a relative `XDG_DATA_HOME` as the XDG Base Directory specification asks ("relative paths are invalid and should be ignored"); the DoR decides which.
- **DoR first**: each item still true on main, with file and line; for (3) every writer of a shared drive's section (`drive add`, the shared-folder add by name) and whether any leaves `sync_dir` empty, and the folder `status` shows for such a drive; for (4) where `DefaultDataDir` reads `XDG_DATA_HOME` and whether anything expands or refuses a relative value, and the same for `XDG_CONFIG_HOME`; the tests that match "is in config.toml".
- **Tests first**: a logout with an unreadable catalog and a config holding an unknown key prints that warning once; a drive missing from the catalog with the config at a path not named `config.toml`: the refusal names that path; a shared drive with `display_name` and no `sync_dir` beside a second drive: its folder and the second drive's collision list agree; a relative `XDG_DATA_HOME`: the warning names the folder the DoR found, or the relative value is ignored. Each seen failing first.
- **Order**: not with W105 (logout, `internal/cli/auth_logout.go`) or a branch that edits `internal/cli/auth_flow.go`, `internal/config/validated_state.go`, `internal/config/drive.go` or `internal/config/paths.go` (check at dispatch). Lane x. Size S.

#### W112. Creates at a waiting item's own landing below a folder moved here wait until it has left

- **Source**: the leftovers of #952 (W79), its body's "Not in this change"; found in its last review fix pass, reproduced on the branch and on main by the writer. The same on main.
- **What is wrong**: on OneDrive rename `Y` to `Z` and move `Z/f` into a new `Z/f/a/f`, then sync. Sync 1: here `Y` is moved to `Z`, the old `f`'s local move waits ("move waits for the next pass", Debug), and the local create of `Z/f/a` lands inside the old `f`. Every later sync: Debug waits only, WillRetry 2; here the tree is `Z/f/c.txt` with `a` inside the old `f`, OneDrive has `Z/f/a/f/c.txt`. Cause: `landingFolderHolder` (`planner_move_composition.go`) looks the target's folders up in the local snapshot on this computer's side, where the item is still at `Y/f`; the OneDrive side looks up through `snapshotPath`. `spec/design/sync-planning.md` names it as a known limit in the parking paragraph, as does the `holdOwnLandings` comment.
- **Change**: map the local lookup through the plan's local moves, as `planMove` gives the local path, so the create of `Z/f/a` waits until the old `f` has left `Z/f`; the next sync moves `f` aside and then into `Z/f/a/f`, and nothing is made inside the old `f`. Remove the known-limit sentences from the spec and the comment.
- **DoR first**: the reproduction on main with what each pass plans; every caller of `landingFolderHolder` and `folderHolder` on this computer's side, and whether the mapped lookup changes any of them.
- **Tests first**: the OneDrive-side row in `ownLandingCases` and the renamed-on-OneDrive case in `TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath`, each seen failing; the exhaustive check's `pairs.golden` refreshed only if it changes, and no run breaks only on the branch.
- **Order**: sync planner rows one at a time (not with W52, W53, W67, W68, W80, W92, W113 or W114; same file). Sync lane. Size M.

#### W113. A folder moved into different new folders at its own path on each side converges

- **Source**: the leftovers of #952 (W79), its body's "Not in this change"; found in its last review fix pass, reproduced on the branch and on main by the writer. The same on main.
- **What is wrong**: here move `f` into a new `f/a/f` while OneDrive moves `f` into a new `f/b/f`, then sync. Every sync supersedes 4 actions and leaves WillRetry 5 with nothing above Debug.
- **Change**: OneDrive's location wins as for any divergent move: this computer ends with `f/b/f`, keeps `f/a` as a new folder, and the sync finishes in one or two syncs. Where the divergent-move rule and the own-landing parking in `planner_move_composition.go` meet is sized in the DoR.
- **DoR first**: the reproduction on main with what each pass plans and which actions are superseded; which rule decides the divergent move and which the own landing.
- **Tests first**: a composition row and a RunOnce scenario, each seen failing on main; the exhaustive check as in W112.
- **Order**: after W112 (same functions); sync planner rows one at a time. Sync lane. Size M.

#### W114. Three folders moved into each other's new folders converge

- **Source**: the leftovers of #952 (W79), its body's "Not in this change"; found in its last review fix pass, reproduced on the branch and on main by the writer. The same on main.
- **What is wrong**: here move `H` aside, move `Y/f` into a new `Y/f/a/f`, move `Y` into a new `H/Y`, and move the old `H` into `H/Y/f/a/H`, then sync. Since #952 every sync fails one create ("parent folder H/Y has not been synced yet", WillRetry 4); main moves `Y` into the old `H` on OneDrive, makes `a` inside the old `f`, then waits at Debug.
- **Change**: the folders end where they were put on both sides in a bounded number of syncs, with nothing moved into or made inside an old folder on OneDrive. Sized in the DoR.
- **DoR first**: the reproduction on main with what each pass plans; which hold leaves the create of `H/Y`'s content without its parent.
- **Tests first**: a RunOnce scenario asserting the end trees, the bound and no create below an old folder on OneDrive, seen failing; the exhaustive check as in W112.
- **Order**: after W112 and W113 (same functions); sync planner rows one at a time. Sync lane. Size M.

#### W115. Every run the exhaustive planner check records as broken has a row

- **Source**: #952 (W79) adds the exhaustive planner check `TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge`; its writer's pair counts.
- **What is wrong**: of the check's 8,387 runs, 255 (154 pairs) break an invariant (content removed, sides not converged, something held at the end, or a delete not done) and are recorded in `internal/sync/testdata/exhaustive/pairs.golden`. Each of the 255 breaks on main too (main's planner breaks 824 of 8,671). No row says which shapes they are. Rows that may cover some: W52, W53, W67, W68, W80, W92, W112, W113, W114; which runs each covers, and what the rest are, is unknown.
- **Change**: group the 255 runs by invariant and shape; map each group to an existing row or to a new row with a reproduction and the expected end state; a group that is right as it is gets its reason in the spec. No product change; each fix row deletes its lines from `pairs.golden`.
- **DoR first**: the golden list on main at dispatch (rows merged since may have changed it); the invariant each line names.
- **Tests first**: none (an investigation); its output is plan rows and a report.
- **Order**: any time after #952; read-only on the code. Sync lane. Size S.

## 7. Store-generation bumps

One PR changes `currentSyncStoreGeneration` (28 today): C3, to 29. Waves F and
I are not expected to change the schema (I's cut-over would, and is not in this
program). The installed client refuses a store of another generation by design,
so that install goes in this order:

1. Stop the wrapper (`kill -TERM` of `onedrive-go-watch` in a terminal session).
2. `onedrive-go-install <checkout>` (the new binary first).
3. `onedrive-go drive reset-sync-state --drive 'personal:test-account@example.com' --yes`.
4. Restart the wrapper (run `onedrive-go-watch` again in a terminal session).
5. `onedrive-go status` until it reads "Up to date"; the 19 files of the test
   drive are all present under `~/sync-test-root`.

If another PR turns out to need a bump, it follows the same order and is added
here.

## 8. Risks and how each is held

| Risk | Held by |
|---|---|
| A refactor changes sync behaviour unnoticed | The reconcile golden matrix, the model and planner seed tests, the stress lane, and a full live e2e tier after every wave. A behaviour change and a pure move are never in one PR. |
| The generated move of the split is too large to review | It is reviewed as a tool and a verdict: the model must report zero violations on the result, and the tool's output is reproducible from the commit before it. |
| Two lanes collide in one file | Lanes are defined by directory; `orchestrator_watch.go` is split first; cross-lane PRs run alone; the outer lane pauses for the move. |
| A nightly failure appears mid-program | Every start stage claims it; the PR fixes it or releases it to the plan PR that owns it. |
| A change needs a decision only the owner can take | Taken on the analysis's recommendation, recorded in section 3, reported at the end. Nothing here touches production data. |
| The watch client on the test drive breaks after an install | `status` after every install; a break stops the lane until fixed (the previous commit is reinstalled first). |
| An analysis claim is wrong | Step 3 of every PR checks it against the code; a failing-first test settles it. A PR whose defect cannot be reproduced lands as the test and a note. |
| The program outlasts one working session | The status table in section 1 is updated at every merge and is the only record needed to resume. |
