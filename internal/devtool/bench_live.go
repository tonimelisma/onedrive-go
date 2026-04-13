package devtool

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/testutil"
)

const (
	benchLiveFixtureDirPerm      = 0o700
	benchLiveFilePerm            = 0o600
	benchLivePatternBytes        = 4096
	benchLivePollTimeout         = 2 * time.Minute
	benchLiveDeleteRetryAttempts = 4
	benchLivePollBackoffCap      = 4 * time.Second
	benchLiveVerifyBufferBytes   = 32 * 1024
)

//go:embed testdata/bench/*.json
var benchFixtureFS embed.FS

type benchLiveFixtureManifest struct {
	Version          int                       `json:"version"`
	ScenarioID       string                    `json:"scenario_id"`
	RemoteScopePath  string                    `json:"remote_scope_path"`
	Groups           []benchLiveFixtureGroup   `json:"groups"`
	DeleteSelector   benchLiveMutationSelector `json:"delete_selector"`
	TruncateSelector benchLiveMutationSelector `json:"truncate_selector"`
}

type benchLiveFixtureGroup struct {
	Name           string `json:"name"`
	FileCount      int    `json:"file_count"`
	SizeBytes      int64  `json:"size_bytes"`
	TopLevelDirs   int    `json:"top_level_dirs"`
	LeafDirsPerTop int    `json:"leaf_dirs_per_top"`
	Extension      string `json:"extension"`
}

type benchLiveMutationSelector struct {
	Modulus         int   `json:"modulus"`
	Remainder       int   `json:"remainder"`
	Limit           int   `json:"limit"`
	TruncateToBytes int64 `json:"truncate_to_bytes,omitempty"`
}

type benchLiveFileEntry struct {
	RelativePath string `json:"relative_path"`
	SizeBytes    int64  `json:"size_bytes"`
}

type benchLiveMutationEntry struct {
	File            benchLiveFileEntry `json:"file"`
	TruncateToBytes int64              `json:"truncate_to_bytes,omitempty"`
}

type benchLiveMutationPlan struct {
	Deletes   []benchLiveMutationEntry `json:"deletes"`
	Truncates []benchLiveMutationEntry `json:"truncates"`
}

type benchLiveFixturePlan struct {
	Manifest              benchLiveFixtureManifest `json:"manifest"`
	ScopeRootRelativePath string                   `json:"scope_root_relative_path"`
	Files                 []benchLiveFileEntry     `json:"files"`
	Directories           []string                 `json:"directories"`
	Mutations             benchLiveMutationPlan    `json:"mutations"`
	Denominators          BenchDenominator         `json:"denominators"`
}

type benchLiveCommandRuntime struct {
	rootDir    string
	syncDir    string
	configPath string
	logPath    string
	driveID    string
	env        []string
}

type benchLiveScenarioState struct {
	repoRoot string
	fixture  benchLiveFixturePlan
	workRoot string

	setupOnce sync.Once
	setupErr  error
}

func lookupSyncPartialLocalCatchup100MScenario() (benchScenarioDefinition, error) {
	fixture, err := loadSyncPartialLocalCatchup100MFixturePlan()
	if err != nil {
		return benchScenarioDefinition{}, fmt.Errorf("load benchmark fixture plan: %w", err)
	}

	return benchScenarioDefinition{
		Spec: BenchScenarioSpec{
			ID:            syncPartialLocalCatchup100MID,
			Class:         "live",
			ConfigProfile: "default-safe",
			DefaultRuns:   syncPartialLocalCatchup100MRuns,
			DefaultWarmup: syncPartialLocalCatchup100MWarm,
			Denominators:  fixture.Denominators,
		},
		Prepare: func(
			_ context.Context,
			repoRoot string,
			_ preparedBenchSubject,
		) (preparedBenchScenario, error) {
			workRoot, err := localpath.MkdirTemp(os.TempDir(), "onedrive-go-bench-live-*")
			if err != nil {
				return preparedBenchScenario{}, fmt.Errorf("create live benchmark work root: %w", err)
			}

			state := &benchLiveScenarioState{
				repoRoot: repoRoot,
				fixture:  fixture,
				workRoot: workRoot,
			}

			return preparedBenchScenario{
				run: state.runSample,
				cleanup: func() error {
					return localpath.RemoveAll(workRoot)
				},
			}, nil
		},
	}, nil
}

