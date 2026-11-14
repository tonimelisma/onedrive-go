package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

const (
	statusLiveTestEmailA    = "first@example.com"
	statusLiveTestEmailB    = "second@example.com"
	statusLiveTestLocalName = "Local Name"
	statusLiveTestLiveName  = "Live Name"

	// statusLiveTestGuard fails a test whose status run never returns, the
	// behavior before the budget existed. A passing run takes milliseconds.
	statusLiveTestGuard = 30 * time.Second

	statusLiveTestNotePrefix   = "Note: showing what is known locally for "
	statusLiveTestUnanswered   = "OneDrive did not answer within 10 s"
	statusLiveTestStorageLabel = "Storage: "
)

// statusLiveTestBudget stands in for the 10 s budget of the live details. A
// test's fake OneDrive ends it with spend while the request the test is about
// is in flight, and a test about what OneDrive answered never ends it, so no
// test waits for the clock or races it.
type statusLiveTestBudget struct {
	once  sync.Once
	spent chan struct{}
}

func newStatusLiveTestBudget() *statusLiveTestBudget {
	return &statusLiveTestBudget{spent: make(chan struct{})}
}

// spend ends the budget, as the clock does for the command.
func (b *statusLiveTestBudget) spend() {
	b.once.Do(func() { close(b.spent) })
}

// context is the command's statusLiveBudgetContext seam: ctx, which ends with
// the cause the command's own budget ends with once the test spends the budget.
func (b *statusLiveTestBudget) context(ctx context.Context) (context.Context, context.CancelFunc) {
	askCtx, cancel := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-b.spent:
			cancel(errStatusLiveBudgetSpent)
		case <-askCtx.Done():
		}
	}()

	return askCtx, func() { cancel(context.Canceled) }
}

// accountTokenSource hands each sign-in file a token of its own, so the fake
// Graph server can tell the accounts apart. It cannot force a refresh, so a
// 401 stays final, as for a saved login Microsoft rejected.
type accountTokenSource struct {
	token string
}

func (s accountTokenSource) Token() (string, error) { return s.token, nil }

// statusLiveRun is one status command against a fake Graph server, with
// stdout and stderr kept apart.
type statusLiveRun struct {
	cc     *CLIContext
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// newStatusLiveRun configures one personal drive per email, each with a valid
// saved login and a local display name, and points the command's session
// runtime at graphURL. budget replaces the live overlay's budget: it ends
// when the test spends it and not before.
func newStatusLiveRun(t *testing.T, graphURL string, budget *statusLiveTestBudget, emails ...string) *statusLiveRun {
	t.Helper()
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	for i, email := range emails {
		cid := driveid.MustCanonicalID("personal:" + email)
		require.NoError(t, config.AppendDriveSection(cfgPath, cid, fmt.Sprintf("~/OneDrive-%d", i)))
		seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
			account.DisplayName = statusLiveTestLocalName
		})
		seedCatalogDrive(t, cid, nil)
		writeAccessTokenFile(t, cid, fmt.Sprintf("access-%d", i))
	}

	logger := slog.New(slog.DiscardHandler)
	runtime := driveops.NewSessionRuntime(nil, "test-agent", logger)
	runtime.GraphBaseURL = graphURL
	runtime.TokenSourceFn = func(_ context.Context, tokenPath string, _ *slog.Logger) (graph.TokenSource, error) {
		return accountTokenSource{token: filepath.Base(tokenPath)}, nil
	}

	run := &statusLiveRun{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	run.cc = &CLIContext{
		Logger:       logger,
		OutputWriter: run.stdout,
		StatusWriter: run.stderr,
		CfgPath:      cfgPath,
		Runtime:      runtime,
		seams:        cliSeams{statusLiveBudgetContext: budget.context},
	}

	return run
}

// status runs the status command and fails the test if it does not return
// within statusLiveTestGuard. The command goroutine is waited for in cleanup,
// after its context is canceled, so a failing run does not outlive the test.
func (r *statusLiveRun) status(t *testing.T, ctx context.Context) error {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runStatusCommand(ctx, r.cc, false) }()

	var result error
	returned := false
	t.Cleanup(func() {
		cancel()
		if !returned {
			<-done
		}
	})

	select {
	case result = <-done:
		returned = true
	case <-time.After(statusLiveTestGuard):
		require.FailNow(t, "status did not return", "it waited on OneDrive for more than %s", statusLiveTestGuard)
	}

	return result
}

