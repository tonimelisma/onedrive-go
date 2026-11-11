package sync

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The comparison CASE tests 17 two-valued inputs and the reconciliation CASE
// nine flags besides its kind and three item types; the exhaustive stages
// enumerate every combination of them.
const (
	comparisonCaseInputCount     = 17
	reconciliationCaseFlagCount  = 9
	reconciliationItemTypeCount  = 3 // file, folder, root
	reconciliationSideTypeCount  = 4 // absent (''), file, folder, root
	comparisonCaseStateCount     = 1 << comparisonCaseInputCount
	reconciliationCaseFlagStates = 1 << reconciliationCaseFlagCount
)

// comparisonCaseInputs are the columns sqlComparisonKindCase reads, in the
// bit order the stage 1 enumeration assigns them. A text column holds the
// empty string for 0 and a path for 1, which is all its "not empty" test
// distinguishes.
func comparisonCaseInputs() []struct {
	column string
	isText bool
} {
	return []struct {
		column string
		isText bool
	}{
		{column: "baseline_present"},
		{column: "local_present"},
		{column: "remote_present"},
		{column: "local_move_candidate_count"},
		{column: "local_move_target", isText: true},
		{column: "local_move_source", isText: true},
		{column: "remote_move_target", isText: true},
		{column: "remote_move_source", isText: true},
		{column: "local_item_replaced"},
		{column: "delete_awaits_ancestor_move"},
		{column: "local_copy_awaits_ancestor_move"},
		{column: "remote_create_awaits_ancestor_move"},
		{column: "kept_local_folder"},
		{column: "remote_move_shadowed"},
		{column: "current_equal"},
		{column: "local_changed"},
		{column: "remote_changed"},
	}
}

// reconciliationCaseFlags are the flags sqlReconciliationKindCase reads
// besides comparison_kind and the three item types.
func reconciliationCaseFlags() []string {
	return []string{
		"local_changed",
		"remote_changed",
		"current_equal",
		"local_subtree_changed",
		"remote_subtree_changed",
		"local_type_changed",
		"remote_type_changed",
		"remote_item_replaced",
		"metadata_drift",
	}
}

func kindNames[K interface{ String() string }](kinds []K) []string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, kind.String())
	}

	return names
}

// sqlLiterals returns the distinct first capture of pattern in sqlText.
func sqlLiterals(sqlText, pattern string) []string {
	var literals []string
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(sqlText, -1) {
		if !slices.Contains(literals, match[1]) {
			literals = append(literals, match[1])
		}
	}

	return literals
}

// Validates: R-6.2.16
//
// The enums are the kind vocabulary and the SQL CASE expressions carry a copy
// of it as literals: every literal must be a member, every member a literal,
// and each CASE's fallback the unclassified member.
func TestPlannerKindCases_LiteralsAreExactlyTheKinds(t *testing.T) {
	t.Parallel()

	comparisonNames := kindNames(allComparisonKinds())
	reconciliationNames := kindNames(allReconciliationKinds())
	var testedComparisonNames []string
	for _, kind := range allComparisonKinds() {
		if kind != comparisonUnclassified {
			testedComparisonNames = append(testedComparisonNames, kind.String())
		}
	}

	assert.ElementsMatch(t, comparisonNames, sqlLiterals(sqlComparisonKindCase, `(?:THEN|ELSE) '([a-z_]+)'`))
	assert.ElementsMatch(t, reconciliationNames, sqlLiterals(sqlReconciliationKindCase, `(?:THEN|ELSE) '([a-z_]+)'`))
	assert.ElementsMatch(t, testedComparisonNames, sqlLiterals(sqlReconciliationKindCase, `comparison_kind = '([a-z_]+)'`),
		"the reconciliation CASE must test every classified comparison kind")
	assert.Equal(t, []string{comparisonUnclassified.String()}, sqlLiterals(sqlComparisonKindCase, `ELSE '([a-z_]+)'`))
	assert.Equal(t, []string{reconcileUnclassified.String()}, sqlLiterals(sqlReconciliationKindCase, `ELSE '([a-z_]+)'`))
}

