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
	data, err := os.ReadFile(path)
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
func Save(path string, tok *oauth2.Token) error {
	if tok == nil {
		return fmt.Errorf("tokenfile: refusing to save nil token to %s", path)
	}

	tf := File{Token: tok}

	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return fmt.Errorf("tokenfile: encoding: %w", err)
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
			_ = os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, FilePerms); err != nil {
		tmp.Close()
		return fmt.Errorf("tokenfile: setting permissions: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("tokenfile: writing: %w", err)
	}

	// Flush to stable storage before rename so a power loss between close and
	// rename cannot leave an empty or partial token file at the final path.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("tokenfile: syncing: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tokenfile: closing: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("tokenfile: renaming: %w", err)
	}

	success = true

	return nil
}
