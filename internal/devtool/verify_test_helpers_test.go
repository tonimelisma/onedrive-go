package devtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
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
	outputFunc          func(cwd string, env []string, name string, args ...string) ([]byte, error)
	combinedOutputs     map[string][]byte
	combinedErr         error
	combinedErrByKey    map[string]error
	// runStdoutFunc, when set, returns what a Run call writes to its stdout,
	// so a test can show a step the output go test would have printed. When
	// it is nil, a go command prints what passingGoTestOutput says.
	runStdoutFunc func(args []string) string
}

func (f *fakeRunner) Run(
	_ context.Context,
	cwd string,
	env []string,
	stdout, _ io.Writer,
	name string,
	args ...string,
) error {
	f.runCommands = append(f.runCommands, recordedCommand{
		cwd:  cwd,
		env:  append([]string{}, env...),
		name: name,
		args: append([]string{}, args...),
	})

	printed := ""
	switch {
	case f.runStdoutFunc != nil:
		printed = f.runStdoutFunc(args)
	case name == goCommandName:
		printed = passingGoTestOutput(args)
	}
	if _, err := io.WriteString(stdout, printed); err != nil {
		return fmt.Errorf("write fake stdout: %w", err)
	}

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
	if f.outputFunc != nil {
		return f.outputFunc(cwd, env, name, args...)
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

	for i := range summary.E2EFullBuckets {
		if summary.E2EFullBuckets[i].Name != name {
			continue
		}

		assert.Equal(t, status, summary.E2EFullBuckets[i].Status)
		return
	}

	require.Failf(t, "missing bucket summary", "bucket %q not found", name)
}

// testE2EPackage is the import path go test reports for the live suite.
const testE2EPackage = "github.com/tonimelisma/onedrive-go/e2e"

// fakePassingTestName is the test the fake runner reports as run and passed.
const fakePassingTestName = "TestFakeRunnerPasses"

// goCommandName is the go command, as a runner is asked to start it, and
// goTestSubcommand its first argument for a test run.
const (
	goCommandName    = "go"
	goTestSubcommand = "test"
)

// passingGoTestOutput is what go test prints for these arguments when one test
// of its target runs and passes: -json events, -v lines, or the package result
// line alone. Any other go command prints nothing, as a build or a vet that
// succeeds does, and so does a compile-only `go test -c`.
func passingGoTestOutput(args []string) string {
	if len(args) == 0 || args[0] != goTestSubcommand || slices.Contains(args, "-c") {
		return ""
	}

	pkg := fakeGoTestPackage(args)
	switch {
	case slices.Contains(args, "-json"):
		return strings.Join([]string{
			`{"Action":"start","Package":"` + pkg + `"}`,
			`{"Action":"run","Package":"` + pkg + `","Test":"` + fakePassingTestName + `"}`,
			`{"Action":"output","Package":"` + pkg + `","Test":"` + fakePassingTestName +
				`","Output":"--- PASS: ` + fakePassingTestName + ` (0.00s)\n"}`,
			`{"Action":"pass","Package":"` + pkg + `","Test":"` + fakePassingTestName + `","Elapsed":0}`,
			`{"Action":"pass","Package":"` + pkg + `","Elapsed":0.01}`,
		}, "\n") + "\n"
	case slices.Contains(args, "-v"):
		return "=== RUN   " + fakePassingTestName + "\n--- PASS: " + fakePassingTestName + " (0.00s)\nPASS\n" +
			"ok  \t" + pkg + "\t0.01s\n"
	default:
		return "ok  \t" + pkg + "\t0.01s\n"
	}
}

// fakeGoTestPackage is the import path go test reports for the target, the
// last argument: ./internal/sync/... reports as its root package.
func fakeGoTestPackage(args []string) string {
	target := strings.TrimSuffix(args[len(args)-1], "/...")
	target = strings.TrimPrefix(strings.TrimPrefix(target, "."), "/")
	if target == "" {
		return repoModulePath
	}

	return repoModulePath + "/" + target
}

// goCommandKey is the fakeRunner key of a `go` command with these arguments.
func goCommandKey(args []string) string {
	return "go " + strings.Join(args, " ")
}

// rawFailLines is the plain go test output of the named tests failing.
func rawFailLines(tests ...string) string {
	var builder strings.Builder
	for _, test := range tests {
		builder.WriteString("--- FAIL: " + test + " (1.00s)\n")
	}

	return builder.String()
}

// jsonFailLines is the `go test -json` output of the named live tests
// failing, ending with the package's own fail event.
func jsonFailLines(tests ...string) string {
	lines := make([]string, 0, 2*len(tests)+1)
	for _, test := range tests {
		lines = append(lines,
			`{"Action":"run","Package":"`+testE2EPackage+`","Test":"`+test+`"}`,
			`{"Action":"fail","Package":"`+testE2EPackage+`","Test":"`+test+`"}`,
		)
	}
	lines = append(lines, `{"Action":"fail","Package":"`+testE2EPackage+`"}`)

	return strings.Join(lines, "\n") + "\n"
}

// bucketStatuses lists the summary's full-suite bucket statuses in run order.
func bucketStatuses(summary VerifySummary) []string {
	statuses := make([]string, 0, len(summary.E2EFullBuckets))
	for i := range summary.E2EFullBuckets {
		statuses = append(statuses, summary.E2EFullBuckets[i].Status)
	}

	return statuses
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