// notes returns the stderr lines status wrote about accounts it could not
// get live details for.
func (r *statusLiveRun) notes() []string {
	var notes []string
	for line := range strings.SplitSeq(strings.TrimRight(r.stderr.String(), "\n"), "\n") {
		if strings.HasPrefix(line, statusLiveTestNotePrefix) {
			notes = append(notes, line)
		}
	}

	return notes
}

// newStatusGraphServer serves handler and releases every request still
// waiting in it before the server closes, so cleanup never waits on a
// request the test left blocked.
func newStatusGraphServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) *httptest.Server {
	t.Helper()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, release)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	return srv
}

// blockUntilRequestEnds holds a request until the client gives up on it.
func blockUntilRequestEnds(r *http.Request, release <-chan struct{}) {
	select {
	case <-r.Context().Done():
	case <-release:
	}
}

func writeStatusTestUser(t *testing.T, w http.ResponseWriter, displayName string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	writeTestResponsef(t, w, `{"id":"user-1","displayName":%q}`, displayName)
}

func writeStatusTestPrimaryDrive(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	writeTestResponse(t, w, `{"id":"drive-1","driveType":"personal","quota":{"used":1048576,"total":5368709120}}`)
}

// A sign-in failure is an answer, not a slow OneDrive: the account is marked
// as needing sign-in in the output and in the catalog, with no note.
//
// Validates: R-6.8.28
func TestStatus_ASignInFailureStillMarksTheAccount(t *testing.T) {
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		assert.Equal(t, testGraphMePath, r.URL.Path, "a rejected sign-in asks for nothing else")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		writeTestResponse(t, w, `{"error":{"code":"InvalidAuthenticationToken","message":"Access token is invalid."}}`)
	})

	t.Run("text", func(t *testing.T) {
		run := newStatusLiveRun(t, srv.URL, newStatusLiveTestBudget(), statusLiveTestEmailA)

		require.NoError(t, run.status(t, t.Context()))
		assert.Contains(t, run.stdout.String(), "Sign-in required: ")
		assert.Empty(t, run.notes())
		assert.True(t, hasPersistedAccountAuthRequirement(t, statusLiveTestEmailA))
	})

	t.Run("json", func(t *testing.T) {
		run := newStatusLiveRun(t, srv.URL, newStatusLiveTestBudget(), statusLiveTestEmailA)
		run.cc.Flags.JSON = true

		require.NoError(t, run.status(t, t.Context()))
		doc := decodeOneStatusDocument(t, run.stdout)
		require.Len(t, doc.Accounts, 1)
		require.NotNil(t, doc.Accounts[0].SignInRequired)
		assert.Equal(t, 1, doc.Summary.AccountsRequiringAuth)
		assert.Empty(t, run.notes())
	})
}

// A OneDrive that never answers costs status its budget, not its retry
// ladder: the local state prints, with one stderr note for the account, and
// --json keeps stdout one document.
//
// Validates: R-6.8.28
func TestStatus_PrintsLocalStateWhenOneDriveDoesNotAnswerInTheBudget(t *testing.T) {
	// The budget ends while the first request waits for an answer that never
	// comes.
	newRun := func(t *testing.T) *statusLiveRun {
		t.Helper()

		budget := newStatusLiveTestBudget()
		srv := newStatusGraphServer(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
			budget.spend()
			blockUntilRequestEnds(r, release)
		})

		return newStatusLiveRun(t, srv.URL, budget, statusLiveTestEmailA)
	}

	t.Run("text", func(t *testing.T) {
		run := newRun(t)

		require.NoError(t, run.status(t, t.Context()))
		out := run.stdout.String()
		assert.Contains(t, out, statusAccountLine(statusLiveTestLocalName, statusLiveTestEmailA))
		assert.Contains(t, out, "Folder: ")
		assert.NotContains(t, out, statusLiveTestStorageLabel)
		assert.Equal(t, []string{
			statusLiveTestNotePrefix + statusLiveTestEmailA + ": " + statusLiveTestUnanswered + ".",
		}, run.notes())
		assert.Equal(t, run.notes(), strings.Split(strings.TrimRight(run.stderr.String(), "\n"), "\n"),
			"the note is all status writes to stderr")
	})

	t.Run("json", func(t *testing.T) {
		run := newRun(t)
		run.cc.Flags.JSON = true

		require.NoError(t, run.status(t, t.Context()))
		doc := decodeOneStatusDocument(t, run.stdout)
		require.Len(t, doc.Accounts, 1)
		assert.Equal(t, statusLiveTestEmailA, doc.Accounts[0].Email)
		assert.Equal(t, statusLiveTestLocalName, doc.Accounts[0].DisplayName)
		assert.Len(t, run.notes(), 1)
	})
}

