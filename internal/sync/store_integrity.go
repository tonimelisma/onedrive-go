package sync

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

const (
	integrityCodeInvalidScopeBlock        = "invalid_scope_block"
	integrityCodeInvalidAuthScopeTiming   = "invalid_auth_scope_timing"
	integrityCodeLegacyThrottleScope      = "legacy_throttle_scope"
	integrityCodeLegacyRemoteScope        = "legacy_remote_scope"
	integrityCodeInvalidFailureRow        = "invalid_failure_row"
	integrityCodeInvalidFailureTiming     = "invalid_failure_timing"
	integrityCodeMissingScopeBlock        = "missing_scope_block"
	integrityCodeLegacyRemoteBoundary     = "legacy_remote_boundary"
	integrityCodeVisibleProjectionOverlap = "visible_projection_overlap"
	integrityCodeBaselineCacheMismatch    = "baseline_cache_mismatch"
	integrityCodeScopeStateDrift          = "scope_state_drift"
	integrityCodeInvalidConflictWorkflow  = "invalid_conflict_workflow"
	integrityCodeInvalidHeldDelete        = "invalid_held_delete_workflow"
)

// IntegrityFinding is one stable integrity problem detected in persisted sync
// state. Codes are machine-stable for JSON and tests; details are human-facing.
type IntegrityFinding struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// IntegrityReport is the store-owned integrity contract consumed by devtool
// and tests. Findings are sorted for deterministic output.
type IntegrityReport struct {
	Findings []IntegrityFinding `json:"findings"`
}

func (r IntegrityReport) HasFindings() bool {
	return len(r.Findings) > 0
}

func (r *IntegrityReport) add(code, detail string) {
	r.Findings = append(r.Findings, IntegrityFinding{
		Code:   code,
		Detail: detail,
	})
}

func (r *IntegrityReport) sort() {
	sort.Slice(r.Findings, func(i, j int) bool {
		if r.Findings[i].Code != r.Findings[j].Code {
			return r.Findings[i].Code < r.Findings[j].Code
		}

		return r.Findings[i].Detail < r.Findings[j].Detail
	})
}

// AuditIntegrity reports DB-only integrity findings through the read-only
// inspection boundary. It never mutates the database or applies repairs.
func (i *Inspector) AuditIntegrity(ctx context.Context) (IntegrityReport, error) {
	failures, err := i.listAllSyncFailures(ctx)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list sync failures for integrity audit: %w", err)
	}

	blocks, err := i.listScopeBlocks(ctx)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list scope blocks for integrity audit: %w", err)
	}

	conflicts, err := queryAllConflictsForAudit(ctx, i.db)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list conflicts for integrity audit: %w", err)
	}
	conflictRequests, err := queryAllConflictRequestsForAudit(ctx, i.db)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list conflict requests for integrity audit: %w", err)
	}

	heldDeletes, err := queryHeldDeletesForAudit(ctx, i.db)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list held deletes for integrity audit: %w", err)
	}

	report := auditPersistedIntegrity(blocks, failures, conflicts, conflictRequests, heldDeletes)
	if err := auditScopeStateConsistency(ctx, i.db, &report); err != nil {
		return IntegrityReport{}, err
	}
	report.sort()

	return report, nil
}

// AuditIntegrity reports persisted integrity findings and also includes the
// store-local baseline cache consistency signal when a cache is loaded.
func (m *SyncStore) AuditIntegrity(ctx context.Context) (IntegrityReport, error) {
	failures, err := m.ListSyncFailures(ctx)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list sync failures for integrity audit: %w", err)
	}

	blocks, err := m.ListScopeBlocks(ctx)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list scope blocks for integrity audit: %w", err)
	}

	conflicts, err := queryAllConflictsForAudit(ctx, m.db)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list conflicts for integrity audit: %w", err)
	}
	conflictRequests, err := queryAllConflictRequestsForAudit(ctx, m.db)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list conflict requests for integrity audit: %w", err)
	}

	heldDeletes, err := queryHeldDeletesForAudit(ctx, m.db)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("list held deletes for integrity audit: %w", err)
	}

	report := auditPersistedIntegrity(blocks, failures, conflicts, conflictRequests, heldDeletes)
	if auditErr := auditScopeStateConsistency(ctx, m.db, &report); auditErr != nil {
		return IntegrityReport{}, auditErr
	}

	if _, loadErr := m.Load(ctx); loadErr != nil {
		return IntegrityReport{}, fmt.Errorf("load baseline for integrity audit: %w", loadErr)
	}

	mismatches, err := m.CheckCacheConsistency(ctx)
	if err != nil {
		return IntegrityReport{}, fmt.Errorf("check baseline cache consistency: %w", err)
	}
	if mismatches > 0 {
		report.add(
			integrityCodeBaselineCacheMismatch,
			fmt.Sprintf("baseline cache mismatches detected: %d", mismatches),
		)
	}

	report.sort()

	return report, nil
}

