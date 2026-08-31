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
	"github.com/tonimelisma/onedrive-go/internal/graphtransport"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// Session holds authenticated clients for one remote drive. Mount-root path
// scoping is owned by MountSession so sync engine construction does not infer
// runtime root identity from generic authenticated clients.
type Session struct {
	Meta              *graph.Client // metadata ops (transport-level stall detection)
	Transfer          *graph.Client // uploads/downloads (no client-level timeout)
	DriveID           driveid.ID
	AccountEmailValue string

	visibilityWaitSchedule []time.Duration
	sleepFunc              func(context.Context, time.Duration) error
}

var _ PathConvergence = (*MountSession)(nil)

// MountSession adds mount-root path semantics to an authenticated Session.
// Direct file commands use this wrapper because their path operations are
// relative to the configured mount root, not necessarily the Graph drive root.
type MountSession struct {
	*Session
	RemoteRootItemID string
}

func NewMountSession(session *Session, remoteRootItemID string) *MountSession {
	if session == nil {
		return nil
	}

	return &MountSession{
		Session:          session,
		RemoteRootItemID: remoteRootItemID,
	}
}

// AccountClients holds authenticated Graph clients for an account-scoped
// command that does not target a configured drive.
type AccountClients struct {
	Meta     *graph.Client
	Transfer *graph.Client
	Account  driveid.CanonicalID
}

// MountSessionConfig is the mount-owned identity required to create an
// authenticated interactive or sync session.
type MountSessionConfig struct {
	TokenOwnerCanonical driveid.CanonicalID
	DriveID             driveid.ID
	RemoteRootItemID    string
	AccountEmail        string
}

// AccountEmail returns the account email associated with this session.
func (s *Session) AccountEmail() string {
	if s == nil {
		return ""
	}

	return s.AccountEmailValue
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

// SessionRuntime owns authenticated Graph session construction plus the
// cached HTTP client profiles and token sources that back those sessions.
// Multiple drives sharing a token path share one TokenSource, preventing
// OAuth2 refresh token rotation races (two independent refreshes can
// invalidate each other's refresh tokens).
type SessionRuntime struct {
	holder       *config.Holder
	userAgent    string
	logger       *slog.Logger
	GraphBaseURL string

	// TokenSourceFn creates a TokenSource from a token file path. Exported
	// for test injection; defaults to graph.TokenSourceFromPath.
	TokenSourceFn func(ctx context.Context, tokenPath string, logger *slog.Logger) (graph.TokenSource, error)

	mu                  gosync.Mutex
	tokenCache          map[string]graph.TokenSource
	bootstrapMeta       *http.Client
	interactiveMeta     map[string]*http.Client
	interactiveTransfer *http.Client
	syncClients         *graphtransport.ClientSet
}

// NewSessionRuntime creates a SessionRuntime with default TokenSourceFn.
func NewSessionRuntime(holder *config.Holder, userAgent string, logger *slog.Logger) *SessionRuntime {
	if logger == nil {
		logger = slog.Default()
	}

	return &SessionRuntime{
		holder:          holder,
		userAgent:       userAgent,
		logger:          logger,
		TokenSourceFn:   graph.TokenSourceFromPath,
		tokenCache:      make(map[string]graph.TokenSource),
		interactiveMeta: make(map[string]*http.Client),
	}
}

// InteractiveSession creates or retrieves an authenticated interactive Session
// for one mounted remote root. Mount-root sessions use a root-scoped throttle
// key so interactive metadata retries stay scoped to the concrete remote root.
func (r *SessionRuntime) InteractiveSession(ctx context.Context, mount *MountSessionConfig) (*MountSession, error) {
	session, err := r.session(ctx, mount, r.interactiveClientsForMount(mount))
	if err != nil {
		return nil, err
	}

	return NewMountSession(session, mount.RemoteRootItemID), nil
}

// SyncSession creates or retrieves the authenticated Session used by sync
// workers. Sync paths intentionally bypass retry-wrapped HTTP clients.
func (r *SessionRuntime) SyncSession(ctx context.Context, mount *MountSessionConfig) (*Session, error) {
	return r.session(ctx, mount, r.syncClientSet())
}

func (r *SessionRuntime) session(
	ctx context.Context,
	mount *MountSessionConfig,
	httpClients graphtransport.ClientSet,
) (*Session, error) {
	if mount == nil {
		return nil, fmt.Errorf("mount session config is required")
	}

	tokenPath := config.DriveTokenPath(mount.TokenOwnerCanonical)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for mount %q", mount.TokenOwnerCanonical)
	}

	meta, transfer, err := r.clientsForTokenPath(ctx, tokenPath, httpClients)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in — run 'onedrive-go login' first: %w", err)
		}

		return nil, err
	}

	if mount.DriveID.IsZero() {
		return nil, fmt.Errorf(
			"drive ID not resolved for token owner %s — token file may be missing or corrupted; re-run 'onedrive-go login'",
			mount.TokenOwnerCanonical,
		)
	}

	r.logger.Debug("session created",
		slog.String("drive_id", mount.DriveID.String()),
		slog.String("token_owner_canonical_id", mount.TokenOwnerCanonical.String()),
	)

	accountEmail := mount.AccountEmail
	if accountEmail == "" {
		accountEmail = mount.TokenOwnerCanonical.Email()
	}

	return &Session{
		Meta:              meta,
		Transfer:          transfer,
		DriveID:           mount.DriveID,
		AccountEmailValue: accountEmail,
	}, nil
}

