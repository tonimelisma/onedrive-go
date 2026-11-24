package devtool

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// benchRemoteRoot is the one remote folder a live benchmark touches: its
	// golden fixture and its work slots live under it, and nothing outside
	// it is ever created, changed or deleted. It sits in the test drive's
	// root under the prefix the live suites own (`onedrive-go-e2e`, the
	// e2e preflight scrub's isE2EArtifactName), so a run killed before its
	// cleanup leaves nothing the next e2e run does not delete; the
	// benchmark shares the e2e jobs' concurrency group, so neither ever
	// runs beside the other.
	benchRemoteRoot = "/onedrive-go-e2e-bench"

	// benchRemoteCleanupTimeout bounds the removal of the benchmark's remote
	// folder after the run, on a context of its own: an interrupted run
	// still removes what it created.
	benchRemoteCleanupTimeout = 5 * time.Minute

	// benchLocalGoldenDirName is the local copy of the golden fixture the
	// preparation uploads, inside its runtime root.
	benchLocalGoldenDirName = "golden"

	// benchFixturePreparedThisRun is the pool status of a run whose golden
	// fixture it uploaded itself: no slot outlives the run.
	benchFixturePreparedThisRun = "prepared-this-run"
)

// validateBenchRemoteFixturePaths refuses a fixture whose golden or work
// path is not strictly inside benchRemoteRoot: the preparation deletes that
// folder whole, and a sample deletes its work slot.
func validateBenchRemoteFixturePaths(remotePaths ...string) error {
	for _, remotePath := range remotePaths {
		clean := path.Clean(remotePath)
		if clean != remotePath || !strings.HasPrefix(clean, benchRemoteRoot+"/") {
			return fmt.Errorf("remote fixture path %q is not inside %s", remotePath, benchRemoteRoot)
		}
	}

	return nil
}

// prepareRemoteFixture makes the remote side a run needs, outside every
// sample: it deletes whatever an earlier run left under benchRemoteRoot,
// uploads the golden fixture the samples copy their work slot from, and
// creates the folder the work slots go in. Only a drive the test-account
// allowlist names is touched.
func (s *benchBidirectionalScenarioState) prepareRemoteFixture(ctx context.Context, subject preparedBenchSubject) error {
	runtime, err := s.newSampleRuntime()
	if err != nil {
		return fmt.Errorf("prepare the remote fixture: %w", err)
	}
	s.setupRuntime = &runtime
	live := runtime.asLiveRuntime()

	if resetErr := resetBenchRemoteScope(ctx, subject, live, benchRemoteRoot); resetErr != nil {
		return fmt.Errorf("prepare the remote fixture: %w", resetErr)
	}

	localGolden := filepath.Join(runtime.rootDir, benchLocalGoldenDirName)
	if materializeErr := materializeBenchLiveFixture(localGolden, s.fixture.Files); materializeErr != nil {
		return fmt.Errorf("prepare the remote fixture: materialize the golden fixture: %w", materializeErr)
	}

	golden := s.fixture.Manifest.GoldenRemoteScopePath
	for _, folder := range []string{path.Dir(golden), path.Dir(s.fixture.RemoteScopePath)} {
		process, mkdirErr := subject.measure(ctx, runtime.commandSpec("mkdir", folder))
		if mkdirErr != nil {
			return fmt.Errorf("prepare the remote fixture: create %s: %w: %s",
				folder, mkdirErr, failureExcerpt(mkdirErr, process.Stdout, process.Stderr))
		}
	}

	process, err := subject.measure(ctx, runtime.commandSpec("put", localGolden, golden))
	if err != nil {
		return fmt.Errorf("prepare the remote fixture: upload the golden fixture to %s: %w: %s",
			golden, err, failureExcerpt(err, process.Stdout, process.Stderr))
	}
	if err := waitForBenchRemoteScopeVisible(ctx, subject, live, golden); err != nil {
		return fmt.Errorf("prepare the remote fixture: %w", err)
	}

	// The samples materialize their own local trees; the upload's copy only
	// takes disk space from here on.
	if err := removeAll(localGolden); err != nil {
		return fmt.Errorf("prepare the remote fixture: remove the local golden copy: %w", err)
	}

	return nil
}

// cleanup removes the benchmark's remote folder, when the preparation got as
// far as a drive the allowlist names, and the run's local work root. The
// remote removal runs detached from ctx's cancellation, under a deadline of
// its own, so a run whose context was canceled still removes what it
// created.
func (s *benchBidirectionalScenarioState) cleanup(ctx context.Context, subject preparedBenchSubject) error {
	var remoteErr error
	if s.setupRuntime != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), benchRemoteCleanupTimeout)
		defer cancel()
		if err := resetBenchRemoteScope(cleanupCtx, subject, s.setupRuntime.asLiveRuntime(), benchRemoteRoot); err != nil {
			remoteErr = fmt.Errorf("remove the benchmark's remote folder: %w", err)
		}
	}

	return errors.Join(remoteErr, removeAll(s.workRoot))
}
