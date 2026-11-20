package sync

import (
	"fmt"
	"os"
	"path/filepath"
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

// fakeSeedReplayTB is a seed's subtest as logSeedReplayOnFailure sees it.
type fakeSeedReplayTB struct {
	name     string
	failed   bool
	cleanups []func()
	logs     []string
}

func (f *fakeSeedReplayTB) Helper()                {}
func (f *fakeSeedReplayTB) Name() string           { return f.name }
func (f *fakeSeedReplayTB) Failed() bool           { return f.failed }
func (f *fakeSeedReplayTB) Cleanup(cleanup func()) { f.cleanups = append(f.cleanups, cleanup) }
func (f *fakeSeedReplayTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

// end runs the registered cleanups as the testing package does when the
// subtest ends: the last registered first.
func (f *fakeSeedReplayTB) end() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// Validates: R-6.10.25
//
// A failing seed of each family logs exactly one replay line, and the line is
// a complete command: its assignments are the failing run's variable and
// seed, it carries -race exactly when this binary has the race detector, it
// names a top-level test of this package, and its package path is this
// package from the module root. A passing seed logs nothing.
func TestLogSeedReplayOnFailure_LogsTheCommandThatRerunsTheSeed(t *testing.T) {
	t.Parallel()

	const seed = 7003
	families := map[string]string{
		"TestRunOnce_RandomizedBidirectionalEditsConverge": modelSeedVars().Single,
		"TestModel_CrashAtSeededPointsRecovers":            modelSeedVars().Single,
		"TestPlannerModel_RandomTriplesConverge":           syncfixture.PlannerSeedVars().Single,
		"TestModel_ChaosGraphConverges":                    chaosSeedVars().Single,
	}
	sources := packageTestSources(t)
	for test, envName := range families {
		passing := &fakeSeedReplayTB{name: test + "/seed=7003"}
		syncfixture.LogSeedReplayOnFailure(passing, envName, seed)
		passing.end()
		assert.Empty(t, passing.logs, "a passing seed logs no replay line")

		failing := &fakeSeedReplayTB{name: test + "/seed=7003", failed: true}
		syncfixture.LogSeedReplayOnFailure(failing, envName, seed)
		failing.end()
		require.Len(t, failing.logs, 1)
		command, found := strings.CutPrefix(failing.logs[0], syncfixture.SeedReplayPrefix)
		require.True(t, found, "the line starts with %q", syncfixture.SeedReplayPrefix)

		replay := parseSeedReplayCommand(t, command)
		want := []string{envName + "=7003"}
		if order := os.Getenv(syncfixture.DispatchOrderEnv); order != "" {
			want = append(want, syncfixture.DispatchOrderEnv+"="+order)
		}
		assert.Equal(t, want, replay.assignments)
		assert.Equal(t, syncfixture.RaceDetectorEnabled, replay.race)
		assert.Equal(t, "^"+test+"$", replay.runPattern)
		assert.Contains(t, sources, "func "+test+"(t *testing.T)", "the replay names a test of this package")
		assertIsThisPackage(t, replay.pkg)
	}

	assert.Equal(t, "seed-replay: A=1 B=two go test -race -run '^TestX$' ./internal/sync",
		syncfixture.SeedReplayLine("TestX", true, []syncfixture.SeedAssignment{{Name: "A", Value: "1"}, {Name: "B", Value: "two"}}),
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
		failing := &fakeSeedReplayTB{name: "TestModel_CrashAtSeededPointsRecovers/seed=9", failed: true}
		syncfixture.LogSeedReplayOnFailure(failing, modelSeedEnv, 9)
		failing.end()

		require.Len(t, failing.logs, 1, order)
		command, found := strings.CutPrefix(failing.logs[0], syncfixture.SeedReplayPrefix)
		require.True(t, found, failing.logs[0])
		want := []string{modelSeedEnv + "=9"}
		if order != "" {
			want = append(want, syncfixture.DispatchOrderEnv+"="+order)
		}
		assert.Equal(t, want, parseSeedReplayCommand(t, command).assignments, "order %q", order)
	}
}

// seedReplayCommand is a replay line's command, read as a shell reads it.
type seedReplayCommand struct {
	assignments []string
	race        bool
	runPattern  string
	pkg         string
}

// parseSeedReplayCommand reads the command after the replay prefix: the
// assignments, then `go test`, -race or not, -run and its single-quoted
// pattern, and the package, nothing else.
func parseSeedReplayCommand(t *testing.T, command string) seedReplayCommand {
	t.Helper()

	words := shellWords(t, command)
	var replay seedReplayCommand
	for len(words) > 0 && strings.Contains(words[0], "=") {
		replay.assignments = append(replay.assignments, words[0])
		words = words[1:]
	}
	goTest := []string{"go", "test"}
	require.Greater(t, len(words), len(goTest), command)
	require.Equal(t, goTest, words[:len(goTest)], command)
	words = words[len(goTest):]
	if words[0] == "-race" {
		replay.race = true
		words = words[1:]
	}
	// -run, its pattern and the package, nothing after them.
	const tail = 3
	require.Len(t, words, tail, command)
	require.Equal(t, "-run", words[0], command)
	replay.runPattern, replay.pkg = words[1], words[2]

	return replay
}

// shellWords splits command into words as a POSIX shell does for what a
// replay line holds: blanks separate words, and single quotes keep a word's
// characters as they are. An unclosed quote fails the test.
func shellWords(t *testing.T, command string) []string {
	t.Helper()

	words := []string{}
	var word strings.Builder
	inWord, quoted := false, false
	for _, r := range command {
		switch {
		case r == '\'':
			quoted, inWord = !quoted, true
		case r == ' ' && !quoted:
			if inWord {
				words = append(words, word.String())
				word.Reset()
			}
			inWord = false
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	require.False(t, quoted, "unclosed quote in %q", command)
	if inWord {
		words = append(words, word.String())
	}

	return words
}

// packageTestSources is the text of this package's test files.
func packageTestSources(t *testing.T) string {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var sources strings.Builder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(entry.Name())
		require.NoError(t, readErr)
		sources.Write(data)
	}

	return sources.String()
}

// assertIsThisPackage holds that pkg, a path from the module root, is the
// package this test runs in: go test runs a package's tests in its directory,
// two levels below the module root.
func assertIsThisPackage(t *testing.T, pkg string) {
	t.Helper()

	here, err := filepath.Abs(".")
	require.NoError(t, err)
	named, err := filepath.Abs(filepath.Join("..", "..", pkg))
	require.NoError(t, err)
	assert.Equal(t, here, named, "the replay runs this package")
	_, err = os.Stat(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err, "the module root is two levels up")
}
