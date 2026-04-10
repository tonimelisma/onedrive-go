// Package synccontrol owns the JSON-over-HTTP Unix-socket protocol shared by
// the CLI client and the multisync owner. It deliberately does not own socket
// lifecycle, transport, or sync-store mutation.
package synccontrol

import (
	"net/url"
	"path"
)

const (
	HTTPBaseURL = "http://unix"

	PathStatus = "/v1/status"
	PathReload = "/v1/reload"
	PathStop   = "/v1/stop"
)

type OwnerMode string

const (
	OwnerModeOneShot OwnerMode = "oneshot"
	OwnerModeWatch   OwnerMode = "watch"
)

type Status string

const (
	StatusOK       Status = "ok"
	StatusError    Status = "error"
	StatusReloaded Status = "reloaded"
	StatusStopping Status = "stopping"
	StatusApproved Status = "approved"
	StatusQueued   Status = "queued"

	StatusAlreadyQueued     Status = "already_queued"
	StatusAlreadyResolving  Status = "already_resolving"
	StatusAlreadyResolved   Status = "already_resolved"
	StatusDifferentStrategy Status = "different_strategy"
)

type ErrorCode string

const (
	ErrorInvalidRequest            ErrorCode = "invalid_request"
	ErrorForegroundSyncRunning     ErrorCode = "foreground_sync_running"
	ErrorDriveNotManaged           ErrorCode = "drive_not_managed"
	ErrorConflictNotFound          ErrorCode = "conflict_not_found"
	ErrorInvalidResolution         ErrorCode = "invalid_resolution"
	ErrorConflictDifferentStrategy ErrorCode = "conflict_different_strategy"
	ErrorConflictAlreadyResolving  ErrorCode = "conflict_already_resolving"
	ErrorStoreOpenFailed           ErrorCode = "store_open_failed"
	ErrorInternal                  ErrorCode = "internal_error"
)

type StatusResponse struct {
	OwnerMode OwnerMode `json:"owner_mode"`
	Drives    []string  `json:"drives"`

	PendingHeldDeleteApprovals int `json:"pending_held_delete_approvals,omitempty"`
	PendingConflictRequests    int `json:"pending_conflict_requests,omitempty"`
	ResolvingConflictRequests  int `json:"resolving_conflict_requests,omitempty"`
	FailedConflictRequests     int `json:"failed_conflict_requests,omitempty"`
}

type MutationResponse struct {
	Status  Status    `json:"status"`
	Code    ErrorCode `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

type ConflictResolutionRequest struct {
	Resolution string `json:"resolution"`
}

func HeldDeletesApprovePath(canonicalID string) string {
	return path.Join("/v1/drives", url.PathEscape(canonicalID), "held-deletes/approve")
}

func ConflictResolutionRequestPath(canonicalID, conflictID string) string {
	return path.Join("/v1/drives", url.PathEscape(canonicalID), "conflicts", url.PathEscape(conflictID), "resolution-request")
}
