package devtool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	benchBidirectionalFixtureControllerID = "onedrive-go-fixture-controller"
	benchBidirectionalDeltaPollTimeout    = 3 * time.Minute
	benchDefaultFixtureSlotCount          = 6
)

type benchBidirectionalFixtureManifest struct {
	Version                     int                            `json:"version"`
	ScenarioID                  string                         `json:"scenario_id"`
	FixtureID                   string                         `json:"fixture_id"`
	GoldenRemoteScopePath       string                         `json:"golden_remote_scope_path"`
	WorkRemoteScopePathTemplate string                         `json:"work_remote_scope_path_template"`
	Groups                      []benchLiveFixtureGroup        `json:"groups"`
	RemoteMutation              benchBidirectionalSideManifest `json:"remote_mutation"`
	LocalMutation               benchBidirectionalSideManifest `json:"local_mutation"`
}

type benchBidirectionalSideManifest struct {
	DeleteSelector benchLiveMutationSelector `json:"delete_selector"`
	ModifySelector benchLiveMutationSelector `json:"modify_selector"`
	CreateGroup    benchLiveCreateGroup      `json:"create_group"`
}

type benchLiveCreateGroup struct {
	Directory string `json:"directory"`
	FileCount int    `json:"file_count"`
	SizeBytes int64  `json:"size_bytes"`
	Extension string `json:"extension"`
}

type benchBidirectionalSideMutations struct {
	Deletes  []benchLiveMutationEntry `json:"deletes"`
	Modifies []benchLiveMutationEntry `json:"modifies"`
	Creates  []benchLiveFileEntry     `json:"creates"`
}

type benchBidirectionalMutationPlan struct {
	Remote benchBidirectionalSideMutations `json:"remote"`
	Local  benchBidirectionalSideMutations `json:"local"`
}

type benchBidirectionalFixturePlan struct {
	Manifest              benchBidirectionalFixtureManifest `json:"manifest"`
	RemoteScopePath       string                            `json:"remote_scope_path"`
	ScopeRootRelativePath string                            `json:"scope_root_relative_path"`
	Files                 []benchLiveFileEntry              `json:"files"`
	ExpectedFiles         []benchLiveFileEntry              `json:"expected_files"`
	Directories           []string                          `json:"directories"`
	ExpectedDirectories   []string                          `json:"expected_directories"`
	Mutations             benchBidirectionalMutationPlan    `json:"mutations"`
	Denominators          BenchDenominator                  `json:"denominators"`
}

type benchBidirectionalScenarioState struct {
	repoRoot     string
	fixture      benchBidirectionalFixturePlan
	workRoot     string
	fixtureSlot  string
	cursorReader benchObservationCursorReader
}

type benchBidirectionalRuntime struct {
	rootDir          string
	syncDir          string
	configPath       string
	logPath          string
	driveID          string
	includedDir      string
	env              []string
	stateDBPath      string
	remoteScopePath  string
	scopeRelativeDir string
}

type benchObservationCursorReader interface {
	readObservationCursor(context.Context, *benchBidirectionalRuntime) (string, error)
}

type benchSyncStoreObservationCursorReader struct{}

type benchSyncPlanCounts struct {
	FolderCreates   int
	Downloads       int
	Uploads         int
	LocalDeletes    int
	RemoteDeletes   int
	ConflictCopies  int
	BaselineUpdates int
}

type benchDeltaReadyResult struct {
	WaitMicros int64
	Attempts   int
}

