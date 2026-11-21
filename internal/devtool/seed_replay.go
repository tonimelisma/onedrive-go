package devtool

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// goCommandName is the go command, as a runner is asked to start it, and
// goTestSubcommand its first argument for a test run.
const (
	goCommandName    = "go"
	goTestSubcommand = "test"
)

// A seeded model test of internal/sync whose seed fails logs one line: the
// prefix and the command that reruns that seed (logSeedReplayOnFailure),
//
//	seed-replay: ONEDRIVE_GO_MODEL_SEED=7 go test -race -run '^TestModel_CrashAtSeededPointsRecovers$' ./internal/sync
//
// with -race when the failing run had the race detector, which changes what
// some families do for one seed, and the dispatch order as a second
// assignment when the run had one. seedReplaysFromLog is the one reader of
// those lines: the nightly report's seed issues, the nightly check's seed
// keys and `verify failed-test` all read through it. A seed that go test's
// deadline ended prints no line, because no cleanup runs; its replay is built
// from its subtest's name (seedReplayFromSubtest).
const (
	seedReplayPrefix  = "seed-replay: "
	seedReplayRace    = "-race"
	seedReplayRunFlag = "-run"
	// seedReplayGoTestWords are `go test`; seedReplayTailWords are -run, its
	// pattern and the package, which end a replay.
	seedReplayGoTestWords = 2
	seedReplayTailWords   = 3
	// seedKeySeparator joins a failing seed's test key and its assignment:
	// internal/sync.TestPlannerModel_RandomTriplesConverge/ONEDRIVE_GO_PLANNER_SEED=7.
	// A key keeps the test key's one dot, so the last dot still ends the
	// package.
	seedKeySeparator = "/"
)

