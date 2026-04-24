# Shared Shortcut Mount Architecture Draft

Status: historical working draft for the multi-increment refactor. Governed
behavior now lives in `spec/design/sync-control-plane.md`,
`spec/design/sync-observation.md`, `spec/design/config.md`, and
`spec/requirements/sync.md`. Keep this file as rationale/reference only; update
the governed specs for current behavior.

## Purpose

This draft records a blank-slate architecture for syncing OneDrive shared folders
added as shortcuts. It is intentionally not a retrofit memo. The goal is to:

- describe the end-state model we would choose from first principles
- support both:
  - automatic "shortcut just works" mounts
  - explicit "add this shared folder as a separate drive" mounts
- explain where each major responsibility should live
- identify which current shortcut/shared-root mechanisms are temporary stepping
  stones versus dead-end vestiges
- guide a large refactor over several increments without reintroducing the
  accidental architecture that previously existed

This draft assumes the repository's stated rule that there is no backwards
compatibility requirement for config shape, state DB schema, CLI flags, or
internal APIs.

## Reading Guide

This document is intentionally opinionated about the target architecture.

- `Core Concepts` through `Summary` describe the blank-slate end state we would
  choose from first principles.
- The current repo, spec, and history analysis exists only to help us migrate
  there deliberately.
- If the target architecture and the current codebase disagree, the target
  architecture wins and the migration plan must explain how to change the repo.

The appendices are therefore subordinate to the target model:

- `Appendix A` validates the target against today's repo/spec reality
- `Appendix B` maps current concepts into the target model
- `Appendix C` classifies vestiges versus useful transitional scaffolding
- `Appendix D` gives the staged migration plan

## Problem Statement

OneDrive shared-folder shortcuts are mount points, not normal items.

Each shortcut has two distinct identities:

- a local namespace entry in the recipient's drive, with recipient-owned alias,
  placement, and lifecycle semantics
- an authoritative remote content root in another drive, with its own remote
  cursor, truth, permissions, and failure modes

The current repository shape partially models the second identity by treating a
shared folder as a separately configured "shared-root drive". That was useful as
an isolated runtime surface, but it also left behind a large amount of
shared-root-specific plumbing inside systems whose real design center is still
"one engine owns one drive-root-shaped session".

The main architectural mismatch is this:

- the user experiences one local namespace tree
- a user may also intentionally choose to surface the same remote subtree as its
  own top-level synced root
- the current engine is designed around one primary remote root and one durable
  sync authority
- shared shortcuts add multiple remote authorities inside that one local
  namespace

Trying to make one existing single-root engine directly own several physical
drives plus shortcut roots would collapse too many authority boundaries at once:

- remote observation ownership
- cursor ownership
- retry/block-scope ownership
- planner/action-graph ownership
- local subtree ownership
- watch-loop runtime ownership
- durable store identity

## Design Goals

- Preserve a single clear owner for each mutable runtime boundary.
- Keep content sync engines single-root and easy to reason about.
- Support "user added a shortcut and it just works" without requiring user
  configuration for every shortcut.
- Preserve explicit "add as separate drive" as a valid long-term product surface.
- Keep remote resume state and durable sync state scoped to one authoritative
  remote root at a time.
- Preserve strong failure isolation between independent mount roots.
- Keep local namespace semantics correct:
  - renaming a shortcut root is a mount-alias change, not a remote folder rename
  - deleting a shortcut root removes the shortcut mount, not the owner's content
- Avoid embedding mount-specific metadata into every action and event when the
  engine can instead run mount-local.
- Make dead code and temporary shims easy to identify and remove later.

## Non-Goals

- This draft does not design a user-facing CLI for shortcut management.
- This draft does not assume shortcut syncing must always surface as either:
  - child mounts inside a parent namespace
  - explicit top-level configured drives
  Both projection modes are valid.
- This draft does not optimize first for minimum goroutine count or minimum
  polling count. It optimizes first for correct ownership and understandable
  runtime boundaries.
- This draft does not preserve "configured shared drive" as the only durable
  representation of a shared-folder runtime. Transitional reuse is allowed;
  target architecture is not bound by that shape.

## Core Concepts

### Namespace

A namespace is one user-visible synced local root. It is the thing the user sees
in Finder or the filesystem.

A namespace contains one or more mounts.

### Mount Instance

A mount instance is one local mount point inside a namespace.

It owns:

- the local mount path
- the local alias/display name
- lifecycle semantics for the mount root itself
- association with one authoritative remote content root

It does not own content reconciliation below that root.

### Mount Projection

A mount projection describes how a mount instance is surfaced locally.

Recommended projection kinds:

- `standalone`
  the mount is its own top-level synced root
- `child`
  the mount appears inside a namespace managed by another root

Projection is a local-surface decision, not a content-engine identity decision.

### Content Root

A content root is the authoritative remote subtree being synced.

It owns:

- remote identity
- remote observation cursor family
- remote truth
- remote permissions
- remote failure and retry semantics

The stable content-root identity should be based on the real owner-side target,
not the shortcut placeholder:

- token-owning account
- remote drive ID
- remote item ID

### Mount Inventory

Mount inventory is the durable registry of namespaces and mount instances.

It owns:

- which mounts currently exist
- which namespace each mount belongs to
- which local mount path each mount occupies
- which content root each mount points at
- which account/token scope authenticates the mount
- lifecycle state for adds/removals/renames

It does not own per-mount content sync state.

### Mount Engine

A mount engine is a single-root content sync runtime.

It owns:

- local observation below one mounted content root
- remote observation for one authoritative content root
- one action graph
- one worker frontier
- one retry/block-scope authority set
- one per-mount sync store

It does not own namespace-level mount discovery or mount-root lifecycle.

### Shared Process Capabilities

These are reusable process-wide facilities, not sources of per-mount truth:

- token source cache
- HTTP client pools
- transfer semaphores and aggregate concurrency budgets
- optionally account-scoped throttle knowledge
- logging and perf aggregation

## Chosen Blank-Slate Architecture

### High-Level Shape

The recommended target model is:

- one control-plane mount orchestrator
- one namespace manager per namespace that actually contains child mounts
- one mount engine for each active mount
- one shared capability layer for auth, transports, and aggregate budgets

Each mount engine owns one content root. A mount may be projected either as a
standalone top-level root or as a child mount inside a namespace. When a
namespace exists, the namespace manager owns the mount table and the mount-root
directory entry itself, while descendants under that root belong to the mount
engine.

### Why This Shape

This keeps the cleanest ownership boundaries:

- namespace concerns stay above content sync when child mounts exist
- content sync remains single-root
- durable resume state stays one-root-per-store
- failures in one shortcut do not poison unrelated shortcuts
- the product can support both explicit separate drives and automatic child
  mounts without changing the engine model

### Rejected End-State Shapes

`One giant multi-root engine`

- Valid only if the entire engine is redesigned around mount-aware planning,
  mount-aware cursors, mount-aware watch ownership, and mount-aware durable
  schema from day one.
- Not compatible with the current engine/store contract.
- Rejected because it combines too many authorities into one runtime and would
  require a different engine, not a small extension.

`Parent engine with fake subengines`

- Rejected because it muddies ownership.
- If subengines own their own store, cursor, planner, and retries, they are
  already real engines and should be orchestrated explicitly.
- If the parent owns local observation while children own remote/store state,
  responsibility gets split across boundaries that currently work precisely
  because observation, planning, execution, and retry are co-owned.

`Hidden child engines created from ephemeral discovery only`

- Better than a giant engine, but discovery misses or temporary Graph gaps could
  incorrectly remove active mounts.
- Rejected unless backed by durable managed mount inventory.

## Responsibility Placement

### Namespace Manager

The namespace manager should own:

- namespace root identity
- mount discovery/reconciliation for shortcut placeholders
- durable mount inventory writes
- creating, renaming, and removing local mount root directory entries
- deciding when a mount engine should start, stop, or restart
- routing mount-root lifecycle events:
  - root rename
  - root removal
  - mount add
  - mount conflict
- parent namespace exclusions so one mount engine never scans another mount's
  subtree
- top-level status aggregation across mounts in the namespace

The namespace manager should not own:

- file reconciliation within a mount
- remote observation cursors
- per-file retries
- per-mount block scopes
- planner/action graph policy

### Mount Inventory

Mount inventory should own these durable facts:

- `NamespaceID`
- `MountID`
- `MountKind`
  - `primary`
  - `shortcut`
- `ProjectionKind`
  - `standalone`
  - `child`
- `LocalMountPath`
- `LocalAlias`
- `TokenOwnerCanonical`
- `RemoteDriveID`
- `RemoteItemID`
- `PlaceholderIdentity` when available and useful for lifecycle reconciliation
- `MountState`
  - active
  - pending removal
  - conflict
  - unavailable

Mount inventory should not own:

- local snapshot truth
- remote snapshot truth
- retry/block state
- observation cursor

### Mount Engine

Each mount engine should own:

- one local descendant subtree under one mount root
- one remote content root
- one current-truth observe/build/reconcile/execute pipeline
- one watch runtime and one watch-loop owner
- one per-mount store
- one planner/action graph
- one retry lane set
- one block-scope set
- one mount-local report/status stream

Each mount engine should not own:

- mount discovery
- mount alias lifecycle
- namespace-level root add/remove decisions
- sibling mount exclusion policy

### Shared Capability Layer

The shared capability layer should own:

- auth token caching
- Graph client construction
- shared transport pools
- aggregate upload/download concurrency budgets
- maybe account-scoped throttle gates

It should not own:

- mount inventory
- per-mount store state
- per-mount runtime queues or retries

## Detailed Functionality Placement

### Shortcut Discovery And Reconciliation

This belongs to the namespace manager plus mount inventory.

Responsibilities:

- enumerate or otherwise discover shortcut placeholder objects in the namespace's
  backing drive
- resolve each placeholder to an authoritative content root
- reconcile discovery against durable mount inventory
- detect adds, removals, alias changes, and conflicts
- start or stop mount engines accordingly

This does not belong to the content engine because shortcut discovery is about
namespace topology, not subtree content truth.

### Local Root Lifecycle

The namespace manager owns the mount root entry itself.

Responsibilities:

- ensure the mount root exists locally
- treat mount-root rename as alias/mount-path change
- treat mount-root delete as unmount/remove-shortcut intent
- prevent parent content engines from treating mount-root lifecycle as ordinary
  file content mutations

The mount engine owns descendants under the root, not the root binding itself.

### Local Observation

Local observation should be split:

- namespace manager observes enough at the namespace boundary to notice mount
  root lifecycle changes
- mount engine observes the local subtree below its mount root

The primary mount engine must exclude child shortcut mount paths from its local
scan/watch set. Otherwise two engines would claim the same local subtree.

### Remote Observation

Remote observation belongs entirely to the mount engine.

Each mount engine owns:

- one remote observation method choice
  - drive-root delta
  - folder-root delta
  - recursive enumeration
- one remote cursor family
- one refresh cadence
- one remote finding batch stream

Remote observation for one shortcut should never reuse another shortcut's cursor
or durable resume state.

### Planning

Planning belongs to the mount engine and should be mount-local.

This means:

- the planner should reason in the mount's local path space
- the planner should not need per-action mount-root metadata to rediscover where
  the engine is rooted
- moves across mount boundaries are not normal moves
- cross-mount boundary crossings are decomposed operations mediated above or at
  the edge of engines, not within one mount-local action graph

### Execution

Execution belongs to the mount engine.

Within a mount engine:

- every remote path is relative to the mount's remote root
- every local path is relative to the mount's local subtree
- path convergence and permission probing are mount-local

The executor should not need to keep rediscovering root item IDs on every
action once the engine itself is mount-rooted. Increment 4 now enforces that
for ordinary sync actions.

### Retry, Permissions, And Block Scopes

These belong to the mount engine, with one caveat:

- mount-local retries and scopes remain per mount
- process-wide or account-wide throttle knowledge may be shared through the
  capability layer if there is evidence that Graph throttling should coordinate
  across mounts

Default rule:

- start mount-local
- share only proven global resource boundaries

### Durable State

Durable state splits into two authorities:

`Mount inventory`

- namespace and mount topology
- alias and lifecycle
- remote-root binding

`Per-mount sync store`

- baseline
- local snapshot
- remote snapshot
- observation issues
- retry work
- block scopes
- cursor and refresh cadence

### Control Plane

The control plane should start one mount engine per active mount record.

It should understand:

- standalone explicit mounts
- configured namespace roots
- managed child shortcut mounts when that projection mode is enabled

It should not need to care whether a mount came from explicit user config or
from automatic shortcut reconciliation. Both should become the same runtime
shape: a mount engine spec.

### Status, Logging, And Perf

Status and perf should be aggregatable at two levels:

- namespace-level aggregate
- per-mount detail

Hidden shortcut mounts do not need to be user-configurable to be observable.

## Blank-Slate Runtime Flows

### Startup

1. Load namespace configuration and mount inventory.
2. Reconcile actual shortcut placeholders against durable mount inventory.
3. Materialize the runtime mount set:
   - standalone explicit mounts
   - primary namespace mounts
   - zero or more child shortcut mounts
4. Build one mount-engine spec per mount.
5. Start one engine per mount.
6. Install local subtree exclusions so parent namespace mounts do not scan child
   shortcut subtrees.

### Watch Mode

The steady-state watch model is:

- namespace manager handles mount lifecycle events
- each mount engine handles content events for exactly one content root
- mount add/remove/rename triggers engine-set changes, not engine-internal root
  rewiring

For standalone mounts, there is no parent namespace manager above the content
root. The control plane simply starts that mount's engine directly.

When a shortcut appears:

- namespace manager creates or reactivates a mount record
- control plane starts a child mount engine
- parent primary engine excludes the child local subtree

When a shortcut disappears:

- namespace manager marks the mount removed
- control plane stops that child engine
- state retention/GC policy decides when to delete the per-mount store

### One-Shot Sync

One-shot sync should still operate per mount.

The namespace orchestration layer builds the current mount set and then runs one
one-shot pass per active mount. The product may render the result as one
namespace summary, but runtime authority remains per mount.

### Shortcut Root Rename

A shortcut root rename is a namespace/mount-lifecycle event.

It should:

- update mount alias/path in inventory
- move or rename the local mount root if needed
- restart or retarget the child mount engine's local subtree root

It should not:

- rename the remote folder in the owner's drive
- create planner actions inside the content engine

### Shortcut Root Removal

A shortcut root removal is an unmount event.

It should:

- stop the child mount engine
- remove or tombstone the mount record
- optionally keep per-mount store state for bounded retention/debug/undo

It should not:

- schedule remote deletes for the owner's shared contents

### Cross-Mount Moves

Cross-mount moves are boundary crossings.

They should not be treated as in-place moves. The correct semantics are one of:

- upload into target mount plus delete from source mount
- copy plus delete
- explicit rejection if product semantics require it

The blank-slate design should not attempt to model cross-mount moves as one
remote move action.

### Duplicate Shortcuts To The Same Content Root

Default blank-slate recommendation:

- one active mount per `(namespace, token-owner-account, remoteDriveID,
  remoteItemID)`

If discovery surfaces duplicates:

- inventory records a conflict state
- namespace manager does not start multiple content engines against the same
  content root inside the same namespace by default

This is the safest default. Product policy can be relaxed later if a strong use
case appears.

## Durable State Model

### Namespace / Mount Inventory Store

Recommended durable facts:

| Fact | Owner | Why |
| --- | --- | --- |
| `NamespaceID` | Mount inventory | Stable namespace identity |
| Namespace local root path | Mount inventory | Runtime bootstrap |
| `MountID` | Mount inventory | Stable lifecycle identity |
| Mount local path | Mount inventory | Local namespace ownership |
| Mount alias | Mount inventory | Recipient-owned display/path semantics |
| Mount kind | Mount inventory | Primary vs shortcut behavior |
| Token owner account | Mount inventory | Auth routing |
| Remote drive ID | Mount inventory | Content-root identity |
| Remote item ID | Mount inventory | Content-root identity |
| Placeholder identity | Mount inventory | Reconciliation against shortcut object |
| Mount lifecycle state | Mount inventory | Start/stop decisions |

### Per-Mount Sync Store

Recommended durable facts:

| Fact | Owner | Why |
| --- | --- | --- |
| Baseline | Mount engine store | Restart-safe converged truth |
| Local snapshot | Mount engine store | Planner input |
| Remote snapshot | Mount engine store | Planner input |
| Observation issues | Mount engine store | Durable current-truth problems |
| Retry work | Mount engine store | Exact delayed work |
| Block scopes | Mount engine store | Shared blockers inside one mount |
| Cursor / refresh cadence | Mount engine store | Remote resume per root |

### Schema Principle

From blank slate, every store schema should be mount-owned.

That means:

- one store per mount
- no shared cursor row across mounts
- no bare assumption that one DB equals one configured drive
- keys should model real item identity for that mount's truth domain

## Appendix A: Validation Against Current Repo And Specs

This appendix exists to keep the migration honest. It is not the source of truth
for the target architecture.

This draft is intentionally blank-slate, but the repository already has a real
architecture and real specs. Any refactor plan must distinguish:

- what is merely historical residue
- what is current intentional architecture
- what is a deliberate future product change beyond runtime cleanup

### Why The Current Code Works The Way It Does

The current shared-root behavior is not just accidental old code. Much of it now
works the way it does because the repository deliberately chose to fit shared
folders into the existing core invariants:

- one engine per runtime mount
- one store per runtime mount
- one primary remote root per engine
- one top-level `sync_dir` per configured standalone mount, with managed child
  mounts attached below a parent namespace
- control plane above engines, not inside them
- snapshot-first SQLite durability

That choice explains most of the current shared-root machinery:

- shared folders became separate configured drives rather than nested children
- the engine gained one-root-below-drive-root support via `RemoteRootItemID`
- before Increment 4, planning/execution gained target-root metadata instead of
  the engine becoming multi-root
- config/catalog/token resolution learned to treat shared folders as drive-like
  runtime units

This architecture was reinforced by later fixes, not just inherited from the
past. Recent incident work explicitly repaired:

- preserving `RemoteRootItemID` even when the backing drive ID comes from the catalog
- anchoring `mkdir` and remote path work at the configured shared root instead
  of the backing drive root
- keeping folder metadata churn from turning into fake conflict work

Those are product fixes built on the current "shared folders can be separate
configured standalone shared-folder drives" model.

### What The Specs Explicitly Say Today

Current design docs intentionally define these facts:

- shared folders can still be separate configured drives at the CLI/config edge
- the control plane builds one engine per runtime mount
- the engine is a single mounted content-root runtime owner
- the engine supports exactly two root shapes today:
  - drive-root sessions
  - mount-root sessions rooted below the remote drive root
- embedded shared-folder links discovered inside another synced drive are
  ignored
- each runtime mount owns one SQLite state DB
- mount-root planning/execution used to thread target metadata through
  ordinary actions so the pre-Increment-4 single-root engine could operate on a
  remote subtree
- shared drives currently have their own deterministic `sync_dir`, usually under
  `~/OneDrive-Shared/...`

So the current repo/spec model is not:

- one namespace root containing auto-mounted shortcut engines

It is:

- many configured drives, some of which happen to be standalone shared-folder drives

### Where The Blank-Slate Draft Aligns With Current Intent

The draft is strongly aligned with these current principles:

- keep one content engine per authoritative remote root
- keep one durable store per content engine/root
- keep the control plane above the engine
- never make embedded shortcuts inside an ordinary drive recursively spawn
  nested engine internals
- keep root-relative path anchoring explicit and correct
- keep failures isolated per root

These should be preserved.

### Where The Blank-Slate Draft Intentionally Departs

The largest intentional departure is how shortcut-derived runtimes are modeled,
not whether standalone shared-folder drives are allowed.

The first version of this draft leaned too hard toward a unified local
namespace model:

- one namespace root
- child shortcut mounts inside that namespace
- namespace manager owning mount-root lifecycle

That was too narrow. A better blank-slate target supports two projection modes
over the same runtime primitive:

- `Child mount projection`
  a shortcut appears inside an existing namespace and is managed there
- `Standalone mount projection`
  a shared folder is intentionally surfaced as its own top-level synced root

The current repo/spec model now implements both projection shapes:

- explicit configured shared folders compile into standalone runtime mounts
- discovered shortcut children compile into managed child mounts inside a
  primary local namespace

That means:

- keeping shared folders addable as separate drives remains compatible with the
  blank-slate model
- adding automatic child-mount shortcut behavior is an extra product/local-UX
  capability beyond the current repo shape

### What The Plan Must Incorporate From The Current Repo

The refactor plan should preserve these current strengths:

- the control-plane rule "one runtime unit -> one engine"
- the store rule "one runtime unit -> one durable store"
- root anchoring:
  all remote operations for subtree-backed runtimes must stay rooted at the
  configured remote root, never silently widen to the backing drive root
- embedded-shortcut ignore semantics in ordinary content observation
- target-scoped transfer/CLI helpers for rooted remote subtrees
- snapshot-first SQLite planning flow instead of introducing a second durable
  coordinator

The plan should also preserve a practical transition strategy:

- compile new mount records into synthetic current-style engine inputs at first
- reuse the current mount-root engine path as the first mount-engine
  implementation
- keep deleting transitional target-root metadata tied to ordinary sync actions
  once engines become truly mount-local

### What The Plan Must Incorporate From The Specs

The plan should explicitly acknowledge and stage around these spec-backed facts:

- explicit shared folders remain valid configured drives at the CLI/config edge
- `sync_dir` is currently per configured standalone mount, including explicit
  shared drives
- the current durable schema is per runtime mount/store and is not designed to
  hold several independent shortcut roots in one store

So the plan needs an explicit decision point about projection, not about engine
ownership:

- `Option A`: preserve standalone explicit mounts and shared-root local roots
  while introducing managed mount inventory and mount-local engine semantics
- `Option B`: add child-mount projection inside a unified namespace in addition
  to standalone explicit mounts

Both can use the same "one engine per mount/content root" runtime principle.
They differ mainly in who owns the local projection surface.

### Adjustment To The Draft Plan

The migration plan should therefore separate two concerns that the first draft
implicitly combined:

1. `Runtime and ownership refactor`
   Move from configured shared drives as the only shortcut runtime shape toward
   explicit managed mount inventory and mount-engine vocabulary, while preserving
   one-engine-per-root and one-store-per-root.

2. `Optional projection expansion`
   Decide later whether shortcut mounts should remain only standalone explicit
   roots or also be projected into a unified local namespace under a primary
   account root.

That separation keeps the refactor honest. It lets the repository improve
ownership and runtime architecture without accidentally forcing a larger product
surface change in the same increment.

## Appendix B: Mapping From Current Model To New Model

This section intentionally separates:

- `transitional reuse`
- `final target home`
- `eventual cleanup`

### Identity And Inventory Mapping

