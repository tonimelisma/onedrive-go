package devtool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBenchSubjectID = "onedrive-go"

	benchSchemaVersion = 1

	benchResultFilePerm = 0o600
	benchResultDirPerm  = 0o700

	benchFailureExcerptLimit = 512
	linuxMaxRSSUnitBytes     = 1024

	startupEmptyConfigScenarioID     = "startup-empty-config"
	startupEmptyConfigDefaultRuns    = 15
	startupEmptyConfigDefaultWarmups = 3
	syncPartialLocalCatchup100MID    = "sync-partial-local-catchup-100m"
	syncPartialLocalCatchup100MRuns  = 3
	syncPartialLocalCatchup100MWarm  = 0

	benchSubjectKindRepoCLI = "repo_cli_binary"
	benchBuildModeRepoHead  = "repo_head"
)

type BenchOptions struct {
	RepoRoot       string
	Scenario       string
	Subject        string
	Runs           int
	Warmup         int
	JSON           bool
	ResultJSONPath string
	Stdout         io.Writer
	Stderr         io.Writer
}

type BenchSubjectSpec struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

type BenchScenarioSpec struct {
	ID            string           `json:"id"`
	Class         string           `json:"class"`
	ConfigProfile string           `json:"config_profile"`
	DefaultRuns   int              `json:"default_runs"`
	DefaultWarmup int              `json:"default_warmup"`
	Denominators  BenchDenominator `json:"denominators"`
}

type BenchDenominator struct {
	FileCount         int   `json:"file_count"`
	DirectoryCount    int   `json:"directory_count"`
	ChangedItemCount  int   `json:"changed_item_count"`
	ChangedByteCount  int64 `json:"changed_byte_count"`
	ExpectedTransfers int   `json:"expected_transfers"`
	ExpectedDeletes   int   `json:"expected_deletes"`
}

type BenchSampleStatus string

const (
	BenchSampleSuccess       BenchSampleStatus = "success"
	BenchSampleSubjectFailed BenchSampleStatus = "subject_failed"
	BenchSampleFixtureFailed BenchSampleStatus = "fixture_failed"
	BenchSampleInvalid       BenchSampleStatus = "invalid"
	BenchSampleAborted       BenchSampleStatus = "aborted"
)

type benchSamplePhase string

const (
	benchSamplePhaseWarmup   benchSamplePhase = "warmup"
	benchSamplePhaseMeasured benchSamplePhase = "measured"
)

type benchResult struct {
	ResultVersion int                `json:"result_version"`
	Subject       benchResultSubject `json:"subject"`
	Scenario      BenchScenarioSpec  `json:"scenario"`
	Environment   benchEnvironment   `json:"environment"`
	Run           benchRun           `json:"run"`
	Samples       []benchSample      `json:"samples"`
	Summary       benchSummary       `json:"summary"`
}

type benchResultSubject struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	GitCommit      string `json:"git_commit"`
	ExecutablePath string `json:"executable_path"`
	BuildMode      string `json:"build_mode"`
}

type benchEnvironment struct {
	Hostname  string `json:"hostname,omitempty"`
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	NumCPU    int    `json:"num_cpu"`
}

type benchRun struct {
	StartedAt       time.Time         `json:"started_at"`
	CompletedAt     time.Time         `json:"completed_at"`
	RequestedRuns   int               `json:"requested_runs"`
	RequestedWarmup int               `json:"requested_warmup"`
	EffectiveRuns   int               `json:"effective_runs"`
	EffectiveWarmup int               `json:"effective_warmup"`
	Status          BenchSampleStatus `json:"status"`
}

type benchSample struct {
	Iteration       int               `json:"iteration"`
	Phase           benchSamplePhase  `json:"phase"`
	Status          BenchSampleStatus `json:"status"`
	ElapsedMicros   int64             `json:"elapsed_micros"`
	ExitCode        int               `json:"exit_code"`
	UserCPUMicros   int64             `json:"user_cpu_micros"`
	SystemCPUMicros int64             `json:"system_cpu_micros"`
	PeakRSSBytes    int64             `json:"peak_rss_bytes"`
	StdoutBytes     int64             `json:"stdout_bytes"`
	StderrBytes     int64             `json:"stderr_bytes"`
	FailureExcerpt  string            `json:"failure_excerpt,omitempty"`
	PerfSummary     *benchPerfSummary `json:"perf_summary,omitempty"`
}

type benchSummary struct {
	Status                BenchSampleStatus `json:"status"`
	SampleCount           int               `json:"sample_count"`
	SuccessfulSampleCount int               `json:"successful_sample_count"`
	WarmupCount           int               `json:"warmup_count"`
	ElapsedMicros         benchMetricStats  `json:"elapsed_micros"`
	PeakRSSBytes          benchMetricStats  `json:"peak_rss_bytes"`
	UserCPUMicros         benchMetricStats  `json:"user_cpu_micros"`
	SystemCPUMicros       benchMetricStats  `json:"system_cpu_micros"`
}

