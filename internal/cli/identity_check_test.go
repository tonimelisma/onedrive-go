package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

const (
	identityTestEmail    = "user@example.com"
	identityTestRenamed  = "renamed@example.com"
	identityTestUserID   = "user-identity"
	identityTestDriveID  = "0000000000000001"
	identityTestSyncDir  = "~/OneDrive"
	identityTestAccounts = 4

	// identityTestGuard fails a test whose command never returns, which is
	// what a check without a budget did against a OneDrive that did not
	// answer. A passing run takes milliseconds.
	identityTestGuard = 30 * time.Second
)

// identityTestNow is the injected clock's time, whole seconds so the recorded
// RFC 3339 text compares exactly.
var identityTestNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) //nolint:gochecknoglobals // fixed test clock

// identityTestClock is the injected clock: a time the test moves.
type identityTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *identityTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *identityTestClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// identityTestBudget stands in for the identity check's 5 s budget: the
// fake OneDrive spends it while the request is in flight. Its context also
// carries statusLiveTestRetryDeadline, as the real budget carries a deadline:
// far beyond any passing run and shorter than the Retry-After the fake
// OneDrive sends with a server error, so the retrying transport returns that
// error at once instead of waiting (R-6.8.8). No test waits for the clock.
type identityTestBudget struct {
	once  sync.Once
	spent chan struct{}
}

func newIdentityTestBudget() *identityTestBudget {
	return &identityTestBudget{spent: make(chan struct{})}
}

func (b *identityTestBudget) spend() {
	b.once.Do(func() { close(b.spent) })
}

func (b *identityTestBudget) context(ctx context.Context) (context.Context, context.CancelFunc) {
	askCtx, cancel := context.WithCancelCause(ctx)
	askCtx, stopDeadline := context.WithTimeoutCause(askCtx, statusLiveTestRetryDeadline, errIdentityCheckBudgetSpent)
	// The command calls this seam off the test's goroutine; the returned
	// cancel, which the command defers, ends this goroutine.
	go func() {
		select {
		case <-b.spent:
			cancel(errIdentityCheckBudgetSpent)
		case <-askCtx.Done():
		}
	}()

	return askCtx, func() {
		stopDeadline()
		cancel(context.Canceled)
	}
}

// identityTestRun is one configured personal drive whose account the
// catalog knows, a saved login, and a CLI context pointed at graphURL.
type identityTestRun struct {
	cid    driveid.CanonicalID
	clock  *identityTestClock
	budget *identityTestBudget
	status *bytes.Buffer
	cc     *CLIContext
}

func newIdentityTestRun(t *testing.T, graphURL string, dueAt string) *identityTestRun {
	t.Helper()
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:" + identityTestEmail)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(t.Context(), configPath, cid, identityTestSyncDir))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = identityTestUserID
		account.PrimaryDriveID = identityTestDriveID
		account.PrimaryDriveCanonical = cid.String()
		account.IdentityCheckDueAt = dueAt
	})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = identityTestDriveID
	})
	writeAccessTokenFile(t, cid, "token-identity")

	rawCfg, err := config.LoadOrDefault(configPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	runtime := driveops.NewSessionRuntime(config.NewHolder(rawCfg, configPath), "test-agent", slog.New(slog.DiscardHandler))
	runtime.GraphBaseURL = graphURL

	run := &identityTestRun{
		cid:    cid,
		clock:  &identityTestClock{now: identityTestNow},
		budget: newIdentityTestBudget(),
		status: &bytes.Buffer{},
	}
	run.cc = &CLIContext{
		Flags:        CLIFlags{ConfigPath: configPath, Drive: []string{cid.String()}},
		Logger:       testDriveLogger(t),
		OutputWriter: io.Discard,
		StatusWriter: run.status,
		CfgPath:      configPath,
		GraphBaseURL: graphURL,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			DriveID:     driveid.New(identityTestDriveID),
			SyncDir:     identityTestSyncDir,
		},
		Runtime: runtime,
		seams: cliSeams{
			now:                        run.clock.Now,
			identityCheckBudgetContext: run.budget.context,
		},
	}

	return run
}

