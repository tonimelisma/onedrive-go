package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// --- CreateConfigWithDrive tests ---

func TestCreateConfigWithDrive_CreatesFileWithTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)

	// Template header present
	assert.Contains(t, content, "# onedrive-go configuration")
	assert.Contains(t, content, "# log_level = \"info\"")
	assert.Contains(t, content, "# poll_interval = \"5m\"")

	// Drive section present
	assert.Contains(t, content, `["personal:toni@outlook.com"]`)
	assert.Contains(t, content, `sync_dir = "~/OneDrive"`)
}

func TestCreateConfigWithDrive_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")
	drive, ok := cfg.Drives[cid]
	assert.True(t, ok)
	assert.Equal(t, "~/OneDrive", drive.SyncDir)
}

func TestCreateConfigWithDrive_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/OneDrive - Contoso")
	require.NoError(t, err)

	_, err = os.Stat(path)
	assert.NoError(t, err)
}

func TestCreateConfigWithDrive_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(configFilePermissions), info.Mode().Perm())
}

// --- AppendDriveSection tests ---

func TestAppendDriveSection_AppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/OneDrive - Contoso")
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `["personal:toni@outlook.com"]`)
	assert.Contains(t, content, `["business:alice@contoso.com"]`)
	assert.Contains(t, content, `sync_dir = "~/OneDrive - Contoso"`)
}

func TestAppendDriveSection_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
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
	err := os.WriteFile(path, []byte(`["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"`), configFilePermissions)
	require.NoError(t, err)

	err = AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/Work")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 2)
	assert.Equal(t, "~/Work", cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")].SyncDir)
}

func TestAppendDriveSection_FileNotFound(t *testing.T) {
	err := AppendDriveSection("/nonexistent/config.toml", driveid.MustCanonicalID("personal:test@test.com"), "~/OneDrive")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
}

// --- SetDriveKey tests ---

func TestSetDriveKey_InsertNewKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "alias", "home")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "home", cfg.Drives[cid].Alias)
}

func TestSetDriveKey_UpdateExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	// First set
	err = SetDriveKey(path, cid, "alias", "home")
	require.NoError(t, err)

	// Update
	err = SetDriveKey(path, cid, "alias", "personal")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "personal", cfg.Drives[cid].Alias)
}

func TestSetDriveKey_BooleanFormatting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "enabled", "false")
	require.NoError(t, err)

	// Verify the raw file content has bare false (not "false")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "enabled = false")
	assert.NotContains(t, string(data), `enabled = "false"`)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	require.NotNil(t, d.Enabled)
	assert.False(t, *d.Enabled)
}

func TestSetDriveKey_StringFormatting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "alias", "work")
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `alias = "work"`)
}

func TestSetDriveKey_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:toni@outlook.com")

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "enabled", "true")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	require.NotNil(t, d.Enabled)
	assert.True(t, *d.Enabled)
}

func TestSetDriveKey_SectionNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, driveid.MustCanonicalID("business:nobody@example.com"), "enabled", "false")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSetDriveKey_FileNotFound(t *testing.T) {
	err := SetDriveKey("/nonexistent/config.toml", driveid.MustCanonicalID("personal:test@test.com"), "enabled", "false")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config file")
}

func TestSetDriveKey_MultipleSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	// Set key on the second section only
	err = SetDriveKey(path, businessCID, "enabled", "false")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	// First section should be unaffected
	personal := cfg.Drives[personalCID]
	assert.Nil(t, personal.Enabled) // not set

	// Second section should have enabled = false
	business := cfg.Drives[businessCID]
	require.NotNil(t, business.Enabled)
	assert.False(t, *business.Enabled)
}

// --- DeleteDriveSection tests ---

func TestDeleteDriveSection_DeleteFromMiddle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")
	spCID := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")

	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
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

