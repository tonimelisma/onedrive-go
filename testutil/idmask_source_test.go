package testutil

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

// The tests of this file hold each position mask to the product text it
// follows, read from the product's source: a reworded message, a renamed key
// or a new route then fails a test here instead of printing an identifier.

// Invented values no role names and of no identifier's shape, so only a
// position mask can mask them; an address is of the address shape, which
// masks it anywhere, so a sharer label is held with an invented name.
const (
	inventedPositionAddress = "nobody.position@example.test"
	inventedPositionSharer  = "Nobody Position"
	inventedPositionDrive   = "inventedpositiondrive"
	inventedPositionItem    = "inventedpositionitem"
	// inventedPathName stands where a route format takes a path or a name.
	inventedPathName = "e2e-path-name"
	// replaceAllArgCount is the argument count of strings.ReplaceAll, and
	// writefArgCount that of a writef that prints one value.
	replaceAllArgCount = 3
	writefArgCount     = 3
)

// productPath is the path, from this package's directory, of the file parts
// name from the repository root.
func productPath(parts ...string) string {
	return filepath.Join(append([]string{".."}, parts...)...)
}

// readProductSource is the text of the product source file at path.
func readProductSource(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	return string(data), nil
}

func parseProductFile(t *testing.T, parts ...string) *ast.File {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), productPath(parts...), nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	return file
}

// productFunc is the function or method named name of file.
func productFunc(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	require.FailNowf(t, "function not found", "%s: the mask follows its text: follow its new name", name)

	return nil
}

// productConstant is the value of the string constant named name of file.
func productConstant(t *testing.T, file *ast.File, name string) string {
	t.Helper()

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			values, isValues := spec.(*ast.ValueSpec)
			if !isValues {
				continue
			}
			for i, ident := range values.Names {
				if ident.Name == name && i < len(values.Values) {
					return stringLiteral(t, values.Values[i])
				}
			}
		}
	}
	require.FailNowf(t, "constant not found", "%s: the mask follows its value: follow its new name", name)

	return ""
}

func stringLiteral(t *testing.T, expr ast.Expr) string {
	t.Helper()

	literal, ok := expr.(*ast.BasicLit)
	require.Truef(t, ok && literal.Kind == token.STRING, "not a string literal: %T", expr)
	value, err := strconv.Unquote(literal.Value)
	require.NoError(t, err)

	return value
}

// isSelectorNamed reports a selector expression whose selected name is one of
// names.
func isSelectorNamed(expr ast.Expr, names ...string) bool {
	selector, ok := expr.(*ast.SelectorExpr)

	return ok && slices.Contains(names, selector.Sel.Name)
}

// assertMasked asserts that none of values is left in what roles make of
// text, naming the product text it stands for.
func assertMasked(t *testing.T, roles DriveRoles, what, text string, values ...string) {
	t.Helper()

	redacted := roles.Redact(text)
	for _, value := range values {
		assert.NotContainsf(t, redacted, value, "%s: %q is printed as %q", what, text, redacted)
	}
}

// Validates: R-6.10.27
func TestSelectorMaskFollowsTheProductsSelector(t *testing.T) {
	t.Parallel()

	selector := sharedref.Ref{
		AccountEmail: inventedPositionAddress, RemoteDriveID: inventedPositionDrive, RemoteItemID: inventedPositionItem,
	}.String()
	assertMasked(t, plantedSuiteRoles(), "a shared selector", "drive add "+selector,
		inventedPositionAddress, inventedPositionDrive, inventedPositionItem)
}

// A shared drive's state database is named by its canonical ID, its colons
// replaced (internal/config's DriveStatePathForDataDir).
//
// Validates: R-6.10.27
func TestStateFileNameMaskFollowsTheProductsStatePath(t *testing.T) {
	t.Parallel()

	configFile := parseProductFile(t, "internal", "config", "drive.go")
	var replaced, replacement string
	ast.Inspect(productFunc(t, configFile, "DriveStatePathForDataDir"), func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && isSelectorNamed(call.Fun, "ReplaceAll") && len(call.Args) == replaceAllArgCount {
			replaced, replacement = stringLiteral(t, call.Args[1]), stringLiteral(t, call.Args[2])
		}

		return true
	})
	require.NotEmpty(t, replaced, "DriveStatePathForDataDir no longer replaces the canonical ID's separator")

	driveIDFile := parseProductFile(t, "internal", "driveid", "canonical.go")
	source, err := readProductSource(productPath("internal", "driveid", "canonical.go"))
	require.NoError(t, err)
	assert.Contains(t, source, `c.driveType + ":" + c.email + ":" + c.sourceDriveID + ":" + c.sourceItemID`,
		"a shared canonical ID is spelled another way: give fileNameSelectorPattern its new form")

	canonicalID := strings.Join([]string{
		productConstant(t, driveIDFile, "DriveTypeShared"), inventedPositionAddress, inventedPositionDrive, inventedPositionItem,
	}, ":")
	fileName := productConstant(t, configFile, "stateDBPrefix") + strings.ReplaceAll(canonicalID, replaced, replacement) +
		productConstant(t, configFile, "stateDBSuffix")
	assertMasked(t, plantedSuiteRoles(), "a shared drive's state database",
		"open /tmp/onedrive-go/"+fileName+": permission denied",
		inventedPositionAddress, inventedPositionDrive, inventedPositionItem)
}

