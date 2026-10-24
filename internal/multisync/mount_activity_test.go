package multisync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-6.6.18
func TestMountActivityHook_TagsActivityWithItsMount(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cfg := testStandaloneMount(t, "personal:owner@example.com", "Personal")
	mount, err := buildStandaloneMountSpec(&cfg)
	require.NoError(t, err)

	var got []MountActivity
	hook := mountActivityHook(mount, func(ma MountActivity) { got = append(got, ma) })
	hook(syncengine.Activity{Kind: syncengine.ActivityUpload, Path: "a.txt", Success: true})

	require.Len(t, got, 1)
	assert.Equal(t, "Personal", got[0].DisplayName)
	assert.Equal(t, cfg.SyncRoot, got[0].SyncRoot)
	assert.Equal(t, mount.identity(), got[0].Identity)
	assert.Equal(t, "a.txt", got[0].Activity.Path)
}
