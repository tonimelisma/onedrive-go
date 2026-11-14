package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func fullE2EParallelMiscBucket() fullE2EBucketSpec {
	return fullE2EBucketSpec{
		Name:      "full-parallel-misc",
		Kind:      fullE2EBucketKindParallelMisc,
		TestNames: fullE2EParallelMiscTestNames(),
		Parallel:  fullE2EParallelMiscParallel,
		Timeout:   fullE2EParallelMiscTimeout,
	}
}

func fullE2ESerialSyncBucket() fullE2EBucketSpec {
	return fullE2EBucketSpec{
		Name:      "full-serial-sync",
		Kind:      fullE2EBucketKindSerialSync,
		TestNames: fullE2ESerialSyncTestNames(),
		Parallel:  fullE2ESerialParallel,
		Timeout:   fullE2ESerialSyncTimeout,
	}
}

func fullE2ESerialWatchSharedBucket() fullE2EBucketSpec {
	return fullE2EBucketSpec{
		Name:      "full-serial-watch-shared",
		Kind:      fullE2EBucketKindSerialWatchShared,
		TestNames: fullE2ESerialWatchSharedTestNames(),
		Parallel:  fullE2ESerialParallel,
		Timeout:   fullE2ESerialWatchSharedTimeout,
	}
}

func fullE2EBuckets() []fullE2EBucketSpec {
	return []fullE2EBucketSpec{
		fullE2EParallelMiscBucket(),
		fullE2ESerialSyncBucket(),
		fullE2ESerialWatchSharedBucket(),
	}
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
		"TestE2E_Status_LiveOverlay_ConfigTolerance",
		"TestE2E_Status_PersonalAccountShowsSinglePersonalLiveDrive",
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
		"TestE2E_Status_AfterSync",
		"TestE2E_Status_JSON",
		"TestE2E_Status_PausedDrive",
		"TestE2E_Pause_WithDuration",
		"TestE2E_Pause_IndefiniteAndResume",
		"TestE2E_Resume_NotPaused",
		"TestE2E_Resume_AllDrives",
		"TestE2E_Status_PerDrive_NoIssuesOrRetries",
		"TestE2E_Status_JSON_IssueDetails",
		"TestE2E_CLI_NoResolveCommand",
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
		"TestE2E_EdgeCases",
		"TestE2E_Sync_BidirectionalMerge",
		"TestE2E_Sync_EditDeleteConflict",
		"TestE2E_Sync_DirectionalModes_PreserveEditEditConflict",
		"TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict",
		"TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict",
		"TestE2E_Sync_DeletePropagation",
		"TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges",
		"TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges",
		"TestE2E_Sync_NestedFolderHierarchy",
		"TestE2E_Sync_DryRunNonDestructive",
		"TestE2E_Sync_ConvergentEdit",
		"TestE2E_Sync_InternalBaselineVerificationDetectsTampering",
		"TestE2E_Sync_EmptyDirectory",
		"TestE2E_Sync_NestedDeletion",
		"TestE2E_Sync_IdempotentReSync",
		"TestE2E_Sync_CrashRecoveryIdempotent",
		"TestE2E_Sync_FolderMovesBothWays",
		"TestE2E_Sync_CrossMoves",
		"TestE2E_Sync_NamingCorpusRoundtrip",
		"TestE2E_Sync_TwoWriters",
		"TestE2E_Sync_LargeFileBetweenClients",
		"TestE2E_Sync_TypeChangesBothWays",
		"TestE2E_Sync_ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta",
		"TestE2E_SyncFull_DeleteHoldKeepsOneDriveUntilConfirmed",
		"TestE2E_Sync_KillMidUploadResumesSession",
		"TestE2E_Sync_KillMidDownloadResumesPartial",
		"TestE2E_SyncFull_RemoteDeleteGoesToLocalTrashAndRestores",
		"TestE2E_SyncFull_ReplacedSyncFolderIsRefusedUntilRebound",
	}
}

