package synctest

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// SkipWithoutHardLinks skips the calling test on a temporary filesystem
// without hard links. A test that needs a no-replace rename to link its entry
// and then unlink the source cannot run there: the rename takes its check and
// plain rename instead, and the case under test never happens.
func SkipWithoutHardLinks(t *testing.T) {
	t.Helper()

	probe, err := localpath.CreateTemp(t.TempDir(), "hard-link-probe-*")
	require.NoError(t, err)
	require.NoError(t, probe.Close())
	err = os.Link(probe.Name(), probe.Name()+"-link")
	if localpath.IsLinkUnsupported(err) {
		t.Skipf("the temporary filesystem has no hard links, which the link and unlink under test need: %v", err)
	}
	require.NoError(t, err)
}
