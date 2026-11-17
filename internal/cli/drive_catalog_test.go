package cli

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	gosynctest "testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// discoveryTestAccounts are count signed-in personal accounts, as the account
// snapshot hands them to drive discovery, sorted by email.
func discoveryTestAccounts(count int) []accountView {
	accounts := make([]accountView, 0, count)
	for i := range count {
		email := fmt.Sprintf("user%02d@example.com", i)
		accounts = append(accounts, accountView{
			Email:                 email,
			DriveType:             driveid.DriveTypePersonal,
			RepresentativeTokenID: driveid.MustCanonicalID(driveid.DriveTypePersonal + ":" + email),
		})
	}

	return accounts
}

// discoveryTestRuntime is a runtime whose every sign-in load is loadToken, so
// a discovery under test sends no request.
func discoveryTestRuntime(loadToken func(tokenPath string) (graph.TokenSource, error)) *driveops.SessionRuntime {
	runtime := driveops.NewSessionRuntime(nil, "test/1.0", slog.New(slog.DiscardHandler))
	runtime.TokenSourceFn = func(tokenPath string, _ *slog.Logger) (graph.TokenSource, error) {
		return loadToken(tokenPath)
	}

	return runtime
}

// Drive discovery reads at most discoveryConcurrency accounts at once, however
// many are signed in, and still reads every one of them. Each account's
// sign-in load holds its read open until the test releases it; inside the
// bubble, once every goroutine is blocked, the loads under way are exactly
// the accounts read at once.
//
// Validates: R-3.3.2
func TestDiscoverAvailableDrives_ReadsAtMostTheDiscoveryBoundOfAccountsAtOnce(t *testing.T) {
	setTestDriveHome(t)
	accounts := discoveryTestAccounts(discoveryConcurrency + 3)

	gosynctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var (
			mu      sync.Mutex // guards reading
			reading int
		)
		runtime := discoveryTestRuntime(func(string) (graph.TokenSource, error) {
			mu.Lock()
			reading++
			mu.Unlock()
			<-release

			return nil, graph.ErrNotLoggedIn
		})
		logger := slog.New(slog.DiscardHandler)

		authRequired := make(chan []accountAuthRequirement, 1)
		go func() {
			_, required, _, _ := discoverAvailableDrives(t.Context(), config.DefaultConfig(), accounts,
				sharePointSiteLimit, logger, newAuthProofRecorder(logger), runtime)
			authRequired <- required
		}()

		gosynctest.Wait()
		mu.Lock()
		assert.Equal(t, discoveryConcurrency, reading, "the accounts read at once")
		mu.Unlock()
		close(release)

		required := <-authRequired
		require.Len(t, required, len(accounts), "every account is read")
		for i := range accounts {
			assert.Equal(t, accounts[i].Email, required[i].Email)
		}
	})
}

// A panic while one account's drives are listed is that account's degraded
// notice; the other accounts are read and report as they would without it
// (R-6.3.10).
//
// Validates: R-3.3.2, R-6.3.10
func TestDiscoverAvailableDrives_APanicIsThatAccountsDegradedNotice(t *testing.T) {
	if !synctest.InChild(t) {
		synctest.RequireChildPassed(t, synctest.RunInChild(t))
		return
	}

	setTestDriveHome(t)
	accounts := discoveryTestAccounts(discoveryConcurrency + 1)
	panicking := accounts[1]
	runtime := discoveryTestRuntime(func(tokenPath string) (graph.TokenSource, error) {
		if strings.Contains(tokenPath, panicking.Email) {
			panic("token load exploded")
		}

		return nil, graph.ErrNotLoggedIn
	})
	logger := slog.New(slog.DiscardHandler)

	entries, required, degraded, gaps := discoverAvailableDrives(t.Context(), config.DefaultConfig(), accounts,
		sharePointSiteLimit, logger, newAuthProofRecorder(logger), runtime)

	assert.Empty(t, entries)
	assert.Equal(t, sharePointGaps{}, gaps)
	require.Len(t, degraded, 1)
	assert.Equal(t, panicking.Email, degraded[0].Email)
	assert.Equal(t, driveCatalogUnavailableReason, degraded[0].Reason)
	require.Len(t, required, len(accounts)-1)
	for i := range required {
		assert.NotEqual(t, panicking.Email, required[i].Email)
		assert.Equal(t, authReasonMissingLogin, required[i].Reason)
	}
}