func lookupSyncBidirectionalCatchup100MScenario() (benchScenarioDefinition, error) {
	fixture, err := loadSyncBidirectionalCatchup100MFixturePlan(benchDefaultFixtureSlot)
	if err != nil {
		return benchScenarioDefinition{}, fmt.Errorf("load benchmark fixture plan: %w", err)
	}

	return benchScenarioDefinition{
		Spec: BenchScenarioSpec{
			ID:            syncBidirectionalCatchup100MID,
			Class:         "live",
			ConfigProfile: "default-safe",
			DefaultRuns:   syncBidirectionalCatchup100MRuns,
			DefaultWarmup: syncBidirectionalCatchup100MWarm,
			Denominators:  fixture.Denominators,
		},
		Prepare: func(_ context.Context, req benchScenarioPrepareRequest) (preparedBenchScenario, error) {
			fixtureSlot, err := normalizeBenchFixtureSlot(req.FixtureSlot)
			if err != nil {
				return preparedBenchScenario{}, err
			}

			fixture, err := loadSyncBidirectionalCatchup100MFixturePlan(fixtureSlot)
			if err != nil {
				return preparedBenchScenario{}, fmt.Errorf("load benchmark fixture plan: %w", err)
			}

			workRoot, err := mkdirTemp(os.TempDir(), "onedrive-go-bench-bidi-*")
			if err != nil {
				return preparedBenchScenario{}, fmt.Errorf("create live benchmark work root: %w", err)
			}

			state := &benchBidirectionalScenarioState{
				repoRoot:     req.RepoRoot,
				fixture:      fixture,
				workRoot:     workRoot,
				fixtureSlot:  fixtureSlot,
				cursorReader: benchSyncStoreObservationCursorReader{},
			}

			return preparedBenchScenario{
				run: state.runSample,
				cleanup: func() error {
					return removeAll(workRoot)
				},
				setup: &benchResultSetup{
					FixtureID:           fixture.Manifest.FixtureID,
					FixtureVersion:      fixture.Manifest.Version,
					FixtureSlot:         fixtureSlot,
					RemoteScopePath:     fixture.RemoteScopePath,
					IncludedDir:         fixture.ScopeRootRelativePath,
					FixtureControllerID: benchBidirectionalFixtureControllerID,
					PoolStatusBeforeRun: "manual-slot",
				},
			}, nil
		},
	}, nil
}

func normalizeBenchFixtureSlot(slot string) (string, error) {
	if strings.TrimSpace(slot) == "" {
		return benchDefaultFixtureSlot, nil
	}

	for _, valid := range benchFixtureSlotIDs(benchDefaultFixtureSlotCount) {
		if slot == valid {
			return slot, nil
		}
	}

	return "", fmt.Errorf(
		"unknown fixture slot %q (known: %s)",
		slot,
		strings.Join(benchFixtureSlotIDs(benchDefaultFixtureSlotCount), ", "),
	)
}

func benchFixtureSlotIDs(count int) []string {
	slots := make([]string, 0, count)
	for i := 0; i < count; i++ {
		slots = append(slots, fmt.Sprintf("slot-%02d", i))
	}

	return slots
}