func loadSyncPartialLocalCatchup100MFixturePlan() (benchLiveFixturePlan, error) {
	manifestPath := path.Join("testdata", "bench", syncPartialLocalCatchup100MID+".json")
	data, err := benchFixtureFS.ReadFile(manifestPath)
	if err != nil {
		return benchLiveFixturePlan{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}

	var manifest benchLiveFixtureManifest
	unmarshalErr := json.Unmarshal(data, &manifest)
	if unmarshalErr != nil {
		return benchLiveFixturePlan{}, fmt.Errorf("decode %s: %w", manifestPath, unmarshalErr)
	}
	if manifest.Version != 1 {
		return benchLiveFixturePlan{}, fmt.Errorf("decode %s: unsupported version %d", manifestPath, manifest.Version)
	}
	if manifest.ScenarioID != syncPartialLocalCatchup100MID {
		return benchLiveFixturePlan{}, fmt.Errorf(
			"decode %s: scenario_id %q does not match %q",
			manifestPath,
			manifest.ScenarioID,
			syncPartialLocalCatchup100MID,
		)
	}

	scopeRootRelativePath, err := benchScopeRootRelativePath(manifest.RemoteScopePath)
	if err != nil {
		return benchLiveFixturePlan{}, fmt.Errorf("decode %s: %w", manifestPath, err)
	}

	files, directories, err := expandBenchLiveFixtureManifest(&manifest)
	if err != nil {
		return benchLiveFixturePlan{}, fmt.Errorf("expand %s: %w", manifestPath, err)
	}

	mutations, denominators, err := buildBenchLiveMutationPlan(files, &manifest)
	if err != nil {
		return benchLiveFixturePlan{}, fmt.Errorf("expand %s mutations: %w", manifestPath, err)
	}
	denominators.DirectoryCount = len(directories)

	return benchLiveFixturePlan{
		Manifest:              manifest,
		ScopeRootRelativePath: scopeRootRelativePath,
		Files:                 files,
		Directories:           directories,
		Mutations:             mutations,
		Denominators:          denominators,
	}, nil
}

func benchScopeRootRelativePath(remoteScopePath string) (string, error) {
	cleanScope := path.Clean(remoteScopePath)
	if cleanScope == "." || cleanScope == "/" || cleanScope == "" || !strings.HasPrefix(cleanScope, "/") {
		return "", fmt.Errorf("remote scope path %q must be an absolute non-root path", remoteScopePath)
	}

	return strings.TrimPrefix(cleanScope, "/"), nil
}

func expandBenchLiveFixtureManifest(
	manifest *benchLiveFixtureManifest,
) ([]benchLiveFileEntry, []string, error) {
	if manifest == nil {
		return nil, nil, fmt.Errorf("fixture manifest is nil")
	}
	files := make([]benchLiveFileEntry, 0)
	directorySet := map[string]struct{}{}

	for _, group := range manifest.Groups {
		if group.Name == "" {
			return nil, nil, fmt.Errorf("group name is empty")
		}
		if group.FileCount <= 0 {
			return nil, nil, fmt.Errorf("group %q file_count must be > 0", group.Name)
		}
		if group.SizeBytes <= 0 {
			return nil, nil, fmt.Errorf("group %q size_bytes must be > 0", group.Name)
		}
		if group.TopLevelDirs <= 0 || group.LeafDirsPerTop <= 0 {
			return nil, nil, fmt.Errorf("group %q directory fanout must be > 0", group.Name)
		}
		if group.Extension == "" {
			return nil, nil, fmt.Errorf("group %q extension is empty", group.Name)
		}

		leafDirCount := group.TopLevelDirs * group.LeafDirsPerTop
		if group.FileCount%leafDirCount != 0 {
			return nil, nil, fmt.Errorf(
				"group %q file_count %d must divide evenly across %d leaf directories",
				group.Name,
				group.FileCount,
				leafDirCount,
			)
		}

		filesPerLeaf := group.FileCount / leafDirCount
		for i := 0; i < group.FileCount; i++ {
			leafIndex := i / filesPerLeaf
			topIndex := leafIndex / group.LeafDirsPerTop
			leafWithinTop := leafIndex % group.LeafDirsPerTop

			relativePath := filepath.ToSlash(filepath.Join(
				group.Name,
				fmt.Sprintf("tier-%02d", topIndex),
				fmt.Sprintf("set-%02d", leafWithinTop),
				fmt.Sprintf("%s-%05d.%s", group.Name, i, group.Extension),
			))
			files = append(files, benchLiveFileEntry{
				RelativePath: relativePath,
				SizeBytes:    group.SizeBytes,
			})

			parent := path.Dir(relativePath)
			for parent != "." && parent != "/" && parent != "" {
				directorySet[parent] = struct{}{}
				parent = path.Dir(parent)
			}
		}
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].RelativePath < files[j].RelativePath
	})

	directories := make([]string, 0, len(directorySet))
	for dir := range directorySet {
		directories = append(directories, dir)
	}
	sort.Strings(directories)

	return files, directories, nil
}

