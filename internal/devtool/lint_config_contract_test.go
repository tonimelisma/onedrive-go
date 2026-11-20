package devtool

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The messages of the forbidigo rules the tests below pin. golangci-lint
// matches an exclusion's text against the issue, which quotes the message.
const (
	rawOSLintMessage       = "use fsroot, synctree, or localpath filesystem boundaries instead of raw os.* calls"
	sleepLintMessage       = "use explicit test synchronization instead of wall-clock sleep"
	testFailureLintMessage = "use testify assert or require instead of direct testing failures"
	slogLintMessage        = "pass an injected *slog.Logger instead of the package-level slog logger"
	stdStreamLintMessage   = "write through the CLI-owned output and status writers"
	logPrintLintMessage    = "use the CLI-owned logger"
)

// Validates: R-6.10.17
//
// A test fails through testify, never through the testing package's own
// failure methods. With analyze-types, forbidigo matches a method call by its
// receiver's type, package name first (t.Fatal on a *testing.T reads
// testing.T.Fatal), so the rule holds for a *testing.T kept in a struct field
// or passed as testing.TB, whatever the variable is called, and in the
// non-test files of testutil; no exclusion exempts any file from it, whatever
// path the exclusion names.
func TestGolangciConfig_ForbidigoRefusesDirectTestingFailures(t *testing.T) {
	t.Parallel()

	config := golangciConfig(t)
	assertLintReadsEveryFile(t, config, "forbidigo")
	assert.Equal(t, "true", yamlValue(t, config, "linters", "settings", "forbidigo", "analyze-types"))

	rule := forbidigoRule(t, config, testFailureLintMessage)
	assert.Equal(t, `^testing\.(T|B|F|TB)\.(Fatal|Fatalf|Error|Errorf)$`, rule["pattern"])
	assert.Equal(t, `^testing$`, rule["pkg"])

	pattern := regexp.MustCompile(rule["pattern"])
	for _, method := range []string{"testing.T.Fatal", "testing.TB.Errorf", "testing.B.Fatalf", "testing.F.Error"} {
		assert.True(t, pattern.MatchString(method), method)
	}
	for _, method := range []string{"testing.T.Log", "testing.T.Skipf", "testing.TB.Helper"} {
		assert.False(t, pattern.MatchString(method), method)
	}

	assert.Empty(t, lintExclusionsAdmitting(t, config, "forbidigo", forbidigoIssueText("t.Fatal", testFailureLintMessage)),
		"no exclusion exempts any file from the testing rule")
}

// Validates: R-6.10.17
//
// A test waits for a readiness signal, an injected clock or an observed
// state, never for the wall clock: forbidigo refuses time.Sleep in every test
// file, the filesystem packages' included. Product code may sleep (a retry's
// backoff): the one exclusion the rule has exempts every file but a test. The
// one live-suite helper that waits for the provider carries its own nolint.
func TestGolangciConfig_ForbidigoRefusesWallClockSleepInTests(t *testing.T) {
	t.Parallel()

	config := golangciConfig(t)
	assertLintReadsEveryFile(t, config, "forbidigo")
	assert.Equal(t, `^time\.Sleep$`, forbidigoRule(t, config, sleepLintMessage)["pattern"])

	exemptions := lintExclusionsAdmitting(t, config, "forbidigo", forbidigoIssueText("time.Sleep", sleepLintMessage))
	require.Len(t, exemptions, 1, "one exclusion lets product code sleep")
	assert.Equal(t, `_test\.go`, exemptions[0]["path-except"], "the exclusion exempts every file but a test")
	assert.NotContains(t, exemptions[0], "path", "the exclusion exempts every file but a test")
	for _, path := range []string{"internal/sync/engine_test.go", "e2e/sync_e2e_test.go", "internal/synctree/root_test.go"} {
		assert.False(t, forbidigoIssueExempt(t, config, path, "time.Sleep", sleepLintMessage), path)
	}
	assert.True(t, forbidigoIssueExempt(t, config, "internal/retry/transport.go", "time.Sleep", sleepLintMessage))
}