func fullE2ESerialWatchSharedTestNames() []string {
	return []string{
		"TestE2E_SyncWatch_WebsocketStartupSmoke",
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
		"TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount",
		"TestE2E_Shortcut_ExplicitStandaloneSharedFolderRemainsConfiguredDrive",
		"TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible",
		"TestE2E_Shortcut_RenameReusesChildMountState",
		"TestE2E_Shortcut_MoveReusesChildMountState",
		"TestE2E_Shortcut_WritableUploadSyncsToSharedTarget",
		"TestE2E_Shortcut_ReadOnlyBlockedUploadStatus",
		"TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes",
		"TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget",
		"TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot",
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

func fullE2EBucketJSONCommandArgs(bucket fullE2EBucketSpec) []string {
	args := fullE2EBucketCommandArgs(bucket)
	return append([]string{args[0], "-json"}, args[1:]...)
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
		if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	data, err := readFile(summaryPath)
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

// runE2EFullCompileCheck compiles the full-tagged E2E package without running
// it. The live suite owns a TestMain, so `go test -run=^$` would still execute
// side effects instead of acting as a pure compile guard.
func runE2EFullCompileCheck(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
) (runErr error) {
	artifactFile, err := os.CreateTemp(
		os.TempDir(),
		strings.TrimSuffix(fullE2ECompileArtifactName, ".test")+"-*.test",
	)
	if err != nil {
		return fmt.Errorf("create full e2e compile artifact: %w", err)
	}
	artifactPath := artifactFile.Name()
	if err := artifactFile.Close(); err != nil {
		if removeErr := remove(artifactPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("close full e2e compile artifact: %w; cleanup error: %s", err, removeErr.Error())
		}
		return fmt.Errorf("close full e2e compile artifact: %w", err)
	}
	if err := remove(artifactPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove reserved full e2e compile artifact: %w", err)
	}
	defer func() {
		if err := remove(artifactPath); err != nil && !errors.Is(err, os.ErrNotExist) && runErr == nil {
			runErr = fmt.Errorf("remove full e2e compile artifact: %w", err)
		}
	}()

	if err := writeStatus(stdout, "==> go test -c -race -tags='e2e e2e_full' ./e2e\n"); err != nil {
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
		"-c",
		"-race",
		"-tags=e2e e2e_full",
		"-o",
		artifactPath,
		"./e2e",
	); err != nil {
		return fmt.Errorf("full e2e compile: %w", err)
	}

	return runTaggedVet(ctx, runner, repoRoot, env, stdout, stderr)
}

// taggedVetTargetCount is the number of build-tagged source sets vetted. It is
// a constant so callers can size against it without the array-length pitfall
// where len(f()) never calls f.
const taggedVetTargetCount = 2

// taggedVetTargets are the build-tagged source sets that a plain `go vet ./...`
// never sees. Compiling them proves they build; it does not run the analyzers,
// so vet findings in live-test code could only surface on a live run -- the
// slowest and least available lane there is.
func taggedVetTargets() [taggedVetTargetCount][2]string {
	return [taggedVetTargetCount][2]string{
		{"-tags=e2e e2e_full", "./e2e"},
		{"-tags=" + integrationBuildTag, "./internal/graph"},
	}
}

func runTaggedVet(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
) error {
	for _, target := range taggedVetTargets() {
		tag, pkg := target[0], target[1]
		if err := writeStatus(stdout, "==> go vet "+tag+" "+pkg+"\n"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		if err := runner.Run(ctx, repoRoot, env, stdout, stderr, "go", "vet", tag, pkg); err != nil {
			return fmt.Errorf("vet %s %s: %w", tag, pkg, err)
		}
	}

	return nil
}

// errE2EPreflight marks a live-lane preflight failure: the account or the
// fixtures are unusable, so nothing after the preflight can verify anything.
var errE2EPreflight = errors.New("e2e preflight failed")

// authPreflightSkippedNotice is the one line printed when the auth preflight
// skipped: Microsoft reported a read-only window, and the lane is untestable.
// It names no drive, because the subtest names do.
const authPreflightSkippedNotice = "verify: auth preflight skipped: Microsoft reported a read-only window " +
	"(LI-20260823-01), so the live lane is untestable in this run"

// goTestSkipPrefix starts go test's line for a skipped test.
const goTestSkipPrefix = "--- SKIP: "

func authE2EPreflightArgs() []string {
	return []string{
		"test", "-tags=e2e", "-run=" + authE2EPreflightPattern, "-count=1", "-v",
		"-timeout=" + e2ePreflightTimeout, "./e2e/...",
	}
}

func fastE2EPreflightArgs() []string {
	return []string{
		"test", "-tags=e2e", "-run=" + fastE2EPreflightPattern, "-count=1", "-v",
		"-timeout=" + e2ePreflightTimeout, "./e2e/...",
	}
}

func fullE2EPreflightArgs() []string {
	return []string{
		"test", "-tags=e2e e2e_full", "-run=" + fullE2EPreflightPattern, "-count=1", "-v",
		"-timeout=" + e2ePreflightTimeout, "./e2e/...",
	}
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
	return collector.runStepWithOutcome("fast e2e", func() (stepOutcome, error) {
		var output string
		var err error
		if classifyLiveQuirks {
			output, err = runFastE2ESuiteWithClassification(ctx, runner, repoRoot, env, collector, stdout, stderr)
		} else {
			output, err = runFastE2ESuiteOnce(ctx, runner, repoRoot, env, stdout, stderr)
		}

		return stepOutcome{failedTests: parseGoTestLog(output).failedTopLevelTests()}, err
	})
}

func runFastE2ESuiteOnce(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	stdout, stderr io.Writer,
) (string, error) {
	if err := writeStatus(stdout, "==> go test -tags=e2e\n"); err != nil {
		return "", fmt.Errorf("write status: %w", err)
	}

	return runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "fast e2e", fastE2EArgs()...)
}

func runFastE2ESuiteWithClassification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) (string, error) {
	if err := writeStatus(stdout, "==> go test -json -tags=e2e\n"); err != nil {
		return "", fmt.Errorf("write status: %w", err)
	}

	output, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "fast e2e", fastE2EJSONArgs()...)
	if err == nil {
		return output, nil
	}

	rerunArgs, incidentID, ok := classifyFastE2EQuirk(parseGoTestLog(output).classifiableFailures())
	if !ok {
		return output, err
	}

	// The first attempt's output names what failed; the rerun covers one test.
	_, err = rerunClassifiedQuirk(ctx, runner, repoRoot, env, collector, stdout, stderr, classifiedQuirkRerun{
		incidentID: incidentID,
		phase:      "fast e2e",
		trigger:    fastDownloadTestName,
		args:       rerunArgs,
		firstErr:   err,
	})

	return output, err
}

// classifiedQuirkRerun is one rerun of a documented live-provider quirk.
type classifiedQuirkRerun struct {
	incidentID string
	phase      string
	trigger    string
	args       []string
	// firstErr is the failure the rerun answers.
	firstErr error
}

// rerunClassifiedQuirk reruns a known quirk once, records the rerun and
// returns its output. A passing rerun downgrades the first failure to a
// warning; a failing one returns its own error, which carries the first
// attempt's too.
func rerunClassifiedQuirk(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	rerun classifiedQuirkRerun,
) (string, error) {
	if err := writeStatus(stdout, fmt.Sprintf("==> rerun known live quirk %s\n", rerun.incidentID)); err != nil {
		return "", fmt.Errorf("write status: %w", err)
	}

	startedAt := time.Now()
	output, rerunErr := runGoTestMatching(
		ctx, runner, repoRoot, env, stdout, stderr, rerun.phase+" rerun of "+rerun.incidentID, rerun.args...,
	)
	status := verifySummaryStatusPass
	if rerunErr != nil {
		status = verifySummaryStatusFail
	}
	collector.recordClassifiedRerun(rerun.incidentID, rerun.phase, rerun.trigger, rerun.args, time.Since(startedAt), status)
	if rerunErr != nil {
		return output, fmt.Errorf("%w (first attempt: %w)", rerunErr, rerun.firstErr)
	}

	warning := fmt.Sprintf("warning: classified known live quirk %s after successful rerun\n", rerun.incidentID)
	if err := writeStatus(stdout, warning); err != nil {
		return output, fmt.Errorf("write status: %w", err)
	}

	return output, nil
}

// runE2EPreflightAuth runs the auth preflight. Under --classify-live-quirks a
// failure is rerun once for LI-20260405-06, but only when it is that quirk's
// shape, the preflight test itself failing by its own verdict: a build
// failure, a pattern that matched nothing or a test killed at the deadline is
// not rerun.
func runE2EPreflightAuth(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	classifyLiveQuirks bool,
) error {
	return collector.runStepWithOutcome("auth preflight", func() (stepOutcome, error) {
		if err := writeStatus(stdout, "==> go test -tags=e2e auth preflight\n"); err != nil {
			return stepOutcome{}, fmt.Errorf("write status: %w", err)
		}

		args := authE2EPreflightArgs()
		output, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "fast e2e auth preflight", args...)
		if err != nil && classifyLiveQuirks && isAuthPreflightQuirk(parseGoTestLog(output)) {
			// The rerun's output decides from here on: it is the attempt whose
			// result counts, skips included.
			output, err = rerunClassifiedQuirk(ctx, runner, repoRoot, env, collector, stdout, stderr, classifiedQuirkRerun{
				incidentID: authPreflightIncidentID,
				phase:      "auth preflight",
				trigger:    authE2EPreflightPattern,
				args:       args,
				firstErr:   err,
			})
		}
		if err != nil {
			return stepOutcome{failedTests: parseGoTestLog(output).failedTopLevelTests()}, fmt.Errorf("%w: %w", errE2EPreflight, err)
		}
		if !authPreflightSkipped(output) {
			return stepOutcome{}, nil
		}

		if err := writeStatus(stdout, authPreflightSkippedNotice+"\n"); err != nil {
			return stepOutcome{}, fmt.Errorf("write status: %w", err)
		}

		return stepOutcome{skipped: true}, nil
	})
}

