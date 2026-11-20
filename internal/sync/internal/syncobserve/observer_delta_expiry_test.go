package syncobserve

import (
	"context"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// tokenRecordingFetcher answers the first delta call with gone and every
// later one with an empty window, recording the tokens it was asked for.
type tokenRecordingFetcher struct {
	mu     stdsync.Mutex // guards tokens
	gone   error
	tokens []string
}

func (f *tokenRecordingFetcher) Delta(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	if len(f.tokens) == 1 {
		return nil, f.gone
	}

	return &graph.DeltaPage{DeltaLink: token}, nil
}

func (f *tokenRecordingFetcher) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.tokens...)
}

// Validates: R-2.15.3
func TestWatch_DeltaExpiredRecoversThroughTheFullRefresh(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	fetcher := &tokenRecordingFetcher{gone: syncfixture.DeltaGone("resyncChangesUploadDifferences")}
	obs := NewRemoteObserver(fetcher, syncfixture.EmptyBaseline(), driveID, syncfixture.TestLogger(t))
	// Every watch sleep lasts until the test cancels: the loop stops after
	// the recovery instead of polling on.
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}}

	recovered := make(chan syncdomain.ResyncKind, 1)
	orphan := syncdomain.RemoteItem{ObservedItem: syncdomain.ObservedItem{ItemID: "gone", Path: "gone.txt", IsDeleted: true, ReuploadLocal: true}}
	obs.SetDeltaExpiredRecovery(func(_ context.Context, kind syncdomain.ResyncKind) (RemoteObservationBatch, error) {
		recovered <- kind
		return RemoteObservationBatch{
			Observed:              []syncdomain.ObservedItem{orphan.ObservedItem},
			Changes:               remoteChangesOf([]syncdomain.RemoteItem{orphan}),
			CursorToken:           "token-after-full-refresh",
			MarkFullRemoteRefresh: true,
		}, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batches := make(chan RemoteObservationBatch, 4)
	applied := make(chan RemoteObservationBatch, 1)
	events := startTestRemoteBatchDrain(t, ctx, batches, func(batch RemoteObservationBatch) error {
		applied <- batch
		return nil
	})
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "expired-token", batches, time.Hour, nil, nil)
	})

	assert.Equal(t, syncdomain.ResyncKindUploadDifferences, syncfixture.ReceiveWithin(t, recovered, "the delta-expired recovery to run"),
		"the resync flavor reaches the full refresh")
	emitted := syncfixture.ReceiveWithin(t, events, "the full refresh's change")
	assert.Equal(t, "gone.txt", emitted.Path)
	handed := syncfixture.ReceiveWithin(t, applied, "the full refresh's batch")
	require.Len(t, handed.Observed, 1)
	assert.True(t, handed.Observed[0].ReuploadLocal, "the full refresh's batch, orphans included, is what watch emits")
	require.Eventually(t, func() bool { return obs.CurrentDeltaToken() == "token-after-full-refresh" },
		syncfixture.EventWait, time.Millisecond)
	syncfixture.StopWatch(t, cancel, done)

	assert.Equal(t, []string{"expired-token"}, fetcher.seen(),
		"watch never re-enumerates on its own, without orphan detection")
}