| Current concept | Current role | Transitional role | Target home | Final fate |
| --- | --- | --- | --- | --- |
| `shared:<email>:<sourceDriveID>:<sourceItemID>` canonical drive ID | Explicit standalone shared-folder identity | No managed-child role after Increment 7d | Explicit standalone drive config only | Keep only if the product still wants explicit standalone shared-folder mounts |
| `ResolvedDrive.RemoteRootItemID` | Explicit standalone shared-folder root item | Top-level CLI/config input only | `MountRecord.RemoteItemID` for managed child mounts | Keep only for explicit standalone shared-folder config |
| Shared drive `display_name` | User-facing name for configured shared drive | No managed-child role after mount-inventory schema cleanup | `MountRecord.LocalAlias` | Config display ownership stays standalone-only |
| `CatalogDrive.OwnerAccountCanonical` | Shared drive token owner | No managed-child role after mount-inventory schema cleanup | `MountRecord.TokenOwnerCanonical` | Config/catalog token owner remains standalone-only |
| `CatalogDrive.RemoteDriveID` | Backing drive ID for configured drives | Temporary content-root field | `MountRecord.RemoteDriveID` | Keep as mount metadata, not drive-config metadata |

### Runtime Mapping

| Current concept | Current role | Transitional role | Target home | Final fate |
| --- | --- | --- | --- | --- |
| `driveops.MountSession.RemoteRootItemID` | Mount-root path scoping | Mount-root input for interactive file operations | Mount-engine remote-root config | Generic drive sessions no longer carry it |
| `Engine.remoteRootItemID` | Non-drive-root remote boundary | Reusable implementation for mount-local engines | Mount engine root config | Generic mount-root input after Increment 7e |
| `RemoteRootDeltaCapable` | Mount-root observation capability branch | Capability selector for mount-root engines | Mount-engine observation capability | Keep capability logic, remove shared-root terminology |
| `FolderDeltaFetcher` for mount roots | Folder-root delta for capable mounted roots | Reusable mount-root observation path | Mount-engine remote observer | Keep as mount-root observation |
| `RecursiveLister` fallback for mount roots | Enumeration fallback | Reusable mount-root fallback | Mount-engine remote observer | Keep as mount-root observation |

### Planning And Execution Mapping

| Current concept | Current role | Transitional role | Target home | Final fate |
| --- | --- | --- | --- | --- |
| `ChangeEvent.TargetRemoteRootItemID` | Per-event shared-root propagation | Transitional support for the pre-mount-local engine | Engine-local mount context | Removed in Increment 4 when observation became mount-local |
| `Action.TargetDriveID` | Correct remote drive for shared-root actions | Transitional support for mount-root engine reuse | Maybe only boundary-crossing logic above engines | Removed from ordinary sync actions in Increment 4 |
| `Action.TargetRemoteRootItemID` | Per-action remote root override | Transitional support for mount-root engine reuse | Engine-local root context | Removed from ordinary sync actions in Increment 4 |
| `Action.TargetRootLocalPath` | Per-action local root prefix stripping | Transitional support for mount-root engine reuse | Engine-local local-root context | Removed from ordinary sync actions in Increment 4 |
| Cross-drive path convergence helpers | Makes one engine aware of non-local remote root | Transitional support only | Cross-mount orchestration or none | Remove from mount-local executor path |

### Store And State Mapping

| Current concept | Current role | Transitional role | Target home | Final fate |
| --- | --- | --- | --- | --- |
| One state DB per runtime mount | Runtime durability boundary | One DB per compiled mount spec | One DB per mount | Keep per-mount principle |
| `observation_state.content_drive_id` | Store owner drive identity | Mount-owned remote drive for the store cursor | Maybe `MountID` or another store-owner identity later | Renamed from configured-drive vocabulary in Increment 7e; re-evaluate only if a distinct durable mount identity becomes necessary |
| `remote_state.drive_id` | Per-row remote owner | Still useful within mount store | Per-row backing drive identity | Keep if still needed |
| `baseline.DriveID` | Current row-level drive identity in a per-mount DB | Still useful during transition | Optional mount-store row field | Re-evaluate after mount-local simplification |

### Current Package And File Mapping

This table maps today's major code areas to their target architectural home.

| Current area | Current role | Target owner in new model | Notes |
| --- | --- | --- | --- |
| `internal/multisync` | Starts one engine per runtime mount | Namespace/control-plane orchestration for mount specs | Strong base to keep and retarget above engines |
| `internal/config/config.go` + drive sections | User-configured namespace roots and drive-section settings | User-configured namespace roots and explicit top-level mounts only | Shortcut child mounts should stop living in ordinary drive config |
| `internal/config/catalog.go` | Managed inventory for accounts and drives | Split into drive catalog plus mount inventory, or add a distinct mount inventory boundary | Blank-slate preference is a separate mount inventory authority |
| `internal/config/drive.go` + resolver | Builds `ResolvedDrive` from config/catalog | Keep for configured namespace roots; stop using as the permanent home for shortcut mounts | Synthetic reuse may be acceptable during transition |
| `internal/driveid` shared `CanonicalID` values | Encodes explicit standalone shared-folder drives | Managed shortcut identity moved to `MountID` in Increment 7d | Configured-drive identity only | Keep only if explicit standalone shared-folder mounts remain a product surface |
| `internal/driveops/session.go` | Authenticated drive/session plus root-aware `MountSession` wrapper | Shared capability layer plus explicit interactive mount wrapper | Generic sessions are path-free; mount-root path operations live on `MountSession` |
| `internal/graph/*` shared-target resolution | Resolves raw share inputs to owner-side identity | Namespace discovery/reconciliation input | The owner-side target identity remains useful |
| `internal/sync/engine*.go` | Single mounted content-root engine | Mount engine | This is the main reusable runtime core |
| `internal/sync/item_converter.go` | Remote item normalization plus embedded-shortcut ignore rules | Mount-engine observation normalizer, plus namespace-lifecycle detection at the boundary | The ignore rule for embedded shortcuts in ordinary content observation remains correct |
| `internal/sync/planner.go` + `actions.go` | Mount-external metadata rethreaded through actions | Mount-local planner | Target-root action metadata should largely disappear |
| `internal/sync/executor.go` | Per-action execution inside one mounted content root | Mount-local executor | Cross-mount behavior should move above the engine |
| `internal/sync/store*.go` | Per-mount-drive durable state | Per-mount durable state | Store semantics stay valuable; owner vocabulary changes |
| `internal/cli/shared*.go` and direct shared commands | User-facing shared-item discovery and ad hoc shared-target operations | Optional product surface outside automatic shortcut runtime | Not required for "shortcut just works" architecture |

### Current Durable State Mapping

This table maps the major persisted artifacts to their target role.

| Current artifact | Current owner | Target owner | Target role |
| --- | --- | --- | --- |
| `config.toml` drive sections | Config | Config | Namespace roots and explicit user-owned sync settings |
| `catalog.json` account records | Catalog | Drive catalog | Account and drive identity cache |
| `catalog.json` shared-drive records | Catalog | Mount inventory or dedicated mount section | Managed shortcut mount bindings |
| Per-drive sync DB | Sync engine/store | Mount engine/store | One durable store per active mount |
| Token files | Config/token resolution | Shared capability layer | Auth input reused across mounts |

## Appendix C: Vestiges And Compensating Scaffolding Around Shared-Root Support

This is the cleanup map for code and docs that currently make shared shortcuts
look like they "fit" the existing engine more naturally than they really do.

The important distinction is not just "vestige vs not vestige". We need three
classes:

- `Preserve`
  current invariant we should keep
- `Transitional reuse`
  useful current machinery, but not in its current vocabulary/placement forever
- `Remove early`
  misleading or dead architecture residue we should delete or quarantine as soon
  as possible

Also, not everything below is a literal surviving fragment of the old
shortcut-in-parent-engine implementation. Two different things are mixed
together in the current codebase:

- `Historical vestiges`
  shapes, names, or assumptions carried forward from the older shortcut-heavy
  architecture
