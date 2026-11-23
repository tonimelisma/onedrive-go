# W70 fix round: default sync folder warning

Commit `8bb0df77` on `7ab45327` (branch base `b00e6da3`). Not pushed, no PR, no gate.
Not installed: after `git fetch origin main`, origin/main is `99c582d4` (#948), which the branch does not
contain. The installed client stays at `7ab45327`.

Each finding was checked against the code before it was changed; all four held.

## Items

1. Guard not held (`validated_state_test.go`, no-home case). The case gave the data folder as an absolute
   path, so with no home folder a relative default could never meet it, guard or no guard. Now the case
   runs in a temporary working folder with `XDG_DATA_HOME` set to the relative `~/OneDrive/data`. With
   the `IsAbs` guard in `checkDefaultSyncDirsMeetNoManagedFolder` removed, the case fails with the
   warning `drive "personal:a@example.com" sync_dir ~/OneDrive contains ~/OneDrive/data/onedrive-go, …`.
   With the guard it passes.
2. Comment and code disagreed (`validated_state.go`). It was a plain substitution: the default's formula
   is now one helper, `defaultSyncDir(cfg, catalog, cid)` (`drive.go`), built from the existing
   `accountNames` and `otherSyncDirs`. `LoadValidatedState` passes the `stored` catalog it has just
   validated. `EffectiveSyncDir` reads the catalog once and calls the same helper, so sync and the warning
   still share one computation. Its result is unchanged; its two catalog reads become one, and its two
   debug lines for an unreadable catalog become one (the `org_name` debug attribute is gone). The
   `LoadValidatedState` comment now gives the true reason. The `resolved_validator.go` comment no longer
   says the default is "known only once the drive is resolved".
3. Names untested. New case "default sync_dir named after the organization": a `business:` drive whose
   catalog account has `OrgName` "Contoso", with the data folder inside `~/OneDrive - Contoso`. It warns
   once, and the warning is word for word `ValidateResolvedForSync`'s refusal. With the default computed
   from no names (`expandTilde(DefaultSyncDir(cid, "", "", nil))`), this case fails with 0 warnings.
4. `config.md:616`: "Validation runs at both config load and control-plane startup." now comes before
   the sentence about `LoadValidatedState` and has its own subject. That sentence names `status`,
   `drive list`, `drive search`, `shared`, `drive add` (adding a shared folder by name) and `logout`, and
   says the default is computed from the catalog the load has just validated.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: clean.
  gofumpt and goimports: clean.
- `go test -count=1 ./internal/config/...` ok; with `-race` behind the lock ok.
- Focused `internal/cli` (`-run "Status|SyncDir|ResetSyncState|DriveList|DriveAdd|DriveSearch|Shared|AccountView|Logout"`)
  ok, covering the callers of `EffectiveSyncDir`.
- golangci-lint over `./internal/config/...` behind the lock: `0 issues.` (`scratchpad/w70/W70-fix-lint.log`).
- `RealRepoIsClean`: 4 PASS.
- Not run: `internal/cli` with `-race` (no cli file changed this round), `devtool verify`, e2e.

## From the first report, now resolved

- "`EffectiveSyncDir` reads `catalog.json` twice per call": fixed by item 2.

## Still not fixed (unchanged from the first report)

- `cli/auth_flow.go:279-297`: logout's recovery path prints the config's warnings twice.
- `config/validated_state.go:72`: the refusal says "config.toml" whatever file `--config` names.
