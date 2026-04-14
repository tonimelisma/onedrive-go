package clishape

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func findSubcommand(spec CommandSpec, name string) (CommandSpec, bool) {
	for i := range spec.Subcommands {
		if spec.Subcommands[i].Name == name {
			return spec.Subcommands[i], true
		}
	}

	return CommandSpec{}, false
}

func findFlag(spec CommandSpec, name string) (FlagSpec, bool) {
	for i := range spec.Flags {
		if spec.Flags[i].Name == name {
			return spec.Flags[i], true
		}
	}

	return FlagSpec{}, false
}

// Validates: R-1.1, R-2.1, R-3.1, R-4.1
func TestRootDescribesCurrentCLIGrammar(t *testing.T) {
	t.Parallel()

	root := Root()
	require.Equal(t, "onedrive-go", root.Name)
	assert.False(t, root.Runnable)

	configFlag, found := findFlag(root, "config")
	require.True(t, found)
	assert.True(t, configFlag.ConsumesValue)

	jsonFlag, found := findFlag(root, "json")
	require.True(t, found)
	assert.False(t, jsonFlag.ConsumesValue)

	syncCmd, found := findSubcommand(root, "sync")
	require.True(t, found)
	assert.True(t, syncCmd.Runnable)
	assert.Len(t, syncCmd.Flags, 5)

	watchFlag, found := findFlag(syncCmd, "watch")
	require.True(t, found)
	assert.False(t, watchFlag.ConsumesValue)

	fullFlag, found := findFlag(syncCmd, "full")
	require.True(t, found)
	assert.False(t, fullFlag.ConsumesValue)

	driveCmd, found := findSubcommand(root, "drive")
	require.True(t, found)
	assert.False(t, driveCmd.Runnable)

	addCmd, found := findSubcommand(driveCmd, "add")
	require.True(t, found)
	assert.True(t, addCmd.Runnable)

	removeCmd, found := findSubcommand(driveCmd, "remove")
	require.True(t, found)
	purgeFlag, found := findFlag(removeCmd, "purge")
	require.True(t, found)
	assert.False(t, purgeFlag.ConsumesValue)

	perfCmd, found := findSubcommand(root, "perf")
	require.True(t, found)
	assert.False(t, perfCmd.Runnable)

	captureCmd, found := findSubcommand(perfCmd, "capture")
	require.True(t, found)
	assert.True(t, captureCmd.Runnable)

	durationFlag, found := findFlag(captureCmd, "duration")
	require.True(t, found)
	assert.True(t, durationFlag.ConsumesValue)

	_, found = findSubcommand(root, "resolve")
	assert.False(t, found)
}

// Validates: R-4.1
func TestCommandHelpersDescribeFlags(t *testing.T) {
	t.Parallel()

	cmd := command("status", boolFlag("perf"), valueFlag("output"))

	require.Equal(t, "status", cmd.Name)
	assert.True(t, cmd.Runnable)
	assert.Equal(t, []FlagSpec{
		{Name: "perf"},
		{Name: "output", ConsumesValue: true},
	}, cmd.Flags)
}
