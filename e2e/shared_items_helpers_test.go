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

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/testutil"
)

type shortcutChildE2ERecord struct {
	MountID           string
	NamespaceID       string
	BindingItemID     string
	RelativeLocalPath string
	RemoteDriveID     string
	RemoteItemID      string
	State             syncengine.ShortcutRootState
	StateReason       string
}

type sharedItemE2E struct {
	Selector      string `json:"selector"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	AccountEmail  string `json:"account_email"`
	SharedByEmail string `json:"shared_by_email"`
	RemoteDriveID string `json:"remote_drive_id"`
	RemoteItemID  string `json:"remote_item_id"`
}

type sharedListE2EOutput struct {
	Items            []sharedItemE2E `json:"items"`
	AccountsDegraded []struct {
		Email     string `json:"email"`
		Reason    string `json:"reason"`
		DriveType string `json:"drive_type"`
	} `json:"accounts_degraded"`
}

type sharedStatE2EOutput struct {
	Name           string `json:"name"`
	AccountEmail   string `json:"account_email"`
	RemoteDriveID  string `json:"remote_drive_id"`
	RemoteItemID   string `json:"remote_item_id"`
	SharedSelector string `json:"shared_selector"`
}

type driveListE2EOutput struct {
	Configured []struct {
		CanonicalID string `json:"canonical_id"`
		SyncDir     string `json:"sync_dir"`
	} `json:"configured"`
	Available []struct {
		CanonicalID string `json:"canonical_id"`
	} `json:"available"`
}

func driveListAvailableContainsCanonicalID(parsed driveListE2EOutput, canonicalID string) bool {
	for i := range parsed.Available {
		if parsed.Available[i].CanonicalID == canonicalID {
			return true
		}
	}

	return false
}

func TestDriveListAvailableContainsCanonicalID(t *testing.T) {
	t.Parallel()

	parsed := driveListE2EOutput{
		Available: []struct {
			CanonicalID string `json:"canonical_id"`
		}{
			{CanonicalID: "shared:user@example.com:drive-a:item-1"},
			{CanonicalID: "shared:user@example.com:drive-b:item-2"},
		},
	}

	require.True(t, driveListAvailableContainsCanonicalID(parsed, "shared:user@example.com:drive-a:item-1"))
	require.False(t, driveListAvailableContainsCanonicalID(parsed, "shared:user@example.com:drive-c:item-3"))
}

type resolvedSharedFileFixture struct {
	RecipientDriveID string
	RecipientEmail   string
	RawStat          sharedStatE2EOutput
	FileItem         sharedItemE2E
}

type resolvedSharedFolderFixture struct {
	RecipientDriveID string
	RecipientEmail   string
	FolderItem       sharedItemE2E
}

type shortcutFixtureKind string

const (
	shortcutFixtureWritable shortcutFixtureKind = "writable"
	shortcutFixtureReadOnly shortcutFixtureKind = "read-only"
)

type resolvedShortcutFixture struct {
	Kind         shortcutFixtureKind
	ParentDrive  string
	ParentEmail  string
	ShortcutName string
	SharerEmail  string
	SentinelPath string
	SharedItem   sharedItemE2E
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

type sharedFileCandidate struct {
	fixture        resolvedSharedFileFixture
	listingMatched bool
}

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

func requireShortcutFixture(t *testing.T, kind shortcutFixtureKind) resolvedShortcutFixture {
	t.Helper()

	return requireShortcutFixtureWithSharedDiscovery(t, kind, false)
}

func requireShortcutFixtureWithCatalog(t *testing.T, kind shortcutFixtureKind) resolvedShortcutFixture {
	t.Helper()

	return requireShortcutFixtureWithSharedDiscovery(t, kind, true)
}

func requireShortcutFixtureWithSharedDiscovery(
	t *testing.T,
	kind shortcutFixtureKind,
	requireSharedCatalog bool,
) resolvedShortcutFixture {
	t.Helper()

	fixtures := liveConfig.Fixtures
	fixture := resolvedShortcutFixture{Kind: kind}
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

	require.NotEmptyf(t, fixture.ParentDrive,
		"shortcut fixture missing: set ONEDRIVE_TEST_SHORTCUT_%s_PARENT_DRIVE in exported env, root .env, or .testdata/fixtures.env",
		shortcutFixtureEnvStem(kind))
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
	fixture resolvedShortcutFixture,
	requireSharedCatalog bool,
) resolvedShortcutFixture {
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
	fixture resolvedShortcutFixture,
	required bool,
) (sharedItemE2E, bool) {
	t.Helper()

	deadline := time.Now().Add(pollTimeout)
	var lastListing sharedListE2EOutput
	var lastErr error

	for attempt := 0; ; attempt++ {
		listing, err := sharedListForRecipientRaw(cfgPath, env, fixture.ParentEmail)
		if err == nil {
			lastListing = listing
			for i := range listing.Items {
				item := listing.Items[i]
				if item.Name != fixture.ShortcutName || item.Type != "folder" {
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
			message := fmt.Sprintf(
				"shortcut fixture %q did not appear in shared --json for %s with shared_by_email=%s within %v; last_items=%d last_err=%v",
				fixture.ShortcutName,
				fixture.ParentEmail,
				fixture.SharerEmail,
				pollTimeout,
				len(lastListing.Items),
				lastErr,
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
	fixture resolvedShortcutFixture,
) {
	t.Helper()

	waitForShortcutRootPlaceholder(t, cfgPath, env, fixture, "shortcut fixture root entry missing")
}

func requireShortcutSentinelVisible(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture resolvedShortcutFixture,
) {
	t.Helper()

	downloadRoot := filepath.Join(t.TempDir(), "shared-target")
	runCLIWithoutDrive(t, cfgPath, env, "get", fixture.SharedItem.Selector, downloadRoot)

	sentinelPath := filepath.Join(downloadRoot, filepath.FromSlash(strings.TrimPrefix(path.Clean(fixture.SentinelPath), "/")))
	assert.FileExists(t, sentinelPath)
}

func requireActiveShortcutChild(
	t *testing.T,
	env map[string]string,
	fixture resolvedShortcutFixture,
) shortcutChildE2ERecord {
	t.Helper()

	record, ok := findActiveShortcutChild(t, env, fixture, fixture.ShortcutName)
	if ok {
		return record
	}

	require.Failf(t,
		"active shortcut child mount missing",
		"parent=%s shortcut=%q remote=%s/%s",
		fixture.ParentDrive,
		fixture.ShortcutName,
		fixture.SharedItem.RemoteDriveID,
		fixture.SharedItem.RemoteItemID,
	)
	return shortcutChildE2ERecord{}
}

func requireActiveShortcutChildAtPath(
	t *testing.T,
	env map[string]string,
	fixture resolvedShortcutFixture,
	relativeLocalPath string,
) shortcutChildE2ERecord {
	t.Helper()

	record, ok := findActiveShortcutChild(t, env, fixture, relativeLocalPath)
	if ok {
		return record
	}

	require.Failf(t,
		"active shortcut child mount missing at projection path",
		"parent=%s shortcut=%q relative_path=%q remote=%s/%s",
		fixture.ParentDrive,
		fixture.ShortcutName,
		relativeLocalPath,
		fixture.SharedItem.RemoteDriveID,
		fixture.SharedItem.RemoteItemID,
	)
	return shortcutChildE2ERecord{}
}

func findActiveShortcutChild(
	t *testing.T,
	env map[string]string,
	fixture resolvedShortcutFixture,
	relativeLocalPath string,
) (shortcutChildE2ERecord, bool) {
	t.Helper()

	for _, record := range shortcutChildRecords(t, env, fixture) {
		if record.NamespaceID != fixture.ParentDrive ||
			record.RelativeLocalPath != relativeLocalPath {
			continue
		}
		if fixture.SharedItem.RemoteDriveID != "" && record.RemoteDriveID != fixture.SharedItem.RemoteDriveID {
			continue
		}
		if fixture.SharedItem.RemoteItemID != "" && record.RemoteItemID != fixture.SharedItem.RemoteItemID {
			continue
		}
		assert.Equal(t, syncengine.ShortcutRootStateActive, record.State)
		assert.Empty(t, record.StateReason)
		return record, true
	}

	return shortcutChildE2ERecord{}, false
}

func shortcutChildRecords(
	t *testing.T,
	env map[string]string,
	fixture resolvedShortcutFixture,
) []shortcutChildE2ERecord {
	t.Helper()

	parentID := driveid.MustCanonicalID(fixture.ParentDrive)
	statePath := e2eDriveStatePath(env, parentID)
	roots, err := syncengine.ReadShortcutRootStatusSnapshot(t.Context(), statePath, nil)
	require.NoError(t, err)
	records := make([]shortcutChildE2ERecord, 0, len(roots))
	for i := range roots {
		root := roots[i]
		state := syncengine.ShortcutRootState(root.Metadata.DisplayState)
		if state == "" {
			state = syncengine.ShortcutRootStateActive
		}
		stateReason := root.Metadata.StateReason
		records = append(records, shortcutChildE2ERecord{
			MountID:           config.ChildMountID(parentID.String(), root.BindingItemID),
			NamespaceID:       parentID.String(),
			BindingItemID:     root.BindingItemID,
			RelativeLocalPath: root.RelativeLocalPath,
			RemoteDriveID:     root.RemoteDriveID,
			RemoteItemID:      root.RemoteItemID,
			State:             state,
			StateReason:       stateReason,
		})
	}
	return records
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
	fixture resolvedShortcutFixture,
) {
	t.Helper()

	listing := sharedListForRecipient(t, cfgPath, env, fixture.ParentEmail)
	var names []string
	for i := range listing.Items {
		names = append(names, listing.Items[i].Name)
		if listing.Items[i].Name != fixture.ShortcutName || listing.Items[i].Type != "folder" {
			continue
		}
		if fixture.SharerEmail != "" && !strings.EqualFold(listing.Items[i].SharedByEmail, fixture.SharerEmail) {
			continue
		}

		return
	}

	require.Failf(t,
		"shortcut fixture missing from shared list",
		"expected folder %q shared_by=%s for %s; shared_list_names=%v",
		fixture.ShortcutName,
		fixture.SharerEmail,
		fixture.ParentEmail,
		names,
	)
}

func requireRootPlaceholderContainsShortcutFixture(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture resolvedShortcutFixture,
) {
	t.Helper()

	waitForShortcutRootPlaceholder(t, cfgPath, env, fixture, "shortcut fixture root placeholder missing")
}

func waitForShortcutRootPlaceholder(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	fixture resolvedShortcutFixture,
	failureTitle string,
) {
	t.Helper()

	deadline := time.Now().Add(pollTimeout)
	var lastNames []string
	var lastStdout string
	var lastErr error
	for attempt := 0; ; attempt++ {
		stdout, _, err := runCLIWithConfigAllowErrorForDrive(t, cfgPath, env, fixture.ParentDrive, "ls", "--json", "/")
		lastStdout = stdout
		lastErr = err
		if err == nil {
			var items []remoteListJSONItem
			if decodeErr := json.Unmarshal([]byte(stdout), &items); decodeErr != nil {
				lastErr = decodeErr
			} else {
				lastNames = rootListingNames(items)
				if stringsContain(lastNames, fixture.ShortcutName) {
					return
				}
			}
		}

		if time.Now().After(deadline) {
			require.Failf(t,
				failureTitle,
				"expected %q in root of %s within %v; root_names=%v last_err=%v last_stdout=%s",
				fixture.ShortcutName,
				fixture.ParentDrive,
				pollTimeout,
				lastNames,
				lastErr,
				lastStdout,
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
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
	entry := entryValue.(*sharedFileFixtureCacheEntry)
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
		cfgPath, env, cleanup, err := writeFixtureConfigForDriveID(candidateDriveID)
		if err != nil {
			return resolvedSharedFileFixture{}, err
		}
		defer cleanup()

		recipientEmail, err := recipientEmailFromCanonicalDriveID(candidateDriveID)
		if err != nil {
			return resolvedSharedFileFixture{}, err
		}

		_, stdout, stderr, err := execCLI(
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
			_ = stderr
			continue
		}

		var rawStat sharedStatE2EOutput
		if err := json.Unmarshal([]byte(stdout), &rawStat); err != nil {
			return resolvedSharedFileFixture{}, fmt.Errorf(
				"parse raw shared-link stat output for candidate %s: %w",
				candidateDriveID,
				err,
			)
		}
		if rawStat.AccountEmail != "" && rawStat.AccountEmail != recipientEmail {
			continue
		}

		fileItem := sharedItemE2E{
			Selector:      rawStat.SharedSelector,
			Type:          "file",
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
					listing.Items[i].Type != "file" {
					continue
				}

				fileItem = listing.Items[i]
				listingMatched = true
				break
			}
		}

		candidates = append(candidates, sharedFileCandidate{
			fixture: resolvedSharedFileFixture{
				RecipientDriveID: candidateDriveID,
				RecipientEmail:   recipientEmail,
				RawStat:          rawStat,
				FileItem:         fileItem,
			},
			listingMatched: listingMatched,
		})
	}

	return selectSharedFileFixture(candidates)
}

func selectSharedFileFixture(candidates []sharedFileCandidate) (resolvedSharedFileFixture, error) {
	if len(candidates) == 0 {
		return resolvedSharedFileFixture{}, fmt.Errorf(
			"shared-file fixture did not resolve for any configured recipient account",
		)
	}

	if len(candidates) == 1 {
		return candidates[0].fixture, nil
	}

	var listingMatched []sharedFileCandidate
	for i := range candidates {
		if candidates[i].listingMatched {
			listingMatched = append(listingMatched, candidates[i])
		}
	}

	if len(listingMatched) == 1 {
		return listingMatched[0].fixture, nil
	}

	if !sharedFileCandidatesShareRemoteIdentity(candidates) {
		return resolvedSharedFileFixture{}, fmt.Errorf(
			"shared-file fixture resolved to multiple distinct configured recipient accounts (%d matches)",
			len(candidates),
		)
	}

	if len(listingMatched) > 0 {
		return listingMatched[0].fixture, nil
	}

	return candidates[0].fixture, nil
}

func sharedFileCandidatesShareRemoteIdentity(candidates []sharedFileCandidate) bool {
	if len(candidates) == 0 {
		return false
	}

	expectedDriveID := candidates[0].fixture.RawStat.RemoteDriveID
	expectedItemID := candidates[0].fixture.RawStat.RemoteItemID
	if expectedDriveID == "" || expectedItemID == "" {
		return false
	}

	for i := 1; i < len(candidates); i++ {
		if candidates[i].fixture.RawStat.RemoteDriveID != expectedDriveID ||
			candidates[i].fixture.RawStat.RemoteItemID != expectedItemID {
			return false
		}
	}

	return true
}

func resolveSharedFolderFixture(t *testing.T, selector string) resolvedSharedFolderFixture {
	t.Helper()

	require.NotEmpty(t, selector,
		"shared-folder fixture missing: store the selector in exported env, root .env, or .testdata/fixtures.env")

	entryValue, _ := sharedFolderFixtureCache.LoadOrStore(selector, &sharedFolderFixtureCacheEntry{})
	entry := entryValue.(*sharedFolderFixtureCacheEntry)
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
			"shared fixture recipient %q does not match any configured drive (%v)",
			ref.AccountEmail,
			liveConfig.CandidateDriveIDs(),
		)
	}

	cfgPath, env, cleanup, err := writeFixtureConfigForDriveID(recipientDriveID)
	if err != nil {
		return resolvedSharedFolderFixture{}, err
	}
	defer cleanup()

	item := sharedItemE2E{
		Selector:      selector,
		Type:          "folder",
		AccountEmail:  ref.AccountEmail,
		RemoteDriveID: ref.RemoteDriveID,
		RemoteItemID:  ref.RemoteItemID,
	}

	listing, err := sharedListForRecipientRaw(cfgPath, env, ref.AccountEmail)
	if err == nil {
		for i := range listing.Items {
			if listing.Items[i].RemoteDriveID == ref.RemoteDriveID &&
				listing.Items[i].RemoteItemID == ref.RemoteItemID &&
				listing.Items[i].Type == "folder" {
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
		_ = os.RemoveAll(perTestData)
		return "", nil, nil, fmt.Errorf("create fixture HOME dir: %w", err)
	}

	cfgDir, err := os.MkdirTemp("", "onedrive-shared-fixture-config-*")
	if err != nil {
		_ = os.RemoveAll(perTestData)
		_ = os.RemoveAll(perTestHome)
		return "", nil, nil, fmt.Errorf("create fixture config dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(perTestData)
		_ = os.RemoveAll(perTestHome)
		_ = os.RemoveAll(cfgDir)
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

func copyFixtureFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read fixture file %s: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir fixture destination for %s: %w", dst, err)
	}

	if err := os.WriteFile(dst, data, perm); err != nil {
		return fmt.Errorf("write fixture file %s: %w", dst, err)
	}

	return nil
}

func sharedListForRecipientRaw(cfgPath string, env map[string]string, recipientEmail string) (sharedListE2EOutput, error) {
	_, stdout, stderr, err := execCLI(cfgPath, env, "", "--account", recipientEmail, "shared", "--json")
	if err != nil {
		return sharedListE2EOutput{}, fmt.Errorf("shared --json for %s: %w (%s)", recipientEmail, err, strings.TrimSpace(stderr))
	}

	var parsed sharedListE2EOutput
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return sharedListE2EOutput{}, fmt.Errorf("parse shared --json for %s: %w", recipientEmail, err)
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
	var lastStdout string
	var lastStderr string

	for attempt := 0; ; attempt++ {
		stdout, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, "drive", "list", "--json")
		lastStdout = stdout
		lastStderr = stderr

		if err != nil {
			if !isRetryableGraphGatewayFailure(stderr) {
				require.NoErrorf(t, err, "drive list --json should succeed\nstdout: %s\nstderr: %s", stdout, stderr)
			}
		} else {
			var parsed driveListE2EOutput
			require.NoErrorf(t, json.Unmarshal([]byte(stdout), &parsed), "drive list --json output should be valid JSON, got: %s", stdout)
			if driveListAvailableContainsCanonicalID(parsed, selector) {
				return parsed
			}
		}

		if time.Now().After(deadline) {
			t.Skipf(
				"live drive list shared discovery did not expose selector %q within %v; Graph search omitted the known fixture on this run\nlast stdout: %s\nlast stderr: %s",
				selector,
				pollTimeout,
				lastStdout,
				lastStderr,
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}

func sharedListForRecipient(t *testing.T, cfgPath string, env map[string]string, recipientEmail string) sharedListE2EOutput {
	t.Helper()

	stdout, _ := runCLIWithoutDrive(t, cfgPath, env, "--account", recipientEmail, "shared", "--json")

	var parsed sharedListE2EOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))

	return parsed
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

	data, err := os.ReadFile(localPath)
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

	require.Eventually(t, func() bool {
		localPath := filepath.Join(t.TempDir(), "downloaded")
		fullArgs := append([]string{"get"}, args...)
		fullArgs = append(fullArgs, localPath)

		_, _, err := runCLICore(t, cfgPath, env, "", fullArgs...)
		if err != nil {
			return false
		}

		data, readErr := os.ReadFile(localPath)
		if readErr != nil {
			return false
		}

		return string(data) == expected
	}, pollTimeout, 2*time.Second)
}

func findSharedItemByRemoteIDs(t *testing.T, items []sharedItemE2E, driveID, itemID, itemType string) sharedItemE2E {
	t.Helper()

	for i := range items {
		if items[i].RemoteDriveID == driveID && items[i].RemoteItemID == itemID && items[i].Type == itemType {
			return items[i]
		}
	}

	require.Failf(t, "shared item not found", "drive=%s item=%s type=%s", driveID, itemID, itemType)
	return sharedItemE2E{}
}
