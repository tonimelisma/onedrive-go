package e2e

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file holds one rule of the live suite by reading its sources: a test
// that starts a process which may still be writing (a `sync --watch` daemon,
// or a one-shot sync it kills mid-transfer) never waits for a name alone. A new
// file reaches OneDrive as an empty item under its final name and its content
// follows in a second request, so the name is visible while the file still
// has 0 bytes (LI-20261110-01); such a test waits for the size or the content
// of what it reads next.
//
// The scan parses the package's files with go/parser and builds no type
// information, so it needs neither build tags nor an account and runs in the
// untagged quick lane. It follows calls through the suite's own functions to
// any depth, and stops at a wait helper, whose class answers for it. What it
// cannot see is a predicate a test writes inline for a poller: the rule covers
// the suite's named waits (poll_predicate_rule_test.go holds the predicates to
// another rule). It reads function bodies only, so a package-level
// variable's initialiser is not scanned. An exception is keyed by test and
// helper, not by call site, so it covers every call of that helper the test
// reaches.

// suitePollPrimitives are the calls that make a function a wait helper: the
// suite's real-clock sleeps and the CLI read pollers whose callers supply the
// predicate. testify's Eventually and Never families count as well
// (isTestifyPoll). A primitive is not itself a wait helper.
func suitePollPrimitives() map[string]string {
	return map[string]string{
		"sleepForLiveTestPropagation": "the one real-clock sleep between attempts of a live poll",
		"retry.TimeSleep":             "the context-aware sleep of the auth preflight's retry loop",
		"pollRemoteEventually":        "runs a CLI read until the caller's predicate accepts it, failing on timeout",
		"pollRemoteUntil":             "runs a CLI read until the caller's predicate accepts it, reporting a timeout",
	}
}

// nameOnlyWaitHelpers are the wait helpers that wait for a name alone. A test
// that starts a daemon calls none of them, directly or through a helper,
// unless daemonTestNameOnlyWaitExceptions says why that call is safe.
func nameOnlyWaitHelpers() map[string]string {
	return map[string]string{
		"getRemoteFileForDrive":                           "the name, then one download whatever it holds",
		"waitForRemoteExactStatVisible":                   "stat answering for the path",
		"waitForRemoteFixtureSeedVisible":                 "stat or the parent listing showing the name",
		"waitForRemoteReadContains":                       "a command's output holding a string, as a listing holds a name",
		"pollCLIWithConfigRetryingTransientGraphFailures": "any command succeeding, which for stat or ls is the name",
		"waitForShortcutRootPlaceholder":                  "the shortcut fixture's placeholder in the root listing",
		"waitForShortcutSharedItem":                       "the shortcut fixture in the shared listing",
		"waitForDriveListSharedSelectorVisible":           "a shared fixture's selector in drive list",
	}
}

