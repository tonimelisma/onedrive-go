package localpath

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// folderKind is what a folder the client creates holds, which decides the
// mode it asks for.
type folderKind string

const (
	// userTreeFolder is a folder in a user's tree: below the sync folder, in
	// a get target tree, or a restored file's missing parent. It asks for
	// 0777 and the kernel applies the umask, as for a new file.
	userTreeFolder folderKind = "a folder in a user's tree"
	// syncRootFolder is the sync folder the client creates on its own
	// initiative. It is private, 0700.
	syncRootFolder folderKind = "the sync folder"
	// managedStateFolder holds the client's own state: config, sign-ins,
	// sync state, locks, logs, trash, upload sessions, the control socket,
	// performance captures.
	managedStateFolder folderKind = "managed state"
)

// classifiedFolderModes is every constant product code may name as the mode
// of a folder it creates, keyed by package directory and name. A folder in a
// user's tree has one mode and one home for it, UserTreeDirPerm; the sync
// folder and each kind of managed state keep their own names.
func classifiedFolderModes() map[string]folderKind {
	return map[string]folderKind{
		userTreeModeHome:                          userTreeFolder,
		"internal/cli.syncRootDirPerms":           syncRootFolder,
		"internal/cli.tokenDirPerms":              managedStateFolder,
		"internal/config.configDirPermissions":    managedStateFolder,
		"internal/driveops.sessionDirPerms":       managedStateFolder,
		"internal/localtrash.entryDirPerm":        managedStateFolder,
		"internal/logfile.dirPerm":                managedStateFolder,
		"internal/multisync.controlSocketDirPerm": managedStateFolder,
		"internal/perf.captureDirPerm":            managedStateFolder,
		"internal/sync.engineLocksDirPerm":        managedStateFolder,
		"internal/sync.syncStoreDirPerm":          managedStateFolder,
		"internal/synctree.detachDirectoryPerm":   managedStateFolder,
		"internal/tokenfile.DirPerms":             managedStateFolder,
	}
}

const (
	// userTreeModeHome names the one constant a user-tree folder may ask for.
	userTreeModeHome = "internal/localpath.UserTreeDirPerm"
	modulePath       = "github.com/tonimelisma/onedrive-go/"
	// repositoryRoot is the module root, seen from this package's folder.
	repositoryRoot = "../.."
	// syncTreePackage holds the sync tree, whose Root creates every folder
	// below a sync folder.
	syncTreePackage = "internal/synctree"
	syncTreeRoot    = "Root"
	// stdlibCreatorArity is the argument count of os.Mkdir, os.MkdirAll and
	// the os.Root methods of those names: a name and a mode.
	stdlibCreatorArity = 2
	// fieldReceiver marks a function literal stored in a struct field, which
	// a call reaches like a method.
	fieldReceiver = "(field)"
	// unnamedReceiver marks a method whose receiver type this scan does not
	// spell out; it is still a method.
	unnamedReceiver = "(receiver)"
)

// syncTreeFolderCreators are the functions that make every folder the sync
// tree creates: Root.MkdirAll, and ensureNoFollowDirectory for each folder
// Root.MkdirAllNoFollow makes.
func syncTreeFolderCreators() []string {
	return []string{
		syncTreePackage + "." + syncTreeRoot + ".MkdirAll",
		syncTreePackage + ".ensureNoFollowDirectory",
	}
}

// folderCreatingNames are the calls that create a folder whose mode is their
// last argument, as os spells them. A function that passes one of its own
// parameters on as such a mode creates folders too, and its callers are
// checked at that parameter.
func folderCreatingNames() []string {
	return []string{"Mkdir", "MkdirAll", "MkdirAllNoFollow"}
}

// productFile is one parsed production Go file.
type productFile struct {
	rel     string // slash path from the repository root
	pkgDir  string // slash path of its folder from the repository root
	file    *ast.File
	fset    *token.FileSet
	imports map[string]importedPackage
}

func (f *productFile) position(pos token.Pos) string {
	return fmt.Sprintf("%s:%d", f.rel, f.fset.Position(pos).Line)
}

// visibleDirs are the package directories of this module a call in the file
// can reach: its own and those it imports.
func (f *productFile) visibleDirs() []string {
	dirs := []string{f.pkgDir}
	for _, imported := range f.imports {
		if imported.inModule {
			dirs = append(dirs, imported.dir)
		}
	}

	return dirs
}

// importedPackage is an import of a file: a package directory of this module,
// or the import path of any other package.
type importedPackage struct {
	dir      string
	inModule bool
}

// declaration is a function a call can reach by name: a function, a method,
// or a function literal stored in a struct field.
type declaration struct {
	pkgDir string
	recv   string // the receiver type of a method, fieldReceiver, or "" for a function
	name   string
	arity  int
}

