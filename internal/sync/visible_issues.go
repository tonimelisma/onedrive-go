package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// RemoteBlockedGroup is the first-class derived view for one active
// shared-folder write block. It is built from held perm:remote-write rows only.
type RemoteBlockedGroup struct {
	BoundaryPath string
	BlockedPaths []string
}

// VisibleIssueGroup is the store-owned grouping used by issues, status, and
// watch summaries. Count is the issue-section item count. VisibleCount is the
// status/watch contribution for this group.
type VisibleIssueGroup struct {
	SummaryKey    SummaryKey
	IssueType     string
	ScopeKey      ScopeKey
	Count         int
	VisibleCount  int
	Paths         []string
	RemoteBlocked *RemoteBlockedGroup
}

type visibleIssueGroupKey struct {
	summaryKey SummaryKey
	scopeKey   ScopeKey
}

type visibleIssueProjection struct {
	groups  []VisibleIssueGroup
	summary IssueSummary
}

func (m *SyncStore) ListVisibleIssueGroups(ctx context.Context) ([]VisibleIssueGroup, error) {
	projection, err := loadVisibleIssueProjection(ctx, m.db, m.logger)
	if err != nil {
		return nil, err
	}
	return projection.groups, nil
}

func (m *SyncStore) CountVisibleIssues(ctx context.Context) (int, error) {
	summary, err := m.ReadVisibleIssueSummary(ctx)
	if err != nil {
		return 0, err
	}
	return summary.VisibleTotal(), nil
}

func (m *SyncStore) ReadVisibleIssueSummary(ctx context.Context) (IssueSummary, error) {
	projection, err := loadVisibleIssueProjection(ctx, m.db, m.logger)
	if err != nil {
		return IssueSummary{}, err
	}
	return projection.summary, nil
}

func loadVisibleIssueProjection(
	ctx context.Context,
	db *sql.DB,
	logger *slog.Logger,
) (visibleIssueProjection, error) {
	actionable, err := querySyncFailureRowsDB(ctx, db,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable'
		ORDER BY last_seen_at DESC`,
	)
	if err != nil {
		return visibleIssueProjection{}, fmt.Errorf("sync: listing visible actionable failures: %w", err)
	}

	remoteBlocked, err := querySyncFailureRowsDB(ctx, db,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE failure_role = ?
			AND (scope_key LIKE 'perm:remote-write:%' OR scope_key LIKE 'perm:remote:%')
		ORDER BY last_seen_at DESC`,
		FailureRoleHeld,
	)
	if err != nil {
		return visibleIssueProjection{}, fmt.Errorf("sync: listing visible remote blocked failures: %w", err)
	}

	authBlocks, err := queryAuthScopeBlocks(ctx, db)
	if err != nil {
		return visibleIssueProjection{}, fmt.Errorf("sync: listing visible auth scope blocks: %w", err)
	}

	retrying, err := queryRetryingIssueCount(ctx, db)
	if err != nil {
		return visibleIssueProjection{}, fmt.Errorf("sync: counting retrying sync failures: %w", err)
	}

	groups := buildVisibleIssueGroups(actionable, remoteBlocked, authBlocks)
	if logger != nil {
		logger.Debug("loaded visible issue projection",
			slog.Int("groups", len(groups)),
		)
	}

	return visibleIssueProjection{
		groups:  groups,
		summary: buildVisibleIssueSummary(groups, retrying),
	}, nil
}

func queryAuthScopeBlocks(ctx context.Context, db *sql.DB) ([]*ScopeBlock, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count
		FROM scope_blocks
		WHERE scope_key = ?`,
		SKAuthAccount().String(),
	)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query auth scope blocks: %w", err)
	}
	defer rows.Close()

	var blocks []*ScopeBlock
	for rows.Next() {
		var block ScopeBlock
		var wire string
		var blockedAt int64
		var trialInterval int64
		var nextTrialAt int64
		var preserveUntil int64
		if err := rows.Scan(
			&wire,
			&block.IssueType,
			&block.TimingSource,
			&blockedAt,
			&trialInterval,
			&nextTrialAt,
			&preserveUntil,
			&block.TrialCount,
		); err != nil {
			return nil, fmt.Errorf("scan auth scope block: %w", err)
		}
		block.Key = ParseScopeKey(wire)
		block.BlockedAt = time.Unix(0, blockedAt)
		block.TrialInterval = time.Duration(trialInterval)
		block.NextTrialAt = time.Unix(0, nextTrialAt)
		if preserveUntil > 0 {
			block.PreserveUntil = time.Unix(0, preserveUntil)
		}
		blocks = append(blocks, &block)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auth scope blocks: %w", err)
	}

	return blocks, nil
}

func isMissingTableErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

func queryRetryingIssueCount(ctx context.Context, db *sql.DB) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3`,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("scan retrying issue count: %w", err)
	}
	return count, nil
}

