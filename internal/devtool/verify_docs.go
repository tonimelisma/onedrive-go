package devtool

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/tonimelisma/onedrive-go/internal/clishape"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type documentedCLIFlagSpec struct {
	consumesValue bool
}

type documentedCLICommandSpec struct {
	name        string
	runnable    bool
	parent      *documentedCLICommandSpec
	flags       map[string]documentedCLIFlagSpec
	subcommands map[string]*documentedCLICommandSpec
}

const documentedCLIExecutableName = "onedrive-go"

var documentedCLIExamplePattern = regexp.MustCompile(documentedCLIExecutableName + "(?:\\s+[^\\s`'\"\\)\\(]+)+")

type documentedCLIExampleResolver struct {
	root *documentedCLICommandSpec
}

func ensureActiveDocCLIExamplesResolve(repoRoot string) error {
	checkRoots := []string{
		filepath.Join(repoRoot, "spec", "design"),
		filepath.Join(repoRoot, "spec", "reference"),
		filepath.Join(repoRoot, "spec", "requirements"),
		filepath.Join(repoRoot, "README.md"),
		filepath.Join(repoRoot, "CLAUDE.md"),
		filepath.Join(repoRoot, "AGENTS.md"),
	}

	resolver := documentedCLIExampleResolver{
		root: currentDocumentedCLIManifest(),
	}

	for _, root := range checkRoots {
		if err := walkDocumentedCLIExamples(root, resolver.resolve); err != nil {
			return err
		}
	}

	return nil
}

func (r documentedCLIExampleResolver) resolve(snippet string) error {
	args := strings.Fields(strings.TrimSpace(snippet))
	if !isDocumentedCLIExample(args) {
		return nil
	}

	current, commandSeen, err := r.resolveTokens(args[1:])
	if err != nil {
		return err
	}

	if !commandSeen {
		return fmt.Errorf("no command found")
	}
	if !current.runnable && len(current.subcommands) > 0 {
		return fmt.Errorf("command %q is incomplete", current.commandPath())
	}

	return nil
}

func isDocumentedCLIExample(args []string) bool {
	return len(args) != 0 && args[0] == documentedCLIExecutableName
}

func (r documentedCLIExampleResolver) resolveTokens(
	args []string,
) (*documentedCLICommandSpec, bool, error) {
	current := r.root
	commandSeen := false
	for i := 0; i < len(args); i++ {
		nextIndex, nextCommand, nextSeen, stop, err := documentedCLIConsumeToken(current, commandSeen, args, i)
		if err != nil {
			return nil, false, err
		}
		current = nextCommand
		commandSeen = nextSeen
		if stop {
			break
		}
		i = nextIndex
	}

	return current, commandSeen, nil
}

func documentedCLIConsumeToken(
	current *documentedCLICommandSpec,
	commandSeen bool,
	args []string,
	index int,
) (nextIndex int, nextCommand *documentedCLICommandSpec, nextSeen bool, stop bool, err error) {
	token := strings.TrimRight(strings.TrimSpace(args[index]), ".,:;")
	if token == "" {
		return index, current, commandSeen, false, nil
	}
	if strings.HasPrefix(token, "<") && strings.HasSuffix(token, ">") {
		if !commandSeen {
			return index, nil, false, false, fmt.Errorf("placeholder %s appears before any command", token)
		}

		return index, current, commandSeen, true, nil
	}
	if strings.HasPrefix(token, "-") {
		consumesValue, flagErr := documentedCLIFlagConsumesValue(current, token)
		if flagErr != nil {
			return index, nil, false, false, flagErr
		}
		if consumesValue && !strings.Contains(token, "=") {
			index++
			if index >= len(args) {
				return index, nil, false, false, fmt.Errorf("flag %s is missing its value", token)
			}
		}

		return index, current, commandSeen, false, nil
	}

	next := documentedCLISubcommand(current, token)
	if next == nil {
		if !commandSeen {
			return index, nil, false, false, fmt.Errorf("unknown command %q", token)
		}

		return index, current, commandSeen, true, nil
	}

	return index, next, true, false, nil
}