// RepairIntegritySafe applies only deterministic integrity repairs that do not
// guess user intent, then returns the number of rows or scope authorities
// normalized.
func (m *SyncStore) RepairIntegritySafe(ctx context.Context) (repairsApplied int, err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return 0, fmt.Errorf("sync: begin integrity repair tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback integrity repair tx")
	}()

	repairsApplied, err = repairIntegritySafeTx(ctx, tx)
	if err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("sync: commit integrity repair: %w", err)
	}

	return repairsApplied, nil
}

func repairIntegritySafeTx(ctx context.Context, tx sqlTxRunner) (int, error) {
	repairsApplied := 0

	repairSteps := []struct {
		run func(context.Context, sqlTxRunner) (int, error)
	}{
		{run: repairLegacyThrottleAccountScope},
		{run: repairAuthScopeTiming},
		{run: repairNonRetryableFailureTiming},
		{run: repairScopeStateConsistencyTx},
	}

	for _, step := range repairSteps {
		rows, err := step.run(ctx, tx)
		if err != nil {
			return 0, err
		}
		repairsApplied += rows
	}

	legacyScopeKeys, err := listLegacyRemoteScopeKeys(ctx, tx)
	if err != nil {
		return 0, err
	}
	for _, scopeKey := range legacyScopeKeys {
		rows, deleteErr := deleteLegacyRemoteScopeAuthorities(ctx, tx, scopeKey)
		if deleteErr != nil {
			return 0, deleteErr
		}
		repairsApplied += rows
	}

	return repairsApplied, nil
}

func queryAllConflictsForAudit(ctx context.Context, db *sql.DB) ([]ConflictRecord, error) {
	return queryConflictRows(
		ctx,
		db,
		sqlListAllConflicts,
		"query conflicts for audit",
		"iterate conflict audit rows",
	)
}

func queryHeldDeletesForAudit(ctx context.Context, db *sql.DB) ([]HeldDeleteRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT drive_id, action_type, path, item_id, state, held_at, approved_at,
			last_planned_at, last_error
		FROM held_deletes
		ORDER BY last_planned_at, path`)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("query held deletes for audit: %w", err)
	}
	defer rows.Close()

	return scanHeldDeleteRows(rows)
}

func queryAllConflictRequestsForAudit(ctx context.Context, db *sql.DB) ([]ConflictRequestRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT conflict_id, requested_resolution, state, requested_at, applying_at, last_error
		FROM conflict_requests
		ORDER BY requested_at, conflict_id`)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("query conflict requests for audit: %w", err)
	}
	defer rows.Close()

	var requests []ConflictRequestRecord
	for rows.Next() {
		var (
			record      ConflictRequestRecord
			requestedAt sql.NullInt64
			applyingAt  sql.NullInt64
			lastErr     sql.NullString
		)
		if err := rows.Scan(
			&record.ID,
			&record.RequestedResolution,
			&record.State,
			&requestedAt,
			&applyingAt,
			&lastErr,
		); err != nil {
			return nil, fmt.Errorf("scan conflict request audit row: %w", err)
		}
		if requestedAt.Valid {
			record.RequestedAt = requestedAt.Int64
		}
		if applyingAt.Valid {
			record.ApplyingAt = applyingAt.Int64
		}
		record.LastError = lastErr.String
		requests = append(requests, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflict request audit rows: %w", err)
	}

	return requests, nil
}

func auditPersistedIntegrity(
	blocks []*ScopeBlock,
	failures []SyncFailureRow,
	conflicts []ConflictRecord,
	conflictRequests []ConflictRequestRecord,
	heldDeletes []HeldDeleteRecord,
) IntegrityReport {
	report := IntegrityReport{
		Findings: make([]IntegrityFinding, 0),
	}

	scopeBlockByKey := make(map[ScopeKey]*ScopeBlock, len(blocks))
	projectionSources := make(map[issueGroupKey]map[string]struct{})

	auditScopeBlocks(&report, blocks, scopeBlockByKey, projectionSources)
	auditFailureRows(&report, failures, scopeBlockByKey, projectionSources)
	auditConflictRows(&report, conflicts)
	auditConflictRequestRows(&report, conflicts, conflictRequests)
	auditHeldDeleteRows(&report, heldDeletes)
	addProjectionOverlapFindings(&report, projectionSources)

	return report
}

