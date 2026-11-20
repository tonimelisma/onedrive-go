//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/testutil"
)

type shortcutChildE2ERecord struct {
	MountID           string
	NamespaceID       string
	RelativeLocalPath string
	State             syncshortcut.ShortcutRootState
	StateReason       string
}

type sharedFileFixtureCacheEntry struct {
	once    sync.Once
	fixture resolvedSharedFileFixture
	err     error
}

type sharedFolderFixtureCacheEntry struct {
	once    sync.Once
	fixture resolvedSharedFolderFixture
	err     error
}

// Suite-wide caches of resolved live fixtures: discovery costs several live
// Graph round trips, and every test that uses a fixture needs the same answer.
//
//nolint:gochecknoglobals // a cache shared by every test in the suite run has no narrower owner than the package.
var (
	sharedFileFixtureCache   sync.Map
	sharedFolderFixtureCache sync.Map
)

func requireSharedFileLink(t *testing.T) string {
	t.Helper()

	rawLink := liveConfig.Fixtures.SharedFileLink
	require.NotEmpty(t, rawLink,
		"shared-file fixture missing: set ONEDRIVE_TEST_SHARED_LINK in exported env, root .env, or .testdata/fixtures.env")

	return rawLink
}

func requireShortcutFixture(t *testing.T, kind shortcutFixtureKind) *resolvedShortcutFixture {
	t.Helper()

	return requireShortcutFixtureWithSharedDiscovery(t, kind, false)
}

func requireShortcutFixtureWithCatalog(t *testing.T, kind shortcutFixtureKind) *resolvedShortcutFixture {
	t.Helper()

	return requireShortcutFixtureWithSharedDiscovery(t, kind, true)
}

func requireShortcutFixtureWithSharedDiscovery(
	t *testing.T,
	kind shortcutFixtureKind,
	requireSharedCatalog bool,
) *resolvedShortcutFixture {
	t.Helper()

	fixtures := liveConfig.Fixtures
	fixture := &resolvedShortcutFixture{Kind: kind}
	switch kind {
	case shortcutFixtureWritable:
		fixture.ParentDrive = fixtures.WritableShortcutParentDrive
		fixture.ShortcutName = fixtures.WritableShortcutName
		fixture.SharerEmail = fixtures.WritableShortcutSharerEmail
		fixture.SentinelPath = fixtures.WritableShortcutSentinelPath
	case shortcutFixtureReadOnly:
		fixture.ParentDrive = fixtures.ReadOnlyShortcutParentDrive
		fixture.ShortcutName = fixtures.ReadOnlyShortcutName
		fixture.SharerEmail = fixtures.ReadOnlyShortcutSharerEmail
		fixture.SentinelPath = fixtures.ReadOnlyShortcutSentinelPath
	default:
		require.FailNowf(t, "unknown shortcut fixture kind", "kind=%s", kind)
	}

	// Nothing guesses which configured drive holds a shortcut: the repository
	// names no test account. CI's masking step sets both variables from its
	// drives by role.
	require.NotEmptyf(t, fixture.ParentDrive,
		"shortcut fixture missing: set %s and %s (the drives that hold the writable and the read-only shortcut) "+
			"in exported env, root .env, or .testdata/fixtures.env",
		testutil.WritableShortcutParentDriveEnvVar, testutil.ReadOnlyShortcutParentDriveEnvVar)
	require.NotEmptyf(t, fixture.ShortcutName,
		"shortcut fixture missing: set ONEDRIVE_TEST_SHORTCUT_%s_NAME in exported env, root .env, or .testdata/fixtures.env",
		shortcutFixtureEnvStem(kind))
	require.NotEmptyf(t, fixture.SharerEmail,
		"shortcut fixture missing: set ONEDRIVE_TEST_SHORTCUT_%s_SHARER_EMAIL in exported env, root .env, or .testdata/fixtures.env",
		shortcutFixtureEnvStem(kind))
	require.NotEmptyf(t, fixture.SentinelPath,
		"shortcut fixture missing: set ONEDRIVE_TEST_SHORTCUT_%s_SENTINEL_PATH in exported env, root .env, or .testdata/fixtures.env",
		shortcutFixtureEnvStem(kind))

	parentEmail, err := recipientEmailFromCanonicalDriveID(fixture.ParentDrive)
	require.NoError(t, err)
	fixture.ParentEmail = parentEmail

	return discoverShortcutFixture(t, fixture, requireSharedCatalog)
}

func shortcutFixtureEnvStem(kind shortcutFixtureKind) string {
	switch kind {
	case shortcutFixtureWritable:
		return "WRITABLE"
	case shortcutFixtureReadOnly:
		return "READONLY"
	default:
		return strings.ToUpper(string(kind))
	}
}

func discoverShortcutFixture(
	t *testing.T,
	fixture *resolvedShortcutFixture,
	requireSharedCatalog bool,
) *resolvedShortcutFixture {
	t.Helper()

	cfgPath, env, cleanup, err := writeFixtureConfigForDriveID(fixture.ParentDrive)
	require.NoError(t, err)
	defer cleanup()

	if item, ok := waitForShortcutSharedItem(t, cfgPath, env, fixture, requireSharedCatalog); ok {
		fixture.SharedItem = item
		requireShortcutSentinelVisible(t, cfgPath, env, fixture)
	}

	requireShortcutRootEntry(t, cfgPath, env, fixture)

	return fixture
}