type benchMetricStats struct {
	Mean   int64 `json:"mean"`
	Median int64 `json:"median"`
	Min    int64 `json:"min"`
	Max    int64 `json:"max"`
}

type benchSampleRunner func(context.Context, preparedBenchSubject, benchSamplePhase, int) benchSample

type benchScenarioDefinition struct {
	Spec    BenchScenarioSpec
	Run     benchSampleRunner
	Prepare func(context.Context, string, preparedBenchSubject) (preparedBenchScenario, error)
}

type benchSubjectDefinition struct {
	Spec    BenchSubjectSpec
	Prepare func(context.Context, commandRunner, string) (preparedBenchSubject, error)
}

type benchInvocation struct {
	subjectID    string
	subjectDef   benchSubjectDefinition
	scenarioDef  benchScenarioDefinition
	measuredRuns int
	warmupRuns   int
}

type preparedBenchSubject struct {
	metadata benchResultSubject
	measure  func(context.Context, benchCommandSpec) (benchMeasuredProcess, error)
	cleanup  func() error
}

type preparedBenchScenario struct {
	run     benchSampleRunner
	cleanup func() error
}

type benchCommandSpec struct {
	CWD string
	Env []string
	Arg []string
}

type benchMeasuredProcess struct {
	ExitCode        int
	ElapsedMicros   int64
	UserCPUMicros   int64
	SystemCPUMicros int64
	PeakRSSBytes    int64
	Stdout          []byte
	Stderr          []byte
}

type benchPerfLogLine struct {
	Msg string `json:"msg"`
	benchPerfSummary
}

type benchPerfSummary struct {
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	DurationMS int64  `json:"duration_ms"`
	Result     string `json:"result,omitempty"`

	CommandItems int   `json:"command_items,omitempty"`
	CommandBytes int64 `json:"command_bytes,omitempty"`

	HTTPRequestCount      int   `json:"http_request_count,omitempty"`
	HTTPSuccessCount      int   `json:"http_success_count,omitempty"`
	HTTPClientErrorCount  int   `json:"http_client_error_count,omitempty"`
	HTTPServerErrorCount  int   `json:"http_server_error_count,omitempty"`
	HTTPTransportErrors   int   `json:"http_transport_errors,omitempty"`
	HTTPRetryCount        int   `json:"http_retry_count,omitempty"`
	HTTPRequestTimeMS     int64 `json:"http_request_time_ms,omitempty"`
	HTTPRetryBackoffMS    int64 `json:"http_retry_backoff_ms,omitempty"`
	DBTransactionCount    int   `json:"db_transaction_count,omitempty"`
	DBTransactionTimeMS   int64 `json:"db_transaction_time_ms,omitempty"`
	DownloadCount         int   `json:"download_count,omitempty"`
	DownloadBytes         int64 `json:"download_bytes,omitempty"`
	UploadCount           int   `json:"upload_count,omitempty"`
	UploadBytes           int64 `json:"upload_bytes,omitempty"`
	TransferTimeMS        int64 `json:"transfer_time_ms,omitempty"`
	ObserveRunCount       int   `json:"observe_run_count,omitempty"`
	ObservedPathCount     int   `json:"observed_path_count,omitempty"`
	ObserveTimeMS         int64 `json:"observe_time_ms,omitempty"`
	PlanRunCount          int   `json:"plan_run_count,omitempty"`
	ActionableActionCount int   `json:"actionable_action_count,omitempty"`
	PlanTimeMS            int64 `json:"plan_time_ms,omitempty"`
	ExecuteRunCount       int   `json:"execute_run_count,omitempty"`
	ExecuteActionCount    int   `json:"execute_action_count,omitempty"`
	ExecuteSucceededCount int   `json:"execute_succeeded_count,omitempty"`
	ExecuteFailedCount    int   `json:"execute_failed_count,omitempty"`
	ExecuteTimeMS         int64 `json:"execute_time_ms,omitempty"`
	RefreshRunCount       int   `json:"refresh_run_count,omitempty"`
	RefreshEventCount     int   `json:"refresh_event_count,omitempty"`
	RefreshTimeMS         int64 `json:"refresh_time_ms,omitempty"`
	WatchBatchCount       int   `json:"watch_batch_count,omitempty"`
	WatchPathCount        int   `json:"watch_path_count,omitempty"`
}

type startupDriveListJSONOutput struct {
	Configured            []json.RawMessage `json:"configured"`
	Available             []json.RawMessage `json:"available"`
	AccountsRequiringAuth []json.RawMessage `json:"accounts_requiring_auth,omitempty"`
	AccountsDegraded      []json.RawMessage `json:"accounts_degraded,omitempty"`
}