// readyWaitHelpers are the wait helpers that wait for a size, a content or a
// state, which no upload in flight can fake.
func readyWaitHelpers() map[string]string {
	return map[string]string{
		"cleanupRemoteFolderForDrive":          "state: the folder's delete succeeds through transient failures",
		"eventuallySharedContentEquals":        "content: a download of the shared target holds the expected bytes",
		"getControlSocketStatus":               "state: the control socket answers",
		"mkdirRemoteFolderForDrive":            "state: the folder its own mkdir made, which has no content",
		"pollLocalFileContent":                 "content: the local file holds the expected bytes",
		"postControlSocket":                    "state: the control socket answers the request",
		"putRemoteFileForDrive":                "state: the file its own put wrote, the put exited with its content",
		"requireNotUploadedDuringPause":        "state: stat says the item is missing through the window a paused drive holds uploads",
		"requireSyncEventuallyConvergesWith":   "state: a one-shot sync pass, exited, meets the caller's predicate",
		"runSyncThroughTransientFailuresWith":  "state: a one-shot sync pass left no work",
		"runSyncUntilOnlyConditionsRemain":     "state: a one-shot sync pass left only recorded conditions",
		"sharedRootListingAnswersWithin":       "state: the mount root's listing route answers",
		"waitForAuthPreflightEndpoint":         "state: an auth preflight endpoint answers",
		"waitForControlSocketMount":            "state: the control socket lists the mount",
		"waitForDaemonReady":                   "state: the daemon's first observer started",
		"waitForDownloadPartial":               "size: a download partial with bytes in it",
		"waitForObserverStarted":               "state: the named observer started",
		"waitForRemoteDeleteDisappearance":     "state: the item is gone, which a delete does in one request",
		"waitForRemoteFileContent":             "content: a download holds the expected bytes",
		"waitForRemoteStatSizeForDrive":        "size: stat --json reports the expected size",
		"waitForSharedFolderNameDriveAdd":      "state: drive add by the shared folder's name succeeds",
		"waitForSharedRootListingVisible":      "state: the mount root's listing route answers",
		"waitForSocketIOConnected":             "state: the websocket connected",
		"waitForSocketIOWakeAndLocalFileAfter": "content: a websocket wake, then the local file's expected bytes",
		"waitForStatusDriveUnpaused":           "state: status no longer reports the drive paused",
		"waitForStatusPerfRequests":            "state: status --perf counts Graph requests for the mount",
		"waitForStderrContains":                "state: the process printed the line",
	}
}

// daemonWaitException is one daemon test's call of one name-only wait helper.
type daemonWaitException struct {
	test   string
	helper string
}

const shortcutFixtureReason = "the shortcut fixture is made outside the suite; no daemon writes it"

// daemonTestNameOnlyWaitExceptions are the calls of a name-only wait helper by
// a test that starts a daemon which are safe, each with its reason.
func daemonTestNameOnlyWaitExceptions() map[daemonWaitException]string {
	return map[daemonWaitException]string{
		{
			test:   "TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget",
			helper: "waitForShortcutRootPlaceholder",
		}: shortcutFixtureReason,
		{
			test:   "TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget",
			helper: "waitForShortcutSharedItem",
		}: shortcutFixtureReason,
		{
			test:   "TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot",
			helper: "waitForShortcutRootPlaceholder",
		}: shortcutFixtureReason,
		{
			test:   "TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot",
			helper: "waitForShortcutSharedItem",
		}: shortcutFixtureReason,
	}
}

// Validates: R-6.10.17
func TestE2EDaemonTestsUseNoNameOnlyWait(t *testing.T) {
	t.Parallel()

	src, err := readSuiteSources(".")
	require.NoError(t, err)

	for name := range suitePollPrimitives() {
		if _, declared := src.funcs[name]; !declared && !strings.Contains(name, ".") {
			assert.Failf(t, "stale poll primitive", "poll primitive %s is not a function of the suite: remove it", name)
		}
	}

	for _, starter := range []string{"startDaemon", "startFastDaemon", "withShortcutWatchDaemon"} {
		assert.Truef(t, src.startsProcess(starter),
			"the scan no longer sees %s start a process: it must follow helper calls down to cmd.Start", starter)
	}

	nameOnly := nameOnlyWaitHelpers()
	findings := checkDaemonWaitRule(src, daemonWaitRule{
		nameOnly:   nameOnly,
		ready:      readyWaitHelpers(),
		exceptions: daemonTestNameOnlyWaitExceptions(),
	})

	for _, name := range findings.unclassified {
		assert.Failf(t, "unclassified wait helper",
			"%s (%s) polls, so it is a wait helper, and it is in no class: add it to nameOnlyWaitHelpers if it "+
				"waits for a name alone, or to readyWaitHelpers with the size, content or state it waits for",
			name, src.funcs[name].file)
	}

	for _, name := range findings.doublyClassified {
		assert.Failf(t, "doubly classified wait helper",
			"wait helper %s is in both nameOnlyWaitHelpers and readyWaitHelpers: keep one", name)
	}

	for _, name := range findings.staleEntries {
		assert.Failf(t, "stale class entry", "%s is classified but is not a wait helper of the suite: remove it", name)
	}

	for _, finding := range findings.nameOnlyWaits {
		assert.Fail(t, finding.message(nameOnly[finding.helper]))
	}

	for _, key := range findings.staleExceptions {
		assert.Failf(t, "stale exception",
			"the exception for %s calling %s matches no call any more: remove it", key.test, key.helper)
	}
}