// dueAt is the account's recorded next identity check.
func identityCheckDueAt(t *testing.T, cid driveid.CanonicalID) string {
	t.Helper()

	catalog, err := config.LoadCatalog()
	require.NoError(t, err)
	account, found := catalog.AccountByCanonicalID(cid)
	require.True(t, found, "account %s in the catalog", cid)

	return account.IdentityCheckDueAt
}

func rfc3339(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// writeIdentityTestUser answers /me with the account's user and email.
func writeIdentityTestUser(t *testing.T, w http.ResponseWriter, email string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	writeTestResponsef(t, w, `{"id":%q,"displayName":"Identity User","mail":%q}`, identityTestUserID, email)
}

// writeIdentityTestServerError answers with a server error whose Retry-After
// outlasts any budget, so the retrying transport returns it at once.
func writeIdentityTestServerError(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", statusLiveTestRetryAfter)
	w.WriteHeader(http.StatusServiceUnavailable)
	writeTestResponse(t, w, `{"error":{"code":"serviceNotAvailable","message":"Try again later."}}`)
}

// receiveIdentityResult reads a background run's result, failing the test if
// it does not arrive within identityTestGuard.
func receiveIdentityResult[T any](t *testing.T, done <-chan T) T {
	t.Helper()

	select {
	case result := <-done:
		return result
	case <-time.After(identityTestGuard):
		require.FailNow(t, "the command did not return", "it waited on the identity check for more than %s", identityTestGuard)
		var zero T
		return zero
	}
}

// Validates: R-6.8.31
func TestSession_AFailedIdentityCheckDoesNotFailTheCommand(t *testing.T) {
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		assert.Equal(t, testGraphMePath, r.URL.Path)
		writeIdentityTestServerError(t, w)
	})
	run := newIdentityTestRun(t, srv.URL, "")

	session, err := run.cc.Session(t.Context())
	require.NoError(t, err, "a failed identity check does not fail the command")
	require.NotNil(t, session)
	assert.Equal(t, run.cid, run.cc.Cfg.CanonicalID, "the stored identity stands")
	assert.Equal(t, rfc3339(identityTestNow.Add(config.IdentityCheckUnansweredInterval)), identityCheckDueAt(t, run.cid),
		"asked again in 15 minutes")
}

// Validates: R-6.8.31
func TestSession_AnIdentityCheckThatDoesNotAnswerEndsAtItsBudget(t *testing.T) {
	var run *identityTestRun
	srv := newStatusGraphServer(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		assert.Equal(t, testGraphMePath, r.URL.Path)
		run.budget.spend()
		blockUntilRequestEnds(r, release)
	})
	run = newIdentityTestRun(t, srv.URL, "")

	type sessionResult struct {
		session *driveops.MountSession
		err     error
	}
	done := make(chan sessionResult, 1)
	go func() {
		session, err := run.cc.Session(t.Context())
		done <- sessionResult{session: session, err: err}
	}()
	result := receiveIdentityResult(t, done)

	require.NoError(t, result.err)
	require.NotNil(t, result.session)
	assert.Equal(t, rfc3339(identityTestNow.Add(config.IdentityCheckUnansweredInterval)), identityCheckDueAt(t, run.cid),
		"a check its budget cut short is asked again in 15 minutes")
}

// Validates: R-6.8.31, R-3.3.1
func TestSession_ASignInFailureFromTheIdentityCheckStillFailsTheCommand(t *testing.T) {
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		writeTestResponse(t, w, `{"error":{"code":"InvalidAuthenticationToken","message":"expired"}}`)
	})
	run := newIdentityTestRun(t, srv.URL, "")

	session, err := run.cc.Session(t.Context())
	require.ErrorIs(t, err, graph.ErrUnauthorized)
	assert.Nil(t, session)
	assert.Empty(t, identityCheckDueAt(t, run.cid), "a sign-in failure is asked again by the next command")
}

