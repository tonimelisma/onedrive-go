package driveops

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"
)

// ErrInvalidSessionRecord is returned when a session file holds anything but
// a record in the one format Save writes: not JSON, a field the record does
// not have, data after it, or another version. Load has already tried to
// remove the file and warned once, with the removal's failure if it failed;
// either way the upload starts over from a fresh session.
var ErrInvalidSessionRecord = errors.New("invalid upload session record")

// sessionSubdir is the subdirectory within the data dir for upload session files.
const sessionSubdir = "upload-sessions"

// sessionRecordSuffix ends every record file's name (sessionKey), and
// sessionTempPattern names the temporary file a Save writes first. The
// pattern must never end in sessionRecordSuffix: scope cleanup reads only
// record files, so it never touches a Save in progress.
const (
	sessionRecordSuffix = ".json"
	sessionTempPattern  = ".session-*.tmp"
)

// sessionFilePerms restricts session files to owner-only because they contain
// pre-authenticated upload URLs.
const sessionFilePerms = 0o600

// sessionDirPerms for the session directory itself.
const sessionDirPerms = 0o700

// StaleSessionAge is the default TTL for upload session files.
// Graph API sessions expire in ~48 hours; 7 days is generous.
const StaleSessionAge = 7 * 24 * time.Hour

// currentSessionVersion is the record version Save writes and the only one
// Load reads.
const currentSessionVersion = 2

// SessionRecord is the on-disk JSON format for a persisted upload session.
type SessionRecord struct {
	Version    int       `json:"version"`
	DriveID    string    `json:"drive_id"`
	LocalPath  string    `json:"local_path"`
	MountID    string    `json:"mount_id,omitempty"`
	LocalRoot  string    `json:"local_root,omitempty"`
	SessionURL string    `json:"session_url"`
	FileHash   string    `json:"file_hash"`
	FileSize   int64     `json:"file_size"`
	CreatedAt  time.Time `json:"created_at"`
	// ExpiresAt is the session's expirationDateTime. A session that is gone
	// before it expired was invalidated, not expired (L-02). Zero when
	// Graph's answer carried no expiration.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// ItemID and CreatedETag name the item a create's session writes: the
	// empty item the create made, and its eTag then. A resume, an adoption
	// lookup and a later overwrite by ID use them instead of a name lookup.
	ItemID      string `json:"item_id,omitempty"`
	CreatedETag string `json:"created_etag,omitempty"`
}

// decodeSessionRecord reads data as a record in the one format Save writes:
// a single JSON object of known fields at the current version. Anything else
// is an error; the client reads no format it does not write.
func decodeSessionRecord(data []byte) (*SessionRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var rec SessionRecord
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("decoding session record: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decoding session record: data after the record")
	}
	if rec.Version != currentSessionVersion {
		return nil, fmt.Errorf("session record version %d, want %d", rec.Version, currentSessionVersion)
	}

	return &rec, nil
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

// Load reads a session record for the given drive and local path. The returned
// found flag is false when no session file exists or when the session file is
// older than StaleSessionAge (Graph API upload sessions expire in ~48h). A file
// that holds anything but a record in the format Save writes is removed, with
// one warning, and reported as ErrInvalidSessionRecord.
func (s *SessionStore) Load(driveID, localPath string) (*SessionRecord, bool, error) {
	path := s.filePath(driveID, localPath)
	root, err := fsroot.Open(s.dir)
	if err != nil {
		return nil, false, fmt.Errorf("opening session root: %w", err)
	}
	name := filepath.Base(path)

	info, err := root.Stat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("stat session file: %w", err)
	}

	// Delete expired sessions eagerly so callers don't attempt a doomed resume.
	if time.Since(info.ModTime()) > StaleSessionAge {
		// Debug only: expected housekeeping with nothing for the user to do.
		s.logger.Debug("session file expired, deleting",
			slog.String("path", path),
			slog.Duration("age", time.Since(info.ModTime()).Truncate(time.Hour)),
		)

		if rmErr := root.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			s.logger.Warn("failed to remove expired session file",
				slog.String("path", path),
				slog.Any("error", rmErr),
			)
		}

		return nil, false, nil
	}

	data, err := root.ReadFile(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("reading session file: %w", err)
	}

	rec, err := decodeSessionRecord(data)
	if err != nil {
		// The caller logs ErrInvalidSessionRecord at Debug only: this warning
		// is the one report of it.
		s.warnInvalidRecordOfUpload(path, localPath, err, removeInvalidRecord(root, name))

		return nil, false, fmt.Errorf("%w: %w", ErrInvalidSessionRecord, err)
	}

	return rec, true, nil
}