func RunBench(ctx context.Context, opts BenchOptions) error {
	return runBench(ctx, ExecRunner{}, opts)
}

func BenchSubjectIDs() []string {
	return []string{DefaultBenchSubjectID}
}

func LookupBenchSubject(id string) (BenchSubjectSpec, error) {
	def, err := lookupBenchSubjectDefinition(id)
	if err != nil {
		return BenchSubjectSpec{}, err
	}

	return def.Spec, nil
}

func BenchScenarioIDs() []string {
	return []string{
		startupEmptyConfigScenarioID,
		syncPartialLocalCatchup100MID,
	}
}

func LookupBenchScenario(id string) (BenchScenarioSpec, error) {
	def, err := lookupBenchScenarioDefinition(id)
	if err != nil {
		return BenchScenarioSpec{}, err
	}

	return def.Spec, nil
}

func runBench(ctx context.Context, runner commandRunner, opts BenchOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	invocation, err := resolveBenchInvocation(opts)
	if err != nil {
		return err
	}

	preparedSubject, err := invocation.subjectDef.Prepare(ctx, runner, opts.RepoRoot)
	if err != nil {
		return fmt.Errorf("bench: prepare subject %s: %w", invocation.subjectID, err)
	}
	defer warnBenchCleanup(stderr, "subject", preparedSubject.cleanup)

	preparedScenario, err := prepareBenchScenario(ctx, &invocation.scenarioDef, opts.RepoRoot, preparedSubject)
	if err != nil {
		return fmt.Errorf("bench: prepare scenario %s: %w", invocation.scenarioDef.Spec.ID, err)
	}
	defer warnBenchCleanup(stderr, "scenario", preparedScenario.cleanup)

	startedAt := time.Now()
	result := benchResult{
		ResultVersion: benchSchemaVersion,
		Subject:       preparedSubject.metadata,
		Scenario:      invocation.scenarioDef.Spec,
		Environment:   collectBenchEnvironment(),
		Run: benchRun{
			StartedAt:       startedAt,
			RequestedRuns:   opts.Runs,
			RequestedWarmup: opts.Warmup,
			EffectiveRuns:   invocation.measuredRuns,
			EffectiveWarmup: invocation.warmupRuns,
			Status:          BenchSampleSuccess,
		},
		Samples: make([]benchSample, 0, invocation.measuredRuns+invocation.warmupRuns),
	}

	result.Samples = collectBenchSamples(
		ctx,
		preparedScenario.run,
		preparedSubject,
		invocation.warmupRuns,
		invocation.measuredRuns,
		result.Samples,
	)

	result.Run.CompletedAt = time.Now()
	result.Summary = summarizeBenchSamples(result.Samples)
	result.Run.Status = result.Summary.Status

	return emitBenchResult(stdout, opts, &result)
}

func resolveBenchInvocation(opts BenchOptions) (benchInvocation, error) {
	if err := validateBenchOptions(opts); err != nil {
		return benchInvocation{}, err
	}

	subjectID := strings.TrimSpace(opts.Subject)
	if subjectID == "" {
		subjectID = DefaultBenchSubjectID
	}

	subjectDef, err := lookupBenchSubjectDefinition(subjectID)
	if err != nil {
		return benchInvocation{}, fmt.Errorf("bench: %w", err)
	}
	scenarioDef, err := lookupBenchScenarioDefinition(opts.Scenario)
	if err != nil {
		return benchInvocation{}, fmt.Errorf("bench: %w", err)
	}

	measuredRuns := scenarioDef.Spec.DefaultRuns
	if opts.Runs >= 0 {
		measuredRuns = opts.Runs
	}
	warmupRuns := scenarioDef.Spec.DefaultWarmup
	if opts.Warmup >= 0 {
		warmupRuns = opts.Warmup
	}

	return benchInvocation{
		subjectID:    subjectID,
		subjectDef:   subjectDef,
		scenarioDef:  scenarioDef,
		measuredRuns: measuredRuns,
		warmupRuns:   warmupRuns,
	}, nil
}

func validateBenchOptions(opts BenchOptions) error {
	if opts.RepoRoot == "" {
		return fmt.Errorf("bench: missing repo root")
	}
	if opts.Scenario == "" {
		return fmt.Errorf("bench: missing scenario")
	}
	if opts.Runs < -1 {
		return fmt.Errorf("bench: runs must be >= -1")
	}
	if opts.Warmup < -1 {
		return fmt.Errorf("bench: warmup must be >= -1")
	}

	return nil
}

