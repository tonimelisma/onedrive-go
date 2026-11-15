package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The live watch tests size their windows from the watch daemon's debounce:
// watchLocalDebounce in sync_watch_full_test.go copies the engine's
// defaultDebounce, which the e2e package cannot import. This file has no
// build tags, so the quick lane holds the copy to the original from the
// sources, without an account.

const (
	engineWatchSource  = "../internal/sync/engine_watch.go"
	suiteWatchSource   = "sync_watch_full_test.go"
	engineDebounceName = "defaultDebounce"
	suiteDebounceName  = "watchLocalDebounce"
)

// readDurationConst reads the duration constant name declared in the Go
// source file at path.
func readDurationConst(path, name string) (time.Duration, error) {
	src, err := os.ReadFile(path) //nolint:gosec // a source file of the repository, named by the test
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	return durationConstValue(path, src, name)
}

// durationConstValue reads the duration constant name from src, the source of
// the file filename. It reads the one form both declarations use, N *
// time.Unit with N a decimal integer, and fails with what to do for any other.
func durationConstValue(filename string, src []byte, name string) (time.Duration, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", filename, err)
	}

	value, isDeclared := constValueExpr(file, name)
	if !isDeclared {
		return 0, fmt.Errorf("%s declares no constant %s: declare it there, or point this test at the file that does",
			filename, name)
	}

	duration, isReadable := unitProduct(value)
	if !isReadable {
		return 0, fmt.Errorf("%s declares %s in a form this test cannot read: write it as N * time.Unit, "+
			"or teach unitProduct the new form", filename, name)
	}

	return duration, nil
}

// constValueExpr finds the value of the top-level constant name in file. A
// constant declared without a value of its own (an iota group's later lines)
// is found with a nil value.
func constValueExpr(file *ast.File, name string) (ast.Expr, bool) {
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}

		for _, spec := range group.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, ident := range valueSpec.Names {
				if ident.Name != name {
					continue
				}
				if index < len(valueSpec.Values) {
					return valueSpec.Values[index], true
				}

				return nil, true
			}
		}
	}

	return nil, false
}

// unitProduct evaluates N * time.Unit.
func unitProduct(expr ast.Expr) (time.Duration, bool) {
	product, ok := expr.(*ast.BinaryExpr)
	if !ok || product.Op != token.MUL {
		return 0, false
	}

	count, ok := product.X.(*ast.BasicLit)
	if !ok || count.Kind != token.INT {
		return 0, false
	}
	units, err := strconv.Atoi(count.Value)
	if err != nil {
		return 0, false
	}

	unit, ok := timeUnit(product.Y)
	if !ok {
		return 0, false
	}

	return time.Duration(units) * unit, true
}

func timeUnit(expr ast.Expr) (time.Duration, bool) {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || !isIdentNamed(selector.X, "time") {
		return 0, false
	}

	unit, ok := map[string]time.Duration{
		"Nanosecond":  time.Nanosecond,
		"Microsecond": time.Microsecond,
		"Millisecond": time.Millisecond,
		"Second":      time.Second,
		"Minute":      time.Minute,
		"Hour":        time.Hour,
	}[selector.Sel.Name]

	return unit, ok
}

// Validates: R-2.8.12
func TestWatchLocalDebounceMatchesEngineDefault(t *testing.T) {
	t.Parallel()

	engineDebounce, err := readDurationConst(filepath.FromSlash(engineWatchSource), engineDebounceName)
	require.NoError(t, err)
	suiteDebounce, err := readDurationConst(suiteWatchSource, suiteDebounceName)
	require.NoError(t, err)

	assert.Equalf(t, engineDebounce, suiteDebounce,
		"%s in %s copies %s in %s, the debounce the watch tests size their windows from: make them equal",
		suiteDebounceName, suiteWatchSource, engineDebounceName, engineWatchSource)
}

// The form the reader takes, and the sentence it fails with for any other.
//
// Validates: R-2.8.12
func TestDurationConstValueReadsUnitProducts(t *testing.T) {
	t.Parallel()

	const src = `package p

const (
	product    = 5 * time.Second
	minutes    = 2 * time.Minute
	reversed   = time.Minute * 2
	bare       = time.Millisecond
	converted  = time.Duration(5) * time.Second
	notTime    = 5 * other.Second
)

var variable = 5 * time.Second
`

	for name, want := range map[string]time.Duration{
		"product": 5 * time.Second,
		"minutes": 2 * time.Minute,
	} {
		got, err := durationConstValue("p.go", []byte(src), name)
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}

	for _, name := range []string{"reversed", "bare", "converted", "notTime"} {
		_, err := durationConstValue("p.go", []byte(src), name)
		require.ErrorContains(t, err, "p.go declares "+name+" in a form this test cannot read: write it as N * time.Unit")
	}

	for _, name := range []string{"variable", "missing"} {
		_, err := durationConstValue("p.go", []byte(src), name)
		require.ErrorContains(t, err, "p.go declares no constant "+name+": declare it there")
	}
}