func waitForShortcutSharedItem(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
	required bool,
) (sharedItemE2E, bool) {
	t.Helper()

	deadline := time.Now().Add(shortcutFixturePropagationTimeout)
	var lastListing sharedListE2EOutput
	var lastErr error

	for attempt := 0; ; attempt++ {
		listing, err := sharedListForRecipientRaw(cfgPath, env, fixture.ParentEmail)
		if err == nil {
			lastListing = listing
			for i := range listing.Items {
				item := listing.Items[i]
				if item.Name != fixture.ShortcutName || item.Type != sharedItemTypeFolder {
					continue
				}
				if fixture.SharerEmail != "" && !strings.EqualFold(item.SharedByEmail, fixture.SharerEmail) {
					continue
				}

				return item, true
			}
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			// The sharer is named by its variable: an address no configured
			// drive has would reach the public log.
			message := fmt.Sprintf(
				"%s shortcut fixture %q did not appear in shared --json for %s shared by its "+
					"ONEDRIVE_TEST_SHORTCUT_%s_SHARER_EMAIL within %v; last_items=%d last_err=%s",
				fixture.Kind,
				suiteDriveRoles.Redact(fixture.ShortcutName),
				suiteDriveRoles.Role(fixture.ParentDrive),
				shortcutFixtureEnvStem(fixture.Kind),
				shortcutFixturePropagationTimeout,
				len(lastListing.Items),
				printable(fmt.Sprint(lastErr)),
			)
			if required {
				require.FailNow(t, message)
			}

			t.Log(message)
			return sharedItemE2E{}, false
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func requireShortcutRootEntry(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) {
	t.Helper()

	waitForShortcutRootPlaceholder(t, cfgPath, env, fixture, "shortcut fixture root entry missing")
}

func requireShortcutSentinelVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) {
	t.Helper()

	require.NoError(t, shortcutSentinelReachable(t, cfgPath, env, fixture))
}

func shortcutSentinelReachable(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) error {
	t.Helper()

	if fixture.SharedItem.Selector == "" {
		return fmt.Errorf("shortcut shared target selector was not resolved")
	}

	downloadRoot := filepath.Join(t.TempDir(), "shared-target")
	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "get", fixture.SharedItem.Selector, downloadRoot)
	if err != nil {
		return fmt.Errorf("download shortcut shared target: %w\nstdout: %s\nstderr: %s",
			err, printable(stdout), printable(stderr))
	}

	sentinelPath := filepath.Join(downloadRoot, filepath.FromSlash(strings.TrimPrefix(path.Clean(fixture.SentinelPath), "/")))
	if _, err := os.Stat(sentinelPath); err != nil {
		return fmt.Errorf("stat shortcut shared target sentinel %s: %w", sentinelPath, err)
	}

	return nil
}

func requireActiveShortcutChild(
	t *testing.T,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) shortcutChildE2ERecord {
	t.Helper()

	record, isActive, err := findActiveShortcutChild(t, env, fixture, fixture.ShortcutName)
	requireNoErrorByRole(t, err)
	if isActive {
		return record
	}

	require.Failf(t,
		"active shortcut child mount missing",
		"parent=%s shortcut=%q remote=fixture.SharedItem state=%q reason=%q",
		suiteDriveRoles.Role(fixture.ParentDrive),
		suiteDriveRoles.Redact(fixture.ShortcutName),
		record.State,
		record.StateReason,
	)
	return shortcutChildE2ERecord{}
}

// requireNoErrorByRole stops the test when err is set, printing its text with
// every configured drive and its account named by role: an error that names
// a state database spells its drive in the file name.
func requireNoErrorByRole(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		require.FailNow(t, suiteDriveRoles.Redact(err.Error()))
	}
}

// assertSameIdentity checks that got is the identity want holds: an account
// email, a shared selector, a remote drive or item ID. On a mismatch it names
// what it compared, never the values: CI logs are public.
func assertSameIdentity(t *testing.T, what, want, got string) {
	t.Helper()

	isSame := got == want
	assert.Truef(t, isSame, "%s differs from the expected value (values not printed: CI logs are public)", what)
}

// assertSharedFileFixtureConsistent checks that the shared-file fixture's
// listing entry and its raw-link stat name the same account and selector.
func assertSharedFileFixtureConsistent(t *testing.T, fixture *resolvedSharedFileFixture) {
	t.Helper()

	assertSameIdentity(t, "the shared file's account email", fixture.RecipientEmail, fixture.FileItem.AccountEmail)
	assertSameIdentity(t, "the raw link stat's shared selector", fixture.FileItem.Selector, fixture.RawStat.SharedSelector)
}