func loadSyncBidirectionalCatchup100MFixturePlan(slot string) (benchBidirectionalFixturePlan, error) {
	manifestPath := path.Join("testdata", "bench", syncBidirectionalCatchup100MID+".json")
	data, err := benchFixtureFS.ReadFile(manifestPath)
	if err != nil {
		return benchBidirectionalFixturePlan{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}

	var manifest benchBidirectionalFixtureManifest
	if unmarshalErr := json.Unmarshal(data, &manifest); unmarshalErr != nil {
		return benchBidirectionalFixturePlan{}, fmt.Errorf("decode %s: %w", manifestPath, unmarshalErr)
	}
	if manifest.Version != 1 {
		return benchBidirectionalFixturePlan{}, fmt.Errorf("decode %s: unsupported version %d", manifestPath, manifest.Version)
	}
	if manifest.ScenarioID != syncBidirectionalCatchup100MID {
		return benchBidirectionalFixturePlan{}, fmt.Errorf(
			"decode %s: scenario_id %q does not match %q",
			manifestPath,
			manifest.ScenarioID,
			syncBidirectionalCatchup100MID,
		)
	}

	remoteScopePath := strings.ReplaceAll(manifest.WorkRemoteScopePathTemplate, "{slot}", slot)
	scopeRootRelativePath, err := benchScopeRootRelativePath(remoteScopePath)
	if err != nil {
		return benchBidirectionalFixturePlan{}, fmt.Errorf("decode %s: %w", manifestPath, err)
	}

	files, directories, err := expandBenchLiveFixtureManifest(&benchLiveFixtureManifest{
		Groups: manifest.Groups,
	})
	if err != nil {
		return benchBidirectionalFixturePlan{}, fmt.Errorf("expand %s: %w", manifestPath, err)
	}

	mutations, expectedFiles, expectedDirectories, denominators, err := buildBenchBidirectionalMutationPlan(files, directories, &manifest)
	if err != nil {
		return benchBidirectionalFixturePlan{}, fmt.Errorf("expand %s mutations: %w", manifestPath, err)
	}

	return benchBidirectionalFixturePlan{
		Manifest:              manifest,
		RemoteScopePath:       remoteScopePath,
		ScopeRootRelativePath: scopeRootRelativePath,
		Files:                 files,
		ExpectedFiles:         expectedFiles,
		Directories:           directories,
		ExpectedDirectories:   expectedDirectories,
		Mutations:             mutations,
		Denominators:          denominators,
	}, nil
}

func buildBenchBidirectionalMutationPlan(
	files []benchLiveFileEntry,
	directories []string,
	manifest *benchBidirectionalFixtureManifest,
) (benchBidirectionalMutationPlan, []benchLiveFileEntry, []string, BenchDenominator, error) {
	excluded := map[string]struct{}{}

	remote, err := selectBenchBidirectionalSideMutations(files, manifest.RemoteMutation, "remote", excluded)
	if err != nil {
		return benchBidirectionalMutationPlan{}, nil, nil, BenchDenominator{}, err
	}
	local, err := selectBenchBidirectionalSideMutations(files, manifest.LocalMutation, "local", excluded)
	if err != nil {
		return benchBidirectionalMutationPlan{}, nil, nil, BenchDenominator{}, err
	}

	mutations := benchBidirectionalMutationPlan{Remote: remote, Local: local}
	expectedFiles, expectedDirectories := buildBenchBidirectionalExpectedTree(files, directories, &mutations)
	denominators := buildBenchBidirectionalDenominators(files, directories, &mutations)

	return mutations, expectedFiles, expectedDirectories, denominators, nil
}

func selectBenchBidirectionalSideMutations(
	files []benchLiveFileEntry,
	manifest benchBidirectionalSideManifest,
	side string,
	excluded map[string]struct{},
) (benchBidirectionalSideMutations, error) {
	deletes, err := selectBenchLiveMutations(files, manifest.DeleteSelector, excluded)
	if err != nil {
		return benchBidirectionalSideMutations{}, fmt.Errorf("select %s deletes: %w", side, err)
	}
	markBenchMutationEntriesExcluded(deletes, excluded)

	modifies, err := selectBenchLiveMutations(files, manifest.ModifySelector, excluded)
	if err != nil {
		return benchBidirectionalSideMutations{}, fmt.Errorf("select %s modifies: %w", side, err)
	}
	markBenchMutationEntriesExcluded(modifies, excluded)

	creates, err := buildBenchLiveCreateFiles(side, manifest.CreateGroup)
	if err != nil {
		return benchBidirectionalSideMutations{}, fmt.Errorf("build %s creates: %w", side, err)
	}

	return benchBidirectionalSideMutations{
		Deletes:  deletes,
		Modifies: modifies,
		Creates:  creates,
	}, nil
}

func markBenchMutationEntriesExcluded(entries []benchLiveMutationEntry, excluded map[string]struct{}) {
	for _, entry := range entries {
		excluded[entry.File.RelativePath] = struct{}{}
	}
}

func buildBenchLiveCreateFiles(side string, group benchLiveCreateGroup) ([]benchLiveFileEntry, error) {
	if group.Directory == "" {
		return nil, fmt.Errorf("create directory is empty")
	}
	if group.FileCount <= 0 {
		return nil, fmt.Errorf("create file_count must be > 0")
	}
	if group.SizeBytes <= 0 {
		return nil, fmt.Errorf("create size_bytes must be > 0")
	}
	if group.Extension == "" {
		return nil, fmt.Errorf("create extension is empty")
	}

	files := make([]benchLiveFileEntry, 0, group.FileCount)
	for i := 0; i < group.FileCount; i++ {
		files = append(files, benchLiveFileEntry{
			RelativePath: filepath.ToSlash(filepath.Join(
				group.Directory,
				fmt.Sprintf("%s-created-%05d.%s", side, i, group.Extension),
			)),
			SizeBytes: group.SizeBytes,
		})
	}

	return files, nil
}

func buildBenchBidirectionalExpectedTree(
	files []benchLiveFileEntry,
	directories []string,
	mutations *benchBidirectionalMutationPlan,
) ([]benchLiveFileEntry, []string) {
	expected := make(map[string]benchLiveFileEntry, len(files)+len(mutations.Remote.Creates)+len(mutations.Local.Creates))
	for _, file := range files {
		expected[file.RelativePath] = file
	}

	applyExpectedSideMutations(expected, mutations.Remote)
	applyExpectedSideMutations(expected, mutations.Local)

	expectedFiles := make([]benchLiveFileEntry, 0, len(expected))
	for _, file := range expected {
		expectedFiles = append(expectedFiles, file)
	}
	sort.Slice(expectedFiles, func(i, j int) bool {
		return expectedFiles[i].RelativePath < expectedFiles[j].RelativePath
	})

	directorySet := make(map[string]struct{}, len(directories)+2)
	for _, dir := range directories {
		directorySet[dir] = struct{}{}
	}
	for _, file := range expectedFiles {
		parent := path.Dir(file.RelativePath)
		for parent != "." && parent != "/" && parent != "" {
			directorySet[parent] = struct{}{}
			parent = path.Dir(parent)
		}
	}

	expectedDirectories := make([]string, 0, len(directorySet))
	for dir := range directorySet {
		expectedDirectories = append(expectedDirectories, dir)
	}
	sort.Strings(expectedDirectories)

	return expectedFiles, expectedDirectories
}

func applyExpectedSideMutations(
	expected map[string]benchLiveFileEntry,
	mutations benchBidirectionalSideMutations,
) {
	for _, entry := range mutations.Deletes {
		delete(expected, entry.File.RelativePath)
	}
	for _, entry := range mutations.Modifies {
		file := entry.File
		file.SizeBytes = entry.TruncateToBytes
		expected[file.RelativePath] = file
	}
	for _, entry := range mutations.Creates {
		expected[entry.RelativePath] = entry
	}
}

func buildBenchBidirectionalDenominators(
	files []benchLiveFileEntry,
	directories []string,
	mutations *benchBidirectionalMutationPlan,
) BenchDenominator {
	localItems, localBytes := benchSideMutationTotals(mutations.Local)
	remoteItems, remoteBytes := benchSideMutationTotals(mutations.Remote)

	expectedUploads := len(mutations.Local.Modifies) + len(mutations.Local.Creates)
	expectedDownloads := len(mutations.Remote.Modifies) + len(mutations.Remote.Creates)
	expectedLocalDeletes := len(mutations.Remote.Deletes)
	expectedRemoteDeletes := len(mutations.Local.Deletes)

	return BenchDenominator{
		FileCount:              len(files),
		DirectoryCount:         len(directories),
		ChangedItemCount:       localItems + remoteItems,
		ChangedByteCount:       localBytes + remoteBytes,
		ExpectedTransfers:      expectedUploads + expectedDownloads,
		ExpectedDeletes:        expectedLocalDeletes + expectedRemoteDeletes,
		LocalChangedItemCount:  localItems,
		LocalChangedByteCount:  localBytes,
		RemoteChangedItemCount: remoteItems,
		RemoteChangedByteCount: remoteBytes,
		ExpectedUploads:        expectedUploads,
		ExpectedDownloads:      expectedDownloads,
		ExpectedLocalDeletes:   expectedLocalDeletes,
		ExpectedRemoteDeletes:  expectedRemoteDeletes,
		ExpectedConflicts:      0,
	}
}

func benchSideMutationTotals(mutations benchBidirectionalSideMutations) (int, int64) {
	count := len(mutations.Deletes) + len(mutations.Modifies) + len(mutations.Creates)
	var bytesChanged int64
	for _, entry := range mutations.Deletes {
		bytesChanged += entry.File.SizeBytes
	}
	for _, entry := range mutations.Modifies {
		bytesChanged += entry.File.SizeBytes
	}
	for _, entry := range mutations.Creates {
		bytesChanged += entry.SizeBytes
	}

	return count, bytesChanged
}

func (s *benchBidirectionalScenarioState) runSample(
	ctx context.Context,
	subject preparedBenchSubject,
	phase benchSamplePhase,
	iteration int,
) benchSample {
	sample := benchSample{
		Iteration: iteration,
		Phase:     phase,
		Status:    BenchSampleSuccess,
		Setup:     &benchSampleSetup{},
	}

	runtime, err := s.newSampleRuntime()
	if err != nil {
		return benchFixtureFailureSample(sample, err)
	}

	scopeRoot := filepath.Join(runtime.syncDir, filepath.FromSlash(s.fixture.ScopeRootRelativePath))
	preparedSample, ok := s.prepareBidirectionalCatchup(ctx, subject, &runtime, scopeRoot, sample)
	if !ok {
		return finalizeBenchLiveSampleRuntime(runtime.rootDir, preparedSample)
	}

	measuredSample := s.measureBidirectionalCatchup(ctx, subject, &runtime, scopeRoot, preparedSample)

	return finalizeBenchLiveSampleRuntime(runtime.rootDir, measuredSample)
}

//nolint:gocritic // Samples are immutable result records; setup returns a reclassified copy on failure.
func (s *benchBidirectionalScenarioState) prepareBidirectionalCatchup(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime *benchBidirectionalRuntime,
	scopeRoot string,
	sample benchSample,
) (benchSample, bool) {
	if err := materializeBenchLiveFixture(scopeRoot, s.fixture.Files); err != nil {
		return benchFixtureFailureSample(sample, fmt.Errorf("materialize baseline fixture: %w", err)), false
	}

	if err := s.ensureRemoteSlotReadyForBaseline(ctx, subject, runtime); err != nil {
		return benchFixtureFailureSample(sample, err), false
	}

	baseline, baselineErr := subject.measure(ctx, runtime.commandSpec("sync"))
	sample.Setup.BaselineElapsedMicros = baseline.ElapsedMicros
	if baselineErr != nil {
		return classifyBenchProcessFailure(ctx, sample, baseline, baselineErr), false
	}

	baselineCursor, err := s.cursorReader.readObservationCursor(ctx, runtime)
	if err != nil {
		return benchFixtureFailureSample(sample, fmt.Errorf("read post-baseline observation cursor: %w", err)), false
	}

	mutationStartedAt := time.Now()
	if mutationErr := applyBenchRemoteMutations(ctx, subject, runtime, s.fixture.Mutations.Remote); mutationErr != nil {
		return benchFixtureFailureSample(sample, mutationErr), false
	}
	sample.Setup.RemoteMutationElapsedMicros = durationMicros(time.Since(mutationStartedAt))

	deltaReady, err := waitForBenchRemoteDeltaReady(ctx, subject, runtime, s.fixture.Mutations.Remote, s.cursorReader, baselineCursor)
	if err != nil {
		return benchFixtureFailureSample(sample, err), false
	}
	sample.Setup.DeltaReadyWaitMicros = deltaReady.WaitMicros
	sample.Setup.DeltaProbeAttempts = deltaReady.Attempts

	if err := applyBenchLocalMutations(scopeRoot, s.fixture.Mutations.Local); err != nil {
		return benchFixtureFailureSample(sample, err), false
	}

	if err := resetBenchLogFile(runtime.logPath); err != nil {
		return benchFixtureFailureSample(sample, err), false
	}

	return sample, true
}

//nolint:gocritic // Samples are immutable result records; measured execution returns a populated copy.
func (s *benchBidirectionalScenarioState) measureBidirectionalCatchup(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime *benchBidirectionalRuntime,
	scopeRoot string,
	sample benchSample,
) benchSample {
	process, measureErr := subject.measure(ctx, runtime.commandSpec("sync"))
	measuredSample := sampleWithMeasuredProcess(sample, process)
	if measureErr != nil {
		return classifyBenchProcessFailure(ctx, measuredSample, process, measureErr)
	}

	perfSummary, perfErr := readPerformanceSummary(runtime.logPath, process.Stderr)
	if perfErr != nil {
		return benchInvalidSample(measuredSample, perfErr)
	}
	measuredSample.PerfSummary = perfSummary

	verifyErr := verifyBenchLiveFixture(scopeRoot, &benchLiveFixturePlan{
		Files:       s.fixture.ExpectedFiles,
		Directories: s.fixture.ExpectedDirectories,
	})
	if verifyErr != nil {
		return benchInvalidSample(measuredSample, verifyErr)
	}

	return measuredSample
}

func (s *benchBidirectionalScenarioState) ensureRemoteSlotReadyForBaseline(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime *benchBidirectionalRuntime,
) error {
	if err := waitForBenchRemoteScopeVisible(ctx, subject, runtime.asLiveRuntime(), runtime.remoteScopePath); err != nil {
		return fmt.Errorf("remote fixture slot %s is not visible: %w", s.fixtureSlot, err)
	}

	process, err := subject.measure(ctx, runtime.commandSpec("sync", "--dry-run"))
	if err != nil {
		return fmt.Errorf(
			"validate remote fixture slot %s with dry-run: %w: %s",
			s.fixtureSlot,
			err,
			failureExcerpt(err, process.Stdout, process.Stderr),
		)
	}

	plan := parseBenchSyncPlanOutput(process.Stdout, process.Stderr)
	if plan.Downloads != 0 || plan.Uploads != 0 || plan.LocalDeletes != 0 ||
		plan.RemoteDeletes != 0 || plan.ConflictCopies != 0 {
		return fmt.Errorf(
			"remote fixture slot %s is not baseline-ready: dry-run planned uploads=%d downloads=%d local_deletes=%d remote_deletes=%d conflicts=%d",
			s.fixtureSlot,
			plan.Uploads,
			plan.Downloads,
			plan.LocalDeletes,
			plan.RemoteDeletes,
			plan.ConflictCopies,
		)
	}

	return nil
}

func (s *benchBidirectionalScenarioState) newSampleRuntime() (benchBidirectionalRuntime, error) {
	liveConfig, err := loadBenchLiveConfig(s.repoRoot)
	if err != nil {
		return benchBidirectionalRuntime{}, fmt.Errorf("load live benchmark config: %w", err)
	}
	if allowlistErr := validateBenchAllowlist(liveConfig.PrimaryDrive); allowlistErr != nil {
		return benchBidirectionalRuntime{}, allowlistErr
	}

	credentialDir, err := locateBenchCredentialDir(s.repoRoot)
	if err != nil {
		return benchBidirectionalRuntime{}, err
	}

	return createBenchBidirectionalRuntime(
		s.workRoot,
		credentialDir,
		liveConfig.PrimaryDrive,
		s.fixture.ScopeRootRelativePath,
		s.fixture.RemoteScopePath,
	)
}

func createBenchBidirectionalRuntime(
	workRoot string,
	credentialDir string,
	driveID string,
	includedDir string,
	remoteScopePath string,
) (benchBidirectionalRuntime, error) {
	runtimeRoot, err := mkdirTemp(workRoot, "runtime-*")
	if err != nil {
		return benchBidirectionalRuntime{}, fmt.Errorf("create benchmark runtime root: %w", err)
	}

	syncDir := filepath.Join(runtimeRoot, "sync-root")
	homeDir := filepath.Join(runtimeRoot, "home")
	configHome := filepath.Join(runtimeRoot, "config")
	dataHome := filepath.Join(runtimeRoot, "data")
	cacheHome := filepath.Join(runtimeRoot, "cache")
	for _, dir := range []string{homeDir, configHome, dataHome, cacheHome, syncDir} {
		if err := mkdirAll(dir, benchLiveFixtureDirPerm); err != nil {
			return benchBidirectionalRuntime{}, fmt.Errorf("create benchmark runtime path %s: %w", dir, err)
		}
	}

	dataDir := filepath.Join(dataHome, "onedrive-go")
	if err := mkdirAll(dataDir, benchLiveFixtureDirPerm); err != nil {
		return benchBidirectionalRuntime{}, fmt.Errorf("create benchmark data dir: %w", err)
	}
	if err := copyBenchCredentials(credentialDir, dataDir, driveID); err != nil {
		return benchBidirectionalRuntime{}, err
	}

	logPath := filepath.Join(runtimeRoot, "bench.log")
	configPath := filepath.Join(runtimeRoot, "config.toml")
	configBody := fmt.Sprintf(
		"log_level = %q\nlog_file = %q\n\n[%q]\nsync_dir = %q\nincluded_dirs = [%q]\n",
		"info",
		logPath,
		driveID,
		syncDir,
		includedDir,
	)
	if err := writeFile(configPath, []byte(configBody)); err != nil {
		return benchBidirectionalRuntime{}, fmt.Errorf("write benchmark config: %w", err)
	}

	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+configHome,
		"XDG_DATA_HOME="+dataHome,
		"XDG_CACHE_HOME="+cacheHome,
	)

	return benchBidirectionalRuntime{
		rootDir:          runtimeRoot,
		syncDir:          syncDir,
		configPath:       configPath,
		logPath:          logPath,
		driveID:          driveID,
		includedDir:      includedDir,
		env:              env,
		stateDBPath:      benchStateDBPath(dataDir, driveID),
		remoteScopePath:  remoteScopePath,
		scopeRelativeDir: includedDir,
	}, nil
}