// Validates: R-6.8.31
func TestProbeAccountIdentity_SkipsWhileNotDue(t *testing.T) {
	var meCalls atomic.Int32
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		meCalls.Add(1)
		writeIdentityTestUser(t, w, identityTestEmail)
	})
	dueAt := rfc3339(identityTestNow.Add(time.Hour))
	run := newIdentityTestRun(t, srv.URL, dueAt)

	result, err := run.cc.probeAccountIdentity(t.Context(), run.cid, "test")
	require.NoError(t, err)
	assert.False(t, result.Changed())
	assert.Zero(t, meCalls.Load(), "no request while the check is not due")
	assert.Equal(t, dueAt, identityCheckDueAt(t, run.cid))
}

// Validates: R-6.8.31
func TestProbeAccountIdentity_AsksWhenDue(t *testing.T) {
	cases := []struct {
		name  string
		dueAt string
	}{
		{name: "a record without the time", dueAt: ""},
		{name: "a time that has passed", dueAt: rfc3339(identityTestNow.Add(-time.Minute))},
		{name: "a time further ahead than a day, left by a clock that went back", dueAt: rfc3339(identityTestNow.Add(25 * time.Hour))},
		{name: "a time that does not parse", dueAt: "tomorrow"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var meCalls atomic.Int32
			srv := newStatusGraphServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
				meCalls.Add(1)
				writeIdentityTestUser(t, w, identityTestEmail)
			})
			run := newIdentityTestRun(t, srv.URL, tc.dueAt)

			_, err := run.cc.probeAccountIdentity(t.Context(), run.cid, "test")
			require.NoError(t, err)
			assert.Equal(t, int32(1), meCalls.Load())
			assert.Equal(t, rfc3339(identityTestNow.Add(config.IdentityCheckAnsweredInterval)), identityCheckDueAt(t, run.cid),
				"an answered check is next due in 24 hours")
		})
	}
}

// Validates: R-6.8.31
func TestProbeAccountIdentity_AFailureIsRetriedAfterFifteenMinutes(t *testing.T) {
	var meCalls atomic.Int32
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		if meCalls.Add(1) == 1 {
			writeIdentityTestServerError(t, w)
			return
		}
		writeIdentityTestUser(t, w, identityTestEmail)
	})
	run := newIdentityTestRun(t, srv.URL, "")

	_, err := run.cc.probeAccountIdentity(t.Context(), run.cid, "test")
	require.NoError(t, err)
	require.Equal(t, int32(1), meCalls.Load())

	run.clock.set(identityTestNow.Add(config.IdentityCheckUnansweredInterval - time.Second))
	_, err = run.cc.probeAccountIdentity(t.Context(), run.cid, "test")
	require.NoError(t, err)
	assert.Equal(t, int32(1), meCalls.Load(), "not asked again within 15 minutes")

	retryAt := identityTestNow.Add(config.IdentityCheckUnansweredInterval)
	run.clock.set(retryAt)
	_, err = run.cc.probeAccountIdentity(t.Context(), run.cid, "test")
	require.NoError(t, err)
	assert.Equal(t, int32(2), meCalls.Load(), "asked again after 15 minutes")
	assert.Equal(t, rfc3339(retryAt.Add(config.IdentityCheckAnsweredInterval)), identityCheckDueAt(t, run.cid))
}

// Validates: R-6.8.31, R-3.7
func TestProbeAccountIdentity_AnEmailChangeIsStillAppliedWhenDue(t *testing.T) {
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
		writeIdentityTestUser(t, w, identityTestRenamed)
	})
	run := newIdentityTestRun(t, srv.URL, "")

	result, err := run.cc.probeAccountIdentity(t.Context(), run.cid, "test")
	require.NoError(t, err)
	require.True(t, result.Changed())

	renamed := driveid.MustCanonicalID("personal:" + identityTestRenamed)
	assert.Equal(t, rfc3339(identityTestNow.Add(config.IdentityCheckAnsweredInterval)), identityCheckDueAt(t, renamed),
		"the renamed account's check is next due in 24 hours")
	assert.Contains(t, run.status.String(), "Account email changed from "+identityTestEmail+" to "+identityTestRenamed)
}