// assertStatNamesSharedItem checks that stat names the shared item item
// describes: the same remote drive and item IDs and the same selector.
func assertStatNamesSharedItem(t *testing.T, item *sharedItemE2E, stat *sharedStatE2EOutput) {
	t.Helper()

	assertSameIdentity(t, "the stat's remote drive ID", item.RemoteDriveID, stat.RemoteDriveID)
	assertSameIdentity(t, "the stat's remote item ID", item.RemoteItemID, stat.RemoteItemID)
	assertSameIdentity(t, "the stat's shared selector", item.Selector, stat.SharedSelector)
}

// discoveredSharedFile is the shared-file fixture found from its raw link,
// with the config of its recipient's drive that reads it.
type discoveredSharedFile struct {
	rawLink string
	fixture resolvedSharedFileFixture
	cfgPath string
	env     map[string]string
}

// discoverSharedFile is the discovery the shared-file tests start from: it
// resolves the fixture from its raw link, writes a config for the
// recipient's drive, and checks that the fixture is consistent and that stat
// through its selector names the same item as the raw link.
func discoverSharedFile(t *testing.T) discoveredSharedFile {
	t.Helper()

	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, rawLink)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	assertSharedFileFixtureConsistent(t, &fixture)

	selectorStat := statSharedTargetJSON(t, cfgPath, env, fixture.FileItem.Selector)
	assertStatNamesSharedItem(t, &fixture.FileItem, &selectorStat)
	assert.Equal(t, fixture.RawStat.Name, selectorStat.Name)

	return discoveredSharedFile{rawLink: rawLink, fixture: fixture, cfgPath: cfgPath, env: env}
}

// findActiveShortcutChild returns the parent drive's child mount record at
// relativeLocalPath, if there is one, and whether it is active with no state
// reason. It asserts nothing, so a poll predicate can call it: a record in
// another state is a state to wait through, and a failed read of the parent's
// state database is its error.
func findActiveShortcutChild(
	t *testing.T,
	env map[string]string,
	fixture *resolvedShortcutFixture,
	relativeLocalPath string,
) (shortcutChildE2ERecord, bool, error) {
	t.Helper()

	records, err := readShortcutChildRecords(t, env, fixture)
	if err != nil {
		return shortcutChildE2ERecord{}, false, err
	}

	for _, record := range records {
		if record.NamespaceID != fixture.ParentDrive ||
			record.RelativeLocalPath != relativeLocalPath {
			continue
		}

		return record, record.State == syncshortcut.ShortcutRootStateActive && record.StateReason == "", nil
	}

	return shortcutChildE2ERecord{}, false, nil
}

// syncShortcutDownloadUntilProjected runs `sync --download-only` on the
// shortcut fixture's parent drive until the shortcut's child mount is active
// at relativeLocalPath and the shared target's sentinel file is on disk under
// it: the placeholder, and a rename or move of it, reach the parent drive
// through delta only eventually. It returns the child mount record and the
// pass that converged.
func syncShortcutDownloadUntilProjected(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
	syncDir string,
	relativeLocalPath string,
) (shortcutChildE2ERecord, syncAttemptResult) {
	t.Helper()

	sentinelPath := filepath.Join(syncDir, filepath.FromSlash(relativeLocalPath), filepath.FromSlash(fixture.SentinelPath))
	var record shortcutChildE2ERecord
	attempt := requireSyncEventuallyConvergesForDrive(
		t,
		cfgPath,
		env,
		fixture.ParentDrive,
		remoteScopeTransitionTimeout,
		fmt.Sprintf("shortcut child mount and sentinel should be projected at %q", relativeLocalPath),
		func(result syncAttemptResult) bool {
			if result.Err != nil {
				return false
			}

			found, isActive, readErr := findActiveShortcutChild(t, env, fixture, relativeLocalPath)
			switch {
			case readErr != nil:
				t.Logf("waiting for the shortcut child mounts to be readable: %s", suiteDriveRoles.Redact(readErr.Error()))
				return false
			case !isActive:
				t.Logf("waiting for an active shortcut child mount at %s (state %q, reason %q)",
					suiteDriveRoles.Redact(relativeLocalPath), found.State, found.StateReason)
				return false
			}

			if _, err := os.Stat(sentinelPath); err != nil {
				t.Logf("shortcut child mount is active at %s but its sentinel is not projected yet",
					suiteDriveRoles.Redact(relativeLocalPath))
				return false
			}

			record = found
			return true
		},
		"--download-only",
	)
	assert.Equal(t, syncshortcut.ShortcutRootStateActive, record.State)
	assert.Empty(t, record.StateReason)

	return record, attempt
}

// shortcutChildRecords is readShortcutChildRecords for a test that cannot go
// on without them.
func shortcutChildRecords(
	t *testing.T,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) []shortcutChildE2ERecord {
	t.Helper()

	records, err := readShortcutChildRecords(t, env, fixture)
	requireNoErrorByRole(t, err)

	return records
}