func documentedCLIFlagConsumesValue(cmd *documentedCLICommandSpec, token string) (bool, error) {
	name := documentedCLIFlagName(token)
	flag := documentedCLIFlag(cmd, name)
	if flag == nil {
		return false, fmt.Errorf("unknown flag %q", token)
	}

	return flag.consumesValue, nil
}

func documentedCLIFlagName(token string) string {
	trimmed := strings.TrimLeft(token, "-")
	parts := strings.SplitN(trimmed, "=", 2)

	return parts[0]
}

func documentedCLIFlag(cmd *documentedCLICommandSpec, name string) *documentedCLIFlagSpec {
	for current := cmd; current != nil; current = current.Parent() {
		if flag, ok := current.flags[name]; ok {
			flagCopy := flag
			return &flagCopy
		}
	}

	return nil
}

func (c *documentedCLICommandSpec) Parent() *documentedCLICommandSpec {
	if c == nil {
		return nil
	}

	return c.parent
}

func (c *documentedCLICommandSpec) commandPath() string {
	if c == nil {
		return "onedrive-go"
	}

	parts := []string{c.name}
	for current := c.parent; current != nil; current = current.parent {
		if current.name == "" {
			continue
		}
		parts = append(parts, current.name)
	}
	slices.Reverse(parts)

	return strings.Join(parts, " ")
}

func documentedCLISubcommand(cmd *documentedCLICommandSpec, token string) *documentedCLICommandSpec {
	if cmd == nil {
		return nil
	}

	return cmd.subcommands[token]
}

func currentDocumentedCLIManifest() *documentedCLICommandSpec {
	return documentedCLIFromShape(clishape.Root(), nil)
}

func documentedCLIFromShape(
	spec clishape.CommandSpec,
	parent *documentedCLICommandSpec,
) *documentedCLICommandSpec {
	cmd := &documentedCLICommandSpec{
		name:        spec.Name,
		runnable:    spec.Runnable,
		parent:      parent,
		flags:       documentedCLIFlagsFromShape(spec.Flags),
		subcommands: map[string]*documentedCLICommandSpec{},
	}

	for i := range spec.Subcommands {
		child := documentedCLIFromShape(spec.Subcommands[i], cmd)
		cmd.subcommands[child.name] = child
	}

	return cmd
}

func documentedCLIFlagsFromShape(flags []clishape.FlagSpec) map[string]documentedCLIFlagSpec {
	if len(flags) == 0 {
		return nil
	}

	result := make(map[string]documentedCLIFlagSpec, len(flags))
	for _, flag := range flags {
		result[flag.Name] = documentedCLIFlagSpec{consumesValue: flag.ConsumesValue}
	}

	return result
}

func walkDocumentedCLIExamples(root string, resolver func(string) error) error {
	info, err := localpath.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat %s: %w", root, err)
	}

	if !info.IsDir() {
		return scanDocumentedCLIExamplesInFile(root, resolver)
	}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}

		return scanDocumentedCLIExamplesInFile(path, resolver)
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", root, walkErr)
	}

	return nil
}

func scanDocumentedCLIExamplesInFile(path string, resolver func(string) error) error {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	for lineNumber, line := range lines {
		examples := documentedCLIExamplePattern.FindAllString(line, -1)
		for _, example := range examples {
			if err := resolver(strings.TrimSpace(example)); err != nil {
				return fmt.Errorf("invalid documented CLI example in %s:%d (%s): %w", path, lineNumber+1, example, err)
			}
		}
	}

	return nil
}

var (
	markdownLinkTargetPattern     = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)
	markdownExplicitAnchorPattern = regexp.MustCompile(`<a\s+id="([^"]+)"`)
)

