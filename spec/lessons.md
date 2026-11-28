# Lessons

This file holds what the code does not make obvious. It has three kinds of
entry: rules learned the hard way, research findings worth keeping, and open
problems. It is bounded. An entry is deleted once it moves into a test, a
design doc, an ADR or `CLAUDE.md`. Where a finding already lives in a current
doc, this file only points there. Several entries were measured on a 2-core,
6.9 GB development host, and they say so. The raw records (review tracks,
analyses, per-change reports, plans and state files) are in git history before
commit cae38767, under the path spec/history/.

## Rules learned

**Code structure**

- **Delete the wide pointer.** Adding a narrow capability next to a wide back-pointer does not work.
  - When the run flow still held the whole `Engine`, it reached through it 228 times.
  - 35 of those uses bypassed a narrower dependency set that already existed, because nothing stopped them.
  - Six production nil-checks existed only so tests could build a flow without an engine.
  - The compiler enforces the boundary only once the pointer is gone ([design/sync-engine.md](design/sync-engine.md), "Run Capabilities Are Passed, Not Reached For").
- **A store mutator mirrored by a cache returns what it changed.**
  - A mutator that returns only an error or a bool makes the cache guess.
  - The retry ledger once had nine hand-written mirror sites and no check that cache and store agreed. It diverged four ways: a cached row outlived its store row, a release edited keys the store had not changed, and two more.
  - Now one file writes retry work, and a cache-equals-store invariant runs after every completion ([design/sync-store.md](design/sync-store.md), "Outcome writes").
- **A lock that guards nothing is a false contract.** It claims concurrency that does not exist, and it hides an accidental access from a second goroutine from the race detector.
  - Loop-only state carries no mutex. It carries a test-only owner check instead (R-6.3.4).
- **Nothing per-plan goes into a closure or cleanup that outlives the plan.**
  - Watch cleanup once captured the first plan's dependency graph, while every plan install replaced it.
  - So its "draining in-flight actions" log could never fire.
- **An invalid state is refused by a table, not by setter discipline.**
  - The watch lifecycle once was a three-value enum with unconditional setters, a variadic mode flag and nil-ed channel fields.
  - A setter called in the wrong place could leave draining and reopen admission during shutdown.
  - It is now one total transition table in `internal/sync/engine_watch_phase.go`. That table is described in code comments only.
- **A config called immutable must be built whole or refused.** The executor config was documented as immutable and had 14 mutation points.

**SQL and the store**

- **Foreign keys only where the code already asserts the reference.**
  - The one key is `retry_work.blocked_scope_key` → `block_scopes`. It has no cascade and is checked immediately.
  - A cascade would silently delete retry work whenever a scope was deleted by mistake.
  - A deferred key fails only at COMMIT, far from the statement that broke it. Deferral would also not have fixed the real bug, which was parent and child written in separate transactions.
  - Scope rows are updated in place. `INSERT OR REPLACE` is a delete plus an insert, and the delete would hit the key.
- **Why the other references are not foreign keys:**
  - `baseline` ↔ `remote_state`: a row on one side only is the create or delete signal.
  - `pending_creates`: written before the item exists.
  - `held_deletes`: rewritten on every plan.
  - `delete_ledger`: outlives its baseline row on purpose.
  - `observation_issues.scope_key`: holds read-boundary tags, not scope rows.
  - `baseline.parent_id`: a top-level item's parent is the remote root, which has no row, and a folder rebind re-points children before the new parent row exists.
- **Every schema change raises the store generation.** Constraints alone (CHECK, NOT NULL, REFERENCES) are invisible to the canonical-schema comparison, which reads only table and column names ([design/sync-store.md](design/sync-store.md), "State-DB Diagnosis And Reset").
- **Path-prefix SQL uses a range, never `substr()`/`length()`.** Write it as `path >= p||'/' AND path < p||'0'`.
  - On 100k rows, a 1,000-row subtree delete takes 6 ms with the range and 44 ms with `substr()`.
  - 100 single-file deletes take 12 ms against 4.4 s.
  - A correlated `substr()` prune cost 10 s, then 65 s, then 619 s per replan at 11.4k rows as the baseline filled.
  - Source: [design/sync-store.md](design/sync-store.md), "Observation writes".
- **modernc SQLite compiles every `ExecContext`/`QueryContext` again.** Only a prepared `*sql.Stmt` is reused.
  - Cost per row: 170 µs for one statement per row, 48 µs prepared, 16 µs in a 200-row chunk.
  - Compiling is about half of every per-row loop at drive scale.

