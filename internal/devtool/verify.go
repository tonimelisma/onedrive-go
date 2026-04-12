package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tonimelisma/onedrive-go/internal/clishape"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type VerifyProfile string

const (
	defaultCoverageThreshold         = 76.0
	defaultCoveragePattern           = "onedrive-go-cover.*"
	authE2EPreflightPattern          = "^TestE2E_AuthPreflight_Fast$"
	fastE2EPreflightPattern          = "^TestE2E_FixturePreflight_Fast$"
	fullE2EPreflightPattern          = "^TestE2E_FixturePreflight_Full$"
	fullE2EFixturePreflight          = "TestE2E_FixturePreflight_Full"
	fullE2EPackageTimeout            = "60m"
	fastE2EPackageTimeout            = "10m"
	stressPackageTimeout             = "20m"
	authPreflightIncidentID          = "LI-20260405-06"
	fastDownloadIncidentID           = "LI-20260405-04"
	fastDownloadTestName             = "TestE2E_Sync_DownloadOnly"
	e2eSkipSuiteScrubEnvVar          = "ONEDRIVE_E2E_SKIP_SUITE_SCRUB"
	e2eRunAuthPreflightEnvVar        = "ONEDRIVE_E2E_RUN_AUTH_PREFLIGHT"
	e2eRunFastFixturePreflightEnvVar = "ONEDRIVE_E2E_RUN_FAST_FIXTURE_PREFLIGHT"
	e2eTimingEventsFileName          = "timing-events.jsonl"
	e2eTimingSummaryFileName         = "timing-summary.json"
	e2eQuirkEventsFileName           = "quirk-events.jsonl"
	e2eQuirkSummaryFileName          = "quirk-summary.json"
	internalPackagePrefix            = "github.com/tonimelisma/onedrive-go/internal/"
	internalPackageLimit             = 27
	internalImportEdgeLimit          = 80
	internalGraphCheckTimeout        = 30 * time.Second

	fullE2EParallelMiscParallel = 5
	fullE2ESerialParallel       = 1
	fastE2EParallel             = 5

	verifySummaryFilePerm = 0o600
	verifySummaryDirPerm  = 0o700

	verifySummaryStatusPass = "pass"
	verifySummaryStatusFail = "fail"

	VerifyDefault     VerifyProfile = "default"
	VerifyPublic      VerifyProfile = "public"
	VerifyE2E         VerifyProfile = "e2e"
	VerifyE2EFull     VerifyProfile = "e2e-full"
	VerifyIntegration VerifyProfile = "integration"
	VerifyStress      VerifyProfile = "stress"
)

type VerifyOptions struct {
	RepoRoot           string
	Profile            VerifyProfile
	CoverageThreshold  float64
	CoverageFile       string
	E2ELogDir          string
	SummaryJSONPath    string
	ClassifyLiveQuirks bool
	Stdout             io.Writer
	Stderr             io.Writer
}

type VerifySummary struct {
	Profile          string                   `json:"profile"`
	Status           string                   `json:"status"`
	TotalDurationMS  int64                    `json:"total_duration_ms"`
	Steps            []VerifyStepSummary      `json:"steps"`
	QuirkEventCount  int                      `json:"quirk_event_count"`
	ClassifiedReruns []ClassifiedRerunSummary `json:"classified_reruns,omitempty"`
	E2EFullBuckets   []E2EBucketSummary       `json:"e2e_full_buckets,omitempty"`
}

type VerifyStepSummary struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type ClassifiedRerunSummary struct {
	IncidentID   string `json:"incident_id"`
	Phase        string `json:"phase"`
	Trigger      string `json:"trigger"`
	RerunCommand string `json:"rerun_command"`
	DurationMS   int64  `json:"duration_ms"`
	Status       string `json:"status"`
}

