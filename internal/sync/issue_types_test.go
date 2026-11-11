package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
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

	parsed, err := parser.ParseFile(token.NewFileSet(), "issue_types.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	var members []IssueType
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
			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "IssueType" {
				continue
			}
			for i := range value.Values {
				lit, ok := value.Values[i].(*ast.BasicLit)
				require.True(t, ok, "IssueType constant %s is not a string literal", value.Names[i].Name)
				unquoted, unquoteErr := strconv.Unquote(lit.Value)
				require.NoError(t, unquoteErr)
				members = append(members, IssueType(unquoted))
			}
		}
	}
	require.NotEmpty(t, members)

	return members
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