// Validates: R-6.2.16
func TestPlannerKinds_ParseRoundTripsEveryMemberAndRefusesOthers(t *testing.T) {
	t.Parallel()

	comparisons := allComparisonKinds()
	assert.Equal(t, comparisonUnclassified, comparisons[0], "the zero value is the unclassified member")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(kindNames(comparisons)))), len(comparisons))
	for _, kind := range comparisons {
		parsed, err := parseComparisonKind(kind.String())
		require.NoError(t, err)
		assert.Equal(t, kind, parsed)
	}
	_, err := parseComparisonKind("bogus")
	require.Error(t, err)

	reconciliations := allReconciliationKinds()
	assert.Equal(t, reconcileUnclassified, reconciliations[0], "the zero value is the unclassified member")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(kindNames(reconciliations)))), len(reconciliations))
	for _, kind := range reconciliations {
		parsed, parseErr := parseReconciliationKind(kind.String())
		require.NoError(t, parseErr)
		assert.Equal(t, kind, parsed)
	}
	_, err = parseReconciliationKind("bogus")
	require.Error(t, err)
}

// Validates: R-6.2.16
//
// A kind the enum does not know means the SQL and the Go vocabulary drifted
// apart: the scan fails and names the path.
func TestQueryPlannerRows_UnknownKindFailsTheScanNamingThePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := queryComparisonRows(ctx, store.rawDB(),
		`SELECT 'docs/compared.txt', '', '', 'file', 1, 1, 1, 0, 0, 0, '', '', 0, '', '', 'bogus'`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/compared.txt"`)
	require.ErrorContains(t, err, `"bogus"`)

	_, err = queryReconciliationRows(ctx, store.rawDB(),
		`SELECT 'docs/compared.txt', 'file', 'bogus', 'noop', '', '', '', ''`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/compared.txt"`)

	_, err = queryReconciliationRows(ctx, store.rawDB(),
		`SELECT 'docs/reconciled.txt', 'file', 'unchanged', 'bogus', '', '', '', ''`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/reconciled.txt"`)
	require.ErrorContains(t, err, `"bogus"`)
}

// comparisonCaseGroup is one group of the stage 1 enumeration.
type comparisonCaseGroup struct {
	kind          comparisonKind
	anySide       bool
	localChanged  bool
	remoteChanged bool
	currentEqual  bool
	states        int
}

// Validates: R-6.2.16
//
// Stage 1: sqlComparisonKindCase over all 131,072 combinations of its inputs.
// Only the combination with no side present, which no path can have (the row
// set is the union of the three tables' paths), is unclassified. The diverged
// facts the reconciliation CASE relies on hold too: a diverged row changed on
// some side, and never has equal content after a remote change.
//
// Exhaustive (a few seconds, minutes under -race): the targeted tests of the
// decision table cover single states; this proves every one.
func TestComparisonKindCase_ClassifiesEveryReachableState(t *testing.T) {
	skipInShort(t)
	t.Parallel()

	inputs := comparisonCaseInputs()
	require.Len(t, inputs, comparisonCaseInputCount)
	columns := make([]string, 0, len(inputs))
	for bit, input := range inputs {
		if input.isText {
			columns = append(columns, fmt.Sprintf("CASE WHEN (i >> %d) & 1 = 1 THEN 'p' ELSE '' END AS %s", bit, input.column))
			continue
		}
		columns = append(columns, fmt.Sprintf("(i >> %d) & 1 AS %s", bit, input.column))
	}
	query := fmt.Sprintf(`
WITH RECURSIVE states(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM states WHERE i + 1 < %d),
inputs AS (SELECT %s FROM states),
classified AS (SELECT *, %s AS comparison_kind FROM inputs)
SELECT comparison_kind, baseline_present + local_present + remote_present > 0,
	local_changed, remote_changed, current_equal, COUNT(*)
FROM classified
GROUP BY 1, 2, 3, 4, 5`, comparisonCaseStateCount, strings.Join(columns, ",\n\t"), sqlComparisonKindCase)

	groups := queryComparisonCaseGroups(t, query)
	total := 0
	produced := map[comparisonKind]bool{}
	var violations []string
	for _, g := range groups {
		total += g.states
		produced[g.kind] = true
		if (g.kind == comparisonUnclassified) == g.anySide {
			violations = append(violations, fmt.Sprintf("%s with a side present: %t", g.kind, g.anySide))
		}
		changedNowhere := !g.localChanged && !g.remoteChanged
		equalAfterRemoteChange := g.currentEqual && g.remoteChanged
		if g.kind == comparisonDiverged && (changedNowhere || equalAfterRemoteChange) {
			violations = append(violations, fmt.Sprintf("diverged with local_changed=%t remote_changed=%t current_equal=%t",
				g.localChanged, g.remoteChanged, g.currentEqual))
		}
	}
	assert.Equal(t, comparisonCaseStateCount, total)
	assert.Empty(t, violations)
	for _, kind := range allComparisonKinds() {
		assert.True(t, produced[kind], "no input state yields %s", kind)
	}
}

func queryComparisonCaseGroups(t *testing.T, query string) []comparisonCaseGroup {
	t.Helper()

	rows, err := newTestStore(t).rawDB().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	var groups []comparisonCaseGroup
	for rows.Next() {
		var (
			g    comparisonCaseGroup
			kind string
		)
		require.NoError(t, rows.Scan(&kind, &g.anySide, &g.localChanged, &g.remoteChanged, &g.currentEqual, &g.states))
		g.kind, err = parseComparisonKind(kind)
		require.NoError(t, err)
		groups = append(groups, g)
	}
	require.NoError(t, rows.Err())

	return groups
}

// Validates: R-6.2.16
//
// Stage 2: sqlReconciliationKindCase over 18 comparison kinds, 3 item types,
// 4 local and 4 remote item types and 512 flag combinations (442,368 inputs).
// A classified comparison kind always gets a reconciliation, except a diverged
// row that changed on neither side, which stage 1 proves never exists (the
// product is synthetic, so this direction only). An unclassified comparison is
// never reconciled into a decision, and every reconciliation kind is reachable.
//
// Exhaustive (several seconds, minutes under -race), like stage 1.
func TestReconciliationKindCase_ReconcilesEveryClassifiedComparison(t *testing.T) {
	skipInShort(t)
	t.Parallel()

	flags := reconciliationCaseFlags()
	require.Len(t, flags, reconciliationCaseFlagCount)
	flagColumns := make([]string, 0, len(flags))
	for bit, flag := range flags {
		flagColumns = append(flagColumns, fmt.Sprintf("(f.i >> %d) & 1 AS %s", bit, flag))
	}
	kindValues := make([]string, 0, len(allComparisonKinds()))
	for _, kind := range allComparisonKinds() {
		kindValues = append(kindValues, fmt.Sprintf("('%s')", kind))
	}
	query := fmt.Sprintf(`
WITH RECURSIVE flags(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM flags WHERE i + 1 < %d),
kinds(comparison_kind) AS (VALUES %s),
item_types(item_type) AS (VALUES ('file'), ('folder'), ('root')),
side_types(item_type) AS (VALUES (''), ('file'), ('folder'), ('root')),
inputs AS (
	SELECT k.comparison_kind, it.item_type, lt.item_type AS local_item_type, rt.item_type AS remote_item_type,
		%s
	FROM kinds k CROSS JOIN item_types it CROSS JOIN side_types lt CROSS JOIN side_types rt CROSS JOIN flags f
),
classified AS (SELECT *, %s AS reconciliation_kind FROM inputs)
SELECT comparison_kind, reconciliation_kind, local_changed, remote_changed, COUNT(*)
FROM classified
GROUP BY 1, 2, 3, 4`, reconciliationCaseFlagStates, strings.Join(kindValues, ", "),
		strings.Join(flagColumns, ",\n\t\t"), sqlReconciliationKindCase)

	rows, err := newTestStore(t).rawDB().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	total := 0
	produced := map[reconciliationKind]bool{}
	var violations []string
	for rows.Next() {
		var (
			comparisonName, reconciliationName string
			localChanged, remoteChanged        bool
			states                             int
		)
		require.NoError(t, rows.Scan(&comparisonName, &reconciliationName, &localChanged, &remoteChanged, &states))
		comparison, parseErr := parseComparisonKind(comparisonName)
		require.NoError(t, parseErr)
		reconciliation, parseErr := parseReconciliationKind(reconciliationName)
		require.NoError(t, parseErr)
		total += states
		produced[reconciliation] = true

		unreachableDiverged := comparison == comparisonDiverged && !localChanged && !remoteChanged
		if reconciliation == reconcileUnclassified && comparison != comparisonUnclassified && !unreachableDiverged {
			violations = append(violations, fmt.Sprintf("%s (local_changed=%t remote_changed=%t) is unclassified",
				comparison, localChanged, remoteChanged))
		}
		if comparison == comparisonUnclassified && reconciliation != reconcileUnclassified {
			violations = append(violations, fmt.Sprintf("an unclassified comparison was reconciled as %s", reconciliation))
		}
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, len(allComparisonKinds())*reconciliationItemTypeCount*
		reconciliationSideTypeCount*reconciliationSideTypeCount*reconciliationCaseFlagStates, total)
	assert.Empty(t, violations)
	for _, kind := range allReconciliationKinds() {
		assert.True(t, produced[kind], "no input yields %s", kind)
	}
}

// Validates: R-6.2.16
//
// The refusal runs before any per-row policy: an unclassified row at a path
// whose local truth is unavailable, which the suppression would otherwise
// plan as nothing (the classified control below), still fails the plan, and
// the error names the first such path and counts the others.
func TestPlanCurrentState_UnclassifiedRowFailsEvenWhereTruthIsUnavailable(t *testing.T) {
	t.Parallel()

	plan := func(kind reconciliationKind) (*actionPlan, error) {
		return newPlanner(testLogger(t)).PlanCurrentState(
			[]sQLiteComparisonRow{
				{Path: "docs/a.txt", LocalPresent: true, ComparisonKind: comparisonLocalOnlyCreate},
				{Path: "docs/b.txt", LocalPresent: true, ComparisonKind: comparisonLocalOnlyCreate},
			},
			[]sQLiteReconciliationRow{
				{Path: "docs/a.txt", ItemType: ItemTypeFile, ComparisonKind: comparisonLocalOnlyCreate, ReconciliationKind: kind},
				{Path: "docs/b.txt", ItemType: ItemTypeFile, ComparisonKind: comparisonLocalOnlyCreate, ReconciliationKind: kind},
			},
			[]localStateRow{
				{Path: "docs/a.txt", ItemType: ItemTypeFile, Hash: "local-a"},
				{Path: "docs/b.txt", ItemType: ItemTypeFile, Hash: "local-b"},
			},
			nil,
			[]ObservationIssueRow{
				{Path: "docs/a.txt", IssueType: issueLocalReadDenied},
				{Path: "docs/b.txt", IssueType: issueLocalReadDenied},
			},
			NewBaselineForTest(nil),
			plannerMountContext{},
			SyncBidirectional,
		)
	}

	suppressed, err := plan(reconcileUpload)
	require.NoError(t, err)
	assert.Empty(t, suppressed.Actions, "the control: unavailable truth suppresses the classified rows")

	_, err = plan(reconcileUnclassified)
	require.ErrorIs(t, err, errUnclassifiedPlan)
	var unclassified *unclassifiedPlanError
	require.ErrorAs(t, err, &unclassified)
	assert.Equal(t, "docs/a.txt", unclassified.Path)
	assert.Equal(t, comparisonLocalOnlyCreate, unclassified.Comparison)
	assert.Equal(t, 1, unclassified.Others)
	assert.EqualError(t, err, `sync: planner state is outside the decision table: "docs/a.txt" `+
		`(comparison local_only_create, reconciliation unclassified) and 1 more path`)
}

// Validates: R-6.2.16
//
// A comparison row the planner is handed without a matching classified
// reconciliation fails the plan too: move composition reads comparison kinds
// directly.
func TestPlanCurrentState_UnclassifiedComparisonRowFailsThePlan(t *testing.T) {
	t.Parallel()

	_, err := newPlanner(testLogger(t)).PlanCurrentState(
		[]sQLiteComparisonRow{{Path: "docs/a.txt", LocalPresent: true}},
		nil,
		[]localStateRow{{Path: "docs/a.txt", ItemType: ItemTypeFile, Hash: "local-a"}},
		nil,
		nil,
		NewBaselineForTest(nil),
		plannerMountContext{},
		SyncBidirectional,
	)
	require.ErrorIs(t, err, errUnclassifiedPlan)
	require.ErrorContains(t, err, `"docs/a.txt"`)
}

// Validates: R-2.2.6, R-6.2.16
//
// A folder planned as a content transfer is a comparison bug. It used to be
// logged and dropped, a path that silently stopped syncing; it now fails the
// plan, naming the path.
func TestPlanCurrentState_FolderTransferFailsThePlan(t *testing.T) {
	t.Parallel()

	_, err := newPlanner(testLogger(t)).PlanCurrentState(
		[]sQLiteComparisonRow{{Path: "local-dir", LocalPresent: true, ComparisonKind: comparisonLocalOnlyCreate}},
		[]sQLiteReconciliationRow{{
			Path:               "local-dir",
			ItemType:           ItemTypeFolder,
			ComparisonKind:     comparisonLocalOnlyCreate,
			ReconciliationKind: reconcileUpload,
		}},
		[]localStateRow{{Path: "local-dir", ItemType: ItemTypeFolder}},
		nil,
		nil,
		NewBaselineForTest(nil),
		plannerMountContext{},
		SyncBidirectional,
	)
	require.ErrorIs(t, err, errFolderTransferPlanned)
	require.ErrorContains(t, err, "local-dir")
}
