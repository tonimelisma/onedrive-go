// Package tokenfile handles reading and writing OAuth2 token files. Token
// files store a pure OAuth2 token — no metadata. This is a leaf package
// imported by both config/ and graph/ to avoid duplication and break the
// config→graph import cycle.
package tokenfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
)

// ErrNotFound is returned by Load when the token file does not exist.
var ErrNotFound = errors.New("token file not found")

// FilePerms restricts token files to owner-only read/write.
const FilePerms = 0o600

// DirPerms is used when creating the tokens directory.
const DirPerms = 0o700

// File is the on-disk format for token files. Contains only the OAuth token.
// Old bare oauth2.Token files are not supported — re-login is required.
type File struct {
	Token *oauth2.Token `json:"token"`
}

// Load reads a saved token file from disk. Returns the OAuth token.
// Returns ErrNotFound if the file does not exist.
// Old bare oauth2.Token files (without the "token" wrapper) will fail with
// "missing token field" — re-login is required.
func Load(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Token path comes from config-managed drive identity resolution.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("tokenfile: reading %s: %w", path, err)
	}

	var tf File

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&tf); err != nil {
		return nil, fmt.Errorf("tokenfile: decoding %s: %w", path, err)
	}

	if tf.Token == nil {
		return nil, fmt.Errorf("tokenfile: %s missing token field (re-login required)", path)
	}

	if tf.Token.AccessToken == "" && tf.Token.RefreshToken == "" {
		return nil, fmt.Errorf("tokenfile: %s has empty credentials (re-login required)", path)
	}

	return tf.Token, nil
}

// Save writes a token file to disk atomically (write-to-temp + rename)
// with 0600 permissions. Never logs token values. Writes only the OAuth
// token — no metadata.
func Save(path string, tok *oauth2.Token) (err error) {
	data, err := marshalTokenFile(tok)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, DirPerms); mkErr != nil {
		return fmt.Errorf("tokenfile: creating directory %s: %w", dir, mkErr)
	}

	// Atomic write: temp file in the same directory, then rename.
	// Same directory guarantees same filesystem for rename(2).
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("tokenfile: creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	// Clean up temp file on any error path.
	success := false
	defer func() {
		if !success {
			err = removeTempPath(tmpPath, err)
		}
	}()

	if err := setTempFilePermissions(tmp); err != nil {
		return closeTempFile(tmp, err)
	}

	if err := writeTempFileData(tmp, data); err != nil {
		return closeTempFile(tmp, err)
	}

	// Flush to stable storage before rename so a power loss between close and
	// rename cannot leave an empty or partial token file at the final path.
	if err := syncTempFile(tmp); err != nil {
		return closeTempFile(tmp, err)
	}

	if err := closeTempFile(tmp, nil); err != nil {
		return err
	}

	if err := renameTempFile(tmpPath, path); err != nil {
		return err
	}

	success = true

	return nil
}