// daemonWaitRule is what the scan holds the suite's sources to: the two
// classes of wait helper and the declared safe calls.
type daemonWaitRule struct {
	nameOnly   map[string]string
	ready      map[string]string
	exceptions map[daemonWaitException]string
}

// daemonWaitRuleFindings is everything a scan holds against the sources, each
// list in a stable order. The sources keep the rule when every list is empty.
type daemonWaitRuleFindings struct {
	// nameOnlyWaits are the name-only waits a daemon test reaches without a
	// declared exception.
	nameOnlyWaits []daemonWaitFinding
	// unclassified are the wait helpers in neither class.
	unclassified []string
	// doublyClassified are the wait helpers in both classes.
	doublyClassified []string
	// staleEntries are the class entries that name no wait helper of the suite.
	staleEntries []string
	// staleExceptions are the exceptions that match no call of a daemon test.
	staleExceptions []daemonWaitException
}

// checkDaemonWaitRule holds src to rule. It reads only src and rule, so the
// planted sources of a test can show each kind of finding reported.
func checkDaemonWaitRule(src *suiteSources, rule daemonWaitRule) daemonWaitRuleFindings {
	var findings daemonWaitRuleFindings

	found := src.waitHelpers()
	for _, name := range found {
		_, isNameOnly := rule.nameOnly[name]
		_, isReady := rule.ready[name]
		switch {
		case isNameOnly && isReady:
			findings.doublyClassified = append(findings.doublyClassified, name)
		case !isNameOnly && !isReady:
			findings.unclassified = append(findings.unclassified, name)
		}
	}

	for _, class := range []map[string]string{rule.nameOnly, rule.ready} {
		for name := range class {
			if !slices.Contains(found, name) {
				findings.staleEntries = append(findings.staleEntries, name)
			}
		}
	}
	slices.Sort(findings.staleEntries)

	matched := make(map[daemonWaitException]bool, len(rule.exceptions))
	for _, test := range src.daemonTests() {
		for _, finding := range src.nameOnlyWaitsIn(test, rule.nameOnly) {
			key := daemonWaitException{test: finding.test, helper: finding.helper}
			if _, ok := rule.exceptions[key]; ok {
				matched[key] = true
				continue
			}

			findings.nameOnlyWaits = append(findings.nameOnlyWaits, finding)
		}
	}

	for key := range rule.exceptions {
		if !matched[key] {
			findings.staleExceptions = append(findings.staleExceptions, key)
		}
	}
	slices.SortFunc(findings.staleExceptions, func(a, b daemonWaitException) int {
		return cmp.Or(strings.Compare(a.test, b.test), strings.Compare(a.helper, b.helper))
	})

	return findings
}

// Validates: R-6.10.17
func TestDaemonWaitScanFindsDirectAndHelperCalls(t *testing.T) {
	t.Parallel()

	src := readPlantedSuiteSources(t)

	nameOnly := map[string]string{"waitForPlantedName": "a planted name"}
	assert.Equal(t, []string{"waitForPlantedName", "waitForPlantedSize"}, src.waitHelpers())
	assert.Equal(t, []string{"TestPlantedDirect", "TestPlantedThroughHelper"}, src.daemonTests())
	assert.Equal(t, []daemonWaitFinding{{test: "TestPlantedDirect", helper: "waitForPlantedName"}},
		src.nameOnlyWaitsIn("TestPlantedDirect", nameOnly))
	assert.Equal(t,
		[]daemonWaitFinding{{test: "TestPlantedThroughHelper", helper: "waitForPlantedName", via: []string{"readPlanted"}}},
		src.nameOnlyWaitsIn("TestPlantedThroughHelper", nameOnly))
}

