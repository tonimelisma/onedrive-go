package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

	report := auditPersistedIntegrity(blocks, failures)
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
func (m *SyncStore) RepairIntegritySafe(ctx context.Context) (int, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sync: begin integrity repair tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	repairsApplied, err := repairIntegritySafeTx(ctx, tx)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sync: commit integrity repair: %w", err)
	}

	return repairsApplied, nil
}

func repairIntegritySafeTx(ctx context.Context, tx *sql.Tx) (int, error) {
	repairsApplied := 0

	repairSteps := []struct {
		run func(context.Context, *sql.Tx) (int, error)
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

func auditPersistedIntegrity(
	blocks []*synctypes.ScopeBlock,
	failures []synctypes.SyncFailureRow,
) IntegrityReport {
	report := IntegrityReport{
		Findings: make([]IntegrityFinding, 0),
	}

	scopeBlockByKey := make(map[synctypes.ScopeKey]*synctypes.ScopeBlock, len(blocks))
	projectionSources := make(map[issueGroupKey]map[string]struct{})

	auditScopeBlocks(&report, blocks, scopeBlockByKey, projectionSources)
	auditFailureRows(&report, failures, scopeBlockByKey, projectionSources)
	addProjectionOverlapFindings(&report, projectionSources)

	return report
}

func repairAuthScopeTiming(ctx context.Context, tx *sql.Tx) (int, error) {
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
		synctypes.IssueUnauthorized,
		synctypes.ScopeTimingNone,
		synctypes.SKAuthAccount().String(),
		synctypes.IssueUnauthorized,
		synctypes.ScopeTimingNone,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: normalize auth scope timing: %w", err)
	}

	return rowsAffected(authResult), nil
}

func repairLegacyThrottleAccountScope(ctx context.Context, tx *sql.Tx) (int, error) {
	nowNano := time.Now().UnixNano()
	repairsApplied := 0

	scopeResult, err := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`,
		synctypes.SKThrottleAccount().String(),
	)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy throttle scope: %w", err)
	}
	repairsApplied += rowsAffected(scopeResult)

	boundaryResult, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures
		WHERE scope_key = ? AND failure_role = ?`,
		synctypes.SKThrottleAccount().String(),
		synctypes.FailureRoleBoundary,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy throttle boundary rows: %w", err)
	}
	repairsApplied += rowsAffected(boundaryResult)

	heldResult, err := tx.ExecContext(ctx,
		`UPDATE sync_failures
		SET failure_role = ?, next_retry_at = ?
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		synctypes.FailureRoleItem,
		nowNano,
		synctypes.SKThrottleAccount().String(),
		synctypes.FailureRoleHeld,
		synctypes.CategoryTransient,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: release legacy throttle held rows: %w", err)
	}
	repairsApplied += rowsAffected(heldResult)

	return repairsApplied, nil
}

func repairNonRetryableFailureTiming(ctx context.Context, tx *sql.Tx) (int, error) {
	retryResult, err := tx.ExecContext(ctx, `
		UPDATE sync_failures
		SET next_retry_at = NULL
		WHERE next_retry_at IS NOT NULL
		  AND (category <> ? OR failure_role IN (?, ?))`,
		synctypes.CategoryTransient,
		synctypes.FailureRoleHeld,
		synctypes.FailureRoleBoundary,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: normalize non-retryable failure timing: %w", err)
	}

	return rowsAffected(retryResult), nil
}

func listLegacyRemoteScopeKeys(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT scope_key
		FROM (
			SELECT scope_key FROM scope_blocks WHERE scope_key LIKE 'perm:remote:%'
			UNION
			SELECT scope_key FROM sync_failures
			WHERE failure_role = ? AND scope_key LIKE 'perm:remote:%'
		)`,
		synctypes.FailureRoleBoundary,
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

func deleteLegacyRemoteScopeAuthorities(ctx context.Context, tx *sql.Tx, scopeKey string) (int, error) {
	repairsApplied := 0

	deleteScopeResult, err := tx.ExecContext(ctx, `DELETE FROM scope_blocks WHERE scope_key = ?`, scopeKey)
	if err != nil {
		return 0, fmt.Errorf("sync: delete legacy remote scope block %s: %w", scopeKey, err)
	}
	repairsApplied += rowsAffected(deleteScopeResult)

	deleteBoundaryResult, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ? AND failure_role = ?`,
		scopeKey, synctypes.FailureRoleBoundary,
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
			if rows[i].Status == synctypes.SyncStatusFiltered {
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
		case expectedReason == syncscope.ExclusionNone && rows[i].Status == synctypes.SyncStatusFiltered:
			report.add(
				integrityCodeScopeStateDrift,
				fmt.Sprintf("remote row %s is filtered but no longer excluded by scope_state", rows[i].Path),
			)
		case expectedReason != syncscope.ExclusionNone:
			if rows[i].Status != synctypes.SyncStatusFiltered ||
				rows[i].FilterGeneration != state.Generation ||
				rows[i].FilterReason != string(expectedReason) {
				report.add(
					integrityCodeScopeStateDrift,
					fmt.Sprintf(
						"remote row %s disagrees with scope_state (status=%s generation=%d reason=%s expected_generation=%d expected_reason=%s)",
						rows[i].Path,
						rows[i].Status,
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

func readScopeStateForAudit(ctx context.Context, db *sql.DB) (synctypes.ScopeStateRecord, bool, error) {
	return readScopeStateRecord(ctx, db, "scan scope state for audit")
}

func listRemoteScopeRowsForAudit(ctx context.Context, db *sql.DB) ([]remoteScopeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT drive_id, item_id, path, hash, sync_status, filter_generation, filter_reason, NULL, NULL
		FROM remote_state
		WHERE sync_status NOT IN (?, ?, ?, ?)`,
		synctypes.SyncStatusDeleted,
		synctypes.SyncStatusPendingDelete,
		synctypes.SyncStatusDownloading,
		synctypes.SyncStatusDeleting,
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
			&row.Status,
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
	blocks []*synctypes.ScopeBlock,
	scopeBlockByKey map[synctypes.ScopeKey]*synctypes.ScopeBlock,
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
		if block.Key == synctypes.SKThrottleAccount() {
			report.add(
				integrityCodeLegacyThrottleScope,
				"legacy persisted throttle:account scope must be released and rewritten as target-scoped throttles on new failures",
			)
		}

		if block.Key == synctypes.SKAuthAccount() && !authAccountScopeIsCanonical(block) {
			report.add(
				integrityCodeInvalidAuthScopeTiming,
				"auth:account scope must use timing_source='none' with zero trial metadata",
			)
		}

		summaryKey := synctypes.SummaryKeyForScopeBlock(block.IssueType, block.Key)
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
	failures []synctypes.SyncFailureRow,
	scopeBlockByKey map[synctypes.ScopeKey]*synctypes.ScopeBlock,
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
	row *synctypes.SyncFailureRow,
	scopeBlockByKey map[synctypes.ScopeKey]*synctypes.ScopeBlock,
) {
	if err := validateAuditedFailureRow(row); err != nil {
		report.add(integrityCodeInvalidFailureRow, err.Error())
	}

	if row.Category != synctypes.CategoryTransient && row.NextRetryAt > 0 {
		report.add(
			integrityCodeInvalidFailureTiming,
			fmt.Sprintf("non-transient row %s must not have retry timing", row.Path),
		)
	}

	if row.Role == synctypes.FailureRoleBoundary && row.ScopeKey.IsPermRemote() {
		report.add(
			integrityCodeLegacyRemoteBoundary,
			fmt.Sprintf("legacy perm:remote boundary row %s should be derived from held rows only", row.Path),
		)
	}

	if (row.Role == synctypes.FailureRoleHeld || row.Role == synctypes.FailureRoleBoundary) &&
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
	row *synctypes.SyncFailureRow,
) {
	summaryKey := synctypes.SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
	if summaryKey == "" {
		return
	}

	sourceType := "failure"
	if row.Role == synctypes.FailureRoleHeld && row.ScopeKey.IsPermRemote() {
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

func authAccountScopeIsCanonical(block *synctypes.ScopeBlock) bool {
	return block.IssueType == synctypes.IssueUnauthorized &&
		block.TimingSource == synctypes.ScopeTimingNone &&
		block.TrialInterval == 0 &&
		block.NextTrialAt.IsZero() &&
		block.PreserveUntil.IsZero() &&
		block.TrialCount == 0
}

func validateAuditedFailureRow(row *synctypes.SyncFailureRow) error {
	switch row.Role {
	case synctypes.FailureRoleHeld:
		if row.ScopeKey.IsZero() {
			return fmt.Errorf("held row %s is missing scope key", row.Path)
		}
		if row.Category != synctypes.CategoryTransient {
			return fmt.Errorf("held row %s must be transient", row.Path)
		}
		if row.NextRetryAt != 0 {
			return fmt.Errorf("held row %s must not be retryable before release", row.Path)
		}
	case synctypes.FailureRoleBoundary:
		if row.ScopeKey.IsZero() {
			return fmt.Errorf("boundary row %s is missing scope key", row.Path)
		}
		if row.Category != synctypes.CategoryActionable {
			return fmt.Errorf("boundary row %s must be actionable", row.Path)
		}
		if row.NextRetryAt != 0 {
			return fmt.Errorf("boundary row %s must not have retry timing", row.Path)
		}
	case synctypes.FailureRoleItem:
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

func (i *Inspector) listAllSyncFailures(ctx context.Context) ([]synctypes.SyncFailureRow, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		if isMissingTableErr(err) {
			return []synctypes.SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}