type E2EBucketSummary struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	RunPattern string `json:"run_pattern"`
	Parallel   int    `json:"parallel"`
	Timeout    string `json:"timeout"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type verifySummaryCollector struct {
	summary         VerifySummary
	stdout          io.Writer
	summaryJSONPath string
	e2eLogDir       string
	startedAt       time.Time
}

type fullE2EBucketKind string

const (
	fullE2EBucketKindParallelMisc      fullE2EBucketKind = "parallel_misc"
	fullE2EBucketKindSerialSync        fullE2EBucketKind = "serial_sync"
	fullE2EBucketKindSerialWatchShared fullE2EBucketKind = "serial_watch_shared"
)

type fullE2EBucketSpec struct {
	Name      string
	Kind      fullE2EBucketKind
	TestNames []string
	Parallel  int
	Timeout   string
}

type verifyPlan struct {
	runPublicChecks bool
	runE2E          bool
	runE2EFull      bool
	runIntegration  bool
	runStress       bool
}

type staleCheck struct {
	name    string
	pattern *regexp.Regexp
}

type documentedCLIFlagSpec struct {
	consumesValue bool
}

type documentedCLICommandSpec struct {
	name        string
	runnable    bool
	parent      *documentedCLICommandSpec
	flags       map[string]documentedCLIFlagSpec
	subcommands map[string]*documentedCLICommandSpec
}

func newVerifySummaryCollector(profile VerifyProfile, stdout io.Writer, summaryJSONPath string, e2eLogDir string) *verifySummaryCollector {
	return &verifySummaryCollector{
		summary: VerifySummary{
			Profile: string(profile),
		},
		stdout:          stdout,
		summaryJSONPath: summaryJSONPath,
		e2eLogDir:       e2eLogDir,
		startedAt:       time.Now(),
	}
}

func (c *verifySummaryCollector) runStep(name string, fn func() error) error {
	startedAt := time.Now()
	err := fn()

	step := VerifyStepSummary{
		Name:       name,
		Status:     verifySummaryStatusPass,
		DurationMS: durationMS(time.Since(startedAt)),
	}
	if err != nil {
		step.Status = verifySummaryStatusFail
		step.Error = err.Error()
	}

	c.summary.Steps = append(c.summary.Steps, step)
	return err
}

func (c *verifySummaryCollector) runBucket(bucket fullE2EBucketSpec, fn func() error) error {
	startedAt := time.Now()
	err := fn()

	summary := E2EBucketSummary{
		Name:       bucket.Name,
		Kind:       string(bucket.Kind),
		RunPattern: fullE2EBucketRunPattern(bucket.TestNames),
		Parallel:   bucket.Parallel,
		Timeout:    bucket.Timeout,
		Status:     verifySummaryStatusPass,
		DurationMS: durationMS(time.Since(startedAt)),
	}
	if err != nil {
		summary.Status = verifySummaryStatusFail
		summary.Error = err.Error()
	}

	c.summary.E2EFullBuckets = append(c.summary.E2EFullBuckets, summary)
	return err
}

func (c *verifySummaryCollector) recordClassifiedRerun(
	incidentID string,
	phase string,
	trigger string,
	rerunArgs []string,
	duration time.Duration,
	status string,
) {
	commandParts := append([]string{"go"}, rerunArgs...)
	c.summary.ClassifiedReruns = append(c.summary.ClassifiedReruns, ClassifiedRerunSummary{
		IncidentID:   incidentID,
		Phase:        phase,
		Trigger:      trigger,
		RerunCommand: strings.Join(commandParts, " "),
		DurationMS:   durationMS(duration),
		Status:       status,
	})
}

func (c *verifySummaryCollector) finalize(runErr error) error {
	if c.e2eLogDir != "" {
		quirkEventCount, err := readE2EQuirkEventCount(c.e2eLogDir)
		if err != nil {
			return err
		}
		c.summary.QuirkEventCount = quirkEventCount
	}

	c.summary.TotalDurationMS = durationMS(time.Since(c.startedAt))
	c.summary.Status = verifySummaryStatusPass
	if runErr != nil {
		c.summary.Status = verifySummaryStatusFail
	}

	if err := writeStatus(c.stdout, c.renderText()); err != nil {
		return fmt.Errorf("write verify summary: %w", err)
	}

	if c.summaryJSONPath == "" {
		return nil
	}

	data, err := json.MarshalIndent(c.summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal verify summary: %w", err)
	}
	data = append(data, '\n')
	if err := localpath.AtomicWrite(
		c.summaryJSONPath,
		data,
		verifySummaryFilePerm,
		verifySummaryDirPerm,
		".verify-summary-*.tmp",
	); err != nil {
		return fmt.Errorf("write verify summary json: %w", err)
	}

	return nil
}

func (c *verifySummaryCollector) renderText() string {
	var builder strings.Builder
	builder.WriteString("==> verify summary\n")
	fmt.Fprintf(&builder, "status: %s\n", c.summary.Status)
	fmt.Fprintf(&builder, "total: %s\n", formatDurationMS(c.summary.TotalDurationMS))

	for _, step := range c.summary.Steps {
		builder.WriteString(renderSummaryLine(step.Name, step.Status, step.DurationMS, step.Error))
	}
	for _, bucket := range c.summary.E2EFullBuckets {
		errorText := bucket.Error
		if bucket.Parallel > 0 {
			if errorText == "" {
				errorText = fmt.Sprintf("parallel=%d", bucket.Parallel)
			} else {
				errorText = fmt.Sprintf("%s; parallel=%d", errorText, bucket.Parallel)
			}
		}
		builder.WriteString(renderSummaryLine(bucket.Name, bucket.Status, bucket.DurationMS, errorText))
	}
	fmt.Fprintf(&builder, "quirk events: %d\n", c.summary.QuirkEventCount)

	if len(c.summary.ClassifiedReruns) == 0 {
		builder.WriteString("classified reruns: none\n")
		return builder.String()
	}

	builder.WriteString("classified reruns:\n")
	for _, rerun := range c.summary.ClassifiedReruns {
		fmt.Fprintf(
			&builder,
			"- %s %s %s (%s)\n",
			rerun.IncidentID,
			rerun.Phase,
			rerun.Status,
			formatDurationMS(rerun.DurationMS),
		)
	}

	return builder.String()
}

func renderSummaryLine(name string, status string, durationMS int64, errorText string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s: %s (%s)", name, status, formatDurationMS(durationMS))
	if errorText != "" {
		fmt.Fprintf(&builder, " [%s]", errorText)
	}
	builder.WriteByte('\n')
	return builder.String()
}

func durationMS(d time.Duration) int64 {
	return d.Milliseconds()
}

func formatDurationMS(durationMS int64) string {
	return (time.Duration(durationMS) * time.Millisecond).String()
}

func fullE2EParallelMiscBucket() fullE2EBucketSpec {
	return fullE2EBucketSpec{
		Name:      "full-parallel-misc",
		Kind:      fullE2EBucketKindParallelMisc,
		TestNames: fullE2EParallelMiscTestNames(),
		Parallel:  fullE2EParallelMiscParallel,
		Timeout:   fullE2EPackageTimeout,
	}
}

func fullE2ESerialSyncBucket() fullE2EBucketSpec {
	return fullE2EBucketSpec{
		Name:      "full-serial-sync",
		Kind:      fullE2EBucketKindSerialSync,
		TestNames: fullE2ESerialSyncTestNames(),
		Parallel:  fullE2ESerialParallel,
		Timeout:   fullE2EPackageTimeout,
	}
}

func fullE2ESerialWatchSharedBucket() fullE2EBucketSpec {
	return fullE2EBucketSpec{
		Name:      "full-serial-watch-shared",
		Kind:      fullE2EBucketKindSerialWatchShared,
		TestNames: fullE2ESerialWatchSharedTestNames(),
		Parallel:  fullE2ESerialParallel,
		Timeout:   fullE2EPackageTimeout,
	}
}

func fullE2EBuckets() []fullE2EBucketSpec {
	return []fullE2EBucketSpec{
		fullE2EParallelMiscBucket(),
		fullE2ESerialSyncBucket(),
		fullE2ESerialWatchSharedBucket(),
	}
}

func fullE2EStandaloneTests() []string {
	return []string{fullE2EFixturePreflight}
}

func fullE2EParallelMiscTestNames() []string {
	return []string{
		"TestE2E_DriveList_HappyPath_Text",
		"TestE2E_DriveList_JSON",
		"TestE2E_DriveList_NoAccounts",
		"TestE2E_DriveList_AccountsNoDrives",
		"TestE2E_DriveList_PersonalAccountDoesNotDuplicateCanonicalDrive",
		"TestE2E_DriveList_ConfiguredNoSyncDir",
		"TestE2E_DriveList_ConfigTolerance",
		"TestE2E_Status_ConfigTolerance",
		"TestE2E_Whoami_ConfigTolerance",
		"TestE2E_Whoami_PersonalAccountShowsSinglePersonalDrive",
		"TestE2E_Shared_FileDiscoveryAndSelectorReadCommands",
		"TestE2E_Shared_FileDiscoveryRejectsDriveAdd",
		"TestE2E_Logout_PreservesOfflineAccountCatalog",
		"TestE2E_Shared_JSON_RecipientListingUsesLiveAccountCatalog",
		"TestE2E_RoundTrip",
		"TestE2E_ErrorCases",
		"TestE2E_JSONOutput",
		"TestE2E_QuietFlag",
		"TestE2E_DriveList_AllFlag",
		"TestE2E_DriveList_StaleStateDB",
		"TestE2E_ZeroByteFileSync",
		"TestE2E_UnicodeFilenameRoundtrip",
		"TestE2E_InvalidFilenameRejection",
		"TestE2E_RapidFileChurn",
		"TestE2E_ConflictDetectionAndResolution",
		"TestE2E_Status_AfterSync",
		"TestE2E_Status_JSON",
		"TestE2E_Status_PausedDrive",
		"TestE2E_Pause_WithDuration",
		"TestE2E_Pause_IndefiniteAndResume",
		"TestE2E_Resume_NotPaused",
		"TestE2E_Resume_AllDrives",
		"TestE2E_Status_PerDrive_NoVisibleProblems",
		"TestE2E_Status_History_NoConflicts",
		"TestE2E_Status_JSON_ConflictDetails",
		"TestE2E_Status_History_ShowsResolvedStrategies",
		"TestE2E_Resolve_TargetNotFound",
		"TestE2E_InternalBaselineVerification_AfterSync",
		"TestE2E_RecycleBinRoundtrip",
		"TestE2E_RecycleBinEmpty",
		"TestE2E_Mv_Rename",
		"TestE2E_Mv_MoveToFolder",
		"TestE2E_Mv_JSON",
		"TestE2E_Mv_NotFound",
		"TestE2E_Cp_File",
		"TestE2E_Cp_IntoFolder",
		"TestE2E_Cp_JSON",
		"TestE2E_Cp_NotFound",
		"TestE2E_Mv_ForceOverwrite",
		"TestE2E_Cp_ForceOverwrite",
		"TestE2E_Mv_Folder",
		"TestE2E_Status_IssueLifecycle",
		"TestE2E_Status_JSONShape",
		"TestE2E_Status_FilteredDriveIsSubsetOfAllDrives",
		"TestE2E_Status_NoDrives",
		"TestE2E_Sync_QuietMode",
		"TestE2E_Sync_NosyncGuard",
		"TestE2E_Sync_MtimeOnlyChange",
		"TestE2E_Sync_TransferWorkersConfig",
		"TestE2E_Sync_IncrementalDeltaToken",
		"TestE2E_Sync_DriveRemovePurgeResetsState",
	}
}

func fullE2ESerialSyncTestNames() []string {
	return []string{
		"TestE2E_Sync_DryRun",
		"TestE2E_Sync_InternalBaselineVerification",
		"TestE2E_Sync_Conflicts",
		"TestE2E_Sync_DriveRemoveAndReAdd",
		"TestE2E_Sync_SyncPathsExactFileDownloadsOnlySelectedRemoteFile",
		"TestE2E_Sync_IgnoreMarkerRemovalReconcilesBlockedRemoteDownload",
		"TestE2E_EdgeCases",
		"TestE2E_Sync_BidirectionalMerge",
		"TestE2E_Sync_EditEditConflict_ResolveKeepRemote",
		"TestE2E_Sync_EditDeleteConflict",
		"TestE2E_Sync_ResolveAll",
		"TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal",
		"TestE2E_Sync_DeletePropagation",
		"TestE2E_Sync_DeleteSafetyThreshold",
		"TestE2E_Sync_DownloadOnlyIgnoresLocal",
		"TestE2E_Sync_UploadOnlyIgnoresRemote",
		"TestE2E_Sync_NestedFolderHierarchy",
		"TestE2E_Sync_DryRunNonDestructive",
		"TestE2E_Sync_ConvergentEdit",
		"TestE2E_Sync_InternalBaselineVerificationDetectsTampering",
		"TestE2E_Sync_ResolveDryRun",
		"TestE2E_Sync_EmptyDirectory",
		"TestE2E_Sync_NestedDeletion",
		"TestE2E_Sync_ResolveKeepLocalThenSync",
		"TestE2E_Sync_ResolveKeepRemoteThenSync",
		"TestE2E_Sync_IdempotentReSync",
		"TestE2E_Sync_CrashRecoveryIdempotent",
		"TestE2E_Sync_ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta",
		"TestE2E_Resolve_Both_PreservesConflictCopy",
	}
}

func fullE2ESerialWatchSharedTestNames() []string {
	return []string{
		"TestE2E_SyncWatch_WebsocketStartupSmoke",
		"TestE2E_Resolve_WithWatchDaemonExecutesQueuedIntent",
		"TestE2E_Resolve_DeletesWithWatchDaemon",
		"TestE2E_Sync_MultiDriveReport",
		"TestE2E_SyncWatch_RemoteToLocal",
		"TestE2E_SyncWatch_Bidirectional",
		"TestE2E_SyncWatch_ConflictDuringWatch",
		"TestE2E_SyncWatch_FileModification",
		"TestE2E_SyncWatch_FileDeletion",
		"TestE2E_SyncWatch_FolderCreation",
		"TestE2E_SyncWatch_MultipleFiles",
		"TestE2E_SyncWatch_LargeFile",
		"TestE2E_SyncWatch_RapidChurn",
		"TestE2E_SyncWatch_GracefulShutdown",
		"TestE2E_SyncWatch_TimedPauseExpiry",
		"TestE2E_SyncWatch_BasicRoundTrip",
		"TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners",
		"TestE2E_SyncWatch_PauseResume",
		"TestE2E_SyncWatch_ControlSocketReload",
		"TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart",
		"TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip",
		"TestE2E_Shared_FolderDiscoveryContinuesToDriveAdd",
		"TestE2E_Shared_RawFolderLinkDriveAdd_NormalizesToCanonicalSharedDrive",
		"TestE2E_Shared_FolderNameDriveAdd_HonorsAccountFilter",
		"TestE2E_Shared_ReadOnlyFolder_DiscoveryDriveAddAndBlockedWriteUX",
		"TestE2E_SharedFolder_DriveList_ShowsExplicitSharedFixtures",
		"TestE2E_SharedFolder_RemoteMutationSyncsToRecipient",
		"TestE2E_SharedFolder_RecipientSyncTwice_Idempotent",
		"TestE2E_Orchestrator_SimultaneousSync",
		"TestE2E_Orchestrator_Status",
		"TestE2E_Orchestrator_DriveIsolation",
		"TestE2E_Orchestrator_OneDriveFails",
		"TestE2E_Orchestrator_SelectiveDrive",
		"TestE2E_Orchestrator_WatchSimultaneous",
		"TestE2E_Orchestrator_WatchDriveIsolation",
		"TestE2E_Orchestrator_WatchPausedDrive",
	}
}

func fullE2EBucketCommandArgs(bucket fullE2EBucketSpec) []string {
	return []string{
		"test",
		"-tags=e2e e2e_full",
		"-race",
		"-v",
		"-run=" + fullE2EBucketRunPattern(bucket.TestNames),
		"-parallel",
		strconv.Itoa(bucket.Parallel),
		"-timeout=" + bucket.Timeout,
		"./e2e/...",
	}
}

func fullE2EBucketRunPattern(testNames []string) string {
	return "^(" + strings.Join(testNames, "|") + ")$"
}

func resetE2EArtifacts(logDir string) error {
	if logDir == "" {
		return nil
	}

	for _, name := range []string{
		e2eTimingEventsFileName,
		e2eTimingSummaryFileName,
		e2eQuirkEventsFileName,
		e2eQuirkSummaryFileName,
	} {
		path := filepath.Join(logDir, name)
		if err := localpath.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	return nil
}

func readE2EQuirkEventCount(logDir string) (int, error) {
	if logDir == "" {
		return 0, nil
	}

	summaryPath := filepath.Join(logDir, e2eQuirkSummaryFileName)
	data, err := localpath.ReadFile(summaryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, fmt.Errorf("read quirk summary %s: %w", summaryPath, err)
	}

	var summary struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(data, &summary); err != nil {
		return 0, fmt.Errorf("decode quirk summary %s: %w", summaryPath, err)
	}

	return len(summary.Events), nil
}

func discoverTaggedE2ETests(e2eDir string, buildExpression string) ([]string, error) {
	entries, err := localpath.ReadDir(e2eDir)
	if err != nil {
		return nil, fmt.Errorf("read e2e dir: %w", err)
	}

	tests := make([]string, 0)
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(e2eDir, entry.Name())
		data, err := localpath.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if !hasBuildExpression(string(data), buildExpression) {
			continue
		}

		file, err := parser.ParseFile(fset, path, data, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}

			tests = append(tests, fn.Name.Name)
		}
	}

	sort.Strings(tests)
	return tests, nil
}

func hasBuildExpression(source string, buildExpression string) bool {
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			break
		}
		if !strings.HasPrefix(line, "//go:build ") {
			continue
		}

		return strings.TrimSpace(strings.TrimPrefix(line, "//go:build ")) == buildExpression
	}

	return false
}

func RunVerify(ctx context.Context, runner commandRunner, opts *VerifyOptions) (runErr error) {
	if opts == nil {
		return fmt.Errorf("verify options are required")
	}

	plan, err := resolveVerifyPlan(opts.Profile)
	if err != nil {
		return err
	}

	stdout, stderr := resolveVerifyWriters(opts)
	effectiveE2ELogDir := resolvedE2ELogDir(opts.E2ELogDir, plan)
	collector := newVerifySummaryCollector(opts.Profile, stdout, opts.SummaryJSONPath, effectiveE2ELogDir)
	defer func() {
		if finalizeErr := collector.finalize(runErr); finalizeErr != nil {
			if runErr == nil {
				runErr = finalizeErr
			} else {
				runErr = errors.Join(runErr, finalizeErr)
			}
		}
	}()
	coverageThreshold := opts.CoverageThreshold
	if coverageThreshold == 0 {
		coverageThreshold = defaultCoverageThreshold
	}

	coverageFile, cleanup, err := prepareCoverageFile(plan, opts.CoverageFile)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil && runErr == nil {
			runErr = cleanupErr
		}
	}()

	env := os.Environ()
	if err := runPublicVerification(
		ctx,
		runner,
		opts.RepoRoot,
		env,
		collector,
		stdout,
		stderr,
		coverageFile,
		coverageThreshold,
		plan,
	); err != nil {
		return err
	}

	runErr = runOptionalVerification(ctx, runner, opts, env, collector, stdout, stderr, plan)

	return runErr
}

func resolveVerifyPlan(profile VerifyProfile) (verifyPlan, error) {
	switch profile {
	case VerifyDefault:
		return verifyPlan{
			runPublicChecks: true,
			runE2E:          true,
		}, nil
	case VerifyPublic:
		return verifyPlan{
			runPublicChecks: true,
		}, nil
	case VerifyE2E:
		return verifyPlan{
			runE2E: true,
		}, nil
	case VerifyE2EFull:
		return verifyPlan{
			runE2E:     true,
			runE2EFull: true,
		}, nil
	case VerifyIntegration:
		return verifyPlan{
			runIntegration: true,
		}, nil
	case VerifyStress:
		return verifyPlan{
			runStress: true,
		}, nil
	default:
		return verifyPlan{}, fmt.Errorf("usage: devtool verify [default|public|e2e|e2e-full|integration|stress]")
	}
}

func resolveVerifyWriters(opts *VerifyOptions) (io.Writer, io.Writer) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	return stdout, stderr
}

func prepareCoverageFile(plan verifyPlan, coverageFile string) (string, func() error, error) {
	if !plan.runPublicChecks || coverageFile != "" {
		return coverageFile, func() error { return nil }, nil
	}

	f, err := localpath.CreateTemp(os.TempDir(), defaultCoveragePattern)
	if err != nil {
		return "", nil, fmt.Errorf("create coverage file: %w", err)
	}

	coverageFile = f.Name()
	if err := f.Close(); err != nil {
		return "", nil, fmt.Errorf("close coverage file: %w", err)
	}

	return coverageFile, func() error {
		if err := localpath.Remove(coverageFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove coverage file %s: %w", coverageFile, err)
		}

		return nil
	}, nil
}

func runPublicVerification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	coverageFile string,
	coverageThreshold float64,
	plan verifyPlan,
) error {
	if !plan.runPublicChecks {
		return nil
	}

	publicSteps := []func(context.Context, commandRunner, string, []string, io.Writer, io.Writer) error{
		runFormat,
		runLint,
		runBuild,
	}
	publicStepNames := []string{"format", "lint", "build"}
	for i, step := range publicSteps {
		if err := collector.runStep(publicStepNames[i], func() error {
			return step(ctx, runner, repoRoot, env, stdout, stderr)
		}); err != nil {
			return err
		}
	}

	if err := collector.runStep("unit tests", func() error {
		return runUnitTests(ctx, runner, repoRoot, env, coverageFile, stdout, stderr)
	}); err != nil {
		return err
	}
	if err := collector.runStep("coverage", func() error {
		return runCoverageGate(ctx, runner, repoRoot, env, coverageFile, coverageThreshold, stdout)
	}); err != nil {
		return err
	}
	if err := collector.runStep("repo consistency", func() error {
		return runRepoConsistencyChecks(repoRoot)
	}); err != nil {
		return err
	}

	return nil
}

func runOptionalVerification(
	ctx context.Context,
	runner commandRunner,
	opts *VerifyOptions,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	plan verifyPlan,
) error {
	e2eEnv := env
	effectiveE2ELogDir := resolvedE2ELogDir(opts.E2ELogDir, plan)
	if effectiveE2ELogDir != "" {
		e2eEnv = append([]string{}, env...)
		e2eEnv = append(e2eEnv, "E2E_LOG_DIR="+effectiveE2ELogDir)
		if err := resetE2EArtifacts(effectiveE2ELogDir); err != nil {
			return err
		}
	}

	if plan.runE2E {
		if err := runE2E(ctx, runner, opts.RepoRoot, e2eEnv, collector, stdout, stderr, opts.ClassifyLiveQuirks); err != nil {
			return err
		}
	}

	if plan.runE2EFull {
		if err := runE2EFull(ctx, runner, opts.RepoRoot, e2eEnv, effectiveE2ELogDir, collector, stdout, stderr); err != nil {
			return err
		}
	}

	if plan.runIntegration {
		if err := runIntegration(ctx, runner, opts.RepoRoot, env, collector, stdout, stderr); err != nil {
			return err
		}
	}
	if plan.runStress {
		if err := runStress(ctx, runner, opts.RepoRoot, env, collector, stdout, stderr); err != nil {
			return err
		}
	}

	return nil
}

func resolvedE2ELogDir(explicit string, plan verifyPlan) string {
	if explicit != "" {
		return explicit
	}
	if plan.runE2E || plan.runE2EFull {
		return filepath.Join(os.TempDir(), "e2e-debug-logs")
	}

	return ""
}

func runFormat(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> gofumpt\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "gofumpt", "-w", "."); err != nil {
		return fmt.Errorf("format gofumpt: %w", err)
	}

	if err := writeStatus(stdout, "==> goimports\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"goimports",
		"-local",
		"github.com/tonimelisma/onedrive-go",
		"-w",
		".",
	); err != nil {
		return fmt.Errorf("format goimports: %w", err)
	}

	return nil
}

func runLint(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> golangci-lint\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "golangci-lint", "run", "--allow-parallel-runners"); err != nil {
		return fmt.Errorf("lint: %w", err)
	}

	return nil
}

func runBuild(ctx context.Context, runner commandRunner, repoRoot string, env []string, stdout, stderr io.Writer) error {
	if err := writeStatus(stdout, "==> go build\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", "build", "./..."); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	return nil
}

func runUnitTests(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	stdout, stderr io.Writer,
) error {
	if err := writeStatus(stdout, "==> go test -race -coverprofile\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(
		ctx,
		repoRoot,
		env,
		stdout,
		stderr,
		"go",
		"test",
		"-race",
		"-coverprofile="+coverageFile,
		"./...",
	); err != nil {
		return fmt.Errorf("unit tests: %w", err)
	}

	return nil
}

func runCoverageGate(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	coverageFile string,
	coverageThreshold float64,
	stdout io.Writer,
) error {
	if err := writeStatus(stdout, "==> coverage\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	report, err := runner.Output(ctx, repoRoot, env, "go", "tool", "cover", "-func="+coverageFile)
	if err != nil {
		return fmt.Errorf("coverage report: %w", err)
	}

	if writeErr := writeStatus(stdout, string(report)); writeErr != nil {
		return fmt.Errorf("write coverage report: %w", writeErr)
	}

	coverageTotal, err := parseCoverageTotal(string(report))
	if err != nil {
		return err
	}

	if coverageTotal < coverageThreshold {
		return fmt.Errorf("coverage gate failed: %.1f%% < %.1f%%", coverageTotal, coverageThreshold)
	}

	return nil
}

func parseCoverageTotal(report string) (float64, error) {
	for _, line := range strings.Split(report, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "total:" {
			continue
		}

		value := strings.TrimSuffix(fields[2], "%")
		coverageTotal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse coverage total %q: %w", value, err)
		}

		return coverageTotal, nil
	}

	return 0, fmt.Errorf("parse coverage total: total line not found")
}

func runE2E(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	classifyLiveQuirks bool,
) error {
	authEnv := append([]string{}, env...)
	authEnv = append(authEnv, e2eRunAuthPreflightEnvVar+"=1")
	if err := runE2EPreflightAuth(
		ctx,
		runner,
		repoRoot,
		authEnv,
		collector,
		stdout,
		stderr,
		classifyLiveQuirks,
	); err != nil {
		return err
	}

	fastFixtureEnv := append([]string{}, env...)
	fastFixtureEnv = append(
		fastFixtureEnv,
		e2eRunFastFixturePreflightEnvVar+"=1",
		e2eSkipSuiteScrubEnvVar+"=1",
	)
	if err := runE2EPreflightFast(ctx, runner, repoRoot, fastFixtureEnv, collector, stdout, stderr); err != nil {
		return err
	}

	fastSuiteEnv := append([]string{}, env...)
	fastSuiteEnv = append(fastSuiteEnv, e2eSkipSuiteScrubEnvVar+"=1")

	return runFastE2ESuite(ctx, runner, repoRoot, fastSuiteEnv, collector, stdout, stderr, classifyLiveQuirks)
}

func runFastE2ESuite(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	classifyLiveQuirks bool,
) error {
	return collector.runStep("fast e2e", func() error {
		if !classifyLiveQuirks {
			return runFastE2ESuiteOnce(ctx, runner, repoRoot, env, stdout, stderr)
		}

		return runFastE2ESuiteWithClassification(ctx, runner, repoRoot, env, collector, stdout, stderr)
	})
}

func runFastE2ESuiteOnce(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
) error {
	if err := writeStatus(stdout, "==> go test -tags=e2e\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", fastE2EArgs()...); err != nil {
		return fmt.Errorf("fast e2e: %w", err)
	}

	return nil
}

func runFastE2ESuiteWithClassification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	if err := writeStatus(stdout, "==> go test -json -tags=e2e\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	output, err := runner.CombinedOutput(ctx, repoRoot, env, "go", fastE2EJSONArgs()...)
	if writeErr := writeCommandOutput(stdout, output); writeErr != nil {
		return fmt.Errorf("write fast e2e output: %w", writeErr)
	}
	if err == nil {
		return nil
	}

	return rerunClassifiedFastE2EQuirk(ctx, runner, repoRoot, env, collector, stdout, stderr, output, err)
}

func rerunClassifiedFastE2EQuirk(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	output []byte,
	runErr error,
) error {
	failedTests := failedGoTestsFromJSON(output)
	rerunArgs, incidentID, ok := classifyFastE2EQuirk(failedTests)
	if !ok {
		return fmt.Errorf("fast e2e: %w", runErr)
	}

	if writeErr := writeStatus(stdout, fmt.Sprintf("==> rerun known live quirk %s\n", incidentID)); writeErr != nil {
		return fmt.Errorf("write status: %w", writeErr)
	}

	rerunStartedAt := time.Now()
	rerunErr := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", rerunArgs...)
	rerunStatus := verifySummaryStatusPass
	if rerunErr != nil {
		rerunStatus = verifySummaryStatusFail
	}
	collector.recordClassifiedRerun(
		incidentID,
		"fast e2e",
		fastDownloadTestName,
		rerunArgs,
		time.Since(rerunStartedAt),
		rerunStatus,
	)
	if rerunErr != nil {
		return fmt.Errorf("fast e2e: %w", runErr)
	}

	warning := fmt.Sprintf("warning: classified known live quirk %s after successful rerun\n", incidentID)
	if writeErr := writeStatus(stdout, warning); writeErr != nil {
		return fmt.Errorf("write status: %w", writeErr)
	}

	return nil
}

func runE2EPreflightAuth(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	classifyLiveQuirks bool,
) error {
	return collector.runStep("auth preflight", func() error {
		if err := writeStatus(stdout, "==> go test -tags=e2e auth preflight\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		authArgs := []string{
			"test",
			"-tags=e2e",
			"-run=" + authE2EPreflightPattern,
			"-count=1",
			"-v",
			"./e2e/...",
		}
		if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", authArgs...); err != nil {
			if !classifyLiveQuirks {
				return fmt.Errorf("fast e2e auth preflight: %w", err)
			}

			if writeErr := writeStatus(stdout, fmt.Sprintf("==> rerun known live quirk %s\n", authPreflightIncidentID)); writeErr != nil {
				return fmt.Errorf("write status: %w", writeErr)
			}

			rerunStartedAt := time.Now()
			rerunErr := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", authArgs...)
			rerunStatus := verifySummaryStatusPass
			if rerunErr != nil {
				rerunStatus = verifySummaryStatusFail
			}
			collector.recordClassifiedRerun(
				authPreflightIncidentID,
				"auth preflight",
				authE2EPreflightPattern,
				authArgs,
				time.Since(rerunStartedAt),
				rerunStatus,
			)
			if rerunErr == nil {
				warning := fmt.Sprintf(
					"warning: classified known live quirk %s after successful rerun\n",
					authPreflightIncidentID,
				)
				if writeErr := writeStatus(stdout, warning); writeErr != nil {
					return fmt.Errorf("write status: %w", writeErr)
				}

				return nil
			}

			return fmt.Errorf("fast e2e auth preflight: %w", err)
		}

		return nil
	})
}

func runE2EPreflightFast(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep("fast fixture preflight", func() error {
		if err := writeStatus(stdout, "==> go test -tags=e2e fixture preflight\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if err := runner.Run(
			ctx,
			repoRoot,
			env,
			stdout,
			stderr,
			"go",
			"test",
			"-tags=e2e",
			"-run="+fastE2EPreflightPattern,
			"-count=1",
			"-v",
			"./e2e/...",
		); err != nil {
			return fmt.Errorf("fast e2e fixture preflight: %w", err)
		}

		return nil
	})
}

func runE2EFull(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	logDir string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	if logDir == "" {
		logDir = filepath.Join(os.TempDir(), "e2e-debug-logs")
	}

	fullEnv := append([]string{}, env...)
	fullEnv = append(fullEnv, "E2E_LOG_DIR="+logDir)

	if err := runE2EPreflightFull(ctx, runner, repoRoot, fullEnv, collector, stdout, stderr); err != nil {
		return err
	}

	bucketEnv := append([]string{}, fullEnv...)
	bucketEnv = append(bucketEnv, e2eSkipSuiteScrubEnvVar+"=1")

	buckets := fullE2EBuckets()
	for i := range buckets {
		bucket := buckets[i]
		if err := collector.runBucket(bucket, func() error {
			if err := writeStatus(stdout, fmt.Sprintf("==> go test -tags='e2e e2e_full' %s\n", bucket.Name)); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
			if err := runner.Run(
				ctx,
				repoRoot,
				bucketEnv,
				stdout,
				stderr,
				"go",
				fullE2EBucketCommandArgs(bucket)...,
			); err != nil {
				return fmt.Errorf("full e2e: %w", err)
			}

			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func fastE2EArgs() []string {
	return []string{
		"test",
		"-tags=e2e",
		"-v",
		"-parallel",
		strconv.Itoa(fastE2EParallel),
		"-timeout=" + fastE2EPackageTimeout,
		"./e2e/...",
	}
}

func fastE2EJSONArgs() []string {
	args := fastE2EArgs()
	return append([]string{args[0], "-json"}, args[1:]...)
}

func classifyFastE2EQuirk(failedTests map[string]struct{}) ([]string, string, bool) {
	if len(failedTests) != 1 {
		return nil, "", false
	}

	if _, ok := failedTests[fastDownloadTestName]; !ok {
		return nil, "", false
	}

	return []string{
		"test",
		"-tags=e2e",
		"-run=^" + fastDownloadTestName + "$",
		"-count=1",
		"-v",
		"./e2e/...",
	}, fastDownloadIncidentID, true
}

func failedGoTestsFromJSON(output []byte) map[string]struct{} {
	failed := make(map[string]struct{})

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event struct {
			Action string
			Test   string
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Action != "fail" || event.Test == "" {
			continue
		}

		failed[event.Test] = struct{}{}
	}

	return failed
}

func writeCommandOutput(w io.Writer, output []byte) error {
	if len(output) == 0 {
		return nil
	}

	_, err := w.Write(output)
	if err != nil {
		return fmt.Errorf("write command output: %w", err)
	}

	if output[len(output)-1] != '\n' {
		if _, err := w.Write([]byte("\n")); err != nil {
			return fmt.Errorf("write command output newline: %w", err)
		}
	}

	return nil
}

func runE2EPreflightFull(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep("full fixture preflight", func() error {
		if err := writeStatus(stdout, "==> go test -tags='e2e e2e_full' fixture preflight\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if err := runner.Run(
			ctx,
			repoRoot,
			env,
			stdout,
			stderr,
			"go",
			"test",
			"-tags=e2e e2e_full",
			"-run="+fullE2EPreflightPattern,
			"-count=1",
			"-v",
			"./e2e/...",
		); err != nil {
			return fmt.Errorf("full e2e fixture preflight: %w", err)
		}

		return nil
	})
}

func runIntegration(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep("integration", func() error {
		if err := writeStatus(stdout, "==> go test -tags=integration\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if err := runner.Run(
			ctx,
			repoRoot,
			env,
			stdout,
			stderr,
			"go",
			"test",
			"-tags=integration",
			"-race",
			"-v",
			"-timeout=5m",
			"./internal/graph/...",
		); err != nil {
			return fmt.Errorf("integration tests: %w", err)
		}

		return nil
	})
}

func runStress(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return collector.runStep("stress", func() error {
		stressCommands := []struct {
			statusLine string
			args       []string
		}{
			{
				statusLine: fmt.Sprintf(
					"==> go test -tags=stress -race -count=50 -timeout=%s "+
						"-run TestWatchOrderingStress_ ./internal/sync\n",
					stressPackageTimeout,
				),
				args: []string{
					"go", "test",
					"-tags=stress",
					"-race",
					"-count=50",
					"-timeout=" + stressPackageTimeout,
					"-run", "TestWatchOrderingStress_",
					"./internal/sync",
				},
			},
			{
				statusLine: fmt.Sprintf(
					"==> go test -race -count=50 -timeout=%s "+
						"./internal/multisync ./internal/sync\n",
					stressPackageTimeout,
				),
				args: []string{
					"go", "test",
					"-race",
					"-count=50",
					"-timeout=" + stressPackageTimeout,
					"./internal/multisync",
					"./internal/sync",
				},
			},
		}

		for _, command := range stressCommands {
			if err := writeStatus(stdout, command.statusLine); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
			if err := runner.Run(ctx, repoRoot, env, stdout, stderr, command.args[0], command.args[1:]...); err != nil {
				return fmt.Errorf("stress tests: %w", err)
			}
		}

		return nil
	})
}

func runRepoConsistencyChecks(repoRoot string) error {
	for _, check := range []func(string) error{
		ensureNoStaleArchitecturePhrases,
		ensureActiveDocCLIExamplesResolve,
		ensureSyncStoreMigrationDiscipline,
		ensureInternalDependencyGraphGuardrails,
		ensureGovernedDesignDocsHaveOwnershipContracts,
		ensureCrossCuttingDesignDocs,
		ensureCrossCuttingDesignDocEvidence,
		ensureGovernedBehaviorDocsHaveEvidence,
		ensureRequirementReferencesResolve,
		ensureEvidenceDocsReferenceRealTests,
		ensureRecurringIncidentPromotedDocsResolve,
		ensureCLIOutputBoundary,
		ensureGuardedPackagesAvoidRawOS,
		ensureFilterSemanticsWording,
		ensureHTTPClientDoStaysAtApprovedBoundary,
		ensurePrivilegedPackageCallsStayAtApprovedBoundaries,
		ensureNoForbiddenProductionPatterns,
		ensureNoResurrectedFiles,
	} {
		if err := check(repoRoot); err != nil {
			return err
		}
	}

	return nil
}

func ensureInternalDependencyGraphGuardrails(repoRoot string) error {
	ctx, cancel := context.WithTimeout(context.Background(), internalGraphCheckTimeout)
	defer cancel()

	output, err := ExecRunner{}.CombinedOutput(
		ctx,
		repoRoot,
		os.Environ(),
		"go",
		"list",
		"-f",
		"{{.ImportPath}} {{join .Imports \" \"}}",
		"./internal/...",
	)
	if err != nil {
		return fmt.Errorf("list internal package graph: %w\n%s", err, strings.TrimSpace(string(output)))
	}

	packages := make(map[string]struct{})
	edges := make(map[[2]string]struct{})
	synctypesTargets := make(map[string]struct{})

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		pkg := fields[0]
		if !strings.HasPrefix(pkg, internalPackagePrefix) {
			continue
		}
		packages[pkg] = struct{}{}

		for _, imp := range fields[1:] {
			if !strings.HasPrefix(imp, internalPackagePrefix) {
				continue
			}
			edges[[2]string{pkg, imp}] = struct{}{}
			if pkg == internalPackagePrefix+"synctypes" {
				synctypesTargets[imp] = struct{}{}
			}
		}
	}

	if len(packages) > internalPackageLimit {
		return fmt.Errorf(
			"internal package graph exceeds limit: %d packages (limit %d)",
			len(packages),
			internalPackageLimit,
		)
	}
	if len(edges) > internalImportEdgeLimit {
		return fmt.Errorf(
			"internal package graph exceeds limit: %d import edges (limit %d)",
			len(edges),
			internalImportEdgeLimit,
		)
	}

	for _, forbidden := range [][2]string{
		{internalPackagePrefix + "cli", internalPackagePrefix + "synctypes"},
		{internalPackagePrefix + "multisync", internalPackagePrefix + "synctypes"},
		{internalPackagePrefix + "syncstore", internalPackagePrefix + "sync"},
	} {
		if _, ok := edges[forbidden]; ok {
			return fmt.Errorf("forbidden internal import edge detected: %s -> %s", forbidden[0], forbidden[1])
		}
	}

	for target := range synctypesTargets {
		if target != internalPackagePrefix+"driveid" {
			return fmt.Errorf("synctypes may only import internal/driveid, found %s", target)
		}
	}

	return nil
}

const documentedCLIExecutableName = "onedrive-go"

var documentedCLIExamplePattern = regexp.MustCompile(documentedCLIExecutableName + "(?:\\s+[^\\s`'\"\\)\\(]+)+")

