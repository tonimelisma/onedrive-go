package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.10.47
func TestGroupBlockedRetryWork_GroupsAndNormalizesPaths(t *testing.T) {
	t.Parallel()

	grouped := GroupBlockedRetryWork([]RetryWorkRow{
		{ScopeKey: SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/b.txt", Blocked: true},
		{ScopeKey: SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/a.txt", Blocked: true},
		{ScopeKey: SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/a.txt", Blocked: true},
		{ScopeKey: SKService(), Path: "", Blocked: true},
		{ScopeKey: ScopeKey{}, Path: "ignored.txt", Blocked: true},
	})

	assert.Equal(t, map[ScopeKey]BlockedRetryGroup{
		SKPermRemoteWrite("Shared/Docs"): {
			Count: 3,
			Paths: []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"},
		},
		SKService(): {
			Count: 1,
			Paths: nil,
		},
	}, grouped)
}
