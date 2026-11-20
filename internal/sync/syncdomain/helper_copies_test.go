package syncdomain

// Private copies of test helpers whose home (syncfixture) these tests
// cannot import, because it imports this package or is another package's
// tests.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// funcDeclName names a function as the exceptions do: Type.Method for a
// method, the name alone otherwise.
func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if star, isStar := receiver.(*ast.StarExpr); isStar {
		receiver = star.X
	}
	if ident, isIdent := receiver.(*ast.Ident); isIdent {
		return ident.Name + "." + fn.Name.Name
	}

	return fn.Name.Name
}

// allCrashPoints is every crash point crash_points.go declares, each of which
// crashRecoveryScenario reaches. TestModel_CrashAtSeededPointsRecovers draws
// from it by index, so a new point goes last.
func allCrashPoints() []string {
	return []string{
		CrashPointRemoteMoveDone,
		CrashPointLocalMoveDone,
		CrashPointUploadCreatedUnrecorded,
		CrashPointUploadRemoteDone,
		CrashPointDownloadRenamed,
		CrashPointLocalDeleteTrashed,
		CrashPointRemoteDeleteDone,
		CrashPointCommitObservation,
		CrashPointCommitOutcome,
		CrashPointLocalMoveDestinationTrashed,
	}
}

func countOf(values []string, want string) int {
	n := 0
	for _, v := range values {
		if v == want {
			n++
		}
	}

	return n
}

// issueTypeConstantsInSource returns the value of every IssueType constant
// issue_types.go declares, read from the source, so a table that must name
// every member is checked against the declarations rather than against a
// second hand-kept list.
func issueTypeConstantsInSource(t *testing.T) []IssueType {
	t.Helper()

	byName := issueTypeConstantsByName(t)
	members := make([]IssueType, 0, len(byName))
	for _, member := range byName {
		members = append(members, member)
	}
	slices.Sort(members)

	return members
}

// issueTypeConstantsByName maps the name of every IssueType constant
// issue_types.go declares to its value, read from the source, so a check
// that looks for the constants by name in other source files names exactly
// the declared ones.
func issueTypeConstantsByName(t *testing.T) map[string]IssueType {
	t.Helper()

	members := make(map[string]IssueType)
	for name, value := range stringConstantsOfType(t, "issue_types.go", "IssueType") {
		members[name] = IssueType(value)
	}

	return members
}

// stringConstantsOfType maps the name of every constant of the string type
// typeName that file declares to its value, read from the source.
func stringConstantsOfType(t *testing.T, file, typeName string) map[string]string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	constants := make(map[string]string)
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			declaredType, ok := value.Type.(*ast.Ident)
			if !ok || declaredType.Name != typeName {
				continue
			}
			for i := range value.Values {
				lit, ok := value.Values[i].(*ast.BasicLit)
				require.True(t, ok, "%s constant %s is not a string literal", typeName, value.Names[i].Name)
				unquoted, unquoteErr := strconv.Unquote(lit.Value)
				require.NoError(t, unquoteErr)
				constants[value.Names[i].Name] = unquoted
			}
		}
	}
	require.NotEmpty(t, constants, "%s declares no %s constant", file, typeName)

	return constants
}

// issueTypeOutsideTheEnum is a value no IssueType member has. The stores
// refuse one, so in production only a programming error makes it; the tests
// use it to pin what each per-issue-type decision says about one. It is
// untyped: a package-level IssueType constant, even in a test file, is a
// member to the exhaustive linter.
const issueTypeOutsideTheEnum = "not_an_issue_type"

// conditionKeyOutsideTheSet is a value no ConditionKey constant has, untyped
// for the same reason.
const conditionKeyOutsideTheSet = "not_a_condition_key"

// requireEveryConstantDecided checks a decision made per member of an enum
// against want, a table that must give every constant of the string type
// typeName that file declares its verdict and name nothing else. A new member
// fails the test until it is decided on purpose, here as well as in the
// decision's exhaustive switch.
func requireEveryConstantDecided[K ~string, V any](t *testing.T, file, typeName string, want map[K]V, decide func(K) V) {
	t.Helper()

	declared := stringConstantsOfType(t, file, typeName)
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)

	require.Len(t, want, len(declared), "the table must give every %s member a verdict and name nothing else", typeName)
	for _, name := range names {
		member := K(declared[name])
		verdict, ok := want[member]
		require.True(t, ok, "no verdict for %s (%q)", name, member)
		assert.Equal(t, verdict, decide(member), "verdict for %s (%q)", name, member)
	}
}

// requireEveryIssueTypeDecided is requireEveryConstantDecided over the
// IssueType members issue_types.go declares.
func requireEveryIssueTypeDecided[V any](t *testing.T, want map[IssueType]V, decide func(IssueType) V) {
	t.Helper()

	requireEveryConstantDecided(t, "issue_types.go", "IssueType", want, decide)
}

// verdictGroup is the members of a per-member decision that share one
// verdict.
type verdictGroup[K comparable, V any] struct {
	verdict V
	members []K
}

// issueTypeVerdictGroup is a verdictGroup of issue types.
type issueTypeVerdictGroup[V any] = verdictGroup[IssueType, V]

// verdictTable flattens groups into the table requireEveryConstantDecided
// reads, refusing a member two groups name.
func verdictTable[K comparable, V any](t *testing.T, groups ...verdictGroup[K, V]) map[K]V {
	t.Helper()

	want := make(map[K]V)
	for _, group := range groups {
		for _, member := range group.members {
			_, named := want[member]
			require.False(t, named, "%v is in two verdict groups", member)
			want[member] = group.verdict
		}
	}

	return want
}

// issueTypeVerdicts is verdictTable over issue types.
func issueTypeVerdicts[V any](t *testing.T, groups ...issueTypeVerdictGroup[V]) map[IssueType]V {
	t.Helper()

	return verdictTable(t, groups...)
}

func kindNames[K interface{ String() string }](kinds []K) []string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, kind.String())
	}

	return names
}
