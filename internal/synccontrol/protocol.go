// Package synccontrol owns the JSON-over-HTTP Unix-socket protocol shared by
// the CLI client and the multisync owner. It deliberately does not own socket
// lifecycle, transport, or sync-store mutation.
package synccontrol

import "github.com/tonimelisma/onedrive-go/internal/perf"

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
)

type ErrorCode string

const (
	ErrorInvalidRequest        ErrorCode = "invalid_request"
	ErrorForegroundSyncRunning ErrorCode = "foreground_sync_running"
	ErrorInternal              ErrorCode = "internal_error"
	ErrorCaptureUnavailable    ErrorCode = "capture_unavailable"
	ErrorCaptureInProgress     ErrorCode = "capture_in_progress"
)

type StatusResponse struct {
	OwnerMode               OwnerMode                   `json:"owner_mode"`
	Mounts                  []string                    `json:"mounts"`
	ShortcutCleanupFailures []ShortcutCleanupDiagnostic `json:"shortcut_cleanup_failures,omitempty"`
}

type ShortcutCleanupDiagnostic struct {
	Source  string `json:"source"`
	Class   string `json:"class"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
}

type MutationResponse struct {
	Status  Status    `json:"status"`
	Code    ErrorCode `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

type PerfStatusResponse struct {
	OwnerMode OwnerMode                `json:"owner_mode"`
	Aggregate perf.Snapshot            `json:"aggregate"`
	Mounts    map[string]perf.Snapshot `json:"mounts,omitempty"`
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
