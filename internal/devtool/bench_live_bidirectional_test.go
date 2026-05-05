package devtool

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type benchStaticCursorReader struct {
	cursor string
}

const benchTestSyncCommand = "sync"

func (r benchStaticCursorReader) readObservationCursor(context.Context, *benchBidirectionalRuntime) (string, error) {
	return r.cursor, nil
}

// Validates: R-6.10.14
func TestLoadSyncBidirectionalCatchup100MFixturePlanIsDeterministicAndSized(t *testing.T) {
	t.Parallel()

	planA, err := loadSyncBidirectionalCatchup100MFixturePlan(benchDefaultFixtureSlot)
	require.NoError(t, err)

	planB, err := loadSyncBidirectionalCatchup100MFixturePlan(benchDefaultFixtureSlot)
	require.NoError(t, err)

	assert.Equal(t, planA, planB)
	assert.Equal(t, syncBidirectionalCatchup100MID, planA.Manifest.ScenarioID)
	assert.Equal(t, "sync-bidirectional-catchup-100m-v1", planA.Manifest.FixtureID)
	assert.Equal(t, "/benchmarks/work/sync-bidirectional-catchup-100m-v1/slot-00", planA.RemoteScopePath)
	assert.Equal(t, "benchmarks/work/sync-bidirectional-catchup-100m-v1/slot-00", planA.ScopeRootRelativePath)
	assert.Equal(t, 2668, planA.Denominators.FileCount)
	assert.Equal(t, 113, planA.Denominators.DirectoryCount)
	assert.Equal(t, int64(100<<20), sumBenchLiveFixtureBytes(planA.Files))
	assert.Len(t, planA.ExpectedFiles, 2668)
	assert.Equal(t, 192, planA.Denominators.ChangedItemCount)
	assert.Equal(t, 96, planA.Denominators.LocalChangedItemCount)
	assert.Equal(t, 96, planA.Denominators.RemoteChangedItemCount)
	assert.Equal(t, 80, planA.Denominators.ExpectedUploads)
	assert.Equal(t, 80, planA.Denominators.ExpectedDownloads)
	assert.Equal(t, 16, planA.Denominators.ExpectedLocalDeletes)
	assert.Equal(t, 16, planA.Denominators.ExpectedRemoteDeletes)
	assert.Zero(t, planA.Denominators.ExpectedConflicts)
}

// Validates: R-6.10.14
func TestSyncBidirectionalCatchupMutationSelectionIsDisjoint(t *testing.T) {
	t.Parallel()

	plan, err := loadSyncBidirectionalCatchup100MFixturePlan(benchDefaultFixtureSlot)
	require.NoError(t, err)

	assert.Len(t, plan.Mutations.Remote.Deletes, 16)
	assert.Len(t, plan.Mutations.Remote.Modifies, 64)
	assert.Len(t, plan.Mutations.Remote.Creates, 16)
	assert.Len(t, plan.Mutations.Local.Deletes, 16)
	assert.Len(t, plan.Mutations.Local.Modifies, 64)
	assert.Len(t, plan.Mutations.Local.Creates, 16)

	seen := map[string]string{}
	record := func(side string, entries []benchLiveMutationEntry) {
		for _, entry := range entries {
			if previous, ok := seen[entry.File.RelativePath]; ok {
				assert.Failf(t, "mutation overlap", "%s overlaps %s at %s", side, previous, entry.File.RelativePath)
			}
			seen[entry.File.RelativePath] = side
		}
	}
	record("remote delete", plan.Mutations.Remote.Deletes)
	record("remote modify", plan.Mutations.Remote.Modifies)
	record("local delete", plan.Mutations.Local.Deletes)
	record("local modify", plan.Mutations.Local.Modifies)

	assert.Equal(t, "assets/tier-00/set-00/assets-00003.bin", plan.Mutations.Remote.Deletes[0].File.RelativePath)
	assert.Equal(t, "assets/tier-00/set-00/assets-00005.bin", plan.Mutations.Remote.Modifies[0].File.RelativePath)
	assert.Equal(t, "assets/tier-00/set-00/assets-00007.bin", plan.Mutations.Local.Deletes[0].File.RelativePath)
	assert.Equal(t, "assets/tier-00/set-01/assets-00011.bin", plan.Mutations.Local.Modifies[0].File.RelativePath)
}

