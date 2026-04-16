package sync

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

func (flow *engineFlow) resultLogFields(decision *ResultDecision, r *WorkerResult) []any {
	if flow == nil || decision == nil || r == nil {
		return nil
	}

	driveID := r.TargetDriveID
	if driveID.IsZero() {
		driveID = r.DriveID
	}
	if driveID.IsZero() {
		driveID = flow.engine.driveID
	}

	fields := []any{
		slog.String("run_id", flow.runID),
		slog.Int64("action_id", r.ActionID),
		slog.String("summary_key", string(decision.SummaryKey)),
		slog.String("failure_class", decision.Class.String()),
		slog.String("log_owner", "sync"),
		slog.String("drive_id", driveID.String()),
		slog.String("action_type", r.ActionType.String()),
		slog.String("path", r.Path),
	}
	if !decision.ScopeKey.IsZero() {
		fields = append(fields, slog.String("scope_key", decision.ScopeKey.String()))
	}

	if r.HTTPStatus != 0 {
		fields = append(fields, slog.Int("http_status", r.HTTPStatus))
	}
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		fields = append(fields, slog.String("trial_scope_key", r.TrialScopeKey.String()))
	}

	return fields
}

func (flow *engineFlow) summaryLogFields(
	class errclass.Class,
	summaryKey SummaryKey,
	path string,
	scopeKey ScopeKey,
) []any {
	if flow == nil {
		return nil
	}

	fields := []any{
		slog.String("run_id", flow.runID),
		slog.String("summary_key", string(summaryKey)),
		slog.String("failure_class", class.String()),
		slog.String("log_owner", "sync"),
		slog.String("path", path),
	}
	if !scopeKey.IsZero() {
		fields = append(fields, slog.String("scope_key", scopeKey.String()))
	}

	return fields
}