type liveIncidentDocPromotion struct {
	incidentID   string
	isRecurring  bool
	promotedDocs string
}

func ensureRecurringIncidentPromotedDocsResolve(repoRoot string) error {
	liveIncidentsPath := filepath.Join(repoRoot, "spec", "reference", "live-incidents.md")
	data, err := localpath.ReadFile(liveIncidentsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read live incidents: %w", err)
	}

	promotions := parseLiveIncidentPromotions(string(data))
	for _, promotion := range promotions {
		if !promotion.isRecurring {
			continue
		}
		if promotion.promotedDocs == "" {
			return fmt.Errorf("recurring incident %s is missing Promoted docs", promotion.incidentID)
		}
		if strings.EqualFold(strings.TrimSpace(promotion.promotedDocs), "none") {
			return fmt.Errorf("recurring incident %s cannot use Promoted docs: none", promotion.incidentID)
		}

		targets := markdownLinkTargets(promotion.promotedDocs)
		if len(targets) == 0 {
			return fmt.Errorf("recurring incident %s has malformed Promoted docs links", promotion.incidentID)
		}
		for _, target := range targets {
			if err := validatePromotedDocLink(liveIncidentsPath, target); err != nil {
				return fmt.Errorf("recurring incident %s promoted doc %q: %w", promotion.incidentID, target, err)
			}
		}
	}

	return nil
}

func parseLiveIncidentPromotions(source string) []liveIncidentDocPromotion {
	lines := strings.Split(source, "\n")
	promotions := make([]liveIncidentDocPromotion, 0)
	var current *liveIncidentDocPromotion

	flush := func() {
		if current == nil || current.incidentID == "" {
			return
		}
		promotions = append(promotions, *current)
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## LI-") {
			flush()
			heading := strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			incidentID, _, _ := strings.Cut(heading, ":")
			current = &liveIncidentDocPromotion{
				incidentID: strings.TrimSpace(incidentID),
			}
			continue
		}
		if current == nil {
			continue
		}

		if strings.HasPrefix(trimmed, "Recurring:") {
			current.isRecurring = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "Recurring:")), "yes")
			continue
		}
		if strings.HasPrefix(trimmed, "Promoted docs:") {
			current.promotedDocs = strings.TrimSpace(strings.TrimPrefix(trimmed, "Promoted docs:"))
		}
	}

	flush()
	return promotions
}

func markdownLinkTargets(source string) []string {
	matches := markdownLinkTargetPattern.FindAllStringSubmatch(source, -1)
	targets := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		targets = append(targets, strings.TrimSpace(match[1]))
	}

	return targets
}

func validatePromotedDocLink(basePath string, target string) error {
	pathPart, fragment, _ := strings.Cut(target, "#")
	resolvedPath := basePath
	if pathPart != "" {
		resolvedPath = filepath.Clean(filepath.Join(filepath.Dir(basePath), filepath.FromSlash(pathPart)))
	}

	if _, err := localpath.Stat(resolvedPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing target %s", resolvedPath)
		}

		return fmt.Errorf("stat target %s: %w", resolvedPath, err)
	}

	if fragment == "" {
		return nil
	}

	ok, err := markdownDocumentHasAnchor(resolvedPath, fragment)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("missing anchor #%s in %s", fragment, resolvedPath)
	}

	return nil
}

func markdownDocumentHasAnchor(path string, fragment string) (bool, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read target %s: %w", path, err)
	}

	anchors := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		for _, match := range markdownExplicitAnchorPattern.FindAllStringSubmatch(line, -1) {
			if len(match) >= 2 && match[1] != "" {
				anchors[match[1]] = struct{}{}
			}
		}

		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}

		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		if heading == "" {
			continue
		}

		anchors[slugifyMarkdownHeading(heading)] = struct{}{}
	}

	_, ok := anchors[fragment]
	return ok, nil
}