// Validates: R-6.10.17
func TestDaemonWaitRuleReportsUnclassifiedAndStaleEntries(t *testing.T) {
	t.Parallel()

	src := readPlantedSuiteSources(t)

	findings := checkDaemonWaitRule(src, daemonWaitRule{
		nameOnly: map[string]string{
			"waitForPlantedName":    "a planted name",
			"waitForPlantedRemoved": "a helper the planted suite does not have",
		},
		ready: map[string]string{
			"waitForPlantedName": "a planted name, classified twice",
			"readPlanted":        "a helper that polls nothing itself",
		},
		exceptions: map[daemonWaitException]string{
			{test: "TestPlantedDirect", helper: "waitForPlantedName"}:   "a declared safe call",
			{test: "TestPlantedNoDaemon", helper: "waitForPlantedName"}: "a test that starts no process",
		},
	})

	assert.Equal(t, []string{"waitForPlantedSize"}, findings.unclassified, "the helper in no class")
	assert.Equal(t, []string{"waitForPlantedName"}, findings.doublyClassified, "the helper in both classes")
	assert.Equal(t, []string{"readPlanted", "waitForPlantedRemoved"}, findings.staleEntries,
		"a class entry that polls nothing and one the sources lack")
	assert.Equal(t,
		[]daemonWaitFinding{{test: "TestPlantedThroughHelper", helper: "waitForPlantedName", via: []string{"readPlanted"}}},
		findings.nameOnlyWaits, "only the call no exception declares")
	assert.Equal(t, []daemonWaitException{{test: "TestPlantedNoDaemon", helper: "waitForPlantedName"}},
		findings.staleExceptions, "the exception for a test that starts no process")
}

// readPlantedSuiteSources parses plantedDaemonWaitSource as a suite of its own.
func readPlantedSuiteSources(t *testing.T) *suiteSources {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "planted_test.go"), []byte(plantedDaemonWaitSource), 0o600))
	src, err := readSuiteSources(dir)
	require.NoError(t, err)

	return src
}

// plantedDaemonWaitSource is a suite in miniature: one test calls a name-only
// wait directly beside a daemon and waits for a size too, one calls the
// name-only wait through a helper inside a subtest, and one starts no daemon.
// One more starts no daemon and passes predicates to waits: one that reports
// (line 20), one that asserts itself (21), one that asserts through a helper
// (22), that helper by name (23), two that are no predicate of a wait, an
// action (24) and a call of a function that does not wait (25), one that
// calls t.Fatalf (26), one that skips the test (27), one that asserts two
// calls down and is given to a function that waits only through another
// (28), and a function without a result passed by name, an action too (29).
// Then predicates that are not written at the call: one an asserting builder
// returns (30), one a pure builder returns, the builder asserting outside it
// (31), and two kept in a variable and handed on through a parameter of
// forwardPlanted, one asserting (32, given at 33) and one pure (34, 35).
const plantedDaemonWaitSource = `package e2e

func startPlanted(cmd plantedCmd) { _ = cmd.Start() }

func waitForPlantedName() { sleepForLiveTestPropagation(0) }

func waitForPlantedSize() { pollRemoteEventually() }

func readPlanted() { waitForPlantedName() }

func TestPlantedDirect(t *testing.T) { startPlanted(cmd); waitForPlantedSize(); waitForPlantedName() }

func TestPlantedThroughHelper(t *testing.T) { startPlanted(cmd); t.Run("x", func(t *testing.T) { readPlanted() }) }

func TestPlantedNoDaemon(t *testing.T) { waitForPlantedName() }

func requirePlantedState(t *testing.T) bool { require.True(t, true); return true }

func TestPlantedPredicates(t *testing.T) {
	pollRemoteUntil(func() bool { return assert.ObjectsAreEqual(1, 1) })
	assert.Eventually(t, func() bool { require.NoError(t, nil); return true }, 0, 0)
	waitForPlantedSize(func() bool { return requirePlantedState(t) })
	waitForPlantedSize(requirePlantedState)
	waitForPlantedSize(func() { require.True(t, true) })
	requirePlantedState(func() bool { require.True(t, true); return true })
	waitForPlantedSize(func() bool { t.Fatalf("planted"); return true })
	waitForPlantedSize(func() bool { t.Skip(); return true })
	readPlanted(func() bool { return plantedStateHolds(t) })
	waitForPlantedSize(notePlantedState)
	waitForPlantedSize(plantedAssertingBuilder(t))
	waitForPlantedSize(plantedPureBuilder(t))
	assertingForward := func() bool { require.True(t, true); return true }
	forwardPlanted(assertingForward)
	pureForward := func() bool { return true }
	forwardPlanted(pureForward)
}

func plantedStateHolds(t *testing.T) bool { return requirePlantedState(t) }

func notePlantedState(t *testing.T) { require.True(t, true) }

func plantedAssertingBuilder(t *testing.T) func() bool {
	return func() bool { require.True(t, true); return true }
}

func plantedPureBuilder(t *testing.T) func() bool {
	require.True(t, true)
	return func() bool { return true }
}

func forwardPlanted(ready func() bool) { waitForPlantedSize(ready) }
`

