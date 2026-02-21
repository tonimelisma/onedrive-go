package sync

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// newTestFilter creates a FilterEngine with a test logger writing to t.Log.
func newTestFilter(t *testing.T, cfg config.FilterConfig, syncRoot string) *FilterEngine {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fe, err := NewFilterEngine(&cfg, syncRoot, logger)
	require.NoError(t, err)

	return fe
}

// --- Layer 1: sync_paths ---

func TestFilterEngine_SyncPaths_Included(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SyncPaths: []string{"docs", "src/main"},
	}, "/tmp/sync")

	tests := []struct {
		name  string
		path  string
		isDir bool
	}{
		{"exact match", "docs", true},
		{"child of sync path", "docs/readme.md", false},
		{"nested child", "docs/api/v1/spec.yaml", false},
		{"second sync path", "src/main", true},
		{"child of second", "src/main/app.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, 0)
			assert.True(t, result.Included, "path %q should be included", tt.path)
		})
	}
}

func TestFilterEngine_SyncPaths_Excluded(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SyncPaths: []string{"docs", "src/main"},
	}, "/tmp/sync")

	tests := []struct {
		name  string
		path  string
		isDir bool
	}{
		{"unrelated path", "build/output.bin", false},
		{"sibling dir", "src/test", true},
		{"partial prefix match", "documents/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, 0)
			assert.False(t, result.Included)
			assert.Equal(t, "not in sync_paths", result.Reason)
		})
	}
}

func TestFilterEngine_SyncPaths_ParentTraversable(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SyncPaths: []string{"a/b/c"},
	}, "/tmp/sync")

	// Parent directories of sync paths must be traversable (for scanner to recurse).
	result := fe.ShouldSync("a", true, 0)
	assert.True(t, result.Included, "parent dir should be traversable")

	result = fe.ShouldSync("a/b", true, 0)
	assert.True(t, result.Included, "nested parent dir should be traversable")

	// But a file in the parent is NOT included.
	result = fe.ShouldSync("a/file.txt", false, 0)
	assert.False(t, result.Included, "file in parent dir should be excluded")
}

func TestFilterEngine_SyncPaths_Empty(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// No sync_paths means everything passes Layer 1.
	result := fe.ShouldSync("anything/at/all.txt", false, 0)
	assert.True(t, result.Included)
}

// --- Layer 2: Config patterns ---

func TestFilterEngine_SkipFiles(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SkipFiles: []string{"*.log", "*.bak", "thumbs.db"},
	}, "/tmp/sync")

	tests := []struct {
		name     string
		path     string
		included bool
	}{
		{"log file excluded", "app.log", false},
		{"bak file excluded", "data.bak", false},
		{"thumbs.db excluded", "thumbs.db", false},
		{"normal file included", "readme.md", true},
		{"nested log excluded", "logs/app.log", false},
		{"case insensitive", "APP.LOG", false},
		{"case insensitive db", "Thumbs.DB", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, false, 0)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
		})
	}
}

func TestFilterEngine_SkipDirs(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SkipDirs: []string{"node_modules", ".git", "vendor"},
	}, "/tmp/sync")

	tests := []struct {
		name     string
		path     string
		included bool
	}{
		{"node_modules excluded", "node_modules", false},
		{"nested node_modules excluded", "project/node_modules", false},
		{".git excluded", ".git", false},
		{"vendor excluded", "vendor", false},
		{"normal dir included", "src", true},
		{"case insensitive", "Node_Modules", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, true, 0)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
		})
	}
}

func TestFilterEngine_SkipDirs_NotAppliedToFiles(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SkipDirs: []string{"vendor"},
	}, "/tmp/sync")

	// A file named "vendor" should NOT be excluded by skip_dirs.
	result := fe.ShouldSync("vendor", false, 0)
	assert.True(t, result.Included)
}

func TestFilterEngine_SkipDotfiles(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SkipDotfiles: true,
	}, "/tmp/sync")

	tests := []struct {
		name     string
		path     string
		isDir    bool
		included bool
	}{
		{"dotfile excluded", ".bashrc", false, false},
		{"dotdir excluded", ".config", true, false},
		{"nested dotfile excluded", "home/.bashrc", false, false},
		{"nested dotdir excluded", "home/.config", true, false},
		{"normal file included", "readme.md", false, true},
		{"normal dir included", "src", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, 0)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
			if !tt.included {
				assert.Equal(t, "dotfile excluded", result.Reason)
			}
		})
	}
}