// readShortcutChildRecords reads the parent drive's shortcut child mount
// records from its state database. It asserts nothing, so a poll predicate
// can call it.
func readShortcutChildRecords(
	t *testing.T,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) ([]shortcutChildE2ERecord, error) {
	t.Helper()

	parentID := driveid.MustCanonicalID(fixture.ParentDrive)
	statePath := e2eDriveStatePath(env, parentID)
	roots, err := syncstore.ReadShortcutRootStatusSnapshot(t.Context(), statePath, parentID.String(), "", nil)
	if err != nil {
		return nil, fmt.Errorf("read shortcut child mounts of %s: %w", suiteDriveRoles.Role(parentID.String()), err)
	}
	records := make([]shortcutChildE2ERecord, 0, len(roots))
	for i := range roots {
		root := roots[i]
		state := syncshortcut.ShortcutRootState(root.Metadata.DisplayState)
		if state == "" {
			state = syncshortcut.ShortcutRootStateActive
		}
		stateReason := root.Metadata.StateReason
		records = append(records, shortcutChildE2ERecord{
			MountID:           root.MountID,
			NamespaceID:       parentID.String(),
			RelativeLocalPath: root.SortPath,
			State:             state,
			StateReason:       stateReason,
		})
	}
	return records, nil
}

func e2eDriveStatePath(env map[string]string, canonicalID driveid.CanonicalID) string {
	return filepath.Join(
		env["XDG_DATA_HOME"],
		"onedrive-go",
		"state_"+strings.ReplaceAll(canonicalID.String(), ":", "_")+".db",
	)
}

func requireSharedListContainsShortcutFixture(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) {
	t.Helper()

	_, _ = waitForShortcutSharedItem(t, cfgPath, env, fixture, true)
}

func requireRootPlaceholderContainsShortcutFixture(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
) {
	t.Helper()

	waitForShortcutRootPlaceholder(t, cfgPath, env, fixture, "shortcut fixture root placeholder missing")
}

