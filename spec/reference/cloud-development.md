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
- GitHub reached through a proxy: `gh` works for the REST API (`gh api`,
  `gh run`), GraphQL is refused with HTTP 403, and git may push only the
  session's branch. The DoD stages read GitHub over REST alone, review threads
  through the proxy's REST thread routes when GraphQL is refused, so they run
  here; the nightly claims are GitHub issues written over REST too, so a
  session takes, renews and releases them like any checkout.
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
| `verify --dod` stages reach GitHub | the same stages; merging through the connector tools, after which `post-merge` reads the server's merge state | the stages read GitHub over REST, which the proxy serves, and review threads over its REST thread routes; `gh pr merge` is GraphQL, so the merge itself is the connector's |

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

None at present.
