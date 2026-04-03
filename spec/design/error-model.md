# Error Model

Implements: R-6.8.16 [verified]

## Overview

The repository uses one domain error model across configuration, Graph I/O,
sync runtime, durable persistence, and CLI presentation. Each boundary owns
one translation step from raw errors into this shared model. Higher layers may
add context, but they do not invent a second classification scheme.

## Canonical Classes

| Class | Meaning | Automatic Follow-Up |
|------|---------|---------------------|
| `success` | The operation completed and durable/runtime state should advance normally. | Commit success, clear stale transient state. |
| `shutdown` | Work stopped because the caller canceled or the process is shutting down. | Stop cleanly; do not invent retry or actionable rows purely because the process is exiting. |
| `retryable transient` | A specific item failed for a condition that is expected to clear without human action. | Persist `sync_failures` row with `category='transient'`, `failure_role='item'`, and `next_retry_at`. |
| `scope-blocking transient` | A wider transient condition makes a whole scope unsafe to keep dispatching. | Persist `scope_blocks` plus held/boundary failure rows and recover through trial actions. |
| `actionable` | Automatic retry is not appropriate; the user must fix content, permissions, or configuration. | Persist/display actionable failure with reason and user action. |
| `fatal` | The current command or drive runtime cannot continue safely. | Abort the current flow and return an error immediately. |

## Translation Ownership

Each boundary owns exactly one translation step:

- `graph`: normalize wire/auth/API failures into `GraphError` plus sentinels such as `ErrGone`, `ErrUnauthorized`, `ErrNotFound`, and `ErrThrottled`.
- `config`: normalize parse, validation, and discovery outcomes into fatal load errors or lenient warnings.
- `sync`: normalize `WorkerResult`, observer sentinels, and permission checks into `ResultDecision`, scope actions, retry scheduling, and success cleanup.
- `syncstore`: persist the engine's classification using `category`, `failure_role`, `scope_key`, and `next_retry_at`; it never reclassifies raw transport failures.
- `cli`: map fatal/actionable/transient outcomes into command exit errors and user-facing reason/action text.

## Persistence Mapping

The durable projection of the error model is intentionally small:

- `retryable transient` -> `sync_failures.category='transient'`, `failure_role='item'`
- `scope-blocking transient` -> `scope_blocks` row plus `sync_failures.failure_role='held'` and `'boundary'`
- `actionable` -> `sync_failures.category='actionable'`
- `success` -> baseline/remote-state commit plus explicit failure cleanup where required
- `shutdown` and `fatal` -> returned to the caller unless a higher boundary intentionally converts them into one of the durable classes above

This keeps durable state as a record of policy decisions, not a copy of every
raw error string seen in the process.

## Boundary Rules

- Errors cross one classification boundary before being wrapped with local context.
- The boundary that understands the invariant owns the classification.
- Retry/backoff consumes the classified result; it does not classify on its own.
- User-facing messaging consumes the classified result; it does not inspect raw HTTP or filesystem payloads directly.
