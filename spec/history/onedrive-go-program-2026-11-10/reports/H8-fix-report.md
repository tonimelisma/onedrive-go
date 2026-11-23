# H8 fix report: outer silent failures (`fix/outer-silent-failures`)

New commit `08c15b40` on `a665e13c` (not amended), parent of that `6421bf98`. Not pushed. **Not installed**: after `git fetch origin main`, origin/main is `4886b38c` and the branch no longer contains it.

## Finding 1: two accounts on one address. Fixed the preferred way

**Why the preferred fix.** It turned out to be contained. Every writer, reader and clear of the mark has its account's canonical ID to hand, and nothing in the store or catalog format changes:
- the sync engine has its token owner (multisync's `tokenOwnerCanonical`);
- CLI proofs have the token's canonical ID;
- status has `RepresentativeTokenID`;
- logout has the token it deleted.

The one part that is not contained is showing the second account at an address on its own status row (see "Stays wrong").

**What changed**
- `config`: these now take a `driveid.CanonicalID` and use `AccountByCanonicalID`:
  - `MarkAccountAuthRequired`
  - `ClearAccountAuthRequirement`
  - `HasPersistedAccountAuthRequirement` / `LoadHasPersistedAccountAuthRequirement`
- `sync`: the engine has a new `EngineMountConfig.AccountCanonicalID` (and `engineInputs`), which multisync fills from `mount.tokenOwnerCanonical()`. The 401 mark, the startup check and the startup-proof clear all use it. They used to use `permHandler.accountEmail`.
- `cli`:
  - `authProofRecorder`, `attachAccountAuthProof` and `attachDriveAuthProof` are keyed by canonical ID; each call site passes the token's owner.
  - status marks and proves `RepresentativeTokenID`.
  - logout clears the owner of the token it deleted: `removeLogoutToken` returns it, and `executePlainLogout` resolves it with `TokenAccountCanonicalID`.
- Reader, `populateStoredAccounts`: the per-address view now takes its names, drive type and requirement from the first record by canonical ID. That is the account whose token the view checks and uses (`RepresentativeTokenID`). It used to take them from the last record, so with two accounts the shown requirement was never the written one.
- The "account" field of the auth-requirement log lines is now the canonical ID.

**Tests**
- `config.TestAccountAuthRequirement_TwoAccountsAtOneAddressAreMarkedReadAndClearedApart`: both orders.
- `sync.TestRunOnce_TheSignInRequirementIsTheSyncedAccountsAloneOfTwoAtOneAddress`: both orders. The 401 marks only the synced account, the startup proof clears only it, and the other account's mark does not trigger a revalidation.
- `cli.TestLogin_EndsTheSignInRequirementOfTheAccountItSignedIn`, now a table:
  - mark (through the product writer), then login, in all four marked/signed-in combinations;
  - both records checked, plus the offline `status --json` row for each case.
- `cli.TestBuildAccountViews_TwoAccountsAtOneAddressShowTheFirstOnesRequirement`.
- `TestClearAccountAuthScopes_ClearsPersistedAuthScope` and `TestExecutePlainLogout_ClearsPersistedAuthRequirement` are now twin tables.
- The multisync and `NewMountEngine` propagation asserts cover the new field.

**Mutants** (`go test -overlay`, files in the scratchpad)

| Mutant | What failed |
|---|---|
| The writer finds the record by email, as `a665e13c` did, while the login clears by canonical ID | The config, sync, cli login, view and logout twin tests. The case "personal account's own login" is the regression itself. |
| The view takes its fields from the last record (the parent) | The view test, and two status cases of the login table |

**Stays wrong** (also in the commit message and in `config.md`)
- Account views are one per address. The second account at an address has its requirement written and cleared on its own record, but no account surface shows it: not a status row, not `drive list`'s `accounts_requiring_auth`, not logout's choice.
- `--account` cannot choose between the two accounts, because it takes an email (R-3.5.2).
- Follow-up:
  - key `accountView` by canonical ID, which gives one status row and one auth-requirement entry per account;
  - group configured drives by owner account rather than by email;
  - count state DBs per account;
  - give logout and `--account` a canonical-ID selector when an address has two accounts.

  This changes status text and JSON, `drive list` and logout.

## Finding 2: `config.md`. Fixed
- The heading is now "…Has One Owner Outside A Login". The first paragraph says that a login ends the requirement in its own enrollment write (`recordLogin`).
- New paragraphs describe the canonical-ID keying and the one-view-per-address limit, with test evidence.
- `sync-engine.md` says the 401 marks the engine's own token owner.

## Finding 3: sync actions after a helper's panic. Fixed
- New test: `sync.TestRunOnce_AHelperPanicOfTheCommandEndsTheActionInFlightAsAShutdown`.
  - A helper goroutine's boundary cancels the command context with `AsCancelCause(Recovered(...))` while a download is in flight.
  - The download returns `context.Cause` as the transports do.
  - Expected: no failure counted, no retry work, no scope, the run's error is `context.Canceled`, and the next run downloads the file.
- It fails on both mutants:
  - `AsCancelCause` returning the plain panic error (the parent's helper sites): the download fails and a `retry_work` row is written.
  - A classifier that checks `ErrPanic` first: same result.
- It is cited in `cli.md`. It is declared in the stress shutdown members (its name holds "Shutdown") and in `runOnceScenarios`, the reverse-dispatch table. It passes under reverse dispatch.

## Notes
- 4 (periodic updates stop at cancel): by decision. Nothing done.
- 5 (the parent's commit message on discovery): the new commit message corrects it. Accounts that discovery never started record nothing; `drive list` prints nothing then anyway.
- 6, fixed:
  - `cleanupPendingToken`, which only warned, is now `withPendingTokenRemoved`. A failed removal joins the login's error.
  - A missing parent directory still counts as removed.
  - Test: `TestLogin_APendingSignInItCannotRemoveJoinsTheLoginsError`. It fails on the warn-only behaviour.
- 7, fixed:
  - `runSyncDaemonWithFactory` now does `withCommandPanic(ctx, orch.RunWatch(...))`, so a keepalive panic joins a failure the watch ends with.
  - Test: `TestRunSyncDaemon_WatchdogPanicJoinsTheFailureTheWatchEndsWith`. It fails on the parent code.
  - Cited in `cli.md`.

## Checks (on the final tree unless noted)
- `go build ./...`: ok. `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...`: ok. gofumpt: clean.
- Plain `go test`: config, multisync, cli and devtool ok.
- `internal/sync` plain: ok (406 s).
  - The first run failed `TestReverseDispatchTable_ListsEveryRunOnceScenario`: the two new `TestRunOnce_` tests were not in the table. They were added, and the rerun passed.
- `-race`: config, multisync, cli and devtool ok. `internal/sync` ok (4053 s on a loaded host).
- After those runs, one test-helper parameter was dropped for lint (`unparam`; same value). Vet and the targeted sync tests passed again after it.
- Source rules: `RealRepoIsClean` (4) and `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`: pass.
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint`: pass.
  - The first run found 1 issue (`unparam`), now fixed.
- Heavy runs went through the scratchpad lock.

## Seen and not fixed
1. Per-account views: the follow-up under finding 1.
2. `drivesForAccount` (`internal/cli/auth_logout.go`) iterates the config's drive map. With two accounts at one address, `canonicalIDForToken` picks whichever non-SharePoint drive the map yields first. So logout deletes either account's token at random while it removes both accounts' drive sections.
3. Status's live overlay asks for every configured drive at the address with the first account's token. For a primary drive `fetchConfiguredStatusDrive` calls `PrimaryDrive`, so the second account's drive shows the first account's quota.
4. `formatWatchStartupError` rebuilds a startup failure from its drives' causes. A keepalive panic joined to a failed start is dropped, and the exit code can then be a sign-in or config one instead of 1. The same happened before this commit.
5. Carried over from the H8 report:
   - `reportMainPanic` never completes the performance session (#2).
   - The transfer watchdog's cause is left plain by design (#4).
