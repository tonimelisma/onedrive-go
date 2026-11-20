package sync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func readObservationCursor(t *testing.T, db *sql.DB, driveID string) string {
	t.Helper()

	var token string
	err := db.QueryRowContext(t.Context(),
		`SELECT cursor FROM observation_state WHERE content_drive_id = ? LIMIT 1`,
		driveID,
	).Scan(&token)
	if err == sql.ErrNoRows {
		return ""
	}

	require.NoError(t, err)
	return token
}