- `Newer compensating scaffolding`
  deliberate later additions that made the cleaned-up shared-root model work
  without redesigning the control plane and engine boundaries again

Both matter, but for different reasons:

- historical vestiges should usually be removed because they tell the wrong
  architectural story or invite regressions
- newer compensating scaffolding should be evaluated based on whether the target
  mount architecture still needs the functionality, even if it will move to a
  different owner

### Re-reading The Earlier "This Fits" Argument

An earlier architectural pass argued that shared shortcuts fit the current
architecture fairly naturally:

- config/catalog can remember them as shared drives
- drive resolution can collapse them into `DriveID + RemoteRootItemID`
- `multisync` can stay drive-shaped
- the engine can stay "drive-root vs mount-root"
- planner/executor/store were able to stay target-root-aware during transition

That argument should now be read primarily as a smell map, not as an endorsed
end-state design. Many of those "easy fits" are easy precisely because the repo
previously carried shortcut semantics deep into generic drive and engine shapes.

The main smoking guns are:

- `If shortcut support feels natural only after turning the shortcut into a
  synthetic configured drive`
  that is evidence the control plane and inventory model are too drive-centric,
  not evidence that the shortcut really belongs in generic drive config forever.

- `If generic drive resolution needs `RemoteRootItemID` to make shortcut runtimes work`
  that was evidence mount-root identity leaked into a generic drive abstraction;
  after Increment 7, `ResolvedDrive` is a CLI/config-edge input only.

- `If generic session/runtime scoping needs shared-target and shared-root
  special cases`
  that was evidence mount policy was living inside account/drive session types;
  after Increment 7f, generic sessions are path-free and root-aware path work
  uses `MountSession`.

- `If the engine only needs a small branch between drive-root and mount-root`
  that is evidence a useful rooted-engine capability exists, but not evidence
  that "shared-root drive" is the right long-term framing or owning abstraction.

- `If ordinary actions need per-action target-root overrides`
  that is a strong sign the engine is not actually rooted locally enough and is
  reconstructing mount context at action time. Increment 4 removed this
  scaffolding by making ordinary planning and execution mount-local.

- `If the store remains coherent only because each shortcut becomes its own
  synthetic drive/store pair`
  that is evidence one-store-per-root is a real invariant worth preserving, but
  also evidence "configured drive" may be standing in for the missing mount
  inventory boundary.

So the earlier writeup is still useful, but as a list of suspicious
compatibilities:

- places where shortcut support already leaked into generic drive/config/session
  concepts
- places where rooted-engine capability is real and should be preserved
- places where current plumbing exists only to compensate for the engine not yet
  being mount-local

### Triage Summary

| Current thing | Class | Why | Recommended timing |
| --- | --- | --- | --- |
| One engine per authoritative root | Preserve | This is the core simplification we want | Keep |
| One store per authoritative root | Preserve | Store/schema are intentionally singular | Keep |
| Ignoring embedded shortcuts in ordinary drive observation | Preserve | Prevents nested engine ownership from creeping back in | Keep |
| Shared-folder support via rooted-below-drive-root engines | Preserve as capability, not current framing | This is real runtime capability | Keep, rename later |
| `RemoteRootItemID`-based mount-root engine path | Transitional reuse | Useful current implementation of a rooted engine | Reframe, then simplify |
| `TargetRoot*` and ordinary `TargetDriveID` action plumbing | Removed | It existed only because the engine was not yet fully mount-local | Deleted in Increment 4 |
| Shared folders represented only as configured shared drives | Transitional reuse | Works today, but too drive-centric as the only model | Replace with mount inventory / mount specs |
| Generic session/config/catalog types carrying mount-root semantics | Transitional reuse | Shortcut/mount behavior is living in the wrong abstractions | Move after control-plane refactor |
| Historical language implying shortcut content belongs inside a parent drive engine | Remove early | This is exactly the old accidental architecture we do not want to normalize | Start immediately |
| Docs/comments/tests that present configured shared drives as the only shortcut model | Remove early | They obscure both automatic mounts and explicit standalone mounts | Start immediately |

### What We Should Remove Early

These are the items worth attacking before or alongside deeper runtime work:

- stale wording in docs/comments/tests that treats embedded shortcut-runtime
  ownership as if it still exists
- stale wording that treats "shared folders as configured drives" as the only
  natural long-term model
- misleading names and comments that describe mount-root runtime capability as if
  it were inherently a configured-shared-drive concern

Early removal does not mean deleting the working rooted-engine code. It means
stopping the repo from telling the wrong architectural story while that code is
still in use.

### Identity And Config Vestiges

`internal/driveid/canonical.go`

- Shared folders are modeled as canonical configured drives via
  `shared:<email>:<sourceDriveID>:<sourceItemID>`.
- This was useful for explicit `drive add`, but in the blank-slate model a
  shortcut is a managed mount, not a configured drive type.
- Cleanup target: move shared shortcut durable identity into mount inventory and
  remove "shared is a drive type" from the core mount-drive model.

`internal/config/drive.go`

- `ResolvedDrive.RemoteRootItemID` is now limited to explicit standalone
  shared-folder configuration at the CLI/config edge.
- Managed shortcut/mount concerns should not live inside generic drive
  resolution.
- Transitional reuse: top-level standalone mounts still start from
  `ResolvedDrive` before the control plane compiles them into mount specs.
- Cleanup target: root item identity leaves generic drive resolution anywhere
  it is not part of the explicit standalone shared-folder product surface.

`internal/config/catalog.go`, `internal/config/catalog_lifecycle.go`

- Shared-root ownership data is mixed into generic drive catalog records.
- Cleanup target: split mount inventory from drive catalog or explicitly add a
  dedicated managed-mount section with separate ownership semantics.

### Session And DriveOps Vestiges

`internal/driveops/session.go`

- Generic drive sessions no longer carry `RemoteRootItemID`; `MountSession`
  owns interactive mount-root path scoping.
- Interactive throttle scoping treats configured mount roots as a target-scoped
  shape.
- Path resolution helpers such as `resolveItemFromMountRoot` make the
  `MountSession` wrapper aware of mount-rooted operation.
- Transitional reuse: useful while child mount engines still piggyback on
  session APIs.
- Cleanup target: keep generic drive/account session separate from mount-rooted
  observers/executors. Sync mount-root scoping is an engine concern, while
  interactive file commands use the explicit `MountSession` wrapper.

### Engine Vestiges

`internal/sync/engine.go`

- `remoteRootItemID` and `remoteRootDeltaCapable` are injected into an engine
  whose published contract is still "single-mount runtime owner".
- After Increment 7e these are mount-root engine inputs, not session-derived
  shared-drive state.
- Remaining cleanup target: preserve one-root-per-engine and keep capability
  naming tied to mount-root semantics.

`internal/sync/engine_primary_root.go`

- The engine builds a single `primaryRootObservationPlan` with exactly two
  variants: drive root or mount root.
- This is still singular runtime ownership and therefore works only for one
  mounted remote root per engine.
- Cleanup target: preserve one-root-per-engine; remove mount-root-specialized
  terminology after mount-engine refactor.

`internal/sync/engine_mount_root.go`

- Entire remote observation path specialized around the engine's mount-root
  runtime path.
- This is not dead code if we keep one engine per shortcut mount.
- This is the mount-engine observation implementation for roots below the drive
  root. It remains valid for explicit standalone shared-folder mounts and
  managed child mounts.

`internal/sync/engine_primary_root_watch.go`

- Watch startup branches between drive-root and mount-root primary watches.
- The branch is now by observation capability/root kind, not by configured
  shared-drive framing.

`internal/sync/engine_config.go`

- `EngineMountConfig` carries the already-resolved `RemoteRootDeltaCapable`
  input. Standalone mounts derive it at the CLI/config edge; managed children
  derive it from `MountRecord.TokenOwnerCanonical`.

