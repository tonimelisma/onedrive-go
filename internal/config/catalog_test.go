package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCatalog_RejectsMissingSchemaVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := CatalogPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"accounts":{},"drives":{}}`), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 0")
}

func TestLoadCatalog_RejectsUnsupportedSchemaVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := CatalogPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":2,"accounts":{},"drives":{}}`), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 2")
}

func TestLoadCatalog_RejectsUnknownFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := CatalogPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":1,"accounts":{},"drives":{},"unexpected":true}`), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestSaveCatalog_WritesSchemaVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	require.NoError(t, SaveCatalog(&Catalog{}))
	loaded, err := LoadCatalog()
	require.NoError(t, err)
	assert.Equal(t, catalogSchemaV1, loaded.SchemaVersion)
}

func TestLoadCatalog_RejectsTrailingJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path := CatalogPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{\"schema_version\":1,\"accounts\":{},\"drives\":{}}\n{}"), 0o600))

	_, err := LoadCatalog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailing data after top-level object")
}