func prepareBenchScenario(
	ctx context.Context,
	def *benchScenarioDefinition,
	repoRoot string,
	subject preparedBenchSubject,
) (preparedBenchScenario, error) {
	if def == nil {
		return preparedBenchScenario{}, fmt.Errorf("scenario definition is nil")
	}
	if def.Prepare == nil {
		return preparedBenchScenario{
			run: def.Run,
		}, nil
	}

	prepared, err := def.Prepare(ctx, repoRoot, subject)
	if err != nil {
		return preparedBenchScenario{}, err
	}
	if prepared.run == nil {
		prepared.run = def.Run
	}
	if prepared.run == nil {
		return preparedBenchScenario{}, fmt.Errorf("scenario %s missing sample runner", def.Spec.ID)
	}

	return prepared, nil
}

func warnBenchCleanup(w io.Writer, kind string, cleanup func() error) {
	if cleanup == nil {
		return
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		_, writeErr := fmt.Fprintf(w, "warning: benchmark %s cleanup: %v\n", kind, cleanupErr)
		if writeErr != nil {
			return
		}
	}
}

func collectBenchSamples(
	ctx context.Context,
	runSample benchSampleRunner,
	preparedSubject preparedBenchSubject,
	warmupRuns int,
	measuredRuns int,
	samples []benchSample,
) []benchSample {
	stop := false
	for i := 0; i < warmupRuns && !stop; i++ {
		sample := runSample(ctx, preparedSubject, benchSamplePhaseWarmup, i+1)
		samples = append(samples, sample)
		if sample.Status != BenchSampleSuccess {
			stop = true
		}
	}
	for i := 0; i < measuredRuns && !stop; i++ {
		sample := runSample(ctx, preparedSubject, benchSamplePhaseMeasured, i+1)
		samples = append(samples, sample)
		if sample.Status != BenchSampleSuccess {
			stop = true
		}
	}

	return samples
}

func emitBenchResult(stdout io.Writer, opts BenchOptions, result *benchResult) error {
	resultJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("bench: marshal result: %w", err)
	}

	if opts.ResultJSONPath != "" {
		if err := writeBenchResultJSON(opts.ResultJSONPath, resultJSON); err != nil {
			return fmt.Errorf("bench: write result json: %w", err)
		}
	}

	if opts.JSON {
		if _, err := fmt.Fprintf(stdout, "%s\n", resultJSON); err != nil {
			return fmt.Errorf("bench: write json output: %w", err)
		}
	} else {
		if err := writeBenchSummaryText(stdout, result); err != nil {
			return fmt.Errorf("bench: write summary: %w", err)
		}
	}

	if result.Summary.Status != BenchSampleSuccess {
		return fmt.Errorf("bench: run finished with status %s", result.Summary.Status)
	}

	return nil
}

func lookupBenchSubjectDefinition(id string) (benchSubjectDefinition, error) {
	switch id {
	case DefaultBenchSubjectID:
		return benchSubjectDefinition{
			Spec: BenchSubjectSpec{
				ID:   DefaultBenchSubjectID,
				Kind: benchSubjectKindRepoCLI,
			},
			Prepare: prepareOnedriveGoBenchSubject,
		}, nil
	default:
		return benchSubjectDefinition{}, fmt.Errorf(
			"unknown bench subject %q (known: %s)",
			id,
			strings.Join(BenchSubjectIDs(), ", "),
		)
	}
}

func lookupBenchScenarioDefinition(id string) (benchScenarioDefinition, error) {
	switch id {
	case startupEmptyConfigScenarioID:
		return benchScenarioDefinition{
			Spec: BenchScenarioSpec{
				ID:            startupEmptyConfigScenarioID,
				Class:         "controlled",
				ConfigProfile: "default-safe",
				DefaultRuns:   startupEmptyConfigDefaultRuns,
				DefaultWarmup: startupEmptyConfigDefaultWarmups,
				Denominators:  BenchDenominator{},
			},
			Run: runStartupEmptyConfigSample,
		}, nil
	case syncPartialLocalCatchup100MID:
		return lookupSyncPartialLocalCatchup100MScenario()
	default:
		return benchScenarioDefinition{}, fmt.Errorf(
			"unknown bench scenario %q (known: %s)",
			id,
			strings.Join(BenchScenarioIDs(), ", "),
		)
	}
}

