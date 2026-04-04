package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

// Validates: R-3.6.4, R-3.6.6, R-3.6.7
func TestSharedService_RunList_JSON(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/drive/search(q='*')":
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"value": [
					{
						"id": "local-shortcut-1",
						"name": "Shared Folder",
						"size": 0,
						"createdDateTime": "2024-01-01T00:00:00Z",
						"lastModifiedDateTime": "2024-06-01T00:00:00Z",
						"folder": {"childCount": 3},
						"remoteItem": {
							"id": "source-item-folder",
							"parentReference": {"driveId": "b!drive1234567890"}
						}
					},
					{
						"id": "local-shortcut-2",
						"name": "shared-file.docx",
						"size": 2048,
						"createdDateTime": "2024-02-01T00:00:00Z",
						"lastModifiedDateTime": "2024-05-01T00:00:00Z",
						"file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
						"remoteItem": {
							"id": "source-item-file",
							"parentReference": {"driveId": "b!drive1234567891"}
						}
					}
				]
			}`)
		case "/drives/b!drive1234567890/items/source-item-folder":
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id": "source-item-folder",
				"name": "Shared Folder",
				"size": 0,
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-06-01T00:00:00Z",
				"folder": {"childCount": 3},
				"parentReference": {"id": "parent", "driveId": "b!drive1234567890"},
				"shared": {"owner": {"user": {"email": "alice@example.com", "displayName": "Alice"}}}
			}`)
		case "/drives/b!drive1234567891/items/source-item-file":
			w.Header().Set("Content-Type", "application/json")
			writeTestResponse(t, w, `{
				"id": "source-item-file",
				"name": "shared-file.docx",
				"size": 2048,
				"createdDateTime": "2024-02-01T00:00:00Z",
				"lastModifiedDateTime": "2024-05-01T00:00:00Z",
				"parentReference": {"id": "parent", "driveId": "b!drive1234567891"},
				"file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
				"shared": {"owner": {"user": {"email": "bob@example.com", "displayName": "Bob"}}}
			}`)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      config.DefaultConfigPath(),
		GraphBaseURL: srv.URL,
	}

	err := newSharedService(cc).runList(context.Background())
	require.NoError(t, err)

	var parsed sharedListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &parsed))
	require.Len(t, parsed.Items, 2)
	assert.Empty(t, parsed.AccountsRequiringAuth)

	assert.Equal(t, "shared:user@example.com:b!drive1234567891:source-item-file", parsed.Items[0].Selector)
	assert.Equal(t, "file", parsed.Items[0].Type)
	assert.Equal(t, "bob@example.com", parsed.Items[0].SharedByEmail)

	assert.Equal(t, "shared:user@example.com:b!drive1234567890:source-item-folder", parsed.Items[1].Selector)
	assert.Equal(t, "folder", parsed.Items[1].Type)
	assert.Equal(t, "alice@example.com", parsed.Items[1].SharedByEmail)
}

// Validates: R-3.3.12
func TestDriveService_RunAdd_RejectsSharedFileSelector(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/b!drive1234567891/items/source-item-file", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{
			"id": "source-item-file",
			"name": "shared-file.docx",
			"size": 2048,
			"createdDateTime": "2024-02-01T00:00:00Z",
			"lastModifiedDateTime": "2024-05-01T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "b!drive1234567891"},
			"file": {"mimeType": "application/pdf"}
		}`)
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      config.DefaultConfigPath(),
		GraphBaseURL: srv.URL,
	}

	err := newDriveService(cc).runAdd(context.Background(), []string{"shared:user@example.com:b!drive1234567891:source-item-file"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared files are direct stat/get/put targets")
}

// Validates: R-1.6.2
func TestRunStat_SharedSelector_JSON(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/b!drive1234567891/items/source-item-file", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{
			"id": "source-item-file",
			"name": "shared-file.docx",
			"size": 2048,
			"createdDateTime": "2024-02-01T00:00:00Z",
			"lastModifiedDateTime": "2024-05-01T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "b!drive1234567891"},
			"file": {"mimeType": "application/pdf"},
			"eTag": "etag-1"
		}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      config.DefaultConfigPath(),
		GraphBaseURL: srv.URL,
		SharedTarget: &sharedTarget{
			Ref: sharedref.MustParse("shared:user@example.com:b!drive1234567891:source-item-file"),
		},
	}

	cmd := newStatCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))
	cmd.SetArgs([]string{"shared:user@example.com:b!drive1234567891:source-item-file"})

	require.NoError(t, cmd.Execute())

	var parsed statJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &parsed))
	assert.Equal(t, "shared:user@example.com:b!drive1234567891:source-item-file", parsed.SharedSelector)
	assert.Equal(t, "user@example.com", parsed.AccountEmail)
	assert.Equal(t, "b!drive1234567891", parsed.RemoteDriveID)
	assert.Equal(t, "source-item-file", parsed.RemoteItemID)
}

// Validates: R-1.3.5
func TestRunPut_SharedFolderTargetRejected(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	localFile := createTempFile(t, "upload.txt", "hello")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/b!drive1234567890/items/source-item-folder", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{
			"id": "source-item-folder",
			"name": "Shared Folder",
			"size": 0,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-06-01T00:00:00Z",
			"folder": {"childCount": 3},
			"parentReference": {"id": "parent", "driveId": "b!drive1234567890"}
		}`)
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      config.DefaultConfigPath(),
		GraphBaseURL: srv.URL,
		SharedTarget: &sharedTarget{
			Ref: sharedref.MustParse("shared:user@example.com:b!drive1234567890:source-item-folder"),
		},
	}

	cmd := newPutCmd()
	cmd.SetContext(context.WithValue(t.Context(), cliContextKey{}, cc))
	err := runPut(cmd, []string{localFile, "shared:user@example.com:b!drive1234567890:source-item-folder"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run 'onedrive-go drive add shared:user@example.com:b!drive1234567890:source-item-folder' first")
}

func createTempFile(t *testing.T, name, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
