package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestDegradedDiscoveryLogAttrs_NonQuirkErrorsStayPlain(t *testing.T) {
	t.Parallel()

	attrs := degradedDiscoveryLogAttrs("user@example.com", "/me/drives", graph.ErrForbidden)

	assert.Equal(t, []any{
		"account", "user@example.com",
		"endpoint", "/me/drives",
		"error", graph.ErrForbidden,
	}, attrs)
}

func TestDriveCatalogDegradationLogging_UsesSameStructuredEvidenceAcrossCallers(t *testing.T) {
	t.Parallel()

	quirkErr := &graph.QuirkRetryError{
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
	}

	var whoamiLog bytes.Buffer
	whoamiLogger := slog.New(slog.NewJSONHandler(&whoamiLog, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_ = whoamiDrives(
		t.Context(),
		fakeWhoamiDriveClient{
			drivesErr: quirkErr,
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
		whoamiLogger,
	)

	var driveListLog bytes.Buffer
	driveListLogger := slog.New(slog.NewJSONHandler(&driveListLog, &slog.HandlerOptions{Level: slog.LevelDebug}))
	entries, authRequired, degraded := discoverAccessibleDrives(
		context.Background(),
		fakeAccessibleDriveClient{
			drivesErr: quirkErr,
			primary: &graph.Drive{
				ID:        driveid.New("drive-primary"),
				Name:      "OneDrive",
				DriveType: driveid.DriveTypePersonal,
			},
		},
		config.DefaultConfig(),
		nil,
		driveid.MustCanonicalID("personal:user@example.com"),
		driveListLogger,
	)
	require.Len(t, entries, 1)
	assert.Empty(t, authRequired)
	require.Len(t, degraded, 1)

	whoamiRecord := decodeLastCLIJSONLog(t, whoamiLog.Bytes())
	driveListRecord := decodeLastCLIJSONLog(t, driveListLog.Bytes())

	assert.Equal(t, "user@example.com", whoamiRecord["account"])
	assert.Equal(t, "user@example.com", driveListRecord["account"])
	assert.Equal(t, "/me/drives", whoamiRecord["endpoint"])
	assert.Equal(t, "/me/drives", driveListRecord["endpoint"])
	assert.Equal(t, "drives-token-propagation", whoamiRecord["graph_quirk"])
	assert.Equal(t, whoamiRecord["graph_quirk"], driveListRecord["graph_quirk"])
	assert.EqualValues(t, 2, whoamiRecord["graph_quirk_attempt_count"])
	assert.Equal(t, whoamiRecord["graph_quirk_attempt_count"], driveListRecord["graph_quirk_attempt_count"])

	whoamiAttempts, ok := whoamiRecord["graph_quirk_attempts"].([]any)
	require.True(t, ok)
	driveListAttempts, ok := driveListRecord["graph_quirk_attempts"].([]any)
	require.True(t, ok)
	require.Len(t, whoamiAttempts, 2)
	require.Len(t, driveListAttempts, 2)
	assert.Equal(t, whoamiAttempts, driveListAttempts)
}

func decodeLastCLIJSONLog(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	require.NotEmpty(t, lines)

	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(line, &decoded))
		return decoded
	}

	require.FailNow(t, "no JSON log lines decoded")
	return nil
}
