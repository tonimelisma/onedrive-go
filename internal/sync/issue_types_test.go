package sync

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

// Validates: R-2.5.6
func TestIssueType_EveryDeclaredConstantIsAMemberAndParses(t *testing.T) {
	t.Parallel()

	members := issueTypeConstantsInSource(t)
	seen := make(map[IssueType]struct{}, len(members))
	for _, member := range members {
		_, duplicate := seen[member]
		assert.False(t, duplicate, "two IssueType constants share the value %q", member)
		seen[member] = struct{}{}

		assert.True(t, member.isMember(), "%q is declared but isMember does not name it", member)
		parsed, err := parseIssueType(string(member))
		require.NoError(t, err)
		assert.Equal(t, member, parsed)
	}
}

// Validates: R-2.5.6
func TestParseIssueType_RefusesAValueNoMemberHas(t *testing.T) {
	t.Parallel()

	for _, stored := range []string{"", "invalid_name", "Invalid_Filename", "hash_error"} {
		parsed, err := parseIssueType(stored)
		require.ErrorContains(t, err, "unknown issue type "+strconv.Quote(stored))
		assert.Empty(t, parsed)
		assert.False(t, IssueType(stored).isMember(), "%q", stored)
	}
}
