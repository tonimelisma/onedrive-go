# 07 — Names and local observation

Analysis of `~/src/onedrive-go` at `70301c0e` (`main`), 2026-11-10.
Read-only: nothing in the repository was changed; no `go test`, `go build`,
`go vet`, `go run` or devtool command was run; the binary was not run; nothing
under `/srv` was read; no network was used.

**Conventions**

- Paths are relative to the repository root. A bare file name
  (`scanner.go:847`) means `internal/sync/scanner.go:847`.
- `split:` means `~/agent/notes/onedrive-go-sync-split-2026-11-10/`.
- **VERIFIED**: I read the code that proves it. **SUSPECTED**: part of the
  chain was not read; the entry says what settles it.
- No test was run, so every "expected to fail today" about a proposed test is a
  prediction from the code read, not an observation.
- Section 7 lists what was not read.

## 0. Findings at a glance

| ID | Finding | Status | PR |
|---|---|---|---|
| P-a | `internal/namekey` already owns OneDrive name equivalence; the brief's "ad-hoc ToLower/fold sites" do not exist for names | VERIFIED | — |
| P-b | Local `changeEvent` values are not even wake hints: production passes a `nil` channel, and every replan re-scans the tree | VERIFIED | P3 |
| N1 | Name keys are untyped `string` in three shapes, with two byte-identical path-key helpers | VERIFIED | P1 |
| N2 | The NFC helper is applied to joined paths against its own contract; three inline copies of "disk path to stored path" | VERIFIED (deviation); harmless SUSPECTED | P6 |
| N3 | Ten "find the child with this name" matchers use five different tie-break rules | VERIFIED mechanism; impact SUSPECTED | P2, P8 |
| N4 | `adoptExistingRemoteFolder` adopts on NFC equality alone, so it can adopt a folder OneDrive treats as a different name | VERIFIED mechanism; exotic | P8 |
| N5 | `lookupUploadTarget` comment says "exact name match"; the code folds | VERIFIED | P2 |
| N6 | A unit test feeds the collision detector an input the scanner cannot produce | VERIFIED | P7 |
| N7 | No table stores a name or a key; all tables store the same NFC path form | VERIFIED (no defect) | — |
| C1 | The full scan does not detect a Unicode-form twin of a synced file; `local_state` silently takes the twin's row | mechanism VERIFIED; planning outcome SUSPECTED | P7 (or narrow fix) |
| C2 | The watch detector and the scan detector apply opposite rules to baseline names that are not on disk | VERIFIED; end-to-end SUSPECTED | P7 |
| C3 | Which collider gets the issue depends on whether it produced a change event | VERIFIED | P7 (spec decision) |
| C4 | The watch-mode collision apparatus (3 fields, 9 functions) is redundant with the replan's own scan | VERIFIED structure | P10 |
| O1 | Create/modify classification, `Source`, `Name` of local events are never read | VERIFIED | P3 |
| O2 | `detectDeletions` iterates the whole baseline on every full scan for a log count | VERIFIED | P3 |
| O3 | `LastActivity` (local and remote) has no production caller | VERIFIED | P3, P5 |
| O4 | Remote `Type` and `CTag` are write-only; `emitted` is used as a count plus three fields | VERIFIED | P5 |
| O5 | A file created in watch mode is hashed by the handler and again by the replan's scan | VERIFIED mechanism; cost unmeasured | P9 |
| O6 | A known file whose metadata changed is re-hashed by every full scan until its baseline row changes | VERIFIED mechanism; cost unmeasured | P9 |
| L1 | Findings that clear without a row change (or appear in fallback mode) are not reconciled | VERIFIED mechanism; upper bound SUSPECTED | P4 (or minimal fix) |
| L2 | The R-2.8.21 test for "a finding that cleared replans" makes a call production never makes | VERIFIED | P4 |
| L3 | Rows and findings of one safety scan travel on separate channels; the findings can be dropped | VERIFIED | P4 |
| L4 | The single-name length check counts UTF-8 bytes, says "characters", and has no requirement | VERIFIED code; reachability SUSPECTED | fix PR |
| L5 | Requirement ID R-2.8.12 is used twice | VERIFIED | P0 |
| L6 | A watch delete of an unwatched, non-baseline directory removes only the exact row | VERIFIED; bounded by the replan scan | fix PR |
| L7 | The watch observer loads mount-boundary flags once and drops `CrossedMounts` on its snapshot paths | VERIFIED mechanism; no planning defect | P4 |
| L8 | Stale doc comments describing the removed event channel | VERIFIED | P3 |

## 1. Two corrections to the brief's premises

**P-a. Name equivalence is already centralised. VERIFIED.**
`internal/namekey` is a leaf package (imports only `slices`, `strings`,
`unicode`, `unicode/utf8`, `golang.org/x/text/unicode/norm`,
`internal/namekey/namekey.go:17-24`) with `Key` (`:57-91`), `Equal` (`:94-96`),
`EqualPath` (`:100-118`) and `DiffersInIgnoredCharacters` (`:123-125`).
`spec/design/system.md:80-89` names it "the single authority" and R-2.12.3
(`spec/requirements/sync.md:210`) requires every "same OneDrive name" check to
use it. A repository-wide search of production code finds **no**
`strings.ToLower`, `strings.EqualFold` or `cases.Fold` used to compare or key
file names against each other (section 2.2, table D lists what remains). The
residual problems are different from the ones the brief expected: two
equivalences (NFC-exact and OneDrive key) mixed at call sites without a type to
tell them apart, untyped keys, duplicated helpers, inconsistent tie-breaks, and
two local collision detectors with different rules.

**P-b. Local change events are not wake hints. VERIFIED.**
The brief's claim ("the engine uses these events only as wake hints and
re-derives truth from a snapshot comparison") understates it:

- Production passes `nil` as the event channel: `watchErr := localObs.Watch(ctx,
  tree, nil)` (`engine_watch.go:456`) and `rt.runPeriodicFullScan(ctx, localObs,
  tree, nil, interval, retryDelay)` (`engine_watch.go:485`). No production file
  creates a `chan changeEvent` (search of non-test files: zero matches).
  `TrySendChangeEventOnly` returns at once: `if events == nil || ev == nil {
  return }` (`observer_local.go:708-710`).
- The wake is `dirtyBuf.MarkDirty()` after the engine applies an observation
  batch (`engine_watch_local_observation.go:17-19`); the batch carries rows and
  deletes, not classifications (section 4).
- Every replan then re-derives local truth with **its own full scan**:
  `runSteadyStateReplan` calls `rt.refreshAndCommitLocalCurrentState(ctx, p.bl)`
  (`engine_watch_replan.go:41`), which builds a fresh observer and runs
  `obs.FullScan(ctx, eng.syncTree)` (`engine_current_plan.go:368`, `:386`).

## 2. Item 1 — a normalised `Name` type

### 2.1 What OneDrive uses

