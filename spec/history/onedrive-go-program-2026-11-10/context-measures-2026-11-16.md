# Context measurements, 2026-11-16

Moved out of RESUME.md on 2026-11-17 to keep it under 8 KB.

- Compaction comes at about 167K tokens. The first request after one
  was 134K to 137K while the summary was 80 to 88 KB, and 92K with a
  10 KB summary. Room per cycle: 30K then, 75K now.
- Per cycle: my own output 30K to 40K tokens; tool results 9K to 15K;
  the tool list 238 KB, of which 186 KB was tools never used here.
- 13:34: deny rules for those tools added to
  `.claude/settings.local.json` (Artifact, Workflow, 32 remote-session
  tools; `get_session` and `send_later` kept). Measured at the 17:33
  compaction: first request 66K (was 92K; 50K was expected), so about
  100K of room per cycle. Told the user. To undo: delete the
  `permissions.deny` list.
- 14:28: hook `scripts/claude-context-guard` (`ed5bb05`) is live. In
  the main conversation of a session listed in
  `~/.cache/onedrive-go-program/guarded-sessions` it refuses file
  tools under `~/src/`, reads and writes over 8 KB, `cat`, unbounded
  `git diff`/`git show`, `gh pr diff`, `gh run view --log`. Subagents
  and other sessions pass. A new program session adds its own id to
  that file first. It does not cap a subagent's hand-back.
- After a merge: one line appended to
  `notes/onedrive-go-merged-prs.md`; in CLAUDE.md only the hash, the
  count and the worktree list change.
