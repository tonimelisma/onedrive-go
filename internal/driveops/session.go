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

// ResolvedDriveEmail returns the account email for the resolved drive.
func (s *Session) ResolvedDriveEmail() string {
	if s == nil || s.Resolved == nil {
		return ""
	}

	return s.Resolved.CanonicalID.Email()
}

// SetAuthenticatedSuccessHooks installs the same best-effort success hook on
// both authenticated Graph clients owned by the session. Pre-authenticated
// transfer URLs bypass these hooks inside graph.Client.
func (s *Session) SetAuthenticatedSuccessHooks(hook func(context.Context)) {
	if s == nil {
		return
	}

	if s.Meta != nil {
		s.Meta.SetAuthenticatedSuccessHook(hook)
	}

	if s.Transfer != nil {
		s.Transfer.SetAuthenticatedSuccessHook(hook)
	}
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
	tokenPath := config.DriveTokenPath(rd.CanonicalID)
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
		return nil, fmt.Errorf(
			"drive ID not resolved for %s — token file may be missing or corrupted; re-run 'onedrive-go login'",
			rd.CanonicalID,
		)
	}

	meta, err := graph.NewClient(graph.DefaultBaseURL, p.metaHTTP, ts, p.logger, p.userAgent)
	if err != nil {
		return nil, fmt.Errorf("create metadata graph client: %w", err)
	}

	transfer, err := graph.NewClient(graph.DefaultBaseURL, p.transferHTTP, ts, p.logger, p.userAgent)
	if err != nil {
		return nil, fmt.Errorf("create transfer graph client: %w", err)
	}

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

// FlushTokenCache clears the cached TokenSources, forcing the next Session()
// call to re-read token files from disk. Called during daemon SIGHUP reload
// to pick up logout/re-login credential changes.
func (p *SessionProvider) FlushTokenCache() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.tokenCache = make(map[string]graph.TokenSource)
	p.logger.Info("token cache flushed")
}

// ResolveItem resolves a remote path to an Item. For root (""), uses GetItem
// with "root". Otherwise uses GetItemByPath. "/" normalizes to "" via
// CleanRemotePath, so callers can pass either "/" or "" to mean root.
func (s *Session) ResolveItem(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		item, err := s.Meta.GetItem(ctx, s.DriveID, "root")
		if err != nil {
			return nil, fmt.Errorf("resolve root item: %w", err)
		}

		return item, nil
	}

	item, err := s.Meta.GetItemByPath(ctx, s.DriveID, clean)
	if err != nil {
		return nil, fmt.Errorf("resolve item path %q: %w", clean, err)
	}

	return item, nil
}

// ListChildren lists children of a remote path. For root (""), uses
// ListChildren with "root". Otherwise uses ListChildrenByPath.
func (s *Session) ListChildren(ctx context.Context, remotePath string) ([]graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		items, err := s.Meta.ListChildren(ctx, s.DriveID, "root")
		if err != nil {
			return nil, fmt.Errorf("list root children: %w", err)
		}

		return items, nil
	}

	items, err := s.Meta.ListChildrenByPath(ctx, s.DriveID, clean)
	if err != nil {
		return nil, fmt.Errorf("list children for %q: %w", clean, err)
	}

	return items, nil
}

// DeleteItem moves an item to the recycle bin.
func (s *Session) DeleteItem(ctx context.Context, itemID string) error {
	if err := s.Meta.DeleteItem(ctx, s.DriveID, itemID); err != nil {
		return fmt.Errorf("delete item %q: %w", itemID, err)
	}

	return nil
}

// PermanentDeleteItem permanently deletes an item (Business/SharePoint only).
func (s *Session) PermanentDeleteItem(ctx context.Context, itemID string) error {
	if err := s.Meta.PermanentDeleteItem(ctx, s.DriveID, itemID); err != nil {
		return fmt.Errorf("permanent delete item %q: %w", itemID, err)
	}

	return nil
}

// CreateFolder creates a folder under the given parent.
func (s *Session) CreateFolder(ctx context.Context, parentID, name string) (*graph.Item, error) {
	item, err := s.Meta.CreateFolder(ctx, s.DriveID, parentID, name)
	if err != nil {
		return nil, fmt.Errorf("create folder %q: %w", name, err)
	}

	return item, nil
}

// MoveItem moves and/or renames an item.
func (s *Session) MoveItem(ctx context.Context, itemID, newParentID, newName string) (*graph.Item, error) {
	item, err := s.Meta.MoveItem(ctx, s.DriveID, itemID, newParentID, newName)
	if err != nil {
		return nil, fmt.Errorf("move item %q: %w", itemID, err)
	}

	return item, nil
}

// CopyItem starts an async copy operation. Returns a monitor URL for polling.
func (s *Session) CopyItem(ctx context.Context, itemID, destParentID, newName string) (*graph.CopyResult, error) {
	result, err := s.Meta.CopyItem(ctx, s.DriveID, itemID, destParentID, newName)
	if err != nil {
		return nil, fmt.Errorf("copy item %q: %w", itemID, err)
	}

	return result, nil
}

// ListRecycleBinItems returns all items in the drive's recycle bin.
func (s *Session) ListRecycleBinItems(ctx context.Context) ([]graph.Item, error) {
	items, err := s.Meta.ListRecycleBinItems(ctx, s.DriveID)
	if err != nil {
		return nil, fmt.Errorf("list recycle bin items: %w", err)
	}

	return items, nil
}

// RestoreItem restores a deleted item from the recycle bin.
func (s *Session) RestoreItem(ctx context.Context, itemID string) (*graph.Item, error) {
	item, err := s.Meta.RestoreItem(ctx, s.DriveID, itemID)
	if err != nil {
		return nil, fmt.Errorf("restore item %q: %w", itemID, err)
	}

	return item, nil
}

// CleanRemotePath strips leading/trailing slashes, returns "" for root.
func CleanRemotePath(path string) string {
	return strings.Trim(path, "/")
}

// SplitParentAndName splits a remote path into parent path and leaf name.
// "foo/bar/baz" → ("foo/bar", "baz"); "baz" → ("", "baz").
func SplitParentAndName(path string) (string, string) {
	clean := CleanRemotePath(path)
	idx := strings.LastIndex(clean, "/")

	if idx < 0 {
		return "", clean
	}

	return clean[:idx], clean[idx+1:]
}

// EnsureFolder creates a folder, returning the existing folder on 409 conflict.
func (s *Session) EnsureFolder(ctx context.Context, parentID, name string) (*graph.Item, error) {
	item, err := s.CreateFolder(ctx, parentID, name)
	if err != nil {
		if errors.Is(err, graph.ErrConflict) {
			children, listErr := s.Meta.ListChildren(ctx, s.DriveID, parentID)
			if listErr != nil {
				return nil, fmt.Errorf("resolving existing folder %q: %w", name, listErr)
			}

			for i := range children {
				if children[i].IsFolder && children[i].Name == name {
					return &children[i], nil
				}
			}

			return nil, fmt.Errorf("folder %q reported as existing but not found in parent", name)
		}

		return nil, err
	}

	return item, nil
}
