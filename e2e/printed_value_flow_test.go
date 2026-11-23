package e2e

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file holds how the print rule (printed_value_rule_test.go) follows a
// value from where a function gets it to where the function prints it, so a
// variable holds a command's output, a command line or an identity whatever
// its name: what a JSON decode fills, what one of the suite's CLI runners
// returns, and what a function sets from a value that already holds one.

// suiteCLIRunners are the suite's functions and methods (Recv.Method) that
// run the CLI and hand back what it printed: each string one returns is the
// command's output and each []string its command line. A runner is a
// function whose own body calls one of suiteRunnerPrimitives and that
// returns a string or a []string; TestSuiteCLIRunnersAreTheRunnersOfTheSources
// holds this list to the sources.
func suiteCLIRunners() []string {
	return []string{
		"execCLI",
		"pollCLIWithConfigRetryingTransientGraphFailures",
		"readRemoteFileOnce",
		"runCLICore",
		"runCLIWithConfig",
		"runCLIWithConfigAllDrives",
		"runCLIWithConfigAllDrivesAllowError",
		"runCLIWithConfigAllowError",
		"runCLIWithConfigAllowErrorForDrive",
		"runCLIWithConfigExpectError",
		"runCLIWithConfigForDrive",
		"runCLIWithoutDrive",
		"runRemoteReadAttemptWithRecovery",
		"runStandingBaselinePass",
		"sharedFolderNameFixtureVisible",
		"syncRunner.run",
		"withShortcutWatchDaemon",
	}
}

// suiteRunnerPrimitives start the CLI or run it and hand back its output.
func suiteRunnerPrimitives() []string {
	return []string{"makeCmd", "execCLI", "runCLICore"}
}

// cliRunnerDeclarations lists, in name order, the functions and methods of
// files that are CLI runners by suiteCLIRunners' definition.
func cliRunnerDeclarations(files []*ast.File) []string {
	var runners []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !slices.ContainsFunc(resultClasses(fn.Type), isClassed) {
				continue
			}
			if callsAny(fn.Body, suiteRunnerPrimitives()) {
				runners = append(runners, funcKey(fn))
			}
		}
	}
	slices.Sort(runners)

	return runners
}

func isClassed(class string) bool { return class != "" }

// callsAny reports a call in body of a function named one of names.
func callsAny(body *ast.BlockStmt, names []string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if ident, isIdent := call.Fun.(*ast.Ident); isIdent && slices.Contains(names, ident.Name) {
				found = true
			}
		}

		return !found
	})

	return found
}

// resultClasses lists, one per result of funcType, the class of value a
// runner's result is: a string its output, a []string its command line,
// anything else none.
func resultClasses(funcType *ast.FuncType) []string {
	if funcType.Results == nil {
		return nil
	}

	var classes []string
	for _, field := range funcType.Results.List {
		class := ""
		switch result := field.Type.(type) {
		case *ast.Ident:
			if result.Name == stringTypeName {
				class = printedClassOutput
			}
		case *ast.ArrayType:
			if isIdentNamed(result.Elt, stringTypeName) && result.Len == nil {
				class = printedClassCommand
			}
		}
		for range max(1, len(field.Names)) {
			classes = append(classes, class)
		}
	}

	return classes
}

// runnerResultsOf maps each runner of suiteCLIRunners that files declare to
// its resultClasses, a method by its name alone: the scan reads no types, so
// a call's receiver is not known.
func runnerResultsOf(files []*ast.File) map[string][]string {
	runners := map[string][]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Body != nil && slices.Contains(suiteCLIRunners(), funcKey(fn)) {
				runners[fn.Name.Name] = resultClasses(fn.Type)
			}
		}
	}

	return runners
}

// valueFlow holds, by name, the class of value a variable of one function
// holds whatever its name. A name is followed through the whole function,
// its literals included, and not by scope: a name that holds a value
// anywhere in the function holds it everywhere in it.
type valueFlow map[string]string

func (flow valueFlow) set(target ast.Expr, class string) {
	ident, ok := target.(*ast.Ident)
	if !ok || ident.Name == "_" || class == "" {
		return
	}
	if _, known := flow[ident.Name]; !known {
		flow[ident.Name] = class
	}
}