func buildBenchLiveMutationPlan(
	files []benchLiveFileEntry,
	manifest *benchLiveFixtureManifest,
) (benchLiveMutationPlan, BenchDenominator, error) {
	if manifest == nil {
		return benchLiveMutationPlan{}, BenchDenominator{}, fmt.Errorf("fixture manifest is nil")
	}
	deleteEntries, err := selectBenchLiveMutations(files, manifest.DeleteSelector, nil)
	if err != nil {
		return benchLiveMutationPlan{}, BenchDenominator{}, fmt.Errorf("select deletes: %w", err)
	}

	deletedPaths := make(map[string]struct{}, len(deleteEntries))
	for _, entry := range deleteEntries {
		deletedPaths[entry.File.RelativePath] = struct{}{}
	}

	truncateEntries, err := selectBenchLiveMutations(files, manifest.TruncateSelector, deletedPaths)
	if err != nil {
		return benchLiveMutationPlan{}, BenchDenominator{}, fmt.Errorf("select truncates: %w", err)
	}

	denominators := BenchDenominator{
		FileCount:         len(files),
		ChangedItemCount:  len(deleteEntries) + len(truncateEntries),
		ExpectedTransfers: len(deleteEntries) + len(truncateEntries),
		ExpectedDeletes:   0,
	}
	for _, entry := range deleteEntries {
		denominators.ChangedByteCount += entry.File.SizeBytes
	}
	for _, entry := range truncateEntries {
		denominators.ChangedByteCount += entry.File.SizeBytes
	}

	return benchLiveMutationPlan{
		Deletes:   deleteEntries,
		Truncates: truncateEntries,
	}, denominators, nil
}

func selectBenchLiveMutations(
	files []benchLiveFileEntry,
	selector benchLiveMutationSelector,
	exclude map[string]struct{},
) ([]benchLiveMutationEntry, error) {
	if selector.Modulus <= 0 {
		return nil, fmt.Errorf("selector modulus must be > 0")
	}
	if selector.Remainder < 0 || selector.Remainder >= selector.Modulus {
		return nil, fmt.Errorf("selector remainder %d must be within [0,%d)", selector.Remainder, selector.Modulus)
	}
	if selector.Limit <= 0 {
		return nil, fmt.Errorf("selector limit must be > 0")
	}

	selected := make([]benchLiveMutationEntry, 0, selector.Limit)
	for index, file := range files {
		if exclude != nil {
			if _, blocked := exclude[file.RelativePath]; blocked {
				continue
			}
		}
		if index%selector.Modulus != selector.Remainder {
			continue
		}

		selected = append(selected, benchLiveMutationEntry{
			File:            file,
			TruncateToBytes: selector.TruncateToBytes,
		})
		if len(selected) == selector.Limit {
			break
		}
	}

	if len(selected) < selector.Limit {
		return nil, fmt.Errorf(
			"selector matched %d entries, fewer than required limit %d",
			len(selected),
			selector.Limit,
		)
	}

	return selected, nil
}

func (s *benchLiveScenarioState) runSample(
	ctx context.Context,
	subject preparedBenchSubject,
	phase benchSamplePhase,
	iteration int,
) benchSample {
	sample := benchSample{
		Iteration: iteration,
		Phase:     phase,
		Status:    BenchSampleSuccess,
	}

	if err := s.ensurePrepared(ctx, subject); err != nil {
		return benchFixtureFailureSample(sample, err)
	}

	runtime, runtimeErr := s.newSampleRuntime()
	if runtimeErr != nil {
		return benchFixtureFailureSample(sample, runtimeErr)
	}

	scopeRoot := filepath.Join(runtime.syncDir, filepath.FromSlash(s.fixture.ScopeRootRelativePath))
	preparedSample, preparedOK := s.prepareMeasuredCatchup(ctx, subject, runtime, scopeRoot, sample)
	if !preparedOK {
		return finalizeBenchLiveSampleRuntime(runtime.rootDir, preparedSample)
	}

	measuredSample := s.measureCatchupSample(ctx, subject, runtime, scopeRoot, preparedSample)

	return finalizeBenchLiveSampleRuntime(runtime.rootDir, measuredSample)
}

