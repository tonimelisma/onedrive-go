package config

import (
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// Validates: R-6.8.16
func TestClassifyLoadOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		warnings []ConfigWarning
		want     errclass.Class
	}{
		{name: "fatal_error", err: errors.New("bad config"), want: errclass.ClassFatal},
		{name: "warnings_are_actionable", warnings: []ConfigWarning{{Message: "bad key"}}, want: errclass.ClassActionable},
		{name: "clean_success", want: errclass.ClassSuccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outcome := ClassifyLoadOutcome(tt.err, tt.warnings)
			assert.Equal(t, tt.want, outcome.Class)
			assert.Equal(t, len(tt.warnings), outcome.WarningCount)
		})
	}
}

// Validates: R-6.6.31
func TestConfigFailuresAreTaggedInvalidConfig(t *testing.T) {
	t.Parallel()

	badSyntax := writeTestConfig(t, "this is = = not toml")
	unknownKey := writeTestConfig(t, `transfer_worker = 8`)
	badValue := writeTestConfig(t, `transfer_workers = -1`)
	logger := slog.New(slog.DiscardHandler)

	_, loadSyntaxErr := Load(badSyntax, logger)
	_, loadUnknownErr := LoadOrDefault(unknownKey, logger)
	_, loadValueErr := Load(badValue, logger)
	_, _, lenientErr := LoadOrDefaultLenient(badSyntax, logger)
	oneDrive := &Config{Drives: map[driveid.CanonicalID]Drive{
		driveid.MustCanonicalID("personal:user@example.com"): {},
	}}
	_, _, matchErr := MatchDrive(oneDrive, "no-such-drive", logger)
	_, resolveErr := ResolveDrives(oneDrive, []string{"no-such-drive"}, true, logger)

	for name, err := range map[string]error{
		"syntax":      loadSyntaxErr,
		"unknown key": loadUnknownErr,
		"bad value":   loadValueErr,
		"lenient":     lenientErr,
		"match":       matchErr,
		"resolve":     resolveErr,
	} {
		require.Error(t, err, name)
		require.ErrorIs(t, err, ErrInvalidConfig, name)
		assert.NotContains(t, err.Error(), ErrInvalidConfig.Error(), "%s: the tag must not change the text", name)
	}
}

// Validates: R-6.6.31
func TestMarkInvalidConfigKeepsChainAndIsIdempotent(t *testing.T) {
	t.Parallel()

	require.NoError(t, markInvalidConfig(nil))

	cause := &os.PathError{Op: "open", Path: "/x/config.toml", Err: os.ErrPermission}
	marked := markInvalidConfig(cause)
	require.ErrorIs(t, marked, ErrInvalidConfig)
	assert.ErrorIs(t, marked, os.ErrPermission)
	assert.Equal(t, cause.Error(), marked.Error())
	assert.Same(t, marked, markInvalidConfig(marked))
	assert.NotErrorIs(t, errors.New("other"), ErrInvalidConfig)
}