// routeFormats are the routes internal/graph requests: every string literal
// that starts with a / and takes a verb, and every one that ends in a / and
// is joined to the value after it.
func routeFormats(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob(productPath("internal", "graph", "*.go"))
	require.NoError(t, err)
	var formats []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		ast.Inspect(file, func(node ast.Node) bool {
			formats = append(formats, routeFormatOf(t, node)...)

			return true
		})
	}
	require.NotEmpty(t, formats, "internal/graph holds no route format")

	return formats
}

// routeFormatOf is the route format node spells, if any: a string literal
// that starts with a / and takes a verb, or the join of a literal that starts
// and ends with a / to the value after it.
func routeFormatOf(t *testing.T, node ast.Node) []string {
	t.Helper()

	if join, ok := node.(*ast.BinaryExpr); ok && join.Op == token.ADD {
		if literal, isLiteral := join.X.(*ast.BasicLit); isLiteral && literal.Kind == token.STRING {
			value := stringLiteral(t, literal)
			if len(value) > len("/") && strings.HasPrefix(value, "/") && strings.HasSuffix(value, "/") {
				return []string{value + "%s"}
			}
		}
	}
	if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
		if value := stringLiteral(t, literal); strings.HasPrefix(value, "/") && strings.Contains(value, "%") {
			return []string{value}
		}
	}

	return nil
}

// renderRoute fills a route format: a %s right after a / stands for an
// identifier, unless it is in a path address (between :/ and its closing :),
// and it gets an invented value of its own; every other %s gets a name and
// %d a number. Any other verb is an error, since what it renders is not
// known to be masked.
func renderRoute(format string) (route string, identifiers []string, err error) {
	var rendered strings.Builder
	inPathAddress := false
	for i := 0; i < len(format); i++ {
		switch {
		case strings.HasPrefix(format[i:], ":/"):
			inPathAddress = true
			rendered.WriteString(":/")
			i++
		case format[i] == ':' && inPathAddress:
			inPathAddress = false
			rendered.WriteByte(':')
		case strings.HasPrefix(format[i:], "%s"):
			value := inventedPathName
			if !inPathAddress && i > 0 && format[i-1] == '/' {
				value = fmt.Sprintf("inventedroutevalue%d", len(identifiers))
				identifiers = append(identifiers, value)
			}
			rendered.WriteString(value)
			i++
		case strings.HasPrefix(format[i:], "%d"):
			rendered.WriteString("7")
			i++
		case format[i] == '%':
			return "", nil, fmt.Errorf("route %q: the verb that starts %q is neither %%s nor %%d", format, format[i:])
		default:
			rendered.WriteByte(format[i])
		}
	}

	return rendered.String(), identifiers, nil
}

// A request's debug record names its route (internal/graph's path attribute),
// in text and in JSON, and a transport error the URL.
//
// Validates: R-6.10.27
func TestRouteMaskFollowsEveryProductRoute(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()
	identifierCount := 0
	for _, format := range routeFormats(t) {
		route, identifiers, err := renderRoute(format)
		require.NoError(t, err, "a route takes a verb renderRoute does not render: render it, as an identifier after a /")
		identifierCount += len(identifiers)
		for _, text := range []string{
			"level=DEBUG msg=request path=" + route,
			`{"level":"DEBUG","msg":"request","path":"` + route + `"}`,
			"graph: GET https://graph.microsoft.com/v1.0" + route + ": context deadline exceeded",
		} {
			assertMasked(t, roles, "route "+format, text, identifiers...)
		}
	}
	assert.Positive(t, identifierCount, "no route of internal/graph names an identifier")
}