func TestDeleteDriveSection_DeleteFromEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	// Delete the last section
	err = DeleteDriveSection(path, businessCID)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
	assert.Contains(t, cfg.Drives, personalCID)
}

func TestDeleteDriveSection_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
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

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
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
	result := DefaultSyncDir("personal", "", nil)
	assert.Equal(t, "~/OneDrive", result)
}

func TestDefaultSyncDir_Personal_WithCollision(t *testing.T) {
	existing := []string{"~/OneDrive"}
	result := DefaultSyncDir("personal", "", existing)
	assert.Equal(t, "~/OneDrive - Personal", result)
}

func TestDefaultSyncDir_Business(t *testing.T) {
	result := DefaultSyncDir("business", "Contoso", nil)
	assert.Equal(t, "~/OneDrive - Contoso", result)
}

func TestDefaultSyncDir_Business_NoOrgName(t *testing.T) {
	result := DefaultSyncDir("business", "", nil)
	assert.Equal(t, "~/OneDrive - Business", result)
}

func TestDefaultSyncDir_SharePoint_Empty(t *testing.T) {
	result := DefaultSyncDir("sharepoint", "Contoso", nil)
	assert.Equal(t, "", result)
}

func TestDefaultSyncDir_UnknownType_Empty(t *testing.T) {
	result := DefaultSyncDir("unknown", "", nil)
	assert.Equal(t, "", result)
}

// --- Comment preservation tests ---

func TestCommentPreservation_AppendDriveSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	err := CreateConfigWithDrive(path, driveid.MustCanonicalID("personal:toni@outlook.com"), "~/OneDrive")
	require.NoError(t, err)

	// Add a user comment by directly modifying the file
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	content = strings.Replace(content, `["personal:toni@outlook.com"]`,
		"# My personal drive\n"+`["personal:toni@outlook.com"]`, 1)

	err = os.WriteFile(path, []byte(content), configFilePermissions)
	require.NoError(t, err)

	// Now append a new section
	err = AppendDriveSection(path, driveid.MustCanonicalID("business:alice@contoso.com"), "~/Work")
	require.NoError(t, err)

	result, err := os.ReadFile(path)
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
	err := os.WriteFile(path, []byte(content), configFilePermissions)
	require.NoError(t, err)

	err = SetDriveKey(path, driveid.MustCanonicalID("business:alice@contoso.com"), "enabled", "false")
	require.NoError(t, err)

	result, err := os.ReadFile(path)
	require.NoError(t, err)
	resultStr := string(result)

	assert.Contains(t, resultStr, "# My custom header")
	assert.Contains(t, resultStr, "# Work drive for office stuff")
	assert.Contains(t, resultStr, "enabled = false")
}

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
	err := os.WriteFile(path, []byte(content), configFilePermissions)
	require.NoError(t, err)

	err = DeleteDriveSection(path, driveid.MustCanonicalID("personal:toni@outlook.com"))
	require.NoError(t, err)

	result, err := os.ReadFile(path)
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

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestAtomicWriteFile_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "test.txt")

	err := atomicWriteFile(path, []byte("hello"))
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestAtomicWriteFile_SetsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	err := atomicWriteFile(path, []byte("hello"))
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(configFilePermissions), info.Mode().Perm())
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
	assert.Contains(t, err.Error(), "creating config directory")
}

// --- formatTOMLValue tests ---

func TestFormatTOMLValue_Boolean(t *testing.T) {
	assert.Equal(t, "true", formatTOMLValue("true"))
	assert.Equal(t, "false", formatTOMLValue("false"))
}

func TestFormatTOMLValue_String(t *testing.T) {
	assert.Equal(t, `"hello"`, formatTOMLValue("hello"))
	assert.Equal(t, `"~/OneDrive"`, formatTOMLValue("~/OneDrive"))
}

// --- driveSection tests ---

func TestDriveSection_Format(t *testing.T) {
	result := driveSection("personal:toni@outlook.com", "~/OneDrive")
	assert.Equal(t, "\n[\"personal:toni@outlook.com\"]\nsync_dir = \"~/OneDrive\"\n", result)
}

