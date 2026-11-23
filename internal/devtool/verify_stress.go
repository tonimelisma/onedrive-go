package devtool

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const millisecondsPerSecond = 1000

// syncTestTree is the sync target of the stress, race and seeds profiles: the
// whole tree, not the one directory, so a package split out of internal/sync
// stays under them instead of silently leaving their lanes.
const syncTestTree = "./internal/sync/..."

// stressMultisyncTarget is the multisync step's package.
const stressMultisyncTarget = "./internal/multisync"

// stressRepeatRuns is how many times each of the stress lane's two repeated
// steps runs its members, as the -count of its one go test. The steps'
// arguments, status lines and deadlines take it from here.
const stressRepeatRuns = 50

// A repeated step's go test deadline follows from what one pass of its
// members may cost: that budget for the cores, times stressRepeatRuns, times
// stressRepeatMargin. On a host with fewer cores than unitTestReferenceCores a
// budget is its small-host core time divided by the cores, the rule of every
// core-scaled deadline (deadlineForCores). The budget of one member's run
// bounds each member the same way: a member fails its step when the slowest
// run of the member itself or of one of its subtests is over the budget, by
// name, instead of eating the step's deadline unseen. A step that runs every
// test of its target calls each of them a member.
const (
	// stressRepeatMargin is the room a repeated step's deadline leaves above
	// its stated cost: the same 244 sync tests took 29.5 and 39.4 minutes for
	// fifty passes on two CI runs of 2026-11-15, a third apart.
	stressRepeatMargin = 1.5

	// stressMultisyncPassBudget is one pass of every multisync test on a CI
	// runner: fifty took 11m57s in the scheduled run of 2026-11-15, 14.3 s a
	// pass. On a shared 2-core host one race pass took 22.4 s on 2026-11-15,
	// 44.8 core-seconds; the small-host budget is about twice that.
	stressMultisyncPassBudget              = 20 * time.Second
	stressMultisyncPassSmallHostCoreBudget = 90 * time.Second

	// stressSyncConcurrencyPassBudget is one pass of the sync step's members
	// on a CI runner: fifty passes of the 244 tests it ran before its members
	// were declared took 39m23s in the scheduled run of 2026-11-15, 47.3 s a
	// pass. Two race passes of that selection on a shared 2-core host that
	// day, 245 tests there, took 182.4 and 139.7 s, and the members among them
	// took 99% of their summed time in each; the small-host budget, 360 s a
	// pass on 2 cores, is about twice the slower.
	stressSyncConcurrencyPassBudget              = 60 * time.Second
	stressSyncConcurrencyPassSmallHostCoreBudget = 720 * time.Second

	// stressMemberRunBudget is the longest one run of one member, or of one
	// of its subtests, may take on a CI runner, half a pass of the sync step's
	// members. The slowest member took 25.3 and 10.6 s in the two passes on
	// the 2-core host, where its budget is 90 s.
	stressMemberRunBudget              = 30 * time.Second
	stressMemberRunSmallHostCoreBudget = 180 * time.Second
)

// stressMemberRunBudgetFor is the budget of one member's run on the cores.
func stressMemberRunBudgetFor(cores int) time.Duration {
	return deadlineForCores(cores, stressMemberRunBudget, stressMemberRunSmallHostCoreBudget)
}

// stressRepeatedStep is one of the stress lane's two repeated steps: the
// go test target it runs, which of its tests it runs, and what one pass of
// them may cost.
type stressRepeatedStep struct {
	name   verifyStepName
	target string
	// selection, when set, is the members the step runs and the rule that
	// says which tests of the target must be declared. A step without one
	// runs every test of its target, so a new test there is repeated without
	// a declaration.
	selection *stressSelection
	// passBudget is one pass on a host with unitTestReferenceCores cores or
	// more, and passSmallHostCoreBudget one pass below that, as core time.
	passBudget              time.Duration
	passSmallHostCoreBudget time.Duration
}

// stressSelection is a repeated step's declared tests, per package of its
// target, and the rule that says which tests of the target must be declared.
type stressSelection struct {
	packages  []stressRepeatedPackage
	placement stressPlacementRule
}

