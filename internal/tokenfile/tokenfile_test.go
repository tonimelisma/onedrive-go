package tokenfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func testToken() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  "access-test",
		RefreshToken: "refresh-test",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}
}

// --- Load tests ---

func TestLoad_FileNotFound(t *testing.T) {
	tok, err := Load("/nonexistent/path/token.json")
	assert.Nil(t, tok)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestLoad_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	expiry := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	original := &oauth2.Token{
		AccessToken:  "access-123",
		RefreshToken: "refresh-456",
		TokenType:    "Bearer",
		Expiry:       expiry,
	}

	require.NoError(t, Save(path, original))

	tok, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "access-123", tok.AccessToken)
	assert.Equal(t, "refresh-456", tok.RefreshToken)
	assert.Equal(t, "Bearer", tok.TokenType)
	assert.True(t, tok.Expiry.Equal(expiry))
}

func TestLoad_BareTokenRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Write a bare oauth2.Token without the "token" wrapper — rejected as
	// unknown fields.
	require.NoError(t, os.WriteFile(path, []byte(`{"access_token":"old","refresh_token":"old"}`), 0o600))

	tok, err := Load(path)
	assert.Nil(t, tok)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, os.WriteFile(path, []byte(`{not json}`), 0o600))

	tok, err := Load(path)
	assert.Nil(t, tok)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

func TestLoad_EmptyCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Write a token file with the wrapper but empty credentials.
	require.NoError(t, os.WriteFile(path, []byte(`{"token":{"token_type":"Bearer"}}`), 0o600))

	tok, err := Load(path)
	assert.Nil(t, tok)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty credentials")
}

func TestLoad_RejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// JSON with extra unknown keys alongside the token. Load should reject
	// the file — token files must contain only the "token" key.
	content := `{
		"token": {
			"access_token": "access-extra",
			"refresh_token": "refresh-extra",
			"token_type": "Bearer"
		},
		"extra_field": {
			"key": "value"
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	tok, err := Load(path)
	assert.Nil(t, tok)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

// --- Save tests ---

func TestSave_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "dir", "token.json")

	err := Save(nested, testToken())
	require.NoError(t, err)

	info, err := os.Stat(nested)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(FilePerms), info.Mode().Perm())
}

func TestSave_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, testToken()))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(FilePerms), info.Mode().Perm())
}

func TestSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	expiry := time.Date(2099, 6, 15, 12, 0, 0, 0, time.UTC)
	original := &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		TokenType:    "Bearer",
		Expiry:       expiry,
	}

	require.NoError(t, Save(path, original))

	tok, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, original.AccessToken, tok.AccessToken)
	assert.Equal(t, original.RefreshToken, tok.RefreshToken)
	assert.True(t, tok.Expiry.Equal(expiry))
}

func TestSave_NilToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	err := Save(path, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to save nil token")
}

func TestSave_NoMetaInOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, testToken()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	// Verify no "meta" key in the output JSON.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	_, hasMeta := raw["meta"]
	assert.False(t, hasMeta, "saved token file should not contain a meta key")
}