func benchStateDBPath(dataDir string, driveID string) string {
	return filepath.Join(dataDir, "state_"+strings.ReplaceAll(driveID, ":", "_")+".db")
}

func (r *benchBidirectionalRuntime) commandSpec(args ...string) benchCommandSpec {
	fullArgs := []string{"--config", r.configPath, "--drive", r.driveID}
	fullArgs = append(fullArgs, args...)

	return benchCommandSpec{
		CWD: r.rootDir,
		Env: r.env,
		Arg: fullArgs,
	}
}

func (r *benchBidirectionalRuntime) asLiveRuntime() benchLiveCommandRuntime {
	return benchLiveCommandRuntime{
		rootDir:    r.rootDir,
		syncDir:    r.syncDir,
		configPath: r.configPath,
		logPath:    r.logPath,
		driveID:    r.driveID,
		env:        r.env,
	}
}

func (benchSyncStoreObservationCursorReader) readObservationCursor(
	ctx context.Context,
	runtime *benchBidirectionalRuntime,
) (cursor string, err error) {
	logger := slog.New(slog.DiscardHandler)
	store, err := syncengine.NewSyncStore(ctx, runtime.stateDBPath, logger)
	if err != nil {
		return "", fmt.Errorf("open sync store for benchmark cursor read: %w", err)
	}
	defer func() {
		closeErr := store.Close(context.WithoutCancel(ctx))
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close sync store after benchmark cursor read: %w", closeErr)
		}
	}()

	state, err := store.ReadObservationState(ctx)
	if err != nil {
		return "", fmt.Errorf("read observation state for benchmark cursor: %w", err)
	}

	return state.Cursor, nil
}