func finalizeBenchLiveSampleRuntime(runtimeRoot string, sample benchSample) benchSample {
	if cleanupErr := localpath.RemoveAll(runtimeRoot); cleanupErr != nil && sample.Status == BenchSampleSuccess {
		return benchFixtureFailureSample(sample, fmt.Errorf("cleanup sample runtime: %w", cleanupErr))
	}

	return sample
}

func (s *benchLiveScenarioState) prepareMeasuredCatchup(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime benchLiveCommandRuntime,
	scopeRoot string,
	sample benchSample,
) (benchSample, bool) {
	baseline, baselineErr := subject.measure(ctx, runtime.commandSpec("sync", "--download-only"))
	if baselineErr != nil {
		return classifyBenchProcessFailure(ctx, sample, baseline, baselineErr), false
	}

	logResetErr := resetBenchLogFile(runtime.logPath)
	if logResetErr != nil {
		return benchFixtureFailureSample(sample, logResetErr), false
	}

	perturbErr := perturbBenchLiveFixture(scopeRoot, s.fixture.Mutations)
	if perturbErr != nil {
		return benchFixtureFailureSample(sample, perturbErr), false
	}

	return sample, true
}

func (s *benchLiveScenarioState) measureCatchupSample(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime benchLiveCommandRuntime,
	scopeRoot string,
	sample benchSample,
) benchSample {
	process, measureErr := subject.measure(ctx, runtime.commandSpec("sync", "--download-only"))
	measuredSample := sampleWithMeasuredProcess(sample, process)
	if measureErr != nil {
		return classifyBenchProcessFailure(ctx, measuredSample, process, measureErr)
	}

	perfSummary, perfErr := readPerformanceSummary(runtime.logPath, process.Stderr)
	if perfErr != nil {
		return benchInvalidSample(measuredSample, perfErr)
	}
	measuredSample.PerfSummary = perfSummary

	verifyErr := verifyBenchLiveFixture(scopeRoot, &s.fixture)
	if verifyErr != nil {
		return benchInvalidSample(measuredSample, verifyErr)
	}

	return measuredSample
}

func classifyBenchProcessFailure(
	ctx context.Context,
	sample benchSample,
	process benchMeasuredProcess,
	runErr error,
) benchSample {
	measuredSample := sampleWithMeasuredProcess(sample, process)
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || ctx.Err() != nil {
		return benchAbortedSample(measuredSample, runErr)
	}

	return benchSubjectFailureSample(measuredSample, runErr, process.Stdout, process.Stderr)
}

func (s *benchLiveScenarioState) ensurePrepared(ctx context.Context, subject preparedBenchSubject) error {
	s.setupOnce.Do(func() {
		s.setupErr = s.prepareRemoteFixture(ctx, subject)
	})

	return s.setupErr
}

func (s *benchLiveScenarioState) prepareRemoteFixture(ctx context.Context, subject preparedBenchSubject) error {
	liveConfig, err := testutil.LoadLiveTestConfig(s.repoRoot)
	if err != nil {
		return fmt.Errorf("load live benchmark config: %w", err)
	}
	allowlistErr := validateBenchAllowlist(liveConfig.PrimaryDrive)
	if allowlistErr != nil {
		return allowlistErr
	}

	credentialDir, err := locateBenchCredentialDir(s.repoRoot)
	if err != nil {
		return err
	}

	seedSyncDir := filepath.Join(s.workRoot, "seed-sync-root")
	mkdirErr := localpath.MkdirAll(seedSyncDir, benchLiveFixtureDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create seed sync root: %w", mkdirErr)
	}

	scopeRoot := filepath.Join(seedSyncDir, filepath.FromSlash(s.fixture.ScopeRootRelativePath))
	materializeErr := materializeBenchLiveFixture(scopeRoot, s.fixture.Files)
	if materializeErr != nil {
		return fmt.Errorf("materialize seed fixture: %w", materializeErr)
	}

	runtime, err := createBenchLiveRuntime(
		s.workRoot,
		credentialDir,
		liveConfig.PrimaryDrive,
		seedSyncDir,
		s.fixture.Manifest.RemoteScopePath,
	)
	if err != nil {
		return err
	}

	resetErr := resetBenchRemoteScope(ctx, subject, runtime, s.fixture.Manifest.RemoteScopePath)
	if resetErr != nil {
		return resetErr
	}

	process, err := subject.measure(ctx, runtime.commandSpec("sync", "--upload-only"))
	if err != nil {
		return fmt.Errorf(
			"seed remote benchmark fixture: %w: %s",
			err,
			failureExcerpt(err, process.Stdout, process.Stderr),
		)
	}

	waitErr := waitForBenchRemoteScopeVisible(ctx, subject, runtime, s.fixture.Manifest.RemoteScopePath)
	if waitErr != nil {
		return waitErr
	}

	return nil
}

