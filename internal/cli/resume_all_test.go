package cli

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

func pausedDrive() config.Drive {
	paused := true

	return config.Drive{SyncDir: "~/OneDrive", Paused: &paused}
}

// Validates: R-2.6
//
// "Resume all" is a bulk request. Returning at the first failure leaves an
// arbitrary subset still paused with nothing saying which: the drives the loop
// had not reached yet look exactly like the ones that resumed.
//
// The failure here is real rather than mocked. Only one drive has a section in
// the config file, so clearing the other's keys fails the way it would if a
// section went missing underneath the command.
func TestResumeAllDrives_ContinuesPastAFailingDrive(t *testing.T) {
	t.Parallel()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")

	// Named so the failing drive sorts first: the point is that the loop
	// continues past it, which map order would otherwise decide at random.
	present := driveid.MustCanonicalID("personal:zzz-present@example.com")
	missing := driveid.MustCanonicalID("personal:aaa-missing@example.com")

	require.NoError(t, config.AppendDriveSection(cfgPath, present, "~/OneDrive"))
	require.NoError(t, config.SetDriveKey(cfgPath, present, "paused", "true"))

	cfg := &config.Config{Drives: map[driveid.CanonicalID]config.Drive{
		present: pausedDrive(),
		missing: pausedDrive(),
	}}

	var status bytes.Buffer

	cc := &CLIContext{CfgPath: cfgPath, StatusWriter: &status}

	err := resumeAllDrivesWithNow(t.Context(), cc, time.Now, cfg)

	require.Error(t, err, "the failing drive is still reported")
	assert.Contains(t, err.Error(), missing.Email())

	data, readErr := localpath.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(data), "paused",
		"the drive that could be resumed must be resumed even though another failed")
	assert.Contains(t, status.String(), present.String())
}
