package devtool

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// What go test prints, raw or as -json events, that devtool reads. The -json
// actions are go test's, the raw prefixes the testing package's.
const (
	goTestActionFail   = "fail"
	goTestActionPass   = "pass"
	goTestActionSkip   = "skip"
	goTestActionOutput = "output"
	goTestActionRun    = "run"
	// goTestActionPause and goTestActionCont report that a parallel test
	// waits for its turn and that it goes on.
	goTestActionPause = "pause"
	goTestActionCont  = "cont"
	// goTestActionBuildOutput is a line the compiler printed while building
	// a test binary; go test names the binary by ImportPath, not Package.
	goTestActionBuildOutput = "build-output"

	rawGoTestFailurePrefix = "--- FAIL:"
	rawGoTestPassPrefix    = "--- PASS:"
	// rawGoTestPackageFailPrefix and rawGoTestPackagePassPrefix start go
	// test's result line of a package, exactly as cmd/go prints it: `FAIL`, a
	// tab and the package (then a tab and the time, or ` [build failed]` or
	// ` [setup failed]`), and `ok`, two spaces, a tab and the package.
	rawGoTestPackageFailPrefix = "FAIL\t"
	rawGoTestPackagePassPrefix = "ok  \t"
	// goTestShufflePrefix starts the line a test binary run with -shuffle
	// prints first: `-test.shuffle <seed>`.
	goTestShufflePrefix = "-test.shuffle "
	// goTestTimeoutPanicPrefix starts the panic a test binary prints when its
	// -timeout fires. goTestRunningTestsHeader follows, then one line per test
	// still running, `<name> (<elapsed>)`, indented or not depending on how
	// the runtime prints a multi-line panic. The list is sorted by name, not
	// by how long each test ran; the top-level test that ran longest is the
	// one that hung (hungTimeoutEntries).
	goTestTimeoutPanicPrefix = "panic: test timed out after "
	goTestRunningTestsHeader = "running tests:"

	// devtoolStatusPrefix starts the status line devtool prints before a
	// command it runs. Other lines devtool prints carry no prefix, the
	// warning after a passing classified rerun and the notice of a skipped
	// auth preflight among them. classifiedRerunMarkerPrefix starts the line
	// rerunClassifiedQuirk prints before the go test of a classified
	// live-quirk rerun, whose window runs from the marker to the next line
	// that starts with devtoolStatusPrefix, or to the end of the log: a go
	// test devtool starts after a rerun must print a status line first, or
	// its results would count as the rerun's.
	devtoolStatusPrefix         = "==> "
	classifiedRerunMarkerPrefix = devtoolStatusPrefix + "rerun known live quirk "

	// logByteOrderMark starts a GitHub Actions job log.
	logByteOrderMark = "\ufeff"
	// rawGoTestScope is the scope of raw output: unlike a -json event, a raw
	// line does not say which package printed it.
	rawGoTestScope = ""
)

// goTestJSONEvent matches the stable field names emitted by `go test -json`.
//
//nolint:tagliatelle // Go owns these capitalized JSON keys.
type goTestJSONEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// decodeGoTestLogLine removes what CI adds to a go test line, the job log's
// byte-order mark and the timestamp GitHub Actions puts before every line,
// and decodes the rest as a -json event when it is one: it starts with `{`
// and decodes into an event with an action. Every other line is text, a raw
// line that merely holds a brace among them, such as a test's own JSON in a
// failure message.
func decodeGoTestLogLine(line string) (string, goTestJSONEvent, bool) {
	text := strings.TrimRight(strings.TrimPrefix(line, logByteOrderMark), "\r\n")
	if stamp, rest, ok := strings.Cut(text, " "); ok && isActionsTimestamp(stamp) {
		text = rest
	}
	if !strings.HasPrefix(text, "{") {
		return text, goTestJSONEvent{}, false
	}

	var event goTestJSONEvent
	if err := json.Unmarshal([]byte(text), &event); err != nil || event.Action == "" {
		return text, goTestJSONEvent{}, false
	}

	return text, event, true
}

// isActionsTimestamp reports whether a field is the UTC timestamp GitHub
// Actions starts a job log line with.
func isActionsTimestamp(field string) bool {
	if !strings.HasSuffix(field, "Z") {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, field)

	return err == nil
}

// goTestFailure is one failure a go test log reports: a test, or a package
// that failed without naming one (a build failure, a panic before any test
// reported).
type goTestFailure struct {
	// pkg is the import path go test printed, empty when the log never said.
	pkg string
	// test is the failed test's full name, empty for a package failure.
	test string
	// timedOut marks a test the timeout panic named as hung: the top-level
	// test that ran longest, or a subtest of it, killed at the binary's
	// deadline rather than failed by its own verdict.
	timedOut bool
	// attempt counts the go tests of its package that the log closed before
	// the one that reported it: zero for the first go test of the package in
	// the log.
	attempt int
}

