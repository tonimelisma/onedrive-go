package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// signInLockWait bounds how long a refresh waits for another process's
// refresh of the same sign-in file. It is longer than the longest sign-in
// request (graphtransport.AuthClient's Timeout, 60 s), so a holder stuck in
// its request releases the lock before a waiter gives up.
const signInLockWait = 90 * time.Second

// adoptedSignInLog is the one Info line of a refresh that found a newer
// sign-in in the file, written by another process, and used it.
const adoptedSignInLog = "found a newer sign-in in the file and used it"

// lockWaitFor is the lock wait of a refresh ctx starts: signInLockWait, or
// what is left of ctx's deadline when that is shorter.
func (b *tokenBridge) lockWaitFor(ctx context.Context) time.Duration {
	wait := signInLockWait
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, deadline.Sub(b.now()))
	}

	return wait
}

// refreshShared is one refresh of held, coordinated with every other process
// using the sign-in file through its lock (R-6.8.30). Under the lock it reads
// the file: a file that is gone means the account was signed out, so the
// refresh is "sign-in required", makes no request and writes nothing. A file
// that holds neither held's refresh token nor the start of this source's
// unsaved chain (tokenSaver's lineage rule) was written by another process:
// its access token is used when still valid (and, for a refresh a 401 forced,
// when it is not the rejected one), and otherwise its refresh token is the
// one redeemed. Otherwise held's refresh token is redeemed, so a sign-in
// whose save keeps failing stays signed in on the token in memory. The
// answer is saved under the same lock. A refresh token the
// sign-in service refuses is checked against the file once more, for a
// writer that did not take the lock or a filesystem that cannot lock.
func (b *tokenBridge) refreshShared(
	ctx context.Context, lockWait time.Duration, held *oauth2.Token, forced bool,
) (*oauth2.Token, error) {
	lockCtx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()

	release, err := tokenfile.AcquireLock(lockCtx, b.saver.path)
	if err != nil {
		if !errors.Is(err, tokenfile.ErrLockUnsupported) {
			return nil, fmt.Errorf("refreshing access token: %w", err)
		}
		b.lockUnsupported.Do(func() {
			b.logger.Warn("can't lock the sign-in file, so another onedrive-go process using it could undo this one's refresh",
				slog.String("path", b.saver.path), slog.Any("error", err))
		})
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			b.logger.Warn("couldn't release the sign-in lock", slog.String("path", b.saver.path), slog.Any("error", releaseErr))
		}
	}()

	onDisk, err := b.readSignIn()
	if err != nil {
		return nil, err
	}

	// The file holds this source's sign-in, or the start of its unsaved
	// chain, which the token in memory replaced: redeem the one in memory.
	refreshToken := held.RefreshToken
	if !b.saver.fileIsOurs(held.RefreshToken) {
		// Another process wrote the file, so its sign-in is the newest.
		if onDisk.Valid() && (!forced || onDisk.AccessToken != held.AccessToken) {
			b.logger.Info(adoptedSignInLog, slog.String("path", b.saver.path), slog.Time("expiry", onDisk.Expiry))
			return onDisk, nil
		}
		refreshToken = onDisk.RefreshToken
	}

	tok, err := b.requestToken(ctx, refreshToken)
	if isRefreshTokenRefused(err) {
		again, readErr := b.readSignIn()
		if readErr == nil && again.RefreshToken != refreshToken {
			b.logger.Debug("refresh token refused; redeeming the one the sign-in file now holds")
			refreshToken = again.RefreshToken
			tok, err = b.requestToken(ctx, refreshToken)
		}
	}
	if err != nil {
		return nil, err
	}

	b.saver.save(tok, refreshToken)

	return tok, nil
}

// readSignIn reads the sign-in file and tells the saver what it holds. A
// file that is gone is "sign-in required": the account was signed out.
func (b *tokenBridge) readSignIn() (*oauth2.Token, error) {
	onDisk, err := tokenfile.Load(b.saver.path)
	if errors.Is(err, tokenfile.ErrNotFound) {
		b.saver.signedOut()
		return nil, fmt.Errorf("refreshing access token: %w: %w", ErrNotLoggedIn, ErrUnauthorized)
	}
	if err != nil {
		return nil, fmt.Errorf("refreshing access token: %w", err)
	}

	b.saver.observe(onDisk)

	return onDisk, nil
}

// isRefreshTokenRefused reports the sign-in service refusing the refresh
// token itself (invalid_grant), the answer to one another process replaced.
func isRefreshTokenRefused(err error) bool {
	var retrieveErr *oauth2.RetrieveError

	return errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == oauthErrInvalidGrant
}
