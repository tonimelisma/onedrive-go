package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// numberVerbAndWords finds a number verb (%d, %5d, %[1]d, %v) followed by a
// space and a lower-case word, and by a second one when only a space parts
// them.
var numberVerbAndWords = regexp.MustCompile(`%[-+# 0-9.*]*(?:\[[0-9]+\])?[dv] ([a-z][a-z-]*)(?: ([a-z][a-z-]*))?`)

// notCountNoun matches the words ending in s that follow a number verb
// without naming what is counted: a verb or an adverb whose subject the
// number is ("%v is not a folder", "%d as a limit"), and the end of a phrase
// that begins after it ("%d on this computer").
var notCountNoun = regexp.MustCompile(`^(?:always|as|does|has|is|its|plus|this|was)$`)

// pluralAfterNumberVerb returns the first count in s worded without countOf:
// a number verb followed by a plural noun (a word of two or more letters that
// ends in s, and is not one notCountNoun matches), alone ("%d files") or
// after one modifier word ("%d shared folders"). That shape reads "1 files"
// for a count of one. It returns "" when s holds none.
func pluralAfterNumberVerb(s string) string {
	isPlural := func(word string) bool {
		return len(word) >= 2 && strings.HasSuffix(word, "s") && !notCountNoun.MatchString(word)
	}

	for _, match := range numberVerbAndWords.FindAllStringSubmatch(s, -1) {
		first, second := match[1], match[2]
		if notCountNoun.MatchString(first) {
			continue
		}
		if isPlural(first) || isPlural(second) {
			return match[0]
		}
	}

	return ""
}

// The count-wording rule: every count the CLI prints is worded by countOf,
// singular for one, so no line reads "1 files". The check reads every
// string literal of the package's non-test Go files and fails on one that
// puts a plural noun, alone or after one modifier word, straight after a
// number verb, the form that bypasses countOf. It cannot see a noun handed
// over as an argument ("%d %s") or a number handed over as a string
// ("%s files"); such a site words its count with countOf itself.
//
// Validates: R-6.6.19, R-6.6.29
func TestCLIStrings_EveryCountIsWordedByCountOf(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var found []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)

		ast.Inspect(file, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			require.NoError(t, unquoteErr)
			if match := pluralAfterNumberVerb(value); match != "" {
				found = append(found, fset.Position(literal.Pos()).String()+": "+match)
			}

			return true
		})
	}

	assert.Empty(t, found, "a count printed as a number verb and a plural noun reads \"1 files\" for one: "+
		"word it with countOf(n, \"file\") instead")
}

// The check refuses a plural noun after a number verb, alone or after one
// modifier word, and lets through countOf's own format (a number before a
// noun it is handed), a number before a word that does not end in s, and a
// number that is the subject of a verb ("%d is the limit").
//
// Validates: R-6.6.19, R-6.6.29
func TestPluralAfterNumberVerb_FindsTheShapeThatBypassesCountOf(t *testing.T) {
	t.Parallel()

	for _, refused := range []string{
		"Permanently deleted %d files", "%d of %d items failed", "(%5d attempts)", "%v drives",
		"%d shared folders match", "%d state databases", "%s, %d transport errors",
		"%d local deletes", "%[1]d files", "Wrote %d files as JSON",
	} {
		assert.NotEmpty(t, pluralAfterNumberVerb(refused), refused)
	}
	for _, allowed := range []string{
		"%d %s", "%d req", "%d tx in %s", "%d min %d s", "Files: %d\n", "%d on OneDrive",
		"%v is not a folder", "%d is the limit", "%v was renamed", "%v has no parent",
		"%v does not exist", "%d as a limit", "%d on this computer",
	} {
		assert.Empty(t, pluralAfterNumberVerb(allowed), allowed)
	}
}