// identifierKeys are the keys isIdentifierKey names one by one.
func identifierKeys(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "idmask.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	var keys []string
	ast.Inspect(productFunc(t, file, "isIdentifierKey"), func(node ast.Node) bool {
		clause, ok := node.(*ast.CaseClause)
		if !ok || len(clause.Body) != 1 {
			return true
		}
		if returned, isReturn := clause.Body[0].(*ast.ReturnStmt); isReturn && len(returned.Results) == 1 {
			if result, isIdent := returned.Results[0].(*ast.Ident); isIdent && result.Name == "true" {
				for _, key := range clause.List {
					keys = append(keys, stringLiteral(t, key))
				}
			}
		}

		return true
	})
	require.NotEmpty(t, keys, "isIdentifierKey names no key")

	return keys
}

// productGoSources is the text of every non-test Go file under internal,
// the sync tree's test-helper packages left out.
func productGoSources(t *testing.T) string {
	t.Helper()

	var sources strings.Builder
	err := filepath.WalkDir(productPath("internal"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// the sync tree's test-helper packages are test code outside _test.go files
			if name := filepath.ToSlash(path); strings.HasSuffix(name, "/internal/sync/internal/syncfixture") ||
				strings.HasSuffix(name, "/internal/sync/internal/syncstoretest") {
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := readProductSource(path)
		sources.WriteString(source)

		return readErr
	})
	require.NoError(t, err)

	return sources.String()
}

// Every key the JSON field and log attribute masks name one by one is a key
// the product writes, as a JSON tag (of its --json output or of a Graph
// answer it decodes) or a log attribute's key, and each key's writers are
// counted. A writer the product renames is no longer masked, so even one of
// a key's several writers changes its count and fails here.
//
// Validates: R-6.10.27
func TestIdentifierKeysFollowTheProductsKeys(t *testing.T) {
	t.Parallel()

	sources := productGoSources(t)
	writers := map[string]int{}
	for _, key := range identifierKeys(t) {
		quoted := regexp.QuoteMeta(key)
		// Each pattern starts with a literal, which keeps a scan of the
		// whole tree fast.
		for _, pattern := range []string{`json:"` + quoted + `[,"]`, `slog\.[A-Za-z]+\("` + quoted + `"`} {
			writers[key] += len(regexp.MustCompile(pattern).FindAllStringIndex(sources, -1))
		}
	}
	assert.Equal(t, map[string]int{
		"id": 27, "email": 6, "etag": 1, "remote_drive": 1, "owner_name": 1, "shared_by_name": 1,
		"shared_owner_name": 1, "org_name": 2, "site_name": 4, "library_name": 3,
	}, writers, "the JSON tags and log attributes that write a masked key changed: mask the key a renamed "+
		"writer writes now, then give the key its new count here")
}

// A throttle scope key and a throttle target name a drive (internal/sync's
// scope keys).
//
// Validates: R-6.10.27
func TestThrottleKeyMaskFollowsTheProductsScopeKey(t *testing.T) {
	t.Parallel()

	file := parseProductFile(t, "internal", "sync", "syncdomain", "scope_key.go")
	target := productConstant(t, file, "throttleDrivePrefix") + inventedPositionDrive
	roles := plantedSuiteRoles()
	assertMasked(t, roles, "a throttle scope key",
		"scope_key="+productConstant(t, file, "wireThrottleTarget")+target, inventedPositionDrive)
	assertMasked(t, roles, "a throttle target", "throttled target="+target, inventedPositionDrive)
}

// stat's text output names the item on a line of its own (internal/cli's
// printStatText).
//
// Validates: R-6.10.27
func TestStatIDLineMaskFollowsStatsOutput(t *testing.T) {
	t.Parallel()

	var formats []string
	ast.Inspect(productFunc(t, parseProductFile(t, "internal", "cli", "stat.go"), "printStatText"), func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && len(call.Args) == writefArgCount && isSelectorNamed(call.Args[2], "ID") {
			formats = append(formats, stringLiteral(t, call.Args[1]))
		}

		return true
	})
	require.Len(t, formats, 1, "printStatText prints the item ID once")
	assertMasked(t, plantedSuiteRoles(), "stat's ID line",
		"Name:     report.txt\n"+fmt.Sprintf(formats[0], inventedPositionItem)+"Size:     7 bytes\n", inventedPositionItem)
}

// sharerFields are the fields that hold a sharer's name or address.
func sharerFields() []string {
	return []string{"OwnerEmail", "OwnerName", "SharedByEmail", "SharedByName"}
}