// Validates: R-6.10.5
//
// Product code prints through the writers internal/cli/root.go and format.go
// own and logs through an injected *slog.Logger. Three forbidigo rules refuse
// the package-level slog functions, os.Stdout and os.Stderr, and log.Print,
// Printf and Println; one exclusion exempts the files the output-boundary
// check allows, the CLI's two writer files and the code that is not product
// code (tests, the verification tool, the live suite, testutil), and no
// other. The filesystem packages' own exclusion exempts the raw os.* rule
// alone, so the output rules hold there as everywhere else.
func TestGolangciConfig_OutputRulesExemptOnlyTheCLIWritersAndNonProductCode(t *testing.T) {
	t.Parallel()

	config := golangciConfig(t)
	assertLintReadsEveryFile(t, config, "forbidigo")
	outputRules := []struct {
		pattern, pkg, message, identifier string
	}{
		{pattern: `^slog\.(Debug|Info|Warn|Error)$`, pkg: `^log/slog$`, message: slogLintMessage, identifier: "slog.Info"},
		{pattern: `^os\.(Stdout|Stderr)$`, pkg: `^os$`, message: stdStreamLintMessage, identifier: "os.Stderr"},
		{pattern: `^log\.Print(f|ln)?$`, pkg: `^log$`, message: logPrintLintMessage, identifier: "log.Printf"},
	}

	for _, want := range outputRules {
		rule := forbidigoRule(t, config, want.message)
		assert.Equal(t, want.pattern, rule["pattern"])
		assert.Equal(t, want.pkg, rule["pkg"])
		assert.Len(t, lintExclusionsAdmitting(t, config, "forbidigo", forbidigoIssueText(want.identifier, want.message)), 1,
			"one exclusion exempts files from %q: the one compared with the output-boundary check below", want.message)

		for _, path := range slices.Concat(outputBoundaryAllowedSamples(), outputBoundaryBoundSamples()) {
			assert.Equal(t, outputBoundaryFileAllowed(path), forbidigoIssueExempt(t, config, path, want.identifier, want.message),
				"lint exempts %s from %q exactly when the output-boundary check allows it", path, want.message)
		}
	}

	assert.True(t, forbidigoIssueExempt(t, config, "internal/fsroot/root.go", "os.Open", rawOSLintMessage),
		"the filesystem packages keep their raw os.* exemption")
}

// Validates: R-6.10.5
//
// The verification tool imports no product package, so a change to a product
// package never has to edit devtool, and devtool never opens a product store.
// depguard's lax mode denies the module's internal/ prefix in both devtool
// trees, tests included, and allows internal/devtool by its exact path: in
// lax mode an allow entry longer than the deny it overlaps wins. No exclusion
// exempts a file from the rule.
func TestGolangciConfig_DevtoolImportsNoProductPackage(t *testing.T) {
	t.Parallel()

	config := golangciConfig(t)
	assertLintReadsEveryFile(t, config, "depguard")

	rule := yamlValue(t, config, "linters", "settings", "depguard", "rules", "devtool-no-product")
	assert.Equal(t, map[string]any{
		"list-mode": "lax",
		"files":     []any{"**/internal/devtool/**", "**/cmd/devtool/**"},
		"allow":     []any{"github.com/tonimelisma/onedrive-go/internal/devtool$"},
		"deny": []any{map[string]any{
			"pkg":  "github.com/tonimelisma/onedrive-go/internal/",
			"desc": "the verification tool must not link product packages",
		}},
	}, rule)

	// depguard's issue names the import, the rule and the deny entry's desc.
	issue := "import 'github.com/tonimelisma/onedrive-go/internal/driveid' is not allowed from list 'devtool-no-product': " +
		"the verification tool must not link product packages"
	assert.Empty(t, lintExclusionsAdmitting(t, config, "depguard", issue), "no exclusion exempts any file from the devtool rule")
}