`spec/reference/graph-api-quirks.md:1128-1135` ("Name Equivalence Is A
Collation, Not Unicode Folding (L-33, M-1)"): "Its equivalence matches no
Unicode standard form", probed on Personal with about 500 pairs. Same name:
simple case folding (including σ/ς/Σ and Cherokee), NFC/NFD Latin spellings,
Indic and Tibetan composition exclusions, Angstrom sign = Å, ß = ss, İ = i̇,
the Latin ligatures. Ignored entirely: every code point above U+FFFF, ZWJ,
U+180B-U+180D, U+2060-U+2064, U+FE00-U+FE0F. Different names: Kelvin sign vs K
and other canonical singletons, Hebrew presentation forms vs their
decompositions, compatibility case variants (µ, ſ, ẞ ...), other expanding
foldings. `:1149-1167` adds "plain Unicode full case folding over-collides on
those compatibility characters". So neither `strings.ToLower`,
`strings.EqualFold`, `cases.Fold` nor NFC/NFKC is correct; `namekey` encodes
the probed behaviour as tables and "its tests replay every probed pair"
(`:1135`). **Business is unprobed** (`:1135`; `namekey.go:1-15`).

### 2.2 Inventory

`pkg/` and `cmd/` contain no name comparison. `internal/config` has one
display-name comparison (table D).

**A. OneDrive key (`namekey`) — 25 production call sites. VERIFIED.**

| Site | Call | Use |
|---|---|---|
| `internal/graph/items_fetch.go:106` | `namekey.EqualPath(exactPath, remotePath)` | comparison: path returned by `GetItemByPath` vs requested |
| `internal/graph/items_fetch.go:118` | `!namekey.Equal(item.Name, pathLeaf(remotePath))` | comparison: leaf fallback |
| `internal/graph/items_mutation.go:142` | `namekey.Equal(child.Name, name)` | comparison: find the folder just created |
| `internal/graph/item_create.go:104` | `namekey.Equal(children[i].Name, name)` | comparison: find the file just created |
| `internal/driveops/session.go:777` | `namekey.Equal(children[i].Name, name)` | comparison: child by name |
| `internal/driveops/transfer_verify.go:260` | `namekey.Equal(children[i].Name, target.name)` | comparison: upload target lookup |
| `internal/cli/put.go:447` | `namekey.Equal(children[i].Name, name)` | comparison: existing item after a 409 |
| `internal/cli/mv.go:281` | `namekey.EqualPath(strings.Join(targetSegments[:len(rootSegments)], "/"), r)` | comparison: target inside source |
| `core_types.go:112` | `NameKey: namekey.Key(filepath.Base(path))` | map key (`ByDirNameKey`) |
| `core_types.go:168` | `DirNameKey{Dir: dir, NameKey: namekey.Key(name)}` | map lookup |
| `scanner.go:847` | `caseGroupKey{dir: ..., nameKey: namekey.Key(filepath.Base(events[i].Path))}` | map key (collision groups) |
| `scanner.go:1062` | `namekey.DiffersInIgnoredCharacters(name, other)` | display (issue detail wording) |
| `observer_local_collisions.go:74, 114, 126, 168, 184, 211` | `namekey.Key(...)` | map key (`DirNameCache`) |
| `planner_dependencies.go:193` | `segments[i] = namekey.Key(segment)` | map key (`pathNameKey`) |
| `planner_dependencies.go:347` | `!namekey.EqualPath(vacated, a.Path)` | comparison: ordering rule |
| `planner_move_composition.go:505` | `segments[i] = namekey.Key(segments[i])` | map key (`foldMovePath`) |
| `executor.go:373` | `namekey.Equal(children[i].Name, name)` | comparison: folder-create conflict |
| `executor.go:625` | `namekey.Equal(entry.Name(), base)` | comparison: on-disk spelling for a case-only rename |
| `executor_transfer.go:603` | `!namekey.Equal(children[i].Name, name)` | comparison: file-create conflict |
| `executor_preconditions.go:1033` | `oldPath != newPath && namekey.EqualPath(oldPath, newPath)` | comparison: spelling-only move |

**B. Stored form (NFC) — production sites. VERIFIED.** `nfcNormalize` is
`norm.NFC.String` (`observer_remote.go:965-966`).

| Site | Use |
|---|---|
| `scanner.go:425`, `:446` | storage: stored path of a walked entry (applied to a **joined** path) |
| `scanner.go:447` | storage: entry name |
| `observer_local_handlers.go:280` | storage: stored path of a watch event (joined path) |
| `observer_local_handlers.go:281`, `:594` | storage: event / new-directory entry name |
| `observer_local_handlers.go:387` | storage: collision peer path |
| `observer_local_handlers.go:1248`, `:1250` | comparison: "is this name still on disk" (`localPathStillPresent`) |
| `observer_local_collisions.go:78`, `:127`, `:169`, `:188` | comparison: exclude an entry's own other spelling from collisions; cache maintenance |
| `symlink_observation.go:334`, `:480`, `:619` | storage: names found through followed symlinks |
| `item_converter.go:840`, `:875`, `:881` | storage: remote item name into the stored path |
| `executor.go:360`, `:374` | comparison: adopt vs "name taken" on folder create |
| `executor_transfer.go:606` | comparison: same, file create |
| `executor_preconditions.go:147` | comparison: live remote leaf vs planned leaf (R-2.12.5) |
| `executor_preconditions.go:164`, `:299` | comparison / storage: remote parent path (joined path) |
| `executor_delete.go:547`, `:549` | comparison: "exact local name exists" before a remote delete |
| `permission_probe_local.go:231` | storage: `storedPathOf` (joined path) |
| `shortcut_root_lifecycle.go:390` | comparison: shortcut alias candidate |
| `internal/synctree/spelling.go:101`, `:104` | comparison: stored component to on-disk entry (`norm.NFC.String` directly) |
| `internal/namekey/namekey.go:70, 207, 210, 215, 219` | inside the key computation and its tables |
| `internal/synctest/fsshim.go:250` | test shim only: `norm.NFD.String(cases.Fold().String(name))` |

`internal/graph`, `internal/driveops` and `internal/cli` never use NFC
(VERIFIED by search).

**C. Stored path to on-disk spelling (`synctree.Root.Spelling` /
`SpellingOfParent`). VERIFIED call sites:** `internal/syncverify/verify.go:130`;
`observer_local_handlers.go:146, 529, 1238`; `permission_probe_local.go:48, 184`;
`executor.go:291, 472, 473`; `executor_conflict.go:30, 51`;
`shortcut_root_lifecycle.go:582`; `executor_transfer.go:121, 124, 192`;
`executor_delete.go:65, 492`.

**D. Other folding that touches a file or folder name. VERIFIED.**

| Site | Code | Use |
|---|---|---|
| `scanner.go:1372` | `lower := strings.ToLower(name)` | comparison with OneDrive reserved names (device names `:1374`, `_vti_` `:1378`, patterns `:1382`) |
| `scanner.go:1421` | `strings.EqualFold(name, "forms")` | comparison: SharePoint root `forms` |
| `internal/driveops/stale_partials.go:71` | `strings.HasSuffix(strings.ToLower(name), downloadPartialSuffix)` | comparison: partial-download suffix |
| `internal/driveops/transfer_verify.go:122` | `strings.ToLower(filepath.Ext(name))` | comparison: enriched-extension table |
| `internal/driveops/transfer_download.go:69` | `strings.EqualFold(filepath.Ext(targetPath), ".heic")` | comparison: extension |
| `internal/cli/shared_discovery.go:94` | `strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))` | display: sort order |
| `internal/cli/drive_add.go:655, 659, 660` | `strings.Contains(strings.ToLower(...), lowerSelector)` | comparison: user's substring selector for a shared folder |
| `internal/config/drive.go:114`, `internal/cli/drive_catalog.go:547` | `strings.EqualFold(cfg.Drives[..].DisplayName, selector)` | comparison: drive display name, not an item name |

All of these compare against ASCII constants or a user-typed selector, not two
OneDrive names against each other, so none violates R-2.12.3. Whether OneDrive
matches reserved names under its collation rather than ASCII case folding is
unprobed (SUSPECTED; a live probe settles it, which this task may not run).

Not about item names (listed so the search is reproducible, no action):
`internal/config/size.go:40`; `internal/graph/items_permissions.go:87, 198, 202,
206, 218, 222, 232`; `internal/graph/url_validation.go:160, 195`;
`internal/graph/download.go:343`; `internal/graph/redaction.go:88, 161, 188`;
`internal/graph/client_auth.go:102, 103`; `internal/graph/quirks.go:200`;
`internal/graph/errors.go:101, 102, 111, 315, 351, 356, 394`;
`domain_graph_error.go:116`; `internal/cli/auth_login.go:156`;
`internal/cli/status_render_sections.go:84`;
`internal/cli/drive_reset_sync_state.go:146` and `internal/driveid/id.go:46`
(drive IDs); `internal/driveops/transfer_verify.go:127-128` (drive type);
`internal/devtool/*`.

**E. Stored form in SQLite. VERIFIED: no divergence between tables.**
`schema.go:77-251` has no name column and no key column. Every table stores
the full relative path in NFC with the default BINARY collation:
`baseline.path UNIQUE` (`:81-98`), `remote_state.path UNIQUE` (`:171-180`),
`local_state(path PRIMARY KEY)` (`:184-195`), `observation_issues(path PRIMARY
KEY, ...)` (`:214-218`), `mount_boundaries(path PRIMARY KEY)` (`:131-133`).
Prefix predicates state `COLLATE BINARY` explicitly:
`store_write_baseline.go:864`; `store_local_state.go:365, 366`;
`store_write_observation.go:319, 460, 587, 588`; `store_remote_twins.go:104, 137`.

### 2.3 How the two equivalences compose (the invariant no code states)

VERIFIED from the code; the Unicode fact that NFC maps U+212A to `K` is not
from the repository.

- A local name enters sync only through the walk or a watch handler, both of
  which NFC-normalise first (`scanner.go:446-447`;
  `observer_local_handlers.go:280-281`). The scan's collision key is then
  computed from that stored form (`scanner.go:847`). So **inside sync the
  identity of a local name is `Key(NFC(spelling))`**.
- NFC erases distinctions the key keeps: `namekey.Key` documents canonical
  equivalents as equal "except `keepsCanonicalIdentity`" (`namekey.go:34-56`).
  The Kelvin sign and `K` are different OneDrive names but one stored form.
- The design accepts this on both sides: two local entries with one NFC form
  are a local collision (R-2.13.2, `spec/requirements/sync.md:219`); two remote
  items whose stored paths are NFC-equal are "remote twins" (R-2.12.6,
  `store_remote_twins.go:14-32`).
- Raw remote names are compared by key without NFC first only in
  `executor.go:373`, `executor_transfer.go:603` and in graph/driveops/cli
  (table A).

The watch detector differs: it keys **disk spellings** directly
(`key := namekey.Key(e.Name())`, `observer_local_collisions.go:114`) and looks
up with the NFC name (`cache[namekey.Key(name)]`, `:126`). For a disk entry
whose key differs from the key of its NFC form (the Kelvin class), the two
detectors disagree. Exotic, but it is the kind of drift a single typed
constructor removes.

### 2.4 Inconsistencies

**N1. Untyped keys, three shapes, two identical helpers. VERIFIED.**
`DirNameKey{Dir, NameKey string}` (`core_types.go:103-106`);
`caseGroupKey{dir, nameKey string}` (`scanner.go:887-890`);
`DirNameCache map[string]map[string][]string` (`observer_local.go:100`, built
at `observer_local_collisions.go:111-118` and again at `:203-216`).
`pathNameKey` (`planner_dependencies.go:190-197`) and `foldMovePath`
(`planner_move_composition.go:502-509`) have the same body: split on `/`,
`namekey.Key` each segment, join. `isWithinRemotePath` (`internal/cli/mv.go:269-282`)
re-implements a segment-prefix test over `EqualPath`. `Key` returns `string`
although its doc says "Keys are comparison values only, never names to write
anywhere" (`namekey.go:34-56`); nothing stops a key from being logged, stored
or joined into a path. Cost: no compiler help; the next helper is a fourth copy.

**N2. NFC on joined paths against the helper's contract. VERIFIED deviation;
harmlessness SUSPECTED.** The doc says "Applied to each name segment
individually, not to joined paths" (`observer_remote.go:963-964`). Joined-path
uses: `scanner.go:425`, `:446`; `observer_local_handlers.go:280`;
`permission_probe_local.go:231`; `executor_preconditions.go:164`, `:299`. The
same expression `nfcNormalize(filepath.ToSlash(relPath))` is written inline
three times although `storedPathOf` (`permission_probe_local.go:228-232`) is
that function. Per-segment and whole-path NFC agree if `/` never composes with
a neighbour; that is a Unicode property (U+002F is a starter with no canonical
composition) which no repository test asserts. Settle with a property test:
for every name in `internal/namekey/testdata/probe-m1-personal.tsv`, plus
segments that begin with a combining mark, whole-path NFC equals per-segment
NFC.

**N3. One question, five tie-break rules. VERIFIED mechanism; impact
SUSPECTED.**

| Matcher | Rule |
|---|---|
| `matchChildByName`, `internal/driveops/session.go:769-787` | byte-exact returns at once (`:773-775`); else **first** key-equal (`:777-779`) |
| `findCreatedFolder`, `internal/graph/items_mutation.go:128-153` | byte-exact returns (`:138-140`); a second key-equal child means not found (`:142-145`); else the **unique** key-equal |
| `storedNameSpelling`, `executor.go:613-631` | byte-exact returns `base` (`:622-624`); else the **last** key-equal (`:625-627`) |
| `readCreatedFileByName`, `internal/graph/item_create.go:96-110` | **first** key-equal non-folder (`:104`), no exact preference |
| `lookupUploadTarget`, `internal/driveops/transfer_verify.go:237-266` | **first** key-equal non-folder (`:260`), no exact preference |
| `putFile`, `internal/cli/put.go:433-459` | **first** key-equal child (`:447`), no exact preference |
| `adoptExistingRemoteFolder`, `executor.go:351-380` | two full passes: NFC-equal folder adopts (`:360-366`), then key-equal-but-not-NFC-equal is "name taken" (`:372-376`) |
| `resolveCreateConflict`, `executor_transfer.go:580-614` | one pass that **breaks** at the first key-equal NFC-equal child (`:603-608`) |
| `uniqueEquivalentEntry`, `internal/synctree/spelling.go:92-114` | NFC only; two NFC-equal entries mean not found (`:107-109`) |
| `onDiskSiblingName` (`observer_local_collisions.go:62-90`), `localPathStillPresent` (`observer_local_handlers.go:1232-1255`), `exactLocalNameExists` (`executor_delete.go:541-554`) | NFC only, each with its own loop |

The first/last/unique differences matter only when a listing holds two
key-equal children, which OneDrive itself forbids; a stale or duplicated
listing is the way to get one. That is why impact is SUSPECTED. Settle with
table tests feeding each matcher a two-candidate listing.

**N4. `adoptExistingRemoteFolder` pass 1 tests NFC only. VERIFIED mechanism;
exotic.** `if children[i].IsFolder && children[i].ID != "" &&
nfcNormalize(children[i].Name) == nfcNormalize(name)` (`executor.go:360`). A
folder named with the Kelvin sign is NFC-equal to `K` but a different OneDrive
name, so it cannot be what caused the 409; if a key-equal sibling `k` caused
it, pass 1 still adopts the Kelvin folder before pass 2 can report "name
taken".

**N5. Comment/code mismatch. VERIFIED.** `lookupUploadTarget`'s comment says
"by an exact name match in the parent's listing for a create"
(`internal/driveops/transfer_verify.go:237-238`); the code is
`namekey.Equal(children[i].Name, target.name)` (`:260`). The test name agrees
with the code (`internal/driveops/transfer_manager_test.go:2825`,
`TestLookupUploadTarget_MatchesNameAcrossCaseAndNormalization`).

**N6. A test on unreachable input. VERIFIED.**
`TestDetectCaseCollisions_KelvinSignAndMicroSignAreDistinctNames`
(`scanner_test.go:339-352`) passes `{Path: "K.txt"}` and `{Path: "K.txt"}`
to `detectCaseCollisions`. In production the walk has already NFC-normalised
(`scanner.go:446`), so both would arrive as `K.txt`. The µ/Μ half of the test
is reachable.

**Also verified as part of this item:** C1-C3 in section 3 (the two local
detectors disagree), and `Root.Spelling` returns the exact path when it exists
without looking for a second spelling (`internal/synctree/spelling.go:52-54`),
which is what makes C1 non-converging.

### 2.5 Proposed end state

**Not proposed: wrapping every name in `type Name struct{ raw string }`.**
Reasons from the code: the stored unit is the path, not the name (2.2 E);
names are transient `filepath.Base` values at about 60 sites; a wrapper would
add conversions at each without making any wrong program fail to compile,
because the mistakes found (N1-N4, C1-C3) are about *which equivalence* and
*which tie-break*, not about a raw string reaching a comparison. The repository
rule "No abstraction without authority" (`CLAUDE.md`) argues against it. What
is proposed instead gives each of the two equivalences one typed constructor
and the sibling lookup one implementation.

All of it lives in `internal/namekey`: a leaf (2.1 P-a) that `graph`,
`driveops`, `cli` and `sync` already import (table A) and `synctree` can
import. `spec/design/system.md:3` already governs `internal/namekey/*.go`.

```go
// Key is the OneDrive comparison value of one name. It is never a name:
// do not display, store, send or join it into a path.
type Key string

// Of returns the key of one name (today's Key(name string) string,
// namekey.go:57-91; body unchanged).
func Of(name string) Key

// PathKey is the per-segment key of a slash-separated path.
type PathKey string
func OfPath(slashPath string) PathKey
// HasPrefix reports whether k is root or lies below it (segment boundary).
func (k PathKey) HasPrefix(root PathKey) bool

// Stored returns the form this client stores for a name: NFC (R-2.13.1).
func Stored(name string) string
// StoredPath returns the stored form of a slash-separated path. It equals
// Stored applied to each segment; TestStoredPath_EqualsPerSegment proves it.
func StoredPath(slashPath string) string

// Relation classifies two names of one folder.
type Relation uint8
const (
    Unrelated  Relation = iota // different key, different stored form
    Twin                       // same stored form, different key (Kelvin sign vs K)
    SameName                   // same key, different stored form (case, ss/ß, emoji)
    SameStored                 // same key, same stored form, different bytes (NFC vs NFD)
    Identical                  // byte-equal
)
func Relate(a, b string) Relation

// Match is the result of looking one name up among siblings.
type Match struct {
    Index     int      // -1 when nothing matched
    Relation  Relation // of the item at Index to want
    Ambiguous bool     // the best tier held more than one candidate
}

// Find answers "which sibling is this OneDrive name": Identical, else the only
// SameStored, else the only SameName; never a Twin. name returns false for an
// item that is not a candidate (wrong kind, no ID).
func Find[T any](items []T, want string, name func(*T) (string, bool)) Match

// FindStored answers "which on-disk entry has this stored name": the only
// entry whose Stored form equals stored. Two such entries are a local
// collision (R-2.13.2) and yield Ambiguous.
func FindStored[T any](items []T, stored string, name func(*T) string) Match
```

`Equal` and `EqualPath` keep their signatures.

**What is deleted or replaced**

- `Key(name) string` becomes `Of(name) Key`: 25 sites (table A), mechanical.
  The repository has no backward-compatibility requirement (`CLAUDE.md`).
- `pathNameKey` (`planner_dependencies.go:190-197`) and `foldMovePath`
  (`planner_move_composition.go:502-509`) are deleted for `namekey.OfPath`;
  `vacatedByNameKey` and the move-target claim map become
  `map[namekey.PathKey]...`. `isWithinRemotePath` (`internal/cli/mv.go:269-282`)
  becomes `namekey.OfPath(target).HasPrefix(namekey.OfPath(root))`.
- `nfcNormalize` (`observer_remote.go:963-966`) is deleted for `namekey.Stored`
  and `namekey.StoredPath`; `storedPathOf` (`permission_probe_local.go:228-232`)
  becomes the one "`tree.Rel` + `ToSlash` + `StoredPath`" function and replaces
  the inline copies at `scanner.go:425`, `:446` and
  `observer_local_handlers.go:280`. `internal/synctree/spelling.go:101, 104`
  use `namekey.Stored`.
- `DirNameKey` and `caseGroupKey` become one type
  `struct{ Dir string; Name namekey.Key }` (and disappear entirely in P7,
  section 3.3).
- The matchers of N3 call `Find` (OneDrive view: `findCreatedFolder`,
  `readCreatedFileByName`, `matchChildByName`, `lookupUploadTarget`, `putFile`,
  `storedNameSpelling`) or `FindStored` (local disk: `uniqueEquivalentEntry`,
  `onDiskSiblingName`, `localPathStillPresent`, `exactLocalNameExists`).
- `adoptExistingRemoteFolder` and `resolveCreateConflict` share one function in
  the executor: `classifyCreateConflict(children []graph.Item, name string,
  wantFolder bool) (adopt *graph.Item, taken *remoteNameTakenError)`. `Identical`
  or `SameStored` of the wanted kind adopts (folder create only), `SameName` is
  `remoteNameTakenError`, `Twin` is never adopted (fixes N4), nothing matched
  returns the original conflict.

**Where `Stored` lives — two options.** The split design moves `nfcNormalize`
to `syncdomain` (`split:end-state.md:117`), where it becomes an exported
identifier with 36 cross-package sites (`split:linux/exports.tsv:123`).
`internal/synctree` uses NFC too (`spelling.go:101, 104`) and is imported by
`internal/sync` (for example `synctree.IdentityFromFileInfo`,
`executor_preconditions.go:406`), so it cannot import `sync`; whether it could
import a future `syncdomain` was not checked. `namekey` is a leaf both can
import today, and it is the only place where the invariant of 2.3 (the local
identity is `Of(Stored(spelling))`) can sit beside both functions with one
test. Recommendation: `namekey`. The package comment (`namekey.go:1-15`) and
`spec/design/system.md:80-89` are widened from "same OneDrive name" to "the two
forms a name has in this client: stored form and OneDrive key".

### 2.6 Boundaries, what stays a string, multi-segment paths

- **Filesystem read** (`ReadDir`, walk, fsnotify event): the disk spelling is
  converted once by `storedPathOf` / `namekey.Stored`; the key, where needed,
  is `namekey.Of(stored)`. The disk spelling is kept only where the code must
  open the file (the walk's `fsPath`, `Root.Spelling`).
- **Graph JSON decode**: the remote spelling is kept verbatim for requests and
  display (R-2.12.5, `spec/requirements/sync.md:212`); `item_converter.go:840,
  875, 881` produce the stored path with `namekey.Stored`. Comparisons of raw
  remote names use `Find`/`Equal`.
- **SQLite scan**: nothing converts. Paths are already stored NFC strings; no
  key column exists or is proposed (2.2 E).
- **Stays `string`**: stored paths in Go and SQL, display text, request bodies,
  raw remote names, on-disk spellings.
- **Multi-segment paths**: `StoredPath` and `OfPath`; no `[]Name` type.

### 2.7 Staging without a flag day

1. Add `Key`/`Of`, `PathKey`/`OfPath`/`HasPrefix` with tests; retype the 25
   sites and delete the two planner helpers (one mechanical PR, no behaviour
   change).
2. Add `Relate`/`Find`/`FindStored` with table tests; adopt in `graph`,
   `driveops`, `cli` one matcher per commit, each with a two-candidate test
   that pins the new tie-break.
3. Add `Stored`/`StoredPath` with the property test of N2; replace
   `nfcNormalize` site by site; delete it last.
4. Sync-side matchers and the detector follow in P7/P8 (section 6).

Each step compiles and passes on its own; no step needs another to land in the
same PR.

### 2.8 Tests

**Exist.** `internal/namekey/namekey_test.go`:
`TestKey_MatchesProbedOneDriveCollisions` (`:15`),
`TestKey_MatchesTheSystematicProbe` (`:64`),
`TestKey_ASCIIFastPathAgreesWithFolding` (`:107`),
`TestKey_InvalidUTF8DoesNotPanic` (`:117`),
`TestEqualPath_ComparesEverySegment` (`:124`),
`TestDiffersInIgnoredCharacters` (`:135`).
`internal/graph/items_test.go:1625`
(`TestGetItemByPath_PathValidationUsesNamespaceKey`), `:1649`
(`TestFindCreatedFolder_MatchesUnderNamespaceKey`).
`internal/cli/put_overwrite_test.go:68`, `:87`; `internal/cli/mv_test.go:290`.
`internal/driveops/transfer_manager_test.go:2825`.
`internal/synctree/spelling_test.go:46, 112, 126, 142`.
`planner_dependencies_test.go:100` (`TestPathNameKey_EqualPathsShareAKey`).
`executor_create_test.go:92, 117`; `executor_remote_spelling_test.go:19`;
`namekey_scenario_test.go:38, 71`; `nfd_local_spelling_test.go:84-364` (ten
tests, each with **one** spelling on disk);
`observer_local_test.go:917` (`TestFullScan_NFCNormalization`);
`observer_remote_test.go:649` (`TestFullDelta_NFCNormalization`).
No dedicated test for `matchChildByName` was found.

**Write first** (each `// Validates:` the ID given):

- `TestOf_StoredComposition_KelvinClass` (R-2.12.3, R-2.13.2): for each
  `keepsCanonicalIdentity` pair, `Of(a) != Of(b)` and `Stored(a) == Stored(b)`,
  hence `Relate(a, b) == Twin`.
- `TestStoredPath_EqualsPerSegment` (R-2.13.1): the property test of N2.
- `TestOfPath_HasPrefix_SegmentBoundary` (R-2.12.3): `a/bc` is not below `a/b`;
  `A/B/c` is below `a/b`.
- `TestRelate_ProbeCorpus` (R-2.12.3): every probed pair classified.
- `TestFind_TieBreak` (R-2.12.3): Identical beats SameStored beats SameName;
  two SameName is Ambiguous; a Twin is never returned.
- Per adopted matcher, one two-candidate test, for example
  `TestMatchChildByName_TwoFoldedCandidatesIsNotFound`,
  `TestReadCreatedFileByName_PrefersExactSpelling`,
  `TestPutFile_PrefersExactSpellingOverCaseVariant`.
- `TestClassifyCreateConflict_NeverAdoptsATwin` (R-2.12.6, R-5.6.15).

### 2.9 Spec to update

`spec/design/system.md:80-89` (namekey's role now includes the stored form and
the lookup); `spec/design/sync-domain.md` (remove `DirNameKey` when P7 lands);
`spec/design/graph-client.md:208` and `spec/design/drive-transfers.md:248`
(created-item lookups use `Find`); `spec/design/sync-execution.md:177`
(`remoteNameTakenError`); `spec/design/sync-planning.md:432, 498-501` (path
keys). Requirements: R-2.12.3 gains "and one lookup rule when several siblings
qualify"; R-2.13.1 names the stored-form function. Verified-By rows that cite
renamed tests must be updated in the same PR (`CLAUDE.md`: cited tests must
exist).

### 2.10 Risks

- Tie-break unification changes behaviour where a listing holds two key-equal
  children: an explicit "not found / ambiguous" replaces a listing-order pick.
  For `putFile` this turns a possible overwrite of the wrong case variant into
  the original 409.
- Moving `Stored` to `namekey` deviates from `split:end-state.md:117`; the
  split's `conf/production.txt` is hand-written against `70301c0e`
  (`split:README.md:36`) and must be regenerated (`split:README.md:53-64`).
- Business accounts are unprobed; nothing here changes that.

## 3. Item 2 — case-collision detection

### 3.1 Every detector

| # | Detector | Folding | Reports as |
|---|---|---|---|
| 1 | Scan: `detectCaseCollisions`, `scanner.go:836-884` | `namekey.Key` of the NFC stored name of every entry **that produced a change event** (`:845-849`); single-event groups cross-check baseline variants that are on disk (`crossCheckBaseline` `:896-927`, `baselineVariantPresent` `:931-933`) | `skippedItem{Reason: issueCaseCollision}` per colliding path, detail names the other file (`:1017-1127`); children of a colliding directory get "parent directory %q has a case collision" (`:1007`); the issue makes local truth unavailable for the path (`truth_status.go:112-126`) |
| 2 | Watch: `HasCaseCollisionCached`, `observer_local_collisions.go:99-158` | `namekey.Key` of **disk spellings** (`:111-118`), excluding the entry's own other spelling by NFC (`:127-133`); then baseline variants **not** in `RecentLocalDeletes` (`:139-155`) | nothing durable: Debug log and return; `AddCollisionPeer` on the create path only (`observer_local_handlers.go:382-395`); no batch, no dirty mark; fails open on `ReadDir` error (`:106-109`) |
| 3 | Planner, move targets: `findTargetCollision`, `planner_move_composition.go:445-486` | `foldMovePath` per-segment key (`:455`, `:468`) | first target with more than one claim (`:479-483`) |
| 4 | Planner, ordering: `vacatesCaseVariantOf`, `planner_dependencies.go:334-` | `pathNameKey` index (`:157-171`, `:323`) then `EqualPath` (`:347`) | a dependency edge, not an issue (R-2.12.1, last sentence) |
| 5 | Executor, folder create: `adoptExistingRemoteFolder`, `executor.go:351-380` | NFC, then key minus NFC-equal | `remoteNameTakenError` (`:372-376`), classified `issueCaseCollision` (`engine_result_classify.go:56`); else `stalePreconditionError` (`:379`) |
| 6 | Executor, file create: `resolveCreateConflict`, `executor_transfer.go:580-614` | key, then NFC to exclude the same name | `remoteNameTakenError` (`:610`) |
| 7 | Store, remote twins: `store_remote_twins.go:69-186` | byte-equal NFC stored path | shadow rows and `issueRemoteNameTwin` (R-2.12.6) |
| 8 | CLI `put`: `putFile`, `internal/cli/put.go:433-459` | key (`:447`) | folder: error `"%q is a folder on OneDrive"` (`:450-452`); file: conditional overwrite by ID (`:454-455`) |
| 9 | CLI `mv`: `isWithinRemotePath`, `internal/cli/mv.go:267-282` (caller `:128`) | per-segment key | refuses the move |
| 10 | Graph: `GetItemByPath` (`internal/graph/items_fetch.go:90-123`), `findCreatedFolder`, `readCreatedFileByName` | key | not found / `ErrUploadCompletedWithoutItem` (`item_create.go:109`) |
| 11 | driveops: `matchChildByName`, `lookupUploadTarget` | key | lookup result |
| 12 | Shortcut alias: `shortcut_root_lifecycle.go:383-394` | NFC only (`:390`) | picks another alias; this is the only name logic found in the shortcut lifecycle file |
| 13 | synctree: `uniqueEquivalentEntry`, `internal/synctree/spelling.go:92-114` | NFC only | "missing" when two entries share a form (`:107-109`), reached only when the exact spelling is absent (`:52-54`) |
| 14 | Executor: `exactLocalNameExists`, `executor_delete.go:541-554` | NFC only | blocks a remote delete; `ReadDir` failure counts as present (`:543-546`) |

Rows 1 and 2 are the two *local collision detectors*; rows 3-14 are lookups or
guards that section 2.5 routes through `namekey.Find`/`FindStored`/`OfPath`.

### 3.2 Where detectors 1 and 2 are wrong or disagree

**C1. A Unicode-form twin of a synced file is not detected by the full scan.
Mechanism VERIFIED; planning outcome SUSPECTED.**

Setup, on a byte-exact filesystem such as ext4: the baseline holds
`dir/café.txt` (stored path P, NFC on disk, unchanged). The user creates
`dir/café.txt` (NFD spelling) with different content.

1. Both entries map to P: `dbRelPath := nfcNormalize(filepath.ToSlash(relPath))`
   (`scanner.go:446`).
2. `processObservedInfo` marks and stores without asking whether P was already
   seen: `observed[dbRelPath] = true` (`:642`), `currentRows[dbRelPath] = row`
   (`:657`). The second entry overwrites the first.
3. The twin differs from the baseline identity, so it becomes a hash job with
   `isNew: false` (`:792-804`); the original reuses the baseline hash
   (`:767-783`).
4. The hash phase emits one `ChangeModify` event for P (`:354-369`) and its row
   replaces whatever the walk left: `currentRows[hashRows[i].Path] =
   hashRows[i]` (`:179-181`).
5. `detectCaseCollisions` sees one event in the group, so it runs
   `crossCheckBaseline`; the only baseline variant is P itself, and `if v.Path
   != ev.Path && baselineVariantPresent(v, observed)` (`:920`) is false.
   **No issue.**

Result: `local_state[P]` carries the twin's hash, size, mtime and inode with no
finding. This contradicts R-2.13.2: "Two entries in one folder with the same
NFC form are a local collision (R-2.12.3), and neither is chosen"
(`spec/requirements/sync.md:219`).

Downstream. VERIFIED: an upload of P reads `source :=
e.syncTree.Spelling(action.Path)` (`executor_transfer.go:192`), `Spelling`
returns the exact NFC path because it exists
(`internal/synctree/spelling.go:52-54`), and
`validateUploadSourcePrecondition` then compares that file with the planned
local row and returns `stalePreconditionError` on a different size, mtime,
ctime or identity (`executor_preconditions.go:396-413`); the inode always
differs. SUSPECTED: that the planner plans an upload for P from this row and
that `action.View.Local` is this row (planner comparison not traced for this
case). If so, the upload is superseded on every pass and the twin is never
synced and never reported; `nfd_local_spelling_test.go:84` documents the same
symptom for an earlier cause ("The upload used to be superseded on every pass
... while the sync reported success"). Settle with the two tests in 3.4.

The only existing test of same-form twins feeds two events with the identical
path to the pure function (`scanner_test.go:605-620`,
`TestDetectCaseCollisions_UnicodeFormTwinsReportOnceWithClearReason`); that is
the both-new case, which works (`sameForm`, `scanner.go:1095-1113`). The watch
detector does catch the synced-twin case (`sameSpellings > 1`,
`observer_local_collisions.go:130-133`) but only suppresses the watch batch;
the next full scan, which every replan runs, produces the row above.

**C2. Opposite baseline rules. VERIFIED; end-to-end SUSPECTED.** The scan
counts a baseline variant only if it **is** on disk (`scanner.go:920`,
`:931-933`; doc `:826-835`: "A baseline variant that is not on disk any more is
the old name of a case-only rename (or a local delete), not a collision"). The
watch detector counts a baseline variant unless it is in `RecentLocalDeletes`
(`observer_local_collisions.go:139-155`), a set that every safety scan empties
(`observer_local_handlers.go:1422`) and that is empty after a restart
(`observer_local.go:161`). The watch observer reads the engine's live
`Baseline` (`engine_watch.go:364`; `core_types.go:155-163`).
Scenario: `Report.txt` is renamed to `report.txt` while the client is stopped,
or more than one safety interval before the remote move commits. A later write
to `report.txt` reaches `HashAndEmit`, which finds the baseline variant
`Report.txt`, logs at Debug and returns without a batch
(`observer_local_handlers.go:1005-1013`). The change is picked up only by the
next full scan. R-2.12.1 says a case-only rename "is not a collision"
(`spec/requirements/sync.md:208`). Settle with
`TestHashAndEmit_BaselineVariantNotOnDiskIsNotACollision` (3.4).

**C3. Who gets the issue depends on change events. VERIFIED.** A new file
`a.TXT` next to an unchanged synced `a.txt`: only `a.TXT` gets the issue
(`appendSingleGroupCollision`, `scanner.go:1017-1053`, issue for the event's
path only). If `a.txt` is also modified, both produce events, the group has two
members and both get issues (`appendMultiGroupCollisions`, `:1077-1127`).
R-2.12.2 says "neither colliding file nor directory shall be uploaded"
(`spec/requirements/sync.md:209`). The effective rule today is "a collider is
blocked when it has something to upload", which is defensible, but it is
written nowhere. Spec decision needed; 3.3 keeps today's rule unless decided
otherwise.

**C4. The watch apparatus is redundant under today's architecture. VERIFIED
structure.** `DirNameCache`, `RecentLocalDeletes`, `CollisionPeers`
(`observer_local.go:100, 107, 114`) and `HasCaseCollisionCached`,
`onDiskSiblingName`, `updateDirNameCache`, `removeDirNameCache`,
`populateDirNameCache`, `AddCollisionPeer`, `removeCollisionPeersFor`,
`buildPeerRelPath`, `readDirForCollisionCheck`
(`observer_local_collisions.go:36-265`) exist to suppress a watch batch and to
re-emit the survivor when a collider is deleted
(`observer_local_handlers.go:1138-1147`). But: a suppressed watch event creates
no issue (row 2); the file header calls the scan "authoritative"
(`observer_local_collisions.go:12`, `:32-35`); every safety scan throws the
state away (`observer_local_handlers.go:1420-1422`); and `HandleDelete` always
sends a delete batch (`:1175`), which marks dirty, whose replan runs a full
scan that finds the survivor on its own. The asymmetry that
`scanNewDirectoryEntry` detects a collision without recording peers
(`observer_local_handlers.go:618-623` vs `:387-388`) has no visible effect for
the same reason.

### 3.3 One detector

**Type** (in `internal/sync` today, `internal/syncobserve` after the split):

```go
// dirNames is the set of names one directory holds on disk, grouped by the
// identity this client gives them: namekey.Of(namekey.Stored(diskName)).
type dirNames struct {
    byKey map[namekey.Key][]diskName
}
type diskName struct {
    disk   string // spelling on disk
    stored string // namekey.Stored(disk)
}
func (d *dirNames) add(disk string)
func (d *dirNames) remove(disk string)
// rivals returns the other on-disk spellings that collide with disk, and
// whether one of them has the same stored form (R-2.13.2).
func (d *dirNames) rivals(disk string) (others []string, sameForm bool)
```

**A. Scan (recommended first step).** The walk adds every entry at the point
where it is accepted as observed, replacing the unconditional mark at
`scanner.go:642`. After the hash phase a pure function replaces
`detectCaseCollisions`:

```go
type changedEntry struct { path string; isDir bool }

// localNameCollisions returns one finding per colliding stored path.
// changed lists the entries that differ from the baseline; an unchanged entry
// whose rivals are all unchanged gets no finding (today's rule, C3).
func localNameCollisions(dirs map[string]*dirNames, changed []changedEntry) []skippedItem
```

It needs no baseline: a baseline variant "present on disk" is simply another
on-disk entry in `dirNames`. It detects C1 because two disk names with one
stored form are two `diskName` values under one key with `sameForm` true,
whether or not either produced an event. Directory-child suppression
(`suppressDirectoryChildren`, `scanner.go:937-964`) keeps its behaviour by
walking `changed`.

Deleted: `caseGroupKey`, `crossCheckBaseline`, `baselineVariantPresent`,
`appendSingleGroupCollision`, `appendMultiGroupCollisions` (logic folded into
the new function; `collisionDetail` `:1059-1073` stays), and the `observed`
parameter of collision detection.

**Watch.** `HasCaseCollisionCached` is rewritten over `dirNames` with the same
rule (on-disk rivals only). That removes its baseline lookup
(`observer_local_collisions.go:139-155`) and with it `RecentLocalDeletes`
(written only at `observer_local_handlers.go:1129-1130`, read only at
`observer_local_collisions.go:150`), fixing C2.

**Then the baseline name index is dead.** `GetCaseVariants` has exactly three
production callers: `scanner.go:917`, `scanner.go:1041`,
`observer_local_collisions.go:139` (VERIFIED by search). With them gone, delete
`Baseline.ByDirNameKey` (`core_types.go:128`), `DirNameKey` (`:103-106`),
`dirNameKeyFromPath` (`:109-114`), `GetCaseVariants` (`:164-174`), the index
maintenance in `Put` (`:201-215`) and `Delete` (`:236-244`), the swap (`:260`)
and both builders (`core_types.go:334-342`, `store_write_baseline.go:173,
185-186`). R-2.12.3 names "the baseline sibling index" in its list
(`spec/requirements/sync.md:210`); that phrase is removed.

**B. Delete watch-side detection (contingent).** If the project confirms that
the replan's scan is the source of local truth (section 4.5, option 1), delete
everything listed in C4 and the three call sites
(`observer_local_handlers.go:382, 618, 1005`). Collisions then surface at the
replan's scan, within the debounce, instead of at the next safety scan as
today (row 2: no issue until a full scan). R-2.12.2's last sentence ("N-way
peer tracking for immediate re-emission on delete resolution") describes
mechanism and is reworded to the observable behaviour: "deleting one collider
lets the survivor sync on the next pass". If instead the replan scan is ever
removed, an exact watch detector is required again; that is why A comes first
and B is separate.

### 3.4 Tests

**Exist.** `scanner_test.go:21-620` (25 `TestDetectCaseCollisions_*` /
`TestFullScan_CaseCollision_*` tests, including `_BaselineCrossCheck` `:281`,
`_CaseOnlyRenameIsNotACollision` `:355`, `_BaselineAndEventCollision` `:395`,
`_DirectoryChildrenSuppressed` `:453`, `_UnicodeFormTwinsReportOnceWithClearReason`
`:605`); `observer_local_collisions_test.go:83-708` (including
`_UnreadableDir_FailOpen` `:135`, `_BaselineSkipsRecentDelete` `:201`,
`TestWatch_CaseCollision_EventSuppressed` `:225`,
`TestWatch_SafetyScan_ClearsPeers` `:643`,
`_NFDOnDiskNameDoesNotCollideWithItself` `:693`);
`observer_local_collision_spelling_test.go:81, 107`;
`executor_create_test.go:92, 117`; `pending_creates_test.go:358, 404`;
`remote_twins_test.go:34-191`; `internal/cli/put_overwrite_test.go:68, 87`;
`internal/cli/mv_test.go:290`; `planner_actions_test.go:1754`;
`internal/synctest/fsshim_test.go:77`.

**Write first.**

- `TestFullScan_UnicodeFormTwinOfSyncedFileIsACollision` (R-2.13.2): temp dir
  on Linux, baseline holds NFC `dir/café.txt` with its hash, add an NFD-named
  sibling with other content, `FullScan`, assert `Skipped` holds
  `issueCaseCollision` for P. Expected to fail today (C1).
- `TestRunOnce_UnicodeFormTwinOfSyncedFileUploadsNothing` (R-2.13.2): one-shot
  engine pass over the same tree, assert no upload action for P and one
  issue. This settles the SUSPECTED half of C1.
- `TestHashAndEmit_BaselineVariantNotOnDiskIsNotACollision` (R-2.12.1):
  baseline holds `Report.txt`, disk holds only `report.txt`, empty
  `RecentLocalDeletes`, a write event for `report.txt`; assert a batch is sent.
  Expected to fail today (C2).
- `TestFullScan_ModifiedSyncedSiblingOfNewColliderBlocksBoth` and
  `..._UnchangedSyncedSiblingKeepsSyncing` (R-2.12.2): pin C3 before changing
  the implementation.
- `TestDirNames_ProbeCorpus` (R-2.12.3): every probed pair plus the NFC twins.
- `TestLocalCollisions_ScanAndWatchAgree` (R-2.12.3): the same directory
  listing through `localNameCollisions` and through the watch check gives the
  same verdict per name.

### 3.5 Spec to update

`spec/design/sync-observation.md`: Local Observation (`:334`), the
collision-peer text (`:358`, `:568-593`) and the Verified-By row at `:89`,
which cites `TestHandleDelete_ReemitsCollisionPeerUnderItsStoredPath` and
`TestHandleDelete_DeletingTheNFDNamedPeerReemitsTheOther` (both go with option
B). `spec/design/sync-domain.md` (the `Baseline` index). Requirements:
R-2.12.1 (unchanged text, now one rule in both modes), R-2.12.2 (C3 decision;
option B rewording), R-2.12.3 (drop "the baseline sibling index"), R-2.13.2
(add: "including when one of them is already synced").

### 3.6 Risks

- A stored path may legitimately be visited twice in one walk through a
  followed symlink; `symlink_observation.go` was not read beyond its name
  sites, so the "already observed" test in `dirNames.add` needs a symlink case
  before it lands (SUSPECTED).
- If the C3 decision is "block every collider", an unchanged synced file whose
  sibling appears stops receiving remote edits until the user renames one;
  today it keeps syncing.
- Option B removes five tests' subject matter and changes when a collision
  becomes visible; it must not land unless the replan scan stays.

## 4. Item 3 — scanner classification vs. wake hints

### 4.1 Verdict

The claim is **true and understated** (P-b). VERIFIED:

- Local events are classified (`changeCreate` / `ChangeModify` /
  `ChangeDelete`; never `ChangeMove`) and the classification is read by one
  `switch` that merges create and modify and by one Debug log.
- The event channel is `nil` in production.
- The engine consumes rows and deletes, marks dirty, and the replan re-scans.

### 4.2 What local observation emits and who reads it

**Event type.** `changeEvent` has 22 fields (`core_types.go:37-69`).
`changeType` has four values (`enums.go:69-74`); `changeSource` two
(`enums.go:45-52`).

**Producers of local events.** Scan: `scanner.go:361` (hash phase), `:689`
(new folder), `:701` (new file with a cached hash), `:1152` (deletions).
Watch: `observer_local_handlers.go:398` (create), `:635` (entry of a new
directory), `:717`, `:1045` (`Type: ChangeModify`, `:1046`), `:1167` (delete).

**Consumers, field by field. VERIFIED by search for every reader.**

| Field | Reader | Note |
|---|---|---|
| `Type` | `local_observation_batch.go:23` (the `switch`) | `case changeCreate, ChangeModify:` is one arm (`:37-40`); `case ChangeDelete:` (`:23-29`); `case ChangeMove:` (`:30-36`) has no local producer |
| `Type` | `observer_local.go:723` | Debug log inside the dropped-event branch of a function that returns early in production (`:708-710`) |
| `Source` | `observed_items.go:23` (`if ev.Source != sourceRemote`) | remote projection only; no reader for local events |
| `Name` | none for local events | `localStateRowFromEvent` (`local_observation_batch.go:46-59`) copies Path, ItemType, Hash, Size, Mtime and the identity fields; not Name, Source or Type |
| `ItemType` on a delete | `local_observation_batch.go:24-29` | chooses `deletedPrefixes` or `deletedPaths` from a guess (`observer_local_handlers.go:1149-1155`), see L6 |
| `Path`, `ItemType`, `Hash`, `Size`, `Mtime`, identity | `localStateRowFromEvent` | live |

**`scanResult.Events`.** Its own comment: "Events remain observation-local
signals for watch dirtiness and diagnostics; they are not planner input"
(`scanner.go:44-56`). Readers: collision candidates (`scanner.go:190`), log
counts (`:197-208`), `recordActivity` (`:210-212`), and two forwarding loops
into the nil channel (`observer_local_handlers.go:1395-1401`;
`engine_watch.go:703-705`). `TrySendFullLocalSnapshot` sends rows only and its
doc explains why events must not be a dirty hint: "The scan's events are
relative to the baseline, not to local_state ... Treating them as a dirty hint
retired the admitted plan at every safety scan during a backlog (R-2.8.21)"
(`observer_local.go:756-770`).

**The engine side.** `handleWatchLocalObservationBatch` applies the batch and
calls `MarkDirty()` (`engine_watch_local_observation.go:5-22`).
`applyLocalObservationBatch` branches three ways: `markSuspect` (`:28-38`),
`fullSnapshot` via `ReplaceLocalStateReportingChange(ctx, batch.rows)` (`:43`,
returning `batch.dirty || changed` `:48`), or a scoped patch (`:51-64`). The
dirty buffer keeps no payload: `dirtyBatch{FullRefresh bool}`
(`buffer.go:54-56`), "deliberately does not preserve path subsets or event
history because the runtime always replans from committed current truth"
(`buffer.go:58-60`).

**The replan.** `runSteadyStateReplan`: "Dirty batches are scheduler hints
only; the actionable set always comes from committed current truth plus durable
held-work state, never from the batch payload itself"
(`engine_watch_replan.go:12-15`), then
`rt.refreshAndCommitLocalCurrentState(ctx, p.bl)` (`:41`) =
`engine_current_plan.go:303-316` → `refreshLocalCurrentState` (`:287-301`) →
`observeLocal` (`:358-392`: fresh `newLocalObserver` `:368`, hash cache
`:369-375`, mount flags reloaded `:380-384`, `obs.FullScan` `:386`) →
`commitCurrentLocalSnapshot` (`:394-399`). The same call is made at
`engine_watch_loop.go:340` and in one-shot at `engine_current_plan.go:193`.
The design text agrees (`spec/design/sync-engine.md:620-635`;
`spec/design/sync-observation.md:368-384`, `:501-532`).

### 4.3 Dead work (VERIFIED)

1. **The event channel and its plumbing.** The `events chan<- changeEvent`
   parameter on `engine_watch.go:628, 677`;
   `observer_local_handlers.go:46, 171, 194, 260, 336, 437, 563, 592, 704, 971,
   1098, 1190, 1358`; `observer_local.go:700, 707, 918`. `TrySendChangeEventOnly`
   (`observer_local.go:707-726`). Both forwarding loops.
2. **Create-versus-modify classification.** `changeType := changeCreate ... if
   !job.isNew { changeType = ChangeModify }` (`scanner.go:354-358`) and the
   `Type:` assignments at every producer listed in 4.2.
3. **`detectDeletions`** (`scanner.go:1131-1164`) and its only helper
   `shouldSuppressDeleteForExcludedPath` (`:1166-`). One caller
   (`scanner.go:194`), placed **after** collision detection (`:190`), so its
   output reaches only the log counts and the dead `Events` field. It iterates
   the whole baseline on every full scan, and every replan runs a full scan.
4. **`ChangeMove` arm** of `localObservationBatchForEvent`
   (`local_observation_batch.go:30-36`).
5. **`localObserver.LastActivity`** (`observer_local.go:879`),
   `lastActivityNano` (`:508`), `recordActivity` (`:889-891`; called at
   `scanner.go:211`, `observer_local.go:714, 735`). No caller of `LastActivity`
   exists in production code (search of `internal`, `cmd`, `pkg`).
6. **The delete type guess** (`observer_local_handlers.go:1149-1155`), once
   deletes are always "path and subtree" (L6).
7. **About 85 test sites** create a `chan changeEvent` and assert on event
   types the engine never sees (for example
   `observer_local_collision_spelling_test.go:20-128`; helpers `startLocalWatch`
   `observer_local_test.go:121`, `waitForLocalEvent`
   `observer_local_watch_health_test.go:320`, `waitForLocalCreateEvent`
   `observer_local_create_test.go:199`, `drainChangeEvents`
   `observer_local_collision_spelling_test.go:64`). They test a channel
   production does not have.

**L8. Stale comments describing the channel. VERIFIED.**
`observer_local.go:697-699` ("If the channel is full, the event is dropped and
logged at Warn. The safety scan (every 5 minutes) catches any dropped events");
the header at `observer_local.go:7`; `observer_local_handlers.go:256-257`,
`:1345-1349`; `scanner.go:9`.

### 4.4 Live work that must stay (VERIFIED)

- **Watch-batch rows and deletes.** R-2.8.8 requires them
  (`spec/requirements/sync.md:97`). They are read between a patch and the
  replan by action freshness (`action_freshness.go:116-140`,
  `store.GetLocalStateByPath` at `:128`; R-2.8.9), and they are what
  `ReplaceLocalStateReportingChange` compares a safety scan against
  (`store_local_state.go:66-72`, `:296-321`; R-2.8.17, R-2.8.21). So "paths
  dirty only" is **not** a safe reduction; see option 2 in 4.5.
- **`hashJob.isNew`** (`scanner.go:91`): it gates durable hash-cache recording
  (`if r.cache == nil || !job.isNew || hash == "" { return }`,
  `scanner_hash_cache.go:69`) and the baseline-equal shortcut
  (`scanner.go:332`). Only the `changeType` derived from it is dead.

### 4.5 The architectural fork

Watch mode has three layers that each compute local truth (VERIFIED):
(i) per-event handlers that stat, hash, collision-check and patch
`local_state`; (ii) safety, recovery and fallback full scans that replace
`local_state` and mark dirty only on change; (iii) the replan's own full scan.

1. **The replan's scan is truth (today).** Then handler-side classification,
   the watch collision apparatus (C4) and the scan's event list are redundant,
   and the handlers' remaining job is to keep `local_state` fresh for action
   freshness and to wake the replan.
2. **Watch patches are truth between safety scans.** Then the replan scan goes
   (large saving on big trees), and the handlers must be exact: collisions,
   findings, mount boundaries and directory subtrees, all incrementally.

The code is on option 1 while still paying for parts of option 2. This report
proposes making option 1 explicit and deleting what it makes redundant. It
does not propose option 2: no measurement of the replan scan's cost exists in
the repository, and the rule "measure before optimizing" (`CLAUDE.md`) applies.

### 4.6 Proposed end state

**Observer output: a sum type matching the engine's three-way branch**
(replacing the 7-field `localObservationBatch` with its three mode flags,
`local_observation_batch.go`):

```go
type localObservation interface{ isLocalObservation() }

// localPatch: scoped facts from watch handlers. Always wakes the replan.
type localPatch struct {
    upserts         []localStateRow
    removedSubtrees []string // each path and everything below it
}

// localSnapshot: the result of one full scan. Wakes the replan only when it
// changes committed truth (R-2.8.17, R-2.8.21).
type localSnapshot struct {
    rows          []localStateRow
    findings      []skippedItem
    crossedMounts []string
}

// localTruthSuspect: observation was lost; local truth is incomplete.
type localTruthSuspect struct{ reason string }
```

**Signatures.** `Watch(ctx context.Context, tree *synctree.Root) error`;
`FullScan` returns `scanResult{Rows, Skipped, CrossedMounts}`; handlers build
`localStateRow` values directly and call one `send(localObservation)`.

**Deleted.** Everything in 4.3 items 1-6; `TrySend` (`observer_local.go:700-705`);
`localObservationBatchForEvent` and `localStateRowFromEvent`
(`local_observation_batch.go`); `scanResult.Events` (after P7 the internal
candidate list is `[]changedEntry`, section 3.3); all local use of
`changeEvent`, `changeCreate`, `ChangeModify`, `SourceLocal`. The split would
otherwise export `changeEvent` to 107 cross-package sites, 77 of them in
`syncobserve` (`split:linux/exports.tsv:16`), `changeCreate` to 8 (`:15`) and
`localObservationBatch` with 7 fields at 17 sites (`split:end-state.md:226-228`).

**Kept.** The rows and deletes of 4.4.

**Tests migrate** to assert on the channel the engine installs through
`SetLocalObservationBatchChannel` (`observer_local.go:596`). Scan tests that
assert delete events (for example `scanner_test.go:166`,
`TestFullScan_CaseCollision_NoFalseDeletion`) assert on `Rows` instead.

**Option the brief named, "paths dirty only".** Possible only by rewording
R-2.8.8 and R-2.8.9: action freshness would compare against rows that are
stale until the replan's scan, so a worker could start an action planned on a
state the user has just changed. Not recommended.

### 4.7 Costs of the duplicated layers

**O5. Double hashing of a new file. VERIFIED mechanism; magnitude unmeasured.**
The watch handler hashes a created file (`watchCreateHash`,
`observer_local_handlers.go:498-520`) but the durable hash cache is filled only
by the scan (`newHashCacheRecorder` is constructed only at `scanner.go:270`).
The replan's scan therefore finds no baseline row and no cache row
(`cachedHashForNewFile`, `scanner_hash_cache.go:20-46`) and hashes the file
again as an `isNew: true` job (`scanner.go:724-736`). The upload reads it a
third time.

**O6. Repeated hashing of a changed known file. VERIFIED mechanism; magnitude
unmeasured.** A baseline-known file whose metadata differs is queued with
`isNew: false` (`scanner.go:792-804`); `addJob` refuses to record those
(`scanner_hash_cache.go:69`), and the cache drops rows whose path has a
baseline row (`pruneLocalHashCacheTx`, `store_local_hash_cache.go:108-117`;
R-2.2.8). So the file is hashed by every full scan, replans and safety scans
alike, until its upload commits and the baseline row changes. For a large file
whose upload is slow, failing or held, that is one full read per scan.

**Fix direction (P9).** Let the scan reuse the hash in the committed
`local_state` row when size, mtime, ctime and identity all match and the file
is outside the racily-clean window, the same contract R-2.2.4 sets for the
baseline (`local_hash_reuse.go:34-66`). The row already carries every field
(`store_local_state.go:333-350`). Interface in `syncdomain`, beside the hash
cache's `Lookup`/`Record` (`split:end-state.md:212-215`):
`type localTruthHashes interface { Lookup(path string) (localStateRow, bool) }`.
Risk: a watch-handler hash taken while the file was still being written; the
identity-and-ctime test plus the existing stable-hash logic
(`stableHashOrEmpty`, `observer_local_handlers.go:539-556`) bound it, but this
needs its own test. **Test first:** `TestFullScan_ReusesCommittedLocalHash`
(R-2.2.4): two scans of a modified known file with a counting `hashFunc`
assert one read. Measure a large-tree replan before and after.

### 4.8 The remote observer

**O4. VERIFIED.** Remote items are classified in `classifyAndConvert`
(`item_converter.go:710-783`): `Type` is written at `:738-766`, `CTag` at
`:731`. Neither is read in production. `appendObservedEvent`
(`observed_items.go:42-71`) copies DriveID, ItemID, ParentID, Path, ItemType,
Hash, Size, SizeUnknown, Mtime, ETag, IsDeleted, ReuploadLocal into the
observed item; not Type, Name, OldPath or CTag. The complete set of production
readers of `emitted`:

- as a count: `engine_watch_maintenance.go:174, 193, 195, 203`;
  `engine_observation_postprocess.go:238, 242`; `engine_primary_root.go:30, 53,
  54`; `observer_remote.go:410`;
- as `Path`, `OldPath`, `ItemType`: `remoteEventHasPlannerVisibleEffect`
  (`engine_observation_postprocess.go:254-260`), which also builds
  `newContentFilter(filter)` once **per event** (`:255`);
  `shouldSkipMountRootWatchBatch` (`engine_mount_root.go:372-381`);
- projection: `observed_items.go:18, 24, 29`;
- returned and discarded: `observeAndCommitRemoteCurrentState` returns
  `observationBatch.emitted` (`engine_current_plan.go:228`) and its only caller
  drops it (`_, pendingRemoteObservation, err := ...`, `:188`).

`remoteObserver.LastActivity` (`observer_remote.go:684`) is called only from
`observer_remote_test.go:1916, 1924`.

**End state.** `remoteObservationResult` (`observed_items.go:7-10`) keeps
`observed` and replaces `emitted []changeEvent` with
`changes []remoteChange` where `type remoteChange struct { Path, OldPath
string; ItemType ItemType }`. `changeEvent.Type` and `.CTag` are deleted, and
with the local side gone (4.6) `changeEvent`, `changeType` and `changeSource`
are deleted altogether; `item_converter` builds `observedItem` and
`remoteChange` directly. `observeAndCommitRemoteCurrentState` loses its first
return value. The content filter is built once per batch. `LastActivity` and
its test (`observer_remote_test.go:1895`) are deleted.

Remote *truth* was already snapshot-based: the planner compares
`remote_state`, `local_state` and `baseline` in SQL (`sqlite_compare.go:56-461`).

### 4.9 The socketio wake path

Nothing to delete. The wake is a payload-free `wakeCh := make(chan struct{},
1)` (`engine_watch.go:517`), consumed by `sleepWatch` (`case <-wakeCh:`,
`observer_remote.go:625-630`) and drained by `drainWakeSignals` (`:646-658`).
It carries no classification. VERIFIED.

### 4.10 Tests, spec, risks

**Exist.** `local_observation_batch_test.go:63`
(`TestLocalObserver_RunSafetyScanEmitsFullLocalSnapshotBatch`);
`engine_watch_rescan_replan_test.go:24`
(`TestWatchRuntime_SafetyScanWithoutNewTruthKeepsAdmittedPlan`), `:110`;
`engine_watch_handlers_test.go:347`; `engine_observation_postprocess_test.go:363`;
`dirty_buffer_test.go:71, 87, 113, 166, 217`;
`observer_local_watch_health_test.go:342, 411`;
`socketio_protocol_test.go:47`; `observer_remote_test.go:1488, 1895`.

**Write first.**

- `TestWatch_CreateSendsOneUpsertPatch`, `TestWatch_DeleteSendsSubtreeRemoval`
  (R-2.8.8): assert on the batch channel; these replace event-channel
  assertions before the channel is removed.
- `TestFullScan_ResultHasNoBaselineDeletionPass` is not a behaviour test; the
  regression guard for item 3 is instead
  `TestFullScan_DeletedSyncedFileHasNoRow` (R-2.8.8): rows, not events, say the
  file is gone.
- `TestRemoteBatch_PlannerVisibleEffect_BuildsFilterOnce` (R-2.8.21) if the
  filter construction is moved.

**Spec.** `spec/design/sync-observation.md`: Ownership Contract (`:54`), Local
Observation (`:334`), Dirty Buffer (`:701`), Verified-By (`:63`).
`spec/design/sync-domain.md`: remove `changeEvent`, `changeType`,
`changeSource`. `spec/design/sync-engine.md:620-635`: state option 1 of 4.5 as
the rule. Requirements: R-2.8.8 (delete wording: "directory delete/rename-away
observations shall delete the path prefix" becomes "every delete observation
removes the path and everything below it").

**Risks.** The test migration is large (about 85 sites) and must not weaken
assertions: each event assertion has a row or delete equivalent, except
create-versus-modify, which has none because nothing consumes it. Any pre-split
PR here changes package-level declarations, so the split simulator must be
re-run (`split:README.md:53-64`).

## 5. Item 4 — lost changes and wake storms in local observation

Only what the code proves. Each defect has a fix and a test.

### 5.1 Defects

**L1. Findings that clear without a row change are not reconciled; in fallback
mode new ones are not either. VERIFIED mechanism; upper bound SUSPECTED.**

- The safety scan sends findings only when there are some: `if
  len(result.Skipped) > 0 { ... o.skippedCh <- result.Skipped ...}`
  (`observer_local_handlers.go:1403-1416`). An empty list is never sent.
- The fallback scan never sends them: "Forward events only — skipped items are
  logged at DEBUG" (`engine_watch.go:701-710`).
- A watch event for a filter-rejected path returns before any handler, for
  creates, writes and deletes: "Watch handlers don't collect SkippedItems — the
  safety scan (FullScan every 5 min) catches them"
  (`observer_local_handlers.go:296-314`). Nothing is marked dirty.
- Filter-rejected entries (invalid name, path too long) never get a
  `local_state` row (`scanner.go:476-509`), so their appearance or
  disappearance changes no row and `ReplaceLocalStateReportingChange` reports
  no change.

Scenario A: the only file with an invalid name is deleted or renamed to a
valid excluded name. No watch batch, the safety scan's `Skipped` is empty, no
signal: the issue stays in `observation_issues` and in status until something
else causes a replan. With nothing else happening, the next replan is the idle
full remote refresh (`fullRemoteRefreshInterval = 24 * time.Hour`,
`domain_observation_mode.go:21`); that bound is SUSPECTED (the idle path was
not traced end to end). Scenario B: watch limit exhausted (fallback mode), a
file with an invalid name appears: no finding is persisted until another
replan.

Contradicts R-2.8.21 ("...shall schedule a replan only when it changes
committed truth: a `local_state` row, the local-truth-complete marker, or the
persisted skipped-file findings", `spec/requirements/sync.md:112`) and R-2.4.12
("clears when a later full scan reads the path normally again", `:56`).

An unreadable file is **not** affected: it still gets an empty-hash row
(`scanner.go:308-315`, `:354-385`), so a row change covers it.

*Minimal fix:* send the list unconditionally at
`observer_local_handlers.go:1403` and send it from `runFallbackScan`.
*Structural fix (P4):* findings and `crossedMounts` travel inside
`localSnapshot` (4.6); one engine function
`commitLocalSnapshot(ctx, localSnapshot) (changed bool, err error)` applies
rows, findings and mount boundaries, and is the same function the replan path
uses (`engine_current_plan.go:287-316`, `:394-399`). Deleted: `skippedCh`
(`observer_local.go:550-553`), `SetSkippedChannel` (`:590-594`),
`resources.skippedItems` (`engine_watch.go:249`, `:362`, `:390`),
`handleWatchSkippedSignal` (`engine_watch_loop.go:375-405`) and its select arm
(`:244`).
*Tests first:* `TestRunSafetyScan_ClearedLastFindingReplans` (R-2.8.21): a
persisted finding, a scan with an empty `Skipped`, assert the issue is gone
and the buffer is dirty. `TestRunFallbackScan_PersistsNewFinding` (R-2.8.12
second text, R-2.8.21).

**L2. The existing test for "a finding that cleared replans" makes a call
production never makes. VERIFIED.**
`TestWatchRuntime_SkippedFindingsReplanOnlyWhenTheyChange`
(`engine_watch_rescan_replan_test.go:105-132`) calls
`rt.handleWatchSkippedSignal(ctx, nil, true)` (`:129`). In production the
sender is guarded by `len(result.Skipped) > 0`, so a nil or empty list never
arrives. The test passes while the behaviour it names does not happen. Fix:
with L1's fix the call becomes real; until then the test should drive
`runSafetyScan`, not the handler.

**L3. Rows and findings of one safety scan travel separately, and the findings
can be dropped. VERIFIED.** Rows go on `localBatches`
(`observer_local_handlers.go:1393`); findings on a 2-slot channel
(`engine_watch.go:362`) with a non-blocking send whose default branch logs
"skipped items channel full, will catch on next scan"
(`observer_local_handlers.go:1406-1411`). Between the two the engine can replan
on rows without their findings. Bounded by the replan's own scan, which
reconciles both (`engine_current_plan.go:296`). Same fix as L1 (P4).

**L4. The single-name length check counts UTF-8 bytes. Code VERIFIED;
reachability SUSPECTED.** `if len(name) > maxComponentLength { return
IssueInvalidFilename, fmt.Sprintf("the name is longer than %d characters",
maxComponentLength) }` (`scanner.go:1368-1370`), `maxComponentLength = 255`
(`:61`). `len` is bytes; the message says characters; the neighbouring path
check counts UTF-16 units (`oneDrivePathLength`, `scanner.go:1512-1514`;
R-2.11.10). OneDrive accepts a 300-character name by JSON body and emoji names
up to 260 UTF-16 units (`spec/reference/graph-api-quirks.md:1169-1178`). No
requirement covers a local-to-remote single-name limit: R-2.11.8 is the remote-
to-local direction (`spec/requirements/sync.md:201`). On a filesystem whose
per-name limit is 255 bytes the check can never fire for a name OneDrive would
take. It can on NTFS or exFAT through `fuseblk`, which R-2.8.19 classifies as
local (`:110`): a 100-character CJK name is 300 bytes, valid for OneDrive, and
is refused as "longer than 255 characters". Reachability on such a mount was
not tested (SUSPECTED; a test on a `fuseblk` mount settles it).
*Fix:* count UTF-16 units against OneDrive's single-name limit and correct the
message. The exact limit is between 300 (works) and 400 (fails) and is
unprobed; path-routed requests fail near 292-300 characters
(`graph-api-quirks.md:1143`, `:1175-1178`), so the limit must be chosen from a
probe, not from this report. Add a requirement next to R-2.11.10.
*Test:* update `TestValidateOneDriveName_MaxLength`
(`observer_edge_test.go:236`, today "255 `a` valid, 256 invalid") with a
multi-byte case.

**L5. Requirement ID R-2.8.12 is used twice. VERIFIED.**
`spec/requirements/sync.md:102` (timed pause ends on its own) and `:103` (watch
limits: ENOSPC, EMFILE, ENFILE, ENOMEM fallback). `// Validates: R-2.8.12`
is therefore ambiguous, for example on
`TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval`
(`observer_local_watch_health_test.go:578`). Fix: renumber the second to the
next free R-2.8 ID (R-2.8.21 at `:112` is the highest read) and update its
`Validates` lines and design-doc references. Spec-only PR.

**L6. A watch delete of a directory that is neither in the baseline nor
watched removes only the exact row. VERIFIED; bounded.** `HandleDelete` guesses
the type: file unless the baseline says folder or the path is a watched
directory (`observer_local_handlers.go:1149-1155`); a file-typed delete becomes
`deletedPaths` (`local_observation_batch.go:24-29`), so descendant rows stay
until the replan's scan replaces the snapshot. No planning defect, because the
replan re-scans. *Fix:* every delete removes the path and its subtree;
`deleteLocalStatePrefixTx` already deletes both (`WHERE path = ? COLLATE BINARY
OR substr(path, 1, length(?)) = ? COLLATE BINARY`,
`store_local_state.go:360-375`); the guess and `deletedPaths` go. The `substr`
predicate cannot use the primary-key index; a range predicate (`path >= p||'/'
AND path < p||'0'`) can. Measure before changing it.
*Test:* `TestHandleDelete_UnwatchedNewDirectoryRemovesSubtreeRows` (R-2.8.8).

**L7. The watch observer's mount knowledge is fixed at start, and its
snapshots drop `CrossedMounts`. VERIFIED mechanism; no planning defect.** Flags
are loaded once (`engine_watch.go:374-378`); `TrySendFullLocalSnapshot` sends
rows only (`observer_local.go:765-770`); `runFallbackScan` ignores
`result.CrossedMounts` (`engine_watch.go:675-714`). The replan's observer
reloads the flags and records crossed mounts every time
(`engine_current_plan.go:380-384`), so planning is unaffected; the cost is an
interim `local_state` that differs from what the replan will compute, which
can cause a spurious "changed" from a safety scan. Fixed by `localSnapshot`
carrying `crossedMounts` (P4). Related, VERIFIED absence: watch setup and
create handlers do not test for a foreign mount point (`isForeignMountPoint` is
called only at `scanner.go:628`); consequence SUSPECTED (watches spent on a
foreign mount until the next scan).

**C1 and C2** (section 3.2) are also lost-change defects of local observation.

### 5.2 Checked and found sound

| Topic | Evidence |
|---|---|
| fsnotify overflow | `handleWatcherError` recognises `fsnotify.ErrEventOverflow`, marks local truth suspect and arms a recovery scan (`observer_local_handlers.go:225-235`); test `TestWatch_EventOverflowRunsRecoveryScan` (`observer_local_watch_health_test.go:342`) |
| Dropped batch | a full batch channel counts the drop and requests a recovery scan (`observer_local.go:728-747`, `:780-820`); test `:411` |
| Recovery-scan damping | 10 s floor (`observer_local.go:46`), R-2.8.13 |
| Watch limits | ENOSPC, EMFILE/ENFILE, ENOMEM classified (`observer_local.go:227-298`); fallback to full scans with retry (`engine_watch.go:449-494`); only the file limit stops watching (`observer_local.go:296-298`), as the second R-2.8.12 text specifies; inotify capacity warning at 80% (`inotify_linux.go:22`, `:50-76`) |
| Rename pairs | handled as delete of the old name plus create of the new (`observer_local_handlers.go:316-329`); a `Remove`/`Rename` for a name still on disk is ignored (`localPathStillPresent`, `:1100-1110`, `:1232-1255`); R-2.8.15 |
| Debounce | max wait `max(30 s, 6 × debounce)` (`buffer.go:75-88`); R-2.8.20 |
| Write coalescing | 500 ms cooldown, 30 s cap, unstable-write limits (`observer_local.go:40-72`); R-2.8.16 |
| Safety-scan interval | 5 min (`observer_local.go:35`); 2 min on network/FUSE filesystems (`:990`, `adaptToRootFilesystem` `:994-1020`); R-2.8.19 |
| Re-watch cap | 256 directories per scan (`observer_local_handlers.go:108`, `:117-166`), required by R-2.8.13 |
| Watcher-error backoff | the loop sleeps after any watcher error, overflow included (`observer_local_handlers.go:239`; policy 1 s to 30 s, `internal/retry/named.go:172-183`); deliberate damping, events queue in the kernel meanwhile |
| Mount crossing (`cross_filesystems`) | detected in the scan (`scanner.go:626-640`); the replan's scan is authoritative (L7); tests `engine_unavailable_not_absent_test.go:180-375` |
| Symlinks | no defect found at the sites read (`scanner.go:449-471`; `symlink_observation.go:334, 480, 619`; tests `observer_local_test.go:638`, `observer_local_delete_test.go:141, 438`; R-2.4.6). The file was not read in full; no further claim is made |

By design, not defects: a watch-detected collision creates no issue until a
full scan (section 3.1 row 2); `EstimateDirCount` counts baseline folders only
(`observer_local.go:896-906`), so it under-counts before the first sync;
`observation_issues` holds one issue per path
(`store_observation_issues.go:153-159`), so two findings for one path keep the
last (impact SUSPECTED, low).

## 6. Dependency-ordered PR breakdown

**Rule for every PR that touches `internal/sync`:** land wholly before or
wholly after the split's mechanical move, never during. Every pre-split PR
that adds, renames or removes a package-level declaration requires re-running
the split simulator, because `conf/production.txt` is hand-written against
`70301c0e` (`split:README.md:36`, `:53-64`).

### Independent of the split (can land any time)

| PR | Content | Depends on |
|---|---|---|
| **P0** | Renumber the second R-2.8.12 (L5). Spec and `Validates` comments only | — |
| **P1** | `namekey.Key`/`Of`, `PathKey`/`OfPath`/`HasPrefix`; retype 25 sites; delete `pathNameKey`, `foldMovePath`; rewrite `isWithinRemotePath` (N1). Touches `internal/sync` only by renaming calls and deleting two unexported helpers | — |
| **P2** | `namekey.Relate`/`Find`/`FindStored`; adopt in `internal/graph`, `internal/driveops`, `internal/cli`, `internal/synctree` (N3, N5). No `internal/sync` change | P1 |
| **F1** | L1 minimal fix: always send findings, also from `runFallbackScan`; redirect the L2 test | — (superseded by P4) |
| **F2** | L4: UTF-16 count, message, new requirement, after a probe fixes the limit | — |
| **F3** | L6: watch deletes always remove the subtree | — |
| **F4** | C1 narrow fix, if urgency demands it before P7: in `processObservedInfo`, a second entry for an already observed stored path records `issueCaseCollision` with the same-form detail (`scanner.go:642`); test `TestFullScan_UnicodeFormTwinOfSyncedFileIsACollision` | — (superseded by P7) |

### Best before the split (they shrink what the split must export)

| PR | Content | Why before | Depends on |
|---|---|---|---|
| **P3** | Delete the dead local event channel and classification (4.3 items 1-5, L8); migrate about 85 test sites to the batch channel | `changeEvent` would be exported to 107 sites, 77 in `syncobserve` (`split:linux/exports.tsv:16`); tests are rewritten once, not moved and then rewritten | — |
| **P4** | `localObservation` sum type with findings and crossed mounts in the snapshot; delete `skippedCh` and `handleWatchSkippedSignal` (L1, L2, L3, L7) | `localObservationBatch` becomes an exported observer-to-engine message with 7 fields at 17 sites (`split:end-state.md:226-228`); define its final shape first | P3 |
| **P5** | Remote: `remoteChange`, delete `Type`/`CTag`/`changeEvent`/`changeType`/`changeSource`, drop the discarded return, build the filter once, delete `LastActivity` (O3, O4) | `remoteObservationBatch` is exported with 14 fields at 71 sites (`split:end-state.md:226-228`) | P3 |
| **P6** | `namekey.Stored`/`StoredPath`; delete `nfcNormalize`; one `storedPathOf` (N2) | otherwise `nfcNormalize` moves to `syncdomain` and is exported to 36 sites (`split:end-state.md:117`; `split:linux/exports.tsv:123`) | P1 |

If the split lands first, P3-P6 are still correct; they then touch more
packages and the test migration happens across package boundaries.

### Must come after the split

| PR | Content | Why after | Depends on |
|---|---|---|---|
| **P7** | One local detector: `dirNames` and `localNameCollisions` in `syncobserve`; watch check on the same rule; delete the baseline name index from `syncdomain` (`Baseline`) and `syncstore` (its loader) and `RecentLocalDeletes` (C1, C2, C3, N6) | Removes a `syncdomain` type's field and methods used from three packages (`dirNameKeyFromPath` is a `syncstore` cross-package site, `split:linux/exports.tsv:48`); doing it mid-split invalidates the hand-written configuration in three places. Needs the C3 spec decision | P1, P3, split |
| **P8** | `classifyCreateConflict` in `syncexec` replacing `adoptExistingRemoteFolder`'s two passes and `resolveCreateConflict` (N3, N4) | Pure executor change; independent in content, placed here only to avoid churn in files the split moves. May land before the split instead | P2 |
| **P9** | Hash reuse from committed local truth (O5, O6), after measurement | Needs a new interface in `syncdomain` implemented by `syncstore` and used by `syncobserve`: exactly the seam the split defines for the hash cache (`split:end-state.md:212-215`) | P3, split |
| **P10** | Option B: delete watch-side collision detection (C4); reword R-2.12.2 | Only after P7 proves the scan detector complete, and only with the explicit decision that the replan scan stays (4.5) | P7 |

**Suggested order:** P0 → P1 → P2 → P3 → P4 → P5 → P6 → *split* → P7 → P8 →
P9 → P10. F1-F4 at any time before the PR that supersedes each.

## 7. Not verified, and not read

**Not verified — what would settle each**

- Per-segment versus whole-path NFC equality (N2): the property test in 2.8.
- C1 downstream: that the planner plans an upload from the twin's row and that
  `action.View.Local` is that row; the engine test in 3.4.
- C2 end to end: the watch test in 3.4.
- Impact of the tie-break differences (N3): two-candidate tests per matcher.
- Reachability of L4 on NTFS/exFAT: a test on a `fuseblk` mount.
- The exact OneDrive single-name limit between 300 and 400 characters, and
  whether reserved names are matched under the collation: live probes.
- OneDrive for Business name equivalence: unprobed by the project
  (`spec/reference/graph-api-quirks.md:1135`).
- Magnitude of O5, O6 and of the per-replan walk: a benchmark or trace on a
  large synthetic tree; nothing was measured.
- The 24 h upper bound for a stale finding in L1: trace the idle full-refresh
  path to a replan.
- Whether colliding paths keep their `local_state` rows after a scan (only the
  event list is filtered at `scanner.go:190`; the row map was not followed to
  the return).
- Whether `synctree` could import a future `syncdomain` (2.5).
- Whether a followed symlink can make one walk visit a stored path twice (3.6).

**Not read**

- `symlink_observation.go` beyond its three name sites.
- The planner comparison SQL for the C1 case (`sqlite_compare.go` was read for
  structure only); `uploadSourceInfo` (`executor_preconditions.go:378`).
- Shortcut code other than `shortcut_root_lifecycle.go:383-394` and `:582`.
- One-directional sync modes, for O6.
- macOS/kqueue-specific watcher code; Windows paths.
- `internal/multisync`, `e2e/`, and `internal/devtool` beyond confirming they
  contain no name comparison in production paths.
- The callers of the ASCII-lowering helper documented at
  `domain_path_kind.go:19`.
- Git history: no `log`, `show` or `blame` was needed.