// SharedTargetClients returns authenticated Graph clients for the token-owning
// account canonical ID scoped to one shared remote target. Shared-item
// commands use this path because they target account-scoped shared items
// rather than configured drives.
func (r *SessionRuntime) SharedTargetClients(
	ctx context.Context,
	cid driveid.CanonicalID,
	remoteDriveID string,
	remoteItemID string,
) (*AccountClients, error) {
	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for account %q", cid.Email())
	}

	meta, transfer, err := r.clientsForTokenPath(
		ctx,
		tokenPath,
		r.interactiveClientsForSharedTarget(cid.Email(), remoteDriveID, remoteItemID),
	)
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
func (r *SessionRuntime) getOrCreateTokenSource(ctx context.Context, tokenPath string) (graph.TokenSource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ts, ok := r.tokenCache[tokenPath]; ok {
		return ts, nil
	}

	ts, err := r.TokenSourceFn(ctx, tokenPath, r.logger)
	if err != nil {
		return nil, err
	}

	r.tokenCache[tokenPath] = ts

	return ts, nil
}

func (r *SessionRuntime) clientsForTokenPath(
	ctx context.Context,
	tokenPath string,
	httpClients graphtransport.ClientSet,
) (*graph.Client, *graph.Client, error) {
	ts, err := r.getOrCreateTokenSource(ctx, tokenPath)
	if err != nil {
		return nil, nil, err
	}

	baseURL := graph.DefaultBaseURL
	if r.GraphBaseURL != "" {
		baseURL = r.GraphBaseURL
	}

	meta, err := graph.NewClient(baseURL, httpClients.Meta, ts, r.logger, r.userAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("create metadata graph client: %w", err)
	}

	transfer, err := graph.NewClient(baseURL, httpClients.Transfer, ts, r.logger, r.userAgent)
	if err != nil {
		return nil, nil, fmt.Errorf("create transfer graph client: %w", err)
	}

	return meta, transfer, nil
}

// FlushTokenCache clears the cached TokenSources, forcing the next Session()
// call to re-read token files from disk. Called during daemon control-socket
// reload to pick up logout/re-login credential changes.
func (r *SessionRuntime) FlushTokenCache() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tokenCache = make(map[string]graph.TokenSource)
	r.logger.Info("token cache flushed")
}

// UpdateConfig replaces the shared raw config snapshot backing the runtime's
// token-resolution holder. Used when a command rewrites canonical IDs in the
// config and needs subsequent Session() calls to resolve shared-drive tokens
// through the updated metadata.
func (r *SessionRuntime) UpdateConfig(cfg *config.Config) {
	if r == nil || r.holder == nil || cfg == nil {
		return
	}

	r.holder.Update(cfg)
}

// BootstrapMeta returns the retrying metadata client used before account
// identity is known.
func (r *SessionRuntime) BootstrapMeta() *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.bootstrapMeta == nil {
		r.bootstrapMeta = graphtransport.BootstrapMetadataClient(r.logger)
	}

	return r.bootstrapMeta
}

func (r *SessionRuntime) interactiveClientsForMount(mount *MountSessionConfig) graphtransport.ClientSet {
	if mount == nil {
		return graphtransport.ClientSet{
			Meta:     r.BootstrapMeta(),
			Transfer: r.syncClientSet().Transfer,
		}
	}

	account := mount.AccountEmail
	if account == "" {
		account = mount.TokenOwnerCanonical.Email()
	}
	if mount.RemoteRootItemID != "" {
		return r.interactiveClientsForSharedTarget(account, mount.DriveID.String(), mount.RemoteRootItemID)
	}

	return r.interactiveClientsForKey(interactiveDriveKey(account, mount.DriveID))
}

