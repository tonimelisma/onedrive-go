package config

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderEffective_DefaultProfile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "/home/user/OneDrive",
		},
	}
	resolved, err := ResolveProfile(cfg, "default")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = RenderEffective(resolved, &buf)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, `profile "default"`)
	assert.Contains(t, output, "account_type")
	assert.Contains(t, output, `"personal"`)
	assert.Contains(t, output, "sync_dir")
	assert.Contains(t, output, "[filter]")
	assert.Contains(t, output, "[transfers]")
	assert.Contains(t, output, "[safety]")
	assert.Contains(t, output, "[sync]")
	assert.Contains(t, output, "[logging]")
	assert.Contains(t, output, "[network]")
}

func TestRenderEffective_OptionalFieldsShown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"work": {
			AccountType:     "business",
			SyncDir:         "/home/user/Work",
			DriveID:         "b!abc123",
			AzureADEndpoint: "USL4",
			AzureTenantID:   "contoso",
		},
	}
	resolved, err := ResolveProfile(cfg, "work")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = RenderEffective(resolved, &buf)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "drive_id")
	assert.Contains(t, output, "azure_ad_endpoint")
	assert.Contains(t, output, "azure_tenant_id")
}

func TestRenderEffective_FilterListsShown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Filter.SkipFiles = []string{"*.tmp", "*.swp"}
	cfg.Filter.SkipDirs = []string{"node_modules"}
	cfg.Filter.SyncPaths = []string{"/Documents"}
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "/home/user/OneDrive",
		},
	}
	resolved, err := ResolveProfile(cfg, "default")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = RenderEffective(resolved, &buf)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "skip_files")
	assert.Contains(t, output, "*.tmp")
	assert.Contains(t, output, "skip_dirs")
	assert.Contains(t, output, "node_modules")
	assert.Contains(t, output, "sync_paths")
}

func TestRenderEffective_LogFileShown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Logging.LogFile = "/var/log/onedrive-go.log"
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "/home/user/OneDrive",
		},
	}
	resolved, err := ResolveProfile(cfg, "default")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = RenderEffective(resolved, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "log_file")
}

func TestRenderEffective_UserAgentShown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Network.UserAgent = "ISV|test|test/v0.1.0"
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "/home/user/OneDrive",
		},
	}
	resolved, err := ResolveProfile(cfg, "default")
	require.NoError(t, err)

	var buf bytes.Buffer
	err = RenderEffective(resolved, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "user_agent")
}

// failWriter is a writer that always fails, used to exercise error paths
// in the errWriter pattern.
type failWriter struct{}

var errWriteFailed = errors.New("write failed")

func (failWriter) Write([]byte) (int, error) {
	return 0, errWriteFailed
}

func TestRenderEffective_WriteError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Profiles = map[string]Profile{
		"default": {
			AccountType: "personal",
			SyncDir:     "/home/user/OneDrive",
		},
	}
	resolved, err := ResolveProfile(cfg, "default")
	require.NoError(t, err)

	err = RenderEffective(resolved, failWriter{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errWriteFailed)
}

func TestJoinQuoted(t *testing.T) {
	assert.Equal(t, `"a", "b", "c"`, joinQuoted([]string{"a", "b", "c"}))
	assert.Equal(t, `"single"`, joinQuoted([]string{"single"}))
	assert.Equal(t, "", joinQuoted(nil))
}
