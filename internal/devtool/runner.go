package devtool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

type commandRunner interface {
	Run(ctx context.Context, cwd string, env []string, stdout, stderr io.Writer, name string, args ...string) error
	Output(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error)
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