### Observation Vestiges

`internal/sync/item_converter.go`

- `RemoteRootItemID` is threaded through item conversion so events can carry the
  mounted remote root.
- The converter skips the mount root item itself.
- Embedded shared-folder items are ignored in a normal drive.
- Transitional reuse:
  - keep ignoring embedded shortcuts inside normal content observation
  - mount-root conversion can continue to back mount engines for now
- Cleanup target:
  - keep "ignore embedded shortcuts in ordinary content observation"
  - remove per-event root metadata once engine-local mount context is enough

`spec/design/sync-observation.md`

- The doc encodes the current two-shape model:
  - whole drives
  - separately configured standalone shared-folder drives
- Cleanup target: replace "configured shared-folder drives as the only model" with mount-engine
  semantics and namespace-manager lifecycle.

### Planning And Execution Vestiges

`internal/sync/actions.go`

- `TargetDriveID`, `TargetRemoteRootItemID`, and `TargetRootLocalPath` were one of
  the strongest signals that shortcut support had been pushed through a
  single mounted content-root engine rather than moving the boundary above it.
- Increment 4 removed those ordinary action fields and made planning plus
  execution consume engine-owned mount context directly.

`internal/sync/planner.go`

- `enrichActionTargets` reconstructed target-root data from baseline/remote
  state after planning.
- Historical transitional reuse: it supported pre-Increment-4 shared-root
  engines.
- Removed in Increment 4 for ordinary mount-local planning.

`internal/sync/executor.go`

- `ExecutorConfig.remoteRootItemID` makes create, move, upload, and delete
  convergence relative to the engine's configured mount root.
- Cleanup target: keep ordinary executor path convergence mount-local.
  Cross-mount operations should be decomposed above the engine boundary.

`spec/design/sync-planning.md`, `spec/design/sync-execution.md`

- Planning and execution docs now describe mount-local engine context rather
  than per-action shared-root target metadata.
- Cleanup target: keep boundary metadata restricted to explicit cross-mount
  orchestration if that product surface is added later.

### Store And Schema Vestiges

`internal/sync/schema.go`

- The schema now assumes one store per runtime mount.
- `observation_state` has one `content_drive_id` owner row.
- `baseline` and `remote_state` are keyed by bare `item_id`, which is a strong
  hint that one DB is not intended to hold multiple independent remote authority
  sets.
- This is the decisive reason not to push multiple shortcut roots into one
  current engine/store pair.

`internal/sync/store_observation_state.go`

- Owner vocabulary is mount-owned and remote-drive-specific.
- Future cleanup target: re-evaluate whether the store should persist a
  distinct `MountID` only if the remote drive ID stops being sufficient for the
  store cursor owner.

`internal/sync/store_write_observation.go`, `internal/sync/store_write_baseline.go`,
`internal/sync/sqlite_compare.go`, `internal/sync/core_types.go`

- All of these still assume one primary content boundary per store and compare
  by bare `item_id`.
- Cleanup target: keep one-store-per-mount and remove any remaining reason to
  pretend one store might directly own several shortcut roots at once.

### Control Plane Vestiges

`internal/multisync`

- The current control plane already has the right high-level runtime shape:
  one engine per runtime mount.
- The previous transition seam where multisync consumed `ResolvedDrive` directly
  is gone: the CLI now compiles configured drives into `StandaloneMountConfig`
  before multisync builds runtime mount specs.
- Cleanup target: keep moving durable child-shortcut authority from
  drive-shaped config/catalog vocabulary into explicit mount specs and mount
  inventory where those concepts own the truth.

### Docs, Tests, And Historical Vocabulary Vestiges

The following docs encode the current shortcut-as-shared-drive vocabulary and
will need cleanup once the new model lands:

- `spec/design/drive-identity.md`
- `spec/design/config.md`
- `spec/design/sync-observation.md`
- `spec/design/sync-engine.md`
- `spec/design/sync-planning.md`
- `spec/design/sync-execution.md`
- `spec/design/data-model.md`
- `spec/design/sync-store.md`
- `spec/reference/onedrive-glossary.md`
- `spec/reference/graph-api-quirks.md`
- `spec/reference/live-incidents.md`

Not every reference should disappear immediately. Some should become explicitly
"historical transitional behavior" notes until the refactor is complete.

## Appendix D: Proposed Refactor Strategy

This section describes how to move toward the target model over several
increments without pretending we can replace everything at once.

### Recommended Migration Route

The cleanest route from the current repo to the target architecture is:

1. `Keep the current product surface stable first`
   Keep explicit standalone shared-folder drives working while we refactor the
   runtime model underneath them.
2. `Introduce mount-shaped runtime seams before changing behavior`
   Make the control plane and engine construction mount-shaped before automatic
   shortcut runtime exists.
3. `Delete engine-local compensating plumbing before adding automatic mounts`
   Make the existing rooted engine genuinely mount-local while it still serves
   today's standalone shared-drive surface.
4. `Add child-projection infrastructure before automatic shortcut sync`
   Make the namespace/lifecycle boundary real before automatic shortcut runtime
   becomes user-facing.
5. `Add automatic shortcut sync as child projection`
   Make "shortcut just works" land inside the parent drive's `sync_dir`, at the
   shortcut location, instead of as a separate long-term synced root.

This route deliberately prefers `Option A` first:

- explicit standalone mounts remain supported
- automatic shortcut sync targets child projection as the real product behavior
- temporary separate-root fallback remains an internal migration option only, not
  the intended user-facing steady state

That sequencing removes the largest architectural debt without dragging in the
hardest local-namespace ownership problem at the same time.

### Increment 0: Remove Early Vestiges And Lock In The Right Story

Goal:

- stop the repo from telling the old shortcut-in-parent-engine story

Code areas:

- design docs under `spec/design/`
- reference docs under `spec/reference/`
- package comments and tests referencing shared-root/shared-drive semantics

Concrete work:

- rewrite docs/comments/tests that still imply embedded shortcut content belongs
  inside a parent drive engine
- rewrite docs/comments/tests that present configured shared drives as the only
  natural long-term shortcut model
- explicitly classify current shared-root code as:
  - preserved invariant
  - transitional reuse
  - remove-early residue

Must not do:

- no behavior changes
- no new runtime types yet

Exit criteria:

- repo guidance consistently says:
  - one engine per authoritative root
  - embedded shortcuts inside normal drives are ignored
  - explicit standalone shared-folder mounts remain valid
  - child-mount projection is optional future work, not assumed current behavior

### Increment 1: Introduce Runtime Mount Specs Above Configured Drives

Goal:

- create a runtime unit above the engine that is no longer synonymous with a
  configured drive

Code areas:

- `internal/multisync/`
- a new mount-spec boundary, likely under `internal/multisync/` first or a new
  small package if that boundary proves durable
- docs: `spec/design/sync-control-plane.md`, this draft

Concrete work:

- introduce a `MountSpec`-shaped runtime type with fields such as:
  - `MountID`
  - `ProjectionKind`
  - `DisplayName`
  - `SyncRoot`
  - `StatePath`
  - `RemoteDriveID`
  - `RemoteRootItemID`
  - `OwnerAccountCanonical`
  - rooted-observation capability hints
- add a builder that compiles current `ResolvedDrive` entries into `MountSpec`
  values
- change orchestrator internals to operate on mount specs instead of directly on
  `*config.ResolvedDrive`, even if the public inputs are still resolved drives
- keep reporting/startup selection stable by carrying the current selection index
  through the new mount-spec layer

Must not do:

- no automatic shortcut discovery yet
- no engine semantic change yet
- do not introduce mount inventory yet

Tests:

- builder tests for `ResolvedDrive -> MountSpec`
- orchestrator tests proving one engine still starts per current configured drive
- startup/reporting tests proving selection order and pause behavior stay stable