// goTestRun is one go test of a package in a log: the normalized package and
// the attempt it is.
type goTestRun struct {
	pkg     string
	attempt int
}

// goTestInRun is a test, or the package itself when test is empty, in one go
// test of the package: a settled failure, or a classified rerun's pass.
type goTestInRun struct {
	run  goTestRun
	test string
}

// goTestLog is what one go test log says about its failures. It is the one
// reader of go test output in devtool: the nightly check, `verify
// failed-test`, the failed-test lists of the verify summary and the live
// quirk classifiers all read through it, so raw and -json output, CI
// timestamps and timeouts are understood the same way everywhere.
type goTestLog struct {
	// failures lists every failure in the order the log settles it.
	failures []goTestFailure
	// rerunPasses holds every top-level test a classified rerun passed, with
	// its normalized package and the go test of the package that passed it.
	rerunPasses map[goTestInRun]bool
	// seedByPackage is the -shuffle seed each package printed, by normalized
	// package.
	seedByPackage map[string]string
	// deadline records that a test binary panicked at its -timeout.
	deadline bool
}

// parseGoTestLog reads a go test log, raw or -json, with or without the
// decoration CI adds.
func parseGoTestLog(log string) goTestLog {
	scan := goTestLogScan{
		log:        goTestLog{seedByPackage: map[string]string{}, rerunPasses: map[goTestInRun]bool{}},
		settled:    map[goTestInRun]bool{},
		failedRuns: map[goTestRun]bool{},
		results:    map[string]int{},
		listing:    map[string]timeoutListing{},
		running:    map[string][]runningTest{},
	}
	for line := range strings.Lines(log) {
		text, event, isEvent := decodeGoTestLogLine(line)
		if isEvent {
			scan.observeEvent(event)
			continue
		}
		if strings.HasPrefix(text, devtoolStatusPrefix) {
			scan.inClassifiedRerun = strings.HasPrefix(text, classifiedRerunMarkerPrefix)
			continue
		}
		scan.observeText(rawGoTestScope, text)
	}
	// A log cut off before its package results, a job killed at its own
	// timeout, still names what it saw fail.
	for scope := range scan.running {
		scan.flushTimeoutListing(scope)
	}
	for _, p := range scan.pending {
		scan.settle(p.failure)
	}

	return scan.log
}

// failedTopLevelTests names the failed top-level tests, timed-out ones
// included, each once and in log order.
func (l goTestLog) failedTopLevelTests() []string {
	tests := []string{}
	seen := map[string]bool{}
	for _, failure := range l.failures {
		name, isTest := topLevelTestName(failure.test)
		if !isTest || seen[name] {
			continue
		}
		seen[name] = true
		tests = append(tests, name)
	}

	return tests
}

// failedTopLevelTestsByPackage names each failed top-level test with the
// package that ran it, normalized and empty when the log never said, each
// pair once and in log order: two packages can hold tests of one name. A
// failure that a classified live-quirk rerun passed in the next go test of
// its package, the go test that follows the rerun's marker line, is left
// out. A failure in that rerun, in any go test after it or in an earlier go
// test of the package than the one it reran is not, and a pass in any other
// go test answers no failure: neither a later go test of the package nor
// another iteration of -count inside the failing one.
func (l goTestLog) failedTopLevelTestsByPackage() []goTestFailure {
	tests := []goTestFailure{}
	seen := map[string]bool{}
	for _, failure := range l.failures {
		name, isTest := topLevelTestName(failure.test)
		pkg := normalizeFailedGoTestPackage(failure.pkg)
		pair := goTestPair(pkg, name)
		if !isTest || seen[pair] {
			continue
		}
		if l.rerunPasses[goTestInRun{run: goTestRun{pkg: pkg, attempt: failure.attempt + 1}, test: name}] {
			continue
		}
		seen[pair] = true
		tests = append(tests, goTestFailure{pkg: pkg, test: name, timedOut: failure.timedOut, attempt: failure.attempt})
	}

	return tests
}

// failedPackageWithoutTest reports whether a package failed without naming a
// failed test: a build or setup failure, a TestMain exit, a package timeout
// with no test running.
func (l goTestLog) failedPackageWithoutTest() bool {
	for _, failure := range l.failures {
		if failure.test == "" && failure.pkg != "" {
			return true
		}
	}

	return false
}

