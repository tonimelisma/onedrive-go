// Package driveops provides authenticated drive access, token caching, and
// transfer operations. It is the single owner of the "resolved drive →
// authenticated Graph API client" glue logic, shared between the CLI (file-op
// commands) and the sync engine (Orchestrator).
//
// SessionProvider caches TokenSources by token file path, preventing OAuth2
// refresh token rotation races when multiple drives share a token path.
// Session wraps a pair of graph.Client instances (metadata + transfer) with
// convenience methods for path resolution and child listing.
//
// TransferManager and SessionStore provide download/upload with resume,
// hash verification, and upload session persistence.
package driveops
