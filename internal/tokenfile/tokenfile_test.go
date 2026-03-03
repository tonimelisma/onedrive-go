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

// completeMeta returns a metadata map with all required keys populated.
func completeMeta() map[string]string {
	return map[string]string{
		"drive_id":     "test-drive-id",
		"user_id":      "test-user-id",
		"display_name": "Test User",
		"org_name":     "TestOrg",
		"cached_at":    "2024-01-01T00:00:00Z",
	}
}

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
	meta := completeMeta()
	meta["org_name"] = "Contoso"
	meta["display_name"] = "Alice"

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

	// nil meta is allowed on Save (login flow).
	require.NoError(t, Save(path, testToken(), nil))

	tok, meta, err := Load(path)
	require.NoError(t, err)
	assert.NotNil(t, tok)
	assert.Nil(t, meta)
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

// --- ReadMeta tests ---

func TestReadMeta_FileNotFound(t *testing.T) {
	meta, err := ReadMeta("/nonexistent/path/token.json")
	assert.Nil(t, meta)
	assert.NoError(t, err)
}

func TestReadMeta_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	meta := completeMeta()
	meta["org_name"] = "ACME"
	meta["display_name"] = "Bob"
	require.NoError(t, Save(path, testToken(), meta))

	loaded, err := ReadMeta(path)
	require.NoError(t, err)
	assert.Equal(t, "ACME", loaded["org_name"])
	assert.Equal(t, "Bob", loaded["display_name"])
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

// --- ValidateMeta tests ---

func TestValidateMeta_AllPresent(t *testing.T) {
	t.Parallel()

	err := ValidateMeta(completeMeta())
	assert.NoError(t, err)
}

func TestValidateMeta_MissingKeys(t *testing.T) {
	t.Parallel()

	// Missing drive_id and cached_at.
	meta := map[string]string{
		"user_id":      "u1",
		"display_name": "Alice",
	}

	err := ValidateMeta(meta)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive_id")
	assert.Contains(t, err.Error(), "cached_at")
	assert.Contains(t, err.Error(), "re-login required")
}

func TestValidateMeta_NilMeta(t *testing.T) {
	t.Parallel()

	err := ValidateMeta(nil)
	require.Error(t, err)
	// All required keys are missing.
	for _, key := range RequiredMetaKeys {
		assert.Contains(t, err.Error(), key)
	}
}

func TestValidateMeta_EmptyValues(t *testing.T) {
	t.Parallel()

	// Keys present but empty values.
	meta := map[string]string{
		"drive_id":     "",
		"user_id":      "u1",
		"display_name": "Alice",
		"cached_at":    "",
	}

	err := ValidateMeta(meta)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive_id")
	assert.Contains(t, err.Error(), "cached_at")
}

func TestValidateMeta_OrgNameNotRequired(t *testing.T) {
	t.Parallel()

	// org_name is intentionally excluded from RequiredMetaKeys
	// because personal accounts have empty org_name.
	meta := completeMeta()
	delete(meta, "org_name")

	assert.NoError(t, ValidateMeta(meta))
}

// --- LoadAndValidate tests ---

func TestLoadAndValidate_CompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, testToken(), completeMeta()))

	tok, meta, err := LoadAndValidate(path)
	require.NoError(t, err)
	assert.NotNil(t, tok)
	assert.Equal(t, "test-drive-id", meta["drive_id"])
}

func TestLoadAndValidate_FileNotFound(t *testing.T) {
	tok, meta, err := LoadAndValidate("/nonexistent/path/token.json")
	assert.Nil(t, tok)
	assert.Nil(t, meta)
	assert.NoError(t, err)
}

func TestLoadAndValidate_MissingRefreshToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Write directly to bypass Save validation — simulates a corrupt file on disk.
	tok := &oauth2.Token{AccessToken: "access-only", TokenType: "Bearer"}
	writeTokenFile(t, path, tok, completeMeta())

	_, _, err := LoadAndValidate(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no refresh token")
	assert.Contains(t, err.Error(), "re-login required")
}

