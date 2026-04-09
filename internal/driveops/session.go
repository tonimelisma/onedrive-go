package driveops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	gosync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// Session holds authenticated clients and the resolved drive identity for a
// single drive. Wraps a pair of graph.Client instances: Meta for Graph
// metadata operations and Transfer for upload/download traffic.
type Session struct {
	Meta     *graph.Client // metadata ops (transport-level stall detection)
	Transfer *graph.Client // uploads/downloads (no client-level timeout)
	DriveID  driveid.ID
	RootItem string
	Resolved *config.ResolvedDrive

	visibilityWaitSchedule []time.Duration
	sleepFunc              func(context.Context, time.Duration) error
}

var (
	_ PathConvergence        = (*Session)(nil)
	_ PathConvergenceFactory = (*Session)(nil)
)

// AccountClients holds authenticated Graph clients for an account-scoped
// command that does not target a configured drive.
type AccountClients struct {
	Meta     *graph.Client
	Transfer *graph.Client
	Account  driveid.CanonicalID
}

// HTTPClients is the HTTP client pair used to construct one authenticated
// graph session.
type HTTPClients struct {
	Meta     *http.Client
	Transfer *http.Client
}

// ClientResolver resolves the HTTP client pair for a specific drive.
type ClientResolver func(*config.ResolvedDrive) HTTPClients

// StaticClientResolver returns a resolver that always returns the same pair.
func StaticClientResolver(metaHTTP, transferHTTP *http.Client) ClientResolver {
	return func(_ *config.ResolvedDrive) HTTPClients {
		return HTTPClients{
			Meta:     metaHTTP,
			Transfer: transferHTTP,
		}
	}
}

// ResolvedDriveEmail returns the account email for the resolved drive.
func (s *Session) ResolvedDriveEmail() string {
	if s == nil || s.Resolved == nil {
		return ""
	}

	return s.Resolved.CanonicalID.Email()
}

// ForTarget returns a drive-scoped path-convergence view for the requested
// drive/root pair. Sessions already carry the authenticated Graph clients and
// convergence schedule; target switching only changes the resolved drive/root
// identity used by path-based operations.
func (s *Session) ForTarget(driveID driveid.ID, rootItemID string) PathConvergence {
	if s == nil {
		return nil
	}
	if driveID.IsZero() || (driveID.Equal(s.DriveID) && rootItemID == s.RootItem) {
		return s
	}

	clone := *s
	clone.DriveID = driveID
	clone.RootItem = rootItemID

	return &clone
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
	resolveHTTP  ClientResolver
	userAgent    string
	logger       *slog.Logger
	GraphBaseURL string

	// TokenSourceFn creates a TokenSource from a token file path. Exported
	// for test injection; defaults to graph.TokenSourceFromPath.
	TokenSourceFn func(ctx context.Context, tokenPath string, logger *slog.Logger) (graph.TokenSource, error)

	mu         gosync.Mutex
	tokenCache map[string]graph.TokenSource // keyed by token file path
}