func TestFilterEngine_SkipDotfiles_Disabled(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SkipDotfiles: false,
	}, "/tmp/sync")

	result := fe.ShouldSync(".bashrc", false, 0)
	assert.True(t, result.Included)
}

func TestFilterEngine_MaxFileSize(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		MaxFileSize: "100MB",
	}, "/tmp/sync")

	tests := []struct {
		name     string
		size     int64
		included bool
	}{
		{"under limit", 50_000_000, true},
		{"at limit", 100_000_000, true},
		{"over limit", 100_000_001, false},
		{"way over limit", 1_000_000_000, false},
		{"zero size", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync("file.bin", false, tt.size)
			assert.Equal(t, tt.included, result.Included, "size %d", tt.size)
		})
	}
}

func TestFilterEngine_MaxFileSize_Zero(t *testing.T) {
	t.Parallel()

	// MaxFileSize "0" means no limit.
	fe := newTestFilter(t, config.FilterConfig{
		MaxFileSize: "0",
	}, "/tmp/sync")

	result := fe.ShouldSync("huge.bin", false, 999_999_999_999)
	assert.True(t, result.Included)
}

func TestFilterEngine_MaxFileSize_Empty(t *testing.T) {
	t.Parallel()

	// Empty MaxFileSize means no limit.
	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	result := fe.ShouldSync("huge.bin", false, 999_999_999_999)
	assert.True(t, result.Included)
}

func TestFilterEngine_MaxFileSize_NotAppliedToDirs(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		MaxFileSize: "1KB",
	}, "/tmp/sync")

	// Directories are never filtered by size.
	result := fe.ShouldSync("big_dir", true, 999_999_999)
	assert.True(t, result.Included)
}

// --- S7 Safety Patterns ---

func TestFilterEngine_S7SafetyPatterns(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	tests := []struct {
		name     string
		path     string
		included bool
		reason   string
	}{
		{"partial file excluded", "download.partial", false, "S7 safety: matches .partial pattern"},
		{"tmp file excluded", "data.tmp", false, "S7 safety: matches .tmp pattern"},
		{"tilde file excluded", "~lockfile", false, "S7 safety: matches ~* pattern"},
		{"tilde dollar excluded", "~$document.docx", false, "name starts with ~$"},
		{"uppercase partial", "FILE.PARTIAL", false, "S7 safety: matches .partial pattern"},
		{"uppercase tmp", "FILE.TMP", false, "S7 safety: matches .tmp pattern"},
		{"normal file included", "document.docx", true, ""},
		{"partial in dir name ok for files", "partial/file.txt", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, false, 0)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
			if !tt.included {
				assert.Equal(t, tt.reason, result.Reason)
			}
		})
	}
}

func TestFilterEngine_S7SafetyPatterns_NotAppliedToDirs(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// S7 patterns only apply to files, not directories.
	result := fe.ShouldSync("temp.tmp", true, 0)
	assert.True(t, result.Included)
}

// --- Layer 3: .odignore ---

func TestFilterEngine_Odignore(t *testing.T) {
	t.Parallel()

	// Create a temp directory with a .odignore file.
	syncRoot := t.TempDir()
	odignoreContent := "*.secret\nbuild/\n!important.secret\n"
	err := os.WriteFile(filepath.Join(syncRoot, ".odignore"), []byte(odignoreContent), 0o644)
	require.NoError(t, err)

	fe := newTestFilter(t, config.FilterConfig{
		IgnoreMarker: ".odignore",
	}, syncRoot)

	tests := []struct {
		name     string
		path     string
		isDir    bool
		included bool
	}{
		{"secret file excluded", "passwords.secret", false, false},
		{"build dir excluded", "build", true, false},
		{"normal file included", "readme.md", false, true},
		{"negation pattern", "important.secret", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, 0)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
		})
	}
}

func TestFilterEngine_Odignore_Missing(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	// No .odignore file created.

	fe := newTestFilter(t, config.FilterConfig{
		IgnoreMarker: ".odignore",
	}, syncRoot)

	result := fe.ShouldSync("anything.secret", false, 0)
	assert.True(t, result.Included, "without .odignore, nothing should be excluded by layer 3")
}

func TestFilterEngine_Odignore_EmptyMarker(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		IgnoreMarker: "",
	}, "/tmp/sync")

	// Empty ignore_marker disables layer 3.
	result := fe.ShouldSync("anything.secret", false, 0)
	assert.True(t, result.Included)
}