func (r *SessionRuntime) interactiveClientsForSharedTarget(
	account string,
	remoteDriveID string,
	remoteItemID string,
) graphtransport.ClientSet {
	return r.interactiveClientsForKey(interactiveSharedKey(account, remoteDriveID, remoteItemID))
}

func (r *SessionRuntime) interactiveClientsForKey(targetKey string) graphtransport.ClientSet {
	r.mu.Lock()
	defer r.mu.Unlock()

	meta := r.interactiveMeta[targetKey]
	if meta == nil {
		meta = graphtransport.InteractiveMetadataClient(r.logger, &retry.ThrottleGate{})
		r.interactiveMeta[targetKey] = meta
	}

	if r.interactiveTransfer == nil {
		r.interactiveTransfer = graphtransport.InteractiveTransferClient(r.logger)
	}

	return graphtransport.ClientSet{
		Meta:     meta,
		Transfer: r.interactiveTransfer,
	}
}

func (r *SessionRuntime) syncClientSet() graphtransport.ClientSet {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.syncClients == nil {
		clients := graphtransport.SyncClientSet()
		r.syncClients = &clients
	}

	return *r.syncClients
}

func interactiveDriveKey(account string, driveID driveid.ID) string {
	return account + "|drive:" + driveID.String()
}

func interactiveSharedKey(account, remoteDriveID, remoteItemID string) string {
	return account + "|shared:" + remoteDriveID + ":" + remoteItemID
}

// ResolveItem resolves a remote path to an Item. For root (""), uses GetItem
// with "root". Otherwise uses GetItemByPath. "/" normalizes to "" via
// CleanRemotePath, so callers can pass either "/" or "" to mean root.
func (s *MountSession) ResolveItem(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		rootID := s.remoteRootItemID()
		item, err := s.Meta.GetItem(ctx, s.DriveID, rootID)
		if err != nil {
			return nil, fmt.Errorf("resolve root item: %w", err)
		}

		if s.hasMountRoot() {
			item.IsRoot = true
		}

		return item, nil
	}

	if s.hasMountRoot() {
		item, err := s.resolveItemFromMountRoot(ctx, clean)
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
func (s *MountSession) ResolveDeleteTarget(ctx context.Context, remotePath string) (*graph.Item, error) {
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
func (s *MountSession) WaitPathVisible(ctx context.Context, remotePath string) (*graph.Item, error) {
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

func (s *MountSession) resolveConvergingPath(ctx context.Context, remotePath string) (*graph.Item, error) {
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
func (s *MountSession) ListChildren(ctx context.Context, remotePath string) ([]graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		items, err := s.Meta.ListChildren(ctx, s.DriveID, s.remoteRootItemID())
		if err != nil {
			return nil, fmt.Errorf("list root children: %w", err)
		}

		return items, nil
	}

	if s.hasMountRoot() {
		parent, err := s.resolveItemFromMountRoot(ctx, clean)
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

func (s *MountSession) deleteResolvedPath(
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
func (s *MountSession) DeleteResolvedPath(ctx context.Context, remotePath, itemID string) error {
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
func (s *MountSession) PermanentDeleteResolvedPath(ctx context.Context, remotePath, itemID string) error {
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

const graphDriveRootItemID = "root"

func (s *MountSession) hasMountRoot() bool {
	return s != nil && s.RemoteRootItemID != "" && s.RemoteRootItemID != graphDriveRootItemID
}

func (s *MountSession) remoteRootItemID() string {
	if s.hasMountRoot() {
		return s.RemoteRootItemID
	}

	return graphDriveRootItemID
}

func (s *MountSession) resolveItemFromMountRoot(ctx context.Context, remotePath string) (*graph.Item, error) {
	currentID := s.remoteRootItemID()
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
		return nil, fmt.Errorf("resolve mount-root item %q: %w", remotePath, graph.ErrNotFound)
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

func (s *MountSession) resolveDeleteConvergenceTarget(
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

func (s *MountSession) resolveItemFromParentListing(ctx context.Context, remotePath string) (*graph.Item, error) {
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

func (s *MountSession) listDeleteParentChildren(ctx context.Context, parentPath string) ([]graph.Item, error) {
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