// NewSessionProvider creates a SessionProvider with default TokenSourceFn.
func NewSessionProvider(
	holder *config.Holder, resolveHTTP ClientResolver,
	userAgent string, logger *slog.Logger,
) *SessionProvider {
	if resolveHTTP == nil {
		resolveHTTP = StaticClientResolver(nil, nil)
	}

	return &SessionProvider{
		holder:        holder,
		resolveHTTP:   resolveHTTP,
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

	httpClients := p.resolveHTTP(rd)
	meta, transfer, err := p.clientsForTokenPath(ctx, tokenPath, httpClients)
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

	p.logger.Debug("session created",
		slog.String("drive_id", rd.DriveID.String()),
		slog.String("canonical_id", rd.CanonicalID.String()),
	)

	return &Session{
		Meta:     meta,
		Transfer: transfer,
		DriveID:  rd.DriveID,
		RootItem: rd.RootItemID,
		Resolved: rd,
	}, nil
}

// ClientsForAccount returns authenticated Graph clients for the token-owning
// account canonical ID. Shared-file commands use this path because they target
// account-scoped shared items rather than configured drives.
func (p *SessionProvider) ClientsForAccount(ctx context.Context, cid driveid.CanonicalID) (*AccountClients, error) {
	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for account %q", cid.Email())
	}

	meta, transfer, err := p.clientsForTokenPath(ctx, tokenPath, p.resolveHTTP(nil))
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in — run 'onedrive-go login' first: %w", err)
		}

		return nil, err
	}

	return &AccountClients{
		Meta:     meta,
		Transfer: transfer,
		Account:  cid,
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

func (p *SessionProvider) clientsForTokenPath(
	ctx context.Context,
	tokenPath string,
	httpClients HTTPClients,
) (*graph.Client, *graph.Client, error) {
	ts, err := p.getOrCreateTokenSource(ctx, tokenPath)
	if err != nil {
		return nil, nil, err
	}

	baseURL := graph.DefaultBaseURL
	if p.GraphBaseURL != "" {
		baseURL = p.GraphBaseURL
	}

	meta, err := graph.NewClient(baseURL, httpClients.Meta, ts, p.logger, p.userAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("create metadata graph client: %w", err)
	}

	transfer, err := graph.NewClient(baseURL, httpClients.Transfer, ts, p.logger, p.userAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("create transfer graph client: %w", err)
	}

	return meta, transfer, nil
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

// UpdateConfig replaces the shared raw config snapshot backing the provider's
// token-resolution holder. Used when a command rewrites canonical IDs in the
// config and needs subsequent Session() calls to resolve shared-drive tokens
// through the updated metadata.
func (p *SessionProvider) UpdateConfig(cfg *config.Config) {
	if p == nil || p.holder == nil || cfg == nil {
		return
	}

	p.holder.Update(cfg)
}

// ResolveItem resolves a remote path to an Item. For root (""), uses GetItem
// with "root". Otherwise uses GetItemByPath. "/" normalizes to "" via
// CleanRemotePath, so callers can pass either "/" or "" to mean root.
func (s *Session) ResolveItem(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		rootID := s.rootItemID()
		item, err := s.Meta.GetItem(ctx, s.DriveID, rootID)
		if err != nil {
			return nil, fmt.Errorf("resolve root item: %w", err)
		}

		if s.hasScopedRoot() {
			item.IsRoot = true
		}

		return item, nil
	}

	if s.hasScopedRoot() {
		item, err := s.resolveItemFromScopedRoot(ctx, clean)
		if err != nil {
			return nil, fmt.Errorf("resolve item path %q: %w", clean, err)
		}

		return item, nil
	}

	item, err := s.Meta.GetItemByPath(ctx, s.DriveID, clean)
	if err != nil {
		return nil, fmt.Errorf("resolve item path %q: %w", clean, err)
	}

	return item, nil
}

// ResolveDeleteTarget resolves a delete-intended remote path to an item.
// Delete intent is authoritative on the path, so when the exact path route
// lies with itemNotFound we fall back to the parent collection before
// declaring the target missing.
func (s *Session) ResolveDeleteTarget(ctx context.Context, remotePath string) (*graph.Item, error) {
	item, err := s.ResolveItem(ctx, remotePath)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return nil, err
	}

	item, err = s.resolveItemFromParentListing(ctx, remotePath)
	if err != nil {
		return nil, err
	}

	return item, nil
}

// WaitPathVisible blocks until the remote path is readable through ordinary
// path resolution after a successful mutation. Graph can acknowledge mkdir,
// put, or move before follow-on path reads stop returning itemNotFound, so the
// command boundary treats visibility convergence as part of mutation success.
func (s *Session) WaitPathVisible(ctx context.Context, remotePath string) (*graph.Item, error) {
	item, err := s.resolveConvergingPath(ctx, remotePath)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return nil, err
	}

	for _, delay := range s.visibilitySchedule() {
		if sleepErr := s.sleep(ctx, delay); sleepErr != nil {
			return nil, fmt.Errorf("wait for path %q visibility: %w", remotePath, sleepErr)
		}

		item, err = s.resolveConvergingPath(ctx, remotePath)
		if err == nil {
			return item, nil
		}
		if !errors.Is(err, graph.ErrNotFound) {
			return nil, err
		}
	}

	return nil, &PathNotVisibleError{Path: remotePath}
}

func (s *Session) resolveConvergingPath(ctx context.Context, remotePath string) (*graph.Item, error) {
	item, err := s.ResolveItem(ctx, remotePath)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return nil, err
	}

	item, err = s.resolveItemFromParentListing(ctx, remotePath)
	if err == nil {
		return item, nil
	}
	if errors.Is(err, graph.ErrNotFound) {
		return nil, err
	}

	return nil, err
}