**Shutdown and contexts**

- Shutdown cancels at the first signal and never drains. Only an action's outcome commit and best-effort cleanups outlive it, each on a bounded detached context ([design/cli.md](design/cli.md), "Exit Status").
- Housekeeping that fails only because the context was canceled logs at Debug. Otherwise a clean shutdown prints a false WARN.

**Tests and CI**

- **Wait for content before acting on a daemon in e2e.** A new remote file's name is visible while it still has 0 bytes.
  - A test that stops, edits or races a daemon first waits for size (new file) or content (overwrite).
  - An audit of 80 name-only waits found 18 racy ones ([reference/live-recurring-families.md](reference/live-recurring-families.md)).
  - Local download waits are safe: a download becomes the final name by one atomic rename after hash, mtime, mode and fsync are set.
  - So for a new local file, existence is proof; for a modified one, only the content is.
- **Choosing a converge helper for a one-shot sync in e2e:**
  - Retry-only re-running does nothing for delta lag. A pass that must see a remote change needs a predicate converge.
  - Never wrap a pass meant to end in a permanent per-item refusal: it runs to its timeout.
  - First-pass assertions, dry runs and idempotence checks stay single passes. A converge helper would hide a regression where one pass should have been enough.
- **A new race test is a true negative on fixed code, not a reliable positive on broken code.**
  - One race reproduced 2 times in 48 under `-race -count=50`, then 0 times in runs of 50 and 60.
  - Another test caught an unguarded read on the old code in only 5 of 20 runs.
- **Deadlines follow measured profiles, never literals.**
  - The `internal/sync` race run took 640 s on Linux and 892 s on macOS against a 20-minute deadline.
  - A literal 40-minute stress deadline was outgrown within days.
  - Fixed unit deadlines scale with cores ([design/system.md](design/system.md), "Verification Policy").
- **Reading `go test` timings on a 2-core host:**
  - Summed per-test elapsed time is about 1.8x package wall time, because parallel tests queue for CPU. Rank tests by their own time and total by package wall time.
  - The long tier is a few giants: the top 10 tests are 76% of it.
  - The short tier is a long tail: about 300 engine run-once tests at about 0.1 s CPU per plan. Speeding up store and plan pays off there more than skipping tests.
- **The filesystem dominates sync test time.** Test stores already skip fsync.
  - Moving TMPDIR to tmpfs cut the long sync tier from 1,760 s to 720 s and the short tier from 307 s to 190 s.
  - It cut the exhaustive planner test from 951 s to 217 s. That test made 16,159 `fsync` calls in 60 s.
- **A differential harness finds what exhaustive tables miss.** Run the same generated step sequences against `main` and the fix, on both sides, then compare convergence, the final trees and superseded counts.
  - This is how the move/folder-create cycle classes were found: 666 sequences, 1,332 runs.
  - The exhaustive known-failures list `internal/sync/internal/syncplan/testdata/exhaustive/pairs.golden` is now empty.
- **Repository self-checks have to be run explicitly.** Removing a test can delete the last citation of a registered identifier. Run the `RealRepoIsClean` tests in `internal/devtool` before a gate, because a targeted `-run` skips them.
- **File dates past 2262 do not hold.** Go's `os.Chtimes` wraps times outside 1678–2262, and macOS did not hold a file date past 2262 (cause not checked). Tests that set extreme times accept the clamped value.

## Graph throttling research