// stressRepeatedPackage is one package of a repeated step's target, by its
// directory from the module root. memberLists return the members the step
// repeats, one list per family of tests, and nonMemberList, when set, the
// non-members: tests the step's placement rule names and the step leaves out
// on purpose. They are functions, so the check that asks for a new test to be
// placed can name the lists it may go to.
type stressRepeatedPackage struct {
	dir           string
	memberLists   []func() []string
	nonMemberList func() []stressNonMember
}

// members are the package's members over all its lists.
func (p *stressRepeatedPackage) members() []string {
	var members []string
	for _, list := range p.memberLists {
		members = append(members, list()...)
	}

	return members
}

// nonMembers are the package's non-members, none when it declares no list.
func (p *stressRepeatedPackage) nonMembers() []stressNonMember {
	if p.nonMemberList == nil {
		return nil
	}

	return p.nonMemberList()
}

// stressNonMember is a test a repeated step leaves out on purpose, with the
// reason in a few words.
type stressNonMember struct {
	test   string
	reason string
}

// stressPlacementRule names the top-level tests of a repeated step's target
// that must be declared, as members or non-members: those whose names start
// with one of prefixes or hold one of phrases. Membership itself never
// follows from a name: only a declared member runs, and a member need not be
// one the rule names.
type stressPlacementRule struct {
	prefixes []string
	phrases  []string
}

// requires reports whether the rule names test.
func (r stressPlacementRule) requires(test string) bool {
	for _, prefix := range r.prefixes {
		if strings.HasPrefix(test, prefix) {
			return true
		}
	}
	for _, phrase := range r.phrases {
		if strings.Contains(test, phrase) {
			return true
		}
	}

	return false
}

// stressRepeatedSteps are the two repeated steps, in the order they run: every
// test of the multisync package, whose orchestrator, runners, control socket
// and panic boundaries are concurrent throughout, and the sync tree's
// declared members, whose selection lives in verify_stress_members.go.
func stressRepeatedSteps() []stressRepeatedStep {
	return []stressRepeatedStep{
		{
			name:                    stressMultisyncRaceStepName,
			target:                  stressMultisyncTarget,
			passBudget:              stressMultisyncPassBudget,
			passSmallHostCoreBudget: stressMultisyncPassSmallHostCoreBudget,
		},
		{
			name:                    stressSyncConcurrencyRaceStepName,
			target:                  syncTestTree,
			selection:               stressSyncConcurrencySelection(),
			passBudget:              stressSyncConcurrencyPassBudget,
			passSmallHostCoreBudget: stressSyncConcurrencyPassSmallHostCoreBudget,
		},
	}
}

// members are the step's declared members over all its packages, none for a
// step that runs every test of its target.
func (s *stressRepeatedStep) members() []string {
	if s.selection == nil {
		return nil
	}
	var members []string
	for i := range s.selection.packages {
		members = append(members, s.selection.packages[i].members()...)
	}

	return members
}

// passBudgetFor is what one pass of the step's members may cost on the cores.
func (s *stressRepeatedStep) passBudgetFor(cores int) time.Duration {
	return deadlineForCores(cores, s.passBudget, s.passSmallHostCoreBudget)
}

// timeout is go test's deadline for the step on the cores.
func (s *stressRepeatedStep) timeout(cores int) time.Duration {
	return time.Duration(float64(s.passBudgetFor(cores)) * stressRepeatRuns * stressRepeatMargin)
}