func auditConflictRows(report *IntegrityReport, conflicts []ConflictRecord) {
	for i := range conflicts {
		row := &conflicts[i]
		if !validConflictResolution(row.Resolution) {
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict %s has invalid final resolution %q", row.ID, row.Resolution),
			)
		}
		if row.Resolution == ResolutionUnresolved {
			if row.ResolvedAt != 0 {
				report.add(
					integrityCodeInvalidConflictWorkflow,
					fmt.Sprintf("conflict %s is unresolved with resolved_at set", row.ID),
				)
			}
			if row.ResolvedBy != "" {
				report.add(
					integrityCodeInvalidConflictWorkflow,
					fmt.Sprintf("conflict %s is unresolved with resolved_by %q", row.ID, row.ResolvedBy),
				)
			}
			continue
		}

		if row.ResolvedAt == 0 {
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict %s is resolved without resolved_at", row.ID),
			)
		}
		if row.ResolvedBy != "" && row.ResolvedBy != ResolvedByUser && row.ResolvedBy != ResolvedByAuto {
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict %s has invalid resolved_by %q", row.ID, row.ResolvedBy),
			)
		}
	}
}

func validConflictResolution(resolution string) bool {
	switch resolution {
	case ResolutionUnresolved, ResolutionKeepBoth,
		ResolutionKeepLocal, ResolutionKeepRemote:
		return true
	default:
		return false
	}
}

func validRequestedConflictResolution(resolution string) bool {
	switch resolution {
	case ResolutionKeepBoth, ResolutionKeepLocal, ResolutionKeepRemote:
		return true
	default:
		return false
	}
}

func auditConflictRequestRows(
	report *IntegrityReport,
	conflicts []ConflictRecord,
	requests []ConflictRequestRecord,
) {
	conflictByID := make(map[string]ConflictRecord, len(conflicts))
	for i := range conflicts {
		conflictByID[conflicts[i].ID] = conflicts[i]
	}

	for i := range requests {
		row := &requests[i]
		if !validRequestedConflictResolution(row.RequestedResolution) {
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict %s has invalid requested resolution %q", row.ID, row.RequestedResolution),
			)
		}

		conflict, ok := conflictByID[row.ID]
		if !ok {
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict request %s has no conflict row", row.ID),
			)
			continue
		}
		if conflict.Resolution != ResolutionUnresolved {
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict request %s targets already resolved conflict", row.ID),
			)
		}

		switch row.State {
		case ConflictStateQueued:
			if row.RequestedResolution == "" {
				report.add(
					integrityCodeInvalidConflictWorkflow,
					fmt.Sprintf("conflict %s is queued without requested_resolution", row.ID),
				)
			}
		case ConflictStateApplying:
			if row.RequestedResolution == "" {
				report.add(
					integrityCodeInvalidConflictWorkflow,
					fmt.Sprintf("conflict %s is applying without requested_resolution", row.ID),
				)
			}
			if row.ApplyingAt == 0 {
				report.add(
					integrityCodeInvalidConflictWorkflow,
					fmt.Sprintf("conflict %s is applying without applying_at", row.ID),
				)
			}
		default:
			report.add(
				integrityCodeInvalidConflictWorkflow,
				fmt.Sprintf("conflict %s has invalid workflow state %q", row.ID, row.State),
			)
		}
	}
}

func auditHeldDeleteRows(report *IntegrityReport, heldDeletes []HeldDeleteRecord) {
	for i := range heldDeletes {
		row := &heldDeletes[i]
		if row.State != HeldDeleteStateHeld && row.State != HeldDeleteStateApproved {
			report.add(
				integrityCodeInvalidHeldDelete,
				fmt.Sprintf("held delete %s has invalid state %q", row.Path, row.State),
			)
		}
		if row.ItemID == "" {
			report.add(
				integrityCodeInvalidHeldDelete,
				fmt.Sprintf("held delete %s is missing item_id", row.Path),
			)
		}
		if row.State == HeldDeleteStateApproved && row.ApprovedAt == 0 {
			report.add(
				integrityCodeInvalidHeldDelete,
				fmt.Sprintf("approved held delete %s is missing approved_at", row.Path),
			)
		}
	}
}