// followFunctions classes the parameters and results of the suite functions
// of live to a fixed point: each pass follows what callers pass
// (followParameters) and what bodies return (followResults), and the classes
// one gives the flows the other reads, until a pass adds none. Each class is
// set once and never taken back, so the passes end.
func (scan *printedValueScan) followFunctions(live []*ast.FuncDecl) {
	scan.params = map[string]valueFlow{}
	scan.results = map[string][]string{}
	for {
		addedParams := scan.followParameters(live)
		if addedResults := scan.followResults(live); !addedParams && !addedResults {
			return
		}
	}
}

// followParameters gives each parameter of a suite function the class of a
// value one of live passes it, until a pass over live adds none: a helper
// holds what its callers hand it whatever its parameters are named. A method
// is not followed: the scan reads no types, so a call's receiver is not
// known. It reports whether it added a class.
func (scan *printedValueScan) followParameters(live []*ast.FuncDecl) bool {
	addedAny := false
	for added := true; added; {
		added = false
		for _, fn := range live {
			flow := scan.flowOf(fn)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok && scan.passClasses(flow, call) {
					added = true
				}

				return true
			})
		}
		addedAny = addedAny || added
	}

	return addedAny
}

// followResults gives each result of a suite function of live the class of
// a value one of its return statements returns there, so a caller that
// prints what a helper returns prints what the helper got: an identity a
// helper reads and returns is the caller's identity, through any number of
// calls. Only a result that is text is classed (textResults): a string, a
// []string or an error. A count (an int, a bool, a duration) names no drive
// or item, and the suite compares and prints such counts freely; a struct, a
// map or a pointer a helper decodes holds counts and states beside names,
// and without types the scan could not tell a count field from a name field,
// so it would class every decoded count the suite compares. Methods are not
// followed, as for parameters. It reports whether it added a class.
func (scan *printedValueScan) followResults(live []*ast.FuncDecl) bool {
	added := false
	for _, fn := range live {
		if fn.Recv != nil || fn.Type.Results == nil || slices.Contains(suiteCLIRunners(), fn.Name.Name) {
			continue
		}

		texts := textResults(fn.Type)
		classes := scan.results[fn.Name.Name]
		if classes == nil {
			classes = make([]string, len(texts))
		}
		flow := scan.flowOf(fn)
		for _, returned := range returnedClasses(scan, flow, fn) {
			for i, class := range returned {
				if i < len(classes) && classes[i] == "" && texts[i] && class != "" {
					classes[i] = class
					added = true
				}
			}
		}
		scan.results[fn.Name.Name] = classes
	}

	return added
}

// returnedClasses lists, for each return statement of fn's own body (not of
// a function literal in it), the class each result holds: the value
// returned at its position, a named result's flow for a bare return, and a
// suite function's results for a return of its call.
func returnedClasses(scan *printedValueScan, flow valueFlow, fn *ast.FuncDecl) [][]string {
	var named []string
	for _, field := range fn.Type.Results.List {
		if len(field.Names) == 0 {
			named = append(named, "")
		}
		for _, name := range field.Names {
			named = append(named, name.Name)
		}
	}

	var returns [][]string
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returns = append(returns, scan.returnClasses(flow, node, named))
		}

		return true
	})

	return returns
}

func (scan *printedValueScan) returnClasses(flow valueFlow, ret *ast.ReturnStmt, named []string) []string {
	classes := make([]string, len(named))
	switch {
	case len(ret.Results) == 0:
		for i, name := range named {
			classes[i] = flow[name]
		}
	case len(ret.Results) == 1 && len(named) > 1:
		if call, isCall := ast.Unparen(ret.Results[0]).(*ast.CallExpr); isCall {
			copy(classes, scan.callResults(call))
		}
	default:
		for i, result := range ret.Results {
			if i < len(classes) {
				classes[i] = scan.valueClass(flow, result)
			}
		}
	}

	return classes
}

