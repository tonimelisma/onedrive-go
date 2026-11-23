# Finish the refactor (owner, 2026-11-21)

Scope is only this list. No measuring, profiling or tool tuning unless a
gate fails, and then only the fix for what failed. Every update names a step.

## PR 1: what is already built
- [x] 1a. One branch: the test-time cut's committed work (uncommitted lint
      edit dropped) + R9 F1-F4 + C3 (finish C3 if not built)
- [x] 1b. Rebase on main, build, vet, short tests of changed packages
- [x] 1c. Pre-pr once, PR, merge, install, one CHANGELOG line

## PR 2: every remaining refactor row
- [x] 2a. R8 (E5, B10, E9, E10, E12), on one branch
- [x] 2b. R10 (F5, F6, F7, I1, I2), same branch
- [x] 2c. R11 (G9, G10, G11, I3), same branch
- [x] 2d. Gate once, one review (file operations change), merge, install

## PR 3: the rows PR 2's builder found far bigger
- [x] 3a. E10, E12, F6, F7, I1, I2, I3 on `refactor/finish-3` (on top of PR 2)
- [x] 3b. Rebase after PR 2 merges, gate once, one review, merge, install
  - 2026-11-22: merged as #969.
  - 2026-11-22: rebased on 6ae83a1e and pushed (`f0e2da88`); gate stopped unfinished at the owner's pause; restart it from scratch

PR 2 note: built E5, B10, E9, F5, G9, G11; G10 closed without code (an unchanged tree is never rehashed, so there is nothing to save).

## Last
- [ ] PR 4: every row of `PARKED.md` still open, plus R12 below; work moved to cloud sessions on 2026-11-23 (no installs, no host soak).
- [x] 3. R12 (B11) if D14's soak has run; otherwise say so to the owner
  - 2026-11-23: shipped in #974 with deterministic tests in place of the soak (owner decision below).
  - 2026-11-23 (owner decision): the 72-hour soak is replaced by deterministic tests. Prove that the multisync watch loop never blocks outside a step it declares to the heartbeat (`internal/multisync/loop_heartbeat.go`), then ship R12: the systemd keepalive is sent only while `LoopAlive` reports progress. A false alarm costs a restart, not data. Ship it with the parked polish in PR 4.
  - 2026-11-22: the heartbeat (B10) was first installed with #966 at 00:32 on 11-22; D14 needs 72 h of watch on the test drive without a false "no progress" line, so R12 can start no earlier than 2026-11-25 00:32. Told the owner.
  - PR 3 (3a) was stopped by the owner over Remote Control on 11-21; its branch `refactor/finish-3` holds partial work; not restarted.
