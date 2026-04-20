package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestShellQuoteArg_EscapesSingleQuotes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, `'owner'"'"'s drive'`, shellQuoteArg("owner's drive"))
}

func TestSyncStateResetCommand_QuotesCanonicalID(t *testing.T) {
	t.Parallel()

	cid := driveid.MustCanonicalID("personal:user@example.com")
	assert.Equal(t,
		"onedrive-go drive reset-sync-state --drive 'personal:user@example.com'",
		syncStateResetCommand(cid),
	)
}