func repairAuthScopeTiming(ctx context.Context, tx sqlTxRunner) (int, error) {
	authResult, err := tx.ExecContext(ctx, `
		UPDATE scope_blocks
		SET issue_type = ?,
			timing_source = ?,
			trial_interval = 0,
			next_trial_at = 0,
			preserve_until = 0,
			trial_count = 0
		WHERE scope_key = ?
		  AND (
			issue_type <> ?
			OR timing_source <> ?
			OR trial_interval <> 0
			OR next_trial_at <> 0
			OR preserve_until <> 0
			OR trial_count <> 0
		  )`,
		IssueUnauthorized,
		ScopeTimingNone,
		SKAuthAccount().String(),
		IssueUnauthorized,
		ScopeTimingNone,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: normalize auth scope timing: %w", err)
	}

	return rowsAffected(authResult), nil
}

func repairLegacyThrottleAccountScope(ctx context.Context, tx sqlTxRunner) (int, error) {
	nowNano := time.Now().UnixNano()
	repairsApplied := 0

	scopeResult, err := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`,
		SKThrottleAccount().String(),
	)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy throttle scope: %w", err)
	}
	repairsApplied += rowsAffected(scopeResult)

	boundaryResult, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures
		WHERE scope_key = ? AND failure_role = ?`,
		SKThrottleAccount().String(),
		FailureRoleBoundary,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy throttle boundary rows: %w", err)
	}
	repairsApplied += rowsAffected(boundaryResult)

	heldResult, err := tx.ExecContext(ctx,
		`UPDATE sync_failures
		SET failure_role = ?, next_retry_at = ?
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		FailureRoleItem,
		nowNano,
		SKThrottleAccount().String(),
		FailureRoleHeld,
		CategoryTransient,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: release legacy throttle held rows: %w", err)
	}
	repairsApplied += rowsAffected(heldResult)

	return repairsApplied, nil
}

func repairNonRetryableFailureTiming(ctx context.Context, tx sqlTxRunner) (int, error) {
	retryResult, err := tx.ExecContext(ctx, `
		UPDATE sync_failures
		SET next_retry_at = NULL
		WHERE next_retry_at IS NOT NULL
		  AND (category <> ? OR failure_role IN (?, ?))`,
		CategoryTransient,
		FailureRoleHeld,
		FailureRoleBoundary,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: normalize non-retryable failure timing: %w", err)
	}

	return rowsAffected(retryResult), nil
}

func listLegacyRemoteScopeKeys(ctx context.Context, tx sqlTxRunner) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT scope_key
		FROM (
			SELECT scope_key FROM scope_blocks WHERE scope_key LIKE 'perm:remote:%'
			UNION
			SELECT scope_key FROM sync_failures
			WHERE failure_role = ? AND scope_key LIKE 'perm:remote:%'
		)`,
		FailureRoleBoundary,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: list legacy remote scope repairs: %w", err)
	}
	defer rows.Close()

	var legacyScopeKeys []string
	for rows.Next() {
		var scopeKey string
		if err := rows.Scan(&scopeKey); err != nil {
			return nil, fmt.Errorf("sync: scan legacy remote scope repair row: %w", err)
		}
		legacyScopeKeys = append(legacyScopeKeys, scopeKey)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterate legacy remote scope repair rows: %w", err)
	}

	return legacyScopeKeys, nil
}

