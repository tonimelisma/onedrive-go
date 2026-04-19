package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
)

// RemoteBlockedGroup is the first-class derived view for one active
// shared-folder write block. It is built from block_scopes plus blocked
// retry_work rows.
type RemoteBlockedGroup struct {
	BoundaryPath string
	BlockedPaths []string
}

// VisibleConditionGroup is the store-owned grouping used by status and watch
// summaries. Count is the item count shown to users. VisibleCount matches the
// contribution to summary totals.
type VisibleConditionGroup struct {
	SummaryKey    SummaryKey
	IssueType     string
	ScopeKey      ScopeKey
	Count         int
	VisibleCount  int
	Paths         []string
	RemoteBlocked *RemoteBlockedGroup
}

type visibleConditionGroupKey struct {
	summaryKey SummaryKey
	scopeKey   ScopeKey
}

type visibleConditionProjection struct {
	groups  []VisibleConditionGroup
	summary ConditionSummary
}

type blockedRetryProjection struct {
	count int
	paths []string
}

func (m *SyncStore) ListVisibleConditionGroups(ctx context.Context) ([]VisibleConditionGroup, error) {
	projection, err := loadVisibleConditionProjection(ctx, m.db, m.logger)
	if err != nil {
		return nil, err
	}

	return projection.groups, nil
}

func (m *SyncStore) ReadVisibleConditionSummary(ctx context.Context) (ConditionSummary, error) {
	projection, err := loadVisibleConditionProjection(ctx, m.db, m.logger)
	if err != nil {
		return ConditionSummary{}, err
	}

	return projection.summary, nil
}

func loadVisibleConditionProjection(
	ctx context.Context,
	db *sql.DB,
	logger *slog.Logger,
) (visibleConditionProjection, error) {
	observationIssues, err := queryObservationIssueRowsDB(ctx, db)
	if err != nil {
		return visibleConditionProjection{}, fmt.Errorf("sync: listing visible observation issues: %w", err)
	}

	blockScopes, err := queryBlockScopesDB(ctx, db)
	if err != nil {
		return visibleConditionProjection{}, fmt.Errorf("sync: listing visible block scopes: %w", err)
	}

	blockedRetryRows, err := queryBlockedRetryWorkRowsDB(ctx, db)
	if err != nil {
		return visibleConditionProjection{}, fmt.Errorf("sync: listing visible blocked retry_work rows: %w", err)
	}

	retrying, err := queryRetryingWorkCount(ctx, db)
	if err != nil {
		return visibleConditionProjection{}, fmt.Errorf("sync: counting retrying retry_work rows: %w", err)
	}

	groups := buildVisibleConditionGroups(observationIssues, blockScopes, blockedRetryRows)
	if logger != nil {
		logger.Debug("loaded visible condition projection",
			slog.Int("groups", len(groups)),
		)
	}

	return visibleConditionProjection{
		groups:  groups,
		summary: buildVisibleConditionSummary(groups, retrying),
	}, nil
}

func queryObservationIssueRowsDB(ctx context.Context, db *sql.DB) ([]ObservationIssueRow, error) {
	configuredDriveID, err := configuredDriveIDForDB(ctx, db)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT `+sqlSelectObservationIssueCols+` FROM observation_issues
		ORDER BY last_seen_at DESC, path`)
	if err != nil {
		return nil, fmt.Errorf("query observation issues: %w", err)
	}
	defer rows.Close()

	return scanObservationIssueRows(rows, configuredDriveID)
}

func queryBlockScopesDB(ctx context.Context, db *sql.DB) ([]*BlockScope, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count
		FROM block_scopes`)
	if err != nil {
		return nil, fmt.Errorf("query block scopes: %w", err)
	}
	defer rows.Close()

	var result []*BlockScope
	for rows.Next() {
		block, err := scanBlockScopeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan block scope row: %w", err)
		}
		if block.Key.IsZero() {
			continue
		}

		result = append(result, block)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate block scope rows: %w", err)
	}

	if result == nil {
		result = []*BlockScope{}
	}

	return result, nil
}