func TestLoadAndValidate_NilMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Save with nil meta (login flow), then try LoadAndValidate.
	require.NoError(t, Save(path, testToken(), nil))

	_, _, err := LoadAndValidate(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no metadata")
	assert.Contains(t, err.Error(), "re-login required")
}

func TestLoadAndValidate_IncompleteMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Write a file with token + incomplete meta directly (bypass Save validation).
	incompleteMeta := map[string]string{"display_name": "Alice"}
	writeTokenFile(t, path, testToken(), incompleteMeta)

	_, _, err := LoadAndValidate(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive_id")
	assert.Contains(t, err.Error(), "re-login required")
}

// --- Save tests ---

func TestSave_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "dir", "token.json")

	// nil meta is allowed (login flow).
	err := Save(nested, testToken(), nil)
	require.NoError(t, err)

	info, err := os.Stat(nested)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(FilePerms), info.Mode().Perm())
}

func TestSave_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, testToken(), nil))

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
	meta := completeMeta()

	require.NoError(t, Save(path, original, meta))

	tok, loadedMeta, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, original.AccessToken, tok.AccessToken)
	assert.Equal(t, original.RefreshToken, tok.RefreshToken)
	assert.True(t, tok.Expiry.Equal(expiry))
	assert.Equal(t, "test-drive-id", loadedMeta["drive_id"])
}

func TestSave_NilToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	err := Save(path, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to save nil token")
}

func TestSave_AllowsNilMeta(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// nil meta is allowed during initial login (exchangeAndSave).
	err := Save(path, testToken(), nil)
	assert.NoError(t, err)
}

func TestSave_RejectsIncompleteMeta(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Non-nil meta with missing required keys should be rejected.
	incompleteMeta := map[string]string{"display_name": "Alice"}
	err := Save(path, testToken(), incompleteMeta)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to save incomplete token")
	assert.Contains(t, err.Error(), "drive_id")

	// File should NOT have been created.
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}

// --- LoadAndMergeMeta tests ---

func TestLoadAndMergeMeta_MergesKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	require.NoError(t, Save(path, testToken(), completeMeta()))

	require.NoError(t, LoadAndMergeMeta(path, map[string]string{
		"org_name": "NewOrg",
	}))

	meta, err := ReadMeta(path)
	require.NoError(t, err)
	assert.Equal(t, "NewOrg", meta["org_name"])
	assert.Equal(t, "Test User", meta["display_name"])
	assert.Equal(t, "test-drive-id", meta["drive_id"])
}

func TestLoadAndMergeMeta_FileNotFound(t *testing.T) {
	err := LoadAndMergeMeta("/nonexistent/path/token.json", map[string]string{"k": "v"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no token file")
}

func TestLoadAndMergeMeta_NilExistingMeta_CompletesMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Save with nil meta (login flow).
	require.NoError(t, Save(path, testToken(), nil))

	// Merge in all required keys (simulates login step 5b).
	require.NoError(t, LoadAndMergeMeta(path, completeMeta()))

	meta, err := ReadMeta(path)
	require.NoError(t, err)
	assert.Equal(t, "test-drive-id", meta["drive_id"])
	assert.Equal(t, "test-user-id", meta["user_id"])
}

func TestLoadAndMergeMeta_ValidatesAfterMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Save with nil meta.
	require.NoError(t, Save(path, testToken(), nil))

	// Merge incomplete metadata — should fail validation.
	err := LoadAndMergeMeta(path, map[string]string{"display_name": "Alice"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata incomplete after merge")
	assert.Contains(t, err.Error(), "drive_id")
}

// --- test helpers ---

// writeTokenFile writes a token file directly, bypassing Save validation.
// Used to create intentionally corrupt files for testing.
func writeTokenFile(t *testing.T, path string, tok *oauth2.Token, meta map[string]string) {
	t.Helper()

	tf := File{Token: tok, Meta: meta}

	data, err := json.MarshalIndent(tf, "", "  ")
	require.NoError(t, err)

	dir := filepath.Dir(path)
	require.NoError(t, os.MkdirAll(dir, DirPerms))
	require.NoError(t, os.WriteFile(path, data, FilePerms))
}
