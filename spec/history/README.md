# Historical program notes

**Historical. Not current guidance.** These are the working notes of the
review and refactor program that ran from 2026-10-31 to 2026-11-23, moved
here from the maintainer's former development host so that the program's
record lives with the code. Package names, file paths, test names, row ids
and rules in them describe the code as it was when each note was written;
the design docs under `spec/design/` and `CLAUDE.md` are the authority now.
A `[planned]` inside these notes is not planned work.

- `onedrive-go-review-2026-10-31/`: the review tracks, the throttling
  research, and the rehearsal and soak runs against synthetic test data.
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
- Each `.go` tool source carries a `.txt` suffix, so the module's build,
  lint and source verifiers do not treat these one-off tools as product code.