// namesSharer reports an argument that is a sharer field, or a call given
// one (shared discovery's extractFirstName(item.SharedByName)).
func namesSharer(arg ast.Expr) bool {
	if isSelectorNamed(arg, sharerFields()...) {
		return true
	}
	call, ok := arg.(*ast.CallExpr)

	return ok && slices.ContainsFunc(call.Args, func(inner ast.Expr) bool {
		return isSelectorNamed(inner, sharerFields()...)
	})
}

// sharerLabels are the labels fn prints a sharer in, a literal joined to the
// sharer or a format given it: masked those under the text sharedByPattern
// starts with, filled with an invented name, and unmasked the literal or
// format of every other one.
func sharerLabels(t *testing.T, fn *ast.FuncDecl) (masked, unmasked []string) {
	t.Helper()

	sharedBy, _ := sharedByPattern.LiteralPrefix()
	add := func(format, label string) {
		if strings.Contains(label, sharedBy) {
			masked = append(masked, label)
		} else {
			unmasked = append(unmasked, format)
		}
	}
	ast.Inspect(fn, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BinaryExpr:
			if literal, ok := node.X.(*ast.BasicLit); ok && literal.Kind == token.STRING && namesSharer(node.Y) {
				add(stringLiteral(t, literal), stringLiteral(t, literal)+inventedPositionSharer)
			}
		case *ast.CallExpr:
			if !isSelectorNamed(node.Fun, "Sprintf") || len(node.Args) < 2 {
				return true
			}
			args := make([]any, 0, len(node.Args)-1)
			isSharerLabel := false
			for _, arg := range node.Args[1:] {
				value := "Planted Folder"
				if namesSharer(arg) {
					value, isSharerLabel = inventedPositionSharer, true
				}
				args = append(args, value)
			}
			if isSharerLabel {
				format := stringLiteral(t, node.Args[0])
				add(format, fmt.Sprintf(format, args...))
			}
		}

		return true
	})

	return masked, unmasked
}

// drive list labels a shared drive with its sharer, and drive add and shared
// discovery name a folder by it (internal/cli). A label outside the shared-by
// text is held by its format: no position mask covers the display names
// shared discovery gives a folder, the sharer's first or full name before the
// folder's, with the address after it in the last one; the learned names and
// the address shape mask those (TestSharedDisplayNamesAreMaskedInEveryForm).
//
// Validates: R-6.10.27
func TestSharedByMaskFollowsTheProductsLabels(t *testing.T) {
	t.Parallel()

	roles := plantedSuiteRoles()
	for _, site := range []struct {
		file, function string
		labelCount     int
		unmasked       []string
	}{
		{file: "drive_list_render.go", function: "driveListSharedOwnerLabel", labelCount: 2},
		{
			file: "shared_discovery.go", function: "deriveSharedDisplayName", labelCount: 1,
			unmasked: []string{"%s's %s", "%s's %s", "%s's %s (%s)"},
		},
		{file: "drive_add.go", function: "addSharedDriveByName", labelCount: 1},
	} {
		labels, unmasked := sharerLabels(t, productFunc(t, parseProductFile(t, "internal", "cli", site.file), site.function))
		require.Lenf(t, labels, site.labelCount,
			"%s's shared-by labels changed: give sharedByPattern their new text, then their count here", site.function)
		for _, label := range labels {
			assertMasked(t, roles, site.function+"'s label", "  Planted Folder  "+label+"\n", inventedPositionSharer)
		}
		assert.Equalf(t, site.unmasked, unmasked,
			"%s prints a sharer in a label no mask covers: mask it, or name its format here", site.function)
	}
}

// A sharer the catalog holds and its first name, and an address no catalog
// holds, all invented.
const (
	plantedSharerName    = "Rosalind Sharer"
	plantedSharerFirst   = "Rosalind"
	outsideSharerAddress = "quiet.outsider@example.org"
	plantedSharedFolder  = "Holiday Folder"
)

// sharedDisplayNames are the display names deriveSharedDisplayName builds
// for a sharer, each format read from its source and filled as the product
// fills it: the sharer's name, its first name (a call given the name), its
// address, and the folder's name for every other argument.
func sharedDisplayNames(t *testing.T, fn *ast.FuncDecl) []string {
	t.Helper()

	var names []string
	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isSelectorNamed(call.Fun, "Sprintf") || len(call.Args) < 2 {
			return true
		}
		args := make([]any, 0, len(call.Args)-1)
		isSharerName := false
		for _, arg := range call.Args[1:] {
			var value string
			switch {
			case isSelectorNamed(arg, "SharedByName"):
				value, isSharerName = plantedSharerName, true
			case isSelectorNamed(arg, "SharedByEmail"):
				value, isSharerName = outsideSharerAddress, true
			case namesSharer(arg):
				value, isSharerName = sharerFirstName(plantedSharerName), true
			default:
				value = plantedSharedFolder
			}
			args = append(args, value)
		}
		if isSharerName {
			names = append(names, fmt.Sprintf(stringLiteral(t, call.Args[0]), args...))
		}

		return true
	})

	return names
}

