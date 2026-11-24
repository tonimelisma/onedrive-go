package devtool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// benchTestPutCommand is the CLI command the preparation uploads with.
const benchTestPutCommand = "put"

// Validates: R-6.10.14, R-6.10.15
//
// A run prepares its own remote fixture and removes it after: it deletes
// what an earlier run left under the benchmark's one remote folder, uploads
// the golden fixture from a local copy holding exactly the fixture's files,
// creates the work slots' folder, waits until the golden is visible, and at
// cleanup deletes the folder again. Every remote path it names is inside
// that folder.
func TestBidirectionalScenarioPreparesAndRemovesItsRemoteFixture(t *testing.T) {
	state, fixture := newScriptedBidirectionalSampleState(t)

	var calls []string
	uploaded := false
	subject := preparedBenchSubject{
		measure: func(_ context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			args := spec.Arg[4:]
			calls = append(calls, strings.Join(args, " "))
			if args[0] == benchTestPutCommand {
				uploaded = true
				assert.Equal(t, filepath.Join(spec.CWD, benchLocalGoldenDirName), args[1])
				require.NoError(t, verifyBenchLiveFixture(args[1], &benchLiveFixturePlan{
					Files:       fixture.Files,
					Directories: fixture.Directories,
				}), "the upload's local copy is the fixture")
			}

			return benchMeasuredProcess{}, nil
		},
	}

	require.NoError(t, state.prepareRemoteFixture(t.Context(), subject))
	require.True(t, uploaded)
	require.NotNil(t, state.setupRuntime)
	assert.NoDirExists(t, filepath.Join(state.setupRuntime.rootDir, benchLocalGoldenDirName),
		"the local golden copy is removed once uploaded")
	assert.Equal(t, []string{
		"rm -r /onedrive-go-e2e-bench",
		"mkdir /onedrive-go-e2e-bench/golden",
		"mkdir /onedrive-go-e2e-bench/work/test",
		"put " + filepath.Join(state.setupRuntime.rootDir, benchLocalGoldenDirName) + " /onedrive-go-e2e-bench/golden/test-fixture",
		"stat --json /onedrive-go-e2e-bench/golden/test-fixture",
	}, calls)

	calls = nil
	require.NoError(t, state.cleanup(t.Context(), subject))
	assert.Equal(t, []string{"rm -r /onedrive-go-e2e-bench"}, calls)
	assert.NoDirExists(t, state.workRoot)
}

// Validates: R-6.10.14
//
// A run whose drive the allowlist does not name touches nothing remote: the
// preparation fails before its first command, every sample is a fixture
// failure that says why, and cleanup removes only the local work root.
func TestBidirectionalScenarioOffTheAllowlistTouchesNothingRemote(t *testing.T) {
	state, _ := newScriptedBidirectionalSampleState(t)
	t.Setenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS", "personal:someone-else@example.com")

	commands := 0
	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			commands++
			return benchMeasuredProcess{}, nil
		},
	}

	state.prepareErr = state.prepareRemoteFixture(t.Context(), subject)
	require.Error(t, state.prepareErr)
	assert.Nil(t, state.setupRuntime)

	sample := state.runSample(t.Context(), subject, benchSamplePhaseMeasured, 1)
	assert.Equal(t, BenchSampleFixtureFailed, sample.Status)
	assert.Contains(t, sample.FailureExcerpt, "prepare the remote fixture")

	require.NoError(t, state.cleanup(t.Context(), subject))
	assert.Zero(t, commands, "no command ran against the drive")
	assert.NoDirExists(t, state.workRoot)
}

// Validates: R-6.10.14
//
// The checked-in fixture keeps its golden and work paths inside the
// benchmark's remote folder, which sits under the prefix the e2e preflight
// scrub deletes, and a path outside it, or one that climbs out of it, is
// refused.
func TestBenchRemoteFixturePathsStayInsideTheBenchmarkFolder(t *testing.T) {
	t.Parallel()

	plan, err := loadSyncBidirectionalCatchup100MFixturePlan(benchDefaultFixtureSlot)
	require.NoError(t, err)
	require.NoError(t, validateBenchRemoteFixturePaths(plan.Manifest.GoldenRemoteScopePath, plan.RemoteScopePath))
	assert.True(t, strings.HasPrefix(strings.TrimPrefix(benchRemoteRoot, "/"), "onedrive-go-e2e"),
		"the e2e preflight scrub deletes root folders named onedrive-go-e2e*")

	for _, refused := range []string{
		benchRemoteRoot,
		"/benchmarks/golden/x",
		benchRemoteRoot + "-other/x",
		benchRemoteRoot + "/work/../../x",
		"onedrive-go-e2e-bench/x",
	} {
		assert.Error(t, validateBenchRemoteFixturePaths(refused), refused)
	}
}