func (s *benchLiveScenarioState) newSampleRuntime() (benchLiveCommandRuntime, error) {
	liveConfig, err := testutil.LoadLiveTestConfig(s.repoRoot)
	if err != nil {
		return benchLiveCommandRuntime{}, fmt.Errorf("load live benchmark config: %w", err)
	}

	credentialDir, err := locateBenchCredentialDir(s.repoRoot)
	if err != nil {
		return benchLiveCommandRuntime{}, err
	}

	return createBenchLiveRuntime(
		s.workRoot,
		credentialDir,
		liveConfig.PrimaryDrive,
		"",
		s.fixture.Manifest.RemoteScopePath,
	)
}

func validateBenchAllowlist(driveID string) error {
	allowlist := strings.TrimSpace(os.Getenv("ONEDRIVE_ALLOWED_TEST_ACCOUNTS"))
	if allowlist == "" {
		return fmt.Errorf("live benchmark requires ONEDRIVE_ALLOWED_TEST_ACCOUNTS to include %q", driveID)
	}

	for _, entry := range strings.Split(allowlist, ",") {
		if strings.TrimSpace(entry) == driveID {
			return nil
		}
	}

	return fmt.Errorf(
		"live benchmark drive %q is not in ONEDRIVE_ALLOWED_TEST_ACCOUNTS=%q",
		driveID,
		allowlist,
	)
}

func locateBenchCredentialDir(repoRoot string) (string, error) {
	dir := filepath.Join(repoRoot, ".testdata")
	if _, err := localpath.Stat(dir); err != nil {
		return "", fmt.Errorf("live benchmark requires %s (run scripts/bootstrap-test-credentials.sh first)", dir)
	}

	return dir, nil
}

func createBenchLiveRuntime(
	workRoot string,
	credentialDir string,
	driveID string,
	syncDir string,
	remoteScopePath string,
) (benchLiveCommandRuntime, error) {
	runtimeRoot, err := localpath.MkdirTemp(workRoot, "runtime-*")
	if err != nil {
		return benchLiveCommandRuntime{}, fmt.Errorf("create benchmark runtime root: %w", err)
	}
	if syncDir == "" {
		syncDir = filepath.Join(runtimeRoot, "sync-root")
	}

	homeDir := filepath.Join(runtimeRoot, "home")
	configHome := filepath.Join(runtimeRoot, "config")
	dataHome := filepath.Join(runtimeRoot, "data")
	cacheHome := filepath.Join(runtimeRoot, "cache")
	for _, dir := range []string{homeDir, configHome, dataHome, cacheHome, syncDir} {
		if err := localpath.MkdirAll(dir, benchLiveFixtureDirPerm); err != nil {
			return benchLiveCommandRuntime{}, fmt.Errorf("create benchmark runtime path %s: %w", dir, err)
		}
	}

	dataDir := filepath.Join(dataHome, "onedrive-go")
	if err := localpath.MkdirAll(dataDir, benchLiveFixtureDirPerm); err != nil {
		return benchLiveCommandRuntime{}, fmt.Errorf("create benchmark data dir: %w", err)
	}
	if err := copyBenchCredentials(credentialDir, dataDir, driveID); err != nil {
		return benchLiveCommandRuntime{}, err
	}

	logPath := filepath.Join(runtimeRoot, "bench.log")
	configPath := filepath.Join(runtimeRoot, "config.toml")
	configBody := fmt.Sprintf(
		"log_level = %q\nlog_file = %q\nsync_paths = [%q]\n\n[%q]\nsync_dir = %q\n",
		"info",
		logPath,
		remoteScopePath,
		driveID,
		syncDir,
	)
	if err := localpath.WriteFile(configPath, []byte(configBody), benchResultFilePerm); err != nil {
		return benchLiveCommandRuntime{}, fmt.Errorf("write benchmark config: %w", err)
	}

	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+configHome,
		"XDG_DATA_HOME="+dataHome,
		"XDG_CACHE_HOME="+cacheHome,
	)

	return benchLiveCommandRuntime{
		rootDir:    runtimeRoot,
		syncDir:    syncDir,
		configPath: configPath,
		logPath:    logPath,
		driveID:    driveID,
		env:        env,
	}, nil
}

