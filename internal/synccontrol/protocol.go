// Package synccontrol owns the JSON-over-HTTP Unix-socket protocol shared by
// the CLI client and the multisync owner. It deliberately does not own socket
// lifecycle, transport, or sync-store mutation.
package synccontrol

import (
	"encoding/json"
	"fmt"
	"path/filepath"

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
)

type ErrorCode string

const (
	ErrorInvalidRequest        ErrorCode = "invalid_request"
	ErrorForegroundSyncRunning ErrorCode = "foreground_sync_running"
	ErrorInternal              ErrorCode = "internal_error"
	ErrorCaptureUnavailable    ErrorCode = "capture_unavailable"
	ErrorCaptureInProgress     ErrorCode = "capture_in_progress"
	// ErrorReloadFailed means the watch owner could not apply the config on
	// disk and kept running with its previous settings.
	ErrorReloadFailed ErrorCode = "reload_failed"
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

// PerfCaptureRequest asks the sync owner for a capture bundle. OutputDir must
// reach the owner as an absolute path: the owner is a different process,
// usually started in another directory, so a relative path would land wherever
// the daemon happens to run instead of where the user typed it.
type PerfCaptureRequest struct {
	DurationMS int64  `json:"duration_ms"`
	OutputDir  string `json:"output_dir,omitempty"`
	Trace      bool   `json:"trace,omitempty"`
	FullDetail bool   `json:"full_detail,omitempty"`
}

// MarshalJSON encodes the request with OutputDir resolved against the
// encoding process's working directory. Encoding happens in the client that
// sends the request, so this is where a relative --output still means what
// the user meant; the owner rejects any relative path that gets past it.
func (r PerfCaptureRequest) MarshalJSON() ([]byte, error) {
	type wireRequest PerfCaptureRequest

	wire := wireRequest(r)
	if wire.OutputDir != "" && !filepath.IsAbs(wire.OutputDir) {
		absolute, err := filepath.Abs(wire.OutputDir)
		if err != nil {
			return nil, fmt.Errorf("resolve capture output directory %s: %w", wire.OutputDir, err)
		}
		wire.OutputDir = absolute
	}

	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode perf capture request: %w", err)
	}

	return encoded, nil
}

type PerfCaptureResponse struct {
	OwnerMode OwnerMode          `json:"owner_mode"`
	Result    perf.CaptureResult `json:"result"`
}
