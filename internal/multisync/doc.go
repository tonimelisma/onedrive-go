// Package multisync implements the multi-mount sync control plane: runtime
// mount-set construction, parent-published child runner reconciliation,
// per-mount panic isolation, and watch-mode config reload.
//
// Shortcut invariants: parent shortcut truth lives in internal/sync; this
// package owns only runtime runners, exact parent snapshots for the active run,
// child artifact cleanup execution, transient cleanup diagnostics, and startup
// reporting. One-shot and watch runtimes own their snapshot caches, so child
// work cannot survive into a later run unless a live parent republishes it.
package multisync