// suiteFunc is one function or method of the package's sources, keyed in
// suiteSources by its name, or Recv.Method for a method.
type suiteFunc struct {
	file string
	// refs are the suite functions it calls or passes as a value, in source
	// order without repeats.
	refs []string
	// polls reports a call of a poll primitive in its own body.
	polls bool
	// startsProcess reports a call of a zero-argument Start, as exec.Cmd.Start.
	startsProcess bool
	// returnsValue reports a declared result.
	returnsValue bool
	// fails names the first call in its own body that can fail the test
	// (failingCall), or is empty.
	fails string
	// passed are the functions its body passes as arguments to a poll
	// primitive or to a suite function, in source order.
	passed []passedFunc
	// returned are the function literals with a result its own body returns:
	// what a call of it hands a wait when it builds a predicate.
	returned []passedFunc
}

// passedFunc is a function literal with a result, a suite function named as
// a value, or a suite function's call, passed as an argument directly or
// through a variable the body set to one: a predicate when the call it is
// passed to waits.
type passedFunc struct {
	// at is the argument's file and line, or the line a variable's literal
	// or builder call stands on.
	at string
	// to is the poll primitive or the suite function it is passed to.
	to string
	// toPrimitive reports that to is a poll primitive.
	toPrimitive bool
	// named is the suite function passed by name, or empty for a literal.
	named string
	// built is the suite function whose call is passed: the literals it
	// returns are the predicate, and its own body is not.
	built string
	// fails names the first call in a literal's body that can fail the test.
	fails string
	// refs are the suite functions a literal calls, or the named function.
	refs []string
}

// suiteSources is the call graph of the package's sources.
type suiteSources struct {
	funcs map[string]*suiteFunc
}

// daemonWaitFinding is one name-only wait helper a daemon test reaches, with
// the suite helpers it reaches it through.
type daemonWaitFinding struct {
	test   string
	helper string
	via    []string
}

func (finding daemonWaitFinding) message(waitsFor string) string {
	via := ""
	if len(finding.via) > 0 {
		via = " through " + strings.Join(finding.via, " > ")
	}

	return fmt.Sprintf("%s starts a process that may still be writing and waits for a name alone with %s%s (%s): "+
		"wait for the size (waitForRemoteStatSize) or the content (waitForRemoteFileContent) it reads next, "+
		"or declare the call in daemonTestNameOnlyWaitExceptions with the reason it is safe",
		finding.test, finding.helper, via, waitsFor)
}

