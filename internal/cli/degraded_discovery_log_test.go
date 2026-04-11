package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestWhoamiDrives_LogsQuirkRetryEvidenceWhenDriveDiscoveryDegrades(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	result := whoamiDrives(
		t.Context(),
		fakeWhoamiDriveClient{
			drivesErr: &graph.QuirkRetryError{
				Quirk: "drives-token-propagation",
				Attempts: []graph.QuirkRetryAttempt{
					{Attempt: 1, StatusCode: http.StatusForbidden, GraphCode: "accessDenied", RequestID: "req-1"},
					{Attempt: 2, StatusCode: http.StatusForbidden, GraphCode: "accessDenied", RequestID: "req-2"},
				},
				Err: &graph.GraphError{
					StatusCode: http.StatusForbidden,
					RequestID:  "req-2",
					Code:       "accessDenied",
					Message:    "Forbidden",
					Err:        graph.ErrForbidden,
				},
			},
			primary: &graph.Drive{
				ID:        driveid.New("drive-primary"),
				Name:      "OneDrive",
				DriveType: driveid.DriveTypePersonal,
			},
		},
		accountAuthRequirement{
			Email:     "user@example.com",
			DriveType: driveid.DriveTypePersonal,
		},
		&graph.User{
			Email:       "user@example.com",
			DisplayName: "Test User",
		},
		logger,
	)

	require.Nil(t, result.authResult)
	require.Len(t, result.degraded, 1)
	assert.Contains(t, logBuf.String(), "\"graph_quirk\":\"drives-token-propagation\"")
	assert.Contains(t, logBuf.String(), "\"graph_quirk_attempt_count\":2")
	assert.Contains(t, logBuf.String(), "\"graphCode\":\"accessDenied\"")
	assert.Contains(t, logBuf.String(), "\"requestId\":\"req-1\"")
}

func TestDiscoverAccessibleDrives_LogsQuirkRetryEvidenceWhenDriveDiscoveryDegrades(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	entries, authRequired, degraded := discoverAccessibleDrives(
		context.Background(),
		fakeAccessibleDriveClient{
			drivesErr: &graph.QuirkRetryError{
				Quirk: "drives-token-propagation",
				Attempts: []graph.QuirkRetryAttempt{
					{Attempt: 1, StatusCode: http.StatusForbidden, GraphCode: "accessDenied", RequestID: "req-1"},
					{Attempt: 2, StatusCode: http.StatusForbidden, GraphCode: "accessDenied", RequestID: "req-2"},
				},
				Err: &graph.GraphError{
					StatusCode: http.StatusForbidden,
					RequestID:  "req-2",
					Code:       "accessDenied",
					Message:    "Forbidden",
					Err:        graph.ErrForbidden,
				},
			},
			primary: &graph.Drive{
				ID:        driveid.New("drive-primary"),
				Name:      "OneDrive",
				DriveType: driveid.DriveTypePersonal,
			},
		},
		config.DefaultConfig(),
		nil,
		driveid.MustCanonicalID("personal:user@example.com"),
		logger,
	)

	require.Empty(t, authRequired)
	require.Len(t, entries, 1)
	require.Len(t, degraded, 1)
	assert.Contains(t, logBuf.String(), "\"graph_quirk\":\"drives-token-propagation\"")
	assert.Contains(t, logBuf.String(), "\"graph_quirk_attempt_count\":2")
	assert.Contains(t, logBuf.String(), "\"graphCode\":\"accessDenied\"")
	assert.Contains(t, logBuf.String(), "\"requestId\":\"req-2\"")
}
