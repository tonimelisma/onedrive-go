package devtool

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// devtoolLinkedModulePackages are the packages of the module the
// verification tool may link: its own two and liveenv, the live runs' shared
// environment rules, which import the standard library alone.
func devtoolLinkedModulePackages() []string {
	return []string{
		repoModulePath + "/testutil/liveenv",
		repoModulePath + "/internal/devtool",
		repoModulePath + "/cmd/devtool",
	}
}

// Validates: R-6.10.5
//
// The verification tool links no product package, through any import:
// depguard's devtool-no-product rule reads direct imports only, and testutil
// once brought localpath and sharedref in behind it. The go command's own
// dependency list of the tool, tests left out, holds no package of the
// module but the tool's and liveenv.
func TestDevtoolLinksNoProductPackage(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "./cmd/devtool")
	cmd.Dir = repoRootForTest(t)
	out, err := cmd.Output()
	require.NoError(t, err, "go list -deps ./cmd/devtool")

	linked := []string{}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == repoModulePath || strings.HasPrefix(pkg, repoModulePath+"/") {
			linked = append(linked, pkg)
		}
	}
	require.NotEmpty(t, linked, "the list names the tool itself")
	for _, pkg := range linked {
		assert.Contains(t, devtoolLinkedModulePackages(), pkg, "devtool links %s", pkg)
	}
}