// readSuiteSources parses every Go file of dir, whatever its build tags.
func readSuiteSources(dir string) (*suiteSources, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("list Go files of %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	var decls []*ast.FuncDecl
	declFiles := map[*ast.FuncDecl]string{}
	for _, path := range paths {
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, parseErr)
		}

		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				decls = append(decls, fn)
				declFiles[fn] = filepath.Base(path)
			}
		}
	}

	functions := map[string]bool{}
	methods := map[string][]string{}
	for _, fn := range decls {
		if fn.Recv == nil {
			functions[fn.Name.Name] = true
			continue
		}
		methods[fn.Name.Name] = append(methods[fn.Name.Name], funcKey(fn))
	}

	src := &suiteSources{funcs: make(map[string]*suiteFunc, len(decls))}
	for _, fn := range decls {
		scan := bodyScan{
			fset: fset, file: declFiles[fn], functions: functions, methods: methods,
			predicates: map[string][]passedFunc{},
		}
		scanned := scan.body(fn.Body)
		scanned.returnsValue = hasResults(fn.Type)
		scanned.returned = scan.returnedLiterals(fn.Body)
		src.funcs[funcKey(fn)] = scanned
	}

	return src, nil
}

// funcKey names a function by its name and a method by Recv.Method.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}

	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}

	return fn.Name.Name
}

// bodyScan is what scanning one body needs from the whole package: the file
// it is in, positions, and the names of the suite's functions and methods.
type bodyScan struct {
	fset      *token.FileSet
	file      string
	functions map[string]bool
	methods   map[string][]string
	// predicates are the predicates the function being scanned keeps in a
	// variable, by the variable's name, so a wait it is handed to later
	// reads it as that predicate. A name reused for another value in another
	// block of the same function is not told apart; the suite reuses none.
	predicates map[string][]passedFunc
}

// body records what one body calls: suite functions by name, suite methods by
// their selector, function values passed as arguments, poll primitives,
// zero-argument Start calls, calls that can fail the test, and the functions
// it passes to a call. Calls inside func literals count for the enclosing
// function.
func (scan bodyScan) body(body *ast.BlockStmt) *suiteFunc {
	fn := &suiteFunc{file: scan.file}
	seen := map[string]bool{}
	addRef := func(name string) {
		if !seen[name] {
			seen[name] = true
			fn.refs = append(fn.refs, name)
		}
	}
	selectorNames := map[*ast.Ident]bool{}

	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			selectorNames[node.Sel] = true
			for _, method := range scan.methods[node.Sel.Name] {
				addRef(method)
			}
		case *ast.CallExpr:
			scan.recordCall(fn, node)
		case *ast.Ident:
			if scan.functions[node.Name] && !selectorNames[node] {
				addRef(node.Name)
			}
		case *ast.AssignStmt:
			scan.recordPredicateVars(node.Lhs, node.Rhs)
		case *ast.ValueSpec:
			names := make([]ast.Expr, 0, len(node.Names))
			for _, name := range node.Names {
				names = append(names, name)
			}
			scan.recordPredicateVars(names, node.Values)
		}

		return true
	})

	return fn
}

// recordPredicateVars keeps what an assignment puts in a variable when it is
// a predicate value (predicateValue), once per position: a literal's body is
// scanned again on its own, which must not keep its variables twice.
func (scan bodyScan) recordPredicateVars(targets, values []ast.Expr) {
	if len(targets) != len(values) {
		return
	}

	for i, target := range targets {
		name, ok := target.(*ast.Ident)
		if !ok {
			continue
		}
		predicate, ok := scan.predicateValue(values[i])
		if !ok || slices.ContainsFunc(scan.predicates[name.Name], func(kept passedFunc) bool { return kept.at == predicate.at }) {
			continue
		}
		scan.predicates[name.Name] = append(scan.predicates[name.Name], predicate)
	}
}