// The accounts are asked at the same time: each account's /me is answered
// only once both are in flight, so asking one after the other never gets an
// answer, and asking together gets both accounts' live details.
//
// Validates: R-6.8.28
func TestStatus_AsksAccountsAtTheSameTime(t *testing.T) {
	var (
		mu      sync.Mutex // guards arrived and bothIn's closing
		arrived = map[string]bool{}
	)
	bothIn := make(chan struct{})

	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		switch r.URL.Path {
		case testGraphMePath:
			account := r.Header.Get("Authorization")
			mu.Lock()
			if !arrived[account] {
				arrived[account] = true
				if len(arrived) == 2 {
					close(bothIn)
				}
			}
			mu.Unlock()

			select {
			case <-bothIn:
				writeStatusTestUser(t, w, statusLiveTestLiveName)
			case <-r.Context().Done():
			case <-release:
			}
		case primaryDrivePath:
			writeStatusTestPrimaryDrive(t, w)
		default:
			http.NotFound(w, r)
		}
	})

	run := newStatusLiveRun(t, srv.URL, newStatusLiveTestBudget(), statusLiveTestEmailA, statusLiveTestEmailB)

	require.NoError(t, run.status(t, t.Context()))
	out := run.stdout.String()
	assert.Contains(t, out, statusAccountLine(statusLiveTestLiveName, statusLiveTestEmailA))
	assert.Contains(t, out, statusAccountLine(statusLiveTestLiveName, statusLiveTestEmailB))
	assert.Equal(t, 2, strings.Count(out, statusLiveTestStorageLabel), "both accounts' storage arrived")
	assert.Empty(t, run.stderr.String())
}

// The budget covers every live request of the overlay, the storage lookups
// after /me included: the name that arrived shows, the storage that did not
// is left out, and the note says why. The budget ends while the storage
// request waits, after /me was answered, whatever the machine's speed.
//
// Validates: R-6.8.28
func TestStatus_StorageCutShortByTheBudgetIsNoted(t *testing.T) {
	budget := newStatusLiveTestBudget()
	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		switch r.URL.Path {
		case testGraphMePath:
			writeStatusTestUser(t, w, statusLiveTestLiveName)
		default:
			budget.spend()
			blockUntilRequestEnds(r, release)
		}
	})

	run := newStatusLiveRun(t, srv.URL, budget, statusLiveTestEmailA)

	require.NoError(t, run.status(t, t.Context()))
	out := run.stdout.String()
	assert.Contains(t, out, statusAccountLine(statusLiveTestLiveName, statusLiveTestEmailA))
	assert.NotContains(t, out, statusLiveTestStorageLabel)
	notes := run.notes()
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], statusLiveTestUnanswered)
}

// A request that fails says why in the note, in the user's words: OneDrive's
// own message for a refusal, the budget for a server error the retries had
// not got past when it ran out.
//
// Validates: R-6.8.28
func TestStatus_SaysWhyOneDriveGaveNoDetails(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		// budgetEnds ends the budget once OneDrive has answered a request,
		// while the command still retries.
		budgetEnds bool
		reason     string
	}{
		{
			name:   "a refusal passes on OneDrive's message",
			status: http.StatusForbidden,
			body:   `{"error":{"code":"accessDenied","message":"The caller cannot read this profile."}}`,
			// OneDrive's own full stop is dropped: the note ends the sentence.
			reason: "OneDrive refused the request: The caller cannot read this profile",
		},
		{
			name:       "a server error still failing when the budget ends",
			status:     http.StatusServiceUnavailable,
			body:       `{"error":{"code":"serviceNotAvailable","message":"Try again later."}}`,
			budgetEnds: true,
			reason:     statusLiveTestUnanswered,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			budget := newStatusLiveTestBudget()
			srv := newStatusGraphServer(t, func(w http.ResponseWriter, _ *http.Request, _ <-chan struct{}) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				writeTestResponse(t, w, tc.body)
				if tc.budgetEnds {
					budget.spend()
				}
			})
			run := newStatusLiveRun(t, srv.URL, budget, statusLiveTestEmailA)

			require.NoError(t, run.status(t, t.Context()))
			assert.Contains(t, run.stdout.String(), statusAccountLine(statusLiveTestLocalName, statusLiveTestEmailA))
			assert.Equal(t, []string{statusLiveTestNotePrefix + statusLiveTestEmailA + ": " + tc.reason + "."}, run.notes())
		})
	}
}

