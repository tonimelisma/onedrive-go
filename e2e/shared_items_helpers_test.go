//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sharedref"
	"github.com/tonimelisma/onedrive-go/testutil"
)

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

		stdout, stderr, err := runCLICoreRaw(
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

func runCLICoreRaw(cfgPath string, env map[string]string, driveID string, args ...string) (string, string, error) {
	var fullArgs []string
	if cfgPath != "" {
		fullArgs = append(fullArgs, "--config", cfgPath)
	}

	if driveID != "" {
		fullArgs = append(fullArgs, "--drive", driveID)
	}

	if shouldAddDebug(args) {
		fullArgs = append(fullArgs, "--debug")
	}

	fullArgs = append(fullArgs, args...)
	cmd := makeCmd(fullArgs, env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

func sharedListForRecipientRaw(cfgPath string, env map[string]string, recipientEmail string) (sharedListE2EOutput, error) {
	stdout, stderr, err := runCLICoreRaw(cfgPath, env, "", "--account", recipientEmail, "shared", "--json")
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

func runCLIWithoutDrive(t *testing.T, cfgPath string, env map[string]string, args ...string) (string, string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", args...)
	require.NoErrorf(t, err, "CLI command %v failed\nstdout: %s\nstderr: %s", args, stdout, stderr)

	return stdout, stderr
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