func waitForShortcutRootPlaceholder(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedShortcutFixture,
	failureTitle string,
) {
	t.Helper()

	startedAt := time.Now()
	deadline := startedAt.Add(shortcutFixturePropagationTimeout)
	var lastNames []string
	var lastStdout string
	var lastErr error
	var lastSentinelErr error
	rootListingDecoded := false
	for attempt := 0; ; attempt++ {
		stdout, _, err := runCLIWithConfigAllowErrorForDrive(t, cfgPath, env, fixture.ParentDrive, "ls", "--json", "/")
		lastStdout = stdout
		lastErr = err
		rootListingDecoded = false
		if err == nil {
			var items []remoteListJSONItem
			if decodeErr := json.Unmarshal([]byte(stdout), &items); decodeErr != nil {
				lastErr = decodeErr
			} else {
				lastNames = rootListingNames(items)
				rootListingDecoded = true
				if stringsContain(lastNames, fixture.ShortcutName) {
					return
				}
			}
		}

		now := time.Now()
		pollState := shortcutRootPlaceholderPollState{
			RootListingDecoded: rootListingDecoded,
			RootNames:          lastNames,
			LastStdout:         lastStdout,
			LastErr:            lastErr,
		}
		if canSkipMissingShortcutRootPlaceholder(pollState, fixture) &&
			shortcutRootPlaceholderProviderProofElapsed(startedAt, now) {
			lastSentinelErr = shortcutSentinelReachable(t, cfgPath, env, fixture)
			if lastSentinelErr == nil {
				skipShortcutRootPlaceholderOmission(t, failureTitle, pollState, fixture, now.Sub(startedAt))
			}
		}

		if now.After(deadline) {
			if !canSkipMissingShortcutRootPlaceholder(pollState, fixture) {
				require.Failf(t,
					failureTitle,
					"root placeholder omission is not skippable because the root listing did not end in a decoded success or the shared target was not resolved; root_listing_decoded=%t shared_target_resolved=%t root_names=%v last_err=%v last_stdout=%s",
					pollState.RootListingDecoded,
					fixture.SharedItem.Selector != "",
					suiteDriveRoles.Redact(fmt.Sprint(pollState.RootNames)),
					suiteDriveRoles.Redact(fmt.Sprint(pollState.LastErr)),
					printable(pollState.LastStdout),
				)
			}
			lastSentinelErr = shortcutSentinelReachable(t, cfgPath, env, fixture)
			require.NoErrorf(t,
				lastSentinelErr,
				"shared target must remain reachable before skipping missing shortcut root placeholder",
			)
			skipShortcutRootPlaceholderOmission(t, failureTitle, pollState, fixture, shortcutFixturePropagationTimeout)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func skipShortcutRootPlaceholderOmission(
	t *testing.T,
	failureTitle string,
	pollState shortcutRootPlaceholderPollState,
	fixture *resolvedShortcutFixture,
	elapsed time.Duration,
) {
	t.Helper()

	decision := classifyShortcutRootPlaceholderProviderSkip(pollState, fixture)
	t.Skipf(
		"%s: provider_skip=%s live Graph did not expose Add-to-OneDrive shortcut placeholder %q in root of %s within %v; root_names=%v last_err=%v last_stdout=%s",
		failureTitle,
		decision.Reason,
		suiteDriveRoles.Redact(fixture.ShortcutName),
		suiteDriveRoles.Role(fixture.ParentDrive),
		elapsed,
		suiteDriveRoles.Redact(fmt.Sprint(pollState.RootNames)),
		suiteDriveRoles.Redact(fmt.Sprint(pollState.LastErr)),
		printable(pollState.LastStdout),
	)
}

func rootListingNames(items []remoteListJSONItem) []string {
	var names []string
	for i := range items {
		names = append(names, items[i].Name)
	}
	return names
}

func stringsContain(items []string, needle string) bool {
	for i := range items {
		if items[i] == needle {
			return true
		}
	}
	return false
}

func resolveSharedFileFixture(t *testing.T, rawLink string) resolvedSharedFileFixture {
	t.Helper()

	entryValue, _ := sharedFileFixtureCache.LoadOrStore(rawLink, &sharedFileFixtureCacheEntry{})
	entry, ok := entryValue.(*sharedFileFixtureCacheEntry)
	require.True(t, ok, "shared-file fixture cache holds %T", entryValue)
	entry.once.Do(func() {
		entry.fixture, entry.err = discoverSharedFileFixture(t, rawLink)
	})

	require.NoError(t, entry.err)

	return entry.fixture
}

func discoverSharedFileFixture(_ *testing.T, rawLink string) (resolvedSharedFileFixture, error) {
	candidateDriveIDs := sharedFixtureCandidateDriveIDs()
	if len(candidateDriveIDs) == 0 {
		return resolvedSharedFileFixture{}, fmt.Errorf("shared-file fixture requires at least one configured drive candidate")
	}

	var candidates []sharedFileCandidate
	for _, candidateDriveID := range candidateDriveIDs {
		candidate, ok, err := probeSharedFileCandidate(candidateDriveID, rawLink)
		if err != nil {
			return resolvedSharedFileFixture{}, err
		}
		if ok {
			candidates = append(candidates, candidate)
		}
	}

	return selectSharedFileFixture(candidates)
}

// probeSharedFileCandidate checks whether one configured drive's account can
// see the shared-file link. ok is false when the account cannot stat the link
// or the link resolves to a different account.
func probeSharedFileCandidate(candidateDriveID, rawLink string) (sharedFileCandidate, bool, error) {
	cfgPath, env, cleanup, err := writeFixtureConfigForDriveID(candidateDriveID)
	if err != nil {
		return sharedFileCandidate{}, false, err
	}
	defer cleanup()

	recipientEmail, err := recipientEmailFromCanonicalDriveID(candidateDriveID)
	if err != nil {
		return sharedFileCandidate{}, false, err
	}

	_, stdout, _, err := execCLI(
		cfgPath,
		env,
		"",
		"stat",
		"--json",
		"--account",
		recipientEmail,
		rawLink,
	)
	if err != nil {
		// An account that cannot stat the link is simply not a candidate.
		return sharedFileCandidate{}, false, nil
	}

	var rawStat sharedStatE2EOutput
	if decodeErr := json.Unmarshal([]byte(stdout), &rawStat); decodeErr != nil {
		return sharedFileCandidate{}, false, fmt.Errorf(
			"parse raw shared-link stat output for candidate %s: %w",
			suiteDriveRoles.Role(candidateDriveID),
			decodeErr,
		)
	}
	if rawStat.AccountEmail != "" && rawStat.AccountEmail != recipientEmail {
		return sharedFileCandidate{}, false, nil
	}

	fileItem := sharedItemE2E{
		Selector:      rawStat.SharedSelector,
		Type:          sharedItemTypeFile,
		Name:          rawStat.Name,
		AccountEmail:  recipientEmail,
		RemoteDriveID: rawStat.RemoteDriveID,
		RemoteItemID:  rawStat.RemoteItemID,
	}
	listingMatched := false

	listing, err := sharedListForRecipientRaw(cfgPath, env, recipientEmail)
	if err == nil {
		for i := range listing.Items {
			if listing.Items[i].RemoteDriveID != rawStat.RemoteDriveID ||
				listing.Items[i].RemoteItemID != rawStat.RemoteItemID ||
				listing.Items[i].Type != sharedItemTypeFile {
				continue
			}

			fileItem = listing.Items[i]
			listingMatched = true
			break
		}
	}

	return sharedFileCandidate{
		fixture: resolvedSharedFileFixture{
			RecipientDriveID: candidateDriveID,
			RecipientEmail:   recipientEmail,
			RawStat:          rawStat,
			FileItem:         fileItem,
		},
		listingMatched: listingMatched,
	}, true, nil
}

func resolveSharedFolderFixture(t *testing.T, selector string) resolvedSharedFolderFixture {
	t.Helper()

	require.NotEmpty(t, selector,
		"shared-folder fixture missing: store the selector in exported env, root .env, or .testdata/fixtures.env")

	entryValue, _ := sharedFolderFixtureCache.LoadOrStore(selector, &sharedFolderFixtureCacheEntry{})
	entry, ok := entryValue.(*sharedFolderFixtureCacheEntry)
	require.True(t, ok, "shared-folder fixture cache holds %T", entryValue)
	entry.once.Do(func() {
		entry.fixture, entry.err = discoverSharedFolderFixture(t, selector)
	})

	require.NoError(t, entry.err)

	return entry.fixture
}

func discoverSharedFolderFixture(_ *testing.T, selector string) (resolvedSharedFolderFixture, error) {
	ref, err := sharedref.Parse(selector)
	if err != nil {
		return resolvedSharedFolderFixture{}, fmt.Errorf("parse shared-folder selector: %w", err)
	}

	recipientDriveID, ok := liveConfig.DriveIDForEmail(ref.AccountEmail)
	if !ok {
		return resolvedSharedFolderFixture{}, fmt.Errorf(
			"shared fixture recipient %s does not match any configured drive (%s)",
			suiteDriveRoles.Redact(ref.AccountEmail),
			suiteDriveRoles.Redact(fmt.Sprint(liveConfig.CandidateDriveIDs())),
		)
	}

	cfgPath, env, cleanup, err := writeFixtureConfigForDriveID(recipientDriveID)
	if err != nil {
		return resolvedSharedFolderFixture{}, err
	}
	defer cleanup()

	item := sharedItemE2E{
		Selector:      selector,
		Type:          sharedItemTypeFolder,
		AccountEmail:  ref.AccountEmail,
		RemoteDriveID: ref.RemoteDriveID,
		RemoteItemID:  ref.RemoteItemID,
	}

	listing, err := sharedListForRecipientRaw(cfgPath, env, ref.AccountEmail)
	if err == nil {
		for i := range listing.Items {
			if listing.Items[i].RemoteDriveID == ref.RemoteDriveID &&
				listing.Items[i].RemoteItemID == ref.RemoteItemID &&
				listing.Items[i].Type == sharedItemTypeFolder {
				item = listing.Items[i]
				break
			}
		}
	}

	return resolvedSharedFolderFixture{
		RecipientDriveID: recipientDriveID,
		RecipientEmail:   ref.AccountEmail,
		FolderItem:       item,
	}, nil
}

func sharedFixtureCandidateDriveIDs() []string {
	return liveConfig.CandidateDriveIDs()
}

func writeFixtureConfigForDriveID(driveID string) (string, map[string]string, func(), error) {
	perTestData, err := os.MkdirTemp("", "onedrive-shared-fixture-data-*")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create fixture XDG data dir: %w", err)
	}

	perTestHome, err := os.MkdirTemp("", "onedrive-shared-fixture-home-*")
	if err != nil {
		removeFixtureDirs(perTestData)
		return "", nil, nil, fmt.Errorf("create fixture HOME dir: %w", err)
	}

	cfgDir, err := os.MkdirTemp("", "onedrive-shared-fixture-config-*")
	if err != nil {
		removeFixtureDirs(perTestData, perTestHome)
		return "", nil, nil, fmt.Errorf("create fixture config dir: %w", err)
	}

	cleanup := func() {
		removeFixtureDirs(perTestData, perTestHome, cfgDir)
	}

	perTestDataDir := filepath.Join(perTestData, "onedrive-go")
	if err := os.MkdirAll(perTestDataDir, 0o700); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("create fixture app data dir: %w", err)
	}

	if err := copyFixtureFile(
		filepath.Join(testDataDir, testutil.TokenFileName(driveID)),
		filepath.Join(perTestDataDir, testutil.TokenFileName(driveID)),
		0o600,
	); err != nil {
		cleanup()
		return "", nil, nil, err
	}

	if err := copyFixtureMetadataFiles(testDataDir, perTestDataDir); err != nil {
		cleanup()
		return "", nil, nil, err
	}

	content := fmt.Sprintf(`["%s"]
sync_dir = %q
`, driveID, filepath.Join(perTestHome, "sync"))

	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("write fixture config: %w", err)
	}

	env := map[string]string{
		"XDG_DATA_HOME": perTestData,
		"HOME":          perTestHome,
	}

	return cfgPath, env, cleanup, nil
}

