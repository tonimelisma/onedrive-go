package driveops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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
// Every client it builds for one token path, drive sessions and account-level
// bootstrap clients alike, shares one TokenSource (R-6.8.27), preventing
// OAuth2 refresh token rotation races (two independent refreshes can
// invalidate each other's refresh tokens).
type SessionRuntime struct {
	holder       *config.Holder
	userAgent    string
	logger       *slog.Logger
	GraphBaseURL string

	// VisibilityWaitSchedule, when not empty, replaces the delays
	// retry.PathVisibilityPolicy gives the visibility wait and the delete
	// convergence of every session the runtime builds. Exported for test
	// injection, as TokenSourceFn is.
	VisibilityWaitSchedule []time.Duration

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
		Meta:                   meta,
		Transfer:               transfer,
		DriveID:                mount.DriveID,
		AccountEmailValue:      accountEmail,
		visibilityWaitSchedule: slices.Clone(r.VisibilityWaitSchedule),
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

// TokenSaveFailure is a refreshed sign-in a token source of the runtime holds
// in memory but could not write to its file.
type TokenSaveFailure struct {
	TokenPath string
	// Err wraps graph.ErrTokenNotSaved and names the file and the cause.
	Err error
}

// TokenSaveFailures lists, sorted by sign-in file, the refreshed sign-ins
// the runtime's token sources could not write to their files. It covers the
// sources loaded since the last FlushTokenCache: a failing source a flush let
// go of still retries its write whenever a session built before the flush
// uses it, but is no longer listed. The owner reports them: a command when it
// ends, a watch on its periodic tick (R-6.8.26).
func (r *SessionRuntime) TokenSaveFailures() []TokenSaveFailure {
	r.mu.Lock()
	loaded := make([]TokenSaveFailure, 0, len(r.tokenSources))
	sources := make(map[string]graph.TokenSource, len(r.tokenSources))
	for path, load := range r.tokenSources {
		select {
		case <-load.done:
		default:
			// A load under way has refreshed nothing yet.
			continue
		}
		if load.err == nil {
			loaded = append(loaded, TokenSaveFailure{TokenPath: path})
			sources[path] = load.ts
		}
	}
	r.mu.Unlock()

	slices.SortFunc(loaded, func(a, b TokenSaveFailure) int { return strings.Compare(a.TokenPath, b.TokenPath) })
	failures := loaded[:0]
	for _, failure := range loaded {
		if failure.Err = graph.TokenSaveError(sources[failure.TokenPath]); failure.Err != nil {
			failures = append(failures, failure)
		}
	}

	return failures
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
// with the mount's root item. Below a mounted folder it walks the path a name
// at a time (resolveItemFromMountRoot); below a drive's root it uses
// GetItemByPath. The path is cleaned first (CleanRemotePath), so "/" and ""
// both mean root and "a//b/" is "a/b".
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
// Answers for the path that disagree (graph.ErrPathAnswersDisagree) can settle
// the same way, so the wait reads the path again on them too, though it takes
// no item from a listing because of them; answers that still disagree when
// the schedule runs out end the wait as a path that is not visible yet, with
// the disagreement as its cause.
func (s *MountSession) WaitPathVisible(ctx context.Context, remotePath string) (*graph.Item, error) {
	item, err := s.resolveConvergingPath(ctx, remotePath)
	if err == nil {
		return item, nil
	}
	if !visibilityMaySettle(err) {
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
		if !visibilityMaySettle(err) {
			return nil, err
		}
	}

	notVisible := &PathNotVisibleError{Path: remotePath}
	if errors.Is(err, graph.ErrPathAnswersDisagree) {
		notVisible.Cause = err
	}

	return nil, notVisible
}

// visibilityMaySettle reports whether a failed resolution of a path can still
// turn into the item as OneDrive converges after a mutation: nothing found
// yet, or answers that disagree.
func visibilityMaySettle(err error) bool {
	return errors.Is(err, graph.ErrNotFound) || errors.Is(err, graph.ErrPathAnswersDisagree)
}

// resolveConvergingPath reads remotePath by path and, when that read finds
// nothing, looks for its name in the parent folder's listing: the path route
// can answer itemNotFound for a name the listing already holds. A read whose
// answers disagree (graph.ErrPathAnswersDisagree) is not "nothing", and is the
// error: a delete's target is not taken from a listing because of it.
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
// read, for a caller that needs the whole list (ls sorts it); the folder is
// found as ListChildrenPages finds it.
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

// resolveItemFromMountRoot walks remotePath, a path CleanRemotePath has
// cleaned, down from the mount's root item a name at a time, each found in the
// folder before it by FindExistingChild: one read of the name, a listing only
// for a name the read cannot settle.
func (s *MountSession) resolveItemFromMountRoot(ctx context.Context, remotePath string) (*graph.Item, error) {
	currentID := s.remoteRootItemID()
	segments := strings.Split(remotePath, "/")
	var current *graph.Item

	for idx, segment := range segments {
		child, err := FindExistingChild(ctx, s.Meta, s.DriveID, currentID, segment)
		switch {
		case err != nil:
			return nil, fmt.Errorf("segment %q: %w", segment, err)
		case child.Item == nil:
			return nil, fmt.Errorf("segment %q%s: %w", segment, child.ambiguity(), graph.ErrNotFound)
		}

		match := child.Item
		current = match
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

// resolveItemFromParentListing finds the name of remotePath in the listing of
// its parent folder (findInParentListing). A delete and a visibility wait
// both fall back to it when the read by path does not find the item.
func (s *MountSession) resolveItemFromParentListing(ctx context.Context, remotePath string) (*graph.Item, error) {
	clean := CleanRemotePath(remotePath)
	if clean == "" {
		return nil, fmt.Errorf("resolve %q from the listing of its parent: %w", remotePath, graph.ErrNotFound)
	}

	parentPath, name := SplitParentAndName(clean)
	child, err := s.findInParentListing(ctx, parentPath, name)
	if err != nil {
		return nil, fmt.Errorf("resolve %q from the listing of its parent: %w", clean, err)
	}
	if child.Item == nil {
		return nil, fmt.Errorf("resolve %q from the listing of parent %q%s: %w", clean, parentPath, child.ambiguity(), graph.ErrNotFound)
	}

	return child.Item, nil
}

// findInParentListing finds name in the listing of the folder at parentPath
// (ListChildrenPages), read up to the page that holds it (graph.NameSeek). A
// listing by ID names its folder; a listing by path hands on only children
// its answers place at the path (graph.Client.ListChildrenByPathPages). When
// a folder below the root is not found that way, it resolves the folder with
// resolveConvergingPath (a read by path, then its own parent's listing) and
// lists it by ID. Any other failure is the error, so answers that disagree
// on the folder at parentPath (graph.ErrPathAnswersDisagree) end the lookup
// instead of sending it to one of them.
func (s *MountSession) findInParentListing(ctx context.Context, parentPath, name string) (ChildByName, error) {
	seek := graph.NewNameSeek(name)
	err := s.ListChildrenPages(ctx, parentPath, seek.Visit)
	if err == nil {
		return childOf(seek), nil
	}
	if !errors.Is(err, graph.ErrNotFound) || parentPath == "" {
		return ChildByName{}, err
	}

	parentItem, err := s.resolveConvergingPath(ctx, parentPath)
	if err != nil {
		return ChildByName{}, err
	}

	seek = graph.NewNameSeek(name)
	if err := s.Meta.ListChildrenPages(ctx, s.DriveID, parentItem.ID, seek.Visit); err != nil {
		return ChildByName{}, fmt.Errorf("list children of parent %q by id: %w", parentPath, err)
	}

	return childOf(seek), nil
}

// ChildByName is what a lookup of one name in a folder found.
type ChildByName struct {
	// Item is the child that holds the name: the one spelled so, else the
	// only one OneDrive treats as that name. Nil when no child holds it, and
	// when Ambiguous.
	Item *graph.Item
	// Ambiguous reports that more than one child holds a name OneDrive
	// treats as the same and none is spelled as asked, so none is taken.
	Ambiguous bool
}

func childOf(seek *graph.NameSeek) ChildByName {
	item, match := seek.Match()

	return ChildByName{Item: item, Ambiguous: match.Ambiguous}
}

// ambiguity is the clause a not-found message gains when more than one
// child holds the name: empty when none does.
func (c ChildByName) ambiguity() string {
	if c.Ambiguous {
		return ": more than one item in the folder holds that name"
	}

	return ""
}

// CleanRemotePath is path in the one form a remote path takes: its names
// joined by single slashes, with no slash at either end and no empty name
// ("/a//b/" is "a/b"), as OneDrive holds no empty name; "" is the root.
func CleanRemotePath(path string) string {
	return strings.Join(strings.FieldsFunc(path, func(r rune) bool { return r == '/' }), "/")
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

	child, findErr := FindExistingChild(ctx, s.Meta, s.DriveID, parentID, name)
	existing := child.Item
	switch {
	case findErr != nil:
		return nil, fmt.Errorf("resolving existing folder %q: %w", name, findErr)
	case existing == nil:
		return nil, fmt.Errorf("folder %q reported as existing but not found in parent%s", name, child.ambiguity())
	case !existing.IsFolder:
		return nil, fmt.Errorf("%q is a file on OneDrive", existing.Name)
	}

	return existing, nil
}

// ChildLookup reads one name in a folder and lists a folder a page at a time;
// *graph.Client satisfies it.
type ChildLookup interface {
	GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	ListChildrenPages(ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc) error
}

// FindExistingChild finds the item that holds name in the folder parentID:
// the item of that name, or of a name OneDrive treats as the same
// (namekey.Equal). A create answered 409, an upload whose answer did not carry
// its item and each name of a path below a mounted folder are looked up this
// way. It reads the one name, so a folder of n children costs one request
// instead of a listing of n. It lists the folder only when the read cannot
// settle the name: a name the path route cannot carry
// (graph.ErrNameNotPathAddressable), a read answered 400 (graph.ErrBadRequest;
// measured for a child named $value, which the listing holds), or a read that
// finds nothing equal, which the listing can still hold while the path route
// lags behind it. The listing is read a page at a time and stops at the page
// that holds the name as spelled (graph.NameSeek); failing that spelling, a
// child is taken only when it is the one child of that name, and two are
// Ambiguous. Any other failed read is the error. The Item is nil when
// neither finds it. An item of the name that carries no ID is an error: no
// later request could name it.
func FindExistingChild(
	ctx context.Context, lookup ChildLookup, driveID driveid.ID, parentID, name string,
) (ChildByName, error) {
	item, err := lookup.GetChildByName(ctx, driveID, parentID, name)
	switch {
	case err == nil && item.ID == "":
		return ChildByName{}, fmt.Errorf("the read of %q in its folder answered without an item ID", item.Name)
	case err == nil:
		return ChildByName{Item: item}, nil
	case !graph.ReadCannotSettleName(err):
		return ChildByName{}, fmt.Errorf("reading the name in its folder: %w", err)
	}

	seek := graph.NewNameSeek(name)
	if err := lookup.ListChildrenPages(ctx, driveID, parentID, seek.Visit); err != nil {
		return ChildByName{}, fmt.Errorf("listing the folder: %w", err)
	}
	child := childOf(seek)
	if child.Item != nil && child.Item.ID == "" {
		return ChildByName{}, fmt.Errorf("the listing of the folder holds %q without an item ID", child.Item.Name)
	}

	return child, nil
}

