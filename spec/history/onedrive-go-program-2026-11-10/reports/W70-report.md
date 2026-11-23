# W70 report: status warns about a default sync folder that meets a managed folder

Branch `fix/lenient-load-checks-default-sync-dir` (worktree `~/src/onedrive-go-wt/lenient-load-checks-default-sync-dir`),
one commit `7ab45327` on origin/main `b00e6da3` (still main after a fetch at the end). Not pushed, no PR, no gate.
Installed: `onedrive-go --version` prints `7ab45327`. That build is main plus this commit only, so the watch
client no longer has the other branches' commits that the previous install, `8bc8aaa0`, carried.

## Definition of ready (on `b00e6da3`; the premise holds)

- (i) `EffectiveSyncDir` (`internal/config/drive.go:330`, called from `buildResolvedDrive` `:237`) computes the
  default. It needs the canonical ID, the account's org and display names from `catalog.json` (`:340`, `:356`),
  the other drives' folders (`:341`, also catalog-backed) and the home folder. No network. `loadLenient`
  (`load.go:153`) holds the config only; `LoadValidatedState` (`validated_state.go:33`) holds config and catalog.
- (ii) Both texts come from `checkSyncDirMeetsNoManagedFolder` (`validate_drive.go:312`): `drive "<id>" sync_dir
  <path> contains <folder>, <what it holds>; set sync_dir to a folder that does not contain it`. sync prefixes
  `validate sync config for <id>: ` (`cli/sync_runtime.go:46`); the warning prefixes `<config path>: ` (`load.go:21`).
- (iii) `loadAccountViewSnapshot` (`cli/account_view_snapshot.go:37-45`) logs them for status (`status_snapshot.go:183`),
  drive list (`account_view_snapshot.go:114`), shared (`shared.go:53`), drive search and drive add; logout logs
  them at `auth_flow.go:218-222`. The console prints `Warning: <config path>: <message>` on the status stream
  (`console_log.go:199`), and the log file gets them if one is set. No `--json` field.
- (iv) It could: validation already warns about a configured sync_dir, so a check that also covered configured
  drives would warn twice. The new check skips drives that set `sync_dir`.

## What changed (product +58/-39 lines, tests +118/-10)

Place (decision b): `LoadValidatedState`, not `LoadOrDefaultLenient`, which reads no catalog and also serves
every command's log-file setup (`cli/root.go:303`), which discards its warnings.

- `config/validate_drive.go`: new `checkDefaultSyncDirsMeetNoManagedFolder` runs the same one-path check on
  `EffectiveSyncDir` of each drive without `sync_dir`, skipping a non-absolute default; new `sortedDriveIDs`.
- `config/validated_state.go`: `LoadValidatedState` appends those warnings once the catalog checks pass.
- `config/resolved_validator.go`: comment updated; `cli/account_view_snapshot.go`, `cli/auth_flow.go`: flag removed.
- `spec/design/config.md` (Validation): `LoadValidatedState` warns about the default once per drive; cites both new tests.

## Tests added (all `internal/config` or `internal/cli`; each tests R-4.8.3 and R-4.8.4 unless noted)

- `TestLoadValidatedState_WarnsOnceAboutASyncDirHoldingTheDataFolder`: a default warns once, word for word
  `ValidateResolvedForSync`'s refusal; configured warns once; a default beside the folders or without a home
  folder warns nothing and fails nothing. The default case failed before the change (0 warnings).
- `TestStatusCommand_WarnsOnceAboutASyncDirHoldingTheDataFolder`: `status` prints exactly one such `Warning:`
  line, default and configured. The default case failed before (0 lines).
- The configured cases pass before and after; checking configured drives in the new check as a temporary
  mutation made both fail with 2 warnings.
- `TestLoadValidatedState_NamesTheFirstConfiguredDriveMissingFromTheCatalog` (R-3.1.5): five drives missing
  from the catalog; the refusal names the first. Failed before under `-count=20` (it named c, e, and so on).

## Checks

- build, vet, vet with `e2e,e2e_full,integration,stress`: clean. gofumpt and goimports: clean.
- `go test -count=1 ./internal/config/... ./internal/cli/...` ok; with `-race` behind the lock ok.
- golangci-lint over the two packages, behind the lock, once, last: `0 issues.` (`scratchpad/w70/W70-lint.log`).
- `RealRepoIsClean`: 4 PASS. Both test names cited in `config.md` exist and ran.
- Live: a branch build ran `status` in a private HOME/XDG tree (data folder `~/OneDrive/data`, one personal
  drive without `sync_dir`, synthetic catalog, no token, no network): one `Warning: … sync_dir <home>/OneDrive
  contains <home>/OneDrive/data/onedrive-go, …`, exit 0; the previous build printed none. Configured
  `sync_dir = "~/OneDrive"`: one warning. Tree removed. Not run: `devtool verify`, e2e, `drive list`/`shared` live.

## Also fixed

- `config/validated_state.go`: the `lenient` flag (every caller passed `true`) and the nil guards no caller
  could reach are removed; the refusal for drives missing from the catalog names them in sorted order (test above).
- `config/validated_state_test.go`: a hand-written `io.Discard` writer is replaced by `slog.DiscardHandler`.
- `config/validate_drive.go:17`: the doc comment listed "required fields" (none are checked) and left out
  overlap and managed folders.

## Not fixed

- `cli/auth_flow.go:279-297` `loadLogoutRecoveryState`: when the catalog fails after the config loads, the
  config's warnings print twice (once from `LoadValidatedState`, then again from a second lenient load of
  the same file). Fix: print only the second load's warnings and drop the `validatedWarnings` parameter;
  test a logout with an unreadable catalog and a config holding an unknown key. Left out: other warnings
  were out of scope.
- `config/drive.go:340-341` `EffectiveSyncDir` reads `catalog.json` twice per call, and `LoadValidatedState`
  now calls it once per default drive while it already holds the catalog. Fix: a variant over a catalog in
  hand (`accountNames`, `otherSyncDirs` exist). Cost not measured.
- `config/validated_state.go:72`: the refusal says "is in config.toml" even when `--config` names another
  file. Fix: pass `cfgPath` and name it; update the two tests that match the text.

## Merged as #950

- Squash `5ce3ad30`, 06:11 PST on 2026-11-18; installed from main at 06:13 (`onedrive-go --version` prints 5ce3ad30).
- Before the merge the second review's two should-fix items were fixed: a test case whose default steps on to the account's display name because another drive's section holds `~/OneDrive` (seen failing with the default computed without the other drives' folders), and the test comment for the no-home case. The two commits went in as one on `2686d99d`.
- Start stage (nightly green, nothing claimed), pre-pr (lint 0 issues, quick tier), pre-merge (the first run could not reach GitHub; the second passed) and post-merge.
- A build of the branch, in a private home and XDG tree with no token: `status` printed one warning for the default `~/OneDrive` and one for `~/OneDrive - Alice`; `sync` refused the second in the same words with exit 78; the build of main before it printed none.
- Leftovers: plan row W111.
