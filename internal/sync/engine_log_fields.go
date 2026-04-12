package sync

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/failures"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
		slog.String("log_owner", decision.LogOwner.String()),
		slog.String("scope_key", decision.ScopeKey.String()),
		slog.String("drive_id", driveID.String()),
		slog.String("action_type", r.ActionType.String()),
		slog.String("path", r.Path),
	}

	if r.ShortcutKey != "" {
		fields = append(fields, slog.String("shortcut_key", r.ShortcutKey))
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
	class failures.Class,
	summaryKey synctypes.SummaryKey,
	path string,
	scopeKey synctypes.ScopeKey,
) []any {
	if flow == nil {
		return nil
	}

	return []any{
		slog.String("run_id", flow.runID),
		slog.String("summary_key", string(summaryKey)),
		slog.String("failure_class", class.String()),
		slog.String("log_owner", failures.LogOwnerSync.String()),
		slog.String("scope_key", scopeKey.String()),
		slog.String("path", path),
	}
}
