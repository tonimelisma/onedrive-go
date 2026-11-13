package config

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// --- AppendDriveSection tests ---

func writeConfigFixture(t *testing.T, path string, data []byte) {
	t.Helper()

	root, err := os.OpenRoot(filepath.Dir(path))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, root.Close())
	}()

	file, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, configFilePermissions)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, file.Close())
	}()

	_, err = file.Write(data)
	require.NoError(t, err)
}

func TestAppendDriveSection_AppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/OneDrive - Contoso")
	require.NoError(t, err)

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `["personal:toni@outlook.com"]`)
	assert.Contains(t, content, `["business:alice@contoso.com"]`)
	assert.Contains(t, content, `sync_dir = "~/OneDrive - Contoso"`)
}

func TestAppendDriveSection_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/OneDrive - Contoso")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 2)

	personal := cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")]
	assert.Equal(t, "~/OneDrive", personal.SyncDir)

	business := cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")]
	assert.Equal(t, "~/OneDrive - Contoso", business.SyncDir)
}

func TestAppendDriveSection_FileWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Write file without trailing newline
	writeConfigFixture(t, path, []byte(`["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"`))

	err := AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/Work")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 2)
	assert.Equal(t, "~/Work", cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")].SyncDir)
}

// Validates: R-4.2.1
func TestAppendDriveSection_CreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	content := string(data)

	// Template header present (same as AppendDriveSection used to produce).
	assert.Contains(t, content, "# onedrive-go configuration")
	assert.Contains(t, content, "# log_level = \"info\"")
	assert.Contains(t, content, "# poll_interval = \"5m\"")

	// Drive section present.
	assert.Contains(t, content, `["personal:toni@outlook.com"]`)
	assert.Contains(t, content, `sync_dir = "~/OneDrive"`)

	// Round-trip: verify the config loads correctly.
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
	assert.Equal(t, "~/OneDrive", cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")].SyncDir)
}

func TestAppendDriveSection_ConcurrentCreatesPreserveAllSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cids := []driveid.CanonicalID{
		driveid.MustCanonicalID("personal:toni@outlook.com"),
		driveid.MustCanonicalID("business:alice@contoso.com"),
	}

	start := make(chan struct{})
	errs := make(chan error, len(cids))
	var wg sync.WaitGroup
	for _, cid := range cids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- AppendDriveSection(path, cid, "~/"+cid.Email())
		}()
	}

	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Contains(t, cfg.Drives, cids[0])
	assert.Contains(t, cfg.Drives, cids[1])
}

// Validates: R-4.2.1
func TestAppendDriveSection_CrossProcessCreatesPreserveAllSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cids := []driveid.CanonicalID{
		driveid.MustCanonicalID("personal:toni@outlook.com"),
		driveid.MustCanonicalID("business:alice@contoso.com"),
		driveid.MustCanonicalID("business:bob@contoso.com"),
		driveid.MustCanonicalID("personal:chris@example.com"),
		driveid.MustCanonicalID("business:dana@contoso.com"),
		driveid.MustCanonicalID("personal:erin@example.com"),
	}

	type worker struct {
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		output *bytes.Buffer
	}

	workers := make([]worker, 0, len(cids))
	for _, cid := range cids {
		//nolint:gosec // Fixed worker subprocess uses the current test binary, a fixed test name, and controlled env.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAppendDriveSection_CrossProcessWorker$")
		cmd.Env = append(os.Environ(),
			"ONEDRIVE_GO_CONFIG_WRITE_WORKER=1",
			"ONEDRIVE_GO_CONFIG_WRITE_PATH="+path,
			"ONEDRIVE_GO_CONFIG_WRITE_CID="+cid.String(),
			"ONEDRIVE_GO_CONFIG_WRITE_SYNC_DIR=~/"+cid.Email(),
		)
		output := &bytes.Buffer{}
		cmd.Stdout = output
		cmd.Stderr = output

		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		workers = append(workers, worker{cmd: cmd, stdin: stdin, output: output})
	}

	for _, worker := range workers {
		require.NoError(t, worker.stdin.Close())
	}

	for _, worker := range workers {
		err := worker.cmd.Wait()
		require.NoError(t, err, worker.output.String())
	}

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	for _, cid := range cids {
		drive, found := cfg.Drives[cid]
		require.True(t, found, "missing drive section %s", cid.String())
		assert.Equal(t, "~/"+cid.Email(), drive.SyncDir)
	}
}

