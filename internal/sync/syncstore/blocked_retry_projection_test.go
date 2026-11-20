package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.47
func TestGroupBlockedRetryWork_GroupsAndNormalizesPaths(t *testing.T) {
	t.Parallel()

	grouped := GroupBlockedRetryWork([]syncdomain.RetryWorkRow{
		{ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/b.txt", Blocked: true},
		{ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/a.txt", Blocked: true},
		{ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/a.txt", Blocked: true},
		{ScopeKey: syncdomain.SKService(), Path: "", Blocked: true},
		{ScopeKey: syncdomain.ScopeKey{}, Path: "ignored.txt", Blocked: true},
	})

	assert.Equal(t, map[syncdomain.ScopeKey]blockedRetryGroup{
		syncdomain.SKPermRemoteWrite("Shared/Docs"): {
			Count: 3,
			Paths: []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"},
		},
		syncdomain.SKService(): {
			Count: 1,
			Paths: nil,
		},
	}, grouped)
}