// goTestPair is the map key of a normalized package and a test.
func goTestPair(pkg string, test string) string {
	return pkg + "\x00" + test
}

// topLevelTestName is the top-level test of a failure's full test name, and
// whether it is a Test function's: a package failure names none.
func topLevelTestName(test string) (string, bool) {
	name, _, _ := strings.Cut(test, "/")

	return name, strings.HasPrefix(name, "Test")
}

// classifiableFailures is what a classified live-quirk rerun may answer: the
// full name of every test that failed by its own verdict. It is empty when a
// test binary hit its deadline, because a rerun must not paper over a hang.
func (l goTestLog) classifiableFailures() map[string]struct{} {
	failed := map[string]struct{}{}
	if l.deadline {
		return failed
	}
	for _, failure := range l.failures {
		if failure.test != "" {
			failed[failure.test] = struct{}{}
		}
	}

	return failed
}

// lastFailure is the failure the log settled last.
func (l goTestLog) lastFailure() (goTestFailure, bool) {
	if len(l.failures) == 0 {
		return goTestFailure{}, false
	}

	return l.failures[len(l.failures)-1], true
}

// shuffleSeed is the -shuffle seed pkg printed, empty when it ran its tests
// in source order.
func (l goTestLog) shuffleSeed(pkg string) string {
	return l.seedByPackage[normalizeFailedGoTestPackage(pkg)]
}

// timeoutListing is where a timeout panic's list of running tests stands in
// one scope.
type timeoutListing int

const (
	timeoutNotSeen timeoutListing = iota
	timeoutPanicked
	timeoutListingTests
)

// goTestLogScan is the state of one parseGoTestLog pass.
type goTestLogScan struct {
	log goTestLog
	// pending holds failures reported before their package's result. Raw
	// output names a failed test's package only in the line that closes the
	// package, and a failure line printed by a package that then passed (a
	// test's own fixture text) is no failure.
	pending []pendingGoTestFailure
	// settled holds every settled failure, so a test reported twice in one go
	// test (a -json fail event and its --- FAIL output line, or two
	// iterations of -count) counts once, and a failure in a later go test of
	// its package is a failure of its own.
	settled map[goTestInRun]bool
	// failedRuns holds the go tests of a package with a settled failure.
	failedRuns map[goTestRun]bool
	// results counts the results each normalized package reported: a
	// package's attempt is the number of its results before it.
	results map[string]int
	// pendingPasses holds the top-level tests a raw `--- PASS:` line named,
	// waiting for the result line that names their package.
	pendingPasses []pendingGoTestPass
	// inClassifiedRerun is set from the marker line of a classified rerun to
	// the next line that starts with devtoolStatusPrefix, or the end of the
	// log.
	inClassifiedRerun bool
	// pendingRawSeed is a raw seed line not yet followed by its package's
	// result line.
	pendingRawSeed string
	listing        map[string]timeoutListing
	// running holds, per scope, the tests the timeout panic being read lists,
	// until the list ends and flushTimeoutListing keeps the hung one.
	running map[string][]runningTest
}

// runningTest is one `<name> (<elapsed>)` line of a timeout panic.
type runningTest struct {
	name    string
	elapsed time.Duration
}

// pendingGoTestFailure is a failure waiting for the result of the package
// that printed it: scope is the -json event's package, or rawGoTestScope.
type pendingGoTestFailure struct {
	scope   string
	failure goTestFailure
}

// pendingGoTestPass is a passed top-level test waiting for the result of the
// package that printed it; inRerun marks a pass a classified rerun printed.
type pendingGoTestPass struct {
	scope   string
	test    string
	inRerun bool
}

func (s *goTestLogScan) observeEvent(event goTestJSONEvent) {
	switch event.Action {
	case goTestActionOutput:
		for line := range strings.Lines(event.Output) {
			s.observeText(event.Package, strings.TrimRight(line, "\r\n"))
		}
	case goTestActionFail:
		if event.Test != "" {
			s.settle(goTestFailure{pkg: event.Package, test: event.Test})
			return
		}
		s.packageFailed(event.Package, event.Package)
	case goTestActionPass:
		if event.Test == "" {
			s.packagePassed(event.Package, event.Package)
			return
		}
		s.recordPass(event.Package, event.Test, s.inClassifiedRerun)
	}
}