func (d declaration) qualifiedName() string {
	if d.recv == "" {
		return d.pkgDir + "." + d.name
	}

	return d.pkgDir + "." + d.recv + "." + d.name
}

// creatorIndex knows the product code's declarations, and which of them
// pass a parameter on as a folder's mode.
type creatorIndex struct {
	byName  map[string][]declaration
	modeArg map[declaration]int
}

// folderModeUse is one folder-creating call whose mode is not its own
// function's parameter.
type folderModeUse struct {
	at       string // file:line
	mode     string // package directory and constant, or "" when the mode is no named constant
	expr     string // the mode argument as written
	function string // the named function the call is in
}

// Every folder product code creates names its mode with a constant this test
// classifies, so a folder in a user's tree cannot get a mode of its own: a
// literal, an unclassified constant or a computed mode fails, and each
// classified constant must still be in use.
//
// Validates: R-5.10.2
func TestFolderModes_EveryCreatedFolderNamesAClassifiedMode(t *testing.T) {
	t.Parallel()

	files := parseProductFiles(t, repositoryRoot)
	uses := folderModeUses(files, newCreatorIndex(files))
	require.NotEmpty(t, uses, "the scan found no folder creation at all")

	classified := classifiedFolderModes()
	inUse := make(map[string]bool, len(classified))
	var problems []string
	for _, use := range uses {
		switch _, known := classified[use.mode]; {
		case use.mode == "":
			problems = append(problems, fmt.Sprintf("%s: folder mode %s is not a named constant", use.at, use.expr))
		case !known:
			problems = append(problems, fmt.Sprintf("%s: folder mode %s is not classified", use.at, use.mode))
		default:
			inUse[use.mode] = true
		}
	}
	for name := range classified {
		if !inUse[name] {
			problems = append(problems, name+" is classified, but no folder creation uses it")
		}
	}
	slices.Sort(problems)

	assert.Empty(t, problems, "A folder in a user's tree (below the sync folder, a get target tree, "+
		"a restored file's parents) asks for localpath.UserTreeDirPerm; the sync folder and managed "+
		"state use their own named constant, listed with its kind in classifiedFolderModes.")
}

// The sync tree creates every folder below a sync folder, and every one of
// them is a folder in a user's tree: no method of synctree.Root takes a mode
// from its caller, and the functions that make its folders ask for the
// user-tree mode and nothing else.
//
// Validates: R-5.10.2
func TestFolderModes_TheSyncTreeCreatesOnlyUserTreeFolders(t *testing.T) {
	t.Parallel()

	files := parseProductFiles(t, repositoryRoot)
	index := newCreatorIndex(files)

	for creator := range index.modeArg {
		assert.False(t, creator.pkgDir == syncTreePackage && creator.recv == syncTreeRoot,
			"%s takes a folder mode from its caller", creator.qualifiedName())
	}

	byFunction := make(map[string][]folderModeUse)
	for _, use := range folderModeUses(files, index) {
		byFunction[use.function] = append(byFunction[use.function], use)
	}
	for _, creator := range syncTreeFolderCreators() {
		require.NotEmpty(t, byFunction[creator], "%s creates no folder any more", creator)
		for _, use := range byFunction[creator] {
			assert.Equal(t, userTreeModeHome, use.mode, "%s: %s", use.at, use.expr)
		}
	}
}

// Validates: R-5.10.2
func TestFolderModes_OneHomeForTheUserTreeMode(t *testing.T) {
	t.Parallel()

	var userTree []string
	for name, kind := range classifiedFolderModes() {
		if kind == userTreeFolder {
			userTree = append(userTree, name)
		}
	}
	assert.Equal(t, []string{userTreeModeHome}, userTree)
	assert.Equal(t, UserTreeDirPerm, os.FileMode(0o777), "the umask alone decides, as for a new file")
}

// parseProductFiles parses every production Go file: main.go, cmd/,
// internal/ and pkg/, without the repository's own tooling (devtool) and
// without tests.
func parseProductFiles(t *testing.T, root string) []productFile {
	t.Helper()

	fset := token.NewFileSet()
	var files []productFile
	for _, top := range []string{"main.go", "cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return fmt.Errorf("relativize %s: %w", path, relErr)
			}
			rel = filepath.ToSlash(rel)
			if entry.IsDir() {
				if rel == "internal/devtool" || rel == "cmd/devtool" || entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			parsed, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				return fmt.Errorf("parse %s: %w", rel, parseErr)
			}
			files = append(files, productFile{
				rel:     rel,
				pkgDir:  filepath.ToSlash(filepath.Dir(rel)),
				file:    parsed,
				fset:    fset,
				imports: fileImports(parsed),
			})
			return nil
		})
		require.NoError(t, err)
	}

	return files
}

