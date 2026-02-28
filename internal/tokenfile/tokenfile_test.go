package tokenfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestLoad_FileNotFound(t *testing.T) {
	tok, meta, err := Load("/nonexistent/path/token.json")
	assert.Nil(t, tok)
	assert.Nil(t, meta)
	assert.NoError(t, err)
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
	meta := map[string]string{"org_name": "Contoso", "display_name": "Alice"}

	require.NoError(t, Save(path, original, meta))

	tok, loadedMeta, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "access-123", tok.AccessToken)
	assert.Equal(t, "refresh-456", tok.RefreshToken)
	assert.Equal(t, "Bearer", tok.TokenType)
	assert.True(t, tok.Expiry.Equal(expiry))
	assert.Equal(t, "Contoso", loadedMeta["org_name"])
	assert.Equal(t, "Alice", loadedMeta["display_name"])
}

func TestLoad_MissingTokenField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Write a file with no "token" field (old format).
	require.NoError(t, os.WriteFile(path, []byte(`{"access_token":"old","refresh_token":"old"}`), 0o600))

	tok, meta, err := Load(path)
	assert.Nil(t, tok)
	assert.Nil(t, meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing token field")
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, os.WriteFile(path, []byte(`{not json}`), 0o600))

	tok, meta, err := Load(path)
	assert.Nil(t, tok)
	assert.Nil(t, meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

func TestLoad_NilMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, nil))

	tok, meta, err := Load(path)
	require.NoError(t, err)
	assert.NotNil(t, tok)
	assert.Nil(t, meta)
}

func TestReadMeta_FileNotFound(t *testing.T) {
	meta, err := ReadMeta("/nonexistent/path/token.json")
	assert.Nil(t, meta)
	assert.NoError(t, err)
}

func TestReadMeta_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, map[string]string{"org_name": "ACME", "display_name": "Bob"}))

	meta, err := ReadMeta(path)
	require.NoError(t, err)
	assert.Equal(t, "ACME", meta["org_name"])
	assert.Equal(t, "Bob", meta["display_name"])
}

func TestReadMeta_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, os.WriteFile(path, []byte(`{corrupt`), 0o600))

	meta, err := ReadMeta(path)
	assert.Nil(t, meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

func TestSave_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "dir", "token.json")

	err := Save(nested, &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, nil)
	require.NoError(t, err)

	info, err := os.Stat(nested)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(FilePerms), info.Mode().Perm())
}

func TestSave_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, nil))

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
	meta := map[string]string{"key": "value"}

	require.NoError(t, Save(path, original, meta))

	tok, loadedMeta, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, original.AccessToken, tok.AccessToken)
	assert.Equal(t, original.RefreshToken, tok.RefreshToken)
	assert.True(t, tok.Expiry.Equal(expiry))
	assert.Equal(t, "value", loadedMeta["key"])
}

func TestLoad_EmptyCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Write a token file with the wrapper but empty credentials.
	require.NoError(t, os.WriteFile(path, []byte(`{"token":{"token_type":"Bearer"}}`), 0o600))

	tok, meta, err := Load(path)
	assert.Nil(t, tok)
	assert.Nil(t, meta)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty credentials")
}

func TestSave_NilToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	err := Save(path, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to save nil token")
}

func TestLoadAndMergeMeta_MergesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, map[string]string{"org_name": "Old", "display_name": "Alice"}))

	require.NoError(t, LoadAndMergeMeta(path, map[string]string{
		"org_name": "New",
		"user_id":  "abc123",
	}))

	meta, err := ReadMeta(path)
	require.NoError(t, err)
	assert.Equal(t, "New", meta["org_name"])
	assert.Equal(t, "Alice", meta["display_name"])
	assert.Equal(t, "abc123", meta["user_id"])
}

func TestLoadAndMergeMeta_FileNotFound(t *testing.T) {
	err := LoadAndMergeMeta("/nonexistent/path/token.json", map[string]string{"k": "v"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no token file")
}

func TestLoadAndMergeMeta_NilExistingMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, &oauth2.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}, nil))

	require.NoError(t, LoadAndMergeMeta(path, map[string]string{"key": "value"}))

	meta, err := ReadMeta(path)
	require.NoError(t, err)
	assert.Equal(t, "value", meta["key"])
}