func applyBenchLocalMutations(scopeRoot string, mutations benchBidirectionalSideMutations) error {
	for _, entry := range mutations.Deletes {
		targetPath := filepath.Join(scopeRoot, filepath.FromSlash(entry.File.RelativePath))
		if err := remove(targetPath); err != nil {
			return fmt.Errorf("delete local mutation %s: %w", entry.File.RelativePath, err)
		}
	}
	for _, entry := range mutations.Modifies {
		targetPath := filepath.Join(scopeRoot, filepath.FromSlash(entry.File.RelativePath))
		if err := truncateBenchLiveFile(targetPath, entry.TruncateToBytes); err != nil {
			return fmt.Errorf("modify local mutation %s: %w", entry.File.RelativePath, err)
		}
	}
	if err := materializeBenchLiveFixture(scopeRoot, mutations.Creates); err != nil {
		return fmt.Errorf("create local mutations: %w", err)
	}

	return nil
}

func applyBenchRemoteMutations(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime *benchBidirectionalRuntime,
	mutations benchBidirectionalSideMutations,
) error {
	for _, entry := range mutations.Deletes {
		remotePath := path.Join(runtime.remoteScopePath, entry.File.RelativePath)
		process, err := subject.measure(ctx, runtime.commandSpec("rm", remotePath))
		if err != nil {
			return fmt.Errorf("delete remote mutation %s: %w: %s", remotePath, err, failureExcerpt(err, process.Stdout, process.Stderr))
		}
	}

	for _, entry := range mutations.Modifies {
		remotePath := path.Join(runtime.remoteScopePath, entry.File.RelativePath)
		if err := putBenchRemoteMutationFile(ctx, subject, runtime, remotePath, benchLiveFileEntry{
			RelativePath: entry.File.RelativePath,
			SizeBytes:    entry.TruncateToBytes,
		}); err != nil {
			return err
		}
	}

	createParents := map[string]struct{}{}
	for _, entry := range mutations.Creates {
		remoteParent := path.Dir(path.Join(runtime.remoteScopePath, entry.RelativePath))
		createParents[remoteParent] = struct{}{}
	}
	parents := make([]string, 0, len(createParents))
	for parent := range createParents {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		process, err := subject.measure(ctx, runtime.commandSpec("mkdir", parent))
		if err != nil {
			return fmt.Errorf("create remote mutation parent %s: %w: %s", parent, err, failureExcerpt(err, process.Stdout, process.Stderr))
		}
	}
	for _, entry := range mutations.Creates {
		remotePath := path.Join(runtime.remoteScopePath, entry.RelativePath)
		if err := putBenchRemoteMutationFile(ctx, subject, runtime, remotePath, entry); err != nil {
			return err
		}
	}

	return nil
}