func prepareOnedriveGoBenchSubject(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
) (preparedBenchSubject, error) {
	gitCommit, err := gitHeadCommit(ctx, runner, repoRoot)
	if err != nil {
		return preparedBenchSubject{}, err
	}

	buildRoot, err := mkdirTemp(os.TempDir(), "onedrive-go-bench-*")
	if err != nil {
		return preparedBenchSubject{}, fmt.Errorf("create build root: %w", err)
	}

	executablePath := filepath.Join(buildRoot, "onedrive-go")
	buildOut, buildErr := runner.CombinedOutput(
		ctx,
		repoRoot,
		os.Environ(),
		"go",
		"build",
		"-o",
		executablePath,
		"./",
	)
	if buildErr != nil {
		if cleanupErr := removeAll(buildRoot); cleanupErr != nil {
			return preparedBenchSubject{}, fmt.Errorf("cleanup failed build root: %w", cleanupErr)
		}
		return preparedBenchSubject{}, fmt.Errorf(
			"go build bench subject: %w: %s",
			buildErr,
			strings.TrimSpace(string(buildOut)),
		)
	}

	return preparedBenchSubject{
		metadata: benchResultSubject{
			ID:             DefaultBenchSubjectID,
			Kind:           benchSubjectKindRepoCLI,
			GitCommit:      gitCommit,
			ExecutablePath: executablePath,
			BuildMode:      benchBuildModeRepoHead,
		},
		measure: func(ctx context.Context, spec benchCommandSpec) (benchMeasuredProcess, error) {
			args := append([]string(nil), spec.Arg...)
			return runMeasuredCommand(ctx, executablePath, spec.CWD, spec.Env, args...)
		},
		cleanup: func() error {
			return removeAll(buildRoot)
		},
	}, nil
}

func gitHeadCommit(ctx context.Context, runner commandRunner, repoRoot string) (string, error) {
	output, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve git HEAD: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func runStartupEmptyConfigSample(
	ctx context.Context,
	subject preparedBenchSubject,
	phase benchSamplePhase,
	iteration int,
) (sample benchSample) {
	sample = benchSample{
		Iteration: iteration,
		Phase:     phase,
		Status:    BenchSampleSuccess,
	}

	runtimeRoot, err := mkdirTemp(os.TempDir(), "onedrive-go-bench-runtime-*")
	if err != nil {
		return benchFixtureFailureSample(sample, fmt.Errorf("create runtime root: %w", err))
	}
	defer func() {
		if cleanupErr := removeAll(runtimeRoot); cleanupErr != nil && sample.Status == BenchSampleSuccess {
			sample = benchFixtureFailureSample(sample, fmt.Errorf("cleanup runtime root: %w", cleanupErr))
		}
	}()

	homeDir := filepath.Join(runtimeRoot, "home")
	configHome := filepath.Join(runtimeRoot, "config")
	dataHome := filepath.Join(runtimeRoot, "data")
	cacheHome := filepath.Join(runtimeRoot, "cache")
	for _, path := range []string{homeDir, configHome, dataHome, cacheHome} {
		if mkdirErr := mkdirAll(path, benchResultDirPerm); mkdirErr != nil {
			return benchFixtureFailureSample(sample, fmt.Errorf("create runtime path %s: %w", path, mkdirErr))
		}
	}

	logPath := filepath.Join(runtimeRoot, "bench.log")
	configPath := filepath.Join(runtimeRoot, "config.toml")
	configBody := fmt.Sprintf("log_level = \"info\"\nlog_file = %q\n", logPath)
	if writeErr := writeFile(configPath, []byte(configBody)); writeErr != nil {
		return benchFixtureFailureSample(sample, fmt.Errorf("write benchmark config: %w", writeErr))
	}

	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"HOME="+homeDir,
		"XDG_CONFIG_HOME="+configHome,
		"XDG_DATA_HOME="+dataHome,
		"XDG_CACHE_HOME="+cacheHome,
	)

	process, err := subject.measure(ctx, benchCommandSpec{
		CWD: runtimeRoot,
		Env: env,
		Arg: []string{"--config", configPath, "--verbose", "drive", "list", "--json"},
	})
	sample = sampleWithMeasuredProcess(sample, process)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return benchAbortedSample(sample, err)
		}

		return benchSubjectFailureSample(sample, err, process.Stdout, process.Stderr)
	}

	if validationErr := validateStartupEmptyConfigOutput(process.Stdout); validationErr != nil {
		return benchInvalidSample(sample, validationErr)
	}

	perfSummary, perfErr := readPerformanceSummary(logPath, process.Stderr)
	if perfErr != nil {
		return benchInvalidSample(sample, perfErr)
	}
	sample.PerfSummary = perfSummary

	return sample
}