// fileImports maps each import name of a file to its package.
func fileImports(file *ast.File) map[string]importedPackage {
	imports := make(map[string]importedPackage, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		dir, inModule := strings.CutPrefix(importPath, modulePath)
		imports[name] = importedPackage{dir: dir, inModule: inModule}
	}

	return imports
}

// newCreatorIndex collects the declarations, then finds to a fixed point
// every one that passes a parameter on as a folder's mode, starting from the
// folder-creating names.
func newCreatorIndex(files []productFile) *creatorIndex {
	index := &creatorIndex{byName: make(map[string][]declaration), modeArg: make(map[declaration]int)}
	for i := range files {
		walkCalls(&files[i], func(*ast.CallExpr, []enclosingFunction) {}, func(decl declaration) {
			index.byName[decl.name] = append(index.byName[decl.name], decl)
		})
	}
	for {
		added := false
		for i := range files {
			file := &files[i]
			walkCalls(file, func(call *ast.CallExpr, functions []enclosingFunction) {
				for _, arg := range index.modeArguments(call, file) {
					function, param, isParam := parameterNamed(functions, call.Args[arg])
					if !isParam || function.decl == nil {
						continue
					}
					if _, known := index.modeArg[*function.decl]; !known {
						index.modeArg[*function.decl] = param
						added = true
					}
				}
			}, nil)
		}
		if !added {
			return index
		}
	}
}

// modeArguments returns which arguments of a call are folder modes. Calls
// carry no type information here, so a call is resolved by its name, its
// argument count and the packages its file can reach, and an ambiguous call
// is checked at every argument that could be a mode.
func (index *creatorIndex) modeArguments(call *ast.CallExpr, file *productFile) []int {
	arity := len(call.Args)
	if arity == 0 {
		return nil
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return index.modesOf(index.candidates(fun.Name, arity, false, []string{file.pkgDir}))
	case *ast.SelectorExpr:
		if pkg, isIdent := fun.X.(*ast.Ident); isIdent {
			if imported, isImport := file.imports[pkg.Name]; isImport {
				return index.packageCallModes(fun.Sel.Name, arity, imported)
			}
		}
		return index.methodCallModes(fun.Sel.Name, arity, file)
	default:
		return nil
	}
}

func (index *creatorIndex) packageCallModes(name string, arity int, imported importedPackage) []int {
	if !imported.inModule {
		if slices.Contains(folderCreatingNames(), name) {
			return []int{arity - 1}
		}
		return nil
	}

	return index.modesOf(index.candidates(name, arity, false, []string{imported.dir}))
}

func (index *creatorIndex) methodCallModes(name string, arity int, file *productFile) []int {
	candidates := index.candidates(name, arity, true, file.visibleDirs())
	if len(candidates) == 0 {
		candidates = index.candidates(name, arity, true, nil)
	}
	modes := index.modesOf(candidates)
	isCreatingName := slices.Contains(folderCreatingNames(), name)
	if isCreatingName && (arity == stdlibCreatorArity || len(candidates) == 0) && !slices.Contains(modes, arity-1) {
		modes = append(modes, arity-1)
	}

	return modes
}

// candidates are the declarations a call of that name and argument count
// can reach: methods or functions, in dirs (nil: anywhere).
func (index *creatorIndex) candidates(name string, arity int, method bool, dirs []string) []declaration {
	var found []declaration
	for _, decl := range index.byName[name] {
		if decl.arity != arity || (decl.recv != "") != method {
			continue
		}
		if dirs == nil || slices.Contains(dirs, decl.pkgDir) {
			found = append(found, decl)
		}
	}

	return found
}

func (index *creatorIndex) modesOf(candidates []declaration) []int {
	var modes []int
	for _, decl := range candidates {
		if mode, isCreator := index.modeArg[decl]; isCreator && !slices.Contains(modes, mode) {
			modes = append(modes, mode)
		}
	}

	return modes
}

// enclosingFunction is a function declaration or literal the walk is inside;
// decl is nil for a literal a call cannot reach by name.
type enclosingFunction struct {
	decl   *declaration
	params []string
}

// parameterNamed finds the innermost enclosing function that has the
// identifier arg as a parameter.
func parameterNamed(functions []enclosingFunction, arg ast.Expr) (enclosingFunction, int, bool) {
	ident, isIdent := arg.(*ast.Ident)
	if !isIdent {
		return enclosingFunction{}, 0, false
	}
	for i := len(functions) - 1; i >= 0; i-- {
		if index := slices.Index(functions[i].params, ident.Name); index >= 0 {
			return functions[i], index, true
		}
	}

	return enclosingFunction{}, 0, false
}

