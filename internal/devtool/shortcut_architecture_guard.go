package devtool

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type shortcutArchitectureRule struct {
	Name      string
	Root      string
	FileMatch func(string) bool
	Forbidden []shortcutArchitecturePattern
}

type shortcutArchitecturePattern struct {
	Needle      string
	Description string
	Allow       func(string) bool
}

func RunShortcutArchitectureChecks(repoRoot string) error {
	if strings.TrimSpace(repoRoot) == "" {
		return fmt.Errorf("repo root is required")
	}
	var errs []error
	for _, rule := range shortcutArchitectureRules() {
		if err := runShortcutArchitectureRule(repoRoot, rule); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func shortcutArchitectureRules() []shortcutArchitectureRule {
	return []shortcutArchitectureRule{
		shortcutProductionMultisyncRule(),
		shortcutMultisyncTestRule(),
		shortcutCLIStatusRule(),
		shortcutDeletedConceptRule(),
	}
}

func shortcutProductionMultisyncRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "production multisync shortcut authority",
		Root: "internal/multisync",
		FileMatch: func(path string) bool {
			return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: "github.com/tonimelisma/onedrive-go/internal/graph", Description: "multisync must not import Graph"},
			{Needle: "config.DefaultDataDir()", Description: "multisync cleanup must receive an explicit data dir"},
			{Needle: "config.MountStatePath(", Description: "multisync child state paths must use the explicit orchestrator data dir"},
			{Needle: "ShortcutRootRecord", Description: "multisync must not read parent shortcut-root records"},
			{Needle: "ShortcutRootState", Description: "multisync must not branch on parent shortcut-root states"},
			{Needle: ".ProtectedPaths", Description: "multisync must not branch on parent status-only protected paths"},
			{Needle: ".BlockedDetail", Description: "multisync must not branch on parent status-only blocked details"},
			{Needle: ".Waiting", Description: "multisync must not branch on parent waiting replacement state", Allow: allowWaitingReplacementView},
			{Needle: "applyShortcutAliasMutation", Description: "multisync must not call parent alias mutation"},
			{Needle: "shortcutAliasMutation", Description: "multisync must not know parent alias mutation types"},
			{Needle: "StandaloneContentRootWins", Description: "multisync must not own remote-content-root conflict policy"},
			{Needle: "content root already projected", Description: "multisync must not suppress automatic children by remote content root"},
		},
	}
}

func shortcutMultisyncTestRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "multisync shortcut tests use child process snapshots",
		Root: "internal/multisync",
		FileMatch: func(path string) bool {
			return strings.HasSuffix(path, "_test.go")
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: "syncengine.ShortcutRootRecord", Description: "multisync tests must build child process snapshots, not parent root records"},
			{Needle: "syncengine.ShortcutRootState", Description: "multisync tests must not construct parent root states"},
			{Needle: ".ProtectedPaths", Description: "multisync tests must not assert parent protected path internals"},
			{Needle: ".BlockedDetail", Description: "multisync tests must not assert parent blocked detail internals"},
			{
				Needle:      ".Waiting",
				Description: "multisync tests must not assert parent waiting replacement internals",
				Allow:       allowWaitingReplacementView,
			},
		},
	}
}

func shortcutCLIStatusRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "cli shortcut status view boundary",
		Root: "internal/cli",
		FileMatch: func(path string) bool {
			base := filepath.Base(path)
			return strings.HasSuffix(base, ".go") &&
				!strings.HasSuffix(base, "_test.go") &&
				strings.HasPrefix(base, "status")
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: "ShortcutRootRecord", Description: "CLI status must consume ShortcutRootStatusView, not raw root records"},
			{Needle: "ShortcutRootState", Description: "CLI status must not derive lifecycle policy from raw states"},
			{Needle: ".ProtectedPaths", Description: "CLI status must not read raw protected paths"},
			{Needle: ".BlockedDetail", Description: "CLI status must not read raw blocked details"},
			{Needle: ".Waiting", Description: "CLI status must not read raw waiting replacement state", Allow: allowWaitingReplacementView},
		},
	}
}

func shortcutDeletedConceptRule() shortcutArchitectureRule {
	return shortcutArchitectureRule{
		Name: "shortcut deleted concept drift",
		Root: ".",
		FileMatch: func(path string) bool {
			if path == filepath.Join("internal", "devtool", "shortcut_architecture_guard.go") ||
				path == filepath.Join("internal", "devtool", "shortcut_architecture_guard_test.go") {
				return false
			}
			if strings.HasPrefix(path, ".git"+string(filepath.Separator)) {
				return false
			}
			if strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".yml") {
				return true
			}
			return filepath.Base(path) == "AGENTS.md" || filepath.Base(path) == "CLAUDE.md"
		},
		Forbidden: []shortcutArchitecturePattern{
			{Needle: "preflight parent", Description: "use normal parent child-process snapshot terminology"},
			{Needle: "prepared parent", Description: "use normal parent child-process snapshot terminology"},
			{Needle: "shortcut bootstrap", Description: "shortcut lifecycle must use normal parent run/bootstrap vocabulary"},
			{Needle: "publishParentStartupChildTopology", Description: "startup topology publisher was removed"},
			{Needle: "PublishInitialChildTopology", Description: "startup topology publisher was removed"},
			{Needle: "StandaloneContentRootWins", Description: "standalone-vs-automatic remote-content conflict policy was removed"},
			{
				Needle:      "content root already projected by standalone mount",
				Description: "standalone-vs-automatic remote-content conflict policy was removed",
			},
			{Needle: "localReservations", Description: "parent-owned protected roots replaced multisync reservations"},
			{Needle: "localSkipDirs", Description: "parent-owned protected roots replaced multisync skip dirs"},
			{Needle: "live shortcut delete E2E", Description: "live shortcut delete E2E is out of scope without Microsoft support"},
			{Needle: "shortcut delete E2E", Description: "live shortcut delete E2E is out of scope without Microsoft support"},
		},
	}
}

func allowWaitingReplacementView(line string) bool {
	return strings.Contains(line, "WaitingReplacement")
}

func runShortcutArchitectureRule(repoRoot string, rule shortcutArchitectureRule) error {
	root := filepath.Join(repoRoot, rule.Root)
	if _, err := stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", path, walkErr)
		}
		if entry.IsDir() {
			if shouldSkipShortcutArchitectureDir(entry) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		if !rule.FileMatch(rel) {
			return nil
		}
		if err := checkShortcutArchitectureFile(rule, path, rel); err != nil {
			errs = append(errs, err)
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func shouldSkipShortcutArchitectureDir(entry fs.DirEntry) bool {
	switch entry.Name() {
	case ".git", ".hg", ".svn", ".jj":
		return true
	default:
		return false
	}
}

func checkShortcutArchitectureFile(rule shortcutArchitectureRule, path string, rel string) error {
	data, err := readFile(path)
	if err != nil {
		return err
	}
	var errs []error
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		for _, pattern := range rule.Forbidden {
			if !strings.Contains(line, pattern.Needle) {
				continue
			}
			if pattern.Allow != nil && pattern.Allow(line) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s:%d: %s: %s", rel, lineNo, rule.Name, pattern.Description))
		}
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Errorf("scan %s: %w", rel, err))
	}
	return errors.Join(errs...)
}