// observeText reads one line of printed text: a raw line, or a line of a
// -json output event, whose package is scope.
func (s *goTestLogScan) observeText(scope string, text string) {
	trimmed := strings.TrimSpace(text)
	if s.readTimeoutListing(scope, trimmed) {
		return
	}
	if seed, ok := shuffleSeedFromOutput(trimmed); ok {
		s.recordSeed(scope, seed)
		return
	}

	switch {
	case strings.HasPrefix(trimmed, goTestTimeoutPanicPrefix):
		s.log.deadline = true
		s.listing[scope] = timeoutPanicked
	case strings.HasPrefix(trimmed, rawGoTestFailurePrefix):
		if fields := strings.Fields(strings.TrimPrefix(trimmed, rawGoTestFailurePrefix)); len(fields) > 0 {
			s.pending = append(s.pending, pendingGoTestFailure{
				scope:   scope,
				failure: goTestFailure{pkg: scope, test: fields[0]},
			})
		}
	case strings.HasPrefix(trimmed, rawGoTestPassPrefix):
		if fields := strings.Fields(strings.TrimPrefix(trimmed, rawGoTestPassPrefix)); len(fields) > 0 {
			s.pendingPasses = append(s.pendingPasses, pendingGoTestPass{scope: scope, test: fields[0], inRerun: s.inClassifiedRerun})
		}
	case scope != rawGoTestScope:
		// go test -json prints a package's result line as output and then
		// reports the result as the package's action: one result, which the
		// action closes.
	default:
		// Only go test's own result line closes a package, matched on the
		// untrimmed text: a line a test or a CLI under it prints that merely
		// starts with "ok" or "FAIL" is not one, and a bare FAIL line, which -v
		// prints before the result, names no package.
		if pkg, ok := goTestResultPackage(text, rawGoTestPackageFailPrefix); ok {
			s.packageFailed(scope, pkg)
		} else if pkg, ok := goTestResultPackage(text, rawGoTestPackagePassPrefix); ok {
			s.packagePassed(scope, pkg)
		}
	}
}

// goTestResultPackage reads the package of a result line that starts with
// prefix: the package runs to the next tab or space.
func goTestResultPackage(text string, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(text, prefix)
	if !ok {
		return "", false
	}
	pkg, _, _ := strings.Cut(rest, "\t")
	pkg, _, _ = strings.Cut(pkg, " ")

	return pkg, pkg != ""
}

// readTimeoutListing reads the running tests a timeout panic lists, and
// reports whether the line was part of the list.
func (s *goTestLogScan) readTimeoutListing(scope string, trimmed string) bool {
	switch s.listing[scope] {
	case timeoutNotSeen:
		return false
	case timeoutPanicked:
		if trimmed == goTestRunningTestsHeader {
			s.listing[scope] = timeoutListingTests
			return true
		}
	case timeoutListingTests:
		if running, ok := readRunningTest(trimmed); ok {
			s.running[scope] = append(s.running[scope], running)
			return true
		}
	}
	// A timeout with no test running lists none, and the first line that is
	// not a running test ends the list.
	delete(s.listing, scope)
	s.flushTimeoutListing(scope)

	return false
}

// flushTimeoutListing records the tests of scope's timeout panic that hung
// as failures waiting for their package's result, and forgets the list.
func (s *goTestLogScan) flushTimeoutListing(scope string) {
	for _, hung := range hungTimeoutEntries(s.running[scope]) {
		s.pending = append(s.pending, pendingGoTestFailure{
			scope:   scope,
			failure: goTestFailure{pkg: scope, test: hung.name, timedOut: true},
		})
	}
	delete(s.running, scope)
}

// hungTimeoutEntries keeps, of the tests a timeout panic lists as running,
// the entries of the top-level test that ran longest, its subtests among
// them, in the order listed. Every other listed test was merely running when
// the alarm fired: a parallel test started after the hung one, which the
// deadline cut short, is no failure of its own, and keying it handed a
// healthy test to an increment. Tests that ran equally long are all kept,
// and so is every entry when no elapsed time could be read, since then the
// list cannot tell which one hung.
func hungTimeoutEntries(listed []runningTest) []runningTest {
	longestByTop := map[string]time.Duration{}
	longest := time.Duration(0)
	for _, entry := range listed {
		top, _, _ := strings.Cut(entry.name, "/")
		longestByTop[top] = max(longestByTop[top], entry.elapsed)
		longest = max(longest, entry.elapsed)
	}
	hung := []runningTest{}
	for _, entry := range listed {
		top, _, _ := strings.Cut(entry.name, "/")
		if longestByTop[top] == longest {
			hung = append(hung, entry)
		}
	}

	return hung
}