// innermostNamed is the qualified name of the innermost function a call can
// reach by name, or "" at package level.
func innermostNamed(functions []enclosingFunction) string {
	for i := len(functions) - 1; i >= 0; i-- {
		if functions[i].decl != nil {
			return functions[i].decl.qualifiedName()
		}
	}

	return ""
}

// folderModeUses lists every folder-creating call whose mode is not one of
// its own function's parameters, with the constant it names.
func folderModeUses(files []productFile, index *creatorIndex) []folderModeUse {
	var uses []folderModeUse
	for i := range files {
		file := &files[i]
		walkCalls(file, func(call *ast.CallExpr, functions []enclosingFunction) {
			for _, arg := range index.modeArguments(call, file) {
				mode := call.Args[arg]
				if _, _, isParam := parameterNamed(functions, mode); isParam {
					continue
				}
				uses = append(uses, folderModeUse{
					at:       file.position(call.Pos()),
					mode:     namedConstant(mode, file),
					expr:     types.ExprString(mode),
					function: innermostNamed(functions),
				})
			}
		}, nil)
	}

	return uses
}

// namedConstant is "<package dir>.<name>" for a bare name or a selector on an
// imported package, and "" for anything else.
func namedConstant(mode ast.Expr, file *productFile) string {
	switch expr := mode.(type) {
	case *ast.Ident:
		return file.pkgDir + "." + expr.Name
	case *ast.SelectorExpr:
		pkg, isIdent := expr.X.(*ast.Ident)
		if !isIdent {
			return ""
		}
		imported, isImport := file.imports[pkg.Name]
		if !isImport {
			return ""
		}
		return imported.dir + "." + expr.Sel.Name
	default:
		return ""
	}
}

// walkCalls calls visit for every call in file with the functions it sits
// in, outermost first, and declared (when not nil) for every declaration a
// call can reach by name.
func walkCalls(
	file *productFile,
	visit func(call *ast.CallExpr, functions []enclosingFunction),
	declared func(decl declaration),
) {
	var stack []ast.Node
	var functions []enclosingFunction
	ast.Inspect(file.file, func(node ast.Node) bool {
		if node == nil {
			if isFunctionNode(stack[len(stack)-1]) {
				functions = functions[:len(functions)-1]
			}
			stack = stack[:len(stack)-1]
			return true
		}
		if function, isFunction := functionOf(node, stack, file.pkgDir); isFunction {
			functions = append(functions, function)
			if declared != nil && function.decl != nil {
				declared(*function.decl)
			}
		}
		stack = append(stack, node)
		if call, isCall := node.(*ast.CallExpr); isCall {
			visit(call, functions)
		}
		return true
	})
}

func isFunctionNode(node ast.Node) bool {
	switch node.(type) {
	case *ast.FuncDecl, *ast.FuncLit:
		return true
	default:
		return false
	}
}

// functionOf describes a function node: a declaration by its receiver and
// name, a literal by the struct field it is stored in.
func functionOf(node ast.Node, stack []ast.Node, pkgDir string) (enclosingFunction, bool) {
	switch fn := node.(type) {
	case *ast.FuncDecl:
		params := paramNames(fn.Type)
		decl := declaration{pkgDir: pkgDir, recv: receiverName(fn), name: fn.Name.Name, arity: len(params)}
		return enclosingFunction{decl: &decl, params: params}, true
	case *ast.FuncLit:
		params := paramNames(fn.Type)
		function := enclosingFunction{params: params}
		if field := fieldName(fn, stack); field != "" {
			function.decl = &declaration{pkgDir: pkgDir, recv: fieldReceiver, name: field, arity: len(params)}
		}
		return function, true
	default:
		return enclosingFunction{}, false
	}
}

// receiverName is a method's receiver type name, or "" for a function.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	recv := fn.Recv.List[0].Type
	if star, isStar := recv.(*ast.StarExpr); isStar {
		recv = star.X
	}
	if generic, isGeneric := recv.(*ast.IndexExpr); isGeneric {
		recv = generic.X
	}
	if ident, isIdent := recv.(*ast.Ident); isIdent {
		return ident.Name
	}

	return unnamedReceiver
}

// fieldName is the struct field a function literal is stored in, or "".
func fieldName(fn *ast.FuncLit, stack []ast.Node) string {
	if len(stack) == 0 {
		return ""
	}
	if kv, isKeyValue := stack[len(stack)-1].(*ast.KeyValueExpr); isKeyValue && kv.Value == fn {
		if key, isIdent := kv.Key.(*ast.Ident); isIdent {
			return key.Name
		}
	}

	return ""
}

// paramNames lists a function's parameters in order; an unnamed one is "_".
func paramNames(fnType *ast.FuncType) []string {
	var names []string
	for _, field := range fnType.Params.List {
		if len(field.Names) == 0 {
			names = append(names, "_")
			continue
		}
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}

	return names
}