// removeFixtureDirs removes fixture temp dirs. It has no *testing.T because
// fixture discovery is cached across tests, so it reports a failure on
// stderr, where CI keeps it, instead of failing whichever test came first.
func removeFixtureDirs(dirs ...string) {
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: removing fixture dir %s: %v\n", dir, err)
		}
	}
}

func copyFixtureMetadataFiles(srcDir, dstDir string) error {
	if err := copyFixtureFile(
		filepath.Join(srcDir, "catalog.json"),
		filepath.Join(dstDir, "catalog.json"),
		0o600,
	); err != nil {
		return err
	}

	return nil
}

// copyFixtureFile copies src to dst. A token file's name spells its account,
// so an error names both paths through Redact and carries the os error's
// text, which quotes them, redacted instead of wrapped.
func copyFixtureFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src) //nolint:gosec // fixture metadata from the suite's own isolated data dir
	if err != nil {
		return fmt.Errorf("read fixture file %s: %s", suiteDriveRoles.Redact(src), suiteDriveRoles.Redact(err.Error()))
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("mkdir fixture destination for %s: %s",
			suiteDriveRoles.Redact(dst), suiteDriveRoles.Redact(err.Error()))
	}

	//nolint:gosec // destination is inside a temp dir this helper created
	if err := os.WriteFile(dst, data, perm); err != nil {
		return fmt.Errorf("write fixture file %s: %s", suiteDriveRoles.Redact(dst), suiteDriveRoles.Redact(err.Error()))
	}

	return nil
}