func putBenchRemoteMutationFile(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime *benchBidirectionalRuntime,
	remotePath string,
	entry benchLiveFileEntry,
) error {
	localRoot := filepath.Join(runtime.rootDir, "remote-mutations")
	localPath := filepath.Join(localRoot, filepath.FromSlash(entry.RelativePath))
	if err := mkdirAll(filepath.Dir(localPath), benchLiveFixtureDirPerm); err != nil {
		return fmt.Errorf("create remote mutation parent for %s: %w", entry.RelativePath, err)
	}
	if err := writeBenchLiveFile(localPath, entry); err != nil {
		return err
	}

	process, err := subject.measure(ctx, runtime.commandSpec("put", localPath, remotePath))
	if err != nil {
		return fmt.Errorf("upload remote mutation %s: %w: %s", remotePath, err, failureExcerpt(err, process.Stdout, process.Stderr))
	}

	return nil
}

func waitForBenchRemoteDeltaReady(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime *benchBidirectionalRuntime,
	expected benchBidirectionalSideMutations,
	cursorReader benchObservationCursorReader,
	baselineCursor string,
) (benchDeltaReadyResult, error) {
	startedAt := time.Now()
	deadline := startedAt.Add(benchBidirectionalDeltaPollTimeout)
	var lastProcess benchMeasuredProcess
	var lastErr error

	for attempt := 0; ; attempt++ {
		process, err := subject.measure(ctx, runtime.commandSpec("sync", "--dry-run"))
		lastProcess = process
		lastErr = err
		if err == nil {
			cursor, cursorErr := cursorReader.readObservationCursor(ctx, runtime)
			if cursorErr != nil {
				return benchDeltaReadyResult{}, fmt.Errorf("read observation cursor after dry-run probe: %w", cursorErr)
			}
			if cursor != baselineCursor {
				return benchDeltaReadyResult{}, fmt.Errorf("dry-run probe advanced observation cursor")
			}

			plan := parseBenchSyncPlanOutput(process.Stdout, process.Stderr)
			if benchRemoteDeltaPlanReady(plan, expected) {
				return benchDeltaReadyResult{
					WaitMicros: durationMicros(time.Since(startedAt)),
					Attempts:   attempt + 1,
				}, nil
			}
		}

		if ctx.Err() != nil {
			return benchDeltaReadyResult{}, fmt.Errorf("wait for remote delta readiness: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("remote mutations not yet visible in dry-run plan")
			}

			return benchDeltaReadyResult{}, fmt.Errorf(
				"wait for remote delta readiness timed out after %s: %s",
				benchBidirectionalDeltaPollTimeout,
				failureExcerpt(lastErr, lastProcess.Stdout, lastProcess.Stderr),
			)
		}

		time.Sleep(benchPollBackoff(attempt))
	}
}