// Validates: R-6.10.14
func TestCreateBenchBidirectionalRuntimeWritesScopedIncludedDirs(t *testing.T) {
	t.Parallel()

	workRoot := t.TempDir()
	credentialDir := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(credentialDir, "token_personal_user@example.com.json"),
		[]byte(`{"token":"x"}`),
	))

	runtime, err := createBenchBidirectionalRuntime(
		workRoot,
		credentialDir,
		"personal:user@example.com",
		"benchmarks/work/sync-bidirectional-catchup-100m-v1/slot-00",
		"/benchmarks/work/sync-bidirectional-catchup-100m-v1/slot-00",
	)
	require.NoError(t, err)

	configBody, err := readFile(runtime.configPath)
	require.NoError(t, err)
	assert.Contains(t, string(configBody), `["personal:user@example.com"]`)
	assert.Contains(t, string(configBody), `included_dirs = ["benchmarks/work/sync-bidirectional-catchup-100m-v1/slot-00"]`)
	assert.Equal(t,
		filepath.Join(benchRuntimeEnvValue(runtime.env, "XDG_DATA_HOME"), "onedrive-go", "state_personal_user@example.com.db"),
		runtime.stateDBPath,
	)
}

// Validates: R-6.10.14
func TestApplyBenchLocalMutationsProducesExpectedTree(t *testing.T) {
	t.Parallel()

	plan, err := loadSyncBidirectionalCatchup100MFixturePlan(benchDefaultFixtureSlot)
	require.NoError(t, err)

	scopeRoot := t.TempDir()
	require.NoError(t, materializeBenchLiveFixture(scopeRoot, plan.Files))

	require.NoError(t, applyBenchLocalMutations(scopeRoot, plan.Mutations.Local))

	expectedAfterLocal, expectedDirs := buildBenchBidirectionalExpectedTree(
		plan.Files,
		plan.Directories,
		&benchBidirectionalMutationPlan{Local: plan.Mutations.Local},
	)
	require.NoError(t, verifyBenchLiveFixture(scopeRoot, &benchLiveFixturePlan{
		Files:       expectedAfterLocal,
		Directories: expectedDirs,
	}))
}

// Validates: R-6.10.14
func TestWaitForBenchRemoteDeltaReadyRequiresExpectedRemotePlanAndStableCursor(t *testing.T) {
	t.Parallel()

	expected := benchBidirectionalSideMutations{
		Deletes: []benchLiveMutationEntry{
			{File: benchLiveFileEntry{RelativePath: "docs/deleted.txt", SizeBytes: 1}},
		},
		Modifies: []benchLiveMutationEntry{
			{File: benchLiveFileEntry{RelativePath: "docs/modified.txt", SizeBytes: 1}},
		},
		Creates: []benchLiveFileEntry{
			{RelativePath: "remote-created/new.bin", SizeBytes: 1},
		},
	}

	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			return benchMeasuredProcess{
				Stdout: []byte("Dry run — no changes applied\n\nPlan:\n  Downloads:       2\n  Local deletes:   1\n"),
			}, nil
		},
	}

	runtime := benchBidirectionalRuntime{}
	result, err := waitForBenchRemoteDeltaReady(
		t.Context(),
		subject,
		&runtime,
		expected,
		benchStaticCursorReader{cursor: "cursor-a"},
		"cursor-a",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Attempts)
	assert.GreaterOrEqual(t, result.WaitMicros, int64(0))
}

// Validates: R-6.10.14
func TestWaitForBenchRemoteDeltaReadyFailsIfDryRunAdvancesCursor(t *testing.T) {
	t.Parallel()

	subject := preparedBenchSubject{
		measure: func(context.Context, benchCommandSpec) (benchMeasuredProcess, error) {
			return benchMeasuredProcess{Stdout: []byte("Plan:\n  Downloads: 1\n")}, nil
		},
	}

	runtime := benchBidirectionalRuntime{}
	_, err := waitForBenchRemoteDeltaReady(
		t.Context(),
		subject,
		&runtime,
		benchBidirectionalSideMutations{
			Modifies: []benchLiveMutationEntry{{File: benchLiveFileEntry{RelativePath: "a", SizeBytes: 1}}},
		},
		benchStaticCursorReader{cursor: "cursor-b"},
		"cursor-a",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "advanced observation cursor")
}

