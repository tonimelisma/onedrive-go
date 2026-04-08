# fsnotify Watch Capture

Reference notes for the repo-owned raw watch-event harness:

- Command: `go run ./cmd/devtool watch-capture --scenario <name> --json`
- Fixture location:
  `internal/syncobserve/testdata/watch_capture/<goos>/<scenario>/<variant>.json`
- Purpose: record real fsnotify ordering for marker and path-scope transitions,
  then replay those traces through `LocalObserver.HandleFsEvent` in tests
- Linux capture workflow: run the command inside a Linux Docker container and
  redirect stdout into the checked-in fixture path. This is acceptable because
  `watch-capture` uses `os.MkdirTemp("", ...)`, so the watched temp tree lives
  inside the container filesystem and produces real inotify events.

## Captured Darwin Sequences

Current checked-in captures were generated on macOS (`darwin`) and show these
stable high-level patterns:

| Scenario | Observed raw sequence |
| --- | --- |
| `marker_create` | `blocked/.odignore` emits `create` |
| `marker_delete` | `blocked/.odignore` emits `remove` |
| `marker_rename` | rename-to-marker emits `create` on `blocked/.odignore`, then `rename` on the old path |
| `marker_parent_rename` | parent rename emits `create` on `renamed`, then `rename` on the old parent path |
| `marker_move_between_dirs` | destination marker emits `create` on `right/blocked/.odignore`, then source marker emits `rename` on `left/blocked/.odignore` |
| `dir_move_into_scope` | move emits `create` on `docs/album`, then `rename` on `parking/album` |
| `dir_move_out_of_scope` | move emits `create` on `parking/album`, then `rename` on `docs/album` |

## Captured Linux Sequences

Current checked-in Linux (`linux`) captures show these stable high-level
patterns:

| Scenario | Observed raw sequence |
| --- | --- |
| `marker_create` | `blocked/.odignore` emits `create`, then `write` |
| `marker_delete` | `blocked/.odignore` emits `remove` |
| `marker_rename` | rename-to-marker emits `rename` on the old path, then `create` on `blocked/.odignore` |
| `marker_parent_rename` | parent rename emits `rename` on the old parent path, then `create` on `renamed` |
| `marker_move_between_dirs` | move emits `rename` on `left/blocked`, then `create` on `right/blocked`, then a second `rename` on `left/blocked` |
| `dir_move_into_scope` | move emits `rename` on `parking/album`, then `create` on `docs/album`, then a second `rename` on `parking/album` |
| `dir_move_out_of_scope` | move emits `rename` on `docs/album`, then `create` on `parking/album`, then a second `rename` on `docs/album` |

These are raw watcher observations, not normalized sync semantics. The replay
tests intentionally assert the observer contract instead:

- one effective scope change per real marker transition
- exactly one local scope-generation bump per effective marker transition
- marker-bearing directories stay watched while descendants under the excluded
  subtree are removed and restored as scope changes
- path-scope boundary moves stay data-only and do not synthesize marker scope
  transitions

Each scenario may carry more than one fixture variant for the same OS. Replay
tests run every variant they find for the current platform instead of forcing
one hand-authored “canonical” raw sequence.

## Open Research

- If a future OS release yields multiple valid sequences for the same scenario,
  store explicit per-OS variants rather than collapsing them into one
  hand-edited event list.
