# H8 ship report: outer silent failures (`fix/outer-silent-failures`)

Merged as #947, squash `b00e6da3`, at 2026-11-17 20:57 PST. Installed from main at 21:01 PST; `onedrive-go --version` prints b00e6da3. Post-merge stage passed by 21:20 PST; the worktree and branch are gone.

## Review fixes (second review)

- Should-fix 1: new test `TestLogout_EndsTheSignInRequirementOfTheAccountWhoseTokenItDeleted` (`internal/cli/auth_logout_clear_test.go`). Two accounts at one address, both signed in and both marked, one of them with a configured drive, then `logout --account` through `runLogoutWithContext`, in both orders. It asserts the configured account's token is gone and the other's kept, the configured account's requirement ended and the other's kept. Passes at HEAD; fails (personal case, both requirement asserts) under the reviewer's mutant that passes the first record by email from `executeLogout`, run by `go test -overlay` after the rebase. Cited in `spec/design/config.md` beside the direct `executePlainLogout` test.
- Note 2: `require.True(t, helperStarted.Load(), "the download ran")` before `<-helperDone`, so the helper-panic sync test fails instead of hanging. Passed three times under `-race`.
- Note 3: commit message now says "Before, only the log had the panic." "the parent commit" became "the first commit", since the squash joins both messages.
- Note 4: by design, unchanged; named in the PR's behavior changes.

## Rebase and conventions

- Rebased onto `4886b38c` (#946) with no conflicts; the fixes were amended into the second commit. Build, `go vet` plain and with all tags: clean. gofumpt clean.
- Lint rules of #946 (testing failure methods, Sleep, output): the pre-pr lint reported 0 issues.
- Tiers: the only new `internal/sync` test the stress placement rule names (its name holds "Shutdown") is declared a shutdown member; the other new sync test matches no prefix or phrase. No e2e test added. `RealRepoIsClean`: 4 PASS.

## Gates

- Start stage: manifest ready; the newest nightly passed; nothing claimed.
- Pre-pr (`pr` profile, behind the lock): passed.
- PR #947 opened; pre-merge passed (no review threads); every required check passed before the merge.
- Post-merge from the main checkout: passed.

## Bookkeeping

- `plan.py set H8 "merged 947 b00e6da3"`.
- New row W105 (table and section): one account view per canonical ID, so the second account at a shared address gets a status row, a requirement entry and a selector; logout deletes the token of the account it names; the live overlay asks each drive with its owner's token.
- One line for #947 in `notes/onedrive-go-merged-prs.md`; a CHANGELOG entry; the client bullet of `CLAUDE.md` (installed build, 85 PRs #860 to #947, worktree list).

## Left undone

- Not proven live: the login path (interactive sign-in) and the two-accounts-at-one-address cases (unit tests only).
- Named in the PR as unchanged from main and in no plan row: a command ended by a panic on its own goroutine writes no performance summary (`reportMainPanic`); a keepalive panic joined to a failed watch start is dropped from the startup error (`formatWatchStartupError`).
- STATE.md, RESUME.md and the H8 row report's "Merged as" section were not edited.
