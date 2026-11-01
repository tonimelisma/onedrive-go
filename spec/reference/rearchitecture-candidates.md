# Rearchitecture Candidates

Structural changes that would make whole defect classes impossible to write,
not just easier to catch. Written 2026-10-25 from the hardening research and
updated 2026-10-31 after PRs #799-#825. Each entry says what it would change,
the evidence, how far the hardening PRs already went, and how to research it
before committing to it. Nothing here is decided; each needs an ADR first.

## How the list was produced, and how to redo it

1. **Defect corpus.** 95 distinct defects (D-001..D-132), drawn from about
   1,400 upstream issues and threads: the abraunegg/onedrive tracker, rclone
   OneDrive and bisync issues, Microsoft Q&A and Graph documentation issues,
   Syncthing, Nextcloud, Dropbox, Unison and Mutagen bug classes, and
   filesystem-semantics catalogs. Added to those: the model-based test's seeds
   and the live probes in [live-probes.md](live-probes.md).
2. **Root-cause tags.** Each defect is tagged with the structural reason it
   was possible, not its symptom: "a path was used as an identity",
   "absence was inferred, not observed", "a server reply was trusted",
   "a name crossed a URL".
3. **Clusters.** A tag shared by many defects in different packages points to
   a missing abstraction. The test for each candidate: would the structure
   have made the defect impossible to write, or only easier to catch?