func deleteLegacyRemoteScopeAuthorities(ctx context.Context, tx sqlTxRunner, scopeKey string) (int, error) {
	repairsApplied := 0

	deleteScopeResult, err := tx.ExecContext(ctx, `DELETE FROM scope_blocks WHERE scope_key = ?`, scopeKey)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy remote scope block %s: %w", scopeKey, err)
	}
	repairsApplied += rowsAffected(deleteScopeResult)

	deleteBoundaryResult, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ? AND failure_role = ?`,
		scopeKey, FailureRoleBoundary,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy remote scope boundary %s: %w", scopeKey, err)
	}
	repairsApplied += rowsAffected(deleteBoundaryResult)

	return repairsApplied, nil
}

func auditScopeStateConsistency(ctx context.Context, db *sql.DB, report *IntegrityReport) error {
	state, found, err := readScopeStateForAudit(ctx, db)
	if err != nil {
		return fmt.Errorf("read scope state for integrity audit: %w", err)
	}

	rows, err := listRemoteScopeRowsForAudit(ctx, db)
	if err != nil {
		return fmt.Errorf("list remote scope rows for integrity audit: %w", err)
	}

	if !found {
		for i := range rows {
			if rows[i].IsFiltered {
				report.add(
					integrityCodeScopeStateDrift,
					fmt.Sprintf("filtered remote row %s exists without scope_state authority", rows[i].Path),
				)
			}
		}

		return nil
	}

	snapshot, err := syncscope.UnmarshalSnapshot(state.EffectiveSnapshotJSON)
	if err != nil {
		report.add(integrityCodeScopeStateDrift, fmt.Sprintf("scope_state snapshot is invalid JSON: %v", err))
		return nil
	}

	for i := range rows {
		expectedReason := snapshot.ExclusionReason(rows[i].Path)
		switch {
		case expectedReason == syncscope.ExclusionNone && rows[i].IsFiltered:
			report.add(
				integrityCodeScopeStateDrift,
				fmt.Sprintf("remote row %s is filtered but no longer excluded by scope_state", rows[i].Path),
			)
		case expectedReason != syncscope.ExclusionNone:
			if !rows[i].IsFiltered ||
				rows[i].FilterGeneration != state.Generation ||
				rows[i].FilterReason != string(expectedReason) {
				report.add(
					integrityCodeScopeStateDrift,
					fmt.Sprintf(
						"remote row %s disagrees with scope_state (is_filtered=%t generation=%d reason=%s expected_generation=%d expected_reason=%s)",
						rows[i].Path,
						rows[i].IsFiltered,
						rows[i].FilterGeneration,
						rows[i].FilterReason,
						state.Generation,
						expectedReason,
					),
				)
			}
		}
	}

	return nil
}

func readScopeStateForAudit(ctx context.Context, db *sql.DB) (ScopeStateRecord, bool, error) {
	return readScopeStateRecord(ctx, db, "scan scope state for audit")
}

func listRemoteScopeRowsForAudit(ctx context.Context, db *sql.DB) ([]remoteScopeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT drive_id, item_id, path, hash, is_filtered, filter_generation, filter_reason, NULL, NULL
		FROM remote_state`,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("query remote scope rows for audit: %w", err)
	}
	defer rows.Close()

	var result []remoteScopeRow
	for rows.Next() {
		var row remoteScopeRow
		if err := rows.Scan(
			&row.DriveID,
			&row.ItemID,
			&row.Path,
			&row.Hash,
			&row.IsFiltered,
			&row.FilterGeneration,
			&row.FilterReason,
			&row.BaselinePath,
			&row.BaselineHash,
		); err != nil {
			return nil, fmt.Errorf("scan remote scope row for audit: %w", err)
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate remote scope rows for audit: %w", err)
	}

	return result, nil
}

func auditScopeBlocks(
	report *IntegrityReport,
	blocks []*ScopeBlock,
	scopeBlockByKey map[ScopeKey]*ScopeBlock,
	projectionSources map[issueGroupKey]map[string]struct{},
) {
	for i := range blocks {
		block := blocks[i]
		scopeBlockByKey[block.Key] = block

		if err := validateScopeBlock(block); err != nil {
			report.add(integrityCodeInvalidScopeBlock, err.Error())
		}

		if block.Key.IsPermRemote() {
			report.add(
				integrityCodeLegacyRemoteScope,
				fmt.Sprintf("legacy persisted perm:remote scope %s must be derived from held rows only", block.Key.String()),
			)
		}
		if block.Key == SKThrottleAccount() {
			report.add(
				integrityCodeLegacyThrottleScope,
				"legacy persisted throttle:account scope must be released and rewritten as target-scoped throttles on new failures",
			)
		}

		if block.Key == SKAuthAccount() && !authAccountScopeIsCanonical(block) {
			report.add(
				integrityCodeInvalidAuthScopeTiming,
				"auth:account scope must use timing_source='none' with zero trial metadata",
			)
		}

		summaryKey := SummaryKeyForScopeBlock(block.IssueType, block.Key)
		if summaryKey != "" {
			addProjectionSource(projectionSources, issueGroupKey{
				summaryKey: summaryKey,
				scopeKey:   block.Key.String(),
			}, "scope_block")
		}
	}
}

func auditFailureRows(
	report *IntegrityReport,
	failures []SyncFailureRow,
	scopeBlockByKey map[ScopeKey]*ScopeBlock,
	projectionSources map[issueGroupKey]map[string]struct{},
) {
	for i := range failures {
		row := failures[i]
		auditFailureRow(report, &row, scopeBlockByKey)
		addFailureProjectionSource(projectionSources, &row)
	}
}

