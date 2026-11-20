package sync

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Validates: R-6.10.25
//
// The seed rule of every seeded model family: a single seed wins over start
// and count, a count runs consecutive seeds from start, and a count below 1 or
// a value that does not parse is refused in one sentence that names the
// variable and its value.
func TestSeedRun_AppliesOneRuleToEveryFamily(t *testing.T) {
	t.Parallel()

	const defaultCount = 3
	vars := syncfixture.SeedVars{Single: "SEED", Count: "SEEDS", Start: "SEED_START"}
	cases := []struct {
		name  string
		env   map[string]string
		want  []uint64
		error []string
	}{
		{name: "defaults", want: []uint64{1, 2, 3}},
		{name: "start", env: map[string]string{"SEED_START": "7001"}, want: []uint64{7001, 7002, 7003}},
		{name: "count", env: map[string]string{"SEEDS": "2"}, want: []uint64{1, 2}},
		{name: "start and count", env: map[string]string{"SEED_START": "40", "SEEDS": "4"}, want: []uint64{40, 41, 42, 43}},
		{name: "single wins", env: map[string]string{"SEED": "9", "SEEDS": "5", "SEED_START": "40"}, want: []uint64{9}},
		{name: "zero count", env: map[string]string{"SEEDS": "0"}, error: []string{"SEEDS=0", "at least one seed"}},
		{name: "negative count", env: map[string]string{"SEEDS": "-3"}, error: []string{"SEEDS=-3", "at least one seed"}},
		{name: "unparsable count", env: map[string]string{"SEEDS": "many"}, error: []string{`SEEDS="many"`}},
		{name: "unparsable start", env: map[string]string{"SEED_START": "-1"}, error: []string{`SEED_START="-1"`}},
		{name: "unparsable single", env: map[string]string{"SEED": "x7"}, error: []string{`SEED="x7"`}},
		{
			name:  "past the largest seed",
			env:   map[string]string{"SEED_START": "18446744073709551615"},
			error: []string{"SEED_START=18446744073709551615", "past the largest seed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			seeds, err := syncfixture.SeedRun(func(name string) string { return tc.env[name] }, vars, defaultCount)
			if len(tc.error) == 0 {
				require.NoError(t, err)
				assert.Equal(t, tc.want, seeds)
				return
			}
			require.Error(t, err)
			for _, part := range tc.error {
				assert.Contains(t, err.Error(), part)
			}
			assert.NotContains(t, err.Error(), "\n", "one sentence")
		})
	}
}

// Validates: R-6.10.25
//
// The helper changed no family's plain run: with none of their variables set,
// each runs exactly the seeds it ran before, with and without the race
// detector.
func TestSeededFamilies_DefaultRunsAreUnchanged(t *testing.T) {
	for _, vars := range []syncfixture.SeedVars{modelSeedVars(), syncfixture.PlannerSeedVars(), chaosSeedVars()} {
		t.Setenv(vars.Single, "")
		t.Setenv(vars.Count, "")
		t.Setenv(vars.Start, "")
	}

	want := map[string][]uint64{
		"engine":  {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		"crash":   {1, 2, 3, 4},
		"planner": {1, 2, 3, 4, 5, 6, 7, 8},
		"chaos":   {1, 2},
	}
	if syncfixture.RaceDetectorEnabled {
		want = map[string][]uint64{"engine": {1, 2}, "crash": {1}, "planner": {1}, "chaos": {2}}
	}
	assert.Equal(t, want, map[string][]uint64{
		"engine":  modelSeeds(t),
		"crash":   modelCrashSeeds(t),
		"planner": syncfixture.PlannerModelSeeds(t),
		"chaos":   chaosSeeds(t),
	})
}

// Validates: R-6.10.25
//
// A failing seed of each of this package's families logs exactly one replay
// line, and the line is a complete command: its assignments are the failing
// run's variable and seed, it carries -race exactly when this binary has the
// race detector, it names a top-level test of this package, and its package
// path is this package from the module root. A passing seed logs nothing.
func TestLogSeedReplayOnFailure_LogsTheCommandThatRerunsTheSeed(t *testing.T) {
	t.Parallel()

	const seed = 7003
	families := map[string]string{
		"TestRunOnce_RandomizedBidirectionalEditsConverge": modelSeedVars().Single,
		"TestModel_CrashAtSeededPointsRecovers":            modelSeedVars().Single,
		"TestModel_ChaosGraphConverges":                    chaosSeedVars().Single,
	}
	sources := syncfixture.PackageTestSources(t)
	for test, envName := range families {
		passing := &syncfixture.FakeSeedReplayTB{TestName: test + "/seed=7003"}
		syncfixture.LogSeedReplayOnFailure(passing, envName, seed)
		passing.End()
		assert.Empty(t, passing.Logs(), "a passing seed logs no replay line")

		failing := &syncfixture.FakeSeedReplayTB{TestName: test + "/seed=7003", HasFailed: true}
		syncfixture.LogSeedReplayOnFailure(failing, envName, seed)
		failing.End()
		require.Len(t, failing.Logs(), 1)
		command, found := strings.CutPrefix(failing.Logs()[0], syncfixture.SeedReplayPrefix)
		require.True(t, found, "the line starts with %q", syncfixture.SeedReplayPrefix)

		replay := syncfixture.ParseSeedReplayCommand(t, command)
		want := []string{envName + "=7003"}
		if order := os.Getenv(syncfixture.DispatchOrderEnv); order != "" {
			want = append(want, syncfixture.DispatchOrderEnv+"="+order)
		}
		assert.Equal(t, want, replay.Assignments)
		assert.Equal(t, syncfixture.RaceDetectorEnabled, replay.Race)
		assert.Equal(t, "^"+test+"$", replay.RunPattern)
		assert.Contains(t, sources, "func "+test+"(t *testing.T)", "the replay names a test of this package")
		syncfixture.AssertIsThisPackage(t, replay.Pkg)
	}

	assert.Equal(t, "seed-replay: A=1 B=two go test -race -run '^TestX$' ./internal/sync",
		syncfixture.SeedReplayLine("TestX", "./internal/sync", true, []syncfixture.SeedAssignment{{Name: "A", Value: "1"}, {Name: "B", Value: "two"}}),
		"a second input that changes what a seed means is a second assignment")
}

// Validates: R-6.10.25
//
// The dispatch order replaces a seed's own shuffle in the engine, crash and
// chaos families, so a seed that failed under it replays only with it: the
// line carries it as a second assignment when the failing run had it set, and
// does not when it had not.
func TestLogSeedReplayOnFailure_CarriesTheDispatchOrder(t *testing.T) {
	for _, order := range []string{"", "reverse"} {
		t.Setenv(syncfixture.DispatchOrderEnv, order)
		failing := &syncfixture.FakeSeedReplayTB{TestName: "TestModel_CrashAtSeededPointsRecovers/seed=9", HasFailed: true}
		syncfixture.LogSeedReplayOnFailure(failing, modelSeedEnv, 9)
		failing.End()

		require.Len(t, failing.Logs(), 1, order)
		command, found := strings.CutPrefix(failing.Logs()[0], syncfixture.SeedReplayPrefix)
		require.True(t, found, failing.Logs()[0])
		want := []string{modelSeedEnv + "=9"}
		if order != "" {
			want = append(want, syncfixture.DispatchOrderEnv+"="+order)
		}
		assert.Equal(t, want, syncfixture.ParseSeedReplayCommand(t, command).Assignments, "order %q", order)
	}
}
