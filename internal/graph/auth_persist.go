package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"sync/atomic"

	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// ErrTokenNotSaved marks a refreshed sign-in that could not be written to its
// file. Requests go on with the token in memory; what is at risk is the next
// start, which reads the file and may find a refresh token the sign-in service
// has already replaced (R-6.8.26). Owners test for it with errors.Is.
var ErrTokenNotSaved = errors.New("couldn't save the refreshed sign-in")

// tokenSaver is the one writer of a sign-in file inside a token source: every
// token a refresh produces reaches the file through save, which the refresh
// calls holding the file's lock (refreshShared).
//
// Which sign-in is newer is decided by lineage, never by expiry (access-token
// lifetimes vary): the sign-in service answers a refresh token with its
// successor and refuses the one it replaced, so a token is newer than the
// file exactly when it descends from the refresh token the file holds. The
// saver remembers that refresh token (fileRefresh, as last read or written
// under the lock) and, for a token whose write failed, the refresh token the
// file held when this source's unsaved chain began (pendingBase). It writes a
// token only when the file still holds the start of its chain; a file that
// holds anything else was written by another process (a refresh or a login)
// and is newer, so the unsaved chain is dropped. A write that fails is kept:
// the token stays pending, its error is reported by saveError, and the write
// is tried again at the next token use (retry) or the next refresh. A write
// that succeeds clears it (R-6.8.26, R-6.8.30).
type tokenSaver struct {
	path   string
	logger *slog.Logger
	// write is tokenfile.Save; a test seam for a file system that refuses.
	write func(path string, tok *oauth2.Token) error

	// failing mirrors pending != nil, so a token use checks for a failed
	// write without taking mu.
	failing atomic.Bool

	// mu guards the fields below and is held across a write, so the
	// lineage check and the write it allows are one step. It is never held
	// while a refresh request is out.
	mu stdsync.Mutex
	// fileRefresh is the refresh token the file holds, as last read or
	// written under the file's lock.
	fileRefresh string
	// pending is the newest token of this source's unsaved chain; nil when
	// the file is up to date.
	pending *oauth2.Token
	// pendingBase is the refresh token the file held when the unsaved chain
	// began: the file is still the chain's predecessor while it holds it.
	pendingBase string
	// err is the failure of pending's last write.
	err error
}

func newTokenSaver(path string, onDisk *oauth2.Token, logger *slog.Logger) *tokenSaver {
	return &tokenSaver{
		path:        path,
		logger:      logger,
		write:       tokenfile.Save,
		fileRefresh: onDisk.RefreshToken,
	}
}

// fileIsOursLocked reports whether the file holds this source's sign-in or
// the start of its unsaved chain, given the refresh token held in memory;
// s.mu must be held. Otherwise another process wrote the file.
func (s *tokenSaver) fileIsOursLocked(heldRefresh string) bool {
	return s.fileRefresh == heldRefresh || (s.pending != nil && s.fileRefresh == s.pendingBase)
}

// fileIsOurs is fileIsOursLocked taking s.mu.
func (s *tokenSaver) fileIsOurs(heldRefresh string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.fileIsOursLocked(heldRefresh)
}

// save writes tok, which a refresh redeemed from the refresh token parent,
// while the file holds parent or the start of this source's unsaved chain.
// The caller holds the file's lock and has just read the file (observe).
func (s *tokenSaver) save(tok *oauth2.Token, parent string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.fileIsOursLocked(parent) {
		// Another process wrote the file meanwhile; its sign-in is newer.
		s.logger.Debug("refreshed sign-in not written: the file holds a newer one", slog.String("path", s.path))
		return
	}
	if s.pending == nil {
		s.pendingBase = s.fileRefresh
	}

	s.writeLocked(tok)
}

// retry writes the pending token again, if a write of it failed. It takes
// the file's lock, waiting no longer than ctx and signInLockWait allow, and
// writes only while the file still holds the start of the unsaved chain: a
// file that is gone was signed out, and one holding another sign-in is newer,
// and a retry must neither bring a sign-in back nor replace a newer one. A
// lock it cannot get leaves the token pending for the next use.
func (s *tokenSaver) retry(ctx context.Context) {
	if !s.failing.Load() {
		return
	}

	lockCtx, cancel := context.WithTimeout(ctx, signInLockWait)
	defer cancel()
	release, err := tokenfile.AcquireLock(lockCtx, s.path)
	if err != nil && !errors.Is(err, tokenfile.ErrLockUnsupported) {
		s.logger.Debug("refreshed sign-in not saved: lock not taken", slog.String("path", s.path), slog.Any("error", err))
		return
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			s.logger.Warn("couldn't release the sign-in lock", slog.String("path", s.path), slog.Any("error", releaseErr))
		}
	}()

	onDisk, loadErr := tokenfile.Load(s.path)
	switch {
	case errors.Is(loadErr, tokenfile.ErrNotFound):
		s.signedOut()
		return
	case loadErr != nil:
		s.logger.Debug("refreshed sign-in not saved: file unreadable", slog.String("path", s.path), slog.Any("error", loadErr))
		return
	}
	s.observe(onDisk)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending != nil {
		s.writeLocked(s.pending)
	}
}

// observe records the refresh token the file holds, read under its lock. A
// file that no longer holds the start of the unsaved chain was written by
// another process: the chain is superseded and dropped, its failure with it.
func (s *tokenSaver) observe(onDisk *oauth2.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fileRefresh = onDisk.RefreshToken
	if s.pending != nil && s.fileRefresh != s.pendingBase {
		s.clearPendingLocked()
	}
}

// signedOut drops the pending token of a file that was removed: the account
// was signed out, so nothing is left to save.
func (s *tokenSaver) signedOut() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clearPendingLocked()
}

func (s *tokenSaver) clearPendingLocked() {
	s.pending, s.pendingBase, s.err = nil, "", nil
	s.failing.Store(false)
}

// saveError is the failure of the newest token's last write, or nil once the
// file holds it.
func (s *tokenSaver) saveError() error {
	if !s.failing.Load() {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}

// writeLocked writes tok to the file; s.mu must be held. The error names the
// file and the cause, never the token.
func (s *tokenSaver) writeLocked(tok *oauth2.Token) {
	if err := s.write(s.path, tok); err != nil {
		s.pending = tok
		s.err = fmt.Errorf("%w to %s; the next run may ask you to sign in again: %w", ErrTokenNotSaved, s.path, err)
		s.failing.Store(true)
		// The owner reports it: a command when it ends, a watch on its
		// periodic tick. Logging every retry here would repeat it per request.
		s.logger.Debug("refreshed sign-in not saved", slog.String("path", s.path), slog.Any("error", err))

		return
	}

	s.fileRefresh = tok.RefreshToken
	s.clearPendingLocked()
	s.logger.Info("refreshed sign-in",
		slog.String("path", s.path),
		slog.Time("expiry", tok.Expiry),
	)
}

// tokenSaveReporter is the optional interface of a token source that writes
// refreshed sign-ins to a file. Sources that do not (test fakes) need not
// implement it.
type tokenSaveReporter interface {
	SaveError() error
}

// TokenSaveError is the failure of the last write of ts's newest refreshed
// sign-in (it wraps ErrTokenNotSaved), or nil when the file holds it or ts
// writes no file.
func TokenSaveError(ts TokenSource) error {
	reporter, ok := ts.(tokenSaveReporter)
	if !ok {
		return nil
	}

	//nolint:wrapcheck // the source's error already names the file and the cause
	return reporter.SaveError()
}
