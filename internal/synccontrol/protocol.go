// Package synccontrol owns the JSON-over-HTTP Unix-socket protocol shared by
// the CLI client and the multisync owner. It deliberately does not own socket
// lifecycle, transport, or sync-store mutation.
package synccontrol

import (
	"net/url"
	"path"

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

const (
	HTTPBaseURL = "http://unix"

	PathStatus      = "/v1/status"
	PathReload      = "/v1/reload"
	PathStop        = "/v1/stop"
	PathPerfStatus  = "/v1/perf"
	PathPerfCapture = "/v1/perf/capture"
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

	StatusAlreadyQueued   Status = "already_queued"
	StatusAlreadyApplying Status = "already_applying"
	StatusAlreadyResolved Status = "already_resolved"
)

type ErrorCode string

const (
	ErrorInvalidRequest          ErrorCode = "invalid_request"
	ErrorForegroundSyncRunning   ErrorCode = "foreground_sync_running"
	ErrorDriveNotManaged         ErrorCode = "drive_not_managed"
	ErrorConflictNotFound        ErrorCode = "conflict_not_found"
	ErrorInvalidResolution       ErrorCode = "invalid_resolution"
	ErrorConflictAlreadyApplying ErrorCode = "conflict_already_applying"
	ErrorStoreOpenFailed         ErrorCode = "store_open_failed"
	ErrorInternal                ErrorCode = "internal_error"
	ErrorCaptureUnavailable      ErrorCode = "capture_unavailable"
	ErrorCaptureInProgress       ErrorCode = "capture_in_progress"
)

type StatusResponse struct {
	OwnerMode OwnerMode `json:"owner_mode"`
	Drives    []string  `json:"drives"`

	// Durable-intent counters are watch-owner diagnostics. One-shot owners
	// expose the same status shape for owner-lock visibility, but they do not
	// own a long-lived intent loop and must leave these counters zero/omitted.
	PendingHeldDeleteApprovals int `json:"pending_held_delete_approvals,omitempty"`
	PendingConflictRequests    int `json:"pending_conflict_requests,omitempty"`
	ApplyingConflictRequests   int `json:"applying_conflict_requests,omitempty"`
}

type MutationResponse struct {
	Status  Status    `json:"status"`
	Code    ErrorCode `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

type PerfStatusResponse struct {
	OwnerMode OwnerMode                `json:"owner_mode"`
	Aggregate perf.Snapshot            `json:"aggregate"`
	Drives    map[string]perf.Snapshot `json:"drives,omitempty"`
}

type PerfCaptureRequest struct {
	DurationMS int64  `json:"duration_ms"`
	OutputDir  string `json:"output_dir,omitempty"`
	Trace      bool   `json:"trace,omitempty"`
	FullDetail bool   `json:"full_detail,omitempty"`
}

type PerfCaptureResponse struct {
	OwnerMode OwnerMode          `json:"owner_mode"`
	Result    perf.CaptureResult `json:"result"`
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