func queryBlockedRetryWorkRowsDB(ctx context.Context, db *sql.DB) ([]RetryWorkRow, error) {
	rows, err := db.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("query blocked retry_work rows: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}

func queryRetryingWorkCount(ctx context.Context, db *sql.DB) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM retry_work WHERE blocked = 0 AND attempt_count >= 3`,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("scan retrying retry_work count: %w", err)
	}

	return count, nil
}

func buildVisibleConditionGroups(
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
	blockedRetryRows []RetryWorkRow,
) []VisibleConditionGroup {
	groupIndex := make(map[visibleConditionGroupKey]int)
	groups := make([]VisibleConditionGroup, 0, len(observationIssues)+len(blockScopes))

	addVisibleObservationConditionGroups(&groups, groupIndex, observationIssues)

	blockedByScope := groupBlockedRetryWork(blockedRetryRows)
	addVisibleBlockScopeGroups(&groups, groupIndex, blockScopes, blockedByScope)

	finalizeVisibleConditionGroups(groups)

	return groups
}

func groupBlockedRetryWork(rows []RetryWorkRow) map[ScopeKey]blockedRetryProjection {
	grouped := make(map[ScopeKey]blockedRetryProjection)
	for i := range rows {
		scopeKey := rows[i].ScopeKey
		if scopeKey.IsZero() {
			continue
		}

		projection := grouped[scopeKey]
		projection.count++
		if rows[i].Path != "" {
			projection.paths = append(projection.paths, rows[i].Path)
		}
		grouped[scopeKey] = projection
	}

	return grouped
}

func visibleRemoteBoundaryPath(boundary string) string {
	if boundary == "" {
		return "/"
	}

	return boundary
}

func buildVisibleConditionSummary(groups []VisibleConditionGroup, retrying int) ConditionSummary {
	counts := make(conditionGroupAccumulator)
	for i := range groups {
		if groups[i].SummaryKey == "" || groups[i].Count <= 0 {
			continue
		}
		counts.Add(groups[i].SummaryKey, groups[i].Count, "", "")
	}

	return ConditionSummary{
		Groups:   counts.Groups(),
		Retrying: retrying,
	}
}

func addVisibleObservationConditionGroups(
	groups *[]VisibleConditionGroup,
	groupIndex map[visibleConditionGroupKey]int,
	observationIssues []ObservationIssueRow,
) {
	for i := range observationIssues {
		group := ensureVisibleConditionGroup(
			groups,
			groupIndex,
			SummaryKeyForObservationIssue(observationIssues[i].IssueType, observationIssues[i].ScopeKey),
			observationIssues[i].IssueType,
			observationIssues[i].ScopeKey,
		)
		group.Count++
		group.VisibleCount++
		if observationIssues[i].Path != "" {
			group.Paths = append(group.Paths, observationIssues[i].Path)
		}
	}
}

func addVisibleBlockScopeGroups(
	groups *[]VisibleConditionGroup,
	groupIndex map[visibleConditionGroupKey]int,
	blockScopes []*BlockScope,
	blockedByScope map[ScopeKey]blockedRetryProjection,
) {
	for i := range blockScopes {
		block := blockScopes[i]
		if block == nil {
			continue
		}

		blocked := blockedByScope[block.Key]
		count := blocked.count
		if count == 0 {
			count = 1
		}

		group := ensureVisibleConditionGroup(
			groups,
			groupIndex,
			SummaryKeyForBlockScope(block.IssueType, block.Key),
			block.IssueType,
			block.Key,
		)
		group.Count += count
		group.VisibleCount += count
		if len(blocked.paths) > 0 {
			group.Paths = append(group.Paths, blocked.paths...)
		}
		if block.Key.IsPermRemote() {
			group.RemoteBlocked = &RemoteBlockedGroup{
				BoundaryPath: visibleRemoteBoundaryPath(block.Key.RemotePath()),
				BlockedPaths: append([]string(nil), blocked.paths...),
			}
		}
	}
}

func ensureVisibleConditionGroup(
	groups *[]VisibleConditionGroup,
	groupIndex map[visibleConditionGroupKey]int,
	summaryKey SummaryKey,
	issueType string,
	scopeKey ScopeKey,
) *VisibleConditionGroup {
	groupKey := visibleConditionGroupKey{
		summaryKey: summaryKey,
		scopeKey:   scopeKey,
	}
	if idx, ok := groupIndex[groupKey]; ok {
		return &(*groups)[idx]
	}

	*groups = append(*groups, VisibleConditionGroup{
		SummaryKey: summaryKey,
		IssueType:  issueType,
		ScopeKey:   scopeKey,
	})
	groupIndex[groupKey] = len(*groups) - 1

	return &(*groups)[len(*groups)-1]
}

func finalizeVisibleConditionGroups(groups []VisibleConditionGroup) {
	for i := range groups {
		sort.Strings(groups[i].Paths)
		groups[i].Paths = uniqueSortedStrings(groups[i].Paths)
		if groups[i].RemoteBlocked != nil {
			sort.Strings(groups[i].RemoteBlocked.BlockedPaths)
			groups[i].RemoteBlocked.BlockedPaths = uniqueSortedStrings(groups[i].RemoteBlocked.BlockedPaths)
		}
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].SummaryKey != groups[j].SummaryKey {
			return string(groups[i].SummaryKey) < string(groups[j].SummaryKey)
		}
		if groups[i].ScopeKey != groups[j].ScopeKey {
			return groups[i].ScopeKey.String() < groups[j].ScopeKey.String()
		}

		return groups[i].Count > groups[j].Count
	})
}

func uniqueSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}

	result := values[:1]
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			continue
		}
		result = append(result, values[i])
	}

	return result
}