func TestAppendDriveSection_CrossProcessWorker(t *testing.T) {
	if os.Getenv("ONEDRIVE_GO_CONFIG_WRITE_WORKER") == "" {
		return
	}

	_, err := io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)

	cid := driveid.MustCanonicalID(os.Getenv("ONEDRIVE_GO_CONFIG_WRITE_CID"))
	require.NoError(t, AppendDriveSection(
		os.Getenv("ONEDRIVE_GO_CONFIG_WRITE_PATH"),
		cid,
		os.Getenv("ONEDRIVE_GO_CONFIG_WRITE_SYNC_DIR"),
	))
}

// Validates: R-4.2.1
func TestAppendDriveSection_RefusesASectionThatIsThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:refuse@example.com")
	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
	before, err := localpath.ReadFile(path)
	require.NoError(t, err)

	err = AppendDriveSection(path, cid, "~/Elsewhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already in the config")

	after, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// Validates: R-4.2.2
func TestTOMLString_EveryValueLoadsBackUnchanged(t *testing.T) {
	values := []string{
		"plain",
		`quote " and backslash \`,
		"tab\tnewline\nreturn\r",
		"bell\a vertical\v delete\x7f escape\x1b",
		"unicode ünïcödé 雲",
	}

	for _, value := range values {
		var decoded struct {
			Value string `toml:"value"`
		}
		_, err := toml.Decode("value = "+tomlString(value), &decoded)
		require.NoError(t, err, "tomlString(%q) is not TOML", value)
		assert.Equal(t, value, decoded.Value)
	}
}

func TestConfigWrites_DoNotUseAmbientLogger(t *testing.T) {
	var logOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
	require.NoError(t, SetDriveKey(path, cid, "sync_dir", "~/OneDrive Local"))
	require.NoError(t, DeleteDriveKey(path, cid, "sync_dir"))
	require.NoError(t, DeleteDriveSection(path, cid))

	assert.Empty(t, logOutput.String())
}

// Validates: R-4.2.1
func TestDefaultConfigTemplate_ListsAllGlobalKeys(t *testing.T) {
	t.Parallel()

	template := defaultConfigTemplate()
	keys := newKnownGlobalKeys()

	for key := range keys {
		assert.Contains(t, template, "# "+key+" =", "template should document %q", key)
	}
}

func TestAppendDriveSection_CreatesParentDirectoryWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/OneDrive - Contoso")
	require.NoError(t, err)

	_, err = os.Stat(path)
	assert.NoError(t, err)
}

func TestAppendDriveSection_FileNotFoundUnwritablePath(t *testing.T) {
	// A truly unwritable path (parent is a file, not a directory) should still fail.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("I'm a file"), 0o600))

	err := AppendDriveSection(filepath.Join(blocker, "sub", "config.toml"),
		driveid.MustCanonicalID("personal:test@test.com"), "~/OneDrive")
	require.Error(t, err)
}

// --- SetDriveKey tests ---

func TestSetDriveKey_InsertNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "display_name", "home")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "home", cfg.Drives[cid].DisplayName)
}

// Validates: R-4.2.2
func TestSetDriveKey_StringKeyGivenTrueLoadsBackAsAString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:typed@example.com")
	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))

	require.NoError(t, SetDriveKey(path, cid, "display_name", "true"))

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err, "a string key must stay a string")
	assert.Equal(t, "true", cfg.Drives[cid].DisplayName)
}

// Validates: R-4.2.2
func TestSetDriveKey_RefusesAValueItsKeyCannotHold(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "a bool key given a word", key: "paused", value: "yes"},
		{name: "a key the schema does not have", key: "pasued", value: "true"},
		{name: "a list key given one value", key: "ignored_dirs", value: "node_modules"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			cid := driveid.MustCanonicalID("personal:typed@example.com")
			require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
			before, err := localpath.ReadFile(path)
			require.NoError(t, err)

			err = SetDriveKey(path, cid, tc.key, tc.value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)

			after, err := localpath.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "a refused value must leave the file as it was")
		})
	}
}

// Validates: R-4.1.1, R-4.2.2
func TestDriveSection_NameWithQuoteAndBackslashRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	other := driveid.MustCanonicalID("personal:other@example.com")
	cid := driveid.MustCanonicalID(`sharepoint:alice@contoso.com:site:Docs "Q1" \ Archive`)
	require.NoError(t, AppendDriveSection(path, other, "~/Other"))

	require.NoError(t, AppendDriveSection(path, cid, "~/Archive"))
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Contains(t, cfg.Drives, cid)
	assert.Equal(t, "~/Archive", cfg.Drives[cid].SyncDir)

	err = AppendDriveSection(path, cid, "~/Elsewhere")
	require.Error(t, err, "the section must be found under its own name")
	assert.Contains(t, err.Error(), "already in the config")

	require.NoError(t, SetDriveKey(path, cid, "display_name", "Archive"))
	cfg, err = Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "Archive", cfg.Drives[cid].DisplayName)

	require.NoError(t, DeleteDriveSection(path, cid))
	cfg, err = Load(path, testLogger(t))
	require.NoError(t, err)
	assert.NotContains(t, cfg.Drives, cid)
	assert.Contains(t, cfg.Drives, other)
}

// Validates: R-4.2.2
func TestParseSectionHeader_ReadsEveryHeaderForm(t *testing.T) {
	cases := []struct {
		line     string
		wantName string
		wantOK   bool
	}{
		{line: `["personal:a@example.com"]`, wantName: "personal:a@example.com", wantOK: true},
		{line: `[ "personal:a@example.com" ]`, wantName: "personal:a@example.com", wantOK: true},
		{line: `["sharepoint:a@example.com:site:Docs \"Q1\" \\ Archive"]`, wantName: `sharepoint:a@example.com:site:Docs "Q1" \ Archive`, wantOK: true},
		{line: `['personal:a@example.com']`, wantName: "personal:a@example.com", wantOK: true},
		{line: `["personal:a@example.com"] # my drive`, wantName: "personal:a@example.com", wantOK: true},
		{line: `[filter]`, wantName: "filter", wantOK: true},
		{line: `[["personal:a@example.com"]]`, wantOK: false},
		{line: `["personal:a@example.com"] trailing`, wantOK: false},
		{line: `sync_dir = "~/OneDrive"`, wantOK: false},
	}

	for _, tc := range cases {
		name, ok := parseSectionHeader(tc.line)
		assert.Equal(t, tc.wantOK, ok, "%s", tc.line)
		if tc.wantOK {
			assert.Equal(t, tc.wantName, name, "%s", tc.line)
		}
	}
}

