package driveops

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

// cleanThrottle prevents excessive directory scans. CleanStale is
// a no-op if called again within this interval.
const cleanThrottle = 1 * time.Hour

// SessionRecord is the on-disk JSON format for a persisted upload session.
type SessionRecord struct {
	DriveID    string    `json:"drive_id"`
	LocalPath  string    `json:"remote_path"` // JSON key kept as "remote_path" for backward compatibility
	SessionURL string    `json:"session_url"`
	FileHash   string    `json:"file_hash"`
	FileSize   int64     `json:"file_size"`
	CreatedAt  time.Time `json:"created_at"`
}

// SessionStore manages file-based upload session persistence. Session files
// are JSON files keyed by sha256(len(driveID):driveID:localPath), stored in a
// dedicated directory. Thread-safe for concurrent Save/Load/Delete.
type SessionStore struct {
	dir    string
	logger *slog.Logger

	cleanMu   sync.Mutex
	lastClean time.Time
}

// NewSessionStore creates a SessionStore rooted at dataDir/upload-sessions.
func NewSessionStore(dataDir string, logger *slog.Logger) *SessionStore {
	return &SessionStore{
		dir:    filepath.Join(dataDir, sessionSubdir),
		logger: logger,
	}
}

// Load reads a session record for the given drive and local path.
// Returns nil, nil if no session file exists.
func (s *SessionStore) Load(driveID, localPath string) (*SessionRecord, error) {
	path := s.filePath(driveID, localPath)

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
// Triggers lazy stale-session cleanup (throttled to once per hour).
func (s *SessionStore) Save(driveID, localPath string, rec *SessionRecord) error {
	if err := os.MkdirAll(s.dir, sessionDirPerms); err != nil {
		return fmt.Errorf("creating session dir: %w", err)
	}

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

	// Lazy cleanup — non-blocking, errors logged but not propagated.
	// Pre-check throttle to avoid spawning a goroutine on every Save.
	s.cleanMu.Lock()
	due := time.Since(s.lastClean) >= cleanThrottle
	s.cleanMu.Unlock()

	if due {
		go s.cleanIfDue()
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

// cleanIfDue runs CleanStale if at least cleanThrottle has elapsed since
// the last run. Thread-safe; no-op if throttled. Runs in a goroutine so
// panic recovery prevents crashing the entire process.
func (s *SessionStore) cleanIfDue() {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in session cleanup", slog.Any("panic", r))
		}
	}()

	s.cleanMu.Lock()
	if time.Since(s.lastClean) < cleanThrottle {
		s.cleanMu.Unlock()
		return
	}

	s.lastClean = time.Now()
	s.cleanMu.Unlock()

	n, err := s.CleanStale(StaleSessionAge)
	if err != nil {
		s.logger.Warn("stale session cleanup failed", slog.String("error", err.Error()))
		return
	}

	if n > 0 {
		s.logger.Info("cleaned stale upload sessions", slog.Int("count", n))
	}
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
