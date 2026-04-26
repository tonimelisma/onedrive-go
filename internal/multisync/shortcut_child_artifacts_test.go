package multisync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Validates: R-2.4.8
func TestPurgeShortcutChildArtifacts_IgnoresExplicitMountID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountID := "business:user@example.com"
	statePath := config.MountStatePath(mountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("state"), 0o600))
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.Drives[mountID] = config.CatalogDrive{CanonicalID: mountID, DisplayName: "Explicit shared drive"}
		return nil
	}))

	err := purgeShortcutChildArtifacts(
		context.Background(),
		shortcutChildArtifactScope{mountID: mountID},
		slog.New(slog.DiscardHandler),
	)

	require.NoError(t, err)
	assert.FileExists(t, statePath)
	catalog, err := config.LoadCatalog()
	require.NoError(t, err)
	assert.Contains(t, catalog.Drives, mountID)
}
