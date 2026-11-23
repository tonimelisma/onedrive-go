# Cloud Development

Most increments of this repository are built in Claude Code cloud sessions.
`CLAUDE.md` ("Cloud sessions" under Dev Process) states the rules; this page
gives the reasons, the environment that makes them cheap to follow, and what
is still open.

## What a cloud session is

- An ephemeral Linux container, running as root, rebuilt when the session
  starts on a fresh machine. Anything left only in it is lost.
- One assigned session branch (`branch/<name>`), which the session pushes and
  opens pull requests from.
- GitHub reached through the GitHub connector tools only: there is no
  signed-in `gh` and no GraphQL, so anything that calls `gh api graphql` (the
  review-thread audit, the nightly claim refs, the DoD CI waits) cannot run.
- Builder subagents that work in their own worktrees under
  `.claude/worktrees/agent-*`, each on its own branch, whose work the session
  integrates and ships as one squash-merged pull request.

## Why the process differs

| Local rule | Cloud practice | Why |
| --- | --- | --- |
| `<type>/<task>` branch from `devtool worktree add` | the session's assigned branch | the session may push only its own branch; a second branch would never reach a PR |
| merge, then `post-merge` cleans the worktree | squash merge, then reset the session branch to `origin/main` | the squash commit is not a descendant of the branch, so the next increment would otherwise carry the old commits |
| worktrees removed by `post-merge` | `go run ./cmd/devtool worktree prune-merged` after integrating builders | builder branches are squash-merged too, so no ancestry check ever sees them merged; twenty-five builder worktrees were once left behind |
| tests run as the developer | `verify --as-user <user>` | as root, permission tests do not bite and the sync tests that refuse root fail; running them by hand as another user was the workaround |
| `verify --dod` stages reach GitHub | DoD checks that need GitHub done with the connector tools, stated in the PR body | without `gh` and GraphQL the stages fail, and a check skipped silently is worse than one done by other means and recorded |

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

The session's clone is shallow. A builder branch cut before the oldest
fetched commit of `origin/main` shows no common history with it and is kept
with that reason; `git fetch --unshallow origin` (or `--deepen`) lets the
next run judge it.

`--as-user` switches only the go test steps, so formatting, lint and git keep
running as root, and the test user reuses root's module cache and pinned
toolchain while writing its own build cache, and git, as the user, trusts the
root-owned checkout for that go test alone (R-6.10.38). Before the first go
test it prints what the user must be able to reach.

## Recommended environment setup script

The environment's setup script runs as root each time a container is built,
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

# The non-root test user. It reads root's module cache and toolchain: the
# cache's files are world-readable, so /root only needs to be traversable.
id dev >/dev/null 2>&1 || useradd --create-home --shell /bin/bash dev
chmod 0711 /root
```

With it, set the non-secret environment variable `DEVTOOL_TEST_USER=dev`, so
every `devtool verify` in the session runs its go tests as `dev` without the
flag. If the pins have moved and the script was not updated, devtool says so:
it refuses a golangci-lint of another version and prints the install
command.

## Recommended permission rules (the owner's decision)

Cloud sessions prompt for, or deny, commands that no rule allows. The rules
below would let a session clean up builder worktrees and run devtool without
a prompt. They are a recommendation only: whether to add them to
`.claude/settings.json` is the repository owner's decision, and no increment
adds them on its own.

```json
{
  "permissions": {
    "allow": [
      "Bash(go run ./cmd/devtool:*)",
      "Bash(git worktree unlock .claude/worktrees/*)",
      "Bash(git worktree remove .claude/worktrees/*)",
      "Bash(git branch -D worktree-agent-*)"
    ],
    "deny": [
      "Bash(git worktree remove --force:*)",
      "Bash(git worktree remove -f:*)",
      "Bash(git worktree remove * --force*)",
      "Bash(git worktree remove * -f*)"
    ]
  }
}
```

The `remove` allow rule matches by prefix, so on its own it would also allow
a forced removal written with the flag after the path; the deny rules close
that, since deny wins (the last two need a Claude Code version that accepts
`*` inside a rule, not only at its end). `go run ./cmd/devtool` covers `prune-merged`, which
runs its own git commands, so the git rules matter only for cleanup by hand.
Allowing `go run ./cmd/devtool` also allows every devtool subcommand,
`verify --dod` and `nightly` among them, which write to GitHub where GitHub is
reachable.

## Open items

- **GitHub-dependent DoD checks belong in CI.** The review-thread audit and
  replies, the nightly claims and the CI waits of `verify --dod` call `gh`,
  partly through GraphQL, which a cloud session cannot. Until they move into a
  CI job that runs with the workflow's `GITHUB_TOKEN` (for example on
  `pull_request` and after the merge, reporting as a check), a session does
  them with the connector tools and says so in the PR body, which is slower
  and checked by no tool.
