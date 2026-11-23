package devtool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// defaultVerifyLogDirName is the step-log directory under the repository
	// root when --verify-log-dir names none. .gitignore lists it.
	defaultVerifyLogDirName = ".verify-logs"

	// verifyStepLogPrefix starts the line a failed go test step prints on
	// stderr after its marker line: the prefix and the path of the file that
	// kept the step's output. It is a line of its own because the marker line
	// holds nothing but the profile and the step (failedStepMarker).
	verifyStepLogPrefix = "verify step log: "

	verifyStepLogFilePerm = 0o600
	verifyStepLogDirPerm  = 0o700
)

// stepLogs keeps the output of each go test step of one profile in a file of
// its own. Output that only streamed is lost when the run's output was piped
// through head or tail or cut off by a scrollback, so a failure would leave
// nothing to diagnose; the file is kept when its step fails and removed when
// it passes, so a passing run leaves nothing behind.
//
// The collector owns it and opens one file at a time: steps run one after
// another, and every step writes through the two tees the profile hands its
// steps in place of its stdout and stderr, which copy into the open file.
type stepLogs struct {
	dir     string
	profile VerifyProfile
	shard   verifyShard

	mu sync.Mutex // guards file and writeErr
	// file is the open log of the running go test step, nil between steps
	// and while a step that runs no go test runs.
	file *os.File
	// writeErr is the first write to file that failed; closing the log
	// reports it.
	writeErr error
}

func newStepLogs(dir string, profile VerifyProfile, shard verifyShard) *stepLogs {
	return &stepLogs{dir: dir, profile: profile, shard: shard}
}

// resolveVerifyLogDir is the step-log directory: the one --verify-log-dir
// names, else .verify-logs under the repository root.
func resolveVerifyLogDir(explicit string, repoRoot string) string {
	if explicit != "" {
		return explicit
	}

	return filepath.Join(repoRoot, defaultVerifyLogDirName)
}

// path is the log file of a step, named by the profile, the step and, for a
// sharded run, the shard, so two shards or two profiles never share one.
func (l *stepLogs) path(step verifyStepName) string {
	name := string(l.profile) + "-" + string(step)
	if l.shard.enabled() {
		name += fmt.Sprintf("-shard-%d-of-%d", l.shard.index, l.shard.count)
	}

	return filepath.Join(l.dir, name+".log")
}

// tee returns a writer that writes to base, as the step's stream did before,
// and copies every write into the running step's log. A failed copy does not
// fail the write: the stream is what the run reports through, and closing
// the log reports the copy's failure as the step's.
func (l *stepLogs) tee(base io.Writer) io.Writer {
	return stepLogTee{base: base, logs: l}
}

type stepLogTee struct {
	base io.Writer
	logs *stepLogs
}

func (t stepLogTee) Write(p []byte) (int, error) {
	n, err := t.base.Write(p)
	if err != nil {
		return n, fmt.Errorf("write step output: %w", err)
	}
	t.logs.copyToFile(p)

	return n, nil
}

// copyToFile appends p to the open log, if any. An *os.File serializes
// concurrent writes itself, so the lock covers only the shared fields and not
// the write; exec copies stdout and stderr on two goroutines.
func (l *stepLogs) copyToFile(p []byte) {
	l.mu.Lock()
	file := l.file
	l.mu.Unlock()
	if file == nil {
		return
	}

	if _, err := file.Write(p); err != nil {
		l.mu.Lock()
		if l.writeErr == nil {
			l.writeErr = fmt.Errorf("write step log %s: %w", file.Name(), err)
		}
		l.mu.Unlock()
	}
}

// open starts the log of a go test step, replacing the file an earlier run
// of the same step left. Writes append, so a run killed mid-step leaves a
// readable partial log.
func (l *stepLogs) open(step verifyStepName) (string, error) {
	if err := mkdirAll(l.dir, verifyStepLogDirPerm); err != nil {
		return "", fmt.Errorf("create step log directory: %w", err)
	}

	path := l.path(step)
	file, err := openFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, verifyStepLogFilePerm)
	if err != nil {
		return "", fmt.Errorf("open step log: %w", err)
	}

	l.mu.Lock()
	l.file = file
	l.writeErr = nil
	l.mu.Unlock()

	return path, nil
}

// finish closes the open log and keeps it when the step failed (stepErr is
// not nil) and removes it when it passed. It returns whether the file was
// kept, and any failure to write, close or remove it; a log that could not
// be written in full is kept, since it is all there is of the step's output.
func (l *stepLogs) finish(path string, stepErr error) (bool, error) {
	l.mu.Lock()
	file := l.file
	writeErr := l.writeErr
	l.file = nil
	l.writeErr = nil
	l.mu.Unlock()

	var logErr error
	if file != nil {
		if err := file.Close(); err != nil {
			logErr = fmt.Errorf("close step log %s: %w", path, err)
		}
	}
	logErr = errors.Join(writeErr, logErr)
	if stepErr != nil || logErr != nil {
		return true, logErr
	}

	if err := remove(path); err != nil {
		return true, fmt.Errorf("remove passing step log: %w", err)
	}

	return false, nil
}

// stepLogLine is the line a failed go test step prints after its marker line.
func stepLogLine(path string) string {
	return verifyStepLogPrefix + path + "\n"
}