func ensureNoStaleArchitecturePhrases(repoRoot string) error {
	staleChecks := []staleCheck{
		{name: "stale watch-startup phrase", pattern: regexp.MustCompile("RunWatch calls" + " RunOnce")},
		{name: "stale retry delay phrase", pattern: regexp.MustCompile(`retry\.Reconcile` + `\.Delay`)},
		{name: "stale retry transport phrase", pattern: regexp.MustCompile(`RetryTransport\{Policy:\s*` + `Transport\}`)},
		{name: "stale compatibility-wrapper phrase", pattern: regexp.MustCompile("compatibility" + " wrapper")},
		{name: "stale legacy-bridge phrase", pattern: regexp.MustCompile("migra" + "tion" + " bridge")},
	}

	checkRoots := []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, ".github", "workflows"),
		filepath.Join(repoRoot, "CLAUDE.md"),
	}

	for _, check := range staleChecks {
		match, err := findTextMatch(checkRoots, check.pattern, nil)
		if err != nil {
			return err
		}
		if match != "" {
			return fmt.Errorf("stale architecture/documentation phrase detected (%s): %s", check.name, match)
		}
	}

	return nil
}

func ensureActiveDocCLIExamplesResolve(repoRoot string) error {
	checkRoots := []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "reference"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "README.md"),
		filepath.Join(repoRoot, "CLAUDE.md"),
		filepath.Join(repoRoot, "AGENTS.md"),
	}

	resolver := documentedCLIExampleResolver{
		root: currentDocumentedCLIManifest(),
	}

	for _, root := range checkRoots {
		if err := walkDocumentedCLIExamples(root, resolver.resolve); err != nil {
			return err
		}
	}

	return nil
}

