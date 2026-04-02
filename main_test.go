package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_HelpReturnsSuccess(t *testing.T) {
	exitCode := run([]string{"--help"})

	assert.Equal(t, 0, exitCode)
}

func TestMain_HelpReturnsSuccess(t *testing.T) {
	//nolint:gosec // The test re-executes the current test binary with fixed args.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestMainHelperProcess", "--", "--help")
	cmd.Env = append(os.Environ(), "ONEDRIVE_GO_MAIN_HELPER=1")

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "main() failed:\n%s", output)
	assert.Contains(t, string(output), "OneDrive CLI")
}

func TestMainHelperProcess(t *testing.T) {
	t.Helper()

	if os.Getenv("ONEDRIVE_GO_MAIN_HELPER") != "1" {
		return
	}

	argsIndex := 0
	for i, arg := range os.Args {
		if arg == "--" {
			argsIndex = i + 1
			break
		}
	}

	os.Args = append([]string{os.Args[0]}, os.Args[argsIndex:]...)
	main()
}