func TestFilterEngine_Odignore_Caching(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	err := os.WriteFile(filepath.Join(syncRoot, ".odignore"), []byte("*.log\n"), 0o644)
	require.NoError(t, err)

	fe := newTestFilter(t, config.FilterConfig{
		IgnoreMarker: ".odignore",
	}, syncRoot)

	// First call loads from disk.
	result1 := fe.ShouldSync("app.log", false, 0)
	assert.False(t, result1.Included)

	// Second call should use cache (verified by checking cache map).
	result2 := fe.ShouldSync("server.log", false, 0)
	assert.False(t, result2.Included)

	fe.mu.RLock()
	_, cached := fe.odignoreCache["."]
	fe.mu.RUnlock()
	assert.True(t, cached, "odignore should be cached after first load")
}

// --- OneDrive Name Validation ---

func TestFilterEngine_OneDriveNameValidation(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	tests := []struct {
		name     string
		path     string
		isDir    bool
		included bool
		reason   string
	}{
		{"normal file", "readme.md", false, true, ""},
		{"normal dir", "src", true, true, ""},
		{"illegal star", "file*.txt", false, false, `contains illegal character "*"`},
		{"illegal colon", "file:.txt", false, false, `contains illegal character ":"`},
		{"illegal question", "file?.txt", false, false, `contains illegal character "?"`},
		{"illegal less-than", "file<.txt", false, false, `contains illegal character "<"`},
		{"illegal greater-than", "file>.txt", false, false, `contains illegal character ">"`},
		{"illegal pipe", "file|.txt", false, false, `contains illegal character "|"`},
		{"illegal quote", `file".txt`, false, false, `contains illegal character "\""`},
		{"illegal backslash", `file\.txt`, false, false, `contains illegal character "\\"`},
		{"reserved CON", "CON", false, false, `"CON" is a reserved name`},
		{"reserved con lowercase", "con", false, false, `"con" is a reserved name`},
		{"reserved PRN", "PRN.txt", false, false, `"PRN.txt" is a reserved name`},
		{"reserved AUX", "AUX", false, false, `"AUX" is a reserved name`},
		{"reserved NUL", "NUL", false, false, `"NUL" is a reserved name`},
		{"reserved COM0", "COM0", false, false, `"COM0" is a reserved name`},
		{"reserved COM9", "COM9", false, false, `"COM9" is a reserved name`},
		{"reserved LPT0", "LPT0", false, false, `"LPT0" is a reserved name`},
		{"reserved LPT9", "LPT9", false, false, `"LPT9" is a reserved name`},
		{"trailing dot", "file.", false, false, "name ends with a dot"},
		{"trailing space", "file ", false, false, "name ends with a space"},
		{"leading space", " file", false, false, "name starts with a space"},
		{"tilde dollar", "~$lock.docx", false, false, "name starts with ~$"},
		{"contains vti", "dir_vti_stuff", true, false, "name contains _vti_"},
		{"not reserved COMPUTER", "COMPUTER", false, true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, 0)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
			if !tt.included {
				assert.Equal(t, tt.reason, result.Reason)
			}
		})
	}
}

func TestFilterEngine_PathTooLong(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// Build a path that exceeds 400 characters using short components
	// to avoid hitting the 255-byte component limit first.
	// "aa/" repeated 134 times = 402 chars, then trim to 401.
	longPath := strings.TrimSuffix(strings.Repeat("aa/", 134), "/")

	result := fe.ShouldSync(longPath, false, 0)
	assert.False(t, result.Included)
	assert.Contains(t, result.Reason, "path exceeds 400 characters")
}

func TestFilterEngine_PathExactlyAtLimit(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// Build a path of exactly 400 characters using short components.
	// "a/" * 199 + "aa" = 199*2 + 2 = 400 chars, each component <= 2 bytes.
	exactPath := strings.Repeat("a/", 199) + "aa"

	result := fe.ShouldSync(exactPath, false, 0)
	assert.True(t, result.Included)
}

func TestFilterEngine_ComponentTooLong(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// A component of 256 bytes exceeds the 255-byte limit.
	longName := strings.Repeat("x", 256)
	path := "dir/" + longName

	result := fe.ShouldSync(path, false, 0)
	assert.False(t, result.Included)
	assert.Contains(t, result.Reason, "name exceeds 255 bytes")
}

