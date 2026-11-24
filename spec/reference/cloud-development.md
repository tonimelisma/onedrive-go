# Cloud Development

Every increment of this repository is built in a Claude Code cloud session.
`CLAUDE.md` (Dev Process) states the rules; this page gives the reasons and
the environment that makes them cheap to follow. The one step still done on
the owner's own machine is the one-time live-test credential bootstrap of
CLAUDE.md's E2E section, which needs a browser.

## What a cloud session is

- An ephemeral Linux container, running as root, rebuilt when the session
  starts on a fresh machine. Anything left only in it is lost.
- One assigned session branch (`branch/<name>`), which the session pushes and
  opens pull requests from.
- GitHub reached through a proxy: `gh` works for the REST API (`gh api`,
  `gh run`), GraphQL is refused with HTTP 403, and git may push only the
  session's branch. The DoD stages read GitHub over REST alone, review threads
  through the proxy's REST thread routes when GraphQL is refused, so they run
  here; the nightly claims are GitHub issues written over REST too, so a
  session takes, renews and releases them like any checkout.
- Builder subagents that work in their own worktrees under
  `.claude/worktrees/agent-*`, each on its own branch, whose work the session
  integrates and ships as one squash-merged pull request.

## Why the process is shaped this way

| Practice | Why |
| --- | --- |
| one increment per session branch, squash-merged, then the branch restarted from `origin/main` | the session may push only its own branch; the squash commit is not a descendant of the branch, so without the restart the next increment would carry the merged commits (`verify --dod --stage post-merge` does the restart) |
| builders in harness-made worktrees, removed by `go run ./cmd/devtool worktree prune-merged` | builder branches are squash-merged too, so no ancestry check ever sees them merged; twenty-five builder worktrees were once left behind. Integrate a builder's commits as they are (cherry-pick), so the command can prove them merged |
| go tests as the non-root `dev` user (`DEVTOOL_TEST_USER`, or `verify --as-user`) | as root, permission tests do not bite and the sync tests that refuse root fail |
| every DoD stage run in the session | the stages read GitHub over REST, which the proxy serves, review threads over its REST thread routes, and the merge is a REST call too; the nightly claims are issues |
| no `.env`, no `.testdata` | live tests run only in CI, from the Key Vault secrets; nothing credential-bearing is copied into a container |
| no tags, releases or installs from a session | every merge to `main` is released by `.github/workflows/release.yml` once its push CI passes (R-6.10.41); the GitHub proxy refuses tag pushes and release writes, and a release only that workflow on `main` attests is the one a machine running the newest code accepts |

`prune-merged` judges a builder worktree merged when every commit's patch is
in `origin/main` (`git cherry`), or when merging the branch into
`origin/main`, or into an older commit of its first-parent line since the
merge base, changes nothing (`git merge-tree --write-tree`): that is what a
squash merge leaves, even after main changed the same lines again. A builder
whose work the session integrated with edits of its own does not pass, and
stays for a person to judge. It removes only clean worktrees, never with
`--force`, and keeps one that Claude Code has locked for a running agent;
run it with `--dry-run` first to read the verdicts. The full rule is in
`spec/design/system.md` (R-6.10.37).

A session's clone is shallow unless the environment's setup script (below) deepens it. A builder branch cut before the oldest
fetched commit of `origin/main` shows no common history with it and is kept
with that reason; `git fetch --unshallow origin` (or `--deepen`) lets the
next run judge it.

`--as-user` switches only the go test steps, so formatting, lint and git keep
running as root, and the test user reuses root's module cache and pinned
toolchain while writing its own build cache, and git, as the user, trusts the
root-owned checkout for that go test alone (R-6.10.38). Before the first go
test it prints what the user must be able to reach.

## Environment setup script

The onedrive-go cloud environment carries this script and
`DEVTOOL_TEST_USER=dev`; a session for this repository should run on that
environment. The script runs as root each time a container is built,
in the directory that holds the repository checkouts. This one installs what
`devtool verify` requires and prepares a non-root `dev` user. The versions
are the ones devtool and CI pin: the toolchain is `go.mod`'s `toolchain`
line, gofumpt and goimports are the `go install` lines of
`.github/workflows/ci.yml`, and golangci-lint is the `version:` of its
`golangci-lint-action` steps, which devtool reads and enforces
(`internal/devtool/verify_lint_version.go`, built with the same Go minor
release). Update the script when those pins move.

```bash
#!/bin/bash
set -euo pipefail

if [ -d onedrive-go ]; then
  cd onedrive-go
  # Full history, so worktree prune-merged can judge every builder branch.
  if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
    git fetch --unshallow origin
  fi
  # The go.mod toolchain: the image's go fetches it into the module cache.
  export GOTOOLCHAIN="$(awk '/^toolchain /{print $2}' go.mod)"
  go version
  # Formatters and linter at the versions devtool requires, on every PATH.
  GOBIN=/usr/local/bin go install mvdan.cc/gofumpt@v0.9.2
  GOBIN=/usr/local/bin go install golang.org/x/tools/cmd/goimports@v0.41.0
  GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3
  # Warm the module cache for the build and every tagged test tree.
  go mod download
  go build ./...
fi

# The non-root test user devtool verify --as-user runs go test as. It reads
# root's module cache and toolchain, so /root only needs to be traversable.
id dev >/dev/null 2>&1 || useradd --create-home --shell /bin/bash dev
chmod 0711 /root
```

With `DEVTOOL_TEST_USER=dev` set, every `devtool verify` in the session runs
its go tests as `dev` without the flag. If the pins have moved and the script was not updated, devtool says so:
it refuses a golangci-lint of another version and prints the install
command.

## Permission rules

None are needed. `go run ./cmd/devtool` runs without a prompt, and every
cleanup a session does goes through it: `prune-merged` removes builder
worktrees, and `post-merge` restarts the branch. Removing a worktree by hand
was only ever needed for the local-style worktrees an increment made before
development moved to cloud sessions; with those gone, an allow rule for
`git worktree remove` would widen what a session may delete for no task that
still exists.

## Open items

None at present.
