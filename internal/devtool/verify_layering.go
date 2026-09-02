package devtool

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Layering enforces the direction of dependencies between design-doc families
// inside a single Go package.
//
// The compiler cannot help here: every sync family compiles into one package,
// so a reference from the store up into the engine is as legal to the compiler
// as any other. The families are real all the same -- each design doc declares
// which files it owns -- and the direction between them is a design decision
// worth keeping true. This check is what makes it checkable.
//
// A doc opts in by declaring `LAYER: n` beside its `GOVERNS:` line. A ranked
// family may reference symbols from families of the same rank or lower, never
// higher. Same-rank references are allowed because two families can be
// genuinely mutually recursive without either being above the other.
//
// A family without a LAYER line is unranked: references to and from it are not
// checked. That is deliberate. The set of unranked families is the remaining
// work, it is visible in the docs, and it only shrinks -- which is a more
// honest instrument than an allowlist of individual exceptions that grows
// quietly.

const layerPrefix = "LAYER:"

var layerLinePattern = regexp.MustCompile(`(?m)^` + layerPrefix + `\s*(\d+)\s*$`)

type layeringViolation struct {
	file       string
	line       int
	symbol     string
	fromFamily string
	fromRank   int
	toFamily   string
	toRank     int
}

func (v layeringViolation) String() string {
	return fmt.Sprintf("%s:%d: %s references %s, which %s (layer %d) owns; %s is layer %d and may not depend upward",
		v.file, v.line, filepath.Base(v.file), v.symbol, v.toFamily, v.toRank, v.fromFamily, v.fromRank)
}

func runLayering(
	_ context.Context,
	_ commandRunner,
	repoRoot string,
	_ []string,
	stdout, _ io.Writer,
) error {
	if err := writeStatus(stdout, "==> layering\n"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	violations, err := findLayeringViolations(repoRoot)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}

	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, "  "+v.String())
	}

	return fmt.Errorf("layering check failed:\n%s", strings.Join(lines, "\n"))
}

// readLayerRanks returns the declared rank per design doc, for docs that
// declare one.
func readLayerRanks(repoRoot string) (map[string]int, error) {
	docs, err := filepath.Glob(filepath.Join(repoRoot, specDesignDir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("list design docs: %w", err)
	}

	ranks := make(map[string]int, len(docs))
	for _, doc := range docs {
		data, readErr := readFile(doc)
		if readErr != nil {
			return nil, fmt.Errorf("read design doc %s: %w", doc, readErr)
		}
		match := layerLinePattern.FindSubmatch(data)
		if match == nil {
			continue
		}
		rank, convErr := strconv.Atoi(string(match[1]))
		if convErr != nil {
			return nil, fmt.Errorf("parse LAYER in %s: %w", doc, convErr)
		}
		ranks[repoRelative(repoRoot, doc)] = rank
	}

	return ranks, nil
}

// familyOf maps a production file to the single doc that owns it, using the
// same most-specific-pattern rule the GOVERNS check applies.
func familyOf(ownership map[string][]governsClaim, file string) string {
	claims, ok := ownership[file]
	if !ok {
		return ""
	}
	owners := resolveOwners(claims)
	if len(owners) != 1 {
		return ""
	}

	return owners[0]
}

// layeredFile is one production file placed in its owning family.
type layeredFile struct {
	rel     string
	pkgDir  string
	family  string
	rank    int
	astFile *ast.File
	fset    *token.FileSet
}

// collectLayeredFiles parses every governed production file and records which
// family declares each package-scope symbol. Unranked families are parsed too,
// with rank -1, so their symbols stay attributable rather than looking like
// they belong to whoever references them.
func collectLayeredFiles(
	repoRoot string,
	ranks map[string]int,
	ownership map[string][]governsClaim,
	files []string,
) ([]layeredFile, map[string]map[string]string, error) {
	var parsed []layeredFile
	declOwner := map[string]map[string]string{}

	for _, file := range files {
		rel := repoRelative(repoRoot, file)
		family := familyOf(ownership, rel)
		if family == "" {
			continue
		}

		rank, ranked := ranks[family]
		if !ranked {
			rank = -1
		}

		fset := token.NewFileSet()
		astFile, parseErr := parser.ParseFile(fset, filepath.Join(repoRoot, rel), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", rel, parseErr)
		}

		pkgDir := filepath.Dir(rel)
		if declOwner[pkgDir] == nil {
			declOwner[pkgDir] = map[string]string{}
		}
		for _, name := range topLevelDeclNames(astFile) {
			declOwner[pkgDir][name] = family
		}

		parsed = append(parsed, layeredFile{
			rel: rel, pkgDir: pkgDir, family: family, rank: rank,
			astFile: astFile, fset: fset,
		})
	}

	return parsed, declOwner, nil
}

// upwardReferences reports each distinct symbol this file reads from a family
// ranked above its own.
func upwardReferences(
	pf layeredFile,
	owners map[string]string,
	ranks map[string]int,
) []layeringViolation {
	var found []layeringViolation
	seen := map[string]struct{}{}

	ast.Inspect(pf.astFile, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}

		toFamily, known := owners[ident.Name]
		if !known || toFamily == pf.family {
			return true
		}

		toRank, ranked := ranks[toFamily]
		if !ranked || toRank <= pf.rank {
			return true
		}

		if _, dup := seen[ident.Name]; dup {
			return true
		}
		seen[ident.Name] = struct{}{}

		found = append(found, layeringViolation{
			file:       pf.rel,
			line:       pf.fset.Position(ident.Pos()).Line,
			symbol:     ident.Name,
			fromFamily: pf.family,
			fromRank:   pf.rank,
			toFamily:   toFamily,
			toRank:     toRank,
		})

		return true
	})

	return found
}

func findLayeringViolations(repoRoot string) ([]layeringViolation, error) {
	ranks, err := readLayerRanks(repoRoot)
	if err != nil {
		return nil, err
	}
	if len(ranks) == 0 {
		return nil, nil
	}

	ownership, _, err := readGovernsOwnership(repoRoot)
	if err != nil {
		return nil, err
	}

	files, err := collectProductionGoFiles(repoRoot)
	if err != nil {
		return nil, err
	}

	parsed, declOwner, err := collectLayeredFiles(repoRoot, ranks, ownership, files)
	if err != nil {
		return nil, err
	}

	var violations []layeringViolation
	for _, pf := range parsed {
		if pf.rank < 0 {
			continue
		}
		violations = append(violations, upwardReferences(pf, declOwner[pf.pkgDir], ranks)...)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}

		return violations[i].line < violations[j].line
	})

	return violations, nil
}

// topLevelDeclNames returns the names a file introduces into its package scope.
// Methods are excluded: they are reached through their receiver's type, which
// is itself owned by whichever family declares it.
func topLevelDeclNames(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		switch typed := decl.(type) {
		case *ast.FuncDecl:
			if typed.Recv == nil {
				names = append(names, typed.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch node := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, node.Name.Name)
				case *ast.ValueSpec:
					for _, ident := range node.Names {
						names = append(names, ident.Name)
					}
				}
			}
		}
	}

	return names
}