type documentedCLIExampleResolver struct {
	root *documentedCLICommandSpec
}

func (r documentedCLIExampleResolver) resolve(snippet string) error {
	args := strings.Fields(strings.TrimSpace(snippet))
	if !isDocumentedCLIExample(args) {
		return nil
	}

	current, commandSeen, err := r.resolveTokens(args[1:])
	if err != nil {
		return err
	}

	if !commandSeen {
		return fmt.Errorf("no command found")
	}
	if !current.runnable && len(current.subcommands) > 0 {
		return fmt.Errorf("command %q is incomplete", current.commandPath())
	}

	return nil
}

func isDocumentedCLIExample(args []string) bool {
	return len(args) != 0 && args[0] == documentedCLIExecutableName
}

func (r documentedCLIExampleResolver) resolveTokens(
	args []string,
) (*documentedCLICommandSpec, bool, error) {
	current := r.root
	commandSeen := false
	for i := 0; i < len(args); i++ {
		nextIndex, nextCommand, nextSeen, stop, err := documentedCLIConsumeToken(current, commandSeen, args, i)
		if err != nil {
			return nil, false, err
		}
		current = nextCommand
		commandSeen = nextSeen
		if stop {
			break
		}
		i = nextIndex
	}

	return current, commandSeen, nil
}

