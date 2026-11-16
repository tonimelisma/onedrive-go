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
	"github.com/tonimelisma/onedrive-go/internal/namekey"
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
// Every client it builds for one token path, drive sessions and account-level
// bootstrap clients alike, shares one TokenSource (R-6.8.27), preventing
// OAuth2 refresh token rotation races (two independent refreshes can
// invalidate each other's refresh tokens).
type SessionRuntime struct {
	holder       *config.Holder
	userAgent    string
	logger       *slog.Logger
	GraphBaseURL string

	// TokenSourceFn creates a TokenSource from a token file path. Exported
	// for test injection; defaults to graph.TokenSourceFromPath with authHTTP.
	// It takes no context: a source refreshes under the context of the
	// request that needs a token, never one it keeps.
	TokenSourceFn func(tokenPath string, logger *slog.Logger) (graph.TokenSource, error)

	// authHTTP sends every sign-in request of the default token sources
	// (graphtransport.AuthClient). Built at construction and never replaced,
	// so TokenSourceFn reads it without mu.
	authHTTP *http.Client

	// retryNotices is shared by every retrying client this runtime builds,
	// so one outage or long rate-limit wait is announced once per process
	// rather than once per client or request (R-6.6.8). Set at construction
	// and never replaced; Notices guards its own state.
	retryNotices *retry.Notices

	// mu guards the lazily built caches below: tokenSources, bootstrapMeta,
	// interactiveMeta, interactiveTransfer, and syncClients. Sessions for
	// different mounts build them concurrently. It is never held while a
	// sign-in file is read.
	mu gosync.Mutex
	// tokenSources holds one load per sign-in file, finished or under way.
	tokenSources map[string]*tokenSourceLoad
	// bootstrapMeta holds one bootstrap metadata client per sign-in file, so
	// a throttle deadline OneDrive sets for one sign-in is that sign-in's.
	bootstrapMeta       map[string]*http.Client
	interactiveMeta     map[string]*http.Client
	interactiveTransfer *http.Client
	syncClients         *graphtransport.ClientSet
}

// tokenSourceLoad is the one load of a sign-in file's token source. The
// caller that starts it reads the file outside SessionRuntime.mu; callers for
// the same file wait for done. ts and err are set before done closes and
// never change after.
type tokenSourceLoad struct {
	done chan struct{}
	ts   graph.TokenSource
	err  error
}

// errTokenSourceLoadAbandoned is what the callers waiting for a load get when
// it ended without an answer: its loader panicked.
var errTokenSourceLoadAbandoned = errors.New("loading the sign-in did not finish")

// NewSessionRuntime creates a SessionRuntime with default TokenSourceFn.
func NewSessionRuntime(holder *config.Holder, userAgent string, logger *slog.Logger) *SessionRuntime {
	if logger == nil {
		logger = slog.Default()
	}

	runtime := &SessionRuntime{
		holder:          holder,
		userAgent:       userAgent,
		logger:          logger,
		authHTTP:        graphtransport.AuthClient(),
		retryNotices:    &retry.Notices{},
		tokenSources:    make(map[string]*tokenSourceLoad),
		bootstrapMeta:   make(map[string]*http.Client),
		interactiveMeta: make(map[string]*http.Client),
	}
	runtime.TokenSourceFn = runtime.tokenSourceFromPath

	return runtime
}

// tokenSourceFromPath is the default TokenSourceFn: the saved sign-in at
// tokenPath, refreshed through the runtime's auth client.
func (r *SessionRuntime) tokenSourceFromPath(tokenPath string, logger *slog.Logger) (graph.TokenSource, error) {
	// The graph error already says what failed, and every caller of the
	// runtime adds its own context ("not logged in — run ..."), so the
	// runtime passes it through as it always has for TokenSourceFn errors.
	return graph.TokenSourceFromPath(tokenPath, r.authHTTP, logger) //nolint:wrapcheck // callers add the context, see above
}

// InteractiveSession creates or retrieves an authenticated interactive Session
// for one mounted remote root. Mount-root sessions use a root-scoped throttle
// key so interactive metadata retries stay scoped to the concrete remote root.
func (r *SessionRuntime) InteractiveSession(mount *MountSessionConfig) (*MountSession, error) {
	if mount == nil {
		return nil, errMountConfigRequired
	}

	session, err := r.session(mount, r.interactiveClientsForMount(mount))
	if err != nil {
		return nil, err
	}

	return NewMountSession(session, mount.RemoteRootItemID), nil
}

// errMountConfigRequired is a session constructor called without the mount it
// is for.
var errMountConfigRequired = errors.New("mount session config is required")