func validateStartupEmptyConfigOutput(stdout []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return fmt.Errorf("parse drive list json: %w", err)
	}

	var decoded startupDriveListJSONOutput
	if err := json.Unmarshal(stdout, &decoded); err != nil {
		return fmt.Errorf("decode drive list json: %w", err)
	}

	if _, ok := raw["configured"]; !ok {
		return fmt.Errorf("drive list json missing configured array")
	}
	if _, ok := raw["available"]; !ok {
		return fmt.Errorf("drive list json missing available array")
	}
	if len(decoded.Configured) != 0 {
		return fmt.Errorf("expected configured to be empty, got %d entries", len(decoded.Configured))
	}
	if len(decoded.Available) != 0 {
		return fmt.Errorf("expected available to be empty, got %d entries", len(decoded.Available))
	}
	if authRaw, ok := raw["accounts_requiring_auth"]; ok {
		if len(bytes.TrimSpace(authRaw)) == 0 {
			return fmt.Errorf("accounts_requiring_auth present but empty payload")
		}
		if len(decoded.AccountsRequiringAuth) != 0 {
			return fmt.Errorf("expected accounts_requiring_auth to be empty when present")
		}
	}
	if degradedRaw, ok := raw["accounts_degraded"]; ok {
		if len(bytes.TrimSpace(degradedRaw)) == 0 {
			return fmt.Errorf("accounts_degraded present but empty payload")
		}
		if len(decoded.AccountsDegraded) != 0 {
			return fmt.Errorf("expected accounts_degraded to be empty when present")
		}
	}

	return nil
}

func readPerformanceSummary(logPath string, stderr []byte) (*benchPerfSummary, error) {
	if data, err := readFile(logPath); err == nil {
		if snapshot, parseErr := readPerformanceSummaryFromJSONLog(data); parseErr == nil {
			return snapshot, nil
		}
	}

	if snapshot, err := readPerformanceSummaryFromTextLog(stderr); err == nil {
		return snapshot, nil
	}

	if _, err := readFile(logPath); err != nil {
		return nil, fmt.Errorf("read perf summary: log file unavailable and stderr summary not found: %w", err)
	}

	return nil, fmt.Errorf("read perf summary: no performance summary found in log file or stderr")
}

func readPerformanceSummaryFromJSONLog(data []byte) (*benchPerfSummary, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var record benchPerfLogLine
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.Msg == "performance summary" {
			snapshot := record.benchPerfSummary
			return &snapshot, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan perf log: %w", err)
	}

	return nil, fmt.Errorf("performance summary not found in JSON log")
}

func readPerformanceSummaryFromTextLog(data []byte) (*benchPerfSummary, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.Contains(line, `msg="performance summary"`) {
			continue
		}

		snapshot := benchPerfSummary{}
		if value, ok := extractLogfmtValue(line, "duration_ms"); ok {
			if duration, err := parseInt64Value(value); err == nil {
				snapshot.DurationMS = duration
			}
		}
		if value, ok := extractLogfmtValue(line, "result"); ok {
			snapshot.Result = value
		}
		populateSnapshotInt(line, "command_items", func(v int64) { snapshot.CommandItems = int(v) })
		populateSnapshotInt64(line, "command_bytes", func(v int64) { snapshot.CommandBytes = v })
		populateSnapshotInt(line, "http_requests", func(v int64) { snapshot.HTTPRequestCount = int(v) })
		populateSnapshotInt(line, "http_successes", func(v int64) { snapshot.HTTPSuccessCount = int(v) })
		populateSnapshotInt(line, "http_client_errors", func(v int64) { snapshot.HTTPClientErrorCount = int(v) })
		populateSnapshotInt(line, "http_server_errors", func(v int64) { snapshot.HTTPServerErrorCount = int(v) })
		populateSnapshotInt(line, "http_transport_errors", func(v int64) { snapshot.HTTPTransportErrors = int(v) })
		populateSnapshotInt(line, "http_retries", func(v int64) { snapshot.HTTPRetryCount = int(v) })
		populateSnapshotInt64(line, "http_time_ms", func(v int64) { snapshot.HTTPRequestTimeMS = v })
		populateSnapshotInt64(line, "http_retry_backoff_ms", func(v int64) { snapshot.HTTPRetryBackoffMS = v })
		populateSnapshotInt(line, "db_transactions", func(v int64) { snapshot.DBTransactionCount = int(v) })
		populateSnapshotInt64(line, "db_transaction_time_ms", func(v int64) { snapshot.DBTransactionTimeMS = v })
		populateSnapshotInt(line, "downloads", func(v int64) { snapshot.DownloadCount = int(v) })
		populateSnapshotInt64(line, "download_bytes", func(v int64) { snapshot.DownloadBytes = v })
		populateSnapshotInt(line, "uploads", func(v int64) { snapshot.UploadCount = int(v) })
		populateSnapshotInt64(line, "upload_bytes", func(v int64) { snapshot.UploadBytes = v })
		populateSnapshotInt64(line, "transfer_time_ms", func(v int64) { snapshot.TransferTimeMS = v })
		populateSnapshotInt(line, "observe_runs", func(v int64) { snapshot.ObserveRunCount = int(v) })
		populateSnapshotInt(line, "observed_paths", func(v int64) { snapshot.ObservedPathCount = int(v) })
		populateSnapshotInt64(line, "observe_time_ms", func(v int64) { snapshot.ObserveTimeMS = v })
		populateSnapshotInt(line, "plan_runs", func(v int64) { snapshot.PlanRunCount = int(v) })
		populateSnapshotInt(line, "actionable_actions", func(v int64) { snapshot.ActionableActionCount = int(v) })
		populateSnapshotInt64(line, "plan_time_ms", func(v int64) { snapshot.PlanTimeMS = v })
		populateSnapshotInt(line, "execute_runs", func(v int64) { snapshot.ExecuteRunCount = int(v) })
		populateSnapshotInt(line, "execute_actions", func(v int64) { snapshot.ExecuteActionCount = int(v) })
		populateSnapshotInt(line, "execute_succeeded", func(v int64) { snapshot.ExecuteSucceededCount = int(v) })
		populateSnapshotInt(line, "execute_failed", func(v int64) { snapshot.ExecuteFailedCount = int(v) })
		populateSnapshotInt64(line, "execute_time_ms", func(v int64) { snapshot.ExecuteTimeMS = v })
		populateSnapshotInt(line, "refresh_runs", func(v int64) { snapshot.RefreshRunCount = int(v) })
		populateSnapshotInt(line, "refresh_events", func(v int64) { snapshot.RefreshEventCount = int(v) })
		populateSnapshotInt64(line, "refresh_time_ms", func(v int64) { snapshot.RefreshTimeMS = v })
		populateSnapshotInt(line, "watch_batches", func(v int64) { snapshot.WatchBatchCount = int(v) })
		populateSnapshotInt(line, "watch_paths", func(v int64) { snapshot.WatchPathCount = int(v) })

		return &snapshot, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan perf stderr: %w", err)
	}

	return nil, fmt.Errorf("performance summary not found in stderr")
}