func documentedCLIConsumeToken(
	current *documentedCLICommandSpec,
	commandSeen bool,
	args []string,
	index int,
) (nextIndex int, nextCommand *documentedCLICommandSpec, nextSeen bool, stop bool, err error) {
	token := strings.TrimRight(strings.TrimSpace(args[index]), ".,:;")
	if token == "" {
		return index, current, commandSeen, false, nil
	}
	if strings.HasPrefix(token, "<") && strings.HasSuffix(token, ">") {
		if !commandSeen {
			return index, nil, false, false, fmt.Errorf("placeholder %s appears before any command", token)
		}

		return index, current, commandSeen, true, nil
	}
	if strings.HasPrefix(token, "-") {
		consumesValue, flagErr := documentedCLIFlagConsumesValue(current, token)
		if flagErr != nil {
			return index, nil, false, false, flagErr
		}
		if consumesValue && !strings.Contains(token, "=") {
			index++
			if index >= len(args) {
				return index, nil, false, false, fmt.Errorf("flag %s is missing its value", token)
			}
		}

		return index, current, commandSeen, false, nil
	}

	next := documentedCLISubcommand(current, token)
	if next == nil {
		if !commandSeen {
			return index, nil, false, false, fmt.Errorf("unknown command %q", token)
		}

		return index, current, commandSeen, true, nil
	}

	return index, next, true, false, nil
}

func documentedCLIFlagConsumesValue(cmd *documentedCLICommandSpec, token string) (bool, error) {
	name := documentedCLIFlagName(token)
	flag := documentedCLIFlag(cmd, name)
	if flag == nil {
		return false, fmt.Errorf("unknown flag %q", token)
	}

	return flag.consumesValue, nil
}

func documentedCLIFlagName(token string) string {
	trimmed := strings.TrimLeft(token, "-")
	parts := strings.SplitN(trimmed, "=", 2)

	return parts[0]
}

func documentedCLIFlag(cmd *documentedCLICommandSpec, name string) *documentedCLIFlagSpec {
	for current := cmd; current != nil; current = current.Parent() {
		if flag, ok := current.flags[name]; ok {
			flagCopy := flag
			return &flagCopy
		}
	}

	return nil
}

func (c *documentedCLICommandSpec) Parent() *documentedCLICommandSpec {
	if c == nil {
		return nil
	}

	return c.parent
}

func (c *documentedCLICommandSpec) commandPath() string {
	if c == nil {
		return "onedrive-go"
	}

	parts := []string{c.name}
	for current := c.parent; current != nil; current = current.parent {
		if current.name == "" {
			continue
		}
		parts = append(parts, current.name)
	}
	slices.Reverse(parts)

	return strings.Join(parts, " ")
}

func documentedCLISubcommand(cmd *documentedCLICommandSpec, token string) *documentedCLICommandSpec {
	if cmd == nil {
		return nil
	}

	return cmd.subcommands[token]
}

func currentDocumentedCLIManifest() *documentedCLICommandSpec {
	return documentedCLIFromShape(clishape.Root(), nil)
}