// Validates: R-6.10.2
//
// CLAUDE.md requires checking Close() on anything written to, because Close
// flushes buffers and a dropped error there is silent data loss. That rule is
// enforced only by errcheck, and errcheck once excluded every source line
// containing "defer " -- which switched the rule off entirely while the
// codebase appeared to comply.
//
// The exclusion is now scoped to test files. Nothing but this test stops the
// scope from being widened back, and the failure would be silent: production
// write-path closes would simply stop being reported.
func TestGolangciConfig_DeferErrcheckExclusionStaysScopedToTests(t *testing.T) {
	t.Parallel()

	lines := golangciConfigLines(t)

	found := 0

	for i, line := range lines {
		if !strings.Contains(line, `source:`) || !strings.Contains(line, "defer") {
			continue
		}

		found++

		// Walk back to the start of this exclusion entry and require that it
		// scopes itself to test files.
		scoped := false

		for j := i; j >= 0 && j > i-8; j-- {
			if strings.Contains(lines[j], `path: _test\.go`) {
				scoped = true

				break
			}
			if strings.Contains(lines[j], "- linters:") && j != i {
				break
			}
		}

		assert.Truef(t, scoped,
			"errcheck defer exclusion at .golangci.yml:%d is not scoped to _test.go; "+
				"widening it silently disables write-path Close() checking in production", i+1)
	}

	require.Positivef(t, found,
		"expected a scoped defer exclusion to exist; if it was removed entirely, delete this test deliberately")
}

// Validates: R-6.10.2
//
// depguard matches a rule's files patterns against each file's absolute path,
// anchored, and golangci-lint rewrites nothing in them but ${base-path} and
// ${config-path}. A pattern written relative to the repository, such as
// internal/graph/*.go, therefore never matches, and its rule is off without a
// word: every path-scoped rule here was, the leaf-package rule among them.
// Each pattern has to start with **/ or be a depguard variable such as $test.
func TestGolangciConfig_DepguardFilePatternsMatchAbsolutePaths(t *testing.T) {
	t.Parallel()

	patterns := depguardFilePatterns(golangciConfigLines(t))
	require.NotEmpty(t, patterns, "expected depguard rules with file patterns")

	for _, pattern := range patterns {
		bare := strings.TrimPrefix(pattern, "!")
		assert.Truef(t, strings.HasPrefix(bare, "**/") || strings.HasPrefix(bare, "$"),
			"depguard files pattern %q never matches: depguard matches absolute paths, so start it with **/", pattern)
	}
}

// Validates: R-6.10.2
//
// A depguard rule names its packages by folder. A package that is renamed or
// removed leaves its pattern matching nothing and its rule off for whatever
// took its place, without a word, so every **/<folder>/*.go and
// **/<folder>/** pattern has to name a folder that holds a non-test Go file.
func TestGolangciConfig_DepguardPackagePatternsNameExistingPackages(t *testing.T) {
	t.Parallel()

	root := repoRootFromDevtoolPackage(t)
	checked := 0

	for _, pattern := range depguardFilePatterns(golangciConfigLines(t)) {
		// A negated pattern names its folder all the same.
		folder, isPackagePattern := strings.CutPrefix(strings.TrimPrefix(pattern, "!"), "**/")
		if !isPackagePattern {
			continue
		}
		folder, isPackagePattern = strings.CutSuffix(folder, "/*.go")
		if !isPackagePattern {
			folder, isPackagePattern = strings.CutSuffix(folder, "/**")
		}
		if !isPackagePattern {
			continue
		}
		checked++

		assert.Truef(t, holdsNonTestGoFile(t, filepath.Join(root, filepath.FromSlash(folder))),
			"depguard files pattern %q names %s, which holds no non-test Go file", pattern, folder)
	}

	require.Positive(t, checked, "expected depguard rules that name package folders")
}

