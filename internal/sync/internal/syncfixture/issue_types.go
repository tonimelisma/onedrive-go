package syncfixture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// IssueTypeConstantsInSource returns the value of every IssueType constant
// issue_types.go declares, read from the source, so a table that must name
// every member is checked against the declarations rather than against a
// second hand-kept list.
func IssueTypeConstantsInSource(t *testing.T) []syncdomain.IssueType {
	t.Helper()

	byName := IssueTypeConstantsByName(t)
	members := make([]syncdomain.IssueType, 0, len(byName))
	for _, member := range byName {
		members = append(members, member)
	}
	slices.Sort(members)

	return members
}

// IssueTypeConstantsByName maps the name of every IssueType constant
// issue_types.go declares to its value, read from the source, so a check
// that looks for the constants by name in other source files names exactly
// the declared ones.
func IssueTypeConstantsByName(t *testing.T) map[string]syncdomain.IssueType {
	t.Helper()

	members := make(map[string]syncdomain.IssueType)
	for name, value := range StringConstantsOfType(t, "issue_types.go", "IssueType") {
		members[name] = syncdomain.IssueType(value)
	}

	return members
}

// StringConstantsOfType maps the name of every constant of the string type
// typeName that file, a file of the vocabulary package syncdomain, declares
// to its value, read from the source.
func StringConstantsOfType(t *testing.T, file, typeName string) map[string]string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), vocabularySource(t, file), nil, parser.SkipObjectResolution)
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

// IssueTypeOutsideTheEnum is a value no IssueType member has. The stores
// refuse one, so in production only a programming error makes it; the tests
// use it to pin what each per-issue-type decision says about one. It is
// untyped: a package-level IssueType constant, even in a test file, is a
// member to the exhaustive linter.
const IssueTypeOutsideTheEnum = "not_an_issue_type"

// ConditionKeyOutsideTheSet is a value no ConditionKey constant has, untyped
// for the same reason.
const ConditionKeyOutsideTheSet = "not_a_condition_key"

// requireEveryConstantDecided checks a decision made per member of an enum
// against want, a table that must give every constant of the string type
// typeName that file declares its verdict and name nothing else. A new member
// fails the test until it is decided on purpose, here as well as in the
// decision's exhaustive switch.
func requireEveryConstantDecided[K ~string, V any](t *testing.T, file, typeName string, want map[K]V, decide func(K) V) {
	t.Helper()

	declared := StringConstantsOfType(t, file, typeName)
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

// RequireEveryIssueTypeDecided is requireEveryConstantDecided over the
// IssueType members issue_types.go declares.
func RequireEveryIssueTypeDecided[V any](t *testing.T, want map[syncdomain.IssueType]V, decide func(syncdomain.IssueType) V) {
	t.Helper()

	requireEveryConstantDecided(t, "issue_types.go", "IssueType", want, decide)
}

// verdictGroup is the members of a per-member decision that share one
// verdict.
type verdictGroup[K comparable, V any] struct {
	Verdict V
	Members []K
}

// IssueTypeVerdictGroup is a verdictGroup of issue types.
type IssueTypeVerdictGroup[V any] = verdictGroup[syncdomain.IssueType, V]

// verdictTable flattens groups into the table requireEveryConstantDecided
// reads, refusing a member two groups name.
func verdictTable[K comparable, V any](t *testing.T, groups ...verdictGroup[K, V]) map[K]V {
	t.Helper()

	want := make(map[K]V)
	for _, group := range groups {
		for _, member := range group.Members {
			_, named := want[member]
			require.False(t, named, "%v is in two verdict groups", member)
			want[member] = group.Verdict
		}
	}

	return want
}

// IssueTypeVerdicts is verdictTable over issue types.
func IssueTypeVerdicts[V any](t *testing.T, groups ...IssueTypeVerdictGroup[V]) map[syncdomain.IssueType]V {
	t.Helper()

	return verdictTable(t, groups...)
}

// vocabularySource is the path of file in syncdomain's directory, found from
// this file's own place in the tree, so tests of any package read the same
// declarations.
func vocabularySource(t *testing.T, file string) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "the fixture knows its own source path")

	return filepath.Join(filepath.Dir(self), "..", "..", "syncdomain", file)
}
