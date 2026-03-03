package driveops

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// ErrCorruptSession is returned when a session file cannot be parsed as JSON.
// The corrupt file is deleted automatically.
var ErrCorruptSession = errors.New("corrupt session file")

// sessionSubdir is the subdirectory within the data dir for upload session files.
const sessionSubdir = "upload-sessions"

// sessionFilePerms restricts session files to owner-only because they contain
// pre-authenticated upload URLs.
const sessionFilePerms = 0o600

// sessionDirPerms for the session directory itself.
const sessionDirPerms = 0o700

// StaleSessionAge is the default TTL for upload session files.
// Graph API sessions expire in ~48 hours; 7 days is generous.
const StaleSessionAge = 7 * 24 * time.Hour

// currentSessionVersion is the schema version written by Save().
// v0: unversioned (no "version" key), uses "remote_path".
// v1: added "version" field, still uses "remote_path".
// v2: renamed JSON key "remote_path" → "local_path" (B-300).
const currentSessionVersion = 2

// SessionRecord is the on-disk JSON format for a persisted upload session.
// Custom UnmarshalJSON reads both "remote_path" (v0/v1) and "local_path" (v2+).
type SessionRecord struct {
	Version    int       `json:"version"`
	DriveID    string    `json:"drive_id"`
	LocalPath  string    `json:"local_path"`
	SessionURL string    `json:"session_url"`
	FileHash   string    `json:"file_hash"`
	FileSize   int64     `json:"file_size"`
	CreatedAt  time.Time `json:"created_at"`
}

// UnmarshalJSON implements custom unmarshaling to support both the old
// "remote_path" key (v0/v1) and the new "local_path" key (v2+).
func (r *SessionRecord) UnmarshalJSON(data []byte) error {
	// Alias avoids infinite recursion — the alias type has no UnmarshalJSON method.
	type alias SessionRecord

	// Embed the alias and add the old key as a separate field.
	var raw struct {
		alias
		RemotePath string `json:"remote_path"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*r = SessionRecord(raw.alias)

	// If LocalPath is empty but RemotePath has a value, migrate from old key.
	if r.LocalPath == "" && raw.RemotePath != "" {
		r.LocalPath = raw.RemotePath
	}

	return nil
}

// SessionStore manages file-based upload session persistence. Session files
// are JSON files keyed by sha256(len(driveID):driveID:localPath), stored in a
// dedicated directory. Thread-safe for concurrent Save/Load/Delete.
type SessionStore struct {
	dir    string
	logger *slog.Logger
}

// NewSessionStore creates a SessionStore rooted at dataDir/upload-sessions.
func NewSessionStore(dataDir string, logger *slog.Logger) *SessionStore {
	return &SessionStore{
		dir:    filepath.Join(dataDir, sessionSubdir),
		logger: logger,
	}
}

// Load reads a session record for the given drive and local path.
// Returns nil, nil if no session file exists or if the session file is
// older than StaleSessionAge (Graph API upload sessions expire in ~48h).
func (s *SessionStore) Load(driveID, localPath string) (*SessionRecord, error) {
	path := s.filePath(driveID, localPath)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("stat session file: %w", err)
	}

	// Delete expired sessions eagerly so callers don't attempt a doomed resume.
	if time.Since(info.ModTime()) > StaleSessionAge {
		s.logger.Warn("session file expired, deleting",
			slog.String("path", path),
			slog.Duration("age", time.Since(info.ModTime()).Truncate(time.Hour)),
		)

		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			s.logger.Warn("failed to remove expired session file",
				slog.String("path", path),
				slog.String("error", rmErr.Error()),
			)
		}

		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading session file: %w", err)
	}

	var rec SessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// Corrupt file — delete and treat as absent.
		s.logger.Warn("corrupt session file, deleting",
			slog.String("path", path),
			slog.String("error", err.Error()),
		)

		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			s.logger.Warn("failed to remove corrupt session file",
				slog.String("path", path),
				slog.String("error", rmErr.Error()),
			)
		}

		return nil, fmt.Errorf("%w: %w", ErrCorruptSession, err)
	}

	return &rec, nil
}

// Save persists a session record. Creates the session directory if needed.
func (s *SessionStore) Save(driveID, localPath string, rec *SessionRecord) error {
	if err := os.MkdirAll(s.dir, sessionDirPerms); err != nil {
		return fmt.Errorf("creating session dir: %w", err)
	}

	rec.Version = currentSessionVersion
	rec.DriveID = driveID
	rec.LocalPath = localPath

	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshaling session record: %w", err)
	}

	path := s.filePath(driveID, localPath)
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, data, sessionFilePerms); err != nil {
		return fmt.Errorf("writing session temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // best-effort cleanup
		return fmt.Errorf("renaming session temp file: %w", err)
	}

	return nil
}

// Delete removes the session file for the given drive and local path.
// No error if the file doesn't exist.
func (s *SessionStore) Delete(driveID, localPath string) error {
	path := s.filePath(driveID, localPath)

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting session file: %w", err)
	}

	return nil
}

// CleanStale removes session files older than maxAge. Returns the number
// of files deleted. Safe to call concurrently.
func (s *SessionStore) CleanStale(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}

		return 0, fmt.Errorf("reading session dir: %w", err)
	}

	cutoff := time.Now().Add(-maxAge)
	deleted := 0

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			path := filepath.Join(s.dir, e.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				s.logger.Warn("failed to clean stale session",
					slog.String("file", e.Name()),
					slog.String("error", err.Error()),
				)

				continue
			}

			s.logger.Info("deleted stale upload session",
				slog.String("file", e.Name()),
				slog.Duration("age", time.Since(info.ModTime())),
			)

			deleted++
		}
	}

	return deleted, nil
}

// sessionKey produces a deterministic filename for a (driveID, localPath) pair.
// Uses length-prefixed driveID to prevent hash collisions from delimiter ambiguity
// (e.g., driveID="a:", localPath="b" vs driveID="a", localPath=":b").
func sessionKey(driveID, localPath string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d:%s:%s", len(driveID), driveID, localPath))
	return fmt.Sprintf("%x.json", h)
}

// filePath returns the absolute path to the session file for the given key.
func (s *SessionStore) filePath(driveID, localPath string) string {
	return filepath.Join(s.dir, sessionKey(driveID, localPath))
}
