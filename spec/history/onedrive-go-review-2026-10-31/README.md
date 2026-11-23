# onedrive-go comprehensive review, 2026-10-31

Evidence files of the review that gated production adoption of onedrive-go
(CHANGELOG entries of 2026-10-31, PRs #826–#832). Reviewed commit `26b3bc8b`;
every accepted finding was verified by hand before a fix was written.
Findings that were fixed are marked in the CHANGELOG; the rest are open
follow-ups listed in BACKLOG.md.

| File | Track |
| --- | --- |
| `track-1a.md` | Delete-safety invariants S1, S2, S4, S5, S10–S13, trash, store durability |
| `track-1b.md` | Transfer integrity and content equality (S3, S6, S7, S9, uploads, sessions, mtime) |
| `track-2.md` | Concurrency, runtime lifecycle, long-running stability |
| `track-3.md` | Trust boundaries and untrusted input |
| `track-4.md` | Silent failures (discarded or downgraded errors) |
| `track-5.md` | Spec and documentation drift |
| `track-6.md` | Production profile fit on the dev host |
| `track-7.md` | Test suite quality |
| `mutation-checks.md` | 17 mutations of the safety invariants, 16 red, 1 gap (closed in #827) |
| `throttling-research-2026-11-01.md` | Microsoft's throttling rules, how rclone/abraunegg/Graph SDK back off, and the design behind #833/#834 |
| `plan-profile-2026-11-01.md` | Planner cost at 10k/40k/140k items, the two superlinear stages and the #836 fix |
| `rehearsal-2026-10-31/` | First live rehearsal (130k tree, paused): client log, RSS, status, CPU and heap profiles |
| `rehearsal-2026-11-01/` | Second live rehearsal (10k tree): RSS, status log, edit-burst check, client log, confirm-deletes transcript |
| `soak-2026-11-01/` | Overnight soak (20k tree, 22:50–06:40): RSS, status log, the two deletion batch lists, confirm transcripts, client log |

Status of each finding ID is tracked in the CHANGELOG, not here; these files
are not updated after the review.