func (r benchLiveCommandRuntime) commandSpec(args ...string) benchCommandSpec {
	fullArgs := []string{"--config", r.configPath, "--drive", r.driveID}
	fullArgs = append(fullArgs, args...)

	return benchCommandSpec{
		CWD: r.rootDir,
		Env: r.env,
		Arg: fullArgs,
	}
}

func copyBenchCredentials(srcDir string, dstDir string, driveID string) error {
	tokenFileName, err := benchTokenFileName(driveID)
	if err != nil {
		return err
	}
	if err := copyBenchFile(filepath.Join(srcDir, tokenFileName), filepath.Join(dstDir, tokenFileName)); err != nil {
		return fmt.Errorf("copy token file: %w", err)
	}

	for _, prefix := range []string{"account_", "drive_"} {
		matches, err := filepath.Glob(filepath.Join(srcDir, prefix+"*.json"))
		if err != nil {
			return fmt.Errorf("glob %s metadata: %w", prefix, err)
		}

		for _, match := range matches {
			if err := copyBenchFile(match, filepath.Join(dstDir, filepath.Base(match))); err != nil {
				return fmt.Errorf("copy metadata %s: %w", filepath.Base(match), err)
			}
		}
	}

	return nil
}

func benchTokenFileName(driveID string) (string, error) {
	parts := strings.SplitN(driveID, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", fmt.Errorf("parse drive %q for token filename", driveID)
	}

	return "token_" + parts[0] + "_" + parts[1] + ".json", nil
}

func copyBenchFile(src string, dst string) error {
	data, err := localpath.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := localpath.WriteFile(dst, data, benchLiveFilePerm); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	return nil
}

func materializeBenchLiveFixture(scopeRoot string, files []benchLiveFileEntry) error {
	if err := localpath.MkdirAll(scopeRoot, benchLiveFixtureDirPerm); err != nil {
		return fmt.Errorf("create fixture scope root: %w", err)
	}

	for _, entry := range files {
		targetPath := filepath.Join(scopeRoot, filepath.FromSlash(entry.RelativePath))
		if err := localpath.MkdirAll(filepath.Dir(targetPath), benchLiveFixtureDirPerm); err != nil {
			return fmt.Errorf("create fixture parent for %s: %w", entry.RelativePath, err)
		}
		if err := writeBenchLiveFile(targetPath, entry); err != nil {
			return err
		}
	}

	return nil
}

func writeBenchLiveFile(targetPath string, entry benchLiveFileEntry) (err error) {
	file, err := localpath.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, benchLiveFilePerm)
	if err != nil {
		return fmt.Errorf("open fixture file %s: %w", entry.RelativePath, err)
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	pattern := benchLivePattern(entry.RelativePath)
	remaining := entry.SizeBytes
	for remaining > 0 {
		chunk := pattern
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}

		if _, err := file.Write(chunk); err != nil {
			return fmt.Errorf("write fixture file %s: %w", entry.RelativePath, err)
		}
		remaining -= int64(len(chunk))
	}

	return nil
}

func benchLivePattern(relativePath string) []byte {
	pattern := make([]byte, 0, benchLivePatternBytes)
	for block := 0; len(pattern) < benchLivePatternBytes; block++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", relativePath, block)))
		remaining := benchLivePatternBytes - len(pattern)
		if remaining >= len(sum) {
			pattern = append(pattern, sum[:]...)
			continue
		}

		pattern = append(pattern, sum[:remaining]...)
	}

	return pattern
}

func resetBenchRemoteScope(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime benchLiveCommandRuntime,
	remoteScopePath string,
) error {
	for attempt := 0; attempt < benchLiveDeleteRetryAttempts; attempt++ {
		process, err := subject.measure(ctx, runtime.commandSpec("rm", "-r", remoteScopePath))
		if err == nil || isRemoteNotFoundCleanup(string(process.Stderr)) {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("reset remote scope %s: %w", remoteScopePath, ctx.Err())
		}
		if attempt == benchLiveDeleteRetryAttempts-1 {
			return fmt.Errorf(
				"reset remote scope %s: %w: %s",
				remoteScopePath,
				err,
				failureExcerpt(err, process.Stdout, process.Stderr),
			)
		}

		time.Sleep(benchPollBackoff(attempt))
	}

	return nil
}