func buildVisibleIssueGroups(
	actionable []SyncFailureRow,
	remoteBlocked []SyncFailureRow,
	authBlocks []*ScopeBlock,
) []VisibleIssueGroup {
	groupIndex := make(map[visibleIssueGroupKey]int)
	groups := make([]VisibleIssueGroup, 0, len(actionable)+len(authBlocks)+1)

	addVisibleActionableGroups(&groups, groupIndex, actionable)
	addVisibleRemoteBlockedGroups(&groups, groupIndex, remoteBlocked)
	addVisibleAuthScopeGroups(&groups, groupIndex, authBlocks)
	finalizeVisibleIssueGroups(groups)

	return groups
}

func visibleRemoteBoundaryPath(boundary string) string {
	if boundary == "" {
		return "/"
	}

	return boundary
}

func buildVisibleIssueSummary(groups []VisibleIssueGroup, retrying int) IssueSummary {
	counts := make(map[SummaryKey]int)
	for i := range groups {
		if groups[i].SummaryKey == "" || groups[i].VisibleCount <= 0 {
			continue
		}
		counts[groups[i].SummaryKey] += groups[i].VisibleCount
	}

	summary := IssueSummary{
		Groups:   make([]IssueGroupCount, 0, len(counts)),
		Retrying: retrying,
	}
	for key, count := range counts {
		summary.Groups = append(summary.Groups, IssueGroupCount{Key: key, Count: count})
	}
	sort.Slice(summary.Groups, func(i, j int) bool {
		return string(summary.Groups[i].Key) < string(summary.Groups[j].Key)
	})

	return summary
}

func querySyncFailureRowsDB(ctx context.Context, db *sql.DB, query string, args ...any) ([]SyncFailureRow, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

func addVisibleActionableGroups(
	groups *[]VisibleIssueGroup,
	groupIndex map[visibleIssueGroupKey]int,
	actionable []SyncFailureRow,
) {
	for i := range actionable {
		group := ensureVisibleFailureGroup(groups, groupIndex, &actionable[i], 0)
		group.Count++
		group.VisibleCount++
		if actionable[i].Path != "" {
			group.Paths = append(group.Paths, actionable[i].Path)
		}
	}
}

func addVisibleRemoteBlockedGroups(
	groups *[]VisibleIssueGroup,
	groupIndex map[visibleIssueGroupKey]int,
	remoteBlocked []SyncFailureRow,
) {
	for i := range remoteBlocked {
		group := ensureVisibleFailureGroup(groups, groupIndex, &remoteBlocked[i], 1)
		group.Count++
		if remoteBlocked[i].Path != "" {
			group.Paths = append(group.Paths, remoteBlocked[i].Path)
		}
		if group.RemoteBlocked == nil {
			group.RemoteBlocked = &RemoteBlockedGroup{
				BoundaryPath: visibleRemoteBoundaryPath(remoteBlocked[i].ScopeKey.RemotePath()),
			}
		}
		group.RemoteBlocked.BlockedPaths = append(group.RemoteBlocked.BlockedPaths, remoteBlocked[i].Path)
	}
}

func addVisibleAuthScopeGroups(
	groups *[]VisibleIssueGroup,
	groupIndex map[visibleIssueGroupKey]int,
	authBlocks []*ScopeBlock,
) {
	for i := range authBlocks {
		summaryKey := SummaryKeyForScopeBlock(authBlocks[i].IssueType, authBlocks[i].Key)
		groupKey := visibleIssueGroupKey{
			summaryKey: summaryKey,
			scopeKey:   authBlocks[i].Key,
		}
		if _, ok := groupIndex[groupKey]; ok {
			continue
		}
		*groups = append(*groups, VisibleIssueGroup{
			SummaryKey:   summaryKey,
			IssueType:    authBlocks[i].IssueType,
			ScopeKey:     authBlocks[i].Key,
			Count:        1,
			VisibleCount: 1,
		})
		groupIndex[groupKey] = len(*groups) - 1
	}
}

func ensureVisibleFailureGroup(
	groups *[]VisibleIssueGroup,
	groupIndex map[visibleIssueGroupKey]int,
	row *SyncFailureRow,
	visibleCount int,
) *VisibleIssueGroup {
	summaryKey := SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
	groupKey := visibleIssueGroupKey{
		summaryKey: summaryKey,
		scopeKey:   row.ScopeKey,
	}
	if idx, ok := groupIndex[groupKey]; ok {
		return &(*groups)[idx]
	}

	*groups = append(*groups, VisibleIssueGroup{
		SummaryKey:   summaryKey,
		IssueType:    row.IssueType,
		ScopeKey:     row.ScopeKey,
		VisibleCount: visibleCount,
	})
	groupIndex[groupKey] = len(*groups) - 1

	return &(*groups)[len(*groups)-1]
}

func finalizeVisibleIssueGroups(groups []VisibleIssueGroup) {
	for i := range groups {
		sort.Strings(groups[i].Paths)
		if groups[i].RemoteBlocked != nil {
			sort.Strings(groups[i].RemoteBlocked.BlockedPaths)
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
