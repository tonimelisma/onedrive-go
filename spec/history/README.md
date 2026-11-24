# Historical program notes

**Historical. Not current guidance.** These are the working notes of the
review and refactor program that ran from 2026-10-31 to 2026-11-23, moved
here from the maintainer's former development host so that the program's
record lives with the code. Package names, file paths, test names, row ids
and rules in them describe the code as it was when each note was written;
the design docs under `spec/design/` and `CLAUDE.md` are the authority now.
A `[planned]` inside these notes is not planned work.

Only Markdown notes are kept; the raw run logs, profiles, tables and one-off
tool sources the program produced were removed on 2026-11-24.

- `onedrive-go-review-2026-10-31/`: the review tracks, the throttling
  research, the plan profile and the mutation checks.
- `onedrive-go-sync-split-2026-11-10/`: the analysis behind splitting
  `internal/sync` into packages (#961, #962).
- `onedrive-go-program-2026-11-10/`: the refactor program: `PLAN.md` (the
  rows), `FINISH.md` (the closing checklist), `PARKED.md` (findings left
  for the closing polish), `STATE.md`, `HANDBOOK.md`, the per-row reports
  and the analysis.
- `onedrive-go-merged-prs.md`: one line per merged program PR.

Edits made when the notes were moved:

- The test account's address, the test drive's ID and the host's name, home
  paths and production sync folder were replaced with neutral placeholders
  (`test-account@example.com`, `0000000000000000`, `the dev host`, `~`,
  `<production sync folder>`).
- The compressed run logs were decompressed so they could be scrubbed.
- Each `.go` tool source carried a `.txt` suffix, so the module's build,
  lint and source verifiers did not treat these one-off tools as product code.
- 2026-11-24: every raw artifact was removed (`*.out`, `*.log`, `*.pprof`,
  `*.csv`, `*.tsv`, `*.go.txt`, `*.py`, `*.sh`, `*.json` and raw `.txt`
  dumps, the tool sources and the rehearsal and soak run logs among them),
  leaving the Markdown notes. A note that cites one of those files by name
  now names a file that is not here.
- 2026-11-24: the notes were scrubbed of personal and host details: the
  synthetic-data folder is `~/sync-test-root`, the owner's first name is
  "the owner", terminal-multiplexer window references are "a terminal
  session", the dev host's disk layout is "the dev host's storage", and the
  host's SSH configuration and an agent home path were removed.

## History rewrite (2026-11-22/23)

Every commit before #973 was rewritten to scrub two docs. The
`merge_commit_sha` GitHub reports for #972 and earlier pull requests, and
the commit SHAs cited in these notes, name pre-rewrite objects that GitHub
still serves; they are not ancestors of `main`. To map one to the rewritten
history, find its pull request number and run `git log --grep='(#N)'` on
`main`.