func documentedCLIFromShape(
	spec clishape.CommandSpec,
	parent *documentedCLICommandSpec,
) *documentedCLICommandSpec {
	cmd := &documentedCLICommandSpec{
		name:        spec.Name,
		runnable:    spec.Runnable,
		parent:      parent,
		flags:       documentedCLIFlagsFromShape(spec.Flags),
		subcommands: map[string]*documentedCLICommandSpec{},
	}

	for i := range spec.Subcommands {
		child := documentedCLIFromShape(spec.Subcommands[i], cmd)
		cmd.subcommands[child.name] = child
	}

	return cmd
}

func documentedCLIFlagsFromShape(flags []clishape.FlagSpec) map[string]documentedCLIFlagSpec {
	if len(flags) == 0 {
		return nil
	}

	result := make(map[string]documentedCLIFlagSpec, len(flags))
	for _, flag := range flags {
		result[flag.Name] = documentedCLIFlagSpec{consumesValue: flag.ConsumesValue}
	}

	return result
}

func walkDocumentedCLIExamples(root string, resolver func(string) error) error {
	info, err := localpath.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat %s: %w", root, err)
	}

	if !info.IsDir() {
		return scanDocumentedCLIExamplesInFile(root, resolver)
	}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}

		return scanDocumentedCLIExamplesInFile(path, resolver)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}

	return nil
}

func scanDocumentedCLIExamplesInFile(path string, resolver func(string) error) error {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	for lineNumber, line := range lines {
		examples := documentedCLIExamplePattern.FindAllString(line, -1)
		for _, example := range examples {
			if err := resolver(strings.TrimSpace(example)); err != nil {
				return fmt.Errorf("invalid documented CLI example in %s:%d (%s): %w", path, lineNumber+1, example, err)
			}
		}
	}

	return nil
}

var (
	markdownLinkTargetPattern     = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)
	markdownExplicitAnchorPattern = regexp.MustCompile(`<a\s+id="([^"]+)"`)
)

type liveIncidentDocPromotion struct {
	incidentID   string
	isRecurring  bool
	promotedDocs string
}

func ensureRecurringIncidentPromotedDocsResolve(repoRoot string) error {
	liveIncidentsPath := filepath.Join(repoRoot, "spec", "reference", "live-incidents.md")
	data, err := localpath.ReadFile(liveIncidentsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read live incidents: %w", err)
	}

	promotions := parseLiveIncidentPromotions(string(data))
	for _, promotion := range promotions {
		if !promotion.isRecurring {
			continue
		}
		if promotion.promotedDocs == "" {
			return fmt.Errorf("recurring incident %s is missing Promoted docs", promotion.incidentID)
		}
		if strings.EqualFold(strings.TrimSpace(promotion.promotedDocs), "none") {
			return fmt.Errorf("recurring incident %s cannot use Promoted docs: none", promotion.incidentID)
		}

		targets := markdownLinkTargets(promotion.promotedDocs)
		if len(targets) == 0 {
			return fmt.Errorf("recurring incident %s has malformed Promoted docs links", promotion.incidentID)
		}
		for _, target := range targets {
			if err := validatePromotedDocLink(liveIncidentsPath, target); err != nil {
				return fmt.Errorf("recurring incident %s promoted doc %q: %w", promotion.incidentID, target, err)
			}
		}
	}

	return nil
}

func parseLiveIncidentPromotions(source string) []liveIncidentDocPromotion {
	lines := strings.Split(source, "\n")
	promotions := make([]liveIncidentDocPromotion, 0)
	var current *liveIncidentDocPromotion

	flush := func() {
		if current == nil || current.incidentID == "" {
			return
		}
		promotions = append(promotions, *current)
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## LI-") {
			flush()
			heading := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			incidentID, _, _ := strings.Cut(heading, ":")
			current = &liveIncidentDocPromotion{
				incidentID: strings.TrimSpace(incidentID),
			}
			continue
		}
		if current == nil {
			continue
		}

		if strings.HasPrefix(trimmed, "Recurring:") {
			current.isRecurring = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "Recurring:")), "yes")
			continue
		}
		if strings.HasPrefix(trimmed, "Promoted docs:") {
			current.promotedDocs = strings.TrimSpace(strings.TrimPrefix(trimmed, "Promoted docs:"))
		}
	}

	flush()
	return promotions
}

func markdownLinkTargets(source string) []string {
	matches := markdownLinkTargetPattern.FindAllStringSubmatch(source, -1)
	targets := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		targets = append(targets, strings.TrimSpace(match[1]))
	}

	return targets
}

func validatePromotedDocLink(basePath string, target string) error {
	pathPart, fragment, _ := strings.Cut(target, "#")
	resolvedPath := basePath
	if pathPart != "" {
		resolvedPath = filepath.Clean(filepath.Join(filepath.Dir(basePath), filepath.FromSlash(pathPart)))
	}

	if _, err := localpath.Stat(resolvedPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing target %s", resolvedPath)
		}

		return fmt.Errorf("stat target %s: %w", resolvedPath, err)
	}

	if fragment == "" {
		return nil
	}

	ok, err := markdownDocumentHasAnchor(resolvedPath, fragment)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("missing anchor #%s in %s", fragment, resolvedPath)
	}

	return nil
}

func markdownDocumentHasAnchor(path string, fragment string) (bool, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read target %s: %w", path, err)
	}

	anchors := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		for _, match := range markdownExplicitAnchorPattern.FindAllStringSubmatch(line, -1) {
			if len(match) >= 2 && match[1] != "" {
				anchors[match[1]] = struct{}{}
			}
		}

		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}

		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if heading == "" {
			continue
		}

		anchors[slugifyMarkdownHeading(heading)] = struct{}{}
	}

	_, ok := anchors[fragment]
	return ok, nil
}

func slugifyMarkdownHeading(heading string) string {
	var builder strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-':
			if builder.Len() == 0 || lastHyphen {
				continue
			}
			builder.WriteByte('-')
			lastHyphen = true
		}
	}

	return strings.Trim(builder.String(), "-")
}

func ensureSyncStoreMigrationDiscipline(repoRoot string) error {
	schemaOwnerPath := filepath.Join(repoRoot, "internal", "syncstore", "schema.go")
	if _, err := localpath.Stat(schemaOwnerPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat sync-store schema owner: %w", err)
	}

	legacySchemaPath := filepath.Join(repoRoot, "internal", "syncstore", "schema.sql")
	if _, err := localpath.Stat(legacySchemaPath); err == nil {
		return fmt.Errorf("legacy sync-store schema file detected: %s", legacySchemaPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat legacy sync-store schema file: %w", err)
	}

	initialMigrationPath := filepath.Join(repoRoot, "internal", "syncstore", "migrations", "00001_init.sql")
	if _, err := localpath.Stat(initialMigrationPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing initial sync-store goose migration: %s", initialMigrationPath)
		}
		return fmt.Errorf("stat initial sync-store goose migration: %w", err)
	}

	roots := []string{
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "reference"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "README.md"),
		filepath.Join(repoRoot, "CLAUDE.md"),
	}

	checks := []staleCheck{
		{name: "stale schema metadata table reference", pattern: regexp.MustCompile(`schema_` + `version`)},
		{name: "stale pragma schema version reference", pattern: regexp.MustCompile(`user_` + `version`)},
	}

	for _, check := range checks {
		match, err := findTextMatch(roots, check.pattern, nil)
		if err != nil {
			return err
		}
		if match != "" {
			return fmt.Errorf("stale sync-store schema version trace detected (%s): %s", check.name, match)
		}
	}

	return nil
}

func ensureNoForbiddenProductionPatterns(repoRoot string) error {
	goRoots := []string{repoRoot}
	match, err := findTextMatch(goRoots, regexp.MustCompile(`graph\.MustNewClient\(`), func(path string) bool {
		return strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go")
	})
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("production MustNewClient call detected: %s", match)
	}

	match, err = findTextMatch(goRoots, regexp.MustCompile(`internal/`+`trustedpath|trustedpath`+`\.`), func(path string) bool {
		return !strings.HasSuffix(path, ".go")
	})
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("trustedpath usage detected: %s", match)
	}

	return nil
}

func ensureNoResurrectedFiles(repoRoot string) error {
	checks := []struct {
		path string
		err  string
	}{
		{
			path: filepath.Join(repoRoot, "internal", "sync", "orchestrator.go"),
			err:  "control-plane files resurrected under internal/sync",
		},
		{
			path: filepath.Join(repoRoot, "internal", "sync", "drive_runner.go"),
			err:  "control-plane files resurrected under internal/sync",
		},
		{
			path: filepath.Join(repoRoot, "internal", "sync", "engine_flow_test_helpers_test.go"),
			err:  "sync test shim resurrected",
		},
	}

	for _, check := range checks {
		if _, err := localpath.Stat(check.path); err == nil {
			return errors.New(check.err)
		}
	}

	return nil
}

func ensureCLIOutputBoundary(repoRoot string) error {
	cliRoots := []string{filepath.Join(repoRoot, "internal", "cli")}
	skip := func(path string) bool {
		return strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go")
	}

	checks := []staleCheck{
		{
			name:    "direct fmt.Print in production CLI code",
			pattern: regexp.MustCompile(`fmt\.Print(f|ln)?\(`),
		},
		{
			name:    "direct process-global fmt.Fprint in production CLI code",
			pattern: regexp.MustCompile(`fmt\.Fprint(f|ln)?\(\s*os\.(Stdout|Stderr)\b`),
		},
		{
			name:    "direct process-global writer call in production CLI code",
			pattern: regexp.MustCompile(`os\.(Stdout|Stderr)\.(Write|WriteString)\(`),
		},
	}

	for _, check := range checks {
		match, err := findTextMatch(cliRoots, check.pattern, skip)
		if err != nil {
			return err
		}
		if match != "" {
			return fmt.Errorf("cli output boundary violation (%s): %s", check.name, match)
		}
	}

	return nil
}

