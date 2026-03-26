package reviewgate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (fn roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// Validates: R-6.3.6
func TestClientListChangedFiles(t *testing.T) {
	t.Run("rename metadata is preserved and complete counts pass", func(t *testing.T) {
		client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			assert.Equal(t, "1", req.URL.Query().Get("page"))

			return jsonResponse(t, []map[string]string{
				{
					"filename":          "spec/design/ci-review-gate.md",
					"previous_filename": "spec/design/old-review-gate.md",
					"status":            "renamed",
				},
			}), nil
		}), "https://api.github.com", "token")

		changedFiles, err := client.ListChangedFiles(context.Background(), "tonimelisma/onedrive-go", 326, 1)

		require.NoError(t, err)
		assert.True(t, changedFiles.Complete)
		assert.Equal(t, ChangedFiles{
			Entries: []ChangedFile{
				{
					Path:         "spec/design/ci-review-gate.md",
					PreviousPath: "spec/design/old-review-gate.md",
					Status:       ChangedFileStatusRenamed,
				},
			},
			Complete: true,
		}, changedFiles)
	})

	t.Run("count mismatch fails closed", func(t *testing.T) {
		client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(t, []map[string]string{
				{
					"filename": "spec/design/ci-review-gate.md",
					"status":   "modified",
				},
			}), nil
		}), "https://api.github.com", "token")

		changedFiles, err := client.ListChangedFiles(context.Background(), "tonimelisma/onedrive-go", 326, 2)

		require.NoError(t, err)
		assert.False(t, changedFiles.Complete)
		assert.Len(t, changedFiles.Entries, 1)
	})

	t.Run("unknown expected count fails closed", func(t *testing.T) {
		client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonResponse(t, []map[string]string{
				{
					"filename": "spec/design/ci-review-gate.md",
					"status":   "modified",
				},
			}), nil
		}), "https://api.github.com", "token")

		changedFiles, err := client.ListChangedFiles(context.Background(), "tonimelisma/onedrive-go", 326, 0)

		require.NoError(t, err)
		assert.False(t, changedFiles.Complete)
		assert.Len(t, changedFiles.Entries, 1)
	})

	t.Run("api cap fails closed", func(t *testing.T) {
		client := NewClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			page := req.URL.Query().Get("page")
			require.NotEmpty(t, page)

			return jsonResponse(t, hundredChangedFiles("spec/design/page-"+page+"-")), nil
		}), "https://api.github.com", "token")

		changedFiles, err := client.ListChangedFiles(context.Background(), "tonimelisma/onedrive-go", 326, 3500)

		require.NoError(t, err)
		assert.False(t, changedFiles.Complete)
		assert.Len(t, changedFiles.Entries, githubFilesAPICap)
	})
}

func jsonResponse(t *testing.T, payload any) *http.Response {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err)

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     make(http.Header),
	}
}

func hundredChangedFiles(prefix string) []map[string]string {
	files := make([]map[string]string, 0, githubPageSize)
	for index := 0; index < githubPageSize; index++ {
		files = append(files, map[string]string{
			"filename": prefix + string(rune('a'+(index%26))) + "-file.md",
			"status":   "modified",
		})
	}

	return files
}