func TestSetDriveKey_UpdateExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	// First set
	err = SetDriveKey(path, cid, "display_name", "home")
	require.NoError(t, err)

	// Update
	err = SetDriveKey(path, cid, "display_name", "personal")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "personal", cfg.Drives[cid].DisplayName)
}

func TestSetDriveKey_BooleanFormatting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "paused", "true")
	require.NoError(t, err)

	// Verify the raw file content has bare true (not "true")
	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "paused = true")
	assert.NotContains(t, string(data), `paused = "true"`)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused)
}

func TestSetDriveKey_StringFormatting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "display_name", "work")
	require.NoError(t, err)

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `display_name = "work"`)
}

func TestSetDriveKey_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "paused", "false")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	require.NotNil(t, d.Paused)
	assert.False(t, *d.Paused)
}

func TestSetDriveKey_SectionNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, driveid.MustCanonicalID("business:nobody@example.com"), "paused", "true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSetDriveKey_FileNotFound(t *testing.T) {
	err := SetDriveKey("/nonexistent/config.toml", driveid.MustCanonicalID("personal:test@test.com"), "paused", "true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
}

func TestSetDriveKey_MultipleSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	// Set key on the second section only
	err = SetDriveKey(path, businessCID, "paused", "true")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	// First section should be unaffected
	personal := cfg.Drives[personalCID]
	assert.Nil(t, personal.Paused) // not set

	// Second section should have paused = true
	business := cfg.Drives[businessCID]
	require.NotNil(t, business.Paused)
	assert.True(t, *business.Paused)
}

// --- DeleteDriveKey tests ---

func TestDeleteDriveKey_KeyExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
	require.NoError(t, SetDriveKey(path, cid, "paused", "true"))

	// Verify key exists before deletion.
	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "paused = true")

	// Delete the key.
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))

	data, err = localpath.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "paused")

	// Round-trip: verify the config loads correctly.
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused)
}

func TestDeleteDriveKey_KeyAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))

	// Deleting a key that doesn't exist should be idempotent.
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused)
}

func TestDeleteDriveKey_SectionNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:toni@outlook.com")
	otherCID := driveid.MustCanonicalID("personal:other@outlook.com")

	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))

	err := DeleteDriveKey(path, otherCID, "paused")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteDriveKey_IdempotentDoubleDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
	require.NoError(t, SetDriveKey(path, cid, "paused", "true"))

	// Delete twice — second should be no-op.
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "paused")
}

