package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func expectedGlobalSchemaKeys() []string {
	return []string{
		"check_workers",
		"dry_run",
		"log_file",
		"log_format",
		"log_level",
		"log_retention_days",
		"min_free_space",
		"poll_interval",
		"transfer_workers",
		"websocket",
	}
}

func expectedDriveSchemaKeys() []string {
	return []string{
		"display_name",
		"owner",
		"paused",
		"paused_until",
		"sync_dir",
	}
}

func bannedSurfaceTerms() []string {
	return []string{
		term("shut", "down", "_", "time", "out"),
		term("con", "flict", "_", "stra", "tegy"),
		term("sync", "_", "dir", "_", "permissions"),
		term("sync", "_", "file", "_", "permissions"),
		term("para", "llel", "_", "down", "loads"),
		term("para", "llel", "_", "up", "loads"),
		term("para", "llel", "_", "check", "ers"),
		term("band", "width", "_", "limit"),
		term("band", "width", "_", "schedule"),
		term("R-", "5.4.1"),
		term("R-", "5.4.2"),
		term("R-", "6.2.9"),
		term("Graceful shutdown has", " ", "a deadline."),
	}
}

func term(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part)
	}

	return b.String()
}

func sortedKeys(keys map[string]struct{}) []string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

func taggedKeysFromType(t reflect.Type) map[string]struct{} {
	keys := make(map[string]struct{})

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous {
			for key := range taggedKeysFromType(field.Type) {
				keys[key] = struct{}{}
			}

			continue
		}

		tag := strings.Split(field.Tag.Get("toml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}

		keys[tag] = struct{}{}
	}

	return keys
}

func knownKeysSet(known map[string]bool) map[string]struct{} {
	keys := make(map[string]struct{}, len(known))
	for key := range known {
		keys[key] = struct{}{}
	}

	return keys
}

func repoRootFromConfigPackage(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func templateCommentedKeys(template string) []string {
	re := regexp.MustCompile(`(?m)^# ([a-z0-9_]+) =`)
	matches := re.FindAllStringSubmatch(template, -1)
	keys := make([]string, 0, len(matches))

	for _, match := range matches {
		keys = append(keys, match[1])
	}

	slices.Sort(keys)

	return keys
}

func docFieldKeys(t *testing.T, header string) []string {
	t.Helper()

	docPath := filepath.Join(repoRootFromConfigPackage(t), "spec", "design", "config.md")
	//nolint:gosec // test intentionally reads a repo-owned doc path
	data, err := os.ReadFile(docPath)
	require.NoError(t, err)

	lines := strings.Split(string(data), "\n")
	var keys []string
	inSection := false

	for _, line := range lines {
		switch {
		case strings.TrimSpace(line) == header:
			inSection = true
		case inSection && strings.HasPrefix(line, "### "):
			inSection = false
		case inSection && strings.HasPrefix(line, "| `"):
			parts := strings.Split(line, "|")
			require.GreaterOrEqual(t, len(parts), 3, "unexpected field-reference row: %q", line)
			key := strings.TrimSpace(parts[1])
			keys = append(keys, strings.Trim(key, "`"))
		}
	}

	slices.Sort(keys)

	return keys
}

func repoFilesContainBannedTerms(t *testing.T, root string, banned []string) []string {
	t.Helper()

	var hits []string
	roots := []string{
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(root, "CLAUDE.md"),
		filepath.Join(root, "internal"),
		filepath.Join(root, "spec"),
	}

	for _, start := range roots {
		info, err := os.Stat(start)
		require.NoError(t, err)

		if !info.IsDir() {
			//nolint:gosec // test intentionally reads repo-owned files
			data, readErr := os.ReadFile(start)
			require.NoError(t, readErr)
			content := string(data)
			for _, term := range banned {
				if strings.Contains(content, term) {
					hits = append(hits, start+": "+term)
				}
			}

			continue
		}

		err = filepath.WalkDir(start, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".jpeg") || strings.HasSuffix(path, ".gif") {
				return nil
			}

			//nolint:gosec // test intentionally reads repo-owned files
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("read repo file %s: %w", path, readErr)
			}

			content := string(data)
			for _, term := range banned {
				if strings.Contains(content, term) {
					hits = append(hits, path+": "+term)
				}
			}

			return nil
		})
		require.NoError(t, err)
	}

	slices.Sort(hits)

	return hits
}

func TestConfigSchema_GlobalKeysMatchExpected(t *testing.T) {
	t.Parallel()

	assert.Equal(t, expectedGlobalSchemaKeys(), sortedKeys(taggedKeysFromType(reflect.TypeOf(Config{}))))
	assert.Equal(t, expectedGlobalSchemaKeys(), sortedKeys(knownKeysSet(newKnownGlobalKeys())))
}

func TestConfigSchema_DriveKeysMatchExpected(t *testing.T) {
	t.Parallel()

	assert.Equal(t, expectedDriveSchemaKeys(), sortedKeys(taggedKeysFromType(reflect.TypeOf(Drive{}))))
	assert.Equal(t, expectedDriveSchemaKeys(), sortedKeys(knownKeysSet(newKnownDriveKeys())))
}

// Validates: R-4.2.1, R-4.9.4
func TestDefaultConfigTemplate_ExactGlobalKeySet(t *testing.T) {
	t.Parallel()

	assert.Equal(t, expectedGlobalSchemaKeys(), templateCommentedKeys(defaultConfigTemplate()))
}

// Validates: R-4.9.4
func TestConfigDesignFieldReference_ExactKeySets(t *testing.T) {
	t.Parallel()

	assert.Equal(t, expectedGlobalSchemaKeys(), docFieldKeys(t, "### Global Fields"))
	assert.Equal(t, expectedDriveSchemaKeys(), docFieldKeys(t, "### Per-Drive Fields"))
}

func TestRemovedConfigAndRoadmapTermsAbsentFromRepo(t *testing.T) {
	t.Parallel()

	root := repoRootFromConfigPackage(t)
	hits := repoFilesContainBannedTerms(t, root, bannedSurfaceTerms())

	assert.Empty(t, hits, "repo still contains removed config or roadmap surface:\n%s", strings.Join(hits, "\n"))
}