// warnInvalidRecordOfUpload reports, once, that the session file at path held
// no record Load can read (reason says why), so the upload of localPath (the
// key Load was given: the local path, with the item in front for an
// overwrite) starts over from a fresh session. rmErr is the failure to remove
// the file, if any. The warning carries the decode error and never the
// file's content, which may hold a pre-authenticated upload URL.
func (s *SessionStore) warnInvalidRecordOfUpload(path, localPath string, reason, rmErr error) {
	if rmErr != nil {
		s.logger.Warn("Upload session file is not a valid record and could not be removed; the upload starts over",
			slog.String("path", path),
			slog.String("local_path", localPath),
			slog.Any("reason", reason),
			slog.Any("error", rmErr),
		)

		return
	}

	s.logger.Warn("Removed an upload session file that is not a valid record; the upload starts over",
		slog.String("path", path),
		slog.String("local_path", localPath),
		slog.Any("reason", reason),
	)
}

// removeInvalidRecord deletes the session file name, which holds no record
// the store can read. Only Save writes these files, so such a file never names
// a session this binary can resume. A file already gone is not a failure.
func removeInvalidRecord(root *fsroot.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing invalid session file %s: %w", name, err)
	}

	return nil
}

// Save persists a session record. Creates the session directory if needed.
func (s *SessionStore) Save(driveID, localPath string, rec *SessionRecord) error {
	root, err := fsroot.Open(s.dir)
	if err != nil {
		return fmt.Errorf("opening session root: %w", err)
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

	name := filepath.Base(s.filePath(driveID, localPath))
	if err := root.AtomicWrite(name, data, sessionFilePerms, sessionDirPerms, sessionTempPattern); err != nil {
		return fmt.Errorf("writing session file: %w", err)
	}

	return nil
}

// Delete removes the session file for the given drive and local path.
// No error if the file doesn't exist.
func (s *SessionStore) Delete(driveID, localPath string) error {
	root, err := fsroot.Open(s.dir)
	if err != nil {
		return fmt.Errorf("opening session root: %w", err)
	}

	if err := root.Remove(filepath.Base(s.filePath(driveID, localPath))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting session file: %w", err)
	}

	return nil
}

// DeleteForScope removes upload sessions owned by a mount scope and returns
// how many it removed, with every file it could not read or remove, joined,
// for its caller to report once. MountID is exact; LocalRoot also matches
// records rooted below that path so child cleanup catches records written
// while a shortcut root was temporarily nested. A file that holds no readable
// record is removed too, whatever scope it was written for, and is not
// counted.
func (s *SessionStore) DeleteForScope(mountID, localRoot string) (int, error) {
	mountID = strings.TrimSpace(mountID)
	localRoot = strings.TrimSpace(localRoot)
	if mountID == "" && localRoot == "" {
		return 0, nil
	}

	root, err := fsroot.Open(s.dir)
	if err != nil {
		return 0, fmt.Errorf("opening session root: %w", err)
	}

	entries, err := root.ReadDir("")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading session dir: %w", err)
	}

	var errs []error
	deleted := 0
	for _, entry := range entries {
		// Only record files: a Save in progress writes a temporary file
		// first, and that is no record to decode or remove.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), sessionRecordSuffix) {
			continue
		}
		removed, err := s.deleteRecordForScope(root, entry.Name(), mountID, localRoot)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if removed {
			deleted++
		}
	}

	return deleted, errors.Join(errs...)
}

