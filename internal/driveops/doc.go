// Package driveops provides authenticated drive access, token caching, Graph
// HTTP runtime reuse, and transfer operations. It is the single owner of the
// "resolved drive -> authenticated Graph API client" glue logic, shared
// between the CLI (file-op commands), the multi-drive control plane, and the
// sync engine.
//
// SessionRuntime caches TokenSources by token file path and owns the reused
// bootstrap, interactive, and sync HTTP client profiles. That keeps one
// concrete runtime owner for both auth/session construction and target-scoped
// Graph transport reuse.
// Session wraps a pair of graph.Client instances (metadata + transfer) with
// convenience methods for path resolution and child listing.
//
// TransferManager and SessionStore provide download/upload with resume,
// hash verification, and upload session persistence.
package driveops