// predicateValue describes expr when it can be a predicate: a function
// literal with a result, or a call of a suite function, whose returned
// literals (suiteFunc.returned) are then the predicate.
func (scan bodyScan) predicateValue(expr ast.Expr) (passedFunc, bool) {
	switch expr := expr.(type) {
	case *ast.FuncLit:
		if !hasResults(expr.Type) {
			return passedFunc{}, false
		}
		literal := scan.body(expr.Body)

		return passedFunc{at: scan.position(expr), fails: literal.fails, refs: literal.refs}, true
	case *ast.CallExpr:
		if ident, ok := expr.Fun.(*ast.Ident); ok && scan.functions[ident.Name] {
			return passedFunc{at: scan.position(expr), built: ident.Name}, true
		}
	}

	return passedFunc{}, false
}

// returnedLiterals lists the function literals with a result that body
// returns itself; a return inside a literal of body is the literal's own.
func (scan bodyScan) returnedLiterals(body *ast.BlockStmt) []passedFunc {
	var returned []passedFunc
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			for _, result := range node.Results {
				if literal, ok := result.(*ast.FuncLit); ok {
					if predicate, isPredicate := scan.predicateValue(literal); isPredicate {
						returned = append(returned, predicate)
					}
				}
			}
		}

		return true
	})

	return returned
}

// recordCall records what one call says about the body it is in.
func (scan bodyScan) recordCall(fn *suiteFunc, call *ast.CallExpr) {
	if _, ok := pollPrimitiveCall(call); ok {
		fn.polls = true
	}
	if isZeroArgStart(call) {
		fn.startsProcess = true
	}
	if fn.fails == "" {
		fn.fails = failingCall(call)
	}
	fn.passed = append(fn.passed, scan.passedFuncs(call)...)
}

// passedFuncs lists what call passes when it calls a poll primitive or a
// suite function: each function literal with a result, each suite function
// named as an argument or called in one, and each variable the body set to
// one of those. A parameter the body hands on is not among them: the
// predicate its caller wrote is read at that call, since a function that
// hands a parameter on to a wait reaches the wait itself.
func (scan bodyScan) passedFuncs(call *ast.CallExpr) []passedFunc {
	to, toPrimitive := pollPrimitiveCall(call)
	if !toPrimitive {
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || !scan.functions[ident.Name] {
			return nil
		}
		to = ident.Name
	}

	var passed []passedFunc
	for _, arg := range call.Args {
		for _, predicate := range scan.argPredicates(arg) {
			predicate.to, predicate.toPrimitive = to, toPrimitive
			passed = append(passed, predicate)
		}
	}

	return passed
}

// argPredicates is what one argument passes as a predicate.
func (scan bodyScan) argPredicates(arg ast.Expr) []passedFunc {
	ident, isIdent := arg.(*ast.Ident)
	switch {
	case isIdent && scan.functions[ident.Name]:
		return []passedFunc{{at: scan.position(ident), named: ident.Name, refs: []string{ident.Name}}}
	case isIdent:
		return slices.Clone(scan.predicates[ident.Name])
	}

	if predicate, ok := scan.predicateValue(arg); ok {
		return []passedFunc{predicate}
	}

	return nil
}

func (scan bodyScan) position(node ast.Node) string {
	return fmt.Sprintf("%s:%d", scan.file, scan.fset.Position(node.Pos()).Line)
}

func hasResults(funcType *ast.FuncType) bool {
	return funcType.Results != nil && len(funcType.Results.List) > 0
}

// failingCall names call when it can fail or skip the test: a testify
// assertion on t, or one of t's own methods that fail or skip
// (testEndingMethods). The suite names its *testing.T t, as the repository's
// test conventions do; a testify function given no t, such as
// assert.ObjectsAreEqual, only compares.
func failingCall(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}

	name := receiver.Name + "." + selector.Sel.Name
	switch {
	case isTestifyPackage(receiver.Name):
		if len(call.Args) > 0 && isIdentNamed(call.Args[0], "t") {
			return name
		}
	case receiver.Name == "t":
		if slices.Contains(testEndingMethods(), selector.Sel.Name) {
			return name
		}
	}

	return ""
}

