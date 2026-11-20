package syncstore

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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

	comparisonNames := syncfixture.KindNames(syncdomain.AllComparisonKinds())
	reconciliationNames := syncfixture.KindNames(syncdomain.AllReconciliationKinds())
	var testedComparisonNames []string
	for _, kind := range syncdomain.AllComparisonKinds() {
		if kind != syncdomain.ComparisonUnclassified {
			testedComparisonNames = append(testedComparisonNames, kind.String())
		}
	}

	assert.ElementsMatch(t, comparisonNames, sqlLiterals(sqlComparisonKindCase, `(?:THEN|ELSE) '([a-z_]+)'`))
	assert.ElementsMatch(t, reconciliationNames, sqlLiterals(sqlReconciliationKindCase, `(?:THEN|ELSE) '([a-z_]+)'`))
	assert.ElementsMatch(t, testedComparisonNames, sqlLiterals(sqlReconciliationKindCase, `comparison_kind = '([a-z_]+)'`),
		"the reconciliation CASE must test every classified comparison kind")
	assert.Equal(t, []string{syncdomain.ComparisonUnclassified.String()}, sqlLiterals(sqlComparisonKindCase, `ELSE '([a-z_]+)'`))
	assert.Equal(t, []string{syncdomain.ReconcileUnclassified.String()}, sqlLiterals(sqlReconciliationKindCase, `ELSE '([a-z_]+)'`))
}