func benchRemoteDeltaPlanReady(plan benchSyncPlanCounts, expected benchBidirectionalSideMutations) bool {
	expectedDownloads := len(expected.Modifies) + len(expected.Creates)
	expectedLocalDeletes := len(expected.Deletes)

	return plan.Downloads >= expectedDownloads &&
		plan.LocalDeletes >= expectedLocalDeletes &&
		plan.Uploads == 0 &&
		plan.RemoteDeletes == 0 &&
		plan.ConflictCopies == 0
}

func parseBenchSyncPlanOutput(streams ...[]byte) benchSyncPlanCounts {
	var counts benchSyncPlanCounts
	for _, stream := range streams {
		scanner := bufio.NewScanner(bytes.NewReader(stream))
		for scanner.Scan() {
			label, value, ok := parseBenchSyncCountLine(scanner.Text())
			if !ok {
				continue
			}

			switch label {
			case "folder creates":
				counts.FolderCreates = value
			case "downloads":
				counts.Downloads = value
			case "uploads":
				counts.Uploads = value
			case "local deletes":
				counts.LocalDeletes = value
			case "remote deletes":
				counts.RemoteDeletes = value
			case "conflict copies":
				counts.ConflictCopies = value
			case "baseline updates":
				counts.BaselineUpdates = value
			}
		}
	}

	return counts
}

func parseBenchSyncCountLine(line string) (string, int, bool) {
	left, right, ok := strings.Cut(line, ":")
	if !ok {
		return "", 0, false
	}

	label := strings.ToLower(strings.TrimSpace(left))
	valueText := strings.TrimSpace(right)
	if valueText == "" {
		return "", 0, false
	}

	value, err := strconv.Atoi(strings.Fields(valueText)[0])
	if err != nil {
		return "", 0, false
	}

	return label, value, true
}