func ensureGuardedPackagesAvoidRawOS(repoRoot string) error {
	guardedRoots := []string{
		filepath.Join(repoRoot, "internal", "cli"),
		filepath.Join(repoRoot, "internal", "config"),
		filepath.Join(repoRoot, "internal", "sync"),
		filepath.Join(repoRoot, "internal", "syncverify"),
	}
	skip := func(path string) bool {
		return strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go")
	}

	const guardedOSPattern = `os\.(Stat|ReadDir|Open|OpenFile|Create|CreateTemp|ReadFile|WriteFile|` +
		`Remove|RemoveAll|Rename|Mkdir|MkdirAll|Lstat|Readlink|Symlink|Chmod|Chtimes)\(`

	match, err := findTextMatch(guardedRoots, regexp.MustCompile(guardedOSPattern), skip)
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("raw os filesystem call detected outside boundary packages: %s", match)
	}

	return nil
}

func ensureFilterSemanticsWording(repoRoot string) error {
	roots := []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "CLAUDE.md"),
		filepath.Join(repoRoot, "README.md"),
	}

	match, err := findTextMatch(roots, regexp.MustCompile(`per-drive only`), nil)
	if err != nil {
		return err
	}
	if match != "" {
		return fmt.Errorf("stale filter semantics wording detected: %s", match)
	}

	return nil
}

func ensureGovernedDesignDocsHaveOwnershipContracts(repoRoot string) error {
	designDocs, err := filepath.Glob(filepath.Join(repoRoot, "spec", "design", "*.md"))
	if err != nil {
		return fmt.Errorf("glob design docs: %w", err)
	}

	for _, path := range designDocs {
		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}

		content := string(data)
		if !strings.Contains(content, "GOVERNS:") {
			continue
		}

		if !strings.Contains(content, "## Ownership Contract") {
			return fmt.Errorf("governed design doc missing Ownership Contract section: %s", path)
		}

		for _, bullet := range ownershipContractBullets() {
			if !strings.Contains(content, bullet) {
				return fmt.Errorf("governed design doc missing Ownership Contract bullet %q: %s", strings.TrimPrefix(bullet, "- "), path)
			}
		}
	}

	return nil
}

func ownershipContractBullets() []string {
	return []string{
		"- Owns:",
		"- Does Not Own:",
		"- Source of Truth:",
		"- Allowed Side Effects:",
		"- Mutable Runtime Owner:",
		"- Error Boundary:",
	}
}

func ensureCrossCuttingDesignDocs(repoRoot string) error {
	systemPath := filepath.Join(repoRoot, "spec", "design", "system.md")
	systemData, err := localpath.ReadFile(systemPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", systemPath, err)
	}

	systemText := string(systemData)
	for _, name := range requiredCrossCuttingDesignDocs() {
		path := filepath.Join(repoRoot, "spec", "design", name)
		if _, statErr := localpath.Stat(path); statErr != nil {
			return fmt.Errorf("required cross-cutting design doc missing: %s", path)
		}
		if !strings.Contains(systemText, name) {
			return fmt.Errorf("system.md missing required design doc reference %s: %s", name, systemPath)
		}
	}

	return nil
}

func requiredCrossCuttingDesignDocs() []string {
	return []string{"error-model.md", "threat-model.md", "degraded-mode.md"}
}

func ensureCrossCuttingDesignDocEvidence(repoRoot string) error {
	return ensureDocsContainSnippets("cross-cutting design doc", []docSnippetCheck{
		{
			path: filepath.Join(repoRoot, "spec", "design", "error-model.md"),
			snippets: []string{
				"## Verified By",
				"| Boundary | Evidence |",
			},
		},
		{
			path: filepath.Join(repoRoot, "spec", "design", "degraded-mode.md"),
			snippets: []string{
				"| ID |",
				"| Evidence |",
			},
		},
		{
			path: filepath.Join(repoRoot, "spec", "design", "threat-model.md"),
			snippets: []string{
				"## Mitigation Evidence",
				"| Mitigation | Evidence |",
			},
		},
	})
}

func ensureGovernedBehaviorDocsHaveEvidence(repoRoot string) error {
	checks := governedBehaviorEvidenceDocs(repoRoot)
	snippetChecks := make([]docSnippetCheck, 0, len(checks))
	for _, check := range checks {
		snippetChecks = append(snippetChecks, docSnippetCheck{
			path: check.path,
			snippets: []string{
				check.heading,
				"| Behavior | Evidence |",
			},
		})
	}

	return ensureDocsContainSnippets("governed design doc", snippetChecks)
}

type docSnippetCheck struct {
	path     string
	snippets []string
}

func ensureDocsContainSnippets(docKind string, checks []docSnippetCheck) error {
	for _, check := range checks {
		data, err := localpath.ReadFile(check.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", check.path, err)
		}
		content := string(data)
		for _, snippet := range check.snippets {
			if !strings.Contains(content, snippet) {
				return fmt.Errorf("%s missing required evidence snippet %q: %s", docKind, snippet, check.path)
			}
		}
	}

	return nil
}

var (
	requirementDeclarationPattern = regexp.MustCompile(`^(?:#+|- )\s*(R-\d+(?:\.\d+)*)\b`)
	requirementIDPattern          = regexp.MustCompile(`^R-\d+(?:\.\d+)*$`)
	implementsEntryPattern        = regexp.MustCompile(`^(R-\d+(?:\.\d+)*) \[[^][]+\]$`)
	testFunctionPattern           = regexp.MustCompile(`\bTest[A-Z0-9_][A-Za-z0-9_]*\b`)
)

func ensureRequirementReferencesResolve(repoRoot string) error {
	registry, err := loadRequirementRegistry(repoRoot)
	if err != nil {
		return err
	}

	var problems []string

	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		switch {
		case strings.HasSuffix(path, "_test.go"):
			fileProblems, validateErr := validateRequirementReferencesInTestFile(path, registry)
			if validateErr != nil {
				return validateErr
			}
			problems = append(problems, fileProblems...)
		case strings.HasPrefix(path, filepath.Join(repoRoot, "spec", "design")+string(filepath.Separator)) &&
			strings.HasSuffix(path, ".md"):
			fileProblems, validateErr := validateRequirementReferencesInDesignDoc(path, registry)
			if validateErr != nil {
				return validateErr
			}
			problems = append(problems, fileProblems...)
		}

		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk requirement references: %w", walkErr)
	}

	if len(problems) > 0 {
		return fmt.Errorf("requirement reference validation failed:\n%s", strings.Join(problems, "\n"))
	}

	return nil
}

func loadRequirementRegistry(repoRoot string) (map[string]struct{}, error) {
	requirementFiles, err := filepath.Glob(filepath.Join(repoRoot, "spec", "requirements", "*.md"))
	if err != nil {
		return nil, fmt.Errorf("glob requirement files: %w", err)
	}

	registry := make(map[string]struct{})
	for _, path := range requirementFiles {
		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}

		for _, line := range strings.Split(string(data), "\n") {
			matches := requirementDeclarationPattern.FindStringSubmatch(strings.TrimSpace(line))
			if len(matches) == 2 {
				registry[matches[1]] = struct{}{}
			}
		}
	}

	if len(registry) == 0 {
		return nil, fmt.Errorf("load requirement registry: no declared requirement IDs found")
	}

	return registry, nil
}

func validateRequirementReferencesInTestFile(path string, registry map[string]struct{}) ([]string, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var problems []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			trimmed := strings.TrimSpace(comment.Text)
			if !strings.HasPrefix(trimmed, "// Validates:") {
				continue
			}

			ids, parseErr := parseValidatesLine(strings.TrimSpace(strings.TrimPrefix(trimmed, "// Validates:")))
			lineNumber := fset.Position(comment.Slash).Line
			if parseErr != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %v", path, lineNumber, parseErr))
				continue
			}

			for _, id := range ids {
				if _, ok := registry[id]; !ok {
					problems = append(problems, fmt.Sprintf("%s:%d: unknown requirement ID %s", path, lineNumber, id))
				}
			}
		}
	}

	return problems, nil
}

func parseValidatesLine(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty Validates reference list")
	}

	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty Validates reference entry")
		}
		if !requirementIDPattern.MatchString(part) {
			return nil, fmt.Errorf("malformed Validates reference %q", part)
		}
		ids = append(ids, part)
	}

	return ids, nil
}

func validateRequirementReferencesInDesignDoc(path string, registry map[string]struct{}) ([]string, error) {
	return validateRequirementReferencesInFile(path, "Implements:", registry, parseImplementsLine)
}

func validateRequirementReferencesInFile(
	path string,
	marker string,
	registry map[string]struct{},
	parse func(string) ([]string, error),
) ([]string, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var problems []string
	for lineNumber, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, marker) {
			continue
		}

		ids, parseErr := parse(strings.TrimSpace(strings.TrimPrefix(trimmed, marker)))
		if parseErr != nil {
			problems = append(problems, fmt.Sprintf("%s:%d: %v", path, lineNumber+1, parseErr))
			continue
		}

		for _, id := range ids {
			if _, ok := registry[id]; !ok {
				problems = append(problems, fmt.Sprintf("%s:%d: unknown requirement ID %s", path, lineNumber+1, id))
			}
		}
	}

	return problems, nil
}

func parseImplementsLine(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty Implements reference list")
	}

	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty Implements reference entry")
		}

		matches := implementsEntryPattern.FindStringSubmatch(part)
		if len(matches) != 2 {
			return nil, fmt.Errorf("malformed Implements reference %q", part)
		}
		ids = append(ids, matches[1])
	}

	return ids, nil
}

type evidenceDocCheck struct {
	path    string
	heading string
}

func governedBehaviorEvidenceDocs(repoRoot string) []evidenceDocCheck {
	return []evidenceDocCheck{
		{path: filepath.Join(repoRoot, "spec", "design", "sync-engine.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-execution.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "cli.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-control-plane.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-store.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-observation.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "config.md"), heading: "## Verified By"},
	}
}