func slugifyMarkdownHeading(heading string) string {
	var builder strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-':
			if builder.Len() == 0 || lastHyphen {
				continue
			}
			builder.WriteByte('-')
			lastHyphen = true
		}
	}

	return strings.Trim(builder.String(), "-")
}

func ensureGovernedDesignDocsHaveOwnershipContracts(repoRoot string) error {
	designDocs, err := filepath.Glob(filepath.Join(repoRoot, "spec", "design", "*.md"))
	if err != nil {
		return fmt.Errorf("glob design docs: %w", err)
	}

	for _, path := range designDocs {
		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}

		content := string(data)
		if !strings.Contains(content, "GOVERNS:") {
			continue
		}

		if !strings.Contains(content, "## Ownership Contract") {
			return fmt.Errorf("governed design doc missing Ownership Contract section: %s", path)
		}

		for _, bullet := range ownershipContractBullets() {
			if !strings.Contains(content, bullet) {
				return fmt.Errorf("governed design doc missing Ownership Contract bullet %q: %s", strings.TrimPrefix(bullet, "- "), path)
			}
		}
	}

	return nil
}

func ownershipContractBullets() []string {
	return []string{
		"- Owns:",
		"- Does Not Own:",
		"- Source of Truth:",
		"- Allowed Side Effects:",
		"- Mutable Runtime Owner:",
		"- Error Boundary:",
	}
}

func ensureCrossCuttingDesignDocs(repoRoot string) error {
	systemPath := filepath.Join(repoRoot, "spec", "design", "system.md")
	systemData, err := localpath.ReadFile(systemPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", systemPath, err)
	}

	systemText := string(systemData)
	for _, name := range requiredCrossCuttingDesignDocs() {
		path := filepath.Join(repoRoot, "spec", "design", name)
		if _, statErr := localpath.Stat(path); statErr != nil {
			return fmt.Errorf("required cross-cutting design doc missing: %s", path)
		}
		if !strings.Contains(systemText, name) {
			return fmt.Errorf("system.md missing required design doc reference %s: %s", name, systemPath)
		}
	}

	return nil
}

func requiredCrossCuttingDesignDocs() []string {
	return []string{"error-model.md", "threat-model.md", "degraded-mode.md"}
}

type docSnippetCheck struct {
	path     string
	snippets []string
}

func ensureCrossCuttingDesignDocEvidence(repoRoot string) error {
	return ensureDocsContainSnippets("cross-cutting design doc", []docSnippetCheck{
		{
			path: filepath.Join(repoRoot, "spec", "design", "error-model.md"),
			snippets: []string{
				"## Verified By",
				"| Boundary | Evidence |",
			},
		},
		{
			path: filepath.Join(repoRoot, "spec", "design", "degraded-mode.md"),
			snippets: []string{
				"| ID |",
				"| Evidence |",
			},
		},
		{
			path: filepath.Join(repoRoot, "spec", "design", "threat-model.md"),
			snippets: []string{
				"## Mitigation Evidence",
				"| Mitigation | Evidence |",
			},
		},
	})
}

func ensureGovernedBehaviorDocsHaveEvidence(repoRoot string) error {
	checks := governedBehaviorEvidenceDocs(repoRoot)
	snippetChecks := make([]docSnippetCheck, 0, len(checks))
	for _, check := range checks {
		snippetChecks = append(snippetChecks, docSnippetCheck{
			path: check.path,
			snippets: []string{
				check.heading,
				"| Behavior | Evidence |",
			},
		})
	}

	return ensureDocsContainSnippets("governed design doc", snippetChecks)
}

func ensureDocsContainSnippets(docKind string, checks []docSnippetCheck) error {
	for _, check := range checks {
		data, err := localpath.ReadFile(check.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", check.path, err)
		}
		content := string(data)
		for _, snippet := range check.snippets {
			if !strings.Contains(content, snippet) {
				return fmt.Errorf("%s missing required evidence snippet %q: %s", docKind, snippet, check.path)
			}
		}
	}

	return nil
}

