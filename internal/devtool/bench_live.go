package devtool

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

// benchLiveFixturePlan is the tree a sample's sync root must hold once the
// sample finishes: every file with its size, and every directory.
type benchLiveFixturePlan struct {
	Files       []benchLiveFileEntry
	Directories []string
}

// benchLiveCommandRuntime is what a CLI command of a live sample needs: its
// working directory, environment, config file and drive.
type benchLiveCommandRuntime struct {
	rootDir    string
	configPath string
	driveID    string
	env        []string
}

func benchScopeRootRelativePath(remoteScopePath string) (string, error) {
	cleanScope := path.Clean(remoteScopePath)
	if cleanScope == "." || cleanScope == "/" || cleanScope == "" || !strings.HasPrefix(cleanScope, "/") {
		return "", fmt.Errorf("remote scope path %q must be an absolute non-root path", remoteScopePath)
	}

	return strings.TrimPrefix(cleanScope, "/"), nil
}

// expandBenchLiveFixtureGroups lists the files and directories the fixture
// groups describe, each list in name order.
func expandBenchLiveFixtureGroups(groups []benchLiveFixtureGroup) ([]benchLiveFileEntry, []string, error) {
	files := make([]benchLiveFileEntry, 0)
	directorySet := map[string]struct{}{}

	for _, group := range groups {
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

// finalizeBenchLiveSampleRuntime removes a sample's runtime root. A sample
// that succeeded but leaves a root it cannot remove becomes a fixture failure.
//
//nolint:gocritic // Samples are immutable result records; cleanup may return a reclassified copy.
func finalizeBenchLiveSampleRuntime(runtimeRoot string, sample benchSample) benchSample {
	if cleanupErr := removeAll(runtimeRoot); cleanupErr != nil && sample.Status == BenchSampleSuccess {
		return benchFixtureFailureSample(sample, fmt.Errorf("cleanup sample runtime: %w", cleanupErr))
	}

	return sample
}

// classifyBenchProcessFailure classifies a timed one-shot sync that did not
// exit 0. A sync that finished but left work exits 1 (R-6.6.28) and is no
// measurement of a full sync either: its sample fails like any other, and
// the excerpt carries the sync's summary line, which says what it left.
//
//nolint:gocritic // Samples are immutable result records; copy/return keeps process classification explicit.
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

	failed := benchSubjectFailureSample(measuredSample, runErr, process.Stdout, process.Stderr)
	failed.FailureExcerpt = benchSyncFailureExcerpt(runErr, process)

	return failed
}

// benchSyncFailureExcerpt is the excerpt of a one-shot sync that did not exit
// 0: its summary line, then the tail of its error output, within
// benchFailureExcerptLimit. A sync with no summary line keeps the excerpt
// every failed command has.
func benchSyncFailureExcerpt(runErr error, process benchMeasuredProcess) string {
	summary := benchSyncSummaryLine(process.Stdout)
	if summary == "" {
		return failureExcerpt(runErr, process.Stdout, process.Stderr)
	}

	summary = truncateString(summary, benchFailureExcerptLimit/2)
	rest := failureExcerpt(runErr, nil, process.Stderr)

	return summary + "\n" + tailString(rest, benchFailureExcerptLimit-len(summary)-1)
}

// benchSyncSummaryLine is the last summary line a one-shot sync printed on
// stdout ("Done in ...", which names what the run left), or "".
func benchSyncSummaryLine(stdout []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], benchSyncSummaryMarker) {
			return strings.TrimSpace(lines[i])
		}
	}

	return ""
}

// benchSyncSummaryMarker starts the summary line of a one-shot sync that did
// any work or left any.
const benchSyncSummaryMarker = "Done in "

// tailString is the last limit bytes of value, marked "..." when cut.
func tailString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= len(benchExcerptCutMark) {
		return value[len(value)-max(0, limit):]
	}

	return benchExcerptCutMark + value[len(value)-(limit-len(benchExcerptCutMark)):]
}

// benchExcerptCutMark marks an excerpt cut at its start.
const benchExcerptCutMark = "..."

// validateBenchAllowlist refuses a live benchmark drive the test-account
// allowlist does not name, through the one allowlist reader the live suites
// use.
func validateBenchAllowlist(driveID string) error {
	if err := testutil.CheckDriveAllowed(benchPrimaryDriveEnvVar, driveID); err != nil {
		return fmt.Errorf("live benchmark: %w", err)
	}

	return nil
}

func locateBenchCredentialDir(repoRoot string) (string, error) {
	dir := filepath.Join(repoRoot, ".testdata")
	if _, err := stat(dir); err != nil {
		return "", fmt.Errorf("live benchmark requires %s (run scripts/bootstrap-test-credentials.sh first)", dir)
	}

	return dir, nil
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
	data, err := readFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := writeFile(dst, data); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	return nil
}

func materializeBenchLiveFixture(scopeRoot string, files []benchLiveFileEntry) error {
	if err := mkdirAll(scopeRoot, benchLiveFixtureDirPerm); err != nil {
		return fmt.Errorf("create fixture scope root: %w", err)
	}

	for _, entry := range files {
		targetPath := filepath.Join(scopeRoot, filepath.FromSlash(entry.RelativePath))
		if err := mkdirAll(filepath.Dir(targetPath), benchLiveFixtureDirPerm); err != nil {
			return fmt.Errorf("create fixture parent for %s: %w", entry.RelativePath, err)
		}
		if err := writeBenchLiveFile(targetPath, entry); err != nil {
			return err
		}
	}

	return nil
}

func writeBenchLiveFile(targetPath string, entry benchLiveFileEntry) (err error) {
	file, err := openFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, benchLiveFilePerm)
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
	if err := writeFile(logPath, []byte{}); err != nil {
		return fmt.Errorf("reset benchmark log file: %w", err)
	}

	return nil
}

func truncateBenchLiveFile(path string, size int64) (err error) {
	file, err := openFile(path, os.O_WRONLY, benchLiveFilePerm)
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
	file, err := open(path)
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