// Validates: R-6.10.20
//
// The sync tree's packages import one another in one direction, and depguard
// says which: every directory under internal/sync that holds a package has a
// lax rule over its non-test files that refuses the whole tree and allows,
// by exact path, the tree packages the table below lets it import. A package
// added to the tree without a rule, a rule for a package that is gone, and a
// rule that allows another import than the table all fail here.
func TestGolangciConfig_EverySyncTreePackageHasItsImportRule(t *testing.T) {
	t.Parallel()

	const tree = "github.com/tonimelisma/onedrive-go/internal/sync"
	may := map[string][]string{
		"internal/sync/syncdomain":             nil,
		"internal/sync/syncshortcut":           {"syncdomain"},
		"internal/sync/syncstore":              {"syncdomain", "syncshortcut"},
		"internal/sync/internal/syncplan":      {"syncdomain"},
		"internal/sync/internal/syncobserve":   {"syncdomain", "syncshortcut"},
		"internal/sync/internal/syncexec":      {"syncdomain"},
		"internal/sync":                        {"syncdomain", "syncshortcut", "syncstore", "internal/syncplan", "internal/syncobserve", "internal/syncexec"},
		"internal/sync/internal/syncfixture":   {"syncdomain"},
		"internal/sync/internal/syncstoretest": {"syncdomain", "syncshortcut", "syncstore", "internal/syncfixture"},
	}

	root := repoRootFromDevtoolPackage(t)
	var dirs []string
	err := filepath.WalkDir(filepath.Join(root, "internal", "sync"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == "testdata" {
			return filepath.SkipDir
		}
		if entry.IsDir() && holdsNonTestGoFile(t, path) {
			rel, relErr := filepath.Rel(root, path)
			require.NoError(t, relErr)
			dirs = append(dirs, filepath.ToSlash(rel))
		}

		return nil
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, slices.Collect(maps.Keys(may)), dirs, "the table names every package of the sync tree")

	rules := yamlMapping(t, golangciConfig(t), "linters", "settings", "depguard", "rules")
	for dir, allowed := range may {
		var found map[string]any
		for _, node := range rules {
			rule, isRule := node.(map[string]any)
			if isRule && slices.Equal(yamlList(t, rule, "files"), []any{"**/" + dir + "/*.go", "!$test"}) {
				found = rule
			}
		}
		require.NotNil(t, found, "no depguard rule holds the non-test files of %s alone", dir)
		assert.Equal(t, "lax", found["list-mode"], dir)
		var denied []any
		for _, entry := range yamlList(t, found, "deny") {
			denied = append(denied, yamlValue(t, entry, "pkg"))
		}
		assert.Equal(t, []any{tree + "$", tree + "/"}, denied, "%s refuses the whole tree", dir)
		want := []any{}
		for _, pkg := range allowed {
			want = append(want, tree+"/"+pkg+"$")
		}
		got := []any{}
		if list, hasAllow := found["allow"].([]any); hasAllow {
			got = list
		}
		assert.Equal(t, want, got, "%s allows exactly the tree packages it may import", dir)
	}
}

// Validates: R-6.10.22
//
// devtool gives golangci-lint its deadline on the command line, scaled to the
// host's cores, and .golangci.yml keeps a deadline for a linter started by
// hand. On a host the size of a CI runner the two are one number, and nothing
// but this test stops one from being changed without the other.
func TestGolangciConfig_RunTimeoutIsDevtoolsBaseLintDeadline(t *testing.T) {
	t.Parallel()

	value, ok := golangciRunTimeout(golangciConfigLines(t))
	require.True(t, ok, "expected a timeout in the run block of .golangci.yml")

	timeout, err := time.ParseDuration(value)
	require.NoError(t, err)
	assert.Equal(t, lintBaseTimeout, timeout)
}

// golangciRunTimeout returns the timeout the run block of the config's lines
// holds.
func golangciRunTimeout(lines []string) (string, bool) {
	inRun := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			inRun = trimmed == "run:"

			continue
		}
		if value, found := strings.CutPrefix(trimmed, "timeout:"); inRun && found {
			return strings.TrimSpace(value), true
		}
	}

	return "", false
}

// holdsNonTestGoFile reports whether dir exists and holds a Go file that is
// not a test.
func holdsNonTestGoFile(t *testing.T, dir string) bool {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)

	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			return true
		}
	}

	return false
}

// depguardFilePatterns returns every entry of a files list inside the
// depguard settings block of the config's lines.
func depguardFilePatterns(lines []string) []string {
	var patterns []string

	depguardIndent, filesIndent := -1, -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if depguardIndent < 0 {
			if trimmed == "depguard:" {
				depguardIndent = indent
			}

			continue
		}
		if indent <= depguardIndent {
			break
		}

		if filesIndent >= 0 && indent > filesIndent && strings.HasPrefix(trimmed, "- ") {
			patterns = append(patterns, strings.Trim(strings.TrimPrefix(trimmed, "- "), `"'`))

			continue
		}

		filesIndent = -1
		if trimmed == "files:" {
			filesIndent = indent
		}
	}

	return patterns
}

// forbidigoRule returns the one forbid entry of the config whose msg is
// message.
func forbidigoRule(t *testing.T, config map[string]any, message string) map[string]string {
	t.Helper()

	var found []map[string]string
	for _, entry := range yamlList(t, config, "linters", "settings", "forbidigo", "forbid") {
		fields, ok := entry.(map[string]any)
		require.True(t, ok, "a forbid entry is a mapping: %v", entry)
		if fields["msg"] != message {
			continue
		}
		rule := map[string]string{}
		for key, value := range fields {
			text, isText := value.(string)
			require.True(t, isText, "forbid entry field %s is a scalar", key)
			rule[key] = text
		}
		found = append(found, rule)
	}
	require.Len(t, found, 1, "one forbidigo rule says %q", message)

	return found[0]
}