var (
	requirementDeclarationPattern = regexp.MustCompile(`^(?:#+|- )\s*(R-\d+(?:\.\d+)*)\b`)
	requirementIDPattern          = regexp.MustCompile(`^R-\d+(?:\.\d+)*$`)
	implementsEntryPattern        = regexp.MustCompile(`^(R-\d+(?:\.\d+)*) \[[^][]+\]$`)
	testFunctionPattern           = regexp.MustCompile(`\bTest[A-Z0-9_][A-Za-z0-9_]*\b`)
)

func ensureRequirementReferencesResolve(repoRoot string) error {
	registry, err := loadRequirementRegistry(repoRoot)
	if err != nil {
		return err
	}

	var problems []string

	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		switch {
		case strings.HasSuffix(path, "_test.go"):
			fileProblems, validateErr := validateRequirementReferencesInTestFile(path, registry)
			if validateErr != nil {
				return validateErr
			}
			problems = append(problems, fileProblems...)
		case strings.HasPrefix(path, filepath.Join(repoRoot, "spec", "design")+string(filepath.Separator)) &&
			strings.HasSuffix(path, ".md"):
			fileProblems, validateErr := validateRequirementReferencesInDesignDoc(path, registry)
			if validateErr != nil {
				return validateErr
			}
			problems = append(problems, fileProblems...)
		}

		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk requirement references: %w", walkErr)
	}

	if len(problems) > 0 {
		return fmt.Errorf("requirement reference validation failed:\n%s", strings.Join(problems, "\n"))
	}

	return nil
}

func loadRequirementRegistry(repoRoot string) (map[string]struct{}, error) {
	requirementFiles, err := filepath.Glob(filepath.Join(repoRoot, "spec", "requirements", "*.md"))
	if err != nil {
		return nil, fmt.Errorf("glob requirement files: %w", err)
	}

	registry := make(map[string]struct{})
	for _, path := range requirementFiles {
		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}

		for _, line := range strings.Split(string(data), "\n") {
			matches := requirementDeclarationPattern.FindStringSubmatch(strings.TrimSpace(line))
			if len(matches) == 2 {
				registry[matches[1]] = struct{}{}
			}
		}
	}

	if len(registry) == 0 {
		return nil, fmt.Errorf("load requirement registry: no declared requirement IDs found")
	}

	return registry, nil
}

func validateRequirementReferencesInTestFile(path string, registry map[string]struct{}) ([]string, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var problems []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			trimmed := strings.TrimSpace(comment.Text)
			if !strings.HasPrefix(trimmed, "// Validates:") {
				continue
			}

			ids, parseErr := parseValidatesLine(strings.TrimSpace(strings.TrimPrefix(trimmed, "// Validates:")))
			lineNumber := fset.Position(comment.Slash).Line
			if parseErr != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %v", path, lineNumber, parseErr))
				continue
			}

			for _, id := range ids {
				if _, ok := registry[id]; !ok {
					problems = append(problems, fmt.Sprintf("%s:%d: unknown requirement ID %s", path, lineNumber, id))
				}
			}
		}
	}

	return problems, nil
}

func parseValidatesLine(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty Validates reference list")
	}

	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty Validates reference entry")
		}
		if !requirementIDPattern.MatchString(part) {
			return nil, fmt.Errorf("malformed Validates reference %q", part)
		}
		ids = append(ids, part)
	}

	return ids, nil
}

func validateRequirementReferencesInDesignDoc(path string, registry map[string]struct{}) ([]string, error) {
	return validateRequirementReferencesInFile(path, "Implements:", registry, parseImplementsLine)
}

