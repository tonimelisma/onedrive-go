package syncexec

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// completeExecutorInputs is executor inputs with every required field set.
func completeExecutorInputs(t *testing.T) ExecutorInputs {
	t.Helper()

	syncTree, err := synctree.Open(t.TempDir())
	require.NoError(t, err)
	dl := &syncfixture.ExecutorMockDownloader{}
	ul := &syncfixture.ExecutorMockUploader{}
	logger := synctest.TestLogger(t)
	records := syncfixture.NewMemoryExecutorRecords(time.Now)

	return ExecutorInputs{
		Items:          &syncfixture.ExecutorMockItemClient{},
		Downloads:      dl,
		Uploads:        ul,
		Transfers:      driveops.NewTransferManager(dl, ul, nil, logger),
		SyncTree:       syncTree,
		DriveID:        driveid.New(synctest.TestDriveID),
		Logger:         logger,
		Trash:          localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil),
		PendingCreates: records,
		MoveIntents:    records,
		Now:            time.Now,
	}
}

// Validates: R-6.10.30
//
// An executor configuration is built whole or not at all: inputs that lack
// a required field are refused, naming it, and the optional ones (remote
// root, content filter, sync folder identity, path convergence, hooks) may
// be zero. The trash and the pending-create and move-intent records are
// required: production always has them, so an executor without one would
// run code production never runs (a permanent local delete, a create with no
// intent recorded).
func TestNewExecutorConfig_RejectsEachMissingRequiredField(t *testing.T) {
	t.Parallel()

	complete := completeExecutorInputs(t)
	cfg, err := NewExecutorConfig(&complete)
	require.NoError(t, err)
	assert.NotNil(t, cfg)

	for _, tc := range []struct {
		field string
		clear func(*ExecutorInputs)
	}{
		{field: "Items", clear: func(in *ExecutorInputs) { in.Items = nil }},
		{field: "Downloads", clear: func(in *ExecutorInputs) { in.Downloads = nil }},
		{field: "Uploads", clear: func(in *ExecutorInputs) { in.Uploads = nil }},
		{field: "Transfers", clear: func(in *ExecutorInputs) { in.Transfers = nil }},
		{field: "SyncTree", clear: func(in *ExecutorInputs) { in.SyncTree = nil }},
		{field: "DriveID", clear: func(in *ExecutorInputs) { in.DriveID = driveid.ID{} }},
		{field: "Logger", clear: func(in *ExecutorInputs) { in.Logger = nil }},
		{field: "Trash", clear: func(in *ExecutorInputs) { in.Trash = nil }},
		{field: "PendingCreates", clear: func(in *ExecutorInputs) { in.PendingCreates = nil }},
		{field: "MoveIntents", clear: func(in *ExecutorInputs) { in.MoveIntents = nil }},
		{field: "Now", clear: func(in *ExecutorInputs) { in.Now = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			in := completeExecutorInputs(t)
			tc.clear(&in)
			cfg, err := NewExecutorConfig(&in)
			require.ErrorContains(t, err, tc.field+" is required")
			assert.Nil(t, cfg)
		})
	}
}
