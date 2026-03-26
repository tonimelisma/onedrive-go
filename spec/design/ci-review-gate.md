# CI Review Gate

GOVERNS: cmd/review-gate/main.go, internal/reviewgate/, .github/workflows/review-gate.yml, .github/pull_request_template.md, internal/sync/AGENTS.md, internal/graph/AGENTS.md, internal/syncstore/AGENTS.md

Implements: R-6.3.4 [verified], R-6.3.5 [verified], R-6.3.6 [verified]

## Overview

The repository uses a Codex-only PR review gate. Human approvals are not required. Merge safety comes from three layers working together:

1. the author's mandatory final self-review
2. the `review-gate` required status check
3. GitHub branch protection with required conversation resolution

The gate is intentionally strict about stale reviews. Only a Codex review attached to the current PR head SHA can satisfy the gate.

## Workflow Trigger Model

The `review-gate` workflow runs on:

- `pull_request_target` for `opened`, `reopened`, `synchronize`, and `ready_for_review`
- `pull_request_review` for `submitted` and `dismissed`

No path filters are allowed. The job must always run so the required check never sticks in a permanently pending state.

## Security Model

The workflow checks out the protected base commit via `github.event.pull_request.base.sha`. It must never execute PR-head code. This keeps the gate trustworthy even though it inspects untrusted PR metadata through the GitHub API.

The command reads the event payload from `GITHUB_EVENT_PATH` and uses the GitHub REST API with `GITHUB_TOKEN` to fetch changed files and submitted reviews. The event payload is the source of truth for the current head SHA and the expected `changed_files` count. The reviewer login is configured by the optional repo variable `CODEX_REVIEW_LOGIN`; when unset, the gate defaults to `codex`.

## Gate Semantics

The gate has three outcomes:

- `draft PR` -> pass without review requirement
- `docs-only PR` -> pass without review requirement
- all other PRs -> require Codex review on the current head SHA

Docs-only classification is an inverted whitelist:

- allowed docs-only paths: `README.md`, `TODO.md`, `LICENSE`, and `spec/**`
- control-plane files are never docs-only, even when Markdown: root `AGENTS.md` / `CLAUDE.md`, package-local `AGENTS.md`, workflow files, PR template files, and future merge-review enforcement files
- rename or copy changes are docs-only only when both the source path and destination path are docs-only
- incomplete or uncertain file listings are never docs-only; the gate must fail closed and require review

GitHub's "List pull request files" endpoint is capped at 3000 files. If the gate reaches that cap, if the event payload does not provide a trustworthy `changed_files` count, or if the fetched file count does not match that expected count, docs-only classification becomes uncertain and the gate must require Codex review.

A review counts only when all of the following are true:

- the reviewer login matches `CODEX_REVIEW_LOGIN` or the default `codex`
- the review `commit_id` matches the current PR head SHA
- the review state is submitted and one of `COMMENTED`, `APPROVED`, or `CHANGES_REQUESTED`

Decision rules:

- no qualifying Codex review on the head SHA -> fail
- latest qualifying Codex review on the head SHA is `CHANGES_REQUESTED` -> fail
- latest qualifying Codex review on the head SHA is `COMMENTED` or `APPROVED` -> pass

## Author Workflow

The PR template records the governing docs read, test and docs updates, the final self-review, the latest head SHA, and the disposition of every Codex finding.

Bootstrap note: the first PR that introduces this gate cannot itself be protected by `review-gate`, because `pull_request_target` only trusts workflows already present on the base branch. The workflow therefore checks out the protected base commit and, if that base commit does not yet contain `cmd/review-gate`, exits successfully with a bootstrap-skip message instead of trying to execute PR-head code. That bootstrap PR must still receive a Codex review and pass the existing CI checks before merge. Once it lands on `main`, branch protection can add `review-gate` as a required status check.