func TestDeleteDriveKey_PausedUntilRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	require.NoError(t, AppendDriveSection(path, cid, "~/OneDrive"))
	require.NoError(t, SetDriveKey(path, cid, "paused", "true"))
	require.NoError(t, SetDriveKey(path, cid, "paused_until", "2026-03-01T00:00:00Z"))

	// Verify both keys load.
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	require.NotNil(t, d.Paused)
	assert.True(t, *d.Paused)
	require.NotNil(t, d.PausedUntil)
	assert.Equal(t, "2026-03-01T00:00:00Z", *d.PausedUntil)

	// Delete both keys.
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))
	require.NoError(t, DeleteDriveKey(path, cid, "paused_until"))

	cfg, err = Load(path, testLogger(t))
	require.NoError(t, err)
	d = cfg.Drives[cid]
	assert.Nil(t, d.Paused)
	assert.Nil(t, d.PausedUntil)
}

// --- DeleteDriveSection tests ---

func TestDeleteDriveSection_DeleteFromMiddle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")
	spCID := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")

	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	err = AppendDriveSection(path, spCID, "~/Marketing")
	require.NoError(t, err)

	// Delete the middle section
	err = DeleteDriveSection(path, businessCID)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 2)
	assert.Contains(t, cfg.Drives, personalCID)
	assert.Contains(t, cfg.Drives, spCID)
	assert.NotContains(t, cfg.Drives, businessCID)
}

func TestDeleteDriveSection_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	err = DeleteDriveSection(path, personalCID)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
	assert.Equal(t, "~/Work", cfg.Drives[businessCID].SyncDir)
}

func TestDeleteDriveSection_SectionNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	err = DeleteDriveSection(path, driveid.MustCanonicalID("business:nobody@example.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteDriveSection_FileNotFound(t *testing.T) {
	err := DeleteDriveSection("/nonexistent/config.toml", driveid.MustCanonicalID("personal:test@test.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
}

// --- DefaultSyncDir tests ---

func TestDefaultSyncDir_Personal_NoCollision(t *testing.T) {
	result := DefaultSyncDir(driveid.MustCanonicalID("personal:user@example.com"), "", "", nil)
	assert.Equal(t, "~/OneDrive", result)
}

func TestDefaultSyncDir_Personal_WithCollision_DisplayName(t *testing.T) {
	existing := []string{"~/OneDrive"}
	result := DefaultSyncDir(driveid.MustCanonicalID("personal:user@example.com"), "", "Alice Smith", existing)
	assert.Equal(t, "~/OneDrive - Alice Smith", result)
}

func TestDefaultSyncDir_Personal_WithCollision_NoDisplayName(t *testing.T) {
	// Without display name, falls back to email.
	existing := []string{"~/OneDrive"}
	result := DefaultSyncDir(driveid.MustCanonicalID("personal:user@example.com"), "", "", existing)
	assert.Equal(t, "~/OneDrive - user@example.com", result)
}

func TestDefaultSyncDir_Personal_WithCollision_DisplayNameAlsoCollides(t *testing.T) {
	// Both base and displayName collide — falls back to email.
	existing := []string{"~/OneDrive", "~/OneDrive - Alice Smith"}
	result := DefaultSyncDir(driveid.MustCanonicalID("personal:user@example.com"), "", "Alice Smith", existing)
	assert.Equal(t, "~/OneDrive - user@example.com", result)
}

func TestDefaultSyncDir_Business(t *testing.T) {
	result := DefaultSyncDir(driveid.MustCanonicalID("business:alice@contoso.com"), "Contoso", "", nil)
	assert.Equal(t, "~/OneDrive - Contoso", result)
}

func TestDefaultSyncDir_Business_NoOrgName(t *testing.T) {
	result := DefaultSyncDir(driveid.MustCanonicalID("business:alice@contoso.com"), "", "", nil)
	assert.Equal(t, "~/OneDrive - Business", result)
}

func TestDefaultSyncDir_Business_SameOrg_Collision(t *testing.T) {
	existing := []string{"~/OneDrive - Contoso"}
	result := DefaultSyncDir(driveid.MustCanonicalID("business:bob@contoso.com"), "Contoso", "Bob Smith", existing)
	assert.Equal(t, "~/OneDrive - Contoso - Bob Smith", result)
}

func TestDefaultSyncDir_SharePoint(t *testing.T) {
	result := DefaultSyncDir(driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents"), "", "", nil)
	assert.Equal(t, "~/SharePoint - marketing - Documents", result)
}

func TestDefaultSyncDir_SharePoint_MinimalID(t *testing.T) {
	// SharePoint canonical ID with only email (no site/library) falls back to ~/SharePoint.
	result := DefaultSyncDir(driveid.MustCanonicalID("sharepoint:alice@contoso.com"), "", "", nil)
	assert.Equal(t, "~/SharePoint", result)
}

func TestDefaultSyncDir_SharePoint_Collision(t *testing.T) {
	existing := []string{"~/SharePoint - marketing - Documents"}
	result := DefaultSyncDir(
		driveid.MustCanonicalID("sharepoint:bob@contoso.com:marketing:Documents"),
		"", "Bob Smith", existing,
	)
	assert.Equal(t, "~/SharePoint - marketing - Documents - Bob Smith", result)
}

// --- sanitizePathComponent tests ---

func TestSanitizePathComponent_Clean(t *testing.T) {
	assert.Equal(t, "Contoso Ltd", sanitizePathComponent("Contoso Ltd"))
}

func TestSanitizePathComponent_UnsafeChars(t *testing.T) {
	assert.Equal(t, "path-with-slashes", sanitizePathComponent("path/with\\slashes"))
	assert.Equal(t, "file-name-here", sanitizePathComponent("file:name<here"))
	assert.Equal(t, "pipe-and-question", sanitizePathComponent("pipe|and?question"))
	assert.Equal(t, "quotes-and-star", sanitizePathComponent(`quotes"and*star`))
	assert.Equal(t, "angle-brackets", sanitizePathComponent("angle>brackets"))
}

func TestSanitizePathComponent_Empty(t *testing.T) {
	assert.Empty(t, sanitizePathComponent(""))
}

// --- baseSyncDir tests ---

func TestBaseSyncDir_Personal(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("personal:user@example.com"), "", "")
	assert.Equal(t, "~/OneDrive", result)
}

func TestBaseSyncDir_Business_WithOrg(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("business:alice@contoso.com"), "Contoso", "")
	assert.Equal(t, "~/OneDrive - Contoso", result)
}

func TestBaseSyncDir_Business_NoOrg(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("business:alice@contoso.com"), "", "")
	assert.Equal(t, "~/OneDrive - Business", result)
}

func TestBaseSyncDir_SharePoint(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents"), "", "")
	assert.Equal(t, "~/SharePoint - marketing - Documents", result)
}

func TestBaseSyncDir_SharePoint_NoSiteOrLib(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("sharepoint:alice@contoso.com"), "", "")
	assert.Equal(t, "~/SharePoint", result)
}