// testEndingMethods are the methods of *testing.T that fail the test or skip
// it. A skip counts with the failures: a predicate that skips turns one
// transient state into a test that did not run and reads as green.
func testEndingMethods() []string {
	return []string{"Fatal", "Fatalf", "Error", "Errorf", "Fail", "FailNow", "Skip", "Skipf", "SkipNow"}
}

func isIdentNamed(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)

	return ok && ident.Name == name
}

// pollPrimitiveCall reports whether call is a poll primitive, and its name.
func pollPrimitiveCall(call *ast.CallExpr) (string, bool) {
	name := ""
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		name = pkg.Name + "." + fun.Sel.Name
		if isTestifyPoll(pkg.Name, fun.Sel.Name) {
			return name, true
		}
	default:
		return "", false
	}

	_, ok := suitePollPrimitives()[name]

	return name, ok
}

// isTestifyPoll reports a testify call that polls a condition.
func isTestifyPoll(pkg, function string) bool {
	return isTestifyPackage(pkg) && (strings.HasPrefix(function, "Eventually") || strings.HasPrefix(function, "Never"))
}

// isTestifyPackage reports the name the suite imports a testify assertion
// package under.
func isTestifyPackage(pkg string) bool {
	return pkg == "require" || pkg == "assert"
}

func isZeroArgStart(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)

	return ok && selector.Sel.Name == "Start" && len(call.Args) == 0
}

// isTest reports a top-level Test function.
func isTest(name string) bool {
	return strings.HasPrefix(name, "Test") && !strings.Contains(name, ".")
}

// waitHelpers lists, in name order, the functions that poll: they call a poll
// primitive in their own body, and are neither a test nor a primitive.
func (src *suiteSources) waitHelpers() []string {
	primitives := suitePollPrimitives()
	var helpers []string
	for name, fn := range src.funcs {
		if _, isPrimitive := primitives[name]; fn.polls && !isPrimitive && !isTest(name) {
			helpers = append(helpers, name)
		}
	}
	slices.Sort(helpers)

	return helpers
}

// startsProcess reports whether name, or a suite function it reaches, starts
// a process.
func (src *suiteSources) startsProcess(name string) bool {
	seen := map[string]bool{}
	var reaches func(string) bool
	reaches = func(current string) bool {
		fn, ok := src.funcs[current]
		if !ok || seen[current] {
			return false
		}
		seen[current] = true

		return fn.startsProcess || slices.ContainsFunc(fn.refs, reaches)
	}

	return reaches(name)
}

// daemonTests lists, in name order, the tests that start a process.
func (src *suiteSources) daemonTests() []string {
	var tests []string
	for name := range src.funcs {
		if isTest(name) && src.startsProcess(name) {
			tests = append(tests, name)
		}
	}
	slices.Sort(tests)

	return tests
}

// nameOnlyWaitsIn lists the name-only wait helpers test reaches. The walk
// follows suite functions to any depth and does not enter a wait helper: a
// wait helper's class answers for everything it calls.
func (src *suiteSources) nameOnlyWaitsIn(test string, nameOnly map[string]string) []daemonWaitFinding {
	waits := map[string]bool{}
	for _, name := range src.waitHelpers() {
		waits[name] = true
	}
	primitives := suitePollPrimitives()

	var findings []daemonWaitFinding
	seen := map[string]bool{test: true}
	var walk func(string, []string)
	walk = func(current string, via []string) {
		for _, callee := range src.funcs[current].refs {
			if seen[callee] {
				continue
			}
			seen[callee] = true

			_, isPrimitive := primitives[callee]
			switch {
			case waits[callee]:
				if _, ok := nameOnly[callee]; ok {
					findings = append(findings, daemonWaitFinding{test: test, helper: callee, via: slices.Clone(via)})
				}
			case isPrimitive:
			default:
				walk(callee, append(via, callee))
			}
		}
	}
	if _, ok := src.funcs[test]; ok {
		walk(test, nil)
	}

	return findings
}
