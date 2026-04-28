package devtool

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedCommand struct {
	cwd  string
	env  []string
	name string
	args []string
}

type fakeRunner struct {
	runCommands         []recordedCommand
	outputCommands      []recordedCommand
	combinedCommands    []recordedCommand
	outputs             map[string][]byte
	outputsByCWD        map[string]map[string][]byte
	runErr              error
	runErrByKey         map[string]error
	runErrSequenceByKey map[string][]error
	outputErr           error
	combinedOutputs     map[string][]byte
	combinedErr         error
	combinedErrByKey    map[string]error
}

func (f *fakeRunner) Run(
	_ context.Context,
	cwd string,
	env []string,
	_, _ io.Writer,
	name string,
	args ...string,
) error {
	f.runCommands = append(f.runCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	if err, ok := f.runErrByKey[name+" "+strings.Join(args, " ")]; ok {
		return err
	}
	if seq := f.runErrSequenceByKey[name+" "+strings.Join(args, " ")]; len(seq) > 0 {
		next := seq[0]
		f.runErrSequenceByKey[name+" "+strings.Join(args, " ")] = seq[1:]
		return next
	}

	return f.runErr
}

func (f *fakeRunner) Output(
	_ context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	f.outputCommands = append(f.outputCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})
	if f.outputErr != nil {
		return nil, f.outputErr
	}

	key := name + " " + strings.Join(args, " ")
	if byCWD, ok := f.outputsByCWD[cwd]; ok {
		if out, ok := byCWD[key]; ok {
			return out, nil
		}
	}

	return f.outputs[key], nil
}

func (f *fakeRunner) CombinedOutput(
	_ context.Context,
	cwd string,
	env []string,
	name string,
	args ...string,
) ([]byte, error) {
	f.combinedCommands = append(f.combinedCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	key := name + " " + strings.Join(args, " ")
	if err, ok := f.combinedErrByKey[key]; ok {
		return f.combinedOutputs[key], err
	}
	if f.combinedErr != nil {
		return f.combinedOutputs[key], f.combinedErr
	}

	return f.combinedOutputs[key], nil
}

func readVerifySummaryFile(t *testing.T, path string, summary *VerifySummary) {
	t.Helper()

	data, err := readFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, summary))
}

func assertVerifySummaryHasStep(t *testing.T, summary VerifySummary, name string) {
	t.Helper()

	for _, step := range summary.Steps {
		if step.Name != name {
			continue
		}

		assert.Equal(t, verifySummaryStatusPass, step.Status)
		return
	}

	require.Failf(t, "missing summary step", "step %q not found", name)
}

func requireVerifySummaryStep(t *testing.T, summary VerifySummary, name string) VerifyStepSummary {
	t.Helper()

	for _, step := range summary.Steps {
		if step.Name == name {
			return step
		}
	}

	require.Failf(t, "missing summary step", "step %q not found", name)
	return VerifyStepSummary{}
}

func assertVerifyBucketSummary(t *testing.T, summary VerifySummary, name string, status string) {
	t.Helper()

	for _, bucket := range summary.E2EFullBuckets {
		if bucket.Name != name {
			continue
		}

		assert.Equal(t, status, bucket.Status)
		return
	}

	require.Failf(t, "missing bucket summary", "bucket %q not found", name)
}

func assertCommandHasEnvVar(t *testing.T, cmd recordedCommand, want string) {
	t.Helper()
	assert.Contains(t, cmd.env, want)
}

func assertCommandLacksEnvVar(t *testing.T, cmd recordedCommand, want string) {
	t.Helper()
	assert.NotContains(t, cmd.env, want)
}

func assertCommandLacksSkipSuiteScrubEnvVar(t *testing.T, cmd recordedCommand) {
	t.Helper()
	assert.NotContains(t, cmd.env, e2eSkipSuiteScrubEnvVar+"=1")
}

// Ensure the fake runner still satisfies the commandRunner contract if the
// signatures evolve.
var _ commandRunner = (*fakeRunner)(nil)