func TestBaseSyncDir_Shared_WithDisplayName(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("shared:alice@contoso.com:drv123:item456"), "", "Project Alpha")
	assert.Equal(t, "~/OneDrive-Shared/Project Alpha", result)
}

func TestBaseSyncDir_Shared_NoDisplayName(t *testing.T) {
	result := baseSyncDir(driveid.MustCanonicalID("shared:alice@contoso.com:drv123:item456"), "", "")
	assert.Equal(t, "~/OneDrive-Shared", result)
}

func TestBaseSyncDir_Shared_UnsafeCharsInDisplayName(t *testing.T) {
	// sanitizePathComponent replaces filesystem-unsafe chars (/ \ : < > " | ? *)
	// but not ampersand (filesystem-safe).
	result := baseSyncDir(driveid.MustCanonicalID("shared:alice@contoso.com:drv123:item456"), "", "Sales & Marketing/Q1")
	assert.Equal(t, "~/OneDrive-Shared/Sales & Marketing-Q1", result)
}

// --- Comment preservation tests ---

// Validates: R-4.2.2
func TestCommentPreservation_AppendDriveSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := AppendDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	// Add a user comment by directly modifying the file
	data, err := localpath.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	content = strings.Replace(content, `["personal:toni@outlook.com"]`,
		"# My personal drive\n"+`["personal:toni@outlook.com"]`, 1)

	writeConfigFixture(t, path, []byte(content))

	// Now append a new section
	err = AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/Work")
	require.NoError(t, err)

	result, err := localpath.ReadFile(path)
	require.NoError(t, err)
	resultStr := string(result)

	// User comment preserved
	assert.Contains(t, resultStr, "# My personal drive")
	// Template comments preserved
	assert.Contains(t, resultStr, "# onedrive-go configuration")
	assert.Contains(t, resultStr, "# log_level = \"info\"")
	// Both sections present
	assert.Contains(t, resultStr, `["personal:toni@outlook.com"]`)
	assert.Contains(t, resultStr, `["business:alice@contoso.com"]`)
}