// The shapes of a replay command's words.
var (
	// seedReplayAssignmentPattern is one assignment in front of go test: a
	// variable and a plain value, nothing a shell would expand.
	seedReplayAssignmentPattern = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=([A-Za-z0-9._:-]+)$`)
	// seedReplayTestPattern is the -run pattern of exactly one top-level test.
	seedReplayTestPattern = regexp.MustCompile(`^'\^(Test[A-Za-z0-9_]*)\$'$`)
	// seedReplayDispatchOrderPattern is a value of the dispatch-order
	// variable: forward, reverse or shuffle:<seed>.
	seedReplayDispatchOrderPattern = regexp.MustCompile(`^(forward|reverse|shuffle:\d+)$`)
)

// seedAssignment is one environment assignment of a replay.
type seedAssignment struct {
	name  string
	value string
}

// seedReplay is one failing seed as its replay line states it.
type seedReplay struct {
	// assignments are the variables the replay sets, the family's seed
	// variable and the seed first.
	assignments []seedAssignment
	race        bool
	// test is the top-level test the replay runs.
	test string
	// pkg is its package from the module root (./internal/sync for the
	// engine's families).
	pkg string
	// fromSubtestName marks a replay built from the name of a failing
	// subtest, whose test printed no replay line.
	fromSubtestName bool
}

// seedReplaysFromLog returns the replay lines of a go test log, raw or -json,
// with or without what CI adds to each line, each command once and in log
// order. A line whose command is not exactly the replay shape is no replay.
func seedReplaysFromLog(log string) []seedReplay {
	replays := []seedReplay{}
	seen := map[string]bool{}
	for line := range strings.Lines(log) {
		text, event, isEvent := decodeGoTestLogLine(line)
		if isEvent {
			if event.Action != goTestActionOutput {
				continue
			}
			text = strings.TrimRight(event.Output, "\r\n")
		}
		_, command, found := strings.Cut(text, seedReplayPrefix)
		if !found {
			continue
		}
		replay, ok := parseSeedReplay(command)
		if !ok || seen[replay.command()] {
			continue
		}
		seen[replay.command()] = true
		replays = append(replays, replay)
	}

	return replays
}

// parseSeedReplay reads a replay command: the assignments a replay of its
// test may make (seedAssignmentsDeclared), `go test`, -race or not, -run and
// a single-quoted pattern of one seeded family's test, and the families'
// package, nothing else.
func parseSeedReplay(command string) (seedReplay, bool) {
	words := strings.Fields(command)
	replay := seedReplay{}
	for len(words) > 0 {
		match := seedReplayAssignmentPattern.FindStringSubmatch(words[0])
		if match == nil {
			break
		}
		replay.assignments = append(replay.assignments, seedAssignment{name: match[1], value: match[2]})
		words = words[1:]
	}
	if len(replay.assignments) == 0 || len(words) < seedReplayGoTestWords+seedReplayTailWords ||
		words[0] != goCommandName || words[1] != goTestSubcommand {
		return seedReplay{}, false
	}
	words = words[seedReplayGoTestWords:]
	if words[0] == seedReplayRace {
		replay.race = true
		words = words[1:]
	}
	if len(words) != seedReplayTailWords || words[0] != seedReplayRunFlag {
		return seedReplay{}, false
	}
	test := seedReplayTestPattern.FindStringSubmatch(words[1])
	if len(test) < 2 || !isSeedFamilyPackage(words[2], test[1]) {
		return seedReplay{}, false
	}
	replay.test, replay.pkg = test[1], words[2]

	return replay, seedAssignmentsDeclared(replay.test, replay.assignments)
}

// seedAssignmentsDeclared reports whether assignments are what a replay of
// test may make, the variables devtool declares and nothing else: first the
// seed, decimal, in the variable of test's family, then at most the dispatch
// order. A line that assigns anything else is no replay, so its words never
// reach a rerun's environment or a public issue.
func seedAssignmentsDeclared(test string, assignments []seedAssignment) bool {
	family, isFamily := seededFamilyOf(test)
	if !isFamily || assignments[0].name != family.seedEnv || !isDecimalSeed(assignments[0].value) {
		return false
	}
	switch rest := assignments[1:]; len(rest) {
	case 0:
		return true
	case 1:
		return rest[0].name == seedsDispatchOrderEnv && seedReplayDispatchOrderPattern.MatchString(rest[0].value)
	default:
		return false
	}
}

// isDecimalSeed reports whether value is a seed as the model tests print one:
// decimal digits only.
func isDecimalSeed(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
}

// seedReplayFromSubtest builds the replay of failure from its name when it is
// a failing subtest `<Test>/seed=<n>` of a seeded family: the seed in the
// family's variable and the family's test alone. Its test printed no replay
// line, because go test's deadline ended the run before its cleanup, so the
// replay carries what the name says and no more. A log does not say whether
// its run had the race detector, so the command names none: the issue of such
// a seed says so, and `verify failed-test` reruns it under the detector, as
// it reruns every failure whose log does not say otherwise. ok is false
// for a failure of no family, or of a family's top-level test; the error
// names a family's failing subtest whose name holds no seed, which no rerun
// without its seed reproduces.
func seedReplayFromSubtest(failure goTestFailure) (seedReplay, bool, error) {
	top, subtest, nested := strings.Cut(failure.test, "/")
	family, isFamily := seededFamilyOf(top)
	pkg := normalizeFailedGoTestPackage(failure.pkg)
	if !isFamily || !nested || (pkg != "" && pkg != family.pkg) {
		return seedReplay{}, false, nil
	}
	name, _, _ := strings.Cut(subtest, "/")
	value, named := strings.CutPrefix(name, seedSubtestPrefix)
	seed, err := strconv.ParseUint(value, 10, 64)
	if !named || err != nil {
		return seedReplay{}, false, fmt.Errorf(
			"failed test %s is a subtest of the seeded family %s whose name holds no seed (%s<n>), so no rerun reproduces it",
			failure.test, top, seedSubtestPrefix)
	}

	return seedReplay{
		assignments:     []seedAssignment{{name: family.seedEnv, value: strconv.FormatUint(seed, 10)}},
		test:            top,
		pkg:             family.pkg,
		fromSubtestName: true,
	}, true, nil
}

// failingSeedsOf is the failing seeds of top, one failed top-level test of a
// log whose failures are failures: each replay line of its test, then each of
// its failing subtests whose seed no line names, built from the subtest's
// name. complete is false when a failing subtest of a seeded family names no
// seed, so the seeds do not account for the whole failure.
func failingSeedsOf(top goTestFailure, failures []goTestFailure, replays []seedReplay) ([]seedReplay, bool) {
	seeds := []seedReplay{}
	for _, replay := range replays {
		if replay.replays(top) {
			seeds = append(seeds, replay)
		}
	}
	complete := true
	pkg := normalizeFailedGoTestPackage(top.pkg)
	for _, failure := range failures {
		name, _ := topLevelTestName(failure.test)
		if name != top.test || normalizeFailedGoTestPackage(failure.pkg) != pkg {
			continue
		}
		built, isSeed, err := seedReplayFromSubtest(failure)
		if err != nil {
			complete = false
			continue
		}
		if isSeed && !slices.ContainsFunc(seeds, built.sameSeed) {
			seeds = append(seeds, built)
		}
	}

	return seeds, complete
}

// failingSeedReplays is every failing seed a go test log names, each command
// once, in log order: the replay lines, and the seeds whose tests were ended
// before they printed theirs.
func failingSeedReplays(log string) []seedReplay {
	parsed := parseGoTestLog(log)
	replays := seedReplaysFromLog(log)
	seeds := []seedReplay{}
	seen := map[string]bool{}
	for _, failure := range parsed.failedTopLevelTestsByPackage() {
		failing, _ := failingSeedsOf(failure, parsed.failures, replays)
		for _, seed := range failing {
			if !seen[seed.command()] {
				seen[seed.command()] = true
				seeds = append(seeds, seed)
			}
		}
	}

	return seeds
}

// sameSeed reports whether other replays the seed r replays: the same test
// and the same seed in the same variable, whatever else either assigns.
func (r seedReplay) sameSeed(other seedReplay) bool {
	return r.test == other.test && r.pkg == other.pkg && r.assignments[0] == other.assignments[0]
}

// command is the replay's shell command, rebuilt from what was parsed.
func (r seedReplay) command() string {
	var b strings.Builder
	for _, assignment := range r.assignments {
		b.WriteString(assignment.name + "=" + assignment.value + " ")
	}
	b.WriteString("go test ")
	if r.race {
		b.WriteString(seedReplayRace + " ")
	}
	b.WriteString(seedReplayRunFlag + " '^" + r.test + "$' " + r.pkg)

	return b.String()
}

// line is the replay line as the test printed it.
func (r seedReplay) line() string {
	return seedReplayPrefix + r.command()
}

// key is the claim key of the failing seed: its test's key, as R-6.10.23
// keys a failed test, the separator, and the family's variable and seed.
func (r seedReplay) key() string {
	seed := r.assignments[0]

	return nightlyFailedTestKey(goTestFailure{pkg: r.pkg, test: r.test}) + seedKeySeparator + seed.name + "=" + seed.value
}

// subtest is the full name of the subtest that runs the replay's seed.
func (r seedReplay) subtest() string {
	return r.test + "/" + seedSubtestPrefix + r.assignments[0].value
}

// env is the replay's assignments as environment entries.
func (r seedReplay) env() []string {
	entries := make([]string, 0, len(r.assignments))
	for _, assignment := range r.assignments {
		entries = append(entries, assignment.name+"="+assignment.value)
	}

	return entries
}

// rerunEnv is what a rerun of the replay adds to its caller's environment:
// the replay's assignments and, when it names no dispatch order, that
// variable emptied, since an order named in the caller's environment would
// replace the seed's own.
func (r seedReplay) rerunEnv() []string {
	entries := r.env()
	namesOrder := slices.ContainsFunc(r.assignments, func(assignment seedAssignment) bool {
		return assignment.name == seedsDispatchOrderEnv
	})
	if !namesOrder {
		entries = append(entries, seedsDispatchOrderEnv+"=")
	}

	return entries
}

// replays reports whether the replay reruns a failure of test in pkg, both as
// go test log readers normalize them; a log that never named the failure's
// package matches on the test alone.
func (r seedReplay) replays(failure goTestFailure) bool {
	name, _ := topLevelTestName(failure.test)
	pkg := normalizeFailedGoTestPackage(failure.pkg)

	return name == r.test && (pkg == "" || pkg == r.pkg)
}

// isSeedKey reports whether key is a failing seed's key: a test key, the
// separator, and a seeded family's variable with a seed.
func isSeedKey(key string) bool {
	slash := strings.LastIndex(key, seedKeySeparator)
	if slash < 0 {
		return false
	}
	name, value, found := strings.Cut(key[slash+1:], "=")
	if !found || !isDecimalSeed(value) {
		return false
	}

	return slices.ContainsFunc(seededFamilies(), func(family seededFamily) bool { return family.seedEnv == name })
}