Exit criteria:

- orchestrator can be described as "one engine per mount spec"
- current configured drives are only one source of mount specs, not the runtime
  model itself

### Increment 2: Construct Engines From Mount Specs, Not Resolved Drives

Goal:

- move generic config/catalog knowledge out of engine construction

Code areas:

- `internal/sync/engine_config.go`
- `internal/sync/engine.go`
- `internal/multisync/orchestrator.go`
- possibly a thin adapter layer for current `ResolvedDrive` callers
- docs: `spec/design/sync-engine.md`, `spec/design/sync-control-plane.md`

Concrete work:

- replace resolved-drive engine construction as the primary orchestration
  boundary with a mount-engine constructor fed by:
  - authenticated clients/session capabilities
  - a `MountSpec`
- stop loading catalog/config details inside engine construction to rediscover
  rooted behavior
- make `MountSpec`, not `ResolvedDrive`, the owner of:
  - state DB path
  - sync root
  - remote root item ID
  - rooted-observation capability hints
- remove any thin compatibility adapter once CLI/control-plane entrypoints can
  compile explicit mount identity before engine construction

Must not do:

- no automatic mounts yet
- no planner/executor simplification yet

Tests:

- constructor tests proving engine inputs match previous behavior for ordinary
  drives and current standalone shared-folder drives
- orchestrator tests proving old and new construction paths are behaviorally
  equivalent

Exit criteria:

- engine construction no longer depends on generic drive resolution as its
  permanent source of truth

### Increment 3: Rename Shared-Root Runtime Capability Into Generic Rooted-Mount Capability

Goal:

- preserve the useful mount-root engine capability while deleting the
  misleading configured-shared-drive framing around it

Code areas:

- `internal/sync/engine_primary_root.go`
- `internal/sync/engine_mount_root.go`
- `internal/sync/engine_primary_root_watch.go`
- `internal/sync/engine_config.go`
- `internal/sync/item_converter.go`
- docs: `spec/design/sync-engine.md`, `spec/design/sync-observation.md`

Concrete work:

- rename "shared root" engine terminology toward neutral mount-root terminology
- rename helpers such as:
  - `hasMountRoot()`
  - `RemoteRootDeltaCapable`
  - `observeMountRootRemote()`
  when their real semantics are "engine rooted below drive root"
- keep capability branching by source-drive type where Graph behavior actually
  differs, but stop tying that branching to the concept "configured shared drive"
- keep embedded shortcut ignore semantics in ordinary observation

Must not do:

- do not delete the mount-root observation path itself
- do not yet remove `RemoteRootItemID`-style fields if they are still needed by the
  current constructor

Tests:

- rename-preserving behavior tests for mount-root observation
- watch tests proving mount-root runtimes still poll correctly

Exit criteria:

- the engine no longer claims to have a special "shared-drive" mode internally;
  it has a rooted-mount capability

### Increment 4: Make Observation, Planning, Execution, And Scope Logic Mount-Local [completed]

Goal:

- stop re-threading mount-root identity through observation results and ordinary
  planner state

Code areas:

- `internal/sync/item_converter.go`
- `internal/sync/core_types.go`
- `internal/sync/actions.go`
- `internal/sync/planner.go`
- `internal/sync/engine_result_classify.go`
- docs: `spec/design/sync-observation.md`, `spec/design/sync-planning.md`

Concrete work:

- removed `TargetRemoteRootItemID` from ordinary change-event/current-view
  propagation
- removed `enrichActionTargets()` and the action-level root rediscovery helpers
- made planner decisions relative to the engine's mounted local root and
  mounted remote root
- removed ordinary `TargetDriveID`, `TargetRemoteRootItemID`, and
  `TargetRootLocalPath` from sync actions and results
- made executor path convergence and scope routing mount-local
- removed `driveops.PathConvergenceFactory` and `Session.ForTarget(...)` from
  ordinary sync execution

Must not do:

- do not widen the engine to multiple roots
- do not try to solve child-mount projection here

Tests:

- planner golden tests for ordinary standalone shared-folder drives before/after
  the change
- regression tests proving ordinary mount-root actions bind to concrete
  `DriveID` without target-root metadata
- executor and scope tests proving mount-root execution/path convergence are
  mount-local

Exit criteria:

- ordinary planner output is mount-local
- ordinary engine execution, path convergence, and scope routing are mount-local
- direct shared-item CLI paths still keep explicit target scoping above sync

### Increment 5: Durable Child-Mount Inventory + Child-Projection Seams [completed]

Goal:

- add a durable authority for managed child shortcut mounts and make the sync
  runtime consume mount-owned identity directly

Code areas:

- `internal/config/` for `mounts.json`, validation, and mount state-path helpers
- `internal/driveops/` for mount-shaped sync session construction
- `internal/multisync/` for merged parent + child mount compilation
- `internal/sync/` local observation filters for child subtree exclusions
- docs: `spec/design/config.md`, `spec/design/sync-control-plane.md`,
  `spec/design/drive-transfers.md`, `spec/design/sync-observation.md`

Concrete work:

- landed separate `mounts.json` inventory instead of extending `catalog.json`
- defined child-focused `MountRecord` durable state with validation for:
  - stable mount ID
  - parent mount ID
  - relative local path
  - remote content-root identity
- added `MountStatePath(...)` so managed child mounts get their own retained
  state DBs
- removed `mountSpec.resolved` and promoted sync/runtime identity into
  mount-owned fields
- changed `SessionRuntime.SyncSession(...)` to accept mount-owned identity
  instead of `*config.ResolvedDrive`; Increment 7a later generalized that
  identity as `driveops.MountSessionConfig` for both sync and interactive
  sessions
- taught the control plane to merge:
  - configured standalone parent mounts
  - managed child-mount records
- installed exact child subtree exclusions on parent mounts through
  `LocalFilterConfig.SkipDirs`
- defined duplicate projection policy:
  - explicit standalone mount wins over conflicting child projection for the
    same content root
  - conflicting child projections are skipped before engine startup

Must not do:

- no automatic shortcut reconciliation yet
- no placeholder/tombstone lifecycle yet
- no automatic user-facing shortcut sync rollout yet

Tests:

- inventory load/save/validation tests
- mount-spec merge and conflict tests
- state-path tests proving managed mounts get stable durable store paths
- local observation tests proving parent mounts skip exact child subtrees

Exit criteria:

- managed child mounts can exist durably without pretending to be ordinary
  configured drives
- sync session creation is mount-shaped
- parent mounts exclude child subtrees without reintroducing nested engines

### Increment 6: Add Automatic Shortcut Runtime As Child Projections [completed]

Goal:

- make "user added a shortcut and it just works" land with the actual intended
  product surface

Code areas:

- shared shortcut discovery/reconciliation boundary above the engine
- managed mount inventory lifecycle
- namespace manager / mount-spec builder
- docs: this draft, `spec/design/sync-control-plane.md`,
  `spec/design/drive-identity.md`, `spec/design/config.md`

Concrete work:

- reconciler now runs in `internal/multisync` before one-shot startup, before
  watch startup, on control-socket reload, and on the watch reconcile ticker
- discovered shortcut placeholders now create/update/remove managed child mount
  records automatically in `mounts.json`
- authoritative removal now requires a completed delta pass to a terminal
  delta token; recursive `children` enumeration remains positive-only
- runtime status/control/perf surfaces are now mount-shaped end to end:
  - `MountRunner`, `MountReport`, and mount startup results in `multisync`
    carry `MountIdentity`
  - `StatusResponse.Mounts` and `PerfStatusResponse.Mounts`
  - per-mount perf collectors keyed by mount ID
  - `status` child rows rendered beneath the selected parent drive rows, with
    child rows identified by `mount_id` rather than synthetic `shared:`
    canonical IDs