// A check the command's own stop cut short says nothing about OneDrive, so
// it records nothing and the next command asks.
//
// Validates: R-6.8.31
func TestProbeAccountIdentity_AnInterruptedCheckRecordsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := newStatusGraphServer(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		cancel()
		blockUntilRequestEnds(r, release)
	})
	run := newIdentityTestRun(t, srv.URL, "")

	_, err := run.cc.probeAccountIdentity(ctx, run.cid, "test")
	require.NoError(t, err)
	assert.Empty(t, identityCheckDueAt(t, run.cid))
}

// The account views ask every account at the same time: the fake OneDrive
// answers none until all have asked. The checks then end together, and each
// keeps its record in the catalog.
//
// Validates: R-6.8.31
func TestAccountViewSnapshot_AsksEveryAccountAtOnceAndKeepsEachRecord(t *testing.T) {
	setTestDriveHome(t)

	cids := make([]driveid.CanonicalID, 0, identityTestAccounts)
	for i := range identityTestAccounts {
		cid := driveid.MustCanonicalID(fmt.Sprintf("personal:user%d@example.com", i))
		cids = append(cids, cid)
		seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
			account.UserID = fmt.Sprintf("user-%d", i)
		})
		writeAccessTokenFile(t, cid, fmt.Sprintf("token-%d", i))
	}

	var arrived sync.WaitGroup
	arrived.Add(identityTestAccounts)
	allArrived := make(chan struct{})
	go func() {
		arrived.Wait()
		close(allArrived)
	}()
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		index := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer token-")
		arrived.Done()
		select {
		case <-allArrived:
		case <-r.Context().Done():
			return
		case <-release:
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{"id":"user-%s","displayName":"User","mail":"user%s@example.com"}`, index, index)
	})

	clock := &identityTestClock{now: identityTestNow}
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: io.Discard,
		StatusWriter: io.Discard,
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		GraphBaseURL: srv.URL,
		seams:        cliSeams{now: clock.Now},
	}

	done := make(chan error, 1)
	go func() {
		_, err := loadAccountViewSnapshotWithBestEffortIdentityRefresh(t.Context(), cc)
		done <- err
	}()
	require.NoError(t, receiveIdentityResult(t, done))

	for _, cid := range cids {
		assert.Equal(t, rfc3339(identityTestNow.Add(config.IdentityCheckAnsweredInterval)), identityCheckDueAt(t, cid),
			"account %s keeps its record", cid)
	}
}

// Validates: R-6.8.31
func TestSharedTarget_AFailedIdentityCheckDoesNotFailTheCommand(t *testing.T) {
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		assert.Equal(t, testGraphMePath, r.URL.Path)
		writeIdentityTestServerError(t, w)
	})
	run := newIdentityTestRun(t, srv.URL, "")

	clients, err := run.cc.sharedTargetClients(t.Context(), sharedref.Ref{
		AccountEmail:  identityTestEmail,
		RemoteDriveID: "b!remote-drive",
		RemoteItemID:  "remote-item",
	})
	require.NoError(t, err, "a failed identity check does not fail a shared-item command")
	require.NotNil(t, clients)

	client, email, err := run.cc.sharedBootstrapMetaClient(t.Context(), identityTestEmail)
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, identityTestEmail, email)
}

// drive list waits for no identity answer longer than the check's budget,
// and lists every configured and discovered drive.
//
// Validates: R-6.8.31, R-3.6.5
func TestDriveList_ASlowIdentityAnswerDoesNotDelayTheListing(t *testing.T) {
	var run *identityTestRun
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testGraphMePath:
			run.budget.spend()
			blockUntilRequestEnds(r, release)
		case graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"0000000000000001","name":"OneDrive","driveType":"personal"}]}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"0000000000000001","name":"OneDrive","driveType":"personal"}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{"value":[]}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	})
	run = newIdentityTestRun(t, srv.URL, "")
	var out bytes.Buffer
	run.cc.Flags = CLIFlags{JSON: true}
	run.cc.OutputWriter = &out

	done := make(chan error, 1)
	go func() {
		done <- runDriveListWithContext(t.Context(), run.cc, false)
	}()
	require.NoError(t, receiveIdentityResult(t, done))

	var decoded driveListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Configured, 1)
	assert.Equal(t, run.cid.String(), decoded.Configured[0].CanonicalID)
	assert.Empty(t, decoded.AccountsDegraded)
}
