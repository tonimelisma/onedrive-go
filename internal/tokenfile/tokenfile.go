// Package tokenfile handles reading and writing token files. Token files store
// an OAuth2 token alongside cached API metadata (org name, display name, etc.).
// This is a leaf package imported by both config/ and graph/ to avoid duplication
// and break the config→graph import cycle.
package tokenfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
)

// FilePerms restricts token files to owner-only read/write.
const FilePerms = 0o600

// DirPerms is used when creating the tokens directory.
const DirPerms = 0o700

// File is the on-disk format for token files. Includes the OAuth token and
// optional metadata (org name, display name) cached from API responses.
// Old bare oauth2.Token files are not supported — re-login is required.
type File struct {
	Token *oauth2.Token     `json:"token"`
	Meta  map[string]string `json:"meta,omitempty"`
}

// Load reads a saved token file from disk. Returns the OAuth token and any
// cached metadata. Returns (nil, nil, nil) if the file does not exist.
// Old bare oauth2.Token files (without the "token" wrapper) will fail with
// "missing token field" — re-login is required.
func Load(path string) (*oauth2.Token, map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil //nolint:nilnil // sentinel for "not found"
	}

	if err != nil {
		return nil, nil, fmt.Errorf("tokenfile: reading %s: %w", path, err)
	}

	var tf File
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, nil, fmt.Errorf("tokenfile: decoding %s: %w", path, err)
	}

	if tf.Token == nil {
		return nil, nil, fmt.Errorf("tokenfile: %s missing token field (re-login required)", path)
	}

	return tf.Token, tf.Meta, nil
}

// ReadMeta reads just the metadata from a token file without loading the full
// OAuth token. Returns (nil, nil) if the file does not exist.
func ReadMeta(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // sentinel for "not found"
	}

	if err != nil {
		return nil, fmt.Errorf("tokenfile: reading %s: %w", path, err)
	}

	var parsed struct {
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("tokenfile: decoding %s: %w", path, err)
	}

	return parsed.Meta, nil
}

// Save writes a token file to disk atomically (write-to-temp + rename)
// with 0600 permissions. Never logs token values.
func Save(path string, tok *oauth2.Token, meta map[string]string) error {
	tf := File{Token: tok, Meta: meta}

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

// LoadAndMergeMeta reads the current token file, merges new metadata keys
// (new keys overwrite existing), and saves. Returns an error if the file
// does not exist or has no token.
func LoadAndMergeMeta(path string, meta map[string]string) error {
	tok, existingMeta, err := Load(path)
	if err != nil {
		return fmt.Errorf("reading token for metadata update: %w", err)
	}

	if tok == nil {
		return fmt.Errorf("no token file at %s", path)
	}

	if existingMeta == nil {
		existingMeta = make(map[string]string, len(meta))
	}

	maps.Copy(existingMeta, meta)

	return Save(path, tok, existingMeta)
}