// Shared discovery takes a sharer's first name as the text before the
// first space of its display name (internal/cli's extractFirstName), and the
// masks learn it by the same rule.
//
// Validates: R-6.10.27
func TestSharerFirstNameFollowsTheProductsRule(t *testing.T) {
	t.Parallel()

	source, err := readProductSource(productPath("internal", "cli", "shared_discovery.go"))
	require.NoError(t, err)
	assert.Contains(t, source,
		"func extractFirstName(fullName string) string {\n\tif i := strings.Index(fullName, \" \"); i > 0 {\n"+
			"\t\treturn fullName[:i]\n\t}\n\n\treturn fullName\n}",
		"shared discovery forms a first name another way: give sharerFirstName its new rule")
	assert.Equal(t, plantedSharerFirst, sharerFirstName(plantedSharerName))
	assert.Equal(t, "Mononym", sharerFirstName("Mononym"))
}

// Every display name shared discovery gives a folder by its sharer prints
// neither the sharer's name, nor its first name, nor its address: the names
// a catalog account's display name teaches (AccountNameIdentities) and the
// address shape, which masks an address no catalog holds.
//
// Validates: R-6.10.27
func TestSharedDisplayNamesAreMaskedInEveryForm(t *testing.T) {
	t.Parallel()

	roles := NewSuiteRoles(nil, AccountNameIdentities(plantedSharerName, "drive-1-account"))
	names := sharedDisplayNames(t, productFunc(t,
		parseProductFile(t, "internal", "cli", "shared_discovery.go"), "deriveSharedDisplayName"))
	require.Lenf(t, names, 4,
		"deriveSharedDisplayName's sharer forms changed: hold each new form to a mask, then its count here")
	for _, name := range names {
		assertMasked(t, roles, "a shared folder's display name", "  "+name+"\n",
			plantedSharerName, plantedSharerFirst, outsideSharerAddress)
		assert.Contains(t, roles.Redact(name), plantedSharedFolder, "the folder's own name stays readable")
	}
}

// useOf is the Use of the command the function named name of file builds.
func useOf(t *testing.T, file *ast.File, name string) string {
	t.Helper()

	var use string
	ast.Inspect(productFunc(t, file, name), func(node ast.Node) bool {
		field, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, isIdent := field.Key.(*ast.Ident); isIdent && key.Name == "Use" {
			use = stringLiteral(t, field.Value)
		}

		return true
	})
	require.NotEmptyf(t, use, "%s builds no command", name)

	return use
}

// A command line restores an item by its ID (internal/cli's recycle-bin
// restore).
//
// Validates: R-6.10.27
func TestRestoreArgumentMaskFollowsTheProductsCommand(t *testing.T) {
	t.Parallel()

	file := parseProductFile(t, "internal", "cli", "recycle_bin.go")
	restore, _, _ := strings.Cut(useOf(t, file, "newRecycleBinRestoreCmd"), " ")
	commandLine := strings.Join([]string{"onedrive-go", useOf(t, file, "newRecycleBinCmd"), restore, inventedPositionItem}, " ")
	assertMasked(t, plantedSuiteRoles(), "recycle-bin restore's argument", "CMD: "+commandLine+"\n", inventedPositionItem)
}

// A sharing link's share token is learned as the link's role in the spelling
// internal/graph's encodeSharingURL gives it.
//
// Validates: R-6.10.27
func TestShareTokenFollowsTheProductsEncoding(t *testing.T) {
	t.Parallel()

	assert.Equal(t, graphShareTokenPrefix,
		productConstant(t, parseProductFile(t, "internal", "graph", "shares.go"), "graphSharingURLPrefix"),
		"the share token's prefix changed: give graphShareTokenPrefix its new value")
	source, err := readProductSource(productPath("internal", "graph", "shares.go"))
	require.NoError(t, err)
	assert.Contains(t, source,
		"return graphSharingURLPrefix + base64.RawURLEncoding.EncodeToString([]byte(parsed.String())), nil",
		"the share token is encoded another way: give linkIdentities its new encoding")
}