// isAuthPreflightQuirk reports whether a failed auth preflight has the shape
// LI-20260405-06 documents: the preflight test, and only it, failed by its own
// verdict.
func isAuthPreflightQuirk(log goTestLog) bool {
	failed := log.classifiableFailures()
	if len(failed) == 0 {
		return false
	}
	for name := range failed {
		if top, _, _ := strings.Cut(name, "/"); top != authPreflightTestName {
			return false
		}
	}

	return true
}

// authPreflightSkipped reports whether go test output shows the auth
// preflight, or one of its per-drive subtests, skipped.
func authPreflightSkipped(output string) bool {
	for line := range strings.Lines(output) {
		text, _, _ := decodeGoTestLogLine(line)
		rest, ok := strings.CutPrefix(strings.TrimSpace(text), goTestSkipPrefix)
		if !ok {
			continue
		}

		name, _, _ := strings.Cut(rest, " ")
		if top, _, _ := strings.Cut(name, "/"); top == authPreflightTestName {
			return true
		}
	}

	return false
}

func runE2EPreflightFast(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return runE2EPreflight(ctx, runner, repoRoot, env, collector, stdout, stderr, e2ePreflightSpec{
		stepName:    "fast fixture preflight",
		status:      "==> go test -tags=e2e fixture preflight\n",
		description: "fast e2e fixture preflight",
		args:        fastE2EPreflightArgs(),
	})
}