// A command the user stopped is not a slow OneDrive: status prints its local
// state and adds no note about requests the stop canceled, about OneDrive
// or about the running sync.
//
// Validates: R-6.8.28, R-2.9.3, R-6.3.6
func TestStatus_AStoppedCommandAddsNoNote(t *testing.T) {
	srv := newStatusGraphServer(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		blockUntilRequestEnds(r, release)
	})
	run := newStatusLiveRun(t, srv.URL, newStatusLiveTestBudget(), statusLiveTestEmailA)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.NoError(t, run.status(t, ctx))
	assert.Contains(t, run.stdout.String(), statusAccountLine(statusLiveTestLocalName, statusLiveTestEmailA))
	assert.Empty(t, run.stderr.String())
}

// panickingTokenSource stands for a bug on one account's request path.
type panickingTokenSource struct{}

func (panickingTokenSource) Token() (string, error) { panic("token source bug") }

// A panic while one account's live details are asked for costs that account
// its details, not the command: status prints the account's local state with
// a note, and the other account's live details arrive. The test runs in a
// child test binary, where a panic that escaped the goroutine would end the
// child instead of every test of the package.
//
// Validates: R-6.3.10, R-6.8.28
func TestStatus_APanicAskingOneAccountLeavesItsLocalStateAndTheOthersDetails(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	srv := newStatusGraphServer(t, func(w http.ResponseWriter, r *http.Request, _ <-chan struct{}) {
		switch r.URL.Path {
		case testGraphMePath:
			writeStatusTestUser(t, w, statusLiveTestLiveName)
		case primaryDrivePath:
			writeStatusTestPrimaryDrive(t, w)
		default:
			http.NotFound(w, r)
		}
	})

	run := newStatusLiveRun(t, srv.URL, newStatusLiveTestBudget(), statusLiveTestEmailA, statusLiveTestEmailB)
	run.cc.Runtime.TokenSourceFn = func(_ context.Context, tokenPath string, _ *slog.Logger) (graph.TokenSource, error) {
		if strings.Contains(filepath.Base(tokenPath), statusLiveTestEmailB) {
			return panickingTokenSource{}, nil
		}

		return accountTokenSource{token: filepath.Base(tokenPath)}, nil
	}

	require.NoError(t, run.status(t, t.Context()))
	out := run.stdout.String()
	assert.Contains(t, out, statusAccountLine(statusLiveTestLiveName, statusLiveTestEmailA))
	assert.Contains(t, out, statusAccountLine(statusLiveTestLocalName, statusLiveTestEmailB))
	assert.Equal(t, 1, strings.Count(out, statusLiveTestStorageLabel), "only the first account's storage arrived")
	assert.Equal(t, []string{
		statusLiveTestNotePrefix + statusLiveTestEmailB + ": " + statusLiveInternalErrorReason + ".",
	}, run.notes())
}

// statusAccountLine is the text status prints to head an account.
func statusAccountLine(displayName, email string) string {
	return "Account: " + displayName + " <" + email + ">"
}

// decodeOneStatusDocument decodes stdout as one status document and fails if
// anything follows it.
func decodeOneStatusDocument(t *testing.T, stdout *bytes.Buffer) statusOutput {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var doc statusOutput
	require.NoError(t, dec.Decode(&doc))
	_, err := dec.Token()
	require.ErrorIs(t, err, io.EOF, "stdout holds one JSON document and nothing else")

	return doc
}