func sharedListForRecipientRaw(cfgPath string, env map[string]string, recipientEmail string) (sharedListE2EOutput, error) {
	_, stdout, stderr, err := execCLI(cfgPath, env, "", "--account", recipientEmail, "shared", "--json")
	if err != nil {
		return sharedListE2EOutput{}, fmt.Errorf("shared --json for %s: %w (%s)",
			suiteDriveRoles.Redact(recipientEmail), err, printable(strings.TrimSpace(stderr)))
	}

	var parsed sharedListE2EOutput
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return sharedListE2EOutput{}, fmt.Errorf("parse shared --json for %s: %w", suiteDriveRoles.Redact(recipientEmail), err)
	}

	return parsed, nil
}

func expandHomePath(path string, env map[string]string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}

	home := env["HOME"]
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return path
	}

	return filepath.Join(home, path[2:])
}

func waitForDriveListSharedSelectorVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	selector string,
) driveListE2EOutput {
	t.Helper()

	deadline := time.Now().Add(pollTimeout)
	// drive list names the owners of shared drives, whose accounts the suite
	// does not configure and so cannot redact: messages say what the listing
	// held, by count, never its text, which the test's debug log keeps.
	lastListing := sharedListingNotDecoded
	var lastStderr string

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, "drive", "list", "--json")
		lastStderr = stderr

		if err != nil {
			if !isRetryableGraphGatewayFailure(stderr) {
				require.NoErrorf(t, err, "drive list --json should succeed\nstderr: %s", printable(stderr))
			}
		} else {
			var parsed driveListE2EOutput
			require.NoErrorf(t, json.Unmarshal([]byte(stdout), &parsed),
				"drive list --json output should be valid JSON (%d bytes, in the test's debug log)", len(stdout))
			if driveListAvailableContainsCanonicalID(parsed, selector) {
				return parsed
			}
			lastListing = fmt.Sprintf("%d configured and %d available drives", len(parsed.Configured), len(parsed.Available))
		}

		if time.Now().After(deadline) {
			t.Skipf(
				"provider_skip=%s live drive list shared discovery did not expose the shared fixture's selector within %v; Graph search omitted the known fixture on this run\nlast decoded listing: %s\nlast attempt's stderr: %s",
				liveProviderSkipDriveListSharedOmitted,
				pollTimeout,
				lastListing,
				printable(lastStderr),
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func waitForSharedFolderNameDriveAdd(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedSharedFolderFixture,
) {
	t.Helper()

	deadline := time.Now().Add(pollTimeout)
	var lastStderr string
	lastSharedListing := sharedListingNotDecoded
	var lastSharedStderr string
	var lastSharedErr error
	lastSharedVisible := true

	for attempt := 0; ; attempt++ {
		_, stderr, err := runCLICore(
			t,
			cfgPath,
			env,
			"",
			"--account",
			fixture.RecipientEmail,
			"drive",
			"add",
			fixture.FolderItem.Name,
		)
		lastStderr = stderr

		if err == nil {
			return
		}
		if isSharedFolderNameDiscoveryOmission(stderr) {
			lastSharedVisible, lastSharedListing, lastSharedStderr, lastSharedErr = sharedFolderNameFixtureVisible(
				t,
				cfgPath,
				env,
				fixture,
			)
			if lastSharedErr != nil {
				require.Truef(t, isRetryableGraphGatewayFailure(lastSharedStderr),
					"shared --json preflight should succeed when classifying drive add <shared-folder-name> no-match\nshared err: %s\nshared stderr: %s",
					suiteDriveRoles.Redact(lastSharedErr.Error()),
					printable(lastSharedStderr),
				)
			} else if lastSharedVisible {
				require.NoErrorf(t, err,
					"drive add <shared-folder-name> missed fixture that shared --json exposed by exact name and remote identity\nstderr: %s\nshared --json: %s",
					printable(stderr),
					printable(lastSharedListing),
				)
			}
		} else if !isRetryableGraphGatewayFailure(stderr) {
			require.NoErrorf(t, err,
				"drive add <shared-folder-name> should succeed or expose known provider discovery failure\nstderr: %s",
				printable(stderr),
			)
		}

		if time.Now().After(deadline) {
			endSharedFolderNameDriveAddWait(t, fixture, err, sharedFolderNameDriveAddAttempt{
				stderr:        lastStderr,
				sharedVisible: lastSharedVisible,
				sharedListing: lastSharedListing,
				sharedStderr:  lastSharedStderr,
				sharedErr:     lastSharedErr,
			})
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

// sharedFolderNameDriveAddAttempt is the last observed state of the
// drive add <shared-folder-name> poll, kept for the timeout verdict. It holds
// no stdout: drive add and shared --json can name the owners of shared items,
// whose accounts the suite does not configure and so cannot redact, so the
// verdict says what shared --json listed by count (sharedListing) and leaves
// both outputs to the test's debug log.
type sharedFolderNameDriveAddAttempt struct {
	stderr        string
	sharedVisible bool
	sharedListing string
	sharedStderr  string
	sharedErr     error
}

// endSharedFolderNameDriveAddWait ends a timed-out drive add poll: it skips
// on a provider-only omission or persistent gateway failures, and otherwise
// fails the test. It never returns.
func endSharedFolderNameDriveAddWait(
	t *testing.T,
	fixture *resolvedSharedFolderFixture,
	err error,
	last sharedFolderNameDriveAddAttempt,
) {
	t.Helper()

	account := suiteDriveRoles.Redact(fixture.RecipientEmail)
	if decision := classifySharedFolderNameProviderSkip(last.stderr, last.sharedVisible, last.sharedErr, last.sharedStderr); decision.Skippable {
		t.Skipf(
			"provider_skip=%s live shared discovery omitted the shared folder fixture's name for account %s within %v; independent shared --json did not confirm the known fixture on this run\n%s",
			decision.Reason,
			account,
			pollTimeout,
			last.redactedSummary(),
		)
	}
	if isRetryableGraphGatewayFailure(last.stderr) {
		t.Skipf(
			"live shared discovery could not add the shared folder fixture by name for account %s within %v because Graph kept returning retryable gateway failures\nlast stderr: %s",
			account,
			pollTimeout,
			printable(last.stderr),
		)
	}

	require.NoErrorf(t, err,
		"drive add <shared-folder-name> should not time out without a provider-only omission signal\n%s",
		last.redactedSummary(),
	)
	t.FailNow()
}

// redactedSummary is the attempt for a message: drive add's stderr, what
// shared --json listed and how it failed, each masked and bounded
// (printable).
func (last *sharedFolderNameDriveAddAttempt) redactedSummary() string {
	return fmt.Sprintf(
		"last stderr: %s\nlast shared --json: %s\nlast shared err: %s\nlast shared stderr: %s",
		printable(last.stderr), printable(last.sharedListing), printable(fmt.Sprint(last.sharedErr)),
		printable(last.sharedStderr),
	)
}

// sharedListingNotDecoded is what a message says of a shared --json or
// drive list --json run when no output of it has decoded.
const sharedListingNotDecoded = "no listing decoded"

// sharedFolderNameFixtureVisible runs shared --json for the fixture's
// recipient and reports whether it lists the fixture, what it listed by count
// (its output names sharers the suite cannot redact), its stderr and its
// error.
func sharedFolderNameFixtureVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture *resolvedSharedFolderFixture,
) (bool, string, string, error) {
	t.Helper()

	stdout, stderr, err := runCLICore(
		t,
		cfgPath,
		env,
		"",
		"--account",
		fixture.RecipientEmail,
		"shared",
		"--json",
	)
	if err != nil {
		return false, sharedListingNotDecoded, stderr, err
	}

	var listing sharedListE2EOutput
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		return false, sharedListingNotDecoded, stderr, fmt.Errorf("decode shared --json output: %w", err)
	}

	return sharedFolderFixtureVisibleInListing(listing, fixture),
		fmt.Sprintf("%d shared items listed", len(listing.Items)), stderr, nil
}

func statSharedTargetJSON(t *testing.T, cfgPath string, env map[string]string, args ...string) sharedStatE2EOutput {
	t.Helper()

	fullArgs := append([]string{"stat", "--json"}, args...)
	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, fullArgs...)

	var parsed sharedStatE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	return parsed
}

func getSharedTargetContent(t *testing.T, cfgPath string, env map[string]string, args ...string) string {
	t.Helper()

	localPath := filepath.Join(t.TempDir(), "downloaded")
	fullArgs := append([]string{"get"}, args...)
	fullArgs = append(fullArgs, localPath)
	runCLIWithoutDrive(t, cfgPath, env, fullArgs...)

	data, err := os.ReadFile(localPath) //nolint:gosec // the file the CLI just downloaded into the test's own temp dir
	require.NoError(t, err)

	return string(data)
}

func writeTempContentFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "upload.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func eventuallySharedContentEquals(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	expected string,
	args ...string,
) {
	t.Helper()

	// testify runs the condition on its own goroutine, where a failing t call
	// cannot stop the test, so the temp dir is made here, once: `get` renames
	// each download over the same path.
	localPath := filepath.Join(t.TempDir(), "downloaded")
	fullArgs := append([]string{"get"}, args...)
	fullArgs = append(fullArgs, localPath)
	require.Eventually(t, func() bool {
		_, _, err := runCLICore(t, cfgPath, env, "", fullArgs...)
		if err != nil {
			return false
		}

		data, readErr := os.ReadFile(localPath) //nolint:gosec // the file the CLI just downloaded into the test's own temp dir
		if readErr != nil {
			return false
		}

		return string(data) == expected
	}, pollTimeout, 2*time.Second)
}