// e2ePreflightSpec is one fixture preflight step.
type e2ePreflightSpec struct {
	stepName    string
	status      string
	description string
	args        []string
}

// runE2EPreflight runs one fixture preflight; its failure wraps
// errE2EPreflight, which stops the live lane.
func runE2EPreflight(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
	spec e2ePreflightSpec,
) error {
	return collector.runStepWithOutcome(spec.stepName, func() (stepOutcome, error) {
		if err := writeStatus(stdout, spec.status); err != nil {
			return stepOutcome{}, fmt.Errorf("write status: %w", err)
		}

		output, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, spec.description, spec.args...)
		if err != nil {
			return stepOutcome{failedTests: parseGoTestLog(output).failedTopLevelTests()}, fmt.Errorf("%w: %w", errE2EPreflight, err)
		}

		return stepOutcome{}, nil
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
	classifyLiveQuirks bool,
) error {
	if logDir == "" {
		logDir = filepath.Join(os.TempDir(), "e2e-debug-logs")
	}

	fullEnv := append([]string{}, env...)
	fullEnv = append(fullEnv, "E2E_LOG_DIR="+logDir)

	buckets := fullE2EBuckets()
	if err := runE2EPreflightFull(ctx, runner, repoRoot, fullEnv, collector, stdout, stderr); err != nil {
		collector.recordBucketsNotRun(buckets, "full fixture preflight failed")
		return err
	}

	bucketEnv := append([]string{}, fullEnv...)
	bucketEnv = append(bucketEnv, e2eSkipSuiteScrubEnvVar+"=1")

	// Every bucket runs whatever an earlier one returned: a failure in one
	// says nothing about the others, and stopping would hide their state for
	// the night (CLAUDE.md: collect, report, continue).
	var bucketErrs []error
	notRun := 0
	for _, bucket := range buckets {
		if ctx.Err() != nil {
			collector.recordBucketsNotRun([]fullE2EBucketSpec{bucket}, "run canceled")
			notRun++
			continue
		}

		if err := collector.runBucket(bucket, func() ([]string, error) {
			if classifyLiveQuirks && bucketSupportsClassifiedRerun(bucket) {
				return runFullE2EBucketWithClassification(ctx, runner, repoRoot, bucketEnv, collector, bucket, stdout, stderr)
			}

			return runFullE2EBucket(ctx, runner, repoRoot, bucketEnv, bucket, stdout, stderr)
		}); err != nil {
			bucketErrs = append(bucketErrs, fmt.Errorf("bucket %s: %w", bucket.Name, err))
		}
	}
	if notRun > 0 {
		bucketErrs = append(bucketErrs, fmt.Errorf("full e2e: %d buckets not run: %w", notRun, ctx.Err()))
	}

	return errors.Join(bucketErrs...)
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

func bucketSupportsClassifiedRerun(bucket fullE2EBucketSpec) bool {
	return bucket.Kind == fullE2EBucketKindSerialWatchShared
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
		"-timeout=" + classifiedRerunTimeout,
		"./e2e/...",
	}, fastDownloadIncidentID, true
}

func classifyFullE2EQuirk(bucket fullE2EBucketSpec, failedTests map[string]struct{}) ([]string, string, string, bool) {
	if bucket.Kind != fullE2EBucketKindSerialWatchShared {
		return nil, "", "", false
	}
	if len(failedTests) != 1 {
		return nil, "", "", false
	}
	if _, ok := failedTests[websocketRestartTestName]; !ok {
		return nil, "", "", false
	}

	return []string{
		"test",
		"-tags=e2e e2e_full",
		"-race",
		"-run=^" + websocketRestartTestName + "$",
		"-count=1",
		"-v",
		"-parallel",
		strconv.Itoa(fullE2ESerialParallel),
		"-timeout=" + classifiedRerunTimeout,
		"./e2e/...",
	}, websocketRestartIncidentID, websocketRestartTestName, true
}

func runFullE2EBucket(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	bucket fullE2EBucketSpec,
	stdout, stderr io.Writer,
) ([]string, error) {
	if err := writeStatus(stdout, fmt.Sprintf("==> go test -tags='e2e e2e_full' %s\n", bucket.Name)); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	output, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "full e2e", fullE2EBucketCommandArgs(bucket)...)

	return parseGoTestLog(output).failedTopLevelTests(), err
}