- `driveops.MountSession` now owns mount-root path terminology
- keep explicit standalone shared-folder drive add support working in parallel
- if a temporary separate-root fallback is used during rollout:
  - keep it clearly labeled migration-only
  - do not make it the default user-facing behavior
  - plan its removal as follow-up work, not as optional cleanup

Must not do:

- do not let discovery misses immediately destroy managed mount state; require
  bounded reconciliation rules
- do not ship a steady-state UX where automatic shortcuts appear as separate
  synced roots outside the parent drive tree

Tests:

- delta full enumeration updates a binding in place without changing `MountID`
- authoritative delta removal removes absent bindings and returns removed mount IDs
- `410 Gone` resets parent discovery state without removing existing child mounts
- recursive listing adds/updates child bindings but does not remove on absence
- shortcut appears inside the parent drive `sync_dir` at the shortcut path
- duplicate explicit + child projection detection

Exit criteria:

- automatic shortcut sync works without new CLI configuration
- explicit standalone shared-folder mounts still work
- automatic shortcuts do not create their own user-facing synced roots in steady
  state

### Increment 7: Remove Shared-Drive-Only Coupling And Finish Terminology Cleanup [completed]

Status: sub-slices 7a through 7f have removed resolved-drive runtime
construction, moved multisync to standalone mount inputs, applied PR-feedback
hardening, moved managed child identity to `MountIdentity`, and split generic
sessions from mount-root path scoping while renaming sync-store and engine
runtime owner vocabulary. Increment 7f made generic sessions path-free,
promoted `mounts.json` namespace vocabulary, persisted child
lifecycle/conflict state, and moved namespace inventory mutation behind an
unexported multisync owner. Increment 8 then made unavailable shortcut
bindings producer-owned durable lifecycle state, added schema v4
`reserved_local_paths` for shortcut projection moves, and surfaced child
lifecycle reasons in status/startup reporting. Increment 8b hardened that
model by retrying unavailable shortcut bindings on every reconciliation cycle,
making child projections fully transparent to user controls, rejecting
symlinked projection ancestors, handling case-only projection renames, avoiding
empty-root creation when both old and new projection paths are missing,
recompiling after lifecycle mutations so parent skip dirs are current in the
same run, and limiting inventory-save failures to dirty child records.

Goal:

- stop using configured shared drives as the conceptual center of shared-folder
  runtime ownership

Code areas:

- `internal/driveid/`
- `internal/config/drive.go`
- `internal/config/catalog.go`
- `internal/driveops/session.go`
- `internal/sync/*`
- all governed docs and repo guidance

Concrete work:

- remove or narrow `driveid.DriveTypeShared` wherever it still stands in for a
  managed mount identity rather than an explicit standalone mount surface
- remove generic config/catalog/session fields that only existed to carry mount
  semantics in drive-shaped types
- remove transitional resolved-drive runtime constructors and session APIs:
  the resolved-drive engine constructor and resolved-drive session entrypoint
  are gone, and both interactive and sync session construction now consume
  `driveops.MountSessionConfig`
- remove resolved-drive values from multisync orchestration:
  `multisync.OrchestratorConfig` now accepts `StandaloneMountConfig`, and
  `ResolvedDrive` is consumed at the CLI/config edge before top-level mount
  construction
- harden the mount boundary after review:
  `StandaloneMountSelection` carries valid top-level mounts plus mount-local
  conversion failures, watch startup now closes the control socket when startup
  validation fails after bind, child lifecycle paths still reserve their parent
  subtree while they are unavailable, conflicted, or pending removal, and
  managed mount state paths use bounded collision-resistant digest
  filenames
- make `mounts.json` the durable namespace/mount authority:
  schema v4 uses `namespaces`, `namespace_id`, `local_alias`, `remote_item_id`,
  `token_owner_canonical`, and explicit `MountState` lifecycle values instead
  of parent-drive vocabulary; `reserved_local_paths` preserves parent subtree
  ownership while shortcut rename/move local projection is still settling
- move child namespace reconciliation into an unexported multisync namespace
  runtime owner that loads/saves inventory, reconciles shortcut placeholders,
  computes conflicts/removals, and returns runtime inputs to the orchestrator
- persist lifecycle decisions for managed child mounts:
  duplicate child projections and explicit-standalone conflicts become durable
  `conflict` records, authoritative removals become `pending_removal` until the
  runner is stopped and the child state DB is purged, and item-level shortcut
  target materialization failures become `unavailable` records instead of sync
  store retry/block/observation rows
- move managed child runtime/report/status identity off fabricated `shared:`
  drive IDs:
  `MountIdentity` keeps canonical IDs for standalone mounts while managed child
  mounts report by durable `MountID`
- rename remaining store owner vocabulary from configured-drive-centric to
  mount-owned terminology: `observation_state.content_drive_id`,
  `ObservationState.ContentDriveID`, and related helpers now describe the mounted
  content root rather than a configured-drive runtime owner
- keep only the minimal explicit standalone-mount surface the product still
  wants

Must not do:

- do not remove explicit standalone shared-folder mount support if the product
  still wants that surface

Tests:

- deleted-name sweep tests / repo checks
- drive add / managed mount / optional child mount regression coverage
- schema v4 load/save/validation, namespace conflict, unavailable,
  pending-removal, projection-move, and parent-subtree reservation coverage

Exit criteria:

- the runtime architecture is mount-based end to end
- any remaining configured shared-drive surface is a deliberate product choice,
  not the architectural center of the implementation
- generic sessions have no path helper API; root-aware operations use
  `MountSession`
- managed child lifecycle, conflict, unavailable, and projection-move decisions
  are durable inventory state

## Temporary Coexistence Rules During Refactor

These rules are here to prevent another round of accidental architecture during
the migration.

- Do not add new user-facing shortcut configuration surface unless it is needed
  for a concrete transition step.
- Do not push new shortcut lifecycle semantics down into the content engine.
- Do not allow one local subtree to be owned by two engines.
- Do not let per-mount cursor ownership drift into shared namespace state.
- Do not widen one store to multiple independent shortcut roots as a shortcut to
  avoid control-plane work.
- If a transition step temporarily reuses shared-root drive machinery, document
  exactly which pieces are transitional and which will be deleted.

## Verification Strategy

Each increment should prove both architecture and behavior.

Architecture checks:

- one clear owner for mount inventory
- one clear owner for mount-root lifecycle
- one engine per mount
- no overlapping local subtree ownership
- no new per-action root metadata without an explicit boundary reason

Behavior checks:

- shortcut appears -> child mount engine starts
- shortcut removed -> child mount engine stops without deleting owner content
- shortcut alias rename -> local mount path updates without remote rename
- parent primary engine ignores child mount subtree
- child mount cursor and retry state survive restart independently
- failure in one shortcut mount does not stop unrelated mounts

## Open Questions

- Should mount inventory live in a new managed file/store or extend the catalog?
  Blank-slate preference: separate durable authority.
- Should the namespace manager watch only top-level shortcut placeholders or the
  whole namespace root for mount-lifecycle events?
- What is the retention/GC policy for per-mount state after shortcut removal?
- Process-wide transfer semaphore: deferred until profiling shows pressure.
- Duplicate-shortcut policy: one active projection per namespace/content root;
  explicit standalone mounts win over automatic child mounts, and duplicate
  child shortcuts keep the deterministic first path as active while later
  aliases are durable `conflict` records.

## Summary

The correct blank-slate architecture is not "teach one drive engine to sync
several physical drives". It is:

- namespace manager for topology and mount lifecycle
- durable mount inventory for shortcut bindings
- one mount engine per content root
- shared capability layer for reusable process-wide resources

The current repository already contains reusable pieces of a mount engine in the
mount-root observation and execution path. Those pieces should be harvested
deliberately as transitional implementation material, not treated as proof that
shared shortcuts naturally belong inside a single mounted content-root engine
model.