// textResults reports, one per result of funcType, whether it is text a
// caller can print: a string, a []string or an error (followResults).
func textResults(funcType *ast.FuncType) []bool {
	var texts []bool
	for _, field := range funcType.Results.List {
		isText := false
		switch result := field.Type.(type) {
		case *ast.Ident:
			isText = result.Name == stringTypeName || result.Name == "error"
		case *ast.ArrayType:
			isText = result.Len == nil && isIdentNamed(result.Elt, stringTypeName)
		}
		for range max(1, len(field.Names)) {
			texts = append(texts, isText)
		}
	}

	return texts
}

// callResults is the result classes of call: a CLI runner's, or else a suite
// function's as followResults found them, or nil.
func (scan *printedValueScan) callResults(call *ast.CallExpr) []string {
	if results := scan.runnerResults(call); results != nil {
		return results
	}
	if fun, ok := call.Fun.(*ast.Ident); ok && slices.ContainsFunc(scan.results[fun.Name], isClassed) {
		return scan.results[fun.Name]
	}

	return nil
}

// passClasses gives the parameters of the suite function call calls the
// classes its arguments hold, and reports whether one was new.
func (scan *printedValueScan) passClasses(flow valueFlow, call *ast.CallExpr) bool {
	callee, ok := call.Fun.(*ast.Ident)
	if !ok || len(scan.paramNames[callee.Name]) == 0 {
		return false
	}

	names := scan.paramNames[callee.Name]
	params := scan.params[callee.Name]
	if params == nil {
		params = valueFlow{}
		scan.params[callee.Name] = params
	}

	added := false
	for i, arg := range call.Args {
		// Arguments past the last parameter are a variadic one's.
		name := names[min(i, len(names)-1)]
		if _, known := params[name]; known {
			continue
		}
		if class := scan.valueClass(flow, arg); class != "" {
			params[name] = class
			added = true
		}
	}

	return added
}

// parameterNames lists the parameter names of each function of files.
func parameterNames(files []*ast.File) map[string][]string {
	names := map[string][]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			for _, field := range fn.Type.Params.List {
				for _, param := range field.Names {
					names[fn.Name.Name] = append(names[fn.Name.Name], param.Name)
				}
			}
		}
	}

	return names
}

// flowOf follows the values of fn's body, from the classes its parameters
// hold (followParameters), until a pass over it adds none: a variable whose
// address a JSON decode is given holds a command's output, a variable a
// runner's call sets holds the class of that result, a variable set or
// ranged from a value that holds a class holds that class, and the error of
// an os call holds the class of the paths it is given, which it quotes.
func (scan *printedValueScan) flowOf(fn *ast.FuncDecl) valueFlow {
	flow := valueFlow{}
	if fn.Recv == nil {
		for name, class := range scan.params[fn.Name.Name] {
			flow[name] = class
		}
	}

	body := fn.Body
	for {
		known := len(flow)
		ast.Inspect(body, func(node ast.Node) bool {
			scan.follow(flow, node)
			return true
		})
		if len(flow) == known {
			return flow
		}
	}
}

func (scan *printedValueScan) follow(flow valueFlow, node ast.Node) {
	if targets, values, isAssignment := assignmentOf(node); isAssignment {
		scan.followAssign(flow, targets, values)
		return
	}

	switch node := node.(type) {
	case *ast.RangeStmt:
		if node.Value != nil {
			flow.set(node.Value, scan.valueClass(flow, node.X))
		}
	case *ast.CallExpr:
		if target := decodedValue(node); target != nil {
			flow.set(target, printedClassOutput)
		}
	}
}

func (scan *printedValueScan) followAssign(flow valueFlow, targets, values []ast.Expr) {
	if len(targets) == len(values) {
		for i := range targets {
			flow.set(targets[i], scan.valueClass(flow, values[i]))
		}

		return
	}
	if len(values) != 1 || len(targets) == 0 {
		return
	}

	call, isCall := ast.Unparen(values[0]).(*ast.CallExpr)
	switch {
	case isCall && scan.callResults(call) != nil:
		for i, class := range scan.callResults(call) {
			if i < len(targets) {
				flow.set(targets[i], class)
			}
		}
	case isCall && isPackageCall(call, "os"):
		flow.set(targets[len(targets)-1], scan.valueClass(flow, call))
	case !isCall:
		// v, ok := m[k], x.(T) or <-ch: the value is the first target.
		flow.set(targets[0], scan.valueClass(flow, values[0]))
	}
}