func runFullE2EBucketWithClassification(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	bucket fullE2EBucketSpec,
	stdout, stderr io.Writer,
) ([]string, error) {
	if err := writeStatus(stdout, fmt.Sprintf("==> go test -json -tags='e2e e2e_full' %s\n", bucket.Name)); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Streamed, not captured until exit: this is the nightly's watch bucket,
	// and a job killed during it would otherwise leave no test output at all.
	output, err := runGoTestMatching(ctx, runner, repoRoot, env, stdout, stderr, "full e2e", fullE2EBucketJSONCommandArgs(bucket)...)
	if err == nil {
		return nil, nil
	}

	log := parseGoTestLog(output)
	failedTests := log.failedTopLevelTests()
	rerunArgs, incidentID, trigger, ok := classifyFullE2EQuirk(bucket, log.classifiableFailures())
	if !ok {
		return failedTests, err
	}

	_, err = rerunClassifiedQuirk(ctx, runner, repoRoot, env, collector, stdout, stderr, classifiedQuirkRerun{
		incidentID: incidentID,
		phase:      bucket.Name,
		trigger:    trigger,
		args:       rerunArgs,
		firstErr:   err,
	})

	return failedTests, err
}

func runE2EPreflightFull(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	env []string,
	collector *verifySummaryCollector,
	stdout, stderr io.Writer,
) error {
	return runE2EPreflight(ctx, runner, repoRoot, env, collector, stdout, stderr, e2ePreflightSpec{
		stepName:    "full fixture preflight",
		status:      "==> go test -tags='e2e e2e_full' fixture preflight\n",
		description: "full e2e fixture preflight",
		args:        fullE2EPreflightArgs(),
	})
}