// Validates: R-6.2.16
//
// A kind the enum does not know means the SQL and the Go vocabulary drifted
// apart: the scan fails and names the path.
func TestQueryPlannerRows_UnknownKindFailsTheScanNamingThePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := queryComparisonRows(ctx, store.RawDBForTest(),
		`SELECT 'docs/compared.txt', '', '', 'file', 1, 1, 1, 0, 0, 0, '', '', 0, '', '',
			'file', 'file', 'file', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 'bogus'`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/compared.txt"`)
	require.ErrorContains(t, err, `"bogus"`)

	_, err = QueryReconciliationRows(ctx, store.RawDBForTest(),
		`SELECT 'docs/compared.txt', 'file', 'bogus', 'noop', '', '', '', ''`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/compared.txt"`)

	_, err = QueryReconciliationRows(ctx, store.RawDBForTest(),
		`SELECT 'docs/reconciled.txt', 'file', 'unchanged', 'bogus', '', '', '', ''`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/reconciled.txt"`)
	require.ErrorContains(t, err, `"bogus"`)
}

// comparisonCaseGroup is one group of the stage 1 enumeration.
type comparisonCaseGroup struct {
	kind          syncdomain.ComparisonKind
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
// some side, and never has equal content, whichever side changed.
//
// Exhaustive (a few seconds, minutes under -race): the targeted tests of the
// decision table cover single states; this proves every one.
func TestComparisonKindCase_ClassifiesEveryReachableState(t *testing.T) {
	syncfixture.SkipInShort(t)
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
	produced := map[syncdomain.ComparisonKind]bool{}
	var violations []string
	for _, g := range groups {
		total += g.states
		produced[g.kind] = true
		if (g.kind == syncdomain.ComparisonUnclassified) == g.anySide {
			violations = append(violations, fmt.Sprintf("%s with a side present: %t", g.kind, g.anySide))
		}
		changedNowhere := !g.localChanged && !g.remoteChanged
		if g.kind == syncdomain.ComparisonDiverged && (changedNowhere || g.currentEqual) {
			violations = append(violations, fmt.Sprintf("diverged with local_changed=%t remote_changed=%t current_equal=%t",
				g.localChanged, g.remoteChanged, g.currentEqual))
		}
	}
	assert.Equal(t, comparisonCaseStateCount, total)
	assert.Empty(t, violations)
	for _, kind := range syncdomain.AllComparisonKinds() {
		assert.True(t, produced[kind], "no input state yields %s", kind)
	}
}

func queryComparisonCaseGroups(t *testing.T, query string) []comparisonCaseGroup {
	t.Helper()

	rows, err := newTestStore(t).RawDBForTest().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	var groups []comparisonCaseGroup
	for rows.Next() {
		var (
			g    comparisonCaseGroup
			kind string
		)
		require.NoError(t, rows.Scan(&kind, &g.anySide, &g.localChanged, &g.remoteChanged, &g.currentEqual, &g.states))
		g.kind, err = syncdomain.ParseComparisonKind(kind)
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
// product is synthetic, so this direction only), and a create of a
// root-typed entry, which no observer records and planning refuses. An unclassified comparison is
// never reconciled into a decision, and every reconciliation kind is reachable.
//
// Exhaustive (several seconds, minutes under -race), like stage 1.
func TestReconciliationKindCase_ReconcilesEveryClassifiedComparison(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	flags := reconciliationCaseFlags()
	require.Len(t, flags, reconciliationCaseFlagCount)
	flagColumns := make([]string, 0, len(flags))
	for bit, flag := range flags {
		flagColumns = append(flagColumns, fmt.Sprintf("(f.i >> %d) & 1 AS %s", bit, flag))
	}
	kindValues := make([]string, 0, len(syncdomain.AllComparisonKinds()))
	for _, kind := range syncdomain.AllComparisonKinds() {
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
SELECT comparison_kind, reconciliation_kind, local_changed, remote_changed, item_type = 'root', COUNT(*)
FROM classified
GROUP BY 1, 2, 3, 4, 5`, reconciliationCaseFlagStates, strings.Join(kindValues, ", "),
		strings.Join(flagColumns, ",\n\t\t"), sqlReconciliationKindCase)

	rows, err := newTestStore(t).RawDBForTest().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	total := 0
	produced := map[syncdomain.ReconciliationKind]bool{}
	var violations []string
	for rows.Next() {
		var (
			comparisonName, reconciliationName string
			localChanged, remoteChanged, root  bool
			states                             int
		)
		require.NoError(t, rows.Scan(&comparisonName, &reconciliationName, &localChanged, &remoteChanged, &root, &states))
		comparison, parseErr := syncdomain.ParseComparisonKind(comparisonName)
		require.NoError(t, parseErr)
		reconciliation, parseErr := syncdomain.ParseReconciliationKind(reconciliationName)
		require.NoError(t, parseErr)
		total += states
		produced[reconciliation] = true

		unreachableDiverged := comparison == syncdomain.ComparisonDiverged && !localChanged && !remoteChanged
		rootCreate := root && (comparison == syncdomain.ComparisonLocalOnlyCreate || comparison == syncdomain.ComparisonRemoteOnlyCreate)
		if reconciliation == syncdomain.ReconcileUnclassified && comparison != syncdomain.ComparisonUnclassified &&
			!unreachableDiverged && !rootCreate {
			violations = append(violations, fmt.Sprintf("%s (local_changed=%t remote_changed=%t) is unclassified",
				comparison, localChanged, remoteChanged))
		}
		if comparison == syncdomain.ComparisonUnclassified && reconciliation != syncdomain.ReconcileUnclassified {
			violations = append(violations, fmt.Sprintf("an unclassified comparison was reconciled as %s", reconciliation))
		}
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, len(syncdomain.AllComparisonKinds())*reconciliationItemTypeCount*
		reconciliationSideTypeCount*reconciliationSideTypeCount*reconciliationCaseFlagStates, total)
	assert.Empty(t, violations)
	for _, kind := range syncdomain.AllReconciliationKinds() {
		assert.True(t, produced[kind], "no input yields %s", kind)
	}
}

// stageOneState is the decision state of stage 1 input number i: each bit
// in comparisonCaseInputs order, a present side as a file (stage 1 reads
// presence only), and the move booleans formed from the candidate count as
// the CASE tests them (count = 1 together with a target or a source).
func stageOneState(i int) syncdomain.DecisionState {
	bit := func(n int) bool { return i>>n&1 == 1 }
	side := func(n int) syncdomain.SideType {
		if bit(n) {
			return syncdomain.SideFile
		}
		return syncdomain.SideAbsent
	}
	const (
		bitCount = 3 + iota
		bitLocalMoveTarget
		bitLocalMoveSource
		bitRemoteMoveTarget
		bitRemoteMoveSource
		bitLocalItemReplaced
		bitDeleteAwaits
		bitLocalCopyAwaits
		bitRemoteCreateAwaits
		bitKept
		bitShadowed
		bitCurrentEqual
		bitLocalChanged
		bitRemoteChanged
	)

	return syncdomain.DecisionState{
		Baseline: side(0), Local: side(1), Remote: side(2),
		LocalMoveSource:                bit(bitCount) && bit(bitLocalMoveTarget),
		LocalMoveDest:                  bit(bitCount) && bit(bitLocalMoveSource),
		RemoteMoveSource:               bit(bitRemoteMoveTarget),
		RemoteMoveDest:                 bit(bitRemoteMoveSource),
		LocalItemReplaced:              bit(bitLocalItemReplaced),
		DeleteAwaitsAncestorMove:       bit(bitDeleteAwaits),
		LocalCopyAwaitsAncestorMove:    bit(bitLocalCopyAwaits),
		RemoteCreateAwaitsAncestorMove: bit(bitRemoteCreateAwaits),
		KeptLocalFolder:                bit(bitKept),
		RemoteMoveShadowed:             bit(bitShadowed),
		CurrentEqual:                   bit(bitCurrentEqual),
		LocalChanged:                   bit(bitLocalChanged),
		RemoteChanged:                  bit(bitRemoteChanged),
	}
}

// Validates: R-6.2.16
//
// The Go decision table is the SQL one, stage 1: over all 131,072
// combinations of sqlComparisonKindCase's inputs, Decide's comparison kind
// is the kind the CASE yields.
func TestDecide_ComparisonMatchesTheSQLCaseOnEveryInput(t *testing.T) {
	syncfixture.SkipInShort(t)
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
inputs AS (SELECT i, %s FROM states)
SELECT i, %s FROM inputs ORDER BY i`, comparisonCaseStateCount, strings.Join(columns, ",\n\t"), sqlComparisonKindCase)

	rows, err := newTestStore(t).RawDBForTest().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	compared := 0
	var disagreements []string
	for rows.Next() {
		var (
			i    int
			name string
		)
		require.NoError(t, rows.Scan(&i, &name))
		sqlKind, parseErr := syncdomain.ParseComparisonKind(name)
		require.NoError(t, parseErr)
		compared++
		if goKind := syncdomain.Decide(stageOneState(i)).Comparison; goKind != sqlKind && len(disagreements) < 10 {
			disagreements = append(disagreements, fmt.Sprintf("input %d: SQL %s, Go %s", i, sqlKind, goKind))
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, comparisonCaseStateCount, compared)
	assert.Empty(t, disagreements)
}

// Validates: R-6.2.16
//
// Stage 2: over all 442,368 inputs of sqlReconciliationKindCase (every
// comparison kind, item type, local and remote side type and flag
// combination), Reconcile yields the kind the CASE yields.
func TestReconcile_MatchesTheSQLCaseOnEveryInput(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	flags := reconciliationCaseFlags()
	require.Len(t, flags, reconciliationCaseFlagCount)
	flagColumns := make([]string, 0, len(flags))
	for bit, flag := range flags {
		flagColumns = append(flagColumns, fmt.Sprintf("(f.i >> %d) & 1 AS %s", bit, flag))
	}
	kindValues := make([]string, 0, len(syncdomain.AllComparisonKinds()))
	for _, kind := range syncdomain.AllComparisonKinds() {
		kindValues = append(kindValues, fmt.Sprintf("('%s')", kind))
	}
	query := fmt.Sprintf(`
WITH RECURSIVE flags(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM flags WHERE i + 1 < %d),
kinds(comparison_kind) AS (VALUES %s),
item_types(item_type) AS (VALUES ('file'), ('folder'), ('root')),
side_types(item_type) AS (VALUES (''), ('file'), ('folder'), ('root')),
inputs AS (
	SELECT k.comparison_kind, it.item_type, lt.item_type AS local_item_type, rt.item_type AS remote_item_type,
		f.i AS flag_bits, %s
	FROM kinds k CROSS JOIN item_types it CROSS JOIN side_types lt CROSS JOIN side_types rt CROSS JOIN flags f
)
SELECT comparison_kind, item_type, local_item_type, remote_item_type, flag_bits, %s FROM inputs`,
		reconciliationCaseFlagStates, strings.Join(kindValues, ", "),
		strings.Join(flagColumns, ",\n\t\t"), sqlReconciliationKindCase)

	rows, err := newTestStore(t).RawDBForTest().QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()

	compared := 0
	var disagreements []string
	for rows.Next() {
		var (
			kindName, itemType, localType, remoteType, reconciliationName string
			bits                                                          int
		)
		require.NoError(t, rows.Scan(&kindName, &itemType, &localType, &remoteType, &bits, &reconciliationName))
		in, kind, sqlKind := reconcileCaseInput(t, kindName, itemType, localType, remoteType, bits, reconciliationName)
		compared++
		if goKind := syncdomain.Reconcile(kind, in); goKind != sqlKind && len(disagreements) < 10 {
			disagreements = append(disagreements, fmt.Sprintf("%s %+v: SQL %s, Go %s", kind, in, sqlKind, goKind))
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, len(syncdomain.AllComparisonKinds())*reconciliationItemTypeCount*
		reconciliationSideTypeCount*reconciliationSideTypeCount*reconciliationCaseFlagStates, compared)
	assert.Empty(t, disagreements)
}

// reconcileCaseInput parses one stage 2 input row: the flags from bits in
// reconciliationCaseFlags order.
func reconcileCaseInput(
	t *testing.T,
	kindName, itemType, localType, remoteType string,
	bits int,
	reconciliationName string,
) (syncdomain.ReconcileInputs, syncdomain.ComparisonKind, syncdomain.ReconciliationKind) {
	t.Helper()

	bit := func(n int) bool { return bits>>n&1 == 1 }
	kind, err := syncdomain.ParseComparisonKind(kindName)
	require.NoError(t, err)
	reconciliation, err := syncdomain.ParseReconciliationKind(reconciliationName)
	require.NoError(t, err)
	side := func(name string) syncdomain.SideType {
		parsed, parseErr := syncdomain.ParseSideType(name)
		require.NoError(t, parseErr)
		return parsed
	}
	const (
		bitLocalChanged = iota
		bitRemoteChanged
		bitCurrentEqual // read by no rule; an input dimension all the same
		bitLocalSubtreeChanged
		bitRemoteSubtreeChanged
		bitLocalTypeChanged
		bitRemoteTypeChanged
		bitRemoteItemReplaced
		bitMetadataDrift
	)

	return syncdomain.ReconcileInputs{
		ItemType:             side(itemType),
		Local:                side(localType),
		Remote:               side(remoteType),
		LocalChanged:         bit(bitLocalChanged),
		RemoteChanged:        bit(bitRemoteChanged),
		LocalSubtreeChanged:  bit(bitLocalSubtreeChanged),
		RemoteSubtreeChanged: bit(bitRemoteSubtreeChanged),
		LocalTypeChanged:     bit(bitLocalTypeChanged),
		RemoteTypeChanged:    bit(bitRemoteTypeChanged),
		RemoteItemReplaced:   bit(bitRemoteItemReplaced),
		MetadataDrift:        bit(bitMetadataDrift),
	}, kind, reconciliation
}
