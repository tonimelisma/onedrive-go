package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.1.3, R-2.1.4
func TestDeferredCountsAddActionAndTotal(t *testing.T) {
	t.Parallel()

	var counts syncdomain.DeferredCounts

	counts.AddAction(nil)
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionCleanup})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionBaselineUpdate})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionFolderCreate})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionLocalMove})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionRemoteMove})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionDownload})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionUpload})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionLocalDelete})
	counts.AddAction(&syncdomain.Action{Type: syncdomain.ActionRemoteDelete})

	assert.Equal(t, syncdomain.DeferredCounts{
		FolderCreates: 1,
		Moves:         2,
		Downloads:     1,
		Uploads:       1,
		LocalDeletes:  1,
		RemoteDeletes: 1,
	}, counts)
	assert.Equal(t, 7, counts.Total())
}