func waitForBenchRemoteScopeVisible(
	ctx context.Context,
	subject preparedBenchSubject,
	runtime benchLiveCommandRuntime,
	remoteScopePath string,
) error {
	cleanScope := path.Clean(remoteScopePath)
	scopeBase := path.Base(cleanScope)
	parentPath := path.Dir(cleanScope)
	if parentPath == "." || parentPath == "" {
		parentPath = "/"
	}

	deadline := time.Now().Add(benchLivePollTimeout)
	var lastProcess benchMeasuredProcess
	var lastErr error
	for attempt := 0; ; attempt++ {
		statProcess, statErr := subject.measure(ctx, runtime.commandSpec("stat", "--json", cleanScope))
		if statErr == nil {
			return nil
		}

		listProcess, listErr := subject.measure(ctx, runtime.commandSpec("ls", parentPath))
		if listErr == nil && benchRemoteScopeVisibleInListing(listProcess.Stdout, scopeBase) {
			return nil
		}

		lastProcess = listProcess
		lastErr = listErr
		if classifyBenchVisibilityError(statProcess.Stderr, statErr) == nil &&
			classifyBenchVisibilityError(listProcess.Stderr, listErr) == nil {
			if ctx.Err() != nil {
				return fmt.Errorf("wait for remote scope visibility %s: %w", remoteScopePath, ctx.Err())
			}
			if time.Now().After(deadline) {
				if lastErr == nil {
					lastErr = fmt.Errorf("scope %q not visible yet", remoteScopePath)
				}
				return fmt.Errorf(
					"wait for remote scope visibility %s: timed out after %s: %s",
					remoteScopePath,
					benchLivePollTimeout,
					failureExcerpt(lastErr, lastProcess.Stdout, lastProcess.Stderr),
				)
			}

			time.Sleep(benchPollBackoff(attempt))
			continue
		}

		if fatalErr := classifyBenchVisibilityError(listProcess.Stderr, listErr); fatalErr != nil {
			return fmt.Errorf(
				"wait for remote scope visibility %s via parent listing %s: %w: %s",
				remoteScopePath,
				parentPath,
				fatalErr,
				failureExcerpt(fatalErr, listProcess.Stdout, listProcess.Stderr),
			)
		}

		if fatalErr := classifyBenchVisibilityError(statProcess.Stderr, statErr); fatalErr != nil {
			return fmt.Errorf(
				"wait for remote scope visibility %s via exact path: %w: %s",
				remoteScopePath,
				fatalErr,
				failureExcerpt(fatalErr, statProcess.Stdout, statProcess.Stderr),
			)
		}
	}
}

func benchRemoteScopeVisibleInListing(stdout []byte, scopeBase string) bool {
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.TrimSpace(line) == scopeBase {
			return true
		}
	}

	return false
}

func classifyBenchVisibilityError(stderr []byte, err error) error {
	if err == nil {
		return nil
	}

	lowerStderr := strings.ToLower(string(stderr))
	if isRemoteNotFoundCleanup(lowerStderr) || isRetryableBenchGraphReadFailure(string(stderr)) {
		return nil
	}

	return err
}

func isRemoteNotFoundCleanup(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "not found") || strings.Contains(lower, "could not be found")
}

func isRetryableBenchGraphReadFailure(stderr string) bool {
	return strings.Contains(stderr, "graph: HTTP 502") ||
		strings.Contains(stderr, "graph: HTTP 503") ||
		strings.Contains(stderr, "graph: HTTP 504")
}

func benchPollBackoff(attempt int) time.Duration {
	delay := 500 * time.Millisecond << uint(attempt)
	if delay > benchLivePollBackoffCap {
		return benchLivePollBackoffCap
	}

	return delay
}

func resetBenchLogFile(logPath string) error {
	if err := localpath.WriteFile(logPath, []byte{}, benchResultFilePerm); err != nil {
		return fmt.Errorf("reset benchmark log file: %w", err)
	}

	return nil
}

func perturbBenchLiveFixture(scopeRoot string, mutations benchLiveMutationPlan) error {
	for _, entry := range mutations.Deletes {
		targetPath := filepath.Join(scopeRoot, filepath.FromSlash(entry.File.RelativePath))
		if err := localpath.Remove(targetPath); err != nil {
			return fmt.Errorf("delete %s: %w", entry.File.RelativePath, err)
		}
	}

	for _, entry := range mutations.Truncates {
		targetPath := filepath.Join(scopeRoot, filepath.FromSlash(entry.File.RelativePath))
		if err := truncateBenchLiveFile(targetPath, entry.TruncateToBytes); err != nil {
			return fmt.Errorf("truncate %s: %w", entry.File.RelativePath, err)
		}
	}

	return nil
}

func truncateBenchLiveFile(path string, size int64) (err error) {
	file, err := localpath.OpenFile(path, os.O_WRONLY, benchLiveFilePerm)
	if err != nil {
		return fmt.Errorf("open %s for truncate: %w", path, err)
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("truncate %s: %w", path, err)
	}

	return nil
}