// assertLintReadsEveryFile asserts that golangci-lint runs linter over every
// file of the module: the linter is enabled, test files are linted, and no
// exclusion drops whole paths from every linter at once.
func assertLintReadsEveryFile(t *testing.T, config map[string]any, linter string) {
	t.Helper()

	assert.Contains(t, yamlList(t, config, "linters", "enable"), any(linter), "lint enables %s", linter)
	assert.NotEqual(t, "false", yamlMapping(t, config, "run")["tests"], "lint reads test files")
	assert.NotContains(t, yamlMapping(t, config, "linters", "exclusions"), "paths",
		"no exclusion drops whole paths from every linter")
}

// forbidigoIssueText is the text of the issue forbidigo reports for
// identifier under a rule whose msg is message.
func forbidigoIssueText(identifier, message string) string {
	return fmt.Sprintf("use of `%s` forbidden because %q", identifier, message)
}

// forbidigoIssueExempt reports whether an exclusion rule of the config drops
// the issue forbidigo reports for identifier at path (relative to the
// repository, slash-separated): one of lintExclusionsAdmitting's whose path
// and path-except agree.
func forbidigoIssueExempt(t *testing.T, config map[string]any, path, identifier, message string) bool {
	t.Helper()

	for _, rule := range lintExclusionsAdmitting(t, config, "forbidigo", forbidigoIssueText(identifier, message)) {
		if exclusionPatternAgrees(t, rule, "path", path, true) && exclusionPatternAgrees(t, rule, "path-except", path, false) {
			return true
		}
	}

	return false
}

// lintExclusionsAdmitting returns every exclusion rule of the config that
// drops linter's issue with text on some path, as golangci-lint applies its
// rules: one that names linter or names no linter at all (such a rule applies
// to every linter), and whose text agrees, whatever path it names. A rule
// that applies to linter and filters by source line fails the test: which
// lines it drops depends on the code, so no pin can say what it exempts.
func lintExclusionsAdmitting(t *testing.T, config map[string]any, linter, text string) []map[string]any {
	t.Helper()

	var admitting []map[string]any
	for index, entry := range yamlList(t, config, "linters", "exclusions", "rules") {
		rule, ok := entry.(map[string]any)
		require.True(t, ok, "an exclusion rule is a mapping: %v", entry)
		if linters, namesLinters := rule["linters"]; namesLinters {
			list, isList := linters.([]any)
			require.True(t, isList, "exclusion rule %d lists its linters as a block sequence", index+1)
			if !slices.Contains(list, any(linter)) {
				continue
			}
		}
		require.NotContains(t, rule, "source",
			"exclusion rule %d drops %s issues by their source line, which no pin can check", index+1, linter)
		if exclusionPatternAgrees(t, rule, "text", text, true) {
			admitting = append(admitting, rule)
		}
	}

	return admitting
}

// exclusionPatternAgrees reports whether the rule's regular expression under
// key lets subject through: it matches (or, for path-except, does not), or
// the rule has no such key.
func exclusionPatternAgrees(t *testing.T, rule map[string]any, key, subject string, wantMatch bool) bool {
	t.Helper()

	value, present := rule[key]
	if !present {
		return true
	}
	pattern, ok := value.(string)
	require.True(t, ok, "exclusion %s is a scalar", key)

	return regexp.MustCompile(pattern).MatchString(subject) == wantMatch
}

// golangciConfig is .golangci.yml read as the subset of YAML it is written
// in: block mappings, block sequences, and plain or quoted scalars, a flow
// sequence kept as its text. A line the subset does not hold fails the test.
func golangciConfig(t *testing.T) map[string]any {
	t.Helper()

	lines := yamlLines(golangciConfigLines(t))
	require.NotEmpty(t, lines)
	node, next := parseYAMLBlock(lines, 0)
	require.Equal(t, len(lines), next, "unread line: %q", lines[min(next, len(lines)-1)].text)
	config, ok := node.(map[string]any)
	require.True(t, ok, ".golangci.yml is a mapping")

	return config
}

