package cli

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestFilterStatusSnapshot_IntersectsAccountAndDriveSelectors(t *testing.T) {
	t.Parallel()

	aliceCID := driveid.MustCanonicalID("personal:alice@example.com")
	bobCID := driveid.MustCanonicalID("personal:bob@example.com")

	cfg := config.DefaultConfig()
	cfg.Drives[aliceCID] = config.Drive{SyncDir: "/sync/alice"}
	cfg.Drives[bobCID] = config.Drive{SyncDir: "/sync/bob"}

	filtered, err := filterStatusSnapshot(accountViewSnapshot{
		Config: cfg,
		Accounts: []accountView{
			{Email: "alice@example.com"},
			{Email: "bob@example.com"},
		},
	}, "alice@example.com", []string{bobCID.String()}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	assert.Empty(t, filtered.Config.Drives, "account and drive filters should intersect, not widen to a union")
	require.Len(t, filtered.Accounts, 1)
	assert.Equal(t, "alice@example.com", filtered.Accounts[0].Email)
}