func verifyBenchLiveFixture(scopeRoot string, fixture *benchLiveFixturePlan) error {
	if fixture == nil {
		return fmt.Errorf("fixture plan is nil")
	}
	expectedFiles := make(map[string]benchLiveFileEntry, len(fixture.Files))
	for _, entry := range fixture.Files {
		expectedFiles[entry.RelativePath] = entry
	}
	expectedDirs := make(map[string]struct{}, len(fixture.Directories))
	for _, dir := range fixture.Directories {
		expectedDirs[dir] = struct{}{}
	}

	seenFiles := map[string]struct{}{}
	seenDirs := map[string]struct{}{}

	if err := walkBenchLiveFixtureTree(scopeRoot, expectedFiles, expectedDirs, seenFiles, seenDirs); err != nil {
		return fmt.Errorf("walk %s: %w", scopeRoot, err)
	}

	return assertBenchLiveFixtureSeen(fixture, seenFiles, seenDirs)
}

func walkBenchLiveFixtureTree(
	scopeRoot string,
	expectedFiles map[string]benchLiveFileEntry,
	expectedDirs map[string]struct{},
	seenFiles map[string]struct{},
	seenDirs map[string]struct{},
) error {
	walkErr := filepath.WalkDir(scopeRoot, func(currentPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk fixture tree: %w", walkErr)
		}

		relativePath, err := filepath.Rel(scopeRoot, currentPath)
		if err != nil {
			return fmt.Errorf("rel path %s: %w", currentPath, err)
		}
		relativePath = filepath.ToSlash(relativePath)
		if relativePath == "." {
			return nil
		}

		if entry.IsDir() {
			return recordBenchLiveDirectory(relativePath, expectedDirs, seenDirs)
		}

		return verifyAndRecordBenchLiveFile(currentPath, relativePath, expectedFiles, seenFiles)
	})
	if walkErr != nil {
		return fmt.Errorf("walk fixture tree %s: %w", scopeRoot, walkErr)
	}

	return nil
}

func recordBenchLiveDirectory(
	relativePath string,
	expectedDirs map[string]struct{},
	seenDirs map[string]struct{},
) error {
	if _, ok := expectedDirs[relativePath]; !ok {
		return fmt.Errorf("unexpected directory %s", relativePath)
	}
	seenDirs[relativePath] = struct{}{}

	return nil
}

func verifyAndRecordBenchLiveFile(
	currentPath string,
	relativePath string,
	expectedFiles map[string]benchLiveFileEntry,
	seenFiles map[string]struct{},
) error {
	expected, ok := expectedFiles[relativePath]
	if !ok {
		return fmt.Errorf("unexpected file %s", relativePath)
	}
	if err := verifyBenchLiveFile(currentPath, expected); err != nil {
		return err
	}
	seenFiles[relativePath] = struct{}{}

	return nil
}

func assertBenchLiveFixtureSeen(
	fixture *benchLiveFixturePlan,
	seenFiles map[string]struct{},
	seenDirs map[string]struct{},
) error {
	for _, dir := range fixture.Directories {
		if _, ok := seenDirs[dir]; !ok {
			return fmt.Errorf("missing directory %s", dir)
		}
	}
	for _, entry := range fixture.Files {
		if _, ok := seenFiles[entry.RelativePath]; !ok {
			return fmt.Errorf("missing file %s", entry.RelativePath)
		}
	}

	return nil
}

func verifyBenchLiveFile(path string, entry benchLiveFileEntry) (err error) {
	file, err := localpath.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", entry.RelativePath, err)
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", entry.RelativePath, err)
	}
	if info.Size() != entry.SizeBytes {
		return fmt.Errorf("file %s size mismatch: got %d want %d", entry.RelativePath, info.Size(), entry.SizeBytes)
	}

	pattern := benchLivePattern(entry.RelativePath)
	buffer := make([]byte, benchLiveVerifyBufferBytes)
	var offset int64
	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			if err := compareBenchLiveChunk(buffer[:n], pattern, offset, entry.RelativePath); err != nil {
				return err
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read %s: %w", entry.RelativePath, readErr)
		}
	}
}

func compareBenchLiveChunk(chunk []byte, pattern []byte, offset int64, relativePath string) error {
	for index, value := range chunk {
		expected := pattern[int((offset+int64(index))%int64(len(pattern)))]
		if value != expected {
			return fmt.Errorf("file %s content mismatch at byte %d", relativePath, offset+int64(index))
		}
	}

	return nil
}