// readRunningTest reads one `<name> (<elapsed>)` line of a timeout panic. An
// elapsed time that does not parse as a Go duration counts as zero: the line
// still names a running test.
func readRunningTest(trimmed string) (runningTest, bool) {
	name, elapsed, ok := strings.Cut(trimmed, " ")
	if !ok || name == "" || !strings.HasPrefix(elapsed, "(") || !strings.HasSuffix(elapsed, ")") {
		return runningTest{}, false
	}
	duration, err := time.ParseDuration(strings.TrimSuffix(strings.TrimPrefix(elapsed, "("), ")"))
	if err != nil {
		duration = 0
	}

	return runningTest{name: name, elapsed: duration}, true
}

// packageFailed settles the failures scope reported with pkg as their
// package, or the package itself when this go test of it failed without
// naming a test.
func (s *goTestLogScan) packageFailed(scope string, pkg string) {
	s.flushTimeoutListing(scope)
	s.claimRawSeed(scope, pkg)
	s.claimPasses(scope, pkg)
	kept := s.pending[:0]
	for _, p := range s.pending {
		if p.scope != scope {
			kept = append(kept, p)
			continue
		}
		p.failure.pkg = pkg
		s.settle(p.failure)
	}
	s.pending = kept
	normalized := normalizeFailedGoTestPackage(pkg)
	if !s.failedRuns[goTestRun{pkg: normalized, attempt: s.results[normalized]}] {
		s.settle(goTestFailure{pkg: pkg})
	}
	s.results[normalized]++
}

// packagePassed drops the failure lines scope printed: a package that passed
// failed no test.
func (s *goTestLogScan) packagePassed(scope string, pkg string) {
	s.flushTimeoutListing(scope)
	s.claimRawSeed(scope, pkg)
	s.claimPasses(scope, pkg)
	kept := s.pending[:0]
	for _, p := range s.pending {
		if p.scope != scope {
			kept = append(kept, p)
		}
	}
	s.pending = kept
	s.results[normalizeFailedGoTestPackage(pkg)]++
}

// claimPasses records the passes scope printed as passes of pkg's current
// attempt.
func (s *goTestLogScan) claimPasses(scope string, pkg string) {
	kept := s.pendingPasses[:0]
	for _, p := range s.pendingPasses {
		if p.scope != scope {
			kept = append(kept, p)
			continue
		}
		s.recordPass(pkg, p.test, p.inRerun)
	}
	s.pendingPasses = kept
}

// recordPass records that a classified rerun passed a top-level test of pkg
// in the package's current attempt. A pass outside a classified rerun
// answers no failure, and a subtest's pass says nothing about its parent.
func (s *goTestLogScan) recordPass(pkg string, test string, inRerun bool) {
	if !inRerun || strings.Contains(test, "/") {
		return
	}
	normalized := normalizeFailedGoTestPackage(pkg)
	s.log.rerunPasses[goTestInRun{run: goTestRun{pkg: normalized, attempt: s.results[normalized]}, test: test}] = true
}

// settle records a failure of its package's current go test, once per go
// test.
func (s *goTestLogScan) settle(failure goTestFailure) {
	pkg := normalizeFailedGoTestPackage(failure.pkg)
	run := goTestRun{pkg: pkg, attempt: s.results[pkg]}
	key := goTestInRun{run: run, test: failure.test}
	if s.settled[key] {
		return
	}
	s.settled[key] = true
	if pkg != "" {
		s.failedRuns[run] = true
	}
	failure.attempt = run.attempt
	s.log.failures = append(s.log.failures, failure)
}

// recordSeed keeps the shuffle seed of scope's package. Raw output names the
// package only in its result line, so a raw seed waits for it.
func (s *goTestLogScan) recordSeed(scope string, seed string) {
	if scope == rawGoTestScope {
		s.pendingRawSeed = seed
		return
	}
	s.log.seedByPackage[normalizeFailedGoTestPackage(scope)] = seed
}

// claimRawSeed gives the pending raw seed line to the package whose result
// line closes its output.
func (s *goTestLogScan) claimRawSeed(scope string, pkg string) {
	if scope != rawGoTestScope || s.pendingRawSeed == "" {
		return
	}
	s.log.seedByPackage[normalizeFailedGoTestPackage(pkg)] = s.pendingRawSeed
	s.pendingRawSeed = ""
}

// shuffleSeedFromOutput reads the seed from a `-test.shuffle <seed>` line. The
// seed must be an integer, because it goes back onto a command line.
func shuffleSeedFromOutput(output string) (string, bool) {
	seed, ok := strings.CutPrefix(output, goTestShufflePrefix)
	if !ok {
		return "", false
	}
	if _, err := strconv.ParseInt(seed, 10, 64); err != nil {
		return "", false
	}

	return seed, true
}