// Validates: R-4.2.2
func TestCommentPreservation_SetDriveKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Create config with a user comment above the drive section
	content := `# My custom header
log_level = "debug"

# Work drive for office stuff
["business:alice@contoso.com"]
sync_dir = "~/Work"
`
	writeConfigFixture(t, path, []byte(content))

	err := SetDriveKey(path, driveid.MustCanonicalID("business:alice@contoso.com"), "paused", "true")
	require.NoError(t, err)

	result, err := localpath.ReadFile(path)
	require.NoError(t, err)
	resultStr := string(result)

	assert.Contains(t, resultStr, "# My custom header")
	assert.Contains(t, resultStr, "# Work drive for office stuff")
	assert.Contains(t, resultStr, "paused = true")
}

// Validates: R-4.2.2
func TestCommentPreservation_DeleteDriveSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `# Global header comment
log_level = "debug"

# First drive comment
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"

# Second drive comment
["business:alice@contoso.com"]
sync_dir = "~/Work"
`
	writeConfigFixture(t, path, []byte(content))

	err := DeleteDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"))
	require.NoError(t, err)

	result, err := localpath.ReadFile(path)
	require.NoError(t, err)
	resultStr := string(result)

	// Global header preserved
	assert.Contains(t, resultStr, "# Global header comment")
	// Second drive comment preserved
	assert.Contains(t, resultStr, "# Second drive comment")
	// First drive and its comment removed
	assert.NotContains(t, resultStr, `["personal:toni@outlook.com"]`)
	// But only the blank lines before the section, not the comment that's part
	// of the remaining content
	assert.Contains(t, resultStr, `["business:alice@contoso.com"]`)
}

// --- atomicWriteFile tests ---

func TestAtomicWriteFile_WritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	err := atomicWriteFile(path, []byte("hello"))
	require.NoError(t, err)

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestAtomicWriteFile_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "test.txt")

	err := atomicWriteFile(path, []byte("hello"))
	require.NoError(t, err)

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

// Validates: R-6.8.23
//
// The config file names accounts in its `personal:<email>` section keys, so
// it is owner-only. The literal mode is asserted, not the constant, so a
// loosened constant fails here.
func TestAtomicWriteFile_SetsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	err := atomicWriteFile(path, []byte("hello"))
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// Validates: R-6.8.23
//
// A folder the client creates for its own state is owner-only, as the XDG
// base directory rule asks of a directory created where none existed: the
// config folder holds the account list, and the catalog and the data folder
// go through the same writer. A folder that already exists keeps its mode.
func TestAtomicWriteFile_CreatesItsFoldersOwnerOnly(t *testing.T) {
	t.Parallel()

	t.Run("new folders", func(t *testing.T) {
		t.Parallel()

		base := t.TempDir()
		path := filepath.Join(base, "onedrive-go", "nested", "config.toml")
		require.NoError(t, atomicWriteFile(path, []byte("hello")))

		for _, dir := range []string{filepath.Join(base, "onedrive-go"), filepath.Dir(path)} {
			info, err := os.Stat(dir)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), dir)
		}
	})

	t.Run("folder created by the mutation lock", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "onedrive-go")
		path := filepath.Join(dir, "config.toml")
		require.NoError(t, AppendDriveSection(path, driveid.MustCanonicalID("personal:user@example.com"), "~/OneDrive"))

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	})

	t.Run("existing folder keeps its mode", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "onedrive-go")
		require.NoError(t, os.Mkdir(dir, 0o750))
		require.NoError(t, atomicWriteFile(filepath.Join(dir, "config.toml"), []byte("hello")))

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o750), info.Mode().Perm(), "an existing folder is never chmodded")
	})
}

