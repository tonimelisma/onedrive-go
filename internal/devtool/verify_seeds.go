package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// The seeds profile searches new seeds of the five seeded model families of
// internal/sync every night, without the race detector, where a seed costs a
// fraction of what it costs under it. The night's start seed is a function of
// the UTC date alone, so a rerun on the same UTC day runs the same seeds; a
// later rerun of a night passes `--seed-start` with the start that night
// printed. The start advances by the stride each day, which is at least every
// family's count, so no two nights share a seed. A failing seed logs its
// replay line, which the nightly report turns into an issue of its own: the
// issue, not the next night, keeps the seed from being forgotten.
const (
	nightlySeedStride = 1000
	// The counts are the families' own default counts (the crash run reads
	// the engine run's count).
	seedsEngineCount  = 12
	seedsPlannerCount = 8
	seedsChaosCount   = 2
	seedsWatchCount   = 4
	// seedsTimeout is go test's deadline for the seeds step, which took 11.5
	// minutes on a shared, loaded 2-core host (November 2026).
	seedsTimeout = "30m"
	hoursPerDay  = 24
)

// seedsStepName names the seeds profile's one step.
const seedsStepName verifyStepName = "model-seeds"

// The model families' variables, as internal/sync's tests read them
// (TestSeedsRunPatternNamesExistingTests holds each to their source).
const (
	seedsModelSeedEnv    = "ONEDRIVE_GO_MODEL_SEED"
	seedsModelCountEnv   = "ONEDRIVE_GO_MODEL_SEEDS"
	seedsModelStartEnv   = "ONEDRIVE_GO_MODEL_SEED_START"
	seedsPlannerSeedEnv  = "ONEDRIVE_GO_PLANNER_SEED"
	seedsPlannerCountEnv = "ONEDRIVE_GO_PLANNER_SEEDS"
	seedsPlannerStartEnv = "ONEDRIVE_GO_PLANNER_SEED_START"
	seedsChaosCountEnv   = "ONEDRIVE_GO_CHAOS_SEEDS"
	seedsChaosStartEnv   = "ONEDRIVE_GO_CHAOS_SEED_START"
	seedsWatchCountEnv   = "ONEDRIVE_GO_WATCH_SEEDS"
	seedsWatchStartEnv   = "ONEDRIVE_GO_WATCH_SEED_START"
	// seedsDispatchOrderEnv pins the order the engine dispatches a ready
	// frontier in, in place of a seed's own shuffle; a seed run under it
	// replays only with it.
	seedsDispatchOrderEnv = "ONEDRIVE_GO_DISPATCH_ORDER"
	// seedEnginePackage and seedPlannerPackage are the packages of the
	// seeded families, from the module root, and seedSubtestPrefix starts the
	// name of each family's subtest of one seed: seed=<n>.
	seedEnginePackage        = "./internal/sync"
	seedPlannerPackage       = "./internal/sync/internal/syncplan"
	seedSubtestPrefix        = "seed="
	nightlySeedEpochYear     = 2026
	nightlySeedEpochMonth    = time.January
	nightlySeedEpochMonthDay = 1
)

// seededFamily is one seeded model family of the sync tree: its top-level
// test, the variable that names one seed of it, the first assignment of its
// replay line, and its package from the module root.
type seededFamily struct {
	test    string
	seedEnv string
	pkg     string
}

// seededFamilies are the five families, the one table of them devtool holds,
// the primary one, the watch model, first:
// the seeds profile runs their tests, the replay reader accepts their
// variables, and a failing subtest of one names its seed
// (TestSeedsRunPatternNamesExistingTests and
// TestSeededFamiliesLogTheirReplayLineWithTheirVariable hold it to
// internal/sync's tests).
func seededFamilies() []seededFamily {
	return []seededFamily{
		{test: "TestModel_WatchModeConverges", seedEnv: seedsModelSeedEnv, pkg: seedEnginePackage},
		{test: "TestRunOnce_RandomizedBidirectionalEditsConverge", seedEnv: seedsModelSeedEnv, pkg: seedEnginePackage},
		{test: "TestModel_CrashAtSeededPointsRecovers", seedEnv: seedsModelSeedEnv, pkg: seedEnginePackage},
		{test: "TestPlannerModel_RandomTriplesConverge", seedEnv: seedsPlannerSeedEnv, pkg: seedPlannerPackage},
		{test: "TestModel_ChaosGraphConverges", seedEnv: seedsModelSeedEnv, pkg: seedEnginePackage},
	}
}

// isSeedFamilyPackage reports whether pkg can hold test's seeds: the
// package of test's family, or of any family for a test that is none.
func isSeedFamilyPackage(pkg, test string) bool {
	if family, ok := seededFamilyOf(test); ok {
		return pkg == family.pkg
	}
	for _, family := range seededFamilies() {
		if family.pkg == pkg {
			return true
		}
	}

	return false
}