// deleteRecordForScope removes the record file name when it belongs to the
// scope, and reports whether it removed a record of the scope. A file that
// holds no readable record can never be resumed, whatever scope it was
// written for, so it is removed with one warning naming the file and the
// reason, and is not counted; a file it cannot read or remove is the error.
// A file gone since the listing is neither.
func (s *SessionStore) deleteRecordForScope(root *fsroot.Root, name, mountID, localRoot string) (bool, error) {
	data, err := root.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading session file %s: %w", name, err)
	}

	rec, err := decodeSessionRecord(data)
	if err != nil {
		if rmErr := removeInvalidRecord(root, name); rmErr != nil {
			return false, rmErr
		}
		s.logger.Warn("Removed an upload session file that is not a valid record",
			slog.String("path", filepath.Join(s.dir, name)),
			slog.Any("reason", err),
		)

		return false, nil
	}
	if !sessionRecordMatchesScope(rec, mountID, localRoot) {
		return false, nil
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("deleting session file %s: %w", name, err)
	}

	return true, nil
}

func sessionRecordMatchesScope(rec *SessionRecord, mountID, localRoot string) bool {
	if rec == nil {
		return false
	}
	if mountID != "" && rec.MountID == mountID {
		return true
	}
	return localRootContains(localRoot, rec.LocalRoot)
}

func localRootContains(root, candidate string) bool {
	root = strings.TrimSpace(root)
	candidate = strings.TrimSpace(candidate)
	if root == "" || candidate == "" {
		return false
	}
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CleanStale removes session files older than maxAge. It returns the number
// of files deleted and every file it could not check or delete, joined. Safe
// to call concurrently.
func (s *SessionStore) CleanStale(maxAge time.Duration) (int, error) {
	root, err := fsroot.Open(s.dir)
	if err != nil {
		return 0, fmt.Errorf("opening session root: %w", err)
	}

	entries, err := root.ReadDir("")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, fmt.Errorf("reading session dir: %w", err)
	}

	return s.removeStaleEntries(root, entries, time.Now().Add(-maxAge))
}

// removeStaleEntries removes each session file among entries last written
// before cutoff and returns how many it removed. It goes on past a failure and
// returns every one joined, as DeleteForScope does, for the caller to report
// once; a file gone since the listing is not a failure.
func (s *SessionStore) removeStaleEntries(root *fsroot.Root, entries []os.DirEntry, cutoff time.Time) (int, error) {
	var errs []error
	deleted := 0

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		info, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("stating session file %s: %w", e.Name(), err))
			continue
		}

		if !info.ModTime().Before(cutoff) {
			continue
		}

		if err := root.Remove(e.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("deleting stale session file %s: %w", e.Name(), err))
			continue
		}

		s.logger.Debug("deleted stale upload session",
			slog.String("file", e.Name()),
			slog.Duration("age", time.Since(info.ModTime()).Truncate(time.Hour)),
		)

		deleted++
	}

	return deleted, errors.Join(errs...)
}

// sessionKey produces a deterministic filename for a (driveID, localPath) pair.
// Uses length-prefixed driveID to prevent hash collisions from delimiter ambiguity
// (e.g., driveID="a:", localPath="b" vs driveID="a", localPath=":b").
func sessionKey(driveID, localPath string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d:%s:%s", len(driveID), driveID, localPath))
	return fmt.Sprintf("%x", h) + sessionRecordSuffix
}

// filePath returns the absolute path to the session file for the given key.
func (s *SessionStore) filePath(driveID, localPath string) string {
	return filepath.Join(s.dir, sessionKey(driveID, localPath))
}