func TestAtomicWriteFile_InvalidDirectory(t *testing.T) {
	// Use a path under a file (not a directory) to trigger MkdirAll failure.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	err := os.WriteFile(blocker, []byte("I'm a file"), configFilePermissions)
	require.NoError(t, err)

	path := filepath.Join(blocker, "sub", "test.txt")
	err = atomicWriteFile(path, []byte("hello"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating root directory")
	assert.Contains(t, err.Error(), path, "the error names the file")
}

// --- newDriveKey tests ---

// Validates: R-4.2.2
func TestNewDriveKey_RendersEachKindItsWay(t *testing.T) {
	cases := []struct {
		key  string
		in   string
		want string
	}{
		{key: "paused", in: "true", want: "true"},
		{key: "follow_symlinks", in: "false", want: "false"},
		{key: "display_name", in: "true", want: `"true"`},
		{key: "sync_dir", in: "~/OneDrive", want: `"~/OneDrive"`},
		{key: "paused_until", in: "2026-03-01T00:00:00Z", want: `"2026-03-01T00:00:00Z"`},
	}

	for _, tc := range cases {
		key, err := newDriveKey(tc.key, tc.in)
		require.NoError(t, err, tc.key)
		assert.Equal(t, tc.key, key.name)
		assert.Equal(t, tc.want, key.formattedValue, tc.key)
	}
}

// --- driveSection tests ---

func TestDriveSection_Format(t *testing.T) {
	result := driveSection("personal:toni@outlook.com", []driveKey{{name: syncDirKey, formattedValue: `"~/OneDrive"`}})
	assert.Equal(t, "\n[\"personal:toni@outlook.com\"]\nsync_dir = \"~/OneDrive\"\n", result)
}

// --- B-284 edge case tests ---

func TestSetDriveKey_InlineCommentPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused = true # temporarily paused
`
	writeConfigFixture(t, path, []byte(content))

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")
	require.NoError(t, SetDriveKey(path, cid, "paused", "false"))

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	resultStr := string(data)

	assert.Contains(t, resultStr, "paused = false # temporarily paused")
}

func TestDeleteDriveKey_PausedDoesNotMatchPausedUntil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused_until = "2026-03-01T00:00:00Z"
paused = true
`
	writeConfigFixture(t, path, []byte(content))

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	// Deleting "paused" must NOT delete "paused_until".
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	resultStr := string(data)

	assert.NotContains(t, resultStr, "paused = true")
	assert.Contains(t, resultStr, "paused_until")

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused, "paused should be nil after deletion")
	require.NotNil(t, d.PausedUntil, "paused_until should be preserved")
	assert.Equal(t, "2026-03-01T00:00:00Z", *d.PausedUntil)
}

func TestSetDriveKey_NoSpacesAroundEquals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	// Write config with key=value (no spaces around =).
	content := `["personal:toni@outlook.com"]
sync_dir="~/OneDrive"
`
	writeConfigFixture(t, path, []byte(content))

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")
	require.NoError(t, SetDriveKey(path, cid, "sync_dir", "~/NewPath"))

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "~/NewPath", cfg.Drives[cid].SyncDir)
}

