# fsnotify Watch Capture

Reference notes for the repo-owned raw watch-event harness:

- Command: `go run ./cmd/devtool watch-capture --scenario <name> --json`
- Fixture location: `internal/syncobserve/testdata/watch_capture/<goos>/`
- Purpose: record real fsnotify ordering for marker and path-scope transitions,
  then replay those traces through `LocalObserver.HandleFsEvent` in tests

## Captured Darwin Sequences

Current checked-in captures were generated on macOS (`darwin`) and show these
stable high-level patterns:

| Scenario | Observed raw sequence |
| --- | --- |
| `marker_create` | `blocked/.odignore` emits `create` |
| `marker_delete` | `blocked/.odignore` emits `remove` |
| `marker_rename` | rename-to-marker emits `create` on `blocked/.odignore`, then `rename` on the old path |
| `marker_parent_rename` | parent rename emits `create` on `renamed`, then `rename` on the old parent path |
| `dir_move_into_scope` | move emits `create` on `docs/album`, then `rename` on `parking/album` |
| `dir_move_out_of_scope` | move emits `create` on `parking/album`, then `rename` on `docs/album` |

These are raw watcher observations, not normalized sync semantics. The replay
tests intentionally assert the observer contract instead:

- one effective scope change per real marker transition
- exactly one local scope-generation bump per effective marker transition
- marker-bearing directories stay watched while descendants under the excluded
  subtree are removed and restored as scope changes
- path-scope boundary moves stay data-only and do not synthesize marker scope
  transitions

## Open Research

- Linux captures are still needed before tightening rename-handling logic.
- If a future OS release yields multiple valid sequences for the same scenario,
  store explicit per-OS fixtures rather than collapsing them into one
  hand-authored “canonical” event list.