// --- Empty config ---

func TestFilterEngine_EmptyConfig(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// With empty config, everything should be included (except name validation violations).
	tests := []struct {
		name  string
		path  string
		isDir bool
		size  int64
	}{
		{"file", "readme.md", false, 100},
		{"dir", "src", true, 0},
		{"dotfile", ".bashrc", false, 50},
		{"dotdir", ".config", true, 0},
		{"large file", "big.bin", false, 999_999_999_999},
		{"nested path", "a/b/c/d/e.txt", false, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, tt.size)
			assert.True(t, result.Included, "path %q should be included with empty config", tt.path)
		})
	}
}

// --- Edge cases ---

func TestFilterEngine_RootPath(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	// Root directory path "." should be included.
	result := fe.ShouldSync(".", true, 0)
	assert.True(t, result.Included)
}

func TestFilterEngine_DeeplyNestedPath(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{}, "/tmp/sync")

	path := "a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z.txt"
	result := fe.ShouldSync(path, false, 0)
	assert.True(t, result.Included)
}

func TestFilterEngine_DirectoryVsFile(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SkipFiles: []string{"vendor"},
		SkipDirs:  []string{"logs"},
	}, "/tmp/sync")

	// "vendor" as a file should match skip_files but NOT skip_dirs.
	fileResult := fe.ShouldSync("vendor", false, 0)
	assert.False(t, fileResult.Included)
	assert.Equal(t, "matches skip_files pattern", fileResult.Reason)

	// "vendor" as a dir should NOT match skip_dirs (it's not in skip_dirs).
	dirResult := fe.ShouldSync("vendor", true, 0)
	assert.True(t, dirResult.Included)

	// "logs" as a dir should match skip_dirs.
	logsDirResult := fe.ShouldSync("logs", true, 0)
	assert.False(t, logsDirResult.Included)
	assert.Equal(t, "matches skip_dirs pattern", logsDirResult.Reason)

	// "logs" as a file should NOT match skip_files (it's not in skip_files).
	logsFileResult := fe.ShouldSync("logs", false, 0)
	assert.True(t, logsFileResult.Included)
}

// --- Constructor error ---

func TestNewFilterEngine_InvalidMaxFileSize(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, err := NewFilterEngine(&config.FilterConfig{
		MaxFileSize: "not-a-size",
	}, "/tmp/sync", logger)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid max_file_size")
}

// --- Size parsing (config.ParseSize integration) ---

func TestParseSizeFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"100", 100, false},
		{"1KB", 1000, false},
		{"1MB", 1_000_000, false},
		{"1GB", 1_000_000_000, false},
		{"1TB", 1_000_000_000_000, false},
		{"1KiB", 1024, false},
		{"1MiB", 1_048_576, false},
		{"1GiB", 1_073_741_824, false},
		{"50GB", 50_000_000_000, false},
		{"100mb", 100_000_000, false},
		{"1B", 1, false},
		{"invalid", 0, true},
		{"-1", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result, err := config.ParseSize(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

// --- matchesSkipPattern ---

func TestMatchesSkipPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		patterns []string
		expected bool
	}{
		{"star glob", "file.log", []string{"*.log"}, true},
		{"no match", "file.txt", []string{"*.log"}, false},
		{"exact match", "thumbs.db", []string{"thumbs.db"}, true},
		{"case insensitive", "FILE.LOG", []string{"*.log"}, true},
		{"multiple patterns", "data.bak", []string{"*.log", "*.bak"}, true},
		{"empty patterns", "file.txt", []string{}, false},
		{"question mark glob", "file1.txt", []string{"file?.txt"}, true},
		{"malformed pattern handled", "file.txt", []string{"[invalid"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, matchesSkipPattern(tt.filename, tt.patterns))
		})
	}
}

// --- isValidOneDriveName ---