// command is the step's one go test for the cores: its declared members,
// anchored, or every test of its target, run stressRepeatRuns times under the
// race detector in shuffled order, through `go test -json`, so each member's
// runs can be timed against the budget of one run. The status line names the
// step and the number of its declared members in place of their pattern,
// which runs to thousands of characters.
func (s *stressRepeatedStep) command(cores int) stressCommandSpec {
	args := []string{
		"test", "-json", "-race",
		"-count=" + strconv.Itoa(stressRepeatRuns),
		"-shuffle=on",
		"-timeout=" + s.timeout(cores).String(),
	}
	shown := slices.Clone(args)
	if s.selection != nil {
		members := s.members()
		args = append(args, "-run", "^("+strings.Join(members, "|")+")$")
		shown = append(shown, "-run", fmt.Sprintf("<the %d declared members of %s>", len(members), s.name))
	}
	args = append(args, s.target)
	shown = append(shown, s.target)

	return stressCommandSpec{
		stepName:        s.name,
		statusLine:      "==> go " + strings.Join(shown, " ") + "\n",
		args:            args,
		memberRunBudget: stressMemberRunBudgetFor(cores),
	}
}

// stressSyncRaceRuns is how many race runs of the whole sync tree the stress
// lane makes, as the -count of one go test. The step's status line and its
// deadline follow from it, the deadline as this many times the race profile's
// deadline for one run on the same cores, so a run of the tree that grows
// moves the two lanes' budgets together.
const stressSyncRaceRuns = 3

// stressSyncRaceTimeout is go test's deadline for the stress lane's race runs
// of the sync tree on a host with the given cores.
func stressSyncRaceTimeout(cores int) time.Duration {
	return stressSyncRaceRuns * raceSyncTimeoutForCores(cores)
}

// The stress lane's steps, in the order they run. A name holds no count, so
// a change to how often a step repeats its tests renames no nightly key.
const (
	stressMultisyncRaceStepName       verifyStepName = "multisync-race-repeated"
	stressSyncConcurrencyRaceStepName verifyStepName = "sync-concurrency-race-repeated"
	stressSyncRaceStepName            verifyStepName = "sync-tree-race-repeated"
)

// stressSyncRaceCommand is the stress lane's step of race runs of the whole
// sync tree for the given cores. It runs through `go test -json`, so the
// slowest tests can be read from its output.
func stressSyncRaceCommand(cores int) stressCommandSpec {
	count := "-count=" + strconv.Itoa(stressSyncRaceRuns)
	deadline := "-timeout=" + stressSyncRaceTimeout(cores).String()

	return stressCommandSpec{
		stepName:   stressSyncRaceStepName,
		statusLine: fmt.Sprintf("==> go test -json -race %s -shuffle=on %s %s\n", count, deadline, syncTestTree),
		args:       []string{"test", "-json", "-race", count, "-shuffle=on", deadline, syncTestTree},
	}
}

// stressCommandSpec is one stress step's go test, which runs through
// `go test -json`: its events give each test's times, and its log gets them
// as text.
type stressCommandSpec struct {
	stepName   verifyStepName
	statusLine string
	args       []string
	// memberRunBudget, when above zero, is the longest one run of a member
	// of the step, the member itself or one of its subtests, may take.
	memberRunBudget time.Duration
}

// stressTestKey identifies one test across a package tree: two packages may
// each hold a test of the same name.
type stressTestKey struct {
	pkg  string
	test string
}

type stressTestTiming struct {
	runs           int
	totalElapsedMS int64
	maxElapsedMS   int64
}

func runStress(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	cores int,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	// Repetition goes where the concurrency is. One race run of the whole
	// sync tree, with the job's setup, took 12.2 and 15.8 minutes in CI's
	// verify-race job on 2026-11-15; three took 31.3 minutes in this lane on
	// 2026-11-14 and had not finished at a fixed 40-minute deadline on
	// 2026-11-15. So fifty no longer fit any budget, and the few runs of the
	// tree take the race profile's deadline once per run. Every test of the
	// multisync package and the tree's declared tests of the watch, one-shot,
	// worker-pool, cancellation and shutdown code (where the 1-in-50 flakes
	// have come from) still run fifty times, within a budget. Every step
	// shuffles test order, so each repetition also varies what runs beside
	// what.
	repeated := stressRepeatedSteps()
	stressCommands := make([]stressCommandSpec, 0, len(repeated)+1)
	for i := range repeated {
		stressCommands = append(stressCommands, repeated[i].command(cores))
	}
	stressCommands = append(stressCommands, stressSyncRaceCommand(cores))

	// Every step runs whatever an earlier one returned: a multisync flake says
	// nothing about the sync tree, and the nightly is the only run of this
	// lane, so stopping would hide the other steps' state for a day.
	var stepErrs []error
	for _, command := range stressCommands {
		err := collector.runStepWithOutcome(command.stepName, func() (stepOutcome, error) {
			return runStressCommand(ctx, runner, repoRoot, env, stdout, stderr, command)
		})
		if err != nil {
			stepErrs = append(stepErrs, err)
		}
	}

	return errors.Join(stepErrs...)
}