func extractLogfmtValue(line, key string) (string, bool) {
	marker := key + "="
	index := strings.Index(line, marker)
	if index == -1 {
		return "", false
	}

	start := index + len(marker)
	if start >= len(line) {
		return "", false
	}

	if line[start] == '"' {
		end := start + 1
		for end < len(line) {
			if line[end] == '"' && line[end-1] != '\\' {
				return strings.ReplaceAll(line[start+1:end], `\"`, `"`), true
			}
			end++
		}

		return "", false
	}

	end := strings.IndexByte(line[start:], ' ')
	if end == -1 {
		return line[start:], true
	}

	return line[start : start+end], true
}

func populateSnapshotInt(line, key string, set func(int64)) {
	populateSnapshotInt64(line, key, set)
}

func populateSnapshotInt64(line, key string, set func(int64)) {
	value, ok := extractLogfmtValue(line, key)
	if !ok {
		return
	}

	parsed, err := parseInt64Value(value)
	if err != nil {
		return
	}

	set(parsed)
}

func parseInt64Value(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse int64 %q: %w", trimmed, err)
	}

	return parsed, nil
}

func collectBenchEnvironment() benchEnvironment {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	return benchEnvironment{
		Hostname:  hostname,
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
	}
}

func summarizeBenchSamples(samples []benchSample) benchSummary {
	summary := benchSummary{
		Status: BenchSampleSuccess,
	}

	elapsedValues := make([]int64, 0, len(samples))
	rssValues := make([]int64, 0, len(samples))
	userCPUValues := make([]int64, 0, len(samples))
	systemCPUValues := make([]int64, 0, len(samples))

	for _, sample := range samples {
		switch sample.Phase {
		case benchSamplePhaseWarmup:
			summary.WarmupCount++
		case benchSamplePhaseMeasured:
			summary.SampleCount++
			if sample.Status == BenchSampleSuccess {
				summary.SuccessfulSampleCount++
				elapsedValues = append(elapsedValues, sample.ElapsedMicros)
				rssValues = append(rssValues, sample.PeakRSSBytes)
				userCPUValues = append(userCPUValues, sample.UserCPUMicros)
				systemCPUValues = append(systemCPUValues, sample.SystemCPUMicros)
			}
		}

		if summary.Status == BenchSampleSuccess && sample.Status != BenchSampleSuccess {
			summary.Status = sample.Status
		}
	}

	summary.ElapsedMicros = summarizeMetric(elapsedValues)
	summary.PeakRSSBytes = summarizeMetric(rssValues)
	summary.UserCPUMicros = summarizeMetric(userCPUValues)
	summary.SystemCPUMicros = summarizeMetric(systemCPUValues)

	return summary
}