// assignmentOf is the targets and values of an assignment or a variable
// declaration, and false for any other node.
func assignmentOf(node ast.Node) (targets, values []ast.Expr, isAssignment bool) {
	switch node := node.(type) {
	case *ast.AssignStmt:
		return node.Lhs, node.Rhs, true
	case *ast.ValueSpec:
		targets = make([]ast.Expr, 0, len(node.Names))
		for _, name := range node.Names {
			targets = append(targets, name)
		}

		return targets, node.Values, true
	}

	return nil, nil, false
}

// elementNames lists the variables of body set from an element of a map or
// a slice, the value of a range statement, whatever it ranges over, or set
// from such a variable, by name as valueFlow follows them: an element of a
// decoded listing is an interface, whose text %d and %t print
// (printsNoText).
func elementNames(body *ast.BlockStmt) map[string]bool {
	elements := map[string]bool{}
	for known := -1; known != len(elements); {
		known = len(elements)
		ast.Inspect(body, func(node ast.Node) bool {
			if ranged, isRange := node.(*ast.RangeStmt); isRange {
				if ident, isIdent := ranged.Value.(*ast.Ident); isIdent {
					elements[ident.Name] = true
				}
			}
			targets, values, _ := assignmentOf(node)
			if len(values) == 1 && len(targets) > 1 {
				// v, ok := m[k]: the element is the first target.
				targets = targets[:1]
			}
			for i, target := range targets {
				ident, isIdent := target.(*ast.Ident)
				if isIdent && i < len(values) && isElement(values[i], elements) {
					elements[ident.Name] = true
				}
			}

			return true
		})
	}

	return elements
}

// isElement reports an element of a map or a slice, and a variable elements
// holds (elementNames).
func isElement(expr ast.Expr, elements map[string]bool) bool {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.IndexExpr:
		return true
	case *ast.Ident:
		return elements[expr.Name]
	}

	return false
}

// valueClass is the class of the first value expr prints that holds one, or
// the empty string.
func (scan *printedValueScan) valueClass(flow valueFlow, expr ast.Expr) string {
	for _, leaf := range scan.printedLeaves(expr) {
		if class := scan.leafClass(flow, leaf); class != "" {
			return class
		}
	}

	return ""
}

// leafClass is what a printed leaf holds: a runner's or a suite function's
// call its first result, a name or field what its name says, and else what
// the variable it is taken from holds.
func (scan *printedValueScan) leafClass(flow valueFlow, leaf ast.Expr) string {
	if call, ok := leaf.(*ast.CallExpr); ok {
		if results := scan.callResults(call); len(results) > 0 {
			return results[0]
		}

		return ""
	}

	name := leafName(leaf)
	if isUnclassedName(name) {
		return ""
	}
	if class := printedValueClass(name); class != "" {
		return class
	}
	if root := rootIdent(leaf); root != nil {
		return flow[root.Name]
	}

	return ""
}

// runnerResults is the result classes of the runner call calls, or nil.
func (scan *printedValueScan) runnerResults(call *ast.CallExpr) []string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return scan.runners[fun.Name]
	case *ast.SelectorExpr:
		if !scan.packages[exprText(fun.X)] {
			return scan.runners[fun.Sel.Name]
		}
	}

	return nil
}

// decodedValue is the variable whose address call gives json.Unmarshal or a
// decoder's Decode, or nil.
func decodedValue(call *ast.CallExpr) ast.Expr {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}

	var target ast.Expr
	switch {
	case isIdentNamed(selector.X, "json") && selector.Sel.Name == "Unmarshal" && len(call.Args) == 2:
		target = call.Args[1]
	case selector.Sel.Name == "Decode" && len(call.Args) == 1:
		target = call.Args[0]
	default:
		return nil
	}

	address, isAddress := target.(*ast.UnaryExpr)
	if !isAddress || address.Op != token.AND {
		return nil
	}
	if root := rootIdent(address.X); root != nil {
		return root
	}

	return nil
}

