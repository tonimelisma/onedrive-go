# Resume note

The one state file of the program. Keep it under 8 KB; the long history
before 11-20 is in git (`git log -p -- RESUME.md`). Updated 2026-11-20.

## Binding rules
- Owner 11-18: finish the refactor core first (`REFACTOR-ORDER.md`,
  R1-R12); polish later in big misc PRs from `PARKED.md`. No new plan
  rows. A finding in touched code is fixed in that PR, anything else is
  one line in PARKED.md. About 5 rows per PR; one review per PR; a
  delta review only after blocking findings. `-race` on touched
  packages once, at ship. GitHub does not run CI for us.
- Trailer `Co-Authored-By: Claude <noreply@anthropic.com>` (no model
  name). No plan ids in commit messages.
- Heavy runs: `TMPDIR=~/.cache/onedrive-go-tmp`, `-p 1`, one
  at a time (`S/lockq.py`, `S/heavy.lock`). Ship-time `-race` runs the
  SHORT tier only (`-race -short`, owner's test cut 11-21); full tiers
  are nightly. Live e2e one test at a time, test account only.
- Ship procedure: `S/procedures.md` lines 22-99.
- Work fully on my own; never stop for or ask the owner (11-20). There
  is NO "confirm before deleting files" rule: that line came from a
  one-off 11-19 settings-fix request and was wrongly generalized.

## State
- 100 PRs merged. Latest: R7 as **#963 `f2392533`** (engine
  construction), installed and verified; plan rows set. R6 was #962
  `f0ac23e5`. Main = `f2392533`. R7's race run: internal/sync alone
  6516 s (the T1 "before").
- R1-R7 done (R3b carried G5, G7; P5 carried W57, W34, W20; C3 moved
  to R9).
- Parked during R6: R6-r1, R6-r2, R6-4 (generator skips the `race`
  tag).
- Nightly: 4 parked tests (R2-n1, R2-n2, W101-n2) are claimed at each
  start stage and released.

## In flight
- Pause lifted by the owner 11-20 ~22:00 ("continue and finish the
  work"). T1 builder (`a26219c4e87a374d6`, `test/per-pr-speed`, items
  1-3 written uncompiled at pause) resumed. 01:00 11-21: items 1-3
  committed locally (head `52af46ec`; fsync off in test stores, planner
  queries prepared once, exhaustive tests whole only in coverage builds
  = nightly). Host never quiet (load 17-40, backup, Immich), so I told it
  to measure interleaved A/B pairs and decide on the CPU-time ratio
  (< 0.5), then finish: gates, push, install, bookkeeping. 07:50: owner angry ("promised to cut test
  time, instead running tests for 5 hours"). I killed the A/B series
  (my mistake to allow it). Its data: internal/sync short 1495 s base ->
  1038 s branch, only -30%; branch fails 2 tests. T1 agent resumed with
  a 2-hour budget, no more measurement series: fix the 2 failures, move
  scenario-style tests (248 TestRunOnce_ etc.) to nightly with a sample
  per PR so internal/sync short is < 25% of today, ship-time -race runs
  the short tier only. One timing run at the end. 08:10: fixes `9d165658`, short-tier
  1-in-8 sample of engine tests `d2756bcf`; its final chain
  (`S/T1-final.sh`: pre-pr, one timing, item-2 agreement run) is
  running; result `~/.cache/onedrive-go-tmp/t1/final.txt`. I finish the
  push/install/bookkeeping myself.
- C3 builder was STOPPED BY THE OWNER over Remote Control 11-21; do
  not restart it unless the owner asks.
- R9 paused (owner's test-first priority): F1-F4 built, head
  `9f9c9471` on `refactor/state-planner` (pushed, no PR). Go/SQL agree
  on all inputs; 100k plan 16.6 s -> 4.7-7.6 s, F4 stays. No
  generation change. pre-pr owed (3 runs each stopped on one small
  finding, all fixed). C3 NOT built: its write paths are in failure/
  trial/permission code, not F1-F4's; FK form decided in R9-report
  (`blocked_scope_key` -> `block_scopes`, NULL under read scopes,
  start-up repair narrowed). C3 becomes its own small PR after R9.
  Parked R9-1..R9-4. Its stray worktree `S/R9-testwt` is removed.

- Owner 11-20 18:20: the slow unit tests are a smell to cut ("with a
  chainsaw"). Profiling agent started (read-only, under the heavy lock),
  report `reports/test-profile.md`. Owner 18:35: "work on reducing the tests
  first". So: R7 finishes its running ship (race run already started;
  stopping it would cost more and the test PR would then conflict with
  it); R9 told to pause heavy runs; the test-cut PR goes next, before R9.
  Owner 18:40: cut per-PR test time (pre-pr and ship `-race`) by more
  than half, moving slow tests to a nightly or weekly tier; numbers are
  for me only; never stop for the owner. Profile done
  (`reports/test-profile.md`): causes are per-plan SQL re-prepare (77%
  of short-run CPU) and fsync on test stores; the exhaustive planner test
  alone is 951 s non-short. T1 builder started on `test/per-pr-speed`
  (synchronous=OFF test stores, prepare once, exhaustive/reverse
  dispatch to nightly), report `reports/T1-report.md`. Then T1 review
  and ship, then R9 resumes (pre-pr on rebase), then C3 alone.

## Next
R8 (now unblocked, after T1); R10 after R8+R9; R11 after R9+R10; R12 gated on D14's
soak. Then the misc polish PRs from PARKED.md.

## Open, owner's call (not blocking)
- Delete local branch `test/crash-model-replacing-move` (needs `-D`).
- #953 trailer history rewrite; NVIDIA WebFetch rules.
- Missing e2e: NFC/NFD twin; refused case-variant overwrite (polish).