func TestIsValidOneDriveName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		valid   bool
		wantMsg string
	}{
		{"valid name", "readme.md", true, ""},
		{"valid with spaces", "my file.txt", true, ""},
		{"valid with dots", "file.tar.gz", true, ""},
		{"star char", "f*le.txt", false, "illegal character"},
		{"colon char", "f:le.txt", false, "illegal character"},
		{"trailing dot", "file.", false, "ends with a dot"},
		{"trailing space", "file ", false, "ends with a space"},
		{"leading space", " file", false, "starts with a space"},
		{"reserved CON", "CON", false, "reserved name"},
		{"reserved con lower", "con", false, "reserved name"},
		{"reserved NUL with ext", "NUL.txt", false, "reserved name"},
		{"tilde dollar", "~$temp", false, "starts with ~$"},
		{"vti pattern", "dir_vti_test", false, "contains _vti_"},
		{"too long name", strings.Repeat("a", 256), false, "exceeds 255 bytes"},
		{"exactly 255", strings.Repeat("a", 255), true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			valid, reason := isValidOneDriveName(tt.input)
			assert.Equal(t, tt.valid, valid)
			if !tt.valid {
				assert.Contains(t, reason, tt.wantMsg)
			}
		})
	}
}

// --- isValidPath ---

func TestIsValidPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		path  string
		valid bool
	}{
		{"short path", "a/b/c.txt", true},
		{"exactly 400", strings.Repeat("a", 400), true},
		{"401 chars", strings.Repeat("a", 401), false},
		{"empty path", "", true},
		{"unicode path", strings.Repeat("ñ", 400), true},
		{"unicode over limit", strings.Repeat("ñ", 401), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			valid, _ := isValidPath(tt.path)
			assert.Equal(t, tt.valid, valid)
		})
	}
}

// --- Combined filter layers ---

func TestFilterEngine_CombinedLayers(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	err := os.WriteFile(filepath.Join(syncRoot, ".odignore"), []byte("secret/\n"), 0o644)
	require.NoError(t, err)

	fe := newTestFilter(t, config.FilterConfig{
		SyncPaths:    []string{"project"},
		SkipFiles:    []string{"*.log"},
		SkipDirs:     []string{"node_modules"},
		SkipDotfiles: true,
		MaxFileSize:  "10MB",
		IgnoreMarker: ".odignore",
	}, syncRoot)

	tests := []struct {
		name     string
		path     string
		isDir    bool
		size     int64
		included bool
		reason   string
	}{
		// Layer 1 exclusions
		{"outside sync path", "other/file.txt", false, 0, false, "not in sync_paths"},
		// Layer 2 exclusions
		{"log file", "project/app.log", false, 0, false, "matches skip_files pattern"},
		{"node_modules dir", "project/node_modules", true, 0, false, "matches skip_dirs pattern"},
		{"dotfile", "project/.env", false, 0, false, "dotfile excluded"},
		{"large file", "project/big.bin", false, 20_000_000, false, "exceeds max_file_size"},
		// Passes all layers
		{"good file", "project/main.go", false, 1000, true, ""},
		{"good dir", "project/src", true, 0, true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := fe.ShouldSync(tt.path, tt.isDir, tt.size)
			assert.Equal(t, tt.included, result.Included, "path %q", tt.path)
			if !tt.included {
				assert.Equal(t, tt.reason, result.Reason)
			}
		})
	}
}

// --- matchesSyncPaths edge cases ---

func TestFilterEngine_MatchesSyncPaths_Normalization(t *testing.T) {
	t.Parallel()

	fe := newTestFilter(t, config.FilterConfig{
		SyncPaths: []string{"docs/api"},
	}, "/tmp/sync")

	// Path with redundant elements should still match after cleaning.
	result := fe.ShouldSync("docs/api/spec.yaml", false, 0)
	assert.True(t, result.Included)

	// Prefix that doesn't end at a boundary should NOT match.
	result = fe.ShouldSync("docs/api-v2/spec.yaml", false, 0)
	assert.False(t, result.Included)
}

// --- Odignore in subdirectory ---

func TestFilterEngine_Odignore_Subdirectory(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()

	// Create a subdirectory with its own .odignore.
	subDir := filepath.Join(syncRoot, "subdir")
	err := os.MkdirAll(subDir, 0o755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(subDir, ".odignore"), []byte("*.generated\n"), 0o644)
	require.NoError(t, err)

	fe := newTestFilter(t, config.FilterConfig{
		IgnoreMarker: ".odignore",
	}, syncRoot)

	// File in root is not affected by subdir's .odignore.
	result := fe.ShouldSync("code.generated", false, 0)
	assert.True(t, result.Included, "root should not be affected by subdir .odignore")

	// File in subdir should be excluded.
	result = fe.ShouldSync("subdir/code.generated", false, 0)
	assert.False(t, result.Included, "subdir file should be excluded by subdir .odignore")
}
