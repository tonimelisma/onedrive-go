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

	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
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
	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, fmt.Errorf("tokenfile: opening token root: %w", err)
	}

	data, err := root.ReadFile(name)
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

	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("tokenfile: opening managed root: %w", err)
	}

	if err := root.MkdirAll(DirPerms); err != nil {
		return fmt.Errorf("tokenfile: creating directory: %w", err)
	}

	if err := root.AtomicWrite(name, data, FilePerms, DirPerms, ".token-*.tmp"); err != nil {
		return fmt.Errorf("tokenfile: writing token file: %w", err)
	}

	return nil
}