The research behind the release ramp. The implemented policy is in
[reference/graph-api-quirks.md](reference/graph-api-quirks.md) (throttling) and
[design/sync-engine.md](design/sync-engine.md) ("Release ramps and activation
retires admitted work"). What follows is the research those docs do not carry.

**Sources:** Microsoft Graph throttling and throttling-limits pages; "Avoid getting throttled or blocked in SharePoint Online"; OneDrive scan guidance; JSON batching; `createUploadSession`; The msgraph-sdk-design RetryHandler spec and kiota-http-go's retry handler; rclone's pacer and OneDrive backend; abraunegg/onedrive's source and issues #815, #1304 and #3862; Theory: Jacobson 1988, Chiu & Jain 1989, RFC 5681, Brooker's "Exponential Backoff and Jitter", the Google SRE book's "Handling Overload".

**Requests made while throttled count against the limits.**
- Graph, SharePoint and the scan guidance all say so.
- The scan guidance tells sync engines to pause all requests during a 429/503 wait, especially in multi-threaded clients, because extra calls extend the penalty.
- Persistent offenders get a lasting 503 block.

**Retry-After is present on files 429/503 answers.** Only OneNote resources omit it.
- Observed values run from 3 s to 3,515 s. The 3,515 s value came from a personal account after about 12 `createLink` calls.
- Absent the header, Microsoft says to use exponential backoff.
- Upload-session guidance: exponential backoff for 5xx on fragments; for other errors, limit the attempts instead.

**RateLimit-\* headers are not supported.** The SharePoint page dropped them in its 2026-08 revision. For consumer OneDrive, nothing documents them either way.

**Published limits (business/SharePoint).** Microsoft says these "may change at any time".
- Per user: 3,000 requests per 5 minutes (about 10/s), 50 GB/h ingress, 100 GB/h egress.
- Resource units per request: 1 for a single-item get, a delta with a token, or a download; 2 for listing children or any create, update, delete or upload; 5 for anything touching permissions, including `$expand=permissions`.
- Per app per tenant:
  - 1,250 to 6,250 RU per minute, scaling with licences;
  - 1.2M to 6M RU per 24 h;
  - 400 GB/h ingress and egress.
- Per tenant: 18,750 to 93,750 RU per 5 minutes.
- Global: 130,000 requests per 10 s per app.
- Estimate: divide an RU limit by 2 to get requests. For the smallest tenant that is about 625 requests/min, about 10/s.
- Consumer OneDrive publishes no numbers.

**Upload fragments.**
- Fragments go to the `uploadUrl` host with no Authorization header.
- They are sent in order, each a multiple of 320 KiB and under 60 MiB; 5–10 MiB is recommended.
- Whether each fragment PUT is metered as a request is undocumented (unconfirmed). The byte limits certainly apply.
- Fragments cannot be batched.

**JSON batching.**
- Up to 20 requests per batch.
- Each sub-request is throttled on its own, while the batch itself answers 200.
- Each sub-request's response carries an `x-ms-resource-unit` header.
- A sub-request may depend on one other request only.
- No batch body size limit is documented.
- The client does not batch today.

**Traffic decoration.** "Well-decorated traffic will be prioritized." The format is `ISV|Company|App/Version` ([design/graph-client.md](design/graph-client.md) carries the client's value).

**Prior art.** No client persists throttle state across a restart.
- **rclone:** one pacer per remote spaces every request start.
  - The gap is set exactly to Retry-After, doubles on errors without the header (capped at 2 s), and decays x3/4 per success down to 10 ms.
  - That is multiplicative increase and decrease on rate. The ramp after a wait sums to about 4x Retry-After.
  - Concurrency stays fixed by flags.
  - `--tpslimit` is an optional process-wide token bucket.
- **abraunegg:** per-request sleeps with no shared state.
  - 120 s when the header is missing, 30 s for 5xx, up to about a year of retries, no jitter.
  - The 8–16 thread cap is the real limiter, so its approach does not carry over to thousands of admitted items.
- **Graph SDK (Go):**
  - 3 retries by default (10 at most), 3 s doubling plus up to 1 s of jitter, a 180 s cumulative cap.
  - The design spec says 10 retries and a quadratic interval: spec and code disagree.
  - It is a retry primitive, not a load controller: nothing stops new requests during the wait.

**Theory, and how the release ramp maps to it.**
- Retiring admitted work when a throttle activates is the circuit breaker's open state and the conservation principle. No prior-art client does it.
- One probe after the wait is the half-open trial.
- Doubling re-admission is slow start. Pure doubling overshoots by up to 2x on each cycle, and every overshoot costs a full Retry-After. That is why the ramp keeps a threshold at half the batch that drew the 429 and grows additively above it (RFC 5681).
- AIMD (additive increase, multiplicative decrease) converges to a fair share; multiplicative increase oscillates (Chiu & Jain).
- Jitter belongs on computed backoff and on spreading a re-admitted batch, never on the server-given wait (Brooker).

**Not adopted yet** (no issue):
- Google SRE adaptive throttling: track requests against accepts per drive over 2 minutes, and lower the ceiling when 429s rise while no scope is blocked (K = 2).
- A per-drive token bucket that caps the steady rate independently of worker count.
- Running the client through Microsoft Dev Proxy's `microsoft-graph-rate-limiting` preset, which fakes 429/Retry-After on real URLs with no account.

## Mutation checks

**Method**
- In a clean worktree, apply one semantic mutation per data-safety invariant, for example `if false && <guard>`, or a comparison forced true.
- Run only the focused tests that cite the invariant, with `-count=1` and no `-race`.
- Revert with `git checkout -- .` and confirm `git status` is empty.
- A mutation that does not compile is a non-result. Discard it and mutate differently.
- Expected result: red. A green result means the requirement is marked verified on the strength of a test that cannot see it.

**Result** (17 mutations): 16 went red and one stayed green.
- The red ones covered: no remote delete without a baseline; orphan live lookup; the hash check before a local delete, and its size+mtime fallback; both delete-hold rules; root identity; a root read error stopping the scan; an unrepresentable synced path counting as unavailable; mount points as read boundaries; the local trash; the free-space check; partials never observed; equality needing both hashes; upload hash verification.

- The green one: removing the download's file fsync before the rename left every test passing.
  - The recording fake logged a step named "chmod,fsync" on the reopen, not on the sync.
  - A fake cannot wrap `*os.File.Sync`, so no test could see the call.
  - Fixed: the fsync goes through the `syncFile` seam in `internal/driveops/transfer_manager.go` and is asserted as its own ordered step, failure branch included.

**Lessons**
- A step name in a recording fake is a claim. Check that it is recorded where the step actually happens.
- A call on a concrete OS type needs a seam before any test can observe it.
- Defence in depth makes single-layer mutations look covered at the engine level. For example, the empty-side delete hold also catches a disabled root-identity check, and only the unit test sees the scan's own check.
- Two invariants were pinned thinly: the foreign-mount check by one unit test, and the owned-partial exclusion by one filter test with no end-to-end case. Re-run this exercise after large refactors of delete, scan or transfer code.

## Why internal/sync is split

The end state, seven packages under compiler, `internal` and depguard
enforcement, is in [design/system.md](design/system.md) under "The sync tree's
layering is machine-checked". The reasons behind it:

**Before the split.** `internal/sync` was one package.
- 169 production files (47,659 lines) and 193 test files (64,895 lines).
- Every unexported name was reachable from every file. That is how the engine back-pointer, the off-loop writers and the cache mirrors spread unnoticed.
- A name-based layering check by file family could not stop it.

**The cut follows authority, not file families.**
- Vocabulary goes in `syncdomain`.
- The shortcut-root state machine goes in `syncshortcut`.
- The one store goes in `syncstore`.
- The three stages, planner, observers and executor, are private under `internal/sync/internal/`.
- The engine in `internal/sync` is the only package that wires stages to the store.
- Rules that follow:
  - **No stage imports the store.** The executor reaches it through its own small interface; observation reaches it through the hash-cache interface.
  - **No stage imports another stage.**
  - **No re-export layer.** Aliases in `sync` would hide the boundaries the split exists to create.
- Scheduling later got its own package, `syncsched`.

**Turning the file families into packages as they stood was illegal Go.** It gave 13 import cycles and 119 illegal references.
- The legal cut moved 111 declarations, exported 444 identifiers, and replaced 39 unexported fields reached across packages (in 11 structs) with methods or interfaces.

**Method that worked.**
- Before touching code, a declaration-level model type-checked the package and attributed every reference to its target package. The verdict was 0 violations, 0 cycles and 0 illegal test references on both the Linux and macOS file sets.
- Then the work was prepared in place: export, move declarations to single-target files, add the seams, split the tests. Every step left one compiling package.
- The move itself was generated by a tool and reviewed as "tool plus verdict", not as 360 hand-edited files.
- A behaviour change and a pure move never share a change.
- Engine fixes that changed what crosses a boundary landed before the split. The rest landed after it, where the smaller engine package is faster to test.

**Considered and not done, on value:**
- a split by one-shot versus watch mode;
- moving shortcuts into `multisync`;
- a re-export alias layer;
- a symbol-accurate layering check (depguard plus the compiler are enough).

**Not measured by the model:**
- which `//nolint` lines go stale;
- coverage under `-coverpkg`;
- the wall time of seven test binaries against one.

## Multi-agent work and context

These rules come from running a long refactor with one coordinating session,
writer agents in their own worktrees, and reviewer agents.

**Context budget (measured)**
- Compaction came at about 167K tokens. The first request after one was 134–137K with an 80–88 KB summary, but 92K with a 10 KB summary.
- Per cycle, the session's own output was 30–40K tokens and tool results 9–15K.
- The tool list was 238 KB, 186 KB of it tools never used. Denying the unused tools took the post-compaction request from 92K to 66K, about 100K of room per cycle.
- Three compactions in a row happened because the plan and the analyses (790 KB) were re-read whole after each one.
- Rules that followed:
  - never load a large plan or analysis whole: read one entry, or a section by heading and line range;
  - a compaction summary is three short paragraphs: the goal and the newest user instruction verbatim, where to resume, and what is in flight;
  - working knowledge lives in files, not in the summary;
  - state is refreshed after every event (merge, hand-back, push, user message), before the next action.
- Keep tool output small. Run builds, tests and lint through a quiet runner that logs everything and prints the exit status and tail. Grep single files with `-l` or `-m N`; never count matches per file across a tree; never print files that hold very long single lines.
- A hook that refuses large reads in the coordinating session (big files, unbounded diffs, CI logs) works. It does not cap a subagent's hand-back, so hand-backs need their own word limit.
- One wide `grep -o` on an instruction file re-attached the whole file to context. Keep greps on such files few.

**Briefing builders**
- **Keep briefs short and in a file.** About 50 lines naming:
  - the rules file and the entry to implement;
  - the worktree;
  - the report file;
  - what done means;
  - the checks to run;
  - the form of the final message.
- **The launch prompt says only "read that file and follow it".**
- **The analysis a brief points to carries no authority.** The builder checks every claim against the code, follows the code where they disagree, and reports the difference.
- **Reports:**
  - at most 60 lines: what changed, tests added, checks run;
  - deviations, only those that change behaviour, scope or an interface;
  - "Also fixed";
  - "Not fixed", as `file:line`, the fault and the fix.
- **Final message:** at most 200 words.
- **Fix what you find where you are.** A smell in a file the change touches is fixed in that change. Anything else is written down and becomes planned work the same day. A "not fixed" entry is never accepted as is: fix it, send it back, or plan it.
- **Re-read every brief against the standing rules before launching.** One draft handed the builder a live test that only the coordinator may run.
- **Builders run only the tests of the packages they touch.** On a 2-core host there is one lint at a time (one golangci-lint run takes about 4 GB) and one full verify at a time. Every heavy run goes behind a host-wide lock.
- **Builders never push or open pull requests, and never run live tests.** Live proof of a product fix is the coordinator's.
- **Check every hand-back for:**
  - process words and plan IDs in code, tests, docs or the commit message (a repository reader has none of them);
  - model names in commit trailers (one slipped through once);
  - escape sequences such as backslash-u, which a subagent's Edit or Write once turned into the literal character.
- **A builder whose context fills ("Prompt is too long") cannot be resumed.** Replace it with a new builder and a brief that first checks the earlier commit against the reviewed one.
- **Up to three builders (usually two) at once on a 2-core host.** More just queue behind the heavy-run lock.

**Briefing reviewers**
- **Read-only, with no builds or tests.** If a question needs a run, the reviewer lists the exact command.
- **Review in this order:**
  1. the decision, item by item, with each deviation judged;
  2. every report claim checked against the code;
  3. faults (lost data, races, dropped or misclassified errors, missing deadlines, locks held across I/O);
  4. tests (each fix has one that fails without it, and an absence is proven by a handled signal, never a wait window);
  5. the "not fixed" entries;
  6. house rules.
- **Hand-back:** full findings go to a file, ranked must fix / should fix / note. The message is at most 250 words, with a one-word verdict.
- **The coordinator verifies each finding** with a tight grep or a short read before acting. It reads a diff itself only when the diff is under about 300 lines.
- **Mechanical diffs (moves, renames) are checked with tools, not read.**

**What went wrong, generalized**
- **Stale "still running" notes after a compaction were wrong.** Task notifications, the agent list and the worktree's `git log` decide what is running. Never read a subagent's transcript file.
- **Several of the coordinator's own hypotheses and planned review items were wrong** until checked against the code. Do not guess causes: say "unknown" when a cause cannot be checked, and never swap a refuted guess for a new one.
- **Read the clock before writing any time; never estimate it.**
- **A long idle period, with the cause unchecked, let a usable service window go by.** Keep a state file current enough that any restart can pick the work up at once.
- **Service outages need a written rule.** Merge on the checks that need no live service, and record which changes owe a live rerun. Reviews and local gates never wait for the service.
- **A clean rebase can still break the build** (a removed export, a lint rule tripped by the combination with main). After every rebase, run build, vet and the touched tests.
- **A user-quota limit on the temp filesystem failed a gate** while `df` still showed free space. Clean Go temp directories before heavy runs.

## Analysis findings

Measurements from the pre-refactor analyses and profiles. They are kept because
no current doc carries them. The incident, Graph and naming findings of those
analyses are already in the docs:
- [reference/live-incidents.md](reference/live-incidents.md): the 0-byte placeholder incident;
- [reference/graph-api-quirks.md](reference/graph-api-quirks.md): the upload session's eTag move, name collation and path limits;
- [design/sync-planning.md](design/sync-planning.md): the fail-closed decision table;
- [reference/rearchitecture-candidates.md](reference/rearchitecture-candidates.md): the rejected identity-keyed planner.

**Planner and store at drive scale** (140k–154k items, 2-core host)
- **Dependency building:**
  - The old all-pairs dependency build took 6.4 s at 11k actions, 148 s at 44k and 1,877 s at 154k. That was 91% of a 34-minute first plan.
  - The indexed rewrite takes 0.63 s at 154k ([design/performance-benchmarking.md](design/performance-benchmarking.md)).
- **Memory:**
  - The live heap after plan plus dependency graph is about 95 MB. Path views take 46.5 MB, and the copy of the action slice made while partitioning takes 21 MB (144 B per action).
  - The R-6.1.1 memory target (100 MB per 100k files) has never been measured end to end.
- **Local scan:** 113 s wall against about 27 s CPU. It is I/O-bound: 154k each of `openat`, `statx` and `read`.
- **Second baseline read:** the baseline copy read inside the planning transaction costs 3.7–3.9 s CPU per replan. It exists because of a real race with concurrent commits.
- **Commits:**
  - Committing remote state after a full enumeration took about 100 s CPU, 48% of it statement compiling.
  - The first local-state replace at 140k took 33 s, about 6 s of it compiling. Later passes write only the diff (0.6 s at 10k).
- **Publication:**
  - It is fsync-bound: 11,010 baseline updates took 214.9 s wall against 25.1 s CPU, 19.5 ms each.
  - At 138k items that is about 45 minutes of publishing after a first plan.
- **What does not help:**
  - `synchronous=FULL` costs only commits; no fsync appeared in read-heavy profiles.
  - Neither `temp_store` nor a larger `cache_size` is the lever. A bigger cache only moves the cliff; fixing the query is the cure.
- **Reading the timers:** `observe_time_ms` includes the SQL planning read, so a slow planning query shows up as observe time.

**Sync test-suite profile** (2026-11-20)
- **Compiling dominates the short run:** `sqlite3Prepare` was 77.5% of CPU, against 8% spent running queries.
  - Every plan recompiles the comparison query (about 0.1 s CPU).
  - Only the exhaustive planner test caches statements.
  - The planner tables are created and rolled back per plan, so a statement cache would also need those tables kept alive on the connection.
- **Lane costs:**
  - The race lane on the sync tree took 87 min on a loaded 2-core host and 12–16 min in CI.
  - The seeds lane takes about 11.5 min.
  - The 50-times concurrency race step took 11.5, 24.0 and 39.4 min on three nights, for the same 244 tests (cause unknown).
- **Per-change pre-PR timings:**
  - lint 2–2.5 min, unit short tier 4.3–6 min, 7–11 min in total;
  - `internal/sync` is the critical path at about 220–260 s.

**CI behaviour** (rules now in [design/system.md](design/system.md), "Verification Policy")
- **GitHub starts scheduled runs 6–12 hours late.** Hence the 36-hour staleness rule and the off-hour cron.
- **A scheduled run's checks did not reach the commit's status rollup.** Hence the nightly's own commit status and issue.
- **Before the nightly report existed,** the nightly was red for 30 nights in a row, and that ended only when the schedule was deleted.
- **e2e buckets:** the Go deadlines (15m/40m/40m plus reruns and 10 minutes of setup) must fit inside the job's `timeout-minutes`.

## Open problems

None of these has an issue unless an issue number is given.

- **Publication runs one fsync'd transaction per baseline update.** That is about 45 minutes at 138k items after a first plan. Batching a few hundred per transaction would make it seconds.
  - The path is `runPublicationDrainStage` in `internal/sync/engine_runtime_completion.go`, which calls `CommitMutation` in `internal/sync/syncstore/store_write_baseline.go`.
- **Per-row unprepared statements in observation commits.**
  - In `internal/sync/syncstore/store_write_observation.go` and `internal/sync/syncstore/store_local_state.go`.
  - The comparison query is recompiled on every plan (`internal/sync/syncstore/sqlite_compare.go`).
  - Preparing or caching them would save about 48 s per full commit at 154k items, and an estimated half or more of short sync test CPU (estimated, not measured).
- **`substr(path, 1, length(?))` prefix predicates remain in the store.**
  - In `internal/sync/syncstore/store_write_observation.go`, `internal/sync/syncstore/store_pending_creates.go`, `internal/sync/syncstore/store_remote_twins.go` and `internal/sync/syncstore/store_write_baseline.go`.
  - They break the range rule above.
- **A changed but unsynced large file is hashed again on every full scan.**
  - Only new files enter the hash cache (`internal/sync/internal/syncobserve/scanner_hash_cache.go`), and the cache is pruned for paths that have a baseline row.
  - So a slow, failing or held upload means one full read per replan.
  - A file created during watch is read three times: by the watch handler, by the replan scan, and by the upload.
  - Not measured.
- **Two new files whose names share a stored form but not a OneDrive key are not reported as a collision.**
  - Example: one name with U+212A KELVIN SIGN, the other with K.
  - `DirNames.Add` buckets by the key of the disk spelling, while `rivals` looks up the key of the stored name (`internal/sync/internal/syncobserve/dir_names.go`). So the second entry overwrites the first row.
  - Found by reading the code; no test covers it.
  - Folder/folder Unicode-form twins at one stored path are also outside the twin rule.
- **Reserved names are matched with ASCII case folding, not OneDrive's collation.**
  - The names are CON, PRN, AUX, NUL, COMn, LPTn, `_vti_` and a root `forms` (`internal/sync/internal/syncobserve/scanner_names.go`).
  - Unprobed: whether OneDrive refuses variants its key treats as equal.
- **Two production hooks are still installed by setters after construction** and read without a lock: `SetDebugEventHook` and `SetActivityHook` in `internal/sync`. Only a "call before a run" comment protects them.
- **The engine model is not fully deterministic.**
  - The watch model follows real time.
  - The engine model runs on a real temp directory, because hashing, uploads and followed-symlink reads go through `localpath` by absolute path.
  - The in-memory volume (`internal/synctree/memory_linux.go`) is Linux-only and replays incidents only.
- **Post-sync housekeeping deletes every `*.partial` file in the sync folder** when `ignore_junk_files` is on (the default), not just this client's own (`internal/driveops/stale_partials.go`). The docs do not say so.
- **The local trash has no size cap, free-space check or location setting.** It keeps copies for 30 days in the data directory, often on another filesystem, so trashing a large file writes a full copy there. A failure is safe: the delete is aborted.
- **The process never rotates its own log** (`internal/logfile/logfile.go`). A long `sync --watch` needs external rotation with copy-truncate, and the docs do not say so.
- **Doc drift.** Row DM-9 of [design/degraded-mode.md](design/degraded-mode.md) still says "drain what can finish", while the code and R-6.3.6 cancel.
- **Untested live:**
  - that the service refuses a replaced refresh token (tested only against the local token endpoint);
  - name collation on Business drives;
  - the upload-session eTag move on Business drives.
- **Slow tests nobody has profiled:**
  - `TestCrashRecovery_EveryCrashPointConverges` (about 26 s, not gated by `-short`);
  - the pending-create-intent replan test (8–21 s);
  - the shortcut-release refused-change test (11–25 s).
- **A subfolder moved onto its moved parent's old path does not converge in one plan.** The plan drops the cycle and loses nothing. This behaviour was kept deliberately; see `isolateDependencyCycles` in [design/sync-planning.md](design/sync-planning.md).
- **On FAT-family filesystems, a put-back can replace a save.** These filesystems have neither hard links nor a no-replace rename, so the put-back of a set-aside file is a check-then-rename. A save that lands in that microsecond window is replaced.
  - Kept on purpose: leaving the file under its hidden temporary name would make the next sync read it as deleted ([design/sync-execution.md](design/sync-execution.md), the local delete's known limit).
