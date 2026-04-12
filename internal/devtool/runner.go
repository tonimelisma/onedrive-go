package devtool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

type commandRunner interface {
	Run(ctx context.Context, cwd string, env []string, stdout, stderr io.Writer, name string, args ...string) error
	Output(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error)
	CombinedOutput(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(
	ctx context.Context,
	cwd string,
	env []string,
	stdout, stderr io.Writer,
	name string,
	args ...string,
) error {
	//nolint:gosec // command names and args come from fixed devtool subcommands, not shell input.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}

	return nil
}

func (ExecRunner) Output(
	ctx context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	//nolint:gosec // command names and args come from fixed devtool subcommands, not shell input.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%s %v: %s: %w", name, args, stderr.String(), err)
		}

		return nil, fmt.Errorf("%s %v: %w", name, args, err)
	}

	return out, nil
}

func (ExecRunner) CombinedOutput(
	ctx context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	//nolint:gosec // command names and args come from fixed devtool subcommands, not shell input.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w", name, args, err)
	}

	return out, nil
}

func runMeasuredCommand(
	ctx context.Context,
	name string,
	cwd string,
	env []string,
	args ...string,
) (benchMeasuredProcess, error) {
	//nolint:gosec // benchmark subjects and args are repo-owned fixed command definitions.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	startedAt := time.Now()
	err := cmd.Run()
	elapsed := time.Since(startedAt)

	result := benchMeasuredProcess{
		ElapsedMicros: durationMicros(elapsed),
		Stdout:        append([]byte(nil), stdout.Bytes()...),
		Stderr:        append([]byte(nil), stderr.Bytes()...),
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
		result.UserCPUMicros = durationMicros(cmd.ProcessState.UserTime())
		result.SystemCPUMicros = durationMicros(cmd.ProcessState.SystemTime())
		result.PeakRSSBytes = processStateMaxRSSBytes(cmd.ProcessState)
	}

	if err != nil {
		return result, fmt.Errorf("run benchmark subject command: %w", err)
	}

	return result, nil
}

func processStateMaxRSSBytes(state *os.ProcessState) int64 {
	if state == nil {
		return 0
	}

	rusage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || rusage == nil {
		return 0
	}

	maxRSS := rusage.Maxrss
	if runtime.GOOS == "linux" {
		return maxRSS * linuxMaxRSSUnitBytes
	}

	return maxRSS
}