// runStressCommand runs one stress step through the empty-run guard. Its log
// gets go test's events as the text go test prints without -v
// (goTestTextWriter): package lines as they come, so a hang shows how far the
// packages got, and a test's output once it failed or its package ended
// without it. Every step fails when no package of its target ran a test, with
// or without a -run pattern: a multisync or sync tree that lost its tests
// verifies nothing either. Once go test exits, the step reads the captured
// events once for each test's runs: it prints and records the slowest, and a
// step with a budget for one member's run fails for each member that took
// longer, beside any test failure. The summary names each failed test.
func runStressCommand(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
	command stressCommandSpec,
) (stepOutcome, error) {
	if err := writeStatus(stdout, command.statusLine); err != nil {
		return stepOutcome{}, fmt.Errorf("write status: %w", err)
	}

	text := newGoTestTextWriter(stdout)
	output, err := runGoTestMatching(ctx, runner, repoRoot, env, text, stderr, string(command.stepName), command.args...)
	stepErr := wrapStressError(err)
	if flushErr := text.Flush(); flushErr != nil {
		stepErr = errors.Join(stepErr, fmt.Errorf("write %s output: %w", command.stepName, flushErr))
	}

	outcome := stepOutcome{failedTests: parseGoTestLog(output).failedTopLevelTests()}
	timings := summarizeStressSlowTests(output, 0)
	if command.memberRunBudget > 0 {
		over, budgetErr := stressMembersOverRunBudget(command.stepName, timings, command.memberRunBudget)
		stepErr = errors.Join(stepErr, budgetErr)
		// One budget line per member over its budget, so the nightly keys
		// each such member even when its job also names a failed test.
		for _, member := range over {
			if writeErr := writeStatus(stderr, overBudgetMarker(normalizeFailedGoTestPackage(member.pkg), member.test)); writeErr != nil {
				stepErr = errors.Join(stepErr, fmt.Errorf("write %s budget line: %w", command.stepName, writeErr))
			}
		}
	}

	outcome.slowTests = timings[:min(len(timings), stressSlowTestLimit)]
	if writeErr := writeStressSlowTestSummary(stdout, outcome.slowTests); writeErr != nil {
		return outcome, errors.Join(fmt.Errorf("write stress timing summary: %w", writeErr), stepErr)
	}

	return outcome, stepErr
}

// stressMembersOverRunBudget is one error for each member of a repeated step
// whose slowest run, of the member itself or of one of its subtests, took
// longer than budget, naming the member, the subtest when that run was one,
// the run's time and the budget; nil when every run fit. It returns those
// members too, by package and top-level test in name order. timings are each
// test's runs, subtests included. A subtest's run counts for its member: a
// parallel subtest runs after its parent's own time is taken, so a parent's
// time does not hold its subtests'.
func stressMembersOverRunBudget(
	step verifyStepName, timings []StressSlowTestSummary, budget time.Duration,
) ([]stressTestKey, error) {
	slowest := map[stressTestKey]StressSlowTestSummary{}
	for _, timing := range timings {
		member, _, _ := strings.Cut(timing.Test, "/")
		key := stressTestKey{pkg: timing.Package, test: member}
		held, seen := slowest[key]
		if !seen || timing.MaxElapsedMS > held.MaxElapsedMS ||
			(timing.MaxElapsedMS == held.MaxElapsedMS && timing.Test < held.Test) {
			slowest[key] = timing
		}
	}

	var over []stressTestKey
	for key, timing := range slowest {
		if time.Duration(timing.MaxElapsedMS)*time.Millisecond > budget {
			over = append(over, key)
		}
	}
	slices.SortFunc(over, func(a, b stressTestKey) int {
		return cmp.Or(cmp.Compare(a.pkg, b.pkg), cmp.Compare(a.test, b.test))
	})

	errs := make([]error, 0, len(over))
	for _, key := range over {
		timing := slowest[key]
		run := ""
		if timing.Test != key.test {
			run = ", of subtest " + timing.Test
		}
		member := StressSlowTestSummary{Package: key.pkg, Test: key.test}
		errs = append(errs, fmt.Errorf("%s: member %s took %s in its slowest run%s, over the %s budget of one run",
			step, member.name(), formatDurationMS(timing.MaxElapsedMS), run, budget))
	}

	return over, errors.Join(errs...)
}