4. **Prior art.** For each cluster, the systems that solved it structurally:
   Dropbox Nucleus (three trees keyed by node ID; the CanopyCheck and Trinity
   testing posts), Unison (Balasubramaniam and Pierce, "What is a file
   synchronizer?"), Syncthing's version vectors, rclone bisync's listing
   diff, Mutagen's three-way merge over snapshots, and deterministic
   simulation in FoundationDB and TigerBeetle's VOPR.
5. **Cost and benefit.** Benefit: defects closed structurally plus classes
   prevented in future work. Cost: lines touched, migration risk, test
   rewrite. Gating question: can it land incrementally behind the current
   code, on the same store and the same tests?

Researching any one item properly means: write an ADR stating the invariant
it enforces; prototype it on one vertical slice (one action type, end to
end); run the model test and the scenario suite against both
implementations; compare the defect classes each exposes; only then plan
the migration.

## Candidates, ranked by evidence

### 1. Identity-keyed reconciliation instead of path-universe comparison

**Today:** the comparison joins baseline, local and remote rows on path
(`sqlite_compare.go`). Moves are recovered afterwards by pairing deletes with
creates, behind gates such as "destination not occupied" and "source still
exists".

**Evidence:** most of WS5 (D-050..D-059e) and part of WS4 (D-060, D-061).
Every one of the 14 move-composition fixes in #799 is a rule that recovers
identity from paths, and the model test's remaining non-converging seeds are
more of the same (cross-move cycles, divergent renames, a move of an edited
file).

**Landed:** #808 applies each delta window as a set, keyed by item ID, and
rebinds a folder recreated online at the same path (D-060, D-061), which
removes the path-ordering wedges on the remote side. #810 composes moves per
item by (parent item, name) with SQLite keeping only structural pairing;
#819 and #822 add the entry's birth time to local identity, the generation
guard this item asks for; the model test converges 80/80 seeds (#810) and
the planner-level model 500/500 (#812).

**Proposal:** reconcile three trees whose nodes are keyed by stable identity:
the remote item ID, the local (device, inode) with a generation guard, and
the baseline row. A change is an attribute delta on a node (name, parent,
content, type), so a move and an edit compose trivially. Paths become a
derived view for addressing the filesystem and for display. This is
Nucleus's design.

**Risk:** the largest change. Local identity on Linux is weaker than
Dropbox's (inode reuse, FAT, network filesystems), so it needs a
"local identity confidence" with a fallback to today's path pairing.

**How to research it:** write the planner as a pure function over the
existing `baseline`, `local_state` and `remote_state` rows (they already
carry item IDs and inodes); run it in shadow mode beside the current planner
in the model test; diff the plans and count converging seeds.

### 2. A name model: comparison key separate from spelling

**Evidence:** D-020..D-032, D-151, and the probes: OneDrive's name equality
is its own collation (L-33, M-1), it ignores characters outside the BMP
(L-30), and it keeps NFD bytes while refusing a normalization-only rename
(L-04).

**Landed:** #804 added `internal/namekey`, an explicit table-driven key that
replays every probed pair, and routed every name comparison through it.

**Still open:** names are still plain strings, so a new comparison site can
bypass the key. D-023 (cross-side grouping in `sqlite_compare.go`) remains;
D-025 (NFD names on byte-exact Linux filesystems) was closed by #815
(`synctree.Root.Spelling`) and #823.

**Proposal:** one `Name` value type holding on-disk bytes, the remote
spelling and the key, with a lint that forbids `EqualFold`, `ToLower` and
`==` on names in `internal/sync`.

**How to research it:** count the remaining string comparisons on names;
prototype the type in one package; add a nightly live job that re-runs the
collation probes, so a server-side change is caught.

### 3. Positive-evidence deletion

**Evidence:** D-001..D-012, D-062, D-063, and the "deleted my whole OneDrive"
incident class in other clients.

**Landed:** #805: the sync folder carries an identity token, mass and
emptied-side deletes are held, local deletes go to a trash, and orphans from
a full rescan are confirmed with a lookup by ID before they become deletes.
#808 routes every expired or unusable delta cursor through that confirmed
full refresh, and reads the 410's resync flavour so an "upload differences"
resync re-uploads local copies instead of deleting them. #811 makes a synced
folder's tombstone pull the full refresh forward; #812 backs off undecided
orphans and applies orphan confirmation to shared-folder mounts; #822 binds
the root's birth time; #825 records a folder kept for ignored content so it
is not re-created online.

**Still open:** the evidence is checked at each site, not carried. Moving a
synced item into an excluded folder still deletes it online.

**Proposal:** every delete action cites evidence from a closed set (delta
tombstone, 404 by ID, local not-found on a confirmed-identity path, a user
command) as a typed field; "not seen" alone yields an unconfirmed absence
that schedules a confirming read. The hold becomes a policy over evidence
counts.

**How to research it:** audit every delete-producing path for the evidence
it uses, then make the field mandatory so the compiler finds every site.

### 4. Name-free Graph addressing

**Evidence:** D-020, D-021, D-022, D-027, D-029; probe 4 (`v1.0` was routed
to `_api/v2.0`), L-05 and L-35 (URL length limits).

**Landed:** #804: new files are created by body with
`conflictBehavior=fail`, content goes by item ID, overwrites and moves carry
If-Match, and names are never URL-decoded.

**Still open:** path-routed lookups remain for CLI path resolution and some
fallbacks.

**Proposal:** a single `resolvePath` for the CLI is the only place a name
enters a URL; a request-shape golden test fails on any other.

### 5. Intents and receipts for every mutation

**Evidence:** D-033..D-041, D-046, D-047, D-150 (a rename reset the
modification time and nothing checked the reply).

**Landed:** #803's transfer verification, #804's pending-create record, and
#807's modification time carried on moves. #818 recognises the client's
placeholder by item ID and emptiness rather than its creation eTag; #824
records the destination's change time after an applied move.

**Still open:** the move reply's echo is not checked, and each action still
writes the baseline from its own ad-hoc checks.

**Proposal:** every executor action produces an intent (expected size, hash,
name, parent, modification time); every Graph reply becomes a receipt; one
verifier yields committed, committed-transformed (a known quirk),
diverged or failed. Only committed outcomes write the baseline, and the
baseline records observed facts, never intended ones.

**How to research it:** inventory the sites that write baseline facts after
an action; each is where a receipt would replace ad-hoc checks.

### 6. A journaled executor with crash points

**Evidence:** D-036, D-037, D-050, D-091, and the "re-upload forever after a
lost final fragment" class.

**Landed:** the pending-create record (#804) is a first durable intent, and
#811 added the crash-point harness (`internal/sync/crash_points.go`, D-091):
six named commit boundaries with a test hook that kills the run at each, and
recovery must converge. The env-var injector proposed below exists as an
in-process hook.

**Proposal:** a durable intent before every remote mutation; on start,
resolve open intents (did it land?) before planning; an
`ONEDRIVE_GO_CRASHPOINT=<name>` injector so tests kill the process at every
boundary.

**How to research it:** enumerate the mutation boundaries, kill the process
at each in a loop against the fake, and record which states fail to
converge.

### 7. Deterministic simulation as the primary test

**Evidence:** the model test found more real bugs in a day than the
scenario suite had. #811 added the engine-owned dispatch order and
serialized worker (D-090) and the `testing/synctest` virtual clock (D-095);
#812 the chaos Graph layer (D-092), filesystem shims (D-094) and the
planner-level model (D-093). Remaining: uploads, hashing and
followed-symlink reads bypass the filesystem seam via `localpath`
(system.md), and `time.Now`/`os.*` sites outside the seams.

**Proposal:** one seeded scheduler owns worker dispatch, watcher events and
timers; a fake clock; the fake Graph with chaos (429, 5xx, timeouts,
reordered pages, lost replies); a fake filesystem with capability flags
(case-insensitive, NFD, 2-second mtime, ENOSPC). Every failure replays from
its seed.

**How to research it:** count the remaining direct `time.Now`, `os.*` and
goroutine sites outside the seams; WS9's dispatch seam is the first step.

### 8. A per-drive capability profile with a live conformance suite

**Evidence:** several spec claims disproved by probes (permanentDelete,
double versions, encoded names), behaviour that differs by drive type, and
conditionals scattered across the graph and sync packages.

**Proposal:** a `DriveProfile` (Personal, Business, SharePoint, shared
folder) declares collation, version behaviour, hash availability, reserved
names and limits. The probes become a Go conformance suite run nightly
against the test account that fails when the server drifts, and
graph-api-quirks.md is checked against its results.

### 9. Separate metadata change from content change

**Evidence:** D-150 (a web rename changes the modification time), D-057
(filesystem identity used as a content signal), D-066 (mtime-preserving
edits), hashless Business items.

**Landed:** #808 trusts a cached local hash only when size, nanosecond mtime,
ctime and file identity all match (D-066). #810: identity is never a content
signal (D-057) and a hash known on one side only is not an edit (D-059b).

**Proposal:** observations carry independent content facts (a hash, or size
and mtime with a confidence) and metadata facts (name, parent, mtime).
Planning acts on each separately: a metadata-only change never transfers
content, and a low-confidence content fact schedules a verify read instead
of a transfer.

### 10. A user-facing "why" log

**Evidence:** "why was my file deleted or renamed?" is the top trust
complaint in every client studied.

**Proposal:** record every applied action with its evidence (item 3) and
receipt (item 5) in a bounded SQLite history; `onedrive-go explain <path>`
prints the item's last decisions in plain language. It doubles as the
debugging tool for the model and live tests.

## Sequencing

- Items 4 and 5 paid off first and are mostly landed; finishing 5 (move
  echo, one verifier) is the next cheap step.
- Item 3 builds on the hold and confirmations from #805.
- Item 2 builds on `internal/namekey` from #804.
- Item 7's harness landed in WS9; what remains is routing the last direct
  I/O through the seams.
- Item 1 is the big one: do it last, in shadow mode, measured by the model
  test.
- Items 6, 8, 9 and 10 are independent and medium-sized.