func ensureEvidenceDocsReferenceRealTests(repoRoot string) error {
	testRegistry, err := loadTestRegistry(repoRoot)
	if err != nil {
		return err
	}

	checks := []evidenceDocCheck{
		{path: filepath.Join(repoRoot, "spec", "design", "error-model.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "threat-model.md"), heading: "## Mitigation Evidence"},
		{path: filepath.Join(repoRoot, "spec", "design", "degraded-mode.md")},
	}
	checks = append(checks, governedBehaviorEvidenceDocs(repoRoot)...)

	var problems []string
	for _, check := range checks {
		docProblems, docErr := validateEvidenceDocReferences(check, testRegistry)
		if docErr != nil {
			return docErr
		}
		problems = append(problems, docProblems...)
	}

	if len(problems) > 0 {
		return fmt.Errorf("evidence doc validation failed:\n%s", strings.Join(problems, "\n"))
	}

	return nil
}

func loadTestRegistry(repoRoot string) (map[string]struct{}, error) {
	registry := make(map[string]struct{})

	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, data, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "Test") {
				registry[fn.Name.Name] = struct{}{}
			}
		}

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk test registry: %w", walkErr)
	}

	if len(registry) == 0 {
		return nil, fmt.Errorf("load test registry: no test functions found")
	}

	return registry, nil
}

func validateEvidenceDocReferences(check evidenceDocCheck, testRegistry map[string]struct{}) ([]string, error) {
	data, err := localpath.ReadFile(check.path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", check.path, err)
	}

	content := string(data)
	section := content
	if check.heading != "" {
		section, err = markdownSection(content, check.heading)
		if err != nil {
			return []string{fmt.Sprintf("%s: %v", check.path, err)}, nil
		}
	}

	matches := testFunctionPattern.FindAllString(section, -1)
	if len(matches) == 0 {
		return []string{fmt.Sprintf("%s: no exact test names found in evidence section", check.path)}, nil
	}

	var problems []string
	seen := make(map[string]struct{})
	for _, name := range matches {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := testRegistry[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown test function %s", check.path, name))
		}
	}

	return problems, nil
}

func markdownSection(content, heading string) (string, error) {
	start := strings.Index(content, heading)
	if start == -1 {
		return "", fmt.Errorf("missing section %q", heading)
	}

	remaining := content[start:]
	nextOffset := strings.Index(remaining[len(heading):], "\n## ")
	if nextOffset == -1 {
		return remaining, nil
	}

	return remaining[:len(heading)+nextOffset], nil
}

func ensureHTTPClientDoStaysAtApprovedBoundary(repoRoot string) error {
	allowedPath := filepath.Join(repoRoot, "internal", "graph", "client_preauth.go")
	internalRoot := filepath.Join(repoRoot, "internal")

	walkErr := filepath.WalkDir(internalRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == allowedPath || strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
			return nil
		}

		match, findErr := findHTTPClientDoCall(path)
		if findErr != nil {
			return findErr
		}
		if match != "" {
			return matchFoundError{value: match}
		}

		return nil
	})
	if walkErr == nil {
		return nil
	}

	var matchErr matchFoundError
	if errors.As(walkErr, &matchErr) {
		return fmt.Errorf("http.Client.Do is only allowed in internal/graph/client_preauth.go: %s", matchErr.value)
	}

	return fmt.Errorf("walk %s: %w", internalRoot, walkErr)
}

type packageSelectorBoundaryRule struct {
	importPath  string
	selector    string
	description string
	allowed     func(string) bool
	roots       []string
}

func ensurePrivilegedPackageCallsStayAtApprovedBoundaries(repoRoot string) error {
	rules := []packageSelectorBoundaryRule{
		{
			importPath:  "os/exec",
			selector:    "Command",
			description: "exec.Command is forbidden in production code",
			allowed: func(string) bool {
				return false
			},
		},
		{
			importPath:  "os/exec",
			selector:    "CommandContext",
			description: "exec.CommandContext is only allowed in internal/cli/auth.go and internal/devtool/runner.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "auth.go") ||
					path == filepath.Join(repoRoot, "internal", "devtool", "runner.go")
			},
		},
		{
			importPath:  "database/sql",
			selector:    "Open",
			description: "sql.Open is only allowed in internal/syncstore/store.go and internal/syncstore/inspector.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "syncstore", "store.go") ||
					path == filepath.Join(repoRoot, "internal", "syncstore", "inspector.go")
			},
		},
		{
			importPath:  "os/signal",
			selector:    "Notify",
			description: "signal.Notify is only allowed in internal/cli/signal.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "signal.go")
			},
		},
		{
			importPath:  "os/signal",
			selector:    "Stop",
			description: "signal.Stop is only allowed in internal/cli/signal.go and internal/cli/sync_runtime.go",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "internal", "cli", "signal.go") ||
					path == filepath.Join(repoRoot, "internal", "cli", "sync_runtime.go")
			},
		},
		{
			importPath:  "os",
			selector:    "Exit",
			description: "os.Exit is only allowed in production entrypoints",
			allowed: func(path string) bool {
				return path == filepath.Join(repoRoot, "main.go") ||
					path == filepath.Join(repoRoot, "cmd", "devtool", "main.go") ||
					path == filepath.Join(repoRoot, "internal", "cli", "signal.go")
			},
			roots: []string{
				filepath.Join(repoRoot, "main.go"),
				filepath.Join(repoRoot, "internal"),
				filepath.Join(repoRoot, "cmd"),
			},
		},
	}

	for _, rule := range rules {
		if err := ensurePackageSelectorStaysAtApprovedBoundary(repoRoot, rule); err != nil {
			return err
		}
	}

	return nil
}

func ensurePackageSelectorStaysAtApprovedBoundary(repoRoot string, rule packageSelectorBoundaryRule) error {
	roots := rule.roots
	if len(roots) == 0 {
		roots = []string{
			filepath.Join(repoRoot, "internal"),
			filepath.Join(repoRoot, "cmd"),
		}
	}

	for _, root := range roots {
		if err := scanPackageSelectorBoundaryRoot(root, rule); err != nil {
			var matchErr matchFoundError
			if errors.As(err, &matchErr) {
				return fmt.Errorf("%s: %s", rule.description, matchErr.value)
			}

			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return err
		}
	}

	return nil
}

func scanPackageSelectorBoundaryRoot(root string, rule packageSelectorBoundaryRule) error {
	info, statErr := localpath.Stat(root)
	if statErr != nil {
		return fmt.Errorf("stat %s: %w", root, statErr)
	}
	if !info.IsDir() {
		return scanPackageSelectorFile(root, rule)
	}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		return scanPackageSelectorFile(path, rule)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}

	return nil
}

func scanPackageSelectorFile(path string, rule packageSelectorBoundaryRule) error {
	if rule.allowed(path) || strings.HasSuffix(path, "_test.go") || !strings.HasSuffix(path, ".go") {
		return nil
	}

	match, findErr := findPackageSelectorCall(path, rule.importPath, rule.selector)
	if findErr != nil {
		return findErr
	}
	if match != "" {
		return matchFoundError{value: match}
	}

	return nil
}

func findHTTPClientDoCall(path string) (string, error) {
	data, readErr := localpath.ReadFile(path)
	if readErr != nil {
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if !strings.Contains(string(data), ".Do(") {
		return "", nil
	}

	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, data, 0)
	if parseErr != nil {
		return "", fmt.Errorf("parse %s: %w", path, parseErr)
	}
	if !importsPackage(file, "net/http") {
		return "", nil
	}

	return firstHTTPDoCallLocation(path, file, fset), nil
}

func findPackageSelectorCall(path string, importPath, selector string) (string, error) {
	data, readErr := localpath.ReadFile(path)
	if readErr != nil {
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if !strings.Contains(string(data), "."+selector+"(") {
		return "", nil
	}

	fset := token.NewFileSet()
	file, parseErr := parser.ParseFile(fset, path, data, 0)
	if parseErr != nil {
		return "", fmt.Errorf("parse %s: %w", path, parseErr)
	}

	aliases := importedNamesForPath(file, importPath)
	if len(aliases) == 0 {
		return "", nil
	}

	var match string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != selector {
			return true
		}

		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, ok := aliases[x.Name]; !ok {
			return true
		}

		match = fmt.Sprintf("%s:%d", path, fset.Position(sel.Pos()).Line)
		return false
	})

	return match, nil
}

func firstHTTPDoCallLocation(path string, file *ast.File, fset *token.FileSet) string {
	var match string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Do" {
			return true
		}

		match = fmt.Sprintf("%s:%d", path, fset.Position(sel.Pos()).Line)
		return false
	})

	return match
}

func importsPackage(file *ast.File, target string) bool {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, "\"") == target {
			return true
		}
	}

	return false
}

func importedNamesForPath(file *ast.File, target string) map[string]struct{} {
	names := make(map[string]struct{})

	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, "\"") != target {
			continue
		}

		name := filepath.Base(target)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}

		names[name] = struct{}{}
	}

	return names
}

func findTextMatch(roots []string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	for _, root := range roots {
		match, err := findTextMatchInRoot(root, pattern, skip)
		if err != nil {
			return "", err
		}
		if match != "" {
			return match, nil
		}
	}

	return "", nil
}

func findTextMatchInRoot(root string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	info, err := localpath.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("stat %s: %w", root, err)
	}

	if !info.IsDir() {
		return scanPathForPattern(root, pattern, skip)
	}

	return walkRootForPattern(root, pattern, skip)
}

func walkRootForPattern(root string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		match, scanErr := scanPathForPattern(path, pattern, skip)
		if scanErr != nil {
			return scanErr
		}
		if match != "" {
			return matchFoundError{value: match}
		}

		return nil
	})
	if walkErr == nil {
		return "", nil
	}

	var matchErr matchFoundError
	if errors.As(walkErr, &matchErr) {
		return matchErr.value, nil
	}

	return "", fmt.Errorf("walk %s: %w", root, walkErr)
}

func scanPathForPattern(path string, pattern *regexp.Regexp, skip func(path string) bool) (string, error) {
	if skip != nil && skip(path) {
		return "", nil
	}

	return scanFileForPattern(path, pattern)
}

type matchFoundError struct {
	value string
}

func (f matchFoundError) Error() string {
	return f.value
}

func scanFileForPattern(path string, pattern *regexp.Regexp) (string, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if pattern.MatchString(line) {
			return fmt.Sprintf("%s:%d", path, i+1), nil
		}
	}

	return "", nil
}

func writeStatus(w io.Writer, text string) error {
	_, err := io.WriteString(w, text)
	if err != nil {
		return fmt.Errorf("write status output: %w", err)
	}

	return nil
}
