package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func TestResolveSharedTargetBootstrap_SelectorSkipsConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cc := &CLIContext{
		Flags:        CLIFlags{},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      config.DefaultConfigPath(),
	}

	target, found, err := resolveSharedTargetBootstrap(
		context.Background(),
		newStatCmd(),
		[]string{"shared:alice@example.com:b!drive1234567890:01DEF456"},
		cc,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, target)

	assert.Equal(t, "shared:alice@example.com:b!drive1234567890:01DEF456", target.Selector())
	assert.Equal(t, "alice@example.com", target.Ref.AccountEmail)
	assert.Equal(t, "b!drive1234567890", target.Ref.RemoteDriveID)
	assert.Equal(t, "01DEF456", target.Ref.RemoteItemID)
}

func TestResolveSharedTargetBootstrap_RawURLRequiresAccountWithMultipleLogins(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	writeTestTokenFile(t, dataDir, "token_personal_a@example.com.json")
	writeTestTokenFile(t, dataDir, "token_personal_b@example.com.json")

	cc := &CLIContext{
		Flags:        CLIFlags{},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      config.DefaultConfigPath(),
	}

	target, found, err := resolveSharedTargetBootstrap(
		context.Background(),
		newStatCmd(),
		[]string{"https://1drv.ms/t/c/example/abcdef"},
		cc,
	)
	require.Error(t, err)
	assert.False(t, found)
	assert.Nil(t, target)
	assert.Contains(t, err.Error(), "use --account")
}

// Validates: R-1.2.5, R-1.3.5, R-1.6.2
func TestResolveSharedTargetBootstrap_RawURLResolvesToSelector(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	rawURL := "https://1drv.ms/t/c/example/abcdef?e=xyz"
	expectedToken := "u!" + base64.RawURLEncoding.EncodeToString([]byte(rawURL))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/shares/"+expectedToken+"/driveItem", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{
			"id": "owner-item-123",
			"name": "Shared Document.docx",
			"size": 42,
			"createdDateTime": "2026-04-03T00:00:00Z",
			"lastModifiedDateTime": "2026-04-03T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "b!drive1234567890"},
			"file": {"mimeType": "application/pdf"}
		}`)
	}))
	defer srv.Close()

	cc := &CLIContext{
		Flags:        CLIFlags{},
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      config.DefaultConfigPath(),
		GraphBaseURL: srv.URL,
	}

	target, found, err := resolveSharedTargetBootstrap(
		context.Background(),
		newStatCmd(),
		[]string{rawURL},
		cc,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, target)

	assert.Equal(t, "shared:user@example.com:b!drive1234567890:owner-item-123", target.Selector())
}

func TestSharedTargetInput(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
		args []string
		want string
		ok   bool
	}{
		{
			name: "stat selector",
			cmd:  newStatCmd(),
			args: []string{"shared:alice@example.com:drv:item"},
			want: "shared:alice@example.com:drv:item",
			ok:   true,
		},
		{
			name: "get raw url",
			cmd:  newGetCmd(),
			args: []string{"https://1drv.ms/t/c/example"},
			want: "https://1drv.ms/t/c/example",
			ok:   true,
		},
		{
			name: "put second arg only",
			cmd:  newPutCmd(),
			args: []string{"./local.txt", "shared:alice@example.com:drv:item"},
			want: "shared:alice@example.com:drv:item",
			ok:   true,
		},
		{
			name: "put default path mode",
			cmd:  newPutCmd(),
			args: []string{"./local.txt"},
			ok:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sharedTargetInput(tc.cmd, tc.args)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