// seededFamilyOf is the family whose top-level test is test.
func seededFamilyOf(test string) (seededFamily, bool) {
	for _, family := range seededFamilies() {
		if family.test == test {
			return family, true
		}
	}

	return seededFamily{}, false
}

// seedsRunPattern selects the five families' tests and nothing else.
func seedsRunPattern() string {
	tests := make([]string, 0, len(seededFamilies()))
	for _, family := range seededFamilies() {
		tests = append(tests, family.test)
	}

	return "^(" + strings.Join(tests, "|") + ")$"
}

// nightlySeedEpoch is the UTC day whose start seed is 1.
func nightlySeedEpoch() time.Time {
	return time.Date(nightlySeedEpochYear, nightlySeedEpochMonth, nightlySeedEpochMonthDay, 0, 0, 0, 0, time.UTC)
}

// nightlySeedStart is the first seed of the night of now's UTC date: 1, plus
// the stride for every whole day since the epoch. A clock before the epoch
// starts at 1.
func nightlySeedStart(now time.Time) uint64 {
	days := int64(now.UTC().Sub(nightlySeedEpoch()) / (hoursPerDay * time.Hour))
	if days < 0 {
		days = 0
	}

	return 1 + uint64(days)*nightlySeedStride
}

// seedsCounts are the families' counts, by the variable that carries each.
func seedsCounts() map[string]int {
	return map[string]int{
		seedsModelCountEnv:   seedsEngineCount,
		seedsPlannerCountEnv: seedsPlannerCount,
		seedsChaosCountEnv:   seedsChaosCount,
		seedsWatchCountEnv:   seedsWatchCount,
	}
}

// seedsEnv is the environment of the seeds step: every family from start,
// each with its count, the single-seed variables emptied, since a seed named
// in the caller's environment would win over the range, and the dispatch
// order emptied, since an order named there would replace each seed's own.
func seedsEnv(env []string, start uint64) []string {
	first := strconv.FormatUint(start, 10)
	seeded := append([]string{}, env...)
	seeded = append(seeded,
		seedsModelSeedEnv+"=",
		seedsPlannerSeedEnv+"=",
		seedsDispatchOrderEnv+"=",
		seedsModelStartEnv+"="+first,
		seedsPlannerStartEnv+"="+first,
		seedsChaosStartEnv+"="+first,
		seedsWatchStartEnv+"="+first,
	)
	for name, count := range seedsCounts() {
		seeded = append(seeded, name+"="+strconv.Itoa(count))
	}

	return seeded
}

// runSeedsVerification runs the seeds profile: the five families' tests over
// the sync tree, without -short (they are exhaustive tests), without the race
// detector and never from go test's cache, through the empty-run guard. It
// prints its start seed and counts before go test starts, so a later rerun
// of the night can pass that start, and the seeds that failed at the end,
// each as the command that replays it, a seed whose test was ended before
// it printed its replay line included.
func runSeedsVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	opts *VerifyOptions,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	start := opts.SeedStart
	if start == 0 {
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		start = nightlySeedStart(now())
	}

	return collector.runStepWithOutcome(seedsStepName, func() (stepOutcome, error) {
		status := fmt.Sprintf("==> model seeds start=%d watch=%d engine and crash=%d planner=%d chaos=%d\n"+
			"==> go test -count=1 -timeout=%s -run %s %s\n",
			start, seedsWatchCount, seedsEngineCount, seedsPlannerCount, seedsChaosCount, seedsTimeout, seedsRunPattern(), syncTestTree)
		if err := writeStatus(stdout, status); err != nil {
			return stepOutcome{}, fmt.Errorf("write status: %w", err)
		}

		output, err := runGoTestMatching(ctx, runner, repoRoot, seedsEnv(env, start), stdout, stderr, string(seedsStepName),
			"test", "-count=1", "-timeout="+seedsTimeout, "-run", seedsRunPattern(), syncTestTree)
		outcome := stepOutcome{failedTests: parseGoTestLog(output).failedTopLevelTests()}
		if replays := failingSeedReplays(output); len(replays) > 0 {
			lines := make([]string, 0, len(replays))
			for _, replay := range replays {
				lines = append(lines, "  "+replay.command())
			}
			if writeErr := writeStatus(stdout, "==> failing seeds, each with the command that replays it:\n"+
				strings.Join(lines, "\n")+"\n"); writeErr != nil {
				return outcome, errors.Join(fmt.Errorf("write status: %w", writeErr), err)
			}
		}

		return outcome, err
	})
}