// wrapStressError names a failed stress step's error as a stress failure.
func wrapStressError(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("stress tests: %w", err)
}

// summarizeStressSlowTests totals the completed tests of `go test -json`
// output, subtests included, by package and test, slowest first: at most
// limit of them when limit is above zero, and all of them otherwise.
func summarizeStressSlowTests(output string, limit int) []StressSlowTestSummary {
	totals := make(map[stressTestKey]*stressTestTiming)

	for line := range strings.Lines(output) {
		_, event, isEvent := decodeGoTestLogLine(line)
		if !isEvent || event.Test == "" {
			continue
		}
		if event.Action != goTestActionPass && event.Action != goTestActionFail {
			continue
		}

		elapsedMS := durationMSFromSeconds(event.Elapsed)
		key := stressTestKey{pkg: event.Package, test: event.Test}
		timing := totals[key]
		if timing == nil {
			timing = &stressTestTiming{}
			totals[key] = timing
		}

		timing.runs++
		timing.totalElapsedMS += elapsedMS
		if elapsedMS > timing.maxElapsedMS {
			timing.maxElapsedMS = elapsedMS
		}
	}

	summaries := make([]StressSlowTestSummary, 0, len(totals))
	for key, timing := range totals {
		summaries = append(summaries, StressSlowTestSummary{
			Package:        key.pkg,
			Test:           key.test,
			Runs:           timing.runs,
			TotalElapsedMS: timing.totalElapsedMS,
			MaxElapsedMS:   timing.maxElapsedMS,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].TotalElapsedMS != summaries[j].TotalElapsedMS {
			return summaries[i].TotalElapsedMS > summaries[j].TotalElapsedMS
		}
		if summaries[i].MaxElapsedMS != summaries[j].MaxElapsedMS {
			return summaries[i].MaxElapsedMS > summaries[j].MaxElapsedMS
		}
		if summaries[i].Package != summaries[j].Package {
			return summaries[i].Package < summaries[j].Package
		}

		return summaries[i].Test < summaries[j].Test
	})

	if limit > 0 && len(summaries) > limit {
		summaries = summaries[:limit]
	}

	return summaries
}

func writeStressSlowTestSummary(w io.Writer, slowTests []StressSlowTestSummary) error {
	if len(slowTests) == 0 {
		return nil
	}

	if err := writeStatus(w, "==> slowest completed race tests\n"); err != nil {
		return err
	}

	for _, slow := range slowTests {
		if err := writeStatus(
			w,
			fmt.Sprintf(
				"  %s runs=%d total=%s max=%s\n",
				slow.name(),
				slow.Runs,
				formatDurationMS(slow.TotalElapsedMS),
				formatDurationMS(slow.MaxElapsedMS),
			),
		); err != nil {
			return err
		}
	}

	return nil
}

// name is the test's package and name, so tests of one name in two packages
// of the tree read apart. A summary without a package prints the test alone.
func (s StressSlowTestSummary) name() string {
	if s.Package == "" {
		return s.Test
	}

	return s.Package + " " + s.Test
}

func durationMSFromSeconds(seconds float64) int64 {
	return int64(seconds * millisecondsPerSecond)
}