// Validates: R-6.10.14
func TestRunBidirectionalSampleCommandOrderAndPerfSummary(t *testing.T) {
	workRoot := t.TempDir()
	credentialDir := filepath.Join(workRoot, ".testdata")
	require.NoError(t, mkdirAll(credentialDir, 0o700))
	require.NoError(t, writeFile(
		filepath.Join(credentialDir, "token_personal_user@example.com.json"),
		[]byte(`{"token":"x"}`),
	))
	t.Setenv("ONEDRIVE_TEST_DRIVE", "personal:user@example.com")
	t.Setenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS", "personal:user@example.com")
	t.Setenv("ONEDRIVE_TEST_DRIVE_2", "")

	fixture := benchBidirectionalFixturePlan{
		Manifest: benchBidirectionalFixtureManifest{
			Version:   1,
			FixtureID: "test-fixture",
		},
		RemoteScopePath:       "/benchmarks/work/test/slot-00",
		ScopeRootRelativePath: "benchmarks/work/test/slot-00",
		Files: []benchLiveFileEntry{
			{RelativePath: "docs/a.txt", SizeBytes: 64},
			{RelativePath: "docs/b.txt", SizeBytes: 64},
			{RelativePath: "docs/c.txt", SizeBytes: 64},
			{RelativePath: "docs/d.txt", SizeBytes: 64},
		},
		Directories: []string{"docs"},
		Mutations: benchBidirectionalMutationPlan{
			Remote: benchBidirectionalSideMutations{
				Deletes:  []benchLiveMutationEntry{{File: benchLiveFileEntry{RelativePath: "docs/a.txt", SizeBytes: 64}}},
				Modifies: []benchLiveMutationEntry{{File: benchLiveFileEntry{RelativePath: "docs/b.txt", SizeBytes: 64}, TruncateToBytes: 0}},
				Creates:  []benchLiveFileEntry{{RelativePath: "remote-created/r.bin", SizeBytes: 8}},
			},
			Local: benchBidirectionalSideMutations{
				Deletes:  []benchLiveMutationEntry{{File: benchLiveFileEntry{RelativePath: "docs/c.txt", SizeBytes: 64}}},
				Modifies: []benchLiveMutationEntry{{File: benchLiveFileEntry{RelativePath: "docs/d.txt", SizeBytes: 64}, TruncateToBytes: 0}},
				Creates:  []benchLiveFileEntry{{RelativePath: "local-created/l.bin", SizeBytes: 8}},
			},
		},
	}
	fixture.ExpectedFiles, fixture.ExpectedDirectories = buildBenchBidirectionalExpectedTree(
		fixture.Files,
		fixture.Directories,
		&fixture.Mutations,
	)

	state := &benchBidirectionalScenarioState{
		repoRoot:     workRoot,
		fixture:      fixture,
		workRoot:     t.TempDir(),
		fixtureSlot:  "slot-00",
		cursorReader: benchStaticCursorReader{cursor: "cursor-a"},
	}

	calls := make([]string, 0, 10)
	syncRuns := 0
	subject := preparedBenchSubject{
		measure: func(_ context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			calls = append(calls, fmt.Sprint(spec.Arg))
			scopeRoot := filepath.Join(spec.CWD, "sync-root", filepath.FromSlash(fixture.ScopeRootRelativePath))
			logPath := filepath.Join(spec.CWD, "bench.log")

			switch {
			case len(spec.Arg) >= 5 && spec.Arg[4] == "stat":
				return benchMeasuredProcess{}, nil
			case len(spec.Arg) >= 5 && spec.Arg[4] == benchTestSyncCommand && len(spec.Arg) == 6 && spec.Arg[5] == "--dry-run":
				if len(calls) < 4 {
					return benchMeasuredProcess{Stdout: []byte("Plan:\n  Baseline updates: 4\n")}, nil
				}

				return benchMeasuredProcess{Stdout: []byte("Plan:\n  Downloads: 2\n  Local deletes: 1\n")}, nil
			case len(spec.Arg) >= 5 && spec.Arg[4] == benchTestSyncCommand:
				syncRuns++
				if syncRuns == 2 {
					require.NoError(t, removeAll(scopeRoot))
					require.NoError(t, materializeBenchLiveFixture(scopeRoot, fixture.ExpectedFiles))
					require.NoError(t, writeBenchLivePerfLog(logPath, &benchPerfSummary{
						DurationMS:    42,
						Result:        "success",
						DownloadCount: 2,
						UploadCount:   2,
					}))
				}

				return benchMeasuredProcess{ElapsedMicros: 1234, PeakRSSBytes: 4096}, nil
			case len(spec.Arg) >= 5 && (spec.Arg[4] == "rm" || spec.Arg[4] == "mkdir" || spec.Arg[4] == "put"):
				return benchMeasuredProcess{}, nil
			default:
				return benchMeasuredProcess{}, nil
			}
		},
	}

	sample := state.runSample(t.Context(), subject, benchSamplePhaseMeasured, 1)
	assert.Equal(t, BenchSampleSuccess, sample.Status)
	require.NotNil(t, sample.PerfSummary)
	require.NotNil(t, sample.Setup)
	assert.EqualValues(t, 42, sample.PerfSummary.DurationMS)
	assert.Equal(t, 1, sample.Setup.DeltaProbeAttempts)
	assert.Contains(t, calls[0], "stat")
	assert.Contains(t, calls[1], "sync --dry-run")
	assert.Contains(t, calls[2], "sync]")
}
