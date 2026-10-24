package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.6.21
func TestFileIsJournalStream_MatchesOnlyTheNamedStream(t *testing.T) {
	t.Parallel()

	f, err := os.Create(filepath.Join(t.TempDir(), "stream"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, f.Close()) })

	var st syscall.Stat_t
	require.NoError(t, syscall.Fstat(int(f.Fd()), &st))
	//nolint:unconvert // see fileIsJournalStream
	matching := fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))

	assert.True(t, fileIsJournalStream(f, matching))
	assert.False(t, fileIsJournalStream(f, ""), "unset JOURNAL_STREAM is not the journal")
	assert.False(t, fileIsJournalStream(f, "1:2"), "an inherited JOURNAL_STREAM for another stream is not this one")
	assert.False(t, fileIsJournalStream(f, "garbage"))
}
