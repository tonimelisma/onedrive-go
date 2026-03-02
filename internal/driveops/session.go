package driveops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	gosync "sync"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Session holds authenticated clients and the resolved drive identity for a
// single drive. Wraps a pair of graph.Client instances: Meta (30s timeout)
// for metadata operations and Transfer (no timeout) for uploads/downloads.
type Session struct {
	Meta     *graph.Client // metadata ops (30s timeout)
	Transfer *graph.Client // uploads/downloads (no timeout)
	DriveID  driveid.ID
	Resolved *config.ResolvedDrive
}

// SessionProvider caches TokenSources by token file path and creates Sessions
// on demand. Multiple drives sharing a token path share one TokenSource,
// preventing OAuth2 refresh token rotation races (two independent refreshes
// can invalidate each other's refresh tokens).
type SessionProvider struct {
	holder       *config.Holder
	metaHTTP     *http.Client
	transferHTTP *http.Client
	userAgent    string
	logger       *slog.Logger

	// TokenSourceFn creates a TokenSource from a token file path. Exported
	// for test injection; defaults to graph.TokenSourceFromPath.
	TokenSourceFn func(ctx context.Context, tokenPath string, logger *slog.Logger) (graph.TokenSource, error)

	mu         gosync.Mutex
	tokenCache map[string]graph.TokenSource // keyed by token file path
}

// NewSessionProvider creates a SessionProvider with default TokenSourceFn.
func NewSessionProvider(
	holder *config.Holder, metaHTTP, transferHTTP *http.Client,
	userAgent string, logger *slog.Logger,
) *SessionProvider {
	return &SessionProvider{
		holder:        holder,
		metaHTTP:      metaHTTP,
		transferHTTP:  transferHTTP,
		userAgent:     userAgent,
		logger:        logger,
		TokenSourceFn: graph.TokenSourceFromPath,
		tokenCache:    make(map[string]graph.TokenSource),
	}
}

// Session creates or retrieves an authenticated Session for the given
// resolved drive. Token caching ensures drives sharing a token path
// reuse the same TokenSource.
func (p *SessionProvider) Session(ctx context.Context, rd *config.ResolvedDrive) (*Session, error) {
	cfg := p.holder.Config()

	tokenPath := config.DriveTokenPath(rd.CanonicalID, cfg)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for drive %q", rd.CanonicalID)
	}

	ts, err := p.getOrCreateTokenSource(ctx, tokenPath)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in — run 'onedrive-go login' first: %w", err)
		}

		return nil, err
	}

	if rd.DriveID.IsZero() {
		return nil, fmt.Errorf("drive ID not resolved for %s — re-run 'onedrive-go login'", rd.CanonicalID)
	}

	meta := graph.NewClient(graph.DefaultBaseURL, p.metaHTTP, ts, p.logger, p.userAgent)
	transfer := graph.NewClient(graph.DefaultBaseURL, p.transferHTTP, ts, p.logger, p.userAgent)

	p.logger.Debug("session created",
		slog.String("drive_id", rd.DriveID.String()),
		slog.String("canonical_id", rd.CanonicalID.String()),
	)

	return &Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  rd.DriveID,
		Resolved: rd,
	}, nil
}

// getOrCreateTokenSource returns a cached TokenSource for the given token
// path, creating one on cache miss. Thread-safe via mutex.
func (p *SessionProvider) getOrCreateTokenSource(ctx context.Context, tokenPath string) (graph.TokenSource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ts, ok := p.tokenCache[tokenPath]; ok {
		return ts, nil
	}

	ts, err := p.TokenSourceFn(ctx, tokenPath, p.logger)
	if err != nil {
		return nil, err
	}

	p.tokenCache[tokenPath] = ts

	return ts, nil
}

// ResolveItem resolves a remote path to an Item. For root (""), uses GetItem
// with "root". Otherwise uses GetItemByPath. "/" normalizes to "" via
// CleanRemotePath, so callers can pass either "/" or "" to mean root.
func (s *Session) ResolveItem(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		return s.Meta.GetItem(ctx, s.DriveID, "root")
	}

	return s.Meta.GetItemByPath(ctx, s.DriveID, clean)
}

// ListChildren lists children of a remote path. For root (""), uses
// ListChildren with "root". Otherwise uses ListChildrenByPath.
func (s *Session) ListChildren(ctx context.Context, remotePath string) ([]graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		return s.Meta.ListChildren(ctx, s.DriveID, "root")
	}

	return s.Meta.ListChildrenByPath(ctx, s.DriveID, clean)
}

// CleanRemotePath strips leading/trailing slashes, returns "" for root.
func CleanRemotePath(path string) string {
	return strings.Trim(path, "/")
}