func summarizeMetric(values []int64) benchMetricStats {
	if len(values) == 0 {
		return benchMetricStats{}
	}

	sortedValues := append([]int64(nil), values...)
	sort.Slice(sortedValues, func(i, j int) bool {
		return sortedValues[i] < sortedValues[j]
	})

	var sum int64
	for _, value := range sortedValues {
		sum += value
	}

	return benchMetricStats{
		Mean:   sum / int64(len(sortedValues)),
		Median: sortedValues[len(sortedValues)/2],
		Min:    sortedValues[0],
		Max:    sortedValues[len(sortedValues)-1],
	}
}

func sampleWithMeasuredProcess(sample benchSample, process benchMeasuredProcess) benchSample {
	sample.ElapsedMicros = process.ElapsedMicros
	sample.ExitCode = process.ExitCode
	sample.UserCPUMicros = process.UserCPUMicros
	sample.SystemCPUMicros = process.SystemCPUMicros
	sample.PeakRSSBytes = process.PeakRSSBytes
	sample.StdoutBytes = int64(len(process.Stdout))
	sample.StderrBytes = int64(len(process.Stderr))
	return sample
}

func benchFixtureFailureSample(sample benchSample, err error) benchSample {
	sample.Status = BenchSampleFixtureFailed
	sample.FailureExcerpt = failureExcerpt(err, nil, nil)
	return sample
}

func benchSubjectFailureSample(sample benchSample, err error, stdout, stderr []byte) benchSample {
	sample.Status = BenchSampleSubjectFailed
	sample.FailureExcerpt = failureExcerpt(err, stdout, stderr)
	return sample
}

func benchInvalidSample(sample benchSample, err error) benchSample {
	sample.Status = BenchSampleInvalid
	sample.FailureExcerpt = failureExcerpt(err, nil, nil)
	return sample
}

func benchAbortedSample(sample benchSample, err error) benchSample {
	sample.Status = BenchSampleAborted
	sample.FailureExcerpt = failureExcerpt(err, nil, nil)
	return sample
}

func failureExcerpt(err error, stdout, stderr []byte) string {
	trimmedErr := strings.TrimSpace(err.Error())
	if excerpt := truncateFailureExcerpt(stderr); excerpt != "" {
		return excerpt
	}
	if excerpt := truncateFailureExcerpt(stdout); excerpt != "" {
		return excerpt
	}
	return truncateString(trimmedErr, benchFailureExcerptLimit)
}

func truncateFailureExcerpt(data []byte) string {
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) <= benchFailureExcerptLimit {
		return trimmed
	}
	if benchFailureExcerptLimit <= 3 {
		return trimmed[len(trimmed)-benchFailureExcerptLimit:]
	}

	return "..." + trimmed[len(trimmed)-(benchFailureExcerptLimit-3):]
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}

	return value[:limit-3] + "..."
}

func writeBenchResultJSON(path string, data []byte) error {
	if err := atomicWrite(
		path,
		data,
		benchResultFilePerm,
		benchResultDirPerm,
		".bench-result-*.tmp",
	); err != nil {
		return fmt.Errorf("atomic write benchmark result: %w", err)
	}

	return nil
}

func writeBenchSummaryText(w io.Writer, result *benchResult) error {
	lines := []string{
		"==> bench summary",
		fmt.Sprintf("subject: %s (%s)", result.Subject.ID, result.Subject.Kind),
		fmt.Sprintf("scenario: %s", result.Scenario.ID),
		fmt.Sprintf("status: %s", result.Summary.Status),
		fmt.Sprintf(
			"measured samples: %d successful / %d total (warmups: %d)",
			result.Summary.SuccessfulSampleCount,
			result.Summary.SampleCount,
			result.Summary.WarmupCount,
		),
		fmt.Sprintf(
			"elapsed_us: min=%d median=%d mean=%d max=%d",
			result.Summary.ElapsedMicros.Min,
			result.Summary.ElapsedMicros.Median,
			result.Summary.ElapsedMicros.Mean,
			result.Summary.ElapsedMicros.Max,
		),
		fmt.Sprintf(
			"peak_rss_bytes: median=%d max=%d",
			result.Summary.PeakRSSBytes.Median,
			result.Summary.PeakRSSBytes.Max,
		),
		fmt.Sprintf("user_cpu_us_mean: %d", result.Summary.UserCPUMicros.Mean),
		fmt.Sprintf("system_cpu_us_mean: %d", result.Summary.SystemCPUMicros.Mean),
	}

	if result.Run.Status != BenchSampleSuccess {
		lines = append(lines, fmt.Sprintf("failure: %s", firstBenchFailure(result.Samples)))
	}

	if _, err := fmt.Fprintf(w, "%s\n", strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("write bench summary text: %w", err)
	}

	return nil
}

func firstBenchFailure(samples []benchSample) string {
	for _, sample := range samples {
		if sample.Status != BenchSampleSuccess {
			return sample.FailureExcerpt
		}
	}

	return ""
}

func durationMicros(value time.Duration) int64 {
	return value.Microseconds()
}