// rootIdent is the variable a name, a field, an element or a dereference is
// taken from, or nil.
func rootIdent(expr ast.Expr) *ast.Ident {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr
	case *ast.SelectorExpr:
		return rootIdent(expr.X)
	case *ast.IndexExpr:
		return rootIdent(expr.X)
	case *ast.StarExpr:
		return rootIdent(expr.X)
	case *ast.ParenExpr:
		return rootIdent(expr.X)
	}

	return nil
}

func isPackageCall(call *ast.CallExpr, pkg string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)

	return ok && isIdentNamed(selector.X, pkg)
}

// importedPackages lists the names files import packages under.
func importedPackages(files []*ast.File) map[string]bool {
	packages := map[string]bool{}
	for _, file := range files {
		for _, spec := range file.Imports {
			name := strings.Trim(spec.Path.Value, `"`)
			name = name[strings.LastIndexByte(name, '/')+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			packages[name] = true
		}
	}

	return packages
}

// Validates: R-6.10.27
func TestSuiteCLIRunnersAreTheRunnersOfTheSources(t *testing.T) {
	t.Parallel()

	_, files, err := parseSuiteFiles(".")
	require.NoError(t, err)

	assert.ElementsMatch(t, suiteCLIRunners(), cliRunnerDeclarations(files),
		"a function that runs the CLI and returns what it printed belongs in suiteCLIRunners, so the print "+
			"rule follows its results whatever the variables they go to are named")
}

// Validates: R-6.10.27
func TestPrintedValueRuleFollowsValues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "planted_flow_test.go"), []byte(plantedFlowSource), 0o600))

	findings, err := findPrintedValues(dir)
	require.NoError(t, err)

	assert.Equal(t, []printedValueFinding{
		{at: "planted_flow_test.go:17", call: "require.Len", value: "listing", class: printedClassOutput},
		{at: "planted_flow_test.go:19", call: "assert.Contains", value: "item", class: printedClassOutput},
		{at: "planted_flow_test.go:22", call: "assert.Equal", value: "first", class: printedClassOutput},
		{at: "planted_flow_test.go:24", call: "assert.Contains", value: "refusal", class: printedClassOutput},
		{at: "planted_flow_test.go:28", call: "t.Skipf", value: "kept", class: printedClassOutput},
		{at: "planted_flow_test.go:29", call: "assert.DirExists", value: "fixture.ShortcutName", class: printedClassIdentity},
		{at: "planted_flow_test.go:31", call: "require.NoError", value: "childPath", class: printedClassIdentity},
		{at: "planted_flow_test.go:33", call: "assertOutputContains", value: "stdout", class: printedClassOutput},
		{at: "planted_flow_test.go:34", call: "t.Logf", value: "listing", class: printedClassOutput},
		{at: "planted_flow_test.go:38", call: "assertPlantedIdentity", value: "stdout", class: printedClassOutput},
		{at: "planted_flow_test.go:39", call: "t.Logf", value: "stdout", class: printedClassOutput},
		{at: "planted_flow_test.go:40", call: "assert.Equal", value: "stdout", class: printedClassOutput},
		{at: "planted_flow_test.go:41", call: "t.Logf", value: "first", class: printedClassOutput},
		{at: "planted_flow_test.go:46", call: "t.Logf", value: "item", class: printedClassOutput},
		{at: "planted_flow_test.go:47", call: "t.Logf", value: "item", class: printedClassOutput},
		{at: "planted_flow_test.go:51", call: "t.Logf", value: "shortcutPath", class: printedClassIdentity},
		{at: "planted_flow_test.go:53", call: "assert.Equal", value: "drive", class: printedClassIdentity},
		{at: "planted_flow_test.go:58", call: "t.Run", value: "drive", class: printedClassIdentity},
		{at: "planted_flow_test.go:62", call: "require.NoError", value: "err", class: printedClassIdentity},
		{at: "planted_flow_test.go:63", call: "t.Logf", value: "err", class: printedClassIdentity},
		{at: "planted_flow_test.go:74", call: "t.Logf", value: "plantedDriveOf(...)", class: printedClassIdentity},
		{at: "planted_flow_test.go:76", call: "t.Logf", value: "selector", class: printedClassIdentity},
	}, findings,
		"a decoded listing, a value ranged and one indexed out of it, a runner's output under any name and a "+
			"variable set from one, a fixture name through a path join, an os error quoting a path built from "+
			"one, an output helper's message and a what argument, an element under %d, a slice conversion under "+
			"%d, an expected value that only looks like a format, a variable set from an element under %t, an "+
			"address under %d, a ranged value under %t, a helper's parameter given a fixture path, a live value in a test that runs no "+
			"CLI, a subtest named by a drive, an os error quoting a token path testutil builds from a drive, and that "+
			"error's text, a helper's result that is a drive, and one passed on through a second helper; not "+
			"a helper's count of one, the data os.WriteFile writes, a typed field under %d, a %T, the value of True, a subtest "+
			"named by a role or a masked message")
}