// ListChildren lists children of a remote path. For root (""), uses
// ListChildren with "root". Otherwise uses ListChildrenByPath.
func (s *Session) ListChildren(ctx context.Context, remotePath string) ([]graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		items, err := s.Meta.ListChildren(ctx, s.DriveID, s.rootItemID())
		if err != nil {
			return nil, fmt.Errorf("list root children: %w", err)
		}

		return items, nil
	}

	if s.hasScopedRoot() {
		parent, err := s.resolveItemFromScopedRoot(ctx, clean)
		if err != nil {
			return nil, fmt.Errorf("list children for %q: %w", clean, err)
		}

		items, err := s.Meta.ListChildren(ctx, s.DriveID, parent.ID)
		if err != nil {
			return nil, fmt.Errorf("list children for %q: %w", clean, err)
		}

		return items, nil
	}

	items, err := s.Meta.ListChildrenByPath(ctx, s.DriveID, clean)
	if err != nil {
		return nil, fmt.Errorf("list children for %q: %w", clean, err)
	}

	return items, nil
}

func (s *Session) visibilitySchedule() []time.Duration {
	if len(s.visibilityWaitSchedule) == 0 {
		policy := retry.PathVisibilityPolicy()
		delayCount := 0
		if policy.MaxAttempts > 0 {
			delayCount = policy.MaxAttempts - 1
		}
		schedule := make([]time.Duration, 0, delayCount)
		for attempt := range policy.MaxAttempts - 1 {
			schedule = append(schedule, policy.Delay(attempt))
		}

		return schedule
	}

	return s.visibilityWaitSchedule
}

