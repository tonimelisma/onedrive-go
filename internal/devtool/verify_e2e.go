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
	"sort"
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
		"TestE2E_Sync_DirectionalModes_PreserveEditEditConflict",
		"TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict",
		"TestE2E_Sync_ResolveAll",
		"TestE2E_Sync_CreateCreateConflict_ResolveKeepLocal",
		"TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict",
		"TestE2E_Sync_DeletePropagation",
		"TestE2E_Sync_DeleteSafetyThreshold",
		"TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges",
		"TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges",
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

func discoverTaggedE2ETests(e2eDir string, buildExpression string) ([]string, error) {
	entries, err := readDir(e2eDir)
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
		data, err := readFile(path)
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

		var event goTestJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Action != verifySummaryStatusFail || event.Test == "" {
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