func validateRequirementReferencesInFile(
	path string,
	marker string,
	registry map[string]struct{},
	parse func(string) ([]string, error),
) ([]string, error) {
	data, err := localpath.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var problems []string
	for lineNumber, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, marker) {
			continue
		}

		ids, parseErr := parse(strings.TrimSpace(strings.TrimPrefix(trimmed, marker)))
		if parseErr != nil {
			problems = append(problems, fmt.Sprintf("%s:%d: %v", path, lineNumber+1, parseErr))
			continue
		}

		for _, id := range ids {
			if _, ok := registry[id]; !ok {
				problems = append(problems, fmt.Sprintf("%s:%d: unknown requirement ID %s", path, lineNumber+1, id))
			}
		}
	}

	return problems, nil
}

func parseImplementsLine(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty Implements reference list")
	}

	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty Implements reference entry")
		}

		matches := implementsEntryPattern.FindStringSubmatch(part)
		if len(matches) != 2 {
			return nil, fmt.Errorf("malformed Implements reference %q", part)
		}
		ids = append(ids, matches[1])
	}

	return ids, nil
}

type evidenceDocCheck struct {
	path    string
	heading string
}

func governedBehaviorEvidenceDocs(repoRoot string) []evidenceDocCheck {
	return []evidenceDocCheck{
		{path: filepath.Join(repoRoot, "spec", "design", "sync-engine.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-execution.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "cli.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-control-plane.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-store.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "sync-observation.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "config.md"), heading: "## Verified By"},
	}
}

func ensureEvidenceDocsReferenceRealTests(repoRoot string) error {
	testRegistry, err := loadTestRegistry(repoRoot)
	if err != nil {
		return err
	}

	checks := []evidenceDocCheck{
		{path: filepath.Join(repoRoot, "spec", "design", "error-model.md"), heading: "## Verified By"},
		{path: filepath.Join(repoRoot, "spec", "design", "threat-model.md"), heading: "## Mitigation Evidence"},
		{path: filepath.Join(repoRoot, "spec", "design", "degraded-mode.md")},
	}
	checks = append(checks, governedBehaviorEvidenceDocs(repoRoot)...)

	var problems []string
	for _, check := range checks {
		docProblems, docErr := validateEvidenceDocReferences(check, testRegistry)
		if docErr != nil {
			return docErr
		}
		problems = append(problems, docProblems...)
	}

	if len(problems) > 0 {
		return fmt.Errorf("evidence doc validation failed:\n%s", strings.Join(problems, "\n"))
	}

	return nil
}

func loadTestRegistry(repoRoot string) (map[string]struct{}, error) {
	registry := make(map[string]struct{})

	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, readErr := localpath.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, data, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "Test") {
				registry[fn.Name.Name] = struct{}{}
			}
		}

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk test registry: %w", walkErr)
	}

	if len(registry) == 0 {
		return nil, fmt.Errorf("load test registry: no test functions found")
	}

	return registry, nil
}

func validateEvidenceDocReferences(check evidenceDocCheck, testRegistry map[string]struct{}) ([]string, error) {
	data, err := localpath.ReadFile(check.path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", check.path, err)
	}

	content := string(data)
	section := content
	if check.heading != "" {
		section, err = markdownSection(content, check.heading)
		if err != nil {
			return []string{fmt.Sprintf("%s: %v", check.path, err)}, nil
		}
	}

	matches := testFunctionPattern.FindAllString(section, -1)
	if len(matches) == 0 {
		return []string{fmt.Sprintf("%s: no exact test names found in evidence section", check.path)}, nil
	}

	var problems []string
	seen := make(map[string]struct{})
	for _, name := range matches {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := testRegistry[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown test function %s", check.path, name))
		}
	}

	return problems, nil
}

func markdownSection(content, heading string) (string, error) {
	start := strings.Index(content, heading)
	if start == -1 {
		return "", fmt.Errorf("missing section %q", heading)
	}

	remaining := content[start:]
	nextOffset := strings.Index(remaining[len(heading):], "\n## ")
	if nextOffset == -1 {
		return remaining, nil
	}

	return remaining[:len(heading)+nextOffset], nil
}