func auditFailureRow(
	report *IntegrityReport,
	row *SyncFailureRow,
	scopeBlockByKey map[ScopeKey]*ScopeBlock,
) {
	if err := validateFailureRowState(row); err != nil {
		report.add(integrityCodeInvalidFailureRow, err.Error())
	}

	if row.Category != CategoryTransient && row.NextRetryAt > 0 {
		report.add(
			integrityCodeInvalidFailureTiming,
			fmt.Sprintf("non-transient row %s must not have retry timing", row.Path),
		)
	}

	if row.Role == FailureRoleBoundary && row.ScopeKey.IsPermRemote() {
		report.add(
			integrityCodeLegacyRemoteBoundary,
			fmt.Sprintf("legacy perm:remote boundary row %s should be derived from held rows only", row.Path),
		)
	}

	if (row.Role == FailureRoleHeld || row.Role == FailureRoleBoundary) &&
		!row.ScopeKey.IsZero() &&
		!row.ScopeKey.IsPermRemote() {
		if _, ok := scopeBlockByKey[row.ScopeKey]; !ok {
			report.add(
				integrityCodeMissingScopeBlock,
				fmt.Sprintf("%s row %s for scope %s has no persisted scope block", row.Role, row.Path, row.ScopeKey.String()),
			)
		}
	}
}

func addFailureProjectionSource(
	projectionSources map[issueGroupKey]map[string]struct{},
	row *SyncFailureRow,
) {
	summaryKey := SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
	if summaryKey == "" {
		return
	}

	sourceType := "failure"
	if row.Role == FailureRoleHeld && row.ScopeKey.IsPermRemote() {
		sourceType = "held_remote"
	}
	addProjectionSource(projectionSources, issueGroupKey{
		summaryKey: summaryKey,
		scopeKey:   row.ScopeKey.String(),
	}, sourceType)
}

func addProjectionOverlapFindings(
	report *IntegrityReport,
	projectionSources map[issueGroupKey]map[string]struct{},
) {
	for groupKey, sources := range projectionSources {
		if len(sources) < 2 {
			continue
		}
		report.add(
			integrityCodeVisibleProjectionOverlap,
			fmt.Sprintf("visible issue projection %s/%s is backed by overlapping durable sources", groupKey.summaryKey, groupKey.scopeKey),
		)
	}
}

func authAccountScopeIsCanonical(block *ScopeBlock) bool {
	return block.IssueType == IssueUnauthorized &&
		block.TimingSource == ScopeTimingNone &&
		block.TrialInterval == 0 &&
		block.NextTrialAt.IsZero() &&
		block.PreserveUntil.IsZero() &&
		block.TrialCount == 0
}

func validateFailureRowState(row *SyncFailureRow) error {
	switch row.Role {
	case FailureRoleHeld:
		if row.ScopeKey.IsZero() {
			return fmt.Errorf("held row %s is missing scope key", row.Path)
		}
		if row.Category != CategoryTransient {
			return fmt.Errorf("held row %s must be transient", row.Path)
		}
		if row.NextRetryAt != 0 {
			return fmt.Errorf("held row %s must not be retryable before release", row.Path)
		}
	case FailureRoleBoundary:
		if row.ScopeKey.IsZero() {
			return fmt.Errorf("boundary row %s is missing scope key", row.Path)
		}
		if row.Category != CategoryActionable {
			return fmt.Errorf("boundary row %s must be actionable", row.Path)
		}
		if row.NextRetryAt != 0 {
			return fmt.Errorf("boundary row %s must not have retry timing", row.Path)
		}
	case FailureRoleItem:
	default:
		return fmt.Errorf("row %s has invalid failure role %q", row.Path, row.Role)
	}

	return nil
}

func addProjectionSource(
	dest map[issueGroupKey]map[string]struct{},
	key issueGroupKey,
	source string,
) {
	if key.summaryKey == "" || key.scopeKey == "" {
		return
	}

	sourceSet, ok := dest[key]
	if !ok {
		sourceSet = make(map[string]struct{})
		dest[key] = sourceSet
	}
	sourceSet[source] = struct{}{}
}

func rowsAffected(result sql.Result) int {
	if result == nil {
		return 0
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0
	}
	return int(rows)
}

func (i *Inspector) listAllSyncFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		if isMissingTableErr(err) {
			return []SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}
