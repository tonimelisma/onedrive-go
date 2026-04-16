package sync

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
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

	report := auditPersistedIntegrity(blocks, failures)
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

	report := auditPersistedIntegrity(blocks, failures)

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
		{run: repairPersistedRemotePermissionScopes},
		{run: repairAuthScopeTiming},
		{run: repairNonRetryableFailureTiming},
	}

	for _, step := range repairSteps {
		rows, err := step.run(ctx, tx)
		if err != nil {
			return 0, err
		}
		repairsApplied += rows
	}

	return repairsApplied, nil
}

func auditPersistedIntegrity(
	blocks []*ScopeBlock,
	failures []SyncFailureRow,
) IntegrityReport {
	report := IntegrityReport{
		Findings: make([]IntegrityFinding, 0),
	}

	scopeBlockByKey := make(map[ScopeKey]*ScopeBlock, len(blocks))
	projectionSources := make(map[issueGroupKey]map[string]struct{})

	auditScopeBlocks(&report, blocks, scopeBlockByKey, projectionSources)
	auditFailureRows(&report, failures, scopeBlockByKey, projectionSources)
	addProjectionOverlapFindings(&report, projectionSources)

	return report
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

func repairPersistedRemotePermissionScopes(ctx context.Context, tx sqlTxRunner) (int, error) {
	repairsApplied := 0

	deleteScopeResult, err := tx.ExecContext(ctx, `
		DELETE FROM scope_blocks
		WHERE scope_key LIKE 'perm:remote:%'
		   OR scope_key LIKE 'perm:remote-write:%'`)
	if err != nil {
		return 0, fmt.Errorf("sync: delete persisted remote permission scopes: %w", err)
	}
	repairsApplied += rowsAffected(deleteScopeResult)

	deleteBoundaryResult, err := tx.ExecContext(ctx, `
		DELETE FROM sync_failures
		WHERE failure_role = ?
		  AND (scope_key LIKE 'perm:remote:%' OR scope_key LIKE 'perm:remote-write:%')`,
		FailureRoleBoundary,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: delete persisted remote permission boundaries: %w", err)
	}
	repairsApplied += rowsAffected(deleteBoundaryResult)

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

		if block.Key.IsPermRemoteWrite() {
			report.add(
				integrityCodeLegacyRemoteScope,
				fmt.Sprintf("persisted remote-write scope %s must be derived from held rows only", block.Key.String()),
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

	if row.Role == FailureRoleBoundary && row.ScopeKey.IsPermRemoteWrite() {
		report.add(
			integrityCodeLegacyRemoteBoundary,
			fmt.Sprintf("remote-write boundary row %s should be derived from held rows only", row.Path),
		)
	}

	if (row.Role == FailureRoleHeld || row.Role == FailureRoleBoundary) &&
		!row.ScopeKey.IsZero() &&
		!row.ScopeKey.IsPermRemoteWrite() {
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
	if row.Role == FailureRoleHeld && row.ScopeKey.IsPermRemoteWrite() {
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
	configuredDriveID, err := configuredDriveIDForDB(ctx, i.db)
	if err != nil {
		return nil, err
	}

	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		if isMissingTableErr(err) {
			return []SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows, configuredDriveID)
}
