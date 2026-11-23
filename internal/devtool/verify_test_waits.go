package devtool

import (
	"go/ast"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
)

// The wait rules of spec/design/system.md bind every test: no absence is
// proved by a fixed wait, an event that must come waits on one named bound,
// and no wait for a watch is unbounded. testify's Never and Consistently
// prove an absence by a fixed wait, so a forbidigo rule of .golangci.yml
// refuses them tree-wide, and time.Sleep has its own forbidigo rule. What
// lint cannot say is where a wait's bound comes from: in the packages whose
// tests wait on the sync engine's events, this check refuses a duration
// written with the time package's units as the wait argument of the calls
// that bound a wait. A test names its bound (eventWait, syncfixture.EventWait)
// instead, so one constant moves every wait of a package together and a slow
// host is never fixed one literal at a time.

// testWaitRuleDirs are the directories whose test files' waits the check
// reads: the sync engine, the multi-drive control plane and the CLI over
// them, whose tests wait on watch-mode events.
func testWaitRuleDirs() []string {
	return []string{"internal/sync/", "internal/multisync/", "internal/cli/"}
}

// timeUnitNames are the time package's duration units.
func timeUnitNames() []string {
	return []string{"Nanosecond", "Microsecond", "Millisecond", "Second", "Minute", "Hour"}
}

// testifyEventuallyNames are testify's bounded waits for a condition.
func testifyEventuallyNames() []string {
	return []string{"Eventually", "Eventuallyf", "EventuallyWithT", "EventuallyWithTf"}
}

const (
	testifyAssertImport  = "github.com/stretchr/testify/assert"
	testifyRequireImport = "github.com/stretchr/testify/require"
	timeImport           = "time"
	contextImport        = "context"

	// The argument of each wait call that bounds the wait: time.After's
	// duration, context.WithTimeout's timeout, SetReadDeadline's deadline,
	// and testify's waitFor, which follows the test and the condition in the
	// package functions and the condition alone in an Assertions method.
	timeAfterWaitArg          = 0
	withTimeoutWaitArg        = 1
	setReadDeadlineWaitArg    = 0
	testifyFuncWaitArg        = 2
	testifyMethodWaitArg      = 1
	setReadDeadlineMethodName = "SetReadDeadline"
)

// testWaitException is a test file whose literal wait bound the check
// accepts, with the reason the literal is the right bound there.
type testWaitException struct {
	path   string
	call   string
	reason string
}

// testWaitExceptions are the declared exceptions to the literal-wait rule.
// TestTestWaitExceptionsNameLiteralWaitsOfTheTree holds each to a file of
// the tree that still has such a wait, so an exception cannot outlive its
// reason.
func testWaitExceptions() []testWaitException {
	return []testWaitException{
		{
			path: "internal/sync/internal/syncobserve/virtual_clock_test.go",
			call: "time.After",
			reason: "runs in a testing/synctest bubble, where time.After is the virtual clock: its 4 s is the churn " +
				"interval the test models against the 5 s quiet period, not a bound on a wait",
		},
	}
}

// isTestWaitRulePath reports whether rel is a test file the wait rule reads.
func isTestWaitRulePath(rel string) bool {
	if !strings.HasSuffix(rel, "_test.go") {
		return false
	}
	for _, dir := range testWaitRuleDirs() {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}

	return false
}

// testWaitImports are the names a file gives the packages the rule reads:
// an aliased import is resolved, and a blank or dot import names nothing the
// rule can qualify a call with.
type testWaitImports struct {
	time    string
	context string
	testify []string
}

func readTestWaitImports(file *ast.File) testWaitImports {
	imports := testWaitImports{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		switch importPath {
		case timeImport:
			imports.time = name
		case contextImport:
			imports.context = name
		case testifyAssertImport, testifyRequireImport:
			imports.testify = append(imports.testify, name)
		}
	}

	return imports
}

// literalWaitViolations holds every wait call of a test file the rule reads
// whose bound is written with the time package's units, unless the file's
// exception names the call.
func literalWaitViolations(fset *token.FileSet, rel string, file *ast.File) []testConventionViolation {
	if !isTestWaitRulePath(rel) {
		return nil
	}

	var violations []testConventionViolation
	for _, site := range literalWaitSites(file) {
		if testWaitExcepted(rel, site.call) {
			continue
		}
		violations = append(violations, testConventionViolation{
			Path: rel,
			Line: fset.Position(site.pos).Line,
			Message: site.call + " waits on a literal duration: name the bound (eventWait, syncfixture.EventWait) " +
				"or declare the file in testWaitExceptions with its reason",
		})
	}

	return violations
}

// literalWaitSite is one wait call whose bound is written with the time
// package's units.
type literalWaitSite struct {
	call string
	pos  token.Pos
}

// literalWaitSites lists every wait call of file whose bound is written with
// the time package's units, whatever the exceptions say.
func literalWaitSites(file *ast.File) []literalWaitSite {
	imports := readTestWaitImports(file)
	if imports.time == "" {
		// A duration written with the time package's units needs the
		// package: without it no wait bound of the file is a literal.
		return nil
	}

	var sites []literalWaitSite
	ast.Inspect(file, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		name, arg, isWait := waitCallBound(call, imports)
		if isWait && arg < len(call.Args) && holdsTimeUnit(call.Args[arg], imports.time) {
			sites = append(sites, literalWaitSite{call: name, pos: call.Pos()})
		}

		return true
	})

	return sites
}

// waitCallBound names a wait call and the index of the argument that bounds
// its wait, or reports that call is no wait the rule reads.
func waitCallBound(call *ast.CallExpr, imports testWaitImports) (string, int, bool) {
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector {
		return "", 0, false
	}
	method := selector.Sel.Name
	qualifier := ""
	if ident, isIdent := selector.X.(*ast.Ident); isIdent {
		qualifier = ident.Name
	}

	switch {
	case qualifier == imports.time && method == "After":
		return "time.After", timeAfterWaitArg, true
	case imports.context != "" && qualifier == imports.context && method == "WithTimeout":
		return "context.WithTimeout", withTimeoutWaitArg, true
	case method == setReadDeadlineMethodName:
		return setReadDeadlineMethodName, setReadDeadlineWaitArg, true
	case slices.Contains(testifyEventuallyNames(), method) && len(imports.testify) > 0:
		if slices.Contains(imports.testify, qualifier) {
			return method, testifyFuncWaitArg, true
		}

		return method, testifyMethodWaitArg, true
	default:
		return "", 0, false
	}
}

// holdsTimeUnit reports whether expr is written with one of the time
// package's units, anywhere in it: 5*time.Second, time.Second, and
// time.Now().Add(time.Minute) all are.
func holdsTimeUnit(expr ast.Expr, timeName string) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		selector, isSelector := node.(*ast.SelectorExpr)
		if !isSelector {
			return !found
		}
		if ident, isIdent := selector.X.(*ast.Ident); isIdent && ident.Name == timeName &&
			slices.Contains(timeUnitNames(), selector.Sel.Name) {
			found = true
		}

		return !found
	})

	return found
}

// testWaitExcepted reports whether an exception declares the call in rel.
func testWaitExcepted(rel string, call string) bool {
	for _, exception := range testWaitExceptions() {
		if exception.path == rel && exception.call == call {
			return true
		}
	}

	return false
}
