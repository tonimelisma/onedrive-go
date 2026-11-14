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
	// the runtime prints a multi-line panic.
	goTestTimeoutPanicPrefix = "panic: test timed out after "
	goTestRunningTestsHeader = "running tests:"

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
	// timedOut marks a test the timeout panic listed as running: it was
	// killed at the binary's deadline, not failed by its own verdict.
	timedOut bool
}

// goTestLog is what one go test log says about its failures. It is the one
// reader of go test output in devtool: the nightly check, `verify
// failed-test`, the failed-test lists of the verify summary and the live
// quirk classifiers all read through it, so raw and -json output, CI
// timestamps and timeouts are understood the same way everywhere.
type goTestLog struct {
	// failures lists every failure in the order the log settles it.
	failures []goTestFailure
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
		log:            goTestLog{seedByPackage: map[string]string{}},
		settled:        map[string]bool{},
		failedPackages: map[string]bool{},
		listing:        map[string]timeoutListing{},
	}
	for line := range strings.Lines(log) {
		text, event, isEvent := decodeGoTestLogLine(line)
		if isEvent {
			scan.observeEvent(event)
			continue
		}
		scan.observeText(rawGoTestScope, text)
	}
	// A log cut off before its package results, a job killed at its own
	// timeout, still names what it saw fail.
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
		name, _, _ := strings.Cut(failure.test, "/")
		if !strings.HasPrefix(name, "Test") || seen[name] {
			continue
		}
		seen[name] = true
		tests = append(tests, name)
	}

	return tests
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
	// settled holds the package and test of every settled failure, so a test
	// reported twice (a -json fail event and its --- FAIL output line) counts
	// once.
	settled map[string]bool
	// failedPackages holds the normalized packages with a settled failure.
	failedPackages map[string]bool
	// pendingRawSeed is a raw seed line not yet followed by its package's
	// result line.
	pendingRawSeed string
	listing        map[string]timeoutListing
}

// pendingGoTestFailure is a failure waiting for the result of the package
// that printed it: scope is the -json event's package, or rawGoTestScope.
type pendingGoTestFailure struct {
	scope   string
	failure goTestFailure
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
		}
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
		if name, ok := runningTestName(trimmed); ok {
			s.pending = append(s.pending, pendingGoTestFailure{
				scope:   scope,
				failure: goTestFailure{pkg: scope, test: name, timedOut: true},
			})
			return true
		}
	}
	// A timeout with no test running lists none, and the first line that is
	// not a running test ends the list.
	delete(s.listing, scope)

	return false
}

// runningTestName reads one `<name> (<elapsed>)` line of a timeout panic.
func runningTestName(trimmed string) (string, bool) {
	name, elapsed, ok := strings.Cut(trimmed, " ")
	if !ok || name == "" || !strings.HasPrefix(elapsed, "(") || !strings.HasSuffix(elapsed, ")") {
		return "", false
	}

	return name, true
}

// packageFailed settles the failures scope reported with pkg as their
// package, or the package itself when it failed without naming a test.
func (s *goTestLogScan) packageFailed(scope string, pkg string) {
	s.claimRawSeed(scope, pkg)
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
	if !s.failedPackages[normalizeFailedGoTestPackage(pkg)] {
		s.settle(goTestFailure{pkg: pkg})
	}
}

// packagePassed drops the failure lines scope printed: a package that passed
// failed no test.
func (s *goTestLogScan) packagePassed(scope string, pkg string) {
	s.claimRawSeed(scope, pkg)
	kept := s.pending[:0]
	for _, p := range s.pending {
		if p.scope != scope {
			kept = append(kept, p)
		}
	}
	s.pending = kept
}

func (s *goTestLogScan) settle(failure goTestFailure) {
	pkg := normalizeFailedGoTestPackage(failure.pkg)
	key := pkg + "\x00" + failure.test
	if s.settled[key] {
		return
	}
	s.settled[key] = true
	if pkg != "" {
		s.failedPackages[pkg] = true
	}
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
