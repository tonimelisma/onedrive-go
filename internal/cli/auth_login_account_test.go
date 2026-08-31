package cli

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.7.31
//
// Account names reach this path from the config file and the catalog, both of
// which the user can edit. A name that is empty, or that contains a colon,
// makes the canonical ID malformed -- and building it with MustCanonicalID
// turned that into a panic, so a hand-edited config crashed `status` with a
// stack trace instead of reporting a problem.
func TestFindTokenFallback_MalformedAccountDoesNotPanic(t *testing.T) {
	tests := []struct {
		name    string
		account string
	}{
		{name: "Empty", account: ""},
		{name: "ContainsColon", account: "user@example.com:extra"},
		{name: "OnlyColon", account: ":"},
		{name: "LeadingColon", account: ":user@example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got driveid.CanonicalID

			require.NotPanics(t, func() {
				got = findTokenFallback(tt.account, slog.Default())
			})

			assert.True(t, got.IsZero(),
				"a malformed account has no canonical ID, and callers treat the zero value as no login")
		})
	}
}

// Validates: R-6.7.31
//
// The ordinary case must be unaffected: a well-formed account with no token on
// disk still falls back to personal.
func TestFindTokenFallback_WellFormedAccountStillDefaultsToPersonal(t *testing.T) {
	got := findTokenFallback("nobody-at-all@example.com", slog.Default())

	assert.Equal(t, driveid.MustCanonicalID("personal:nobody-at-all@example.com"), got)
}
