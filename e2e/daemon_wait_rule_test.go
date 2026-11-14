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
// the suite's named waits. It reads function bodies only, so a package-level
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

const (
	readAfterDaemonExitReason = "reads the content again after the daemon exited, " +
		"which waitForRemoteFileContent confirmed while it ran"
	shortcutFixtureReason = "the shortcut fixture is made outside the suite; no daemon writes it"
)

// daemonTestNameOnlyWaitExceptions are the calls of a name-only wait helper by
// a test that starts a daemon which are safe, each with its reason.
func daemonTestNameOnlyWaitExceptions() map[daemonWaitException]string {
	return map[daemonWaitException]string{
		{test: "TestE2E_SyncWatch_BasicRoundTrip", helper: "getRemoteFileForDrive"}:      readAfterDaemonExitReason,
		{test: "TestE2E_SyncWatch_PauseResume", helper: "getRemoteFileForDrive"}:         readAfterDaemonExitReason,
		{test: "TestE2E_SyncWatch_ControlSocketReload", helper: "getRemoteFileForDrive"}: readAfterDaemonExitReason,
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
const plantedDaemonWaitSource = `package e2e

func startPlanted(cmd plantedCmd) { _ = cmd.Start() }

func waitForPlantedName() { sleepForLiveTestPropagation(0) }

func waitForPlantedSize() { pollRemoteEventually() }

func readPlanted() { waitForPlantedName() }

func TestPlantedDirect(t *testing.T) { startPlanted(cmd); waitForPlantedSize(); waitForPlantedName() }

func TestPlantedThroughHelper(t *testing.T) { startPlanted(cmd); t.Run("x", func(t *testing.T) { readPlanted() }) }

func TestPlantedNoDaemon(t *testing.T) { waitForPlantedName() }
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
		src.funcs[funcKey(fn)] = scanFuncBody(declFiles[fn], fn.Body, functions, methods)
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

// scanFuncBody records what one body calls: suite functions by name, suite
// methods by their selector, function values passed as arguments, poll
// primitives and zero-argument Start calls. Calls inside func literals count
// for the enclosing function.
func scanFuncBody(
	file string,
	body *ast.BlockStmt,
	functions map[string]bool,
	methods map[string][]string,
) *suiteFunc {
	fn := &suiteFunc{file: file}
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
			for _, method := range methods[node.Sel.Name] {
				addRef(method)
			}
		case *ast.CallExpr:
			if _, ok := pollPrimitiveCall(node); ok {
				fn.polls = true
			}
			if isZeroArgStart(node) {
				fn.startsProcess = true
			}
		case *ast.Ident:
			if functions[node.Name] && !selectorNames[node] {
				addRef(node.Name)
			}
		}

		return true
	})

	return fn
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
	return (pkg == "require" || pkg == "assert") &&
		(strings.HasPrefix(function, "Eventually") || strings.HasPrefix(function, "Never"))
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