// SyncSession creates or retrieves the authenticated Session used by sync
// workers. Sync paths intentionally bypass retry-wrapped HTTP clients.
func (r *SessionRuntime) SyncSession(mount *MountSessionConfig) (*Session, error) {
	return r.session(mount, r.syncClientSet())
}

func (r *SessionRuntime) session(
	mount *MountSessionConfig,
	httpClients graphtransport.ClientSet,
) (*Session, error) {
	if mount == nil {
		return nil, errMountConfigRequired
	}

	tokenPath := config.DriveTokenPath(mount.TokenOwnerCanonical)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for mount %q", mount.TokenOwnerCanonical)
	}

	meta, transfer, err := r.clientsForTokenPath(tokenPath, httpClients)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return nil, fmt.Errorf("not logged in — run 'onedrive-go login' first: %w", err)
		}

		return nil, err
	}

	if mount.DriveID.IsZero() {
		// The drive record lost its OneDrive drive ID (a missing or damaged
		// saved login). Say it in the user's terms: which account, and the
		// one command that restores it.
		email := mount.TokenOwnerCanonical.Email()
		return nil, fmt.Errorf(
			"onedrive-go doesn't know which OneDrive to use for %s; run 'onedrive-go login --account %s' to set it up again",
			email,
			email,
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
	cid driveid.CanonicalID,
	remoteDriveID string,
	remoteItemID string,
) (*AccountClients, error) {
	tokenPath := config.DriveTokenPath(cid)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for account %q", cid.Email())
	}

	meta, transfer, err := r.clientsForTokenPath(
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

// BootstrapClient returns a Graph client for account-level calls made before
// a drive session exists, or without one: identity probes, drive and
// shared-item discovery, SharePoint search, shared-item naming. It uses the
// runtime's Graph base URL, the bootstrap metadata client of tokenPath
// (BootstrapMeta) and the runtime's one token source for tokenPath, the same
// source the drive sessions for that sign-in file use (R-6.8.27).
// Token-source errors keep their chain, so callers can still tell
// graph.ErrNotLoggedIn from a damaged sign-in.
//
// It takes no context: the source refreshes under the context of each request
// that needs a token, so a client built here serves requests with any
// deadline, a status budget's included.
func (r *SessionRuntime) BootstrapClient(tokenPath string) (*graph.Client, error) {
	if tokenPath == "" {
		return nil, fmt.Errorf("bootstrap graph client: token path is required")
	}

	ts, err := r.getOrCreateTokenSource(tokenPath)
	if err != nil {
		return nil, err
	}

	client, err := graph.NewClient(r.graphBaseURL(), r.BootstrapMeta(tokenPath), ts, r.logger, r.userAgent)
	if err != nil {
		return nil, fmt.Errorf("create bootstrap graph client: %w", err)
	}

	return client, nil
}

// getOrCreateTokenSource returns the token source of tokenPath, loading it on
// first use. The file is read outside r.mu, so a slow or large read holds up
// no other sign-in and none of the runtime's other caches; callers that want
// the same file meanwhile wait for that one load. A failed load is not kept:
// the next caller reads the file again (a later login in the same process is
// picked up). A load whose loader panics is a failed load too.
func (r *SessionRuntime) getOrCreateTokenSource(tokenPath string) (graph.TokenSource, error) {
	r.mu.Lock()
	load, loading := r.tokenSources[tokenPath]
	if !loading {
		load = &tokenSourceLoad{done: make(chan struct{})}
		r.tokenSources[tokenPath] = load
	}
	r.mu.Unlock()

	if loading {
		<-load.done
		return load.ts, load.err
	}

	// The load counts as failed until the loader answers, so a loader that
	// panics still releases its waiters, with an error, and leaves no entry
	// for a later caller to wait on. The panic itself goes on to this caller.
	var ts graph.TokenSource
	err := errTokenSourceLoadAbandoned
	defer func() {
		r.mu.Lock()
		load.ts, load.err = ts, err
		if err != nil && r.tokenSources[tokenPath] == load {
			delete(r.tokenSources, tokenPath)
		}
		r.mu.Unlock()
		close(load.done)
	}()

	ts, err = r.TokenSourceFn(tokenPath, r.logger)

	return ts, err
}

func (r *SessionRuntime) clientsForTokenPath(
	tokenPath string,
	httpClients graphtransport.ClientSet,
) (*graph.Client, *graph.Client, error) {
	ts, err := r.getOrCreateTokenSource(tokenPath)
	if err != nil {
		return nil, nil, err
	}

	baseURL := r.graphBaseURL()

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

// graphBaseURL is the Graph root every client of the runtime uses: the
// default, or GraphBaseURL when a test points the runtime at a local server.
func (r *SessionRuntime) graphBaseURL() string {
	if r.GraphBaseURL != "" {
		return r.GraphBaseURL
	}

	return graph.DefaultBaseURL
}

// FlushTokenCache clears the cached TokenSources and the per-sign-in
// bootstrap clients beside them, forcing the next Session() call to re-read
// token files from disk. Called during daemon control-socket reload to pick
// up logout/re-login credential changes. A load under way finishes for its
// waiters and is not kept.
func (r *SessionRuntime) FlushTokenCache() {
	r.mu.Lock()
	r.tokenSources = make(map[string]*tokenSourceLoad)
	r.bootstrapMeta = make(map[string]*http.Client)
	r.mu.Unlock()

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

// BootstrapMeta returns the retrying metadata client for account-level calls
// of the sign-in saved at tokenPath, building it on first use: BootstrapClient
// uses it, and so does login's account discovery for the sign-in file it just
// wrote. Each sign-in has its own, so the throttle deadline a 429 sets holds
// for that sign-in's next requests and delays no other account's.
func (r *SessionRuntime) BootstrapMeta(tokenPath string) *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()

	client := r.bootstrapMeta[tokenPath]
	if client == nil {
		client = graphtransport.BootstrapMetadataClient(r.logger, r.retryNotices)
		r.bootstrapMeta[tokenPath] = client
	}

	return client
}

func (r *SessionRuntime) interactiveClientsForMount(mount *MountSessionConfig) graphtransport.ClientSet {
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
		meta = graphtransport.InteractiveMetadataClient(r.logger, &retry.ThrottleGate{}, r.retryNotices)
		r.interactiveMeta[targetKey] = meta
	}

	if r.interactiveTransfer == nil {
		r.interactiveTransfer = graphtransport.InteractiveTransferClient(r.logger, r.retryNotices)
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
	return s.resolveConvergingPath(ctx, remotePath)
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

// resolveConvergingPath reads remotePath by path and, when that read finds
// nothing, looks for its name in the parent folder's listing: the path route
// can answer itemNotFound for a name the listing already holds.
func (s *MountSession) resolveConvergingPath(ctx context.Context, remotePath string) (*graph.Item, error) {
	item, err := s.ResolveItem(ctx, remotePath)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return nil, err
	}

	return s.resolveItemFromParentListing(ctx, remotePath)
}

// ListChildrenPages hands the children of the folder at remotePath to visit a
// page at a time (graph.Client.ListChildrenPages), for a caller that only
// walks them. The root ("") is the mount's root folder; below it, a mounted
// folder is walked down by name from its root item and listed by ID, and a
// drive's folder is listed by path.
func (s *MountSession) ListChildrenPages(ctx context.Context, remotePath string, visit graph.ChildrenPageFunc) error {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		if err := s.Meta.ListChildrenPages(ctx, s.DriveID, s.remoteRootItemID(), visit); err != nil {
			return fmt.Errorf("list root children: %w", err)
		}

		return nil
	}

	if s.hasMountRoot() {
		parent, err := s.resolveItemFromMountRoot(ctx, clean)
		if err != nil {
			return fmt.Errorf("list children for %q: %w", clean, err)
		}

		if err := s.Meta.ListChildrenPages(ctx, s.DriveID, parent.ID, visit); err != nil {
			return fmt.Errorf("list children for %q: %w", clean, err)
		}

		return nil
	}

	if err := s.Meta.ListChildrenByPathPages(ctx, s.DriveID, clean, visit); err != nil {
		return fmt.Errorf("list children for %q: %w", clean, err)
	}

	return nil
}

// ListChildren lists every child of the folder at remotePath, every page
// read, for a caller that needs the whole list (ls sorts it, a lookup by name
// matches over it); the folder is found as ListChildrenPages finds it.
func (s *MountSession) ListChildren(ctx context.Context, remotePath string) ([]graph.Item, error) {
	var items []graph.Item
	if err := s.ListChildrenPages(ctx, remotePath, func(page []graph.Item) bool {
		items = append(items, page...)
		return true
	}); err != nil {
		return nil, err
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

// PermanentDeleteItem permanently deletes an item, bypassing the recycle bin.
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

// MoveItem moves and/or renames an item. keepModified is the item's current
// modification time, which a rename on OneDrive would otherwise reset; zero
// leaves it to OneDrive.
func (s *Session) MoveItem(
	ctx context.Context, itemID, newParentID, newName string, keepModified time.Time,
) (*graph.Item, error) {
	item, err := s.Meta.MoveItem(ctx, s.DriveID, itemID, newParentID, newName, keepModified)
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

		if folded == nil && namekey.Equal(children[i].Name, name) {
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

// resolveItemFromParentListing finds the item at remotePath in the listing of
// its parent folder. A delete and a visibility wait both fall back to it when
// the read by path does not find the item.
func (s *MountSession) resolveItemFromParentListing(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		return nil, fmt.Errorf("resolve %q from the listing of its parent: %w", remotePath, graph.ErrNotFound)
	}

	parentPath, name := SplitParentAndName(clean)
	children, err := s.listParentChildren(ctx, parentPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %q from the listing of its parent: %w", clean, err)
	}

	match, ok := matchChildByName(children, name)
	if !ok {
		return nil, fmt.Errorf("resolve %q from the listing of parent %q: %w", clean, parentPath, graph.ErrNotFound)
	}

	return &match, nil
}

// listParentChildren lists the folder at parentPath with ListChildren. When a
// folder below the root is not found that way, it resolves the folder with
// resolveConvergingPath (a read by path, then its own parent's listing) and
// lists it by ID.
func (s *MountSession) listParentChildren(ctx context.Context, parentPath string) ([]graph.Item, error) {
	children, err := s.ListChildren(ctx, parentPath)
	if err == nil {
		return children, nil
	}
	if !errors.Is(err, graph.ErrNotFound) || parentPath == "" {
		return nil, err
	}

	parentItem, err := s.resolveConvergingPath(ctx, parentPath)
	if err != nil {
		return nil, err
	}

	children, err = s.Meta.ListChildren(ctx, s.DriveID, parentItem.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of parent %q by id: %w", parentPath, err)
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

// EnsureFolder creates a folder, returning the existing folder on 409 conflict:
// the folder of that name, or of a name OneDrive treats as the same, found by
// FindExistingChild. A file that holds the name is an error.
func (s *Session) EnsureFolder(ctx context.Context, parentID, name string) (*graph.Item, error) {
	item, err := s.CreateFolder(ctx, parentID, name)
	if !errors.Is(err, graph.ErrConflict) {
		return item, err
	}

	existing, found, findErr := FindExistingChild(ctx, s.Meta, s.DriveID, parentID, name)
	switch {
	case findErr != nil:
		return nil, fmt.Errorf("resolving existing folder %q: %w", name, findErr)
	case !found:
		return nil, fmt.Errorf("folder %q reported as existing but not found in parent", name)
	case !existing.IsFolder:
		return nil, fmt.Errorf("%q is a file on OneDrive", existing.Name)
	}

	return existing, nil
}

// ChildLookup reads one name in a folder and lists a folder; *graph.Client
// satisfies it.
type ChildLookup interface {
	GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

// FindExistingChild finds the item that holds name in the folder parentID
// after a create of name there was answered 409: the item of that name, or
// of a name OneDrive treats as the same (namekey.Equal). It reads the one
// name, so a folder of n children costs one request instead of a listing of
// n. It lists the folder, once, only when the read cannot settle the name: a
// name the path route cannot carry (graph.ErrNameNotPathAddressable), a read
// answered 400 (graph.ErrBadRequest; measured for a child named $value, which
// the listing holds), or a read that finds nothing equal although the create
// said the name exists. Any other failed read is the error. The bool is false
// when neither finds it. A listed item of the name that carries no ID is an
// error: no later request could name it.
func FindExistingChild(
	ctx context.Context, lookup ChildLookup, driveID driveid.ID, parentID, name string,
) (*graph.Item, bool, error) {
	item, err := lookup.GetChildByName(ctx, driveID, parentID, name)
	switch {
	case err == nil:
		return item, true, nil
	case !readCannotSettleName(err):
		return nil, false, fmt.Errorf("reading the name in the destination folder: %w", err)
	}

	children, err := lookup.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, false, fmt.Errorf("listing the destination folder: %w", err)
	}
	match, ok := matchChildByName(children, name)
	switch {
	case !ok:
		return nil, false, nil
	case match.ID == "":
		return nil, false, fmt.Errorf("the listing of the destination folder holds %q without an item ID", match.Name)
	}

	return &match, true, nil
}

// readCannotSettleName reports whether a failed read of one name leaves the
// name to the listing of its folder: the read found nothing, the route cannot
// carry the name, or Graph answered 400.
func readCannotSettleName(err error) bool {
	return errors.Is(err, graph.ErrNotFound) ||
		errors.Is(err, graph.ErrNameNotPathAddressable) ||
		errors.Is(err, graph.ErrBadRequest)
}