// yamlValue returns the node at keys.
func yamlValue(t *testing.T, node any, keys ...string) any {
	t.Helper()

	for _, key := range keys {
		mapping, ok := node.(map[string]any)
		require.True(t, ok, "%s is under a mapping", key)
		node, ok = mapping[key]
		require.True(t, ok, "the config holds %s", key)
	}

	return node
}

// yamlMapping returns the mapping at keys.
func yamlMapping(t *testing.T, node any, keys ...string) map[string]any {
	t.Helper()

	mapping, ok := yamlValue(t, node, keys...).(map[string]any)
	require.True(t, ok, "%s is a mapping", strings.Join(keys, "."))

	return mapping
}

// yamlList returns the sequence at keys.
func yamlList(t *testing.T, node any, keys ...string) []any {
	t.Helper()

	list, ok := yamlValue(t, node, keys...).([]any)
	require.True(t, ok, "%s is a sequence", strings.Join(keys, "."))

	return list
}

// yamlLine is one line of YAML that holds content: its indent and its text.
// A sequence item that opens a mapping on its own line ("- key: value") is
// split into the bare "-" and the mapping's first line two columns deeper.
type yamlLine struct {
	indent int
	text   string
}

func yamlLines(raw []string) []yamlLine {
	var lines []yamlLine
	for _, line := range raw {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if rest, isItem := strings.CutPrefix(text, "- "); isItem {
			if _, _, isKey := yamlKeyValue(rest); isKey {
				lines = append(lines, yamlLine{indent: indent, text: "-"}, yamlLine{indent: indent + len("- "), text: rest})

				continue
			}
		}
		lines = append(lines, yamlLine{indent: indent, text: text})
	}

	return lines
}

// parseYAMLBlock reads the block that starts at lines[start] and returns it
// with the index of the first line after it.
func parseYAMLBlock(lines []yamlLine, start int) (any, int) {
	indent := lines[start].indent
	if lines[start].text == "-" || strings.HasPrefix(lines[start].text, "- ") {
		var items []any
		next := start
		for next < len(lines) && lines[next].indent == indent {
			if lines[next].text != "-" {
				rest, isItem := strings.CutPrefix(lines[next].text, "- ")
				if !isItem {
					break
				}
				items = append(items, yamlScalar(rest))
				next++

				continue
			}
			var item any
			item, next = parseYAMLBlock(lines, next+1)
			items = append(items, item)
		}

		return items, next
	}

	mapping := map[string]any{}
	next := start
	for next < len(lines) && lines[next].indent == indent {
		key, value, isKey := yamlKeyValue(lines[next].text)
		if !isKey {
			break
		}
		next++
		if value != "" || next == len(lines) || lines[next].indent <= indent {
			mapping[key] = yamlScalar(value)

			continue
		}
		mapping[key], next = parseYAMLBlock(lines, next)
	}

	return mapping, next
}

// yamlKeyValue splits "key: value" and "key:". A key holds no space, so a
// quoted text or a scalar whose comment holds a colon is not one.
func yamlKeyValue(text string) (string, string, bool) {
	key, value, found := strings.Cut(text, ": ")
	if !found {
		key, found = strings.CutSuffix(text, ":")
	}
	if !found || key == "" || strings.ContainsAny(key, ` "'`) {
		return "", "", false
	}

	return key, strings.TrimSpace(value), true
}

// yamlScalar unquotes a quoted scalar and drops a plain one's comment.
func yamlScalar(text string) string {
	switch {
	case strings.HasPrefix(text, `"`):
		if unquoted, err := strconv.Unquote(text); err == nil {
			return unquoted
		}
	case strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'") && len(text) > 1:
		return strings.ReplaceAll(text[1:len(text)-1], "''", "'")
	}
	if plain, _, hasComment := strings.Cut(text, " #"); hasComment {
		return strings.TrimSpace(plain)
	}

	return text
}

func golangciConfigLines(t *testing.T) []string {
	t.Helper()

	root := repoRootFromDevtoolPackage(t)
	//nolint:gosec // reads a fixed repo-relative config path.
	raw, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	require.NoError(t, err)

	return strings.Split(string(raw), "\n")
}

func repoRootFromDevtoolPackage(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)

	return filepath.Dir(filepath.Dir(wd))
}