// plantedFlowSource is a live suite file in miniature whose printed values
// hold their class through the flow, not their names; its lines are the ones
// TestPrintedValueRuleFollowsValues names.
const plantedFlowSource = `//go:build e2e

package e2e

func makeCmd() {}

func runCLICore(t *testing.T, args ...string) (string, string, error) { makeCmd(); return "", "", nil }

func runCLIWithConfigExpectError(t *testing.T, args ...string) string { return "" }

func assertOutputContains(t *testing.T, output, want string, msgAndArgs ...any) {}

func TestPlantedFlow(t *testing.T) {
	stdout, _, _ := runCLICore(t, "ls", "--json")
	var listing []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &listing))
	require.Len(t, listing, 1)
	for _, item := range listing {
		assert.Contains(t, item, "id")
	}
	first := listing[0]["name"]
	assert.Equal(t, "x", first)
	refusal := runCLIWithConfigExpectError(t, "sync")
	assert.Contains(t, refusal, "rebind-root")
	var lastErr string
	_, lastErr, _ = runCLICore(t, "sync")
	kept := lastErr
	t.Skipf("last: %s", kept)
	assert.DirExists(t, filepath.Join(dir, fixture.ShortcutName))
	childPath := path.Join("parent", fixture.ShortcutName)
	require.NoError(t, os.WriteFile(filepath.FromSlash(childPath), []byte(stdout), 0o600))
	logPlantedPath(t, "/"+fixture.ShortcutName)
	assertOutputContains(t, stdout, "Uploaded", "listing: %s", stdout)
	t.Logf("%d bytes, a %T", listing[0]["size"], first)
	isFirst, _ := first.(bool)
	assert.True(t, isFirst)
	assertOutputContains(t, refusal, "rebind-root", "the refusal: %s", printable(refusal))
	assertPlantedIdentity(t, stdout, "want", "got")
	t.Logf("%d", []byte(stdout))
	assert.Equal(t, "%d entries", stdout)
	t.Logf("named: %t", first)
	var entry struct{ Size int64 }
	require.NoError(t, json.Unmarshal([]byte(stdout), &entry))
	t.Logf("%d bytes", entry.Size)
	for _, item := range listing {
		t.Logf("%d", &item)
		t.Logf("%t", item)
	}
}

func logPlantedPath(t *testing.T, shortcutPath string) { t.Logf("restoring %s", shortcutPath) }

func TestPlantedLiveValue(t *testing.T) { assert.Equal(t, "x", drive) }

func assertPlantedIdentity(t *testing.T, what, want, got string) {}

func TestPlantedSubtestAndTokenPath(t *testing.T) {
	t.Run(drive, func(t *testing.T) {})
	t.Run(suiteDriveRoles.Role(drive), func(t *testing.T) {})
	tokenPath := filepath.Join(testDataDir, testutil.TokenFileName(drive))
	_, err := os.Stat(tokenPath)
	require.NoError(t, err)
	t.Logf("stat: %s", err.Error())
}

func plantedDriveOf(t *testing.T) string { return drive }

func plantedSelectorOf(t *testing.T) (string, error) { return plantedDriveOf(t), nil }

func plantedDriveCount(t *testing.T) int { return drive }

func TestPlantedResults(t *testing.T) {
	makeCmd()
	t.Logf("one call: %s", plantedDriveOf(t))
	selector, err := plantedSelectorOf(t)
	t.Logf("two calls: %v, %v", selector, err)
	t.Logf("a count: %v", plantedDriveCount(t))
}
`