func TestDeleteDriveKey_NoSpacesAroundEquals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
paused=true
`
	writeConfigFixture(t, path, []byte(content))

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")
	require.NoError(t, DeleteDriveKey(path, cid, "paused"))

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "paused")
}

func TestSetDriveKey_ValueWithHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	content := `["personal:toni@outlook.com"]
sync_dir = "~/path#with#hash"
`
	writeConfigFixture(t, path, []byte(content))

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")
	require.NoError(t, SetDriveKey(path, cid, "display_name", "my drive"))

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	// Original value with # should be preserved.
	assert.Equal(t, "~/path#with#hash", cfg.Drives[cid].SyncDir)
	assert.Equal(t, "my drive", cfg.Drives[cid].DisplayName)
}

// --- Integration scenario tests ---

func TestScenario_FirstLoginThenSecondLogin(t *testing.T) {
	// Simulates: first login creates config, second login appends drive
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	// First login
	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	// Second login
	err = AppendDriveSection(path, businessCID, "~/OneDrive - Contoso")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 2)

	// Defaults are loaded correctly
	assert.Equal(t, "info", cfg.LogLevel)
}

func TestScenario_DriveRemove(t *testing.T) {
	// Simulates: login, then drive remove (deletes config section)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	err = DeleteDriveSection(path, businessCID)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
	assert.Contains(t, cfg.Drives, personalCID)
	assert.NotContains(t, cfg.Drives, businessCID)
}

func TestScenario_LogoutPurge_AllDrives(t *testing.T) {
	// Simulates: logout --purge removes all sections for an account
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")
	spCID := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")

	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	err = AppendDriveSection(path, spCID, "~/Marketing")
	require.NoError(t, err)

	// Purge all alice drives
	err = DeleteDriveSection(path, businessCID)
	require.NoError(t, err)

	err = DeleteDriveSection(path, spCID)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
	assert.Contains(t, cfg.Drives, personalCID)
}

func TestScenario_SetKeyThenDeleteSection(t *testing.T) {
	// Set a key, then delete the entire section
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "display_name", "home")
	require.NoError(t, err)

	err = DeleteDriveSection(path, cid)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Empty(t, cfg.Drives)
}

func TestSetDriveKey_UpdateSyncDir(t *testing.T) {
	// Verify that updating sync_dir (which already exists) replaces the line
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "sync_dir", "~/NewDrive")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "~/NewDrive", cfg.Drives[cid].SyncDir)
}

func TestScenario_LogoutRegular(t *testing.T) {
	// Regular logout (without --purge) deletes config sections but preserves
	// state DB files on disk.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := AppendDriveSection(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	// Simulate state DB files that would exist on disk.
	stateDB1 := filepath.Join(dir, "state_personal_toni@outlook.com.db")
	stateDB2 := filepath.Join(dir, "state_business_alice@contoso.com.db")
	require.NoError(t, os.WriteFile(stateDB1, []byte("fake-state-1"), 0o600))
	require.NoError(t, os.WriteFile(stateDB2, []byte("fake-state-2"), 0o600))

	// Delete both config sections (simulating regular logout).
	err = DeleteDriveSection(path, personalCID)
	require.NoError(t, err)

	err = DeleteDriveSection(path, businessCID)
	require.NoError(t, err)

	// Verify config has 0 drives.
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Empty(t, cfg.Drives)

	// Verify state DB files are still on disk (not deleted by config operations).
	_, err = os.Stat(stateDB1)
	require.NoError(t, err, "state DB for personal drive should still exist")
	_, err = os.Stat(stateDB2)
	require.NoError(t, err, "state DB for business drive should still exist")
}

func TestScenario_DriveRemoveThenReAdd(t *testing.T) {
	// Remove a drive then re-add it: the new section should have fresh defaults.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	// Create config with drive and customize it.
	err := AppendDriveSection(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "paused", "true")
	require.NoError(t, err)

	// Verify paused is set before removal.
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, cfg.Drives[cid].Paused)
	assert.True(t, *cfg.Drives[cid].Paused)

	// Remove the drive.
	err = DeleteDriveSection(path, cid)
	require.NoError(t, err)

	cfg, err = Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Empty(t, cfg.Drives)

	// Re-add the drive with a different sync_dir.
	err = AppendDriveSection(path, cid, "~/OneDrive-Fresh")
	require.NoError(t, err)

	// Reload and verify fresh defaults (Paused should be nil, new sync_dir).
	cfg, err = Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)

	d := cfg.Drives[cid]
	assert.Nil(t, d.Paused, "re-added drive should have nil Paused (fresh default)")
	assert.Equal(t, "~/OneDrive-Fresh", d.SyncDir)
}

// --- RenameDriveSections tests ---

func TestRenameDriveSections_PreservesCommentsAndKeyOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	writeConfigFixture(t, path, []byte(`# preamble
["business:user@example.com"]
# keep me
sync_dir = "~/Business"
display_name = "Work"
`))

	err := RenameDriveSections(path, map[driveid.CanonicalID]driveid.CanonicalID{
		driveid.MustCanonicalID("business:user@example.com"): driveid.MustCanonicalID("business:renamed@example.com"),
	})
	require.NoError(t, err)

	data, err := localpath.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, `# preamble
["business:renamed@example.com"]
# keep me
sync_dir = "~/Business"
display_name = "Work"
`, string(data))
}

// A section under the new name beside the one under the old name is a
// collision that names both, and the file is left as it was.
//
// Validates: R-3.7.2
func TestRenameDriveSections_Collision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	original := []byte(`["business:user@example.com"]
sync_dir = "~/Business"

["business:renamed@example.com"]
sync_dir = "~/Renamed"
`)
	writeConfigFixture(t, path, original)

	err := RenameDriveSections(path, map[driveid.CanonicalID]driveid.CanonicalID{
		driveid.MustCanonicalID("business:user@example.com"): driveid.MustCanonicalID("business:renamed@example.com"),
	})
	var collision *driveSectionCollisionError
	require.ErrorAs(t, err, &collision)
	assert.Equal(t, "business:user@example.com", collision.from)
	assert.Equal(t, "business:renamed@example.com", collision.to)
	require.ErrorContains(t, err, "already exists")

	data, readErr := localpath.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, string(original), string(data))
}

// A rename whose source section is gone is done already, so the file is not
// rewritten: a resumed email change finds its sections renamed and leaves
// the file as it is.
//
// Validates: R-3.7.3
func TestRenameDriveSections_NothingToRenameLeavesTheFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeConfigFixture(t, path, []byte(`["business:renamed@example.com"]
sync_dir = "~/Business"
`))
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, RenameDriveSections(path, map[driveid.CanonicalID]driveid.CanonicalID{
		driveid.MustCanonicalID("business:user@example.com"): driveid.MustCanonicalID("business:renamed@example.com"),
	}))

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "the config file was not replaced")
}