func (s *Session) sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		if ctx.Err() != nil {
			return fmt.Errorf("wait for path visibility: %w", ctx.Err())
		}

		return nil
	}
	if s.sleepFunc != nil {
		return s.sleepFunc(ctx, delay)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for path visibility: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (s *Session) deleteResolvedPath(
	ctx context.Context,
	remotePath string,
	itemID string,
	deleteByID func(context.Context, string) error,
) error {
	currentID := itemID
	err := deleteByID(ctx, currentID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return err
	}

	lastResolution := deleteConvergenceResolutionUnknown

	for _, delay := range s.visibilitySchedule() {
		if sleepErr := s.sleep(ctx, delay); sleepErr != nil {
			return fmt.Errorf("wait for delete path %q convergence: %w", remotePath, sleepErr)
		}

		resolution, item, resolveErr := s.resolveDeleteConvergenceTarget(ctx, remotePath)
		switch {
		case resolveErr != nil:
			return fmt.Errorf("confirm delete path %q state: %w", remotePath, resolveErr)
		case resolution == deleteConvergenceMissing:
			return nil
		}
		currentID = item.ID
		lastResolution = resolution

		err = deleteByID(ctx, currentID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, graph.ErrNotFound) {
			return err
		}
	}

	// A final exact-path miss plus parent-list-only hit that also deletes as
	// itemNotFound is stale positive evidence, not authoritative existence.
	if errors.Is(err, graph.ErrNotFound) && lastResolution == deleteConvergenceParentListing {
		return nil
	}

	return err
}

// DeleteItem moves an item to the recycle bin.
func (s *Session) DeleteItem(ctx context.Context, itemID string) error {
	if err := s.Meta.DeleteItem(ctx, s.DriveID, itemID); err != nil {
		return fmt.Errorf("delete item %q: %w", itemID, err)
	}

	return nil
}

// DeleteResolvedPath keeps the delete operation authoritative on the remote
// path, not only the initially resolved item ID. Graph can briefly return
// itemNotFound on the delete route even though the same path resolved moments
// earlier, so this method re-resolves and retries until the path disappears or
// the bounded visibility schedule is exhausted.
func (s *Session) DeleteResolvedPath(ctx context.Context, remotePath, itemID string) error {
	return s.deleteResolvedPath(ctx, remotePath, itemID, s.DeleteItem)
}

// PermanentDeleteItem permanently deletes an item (Business/SharePoint only).
func (s *Session) PermanentDeleteItem(ctx context.Context, itemID string) error {
	if err := s.Meta.PermanentDeleteItem(ctx, s.DriveID, itemID); err != nil {
		return fmt.Errorf("permanent delete item %q: %w", itemID, err)
	}

	return nil
}

// PermanentDeleteResolvedPath mirrors DeleteResolvedPath for the permanent
// delete route.
func (s *Session) PermanentDeleteResolvedPath(ctx context.Context, remotePath, itemID string) error {
	return s.deleteResolvedPath(ctx, remotePath, itemID, s.PermanentDeleteItem)
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

const driveRootItemID = "root"

func (s *Session) hasScopedRoot() bool {
	return s != nil && s.RootItem != "" && s.RootItem != driveRootItemID
}

func (s *Session) rootItemID() string {
	if s.hasScopedRoot() {
		return s.RootItem
	}

	return driveRootItemID
}

func (s *Session) resolveItemFromScopedRoot(ctx context.Context, remotePath string) (*graph.Item, error) {
	currentID := s.rootItemID()
	segments := strings.Split(remotePath, "/")
	var current *graph.Item

	for idx, segment := range segments {
		children, err := s.Meta.ListChildren(ctx, s.DriveID, currentID)
		if err != nil {
			return nil, fmt.Errorf("list children for segment %q: %w", segment, err)
		}

		match, ok := matchChildByName(children, segment)
		if !ok {
			return nil, fmt.Errorf("segment %q: %w", segment, graph.ErrNotFound)
		}

		current = &match
		currentID = match.ID

		if idx < len(segments)-1 && !match.IsFolder {
			return nil, fmt.Errorf("segment %q is not a folder: %w", segment, graph.ErrNotFound)
		}
	}

	if current == nil {
		return nil, fmt.Errorf("resolve scoped item %q: %w", remotePath, graph.ErrNotFound)
	}

	return current, nil
}

func matchChildByName(children []graph.Item, name string) (graph.Item, bool) {
	var folded *graph.Item

	for i := range children {
		if children[i].Name == name {
			return children[i], true
		}

		if folded == nil && strings.EqualFold(children[i].Name, name) {
			folded = &children[i]
		}
	}

	if folded != nil {
		return *folded, true
	}

	return graph.Item{}, false
}

type deleteConvergenceResolution int

const (
	deleteConvergenceResolutionUnknown deleteConvergenceResolution = iota
	deleteConvergenceExactPath
	deleteConvergenceParentListing
	deleteConvergenceMissing
)

func (s *Session) resolveDeleteConvergenceTarget(
	ctx context.Context,
	remotePath string,
) (deleteConvergenceResolution, *graph.Item, error) {
	item, err := s.ResolveItem(ctx, remotePath)
	if err == nil {
		return deleteConvergenceExactPath, item, nil
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return deleteConvergenceResolutionUnknown, nil, err
	}

	item, err = s.resolveItemFromParentListing(ctx, remotePath)
	if err == nil {
		return deleteConvergenceParentListing, item, nil
	}
	if errors.Is(err, graph.ErrNotFound) {
		return deleteConvergenceMissing, nil, nil
	}

	return deleteConvergenceResolutionUnknown, nil, err
}

func (s *Session) resolveItemFromParentListing(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		return nil, fmt.Errorf("resolve delete target %q: %w", remotePath, graph.ErrNotFound)
	}

	parentPath, name := SplitParentAndName(clean)
	children, err := s.listDeleteParentChildren(ctx, parentPath)
	if err != nil {
		return nil, fmt.Errorf("resolve delete parent %q: %w", parentPath, err)
	}

	match, ok := matchChildByName(children, name)
	if !ok {
		return nil, fmt.Errorf("resolve delete target %q from parent %q: %w", clean, parentPath, graph.ErrNotFound)
	}

	return &match, nil
}

func (s *Session) listDeleteParentChildren(ctx context.Context, parentPath string) ([]graph.Item, error) {
	children, err := s.ListChildren(ctx, parentPath)
	if err == nil {
		return children, nil
	}
	if !errors.Is(err, graph.ErrNotFound) || parentPath == "" {
		return nil, err
	}

	parentItem, err := s.ResolveDeleteTarget(ctx, parentPath)
	if err != nil {
		return nil, err
	}

	children, err = s.Meta.ListChildren(ctx, s.DriveID, parentItem.ID)
	if err != nil {
		return nil, fmt.Errorf("list children for delete parent %q by id: %w", parentPath, err)
	}

	return children, nil
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