// --- findSectionHeader tests ---

func TestFindSectionHeader_Found(t *testing.T) {
	lines := []string{
		"# comment",
		`["personal:toni@outlook.com"]`,
		`sync_dir = "~/OneDrive"`,
	}
	headerLine, sectionStart := findSectionHeader(lines, "personal:toni@outlook.com")
	assert.Equal(t, 1, headerLine)
	assert.Equal(t, 2, sectionStart)
}

func TestFindSectionHeader_NotFound(t *testing.T) {
	lines := []string{"# comment", `log_level = "info"`}
	headerLine, sectionStart := findSectionHeader(lines, "personal:toni@outlook.com")
	assert.Equal(t, -1, headerLine)
	assert.Equal(t, -1, sectionStart)
}

// --- findSectionEnd tests ---

func TestFindSectionEnd_NextSection(t *testing.T) {
	lines := []string{
		`["personal:toni@outlook.com"]`,
		`sync_dir = "~/OneDrive"`,
		"",
		`["business:alice@contoso.com"]`,
		`sync_dir = "~/Work"`,
	}
	// Section content ends at line 2 (the blank line before the next header
	// belongs to the next section's preamble).
	end := findSectionEnd(lines, 1)
	assert.Equal(t, 2, end)
}

func TestFindSectionEnd_NextSectionWithComment(t *testing.T) {
	lines := []string{
		`["personal:toni@outlook.com"]`,
		`sync_dir = "~/OneDrive"`,
		"",
		"# Business drive",
		`["business:alice@contoso.com"]`,
		`sync_dir = "~/Work"`,
	}
	// Blank line and comment before next header belong to next section.
	end := findSectionEnd(lines, 1)
	assert.Equal(t, 2, end)
}

func TestFindSectionEnd_EOF(t *testing.T) {
	lines := []string{
		`["personal:toni@outlook.com"]`,
		`sync_dir = "~/OneDrive"`,
	}
	end := findSectionEnd(lines, 1)
	assert.Equal(t, 2, end)
}

// --- Integration scenario tests ---

func TestScenario_FirstLoginThenSecondLogin(t *testing.T) {
	// Simulates: first login creates config, second login appends drive
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	// First login
	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
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
	// Simulates: login, then drive remove (set enabled = false)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("business:alice@contoso.com")

	err := CreateConfigWithDrive(path, cid, "~/Work")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "enabled", "false")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	d := cfg.Drives[cid]
	require.NotNil(t, d.Enabled)
	assert.False(t, *d.Enabled)
	assert.Equal(t, "~/Work", d.SyncDir) // sync_dir unchanged
}

func TestScenario_DriveRemovePurge(t *testing.T) {
	// Simulates: login two drives, purge one
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")

	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
	require.NoError(t, err)

	err = AppendDriveSection(path, businessCID, "~/Work")
	require.NoError(t, err)

	err = DeleteDriveSection(path, businessCID)
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	require.Len(t, cfg.Drives, 1)
	assert.Contains(t, cfg.Drives, personalCID)
}

func TestScenario_LogoutPurge_AllDrives(t *testing.T) {
	// Simulates: logout --purge removes all sections for an account
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	personalCID := driveid.MustCanonicalID("personal:toni@outlook.com")
	businessCID := driveid.MustCanonicalID("business:alice@contoso.com")
	spCID := driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents")

	err := CreateConfigWithDrive(path, personalCID, "~/OneDrive")
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

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "alias", "home")
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

	err := CreateConfigWithDrive(path, cid, "~/OneDrive")
	require.NoError(t, err)

	err = SetDriveKey(path, cid, "sync_dir", "~/NewDrive")
	require.NoError(t, err)

	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, "~/NewDrive", cfg.Drives[cid].SyncDir)
}
