package syncexec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const localpathImportPath = "github.com/tonimelisma/onedrive-go/internal/localpath"

// changingFunctions are, per package, the functions that change the
// filesystem whatever path they are given: create, write, rename, remove, or
// change a mode or a time. localpath's list, with localpathReadingFunctions,
// declares every function that package exports
// (TestLocalpathFunctions_AreEachDeclaredReadingOrChanging).
func changingFunctions() map[string][]string {
	return map[string][]string{
		localpathImportPath: {
			"AtomicWrite", "Chmod", "Chtimes", "CreateTemp", "MkdirAll", "MkdirTemp",
			"OpenFile", "Remove", "RemoveAll", "Rename", "WriteDisposableFile",
		},
		"os": {
			"Chmod", "Chown", "Chtimes", "CopyFS", "Create", "CreateTemp", "Lchown", "Link", "Mkdir",
			"MkdirAll", "MkdirTemp", "OpenFile", "Remove", "RemoveAll", "Rename", "Symlink",
			"Truncate", "WriteFile",
		},
	}
}

// belowTheSyncTreeImports are the packages an executor file does not import
// at all: io/ioutil's writers and the system calls of syscall and x/sys/unix
// reach the filesystem by path or descriptor beneath the sync tree, so no
// list of their functions would keep up with them.
func belowTheSyncTreeImports() []string {
	return []string{"io/ioutil", "syscall", "golang.org/x/sys/unix"}
}

// localpathReadingFunctions are the localpath functions that leave the
// filesystem as it is. SyncDir flushes a folder's entries; it changes what
// survives a power cut, not what the folder holds.
func localpathReadingFunctions() []string {
	return []string{
		"EvalSymlinks", "IsDirSyncUnsupported", "IsLinkUnsupported", "IsMissingName",
		"JoinOnOneLine", "Lstat", "Open", "OpenDir", "OpenRegular", "ReadDir", "ReadFile",
		"Stat", "SyncDir", "WithoutPath",
	}
}

// changingCall is one call of a changing function in an executor file.
type changingCall struct {
	file     string
	function string
	call     string
}

// Validates: R-6.4.8, R-2.4.6
//
// The executor changes the sync folder only through the rooted sync tree: no
// executor file calls a localpath or os function that changes the filesystem,
// or imports io/ioutil, syscall or x/sys/unix.
// Such a call takes a path, and a path checked beforehand follows a folder
// swapped for a symlink out of the sync folder after the check. An exception
// would be named in allowed with its reason; there is none.
func TestExecutorFiles_ChangeTheFilesystemOnlyThroughTheSyncTree(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("executor*.go")
	require.NoError(t, err)
	var calls []changingCall
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		found, parseErr := changingCallsIn(file)
		require.NoError(t, parseErr)
		calls = append(calls, found...)
		checked++
	}
	require.Greater(t, checked, 3, "the executor's files are read")

	allowed := map[changingCall]string{}
	var unexpected []changingCall
	for _, call := range calls {
		if _, ok := allowed[call]; !ok {
			unexpected = append(unexpected, call)
		}
	}
	assert.Empty(t, unexpected, "the executor changes the sync folder through the sync tree, never by path")
}

// changingCallsIn lists, in one Go file, each import of a
// belowTheSyncTreeImports package (as function "import") and the calls of
// changingFunctions, resolving each package by the name the file imports it
// under.
func changingCallsIn(path string) ([]changingCall, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	changing := changingFunctions()
	imported := map[string][]string{}
	var found []changingCall
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			return nil, fmt.Errorf("reading an import of %s: %w", path, unquoteErr)
		}
		if slices.Contains(belowTheSyncTreeImports(), importPath) {
			found = append(found, changingCall{file: path, function: "import", call: importPath})
			continue
		}
		names, watched := changing[importPath]
		if !watched {
			continue
		}
		localName := filepath.Base(importPath)
		if spec.Name != nil {
			localName = spec.Name.Name
		}
		imported[localName] = names
	}

	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, isSelector := node.(*ast.SelectorExpr)
			if !isSelector {
				return true
			}
			pkg, isIdent := selector.X.(*ast.Ident)
			if !isIdent || !slices.Contains(imported[pkg.Name], selector.Sel.Name) {
				return true
			}
			found = append(found, changingCall{file: path, function: function.Name.Name, call: pkg.Name + "." + selector.Sel.Name})
			return true
		})
	}

	return found, nil
}

// Validates: R-6.4.8
//
// The executor check finds a changing call under any import name and only a
// changing one: a renamed localpath import's Rename, an os.CopyFS and an
// os.Remove are found, a localpath read and an os.DirFS are not, and an
// import of a package beneath the sync tree, syscall here, is found as such.
func TestChangingCallsIn_FindsChangingCallsUnderAnyImportName(t *testing.T) {
	t.Parallel()

	source := `package sample

import (
	"os"
	"syscall"

	lp "` + localpathImportPath + `"
)

func move(a, b string) error {
	if _, err := lp.Stat(a); err != nil {
		return err
	}
	if err := lp.Rename(a, b); err != nil {
		return err
	}
	if err := os.CopyFS(b, os.DirFS(a)); err != nil {
		return err
	}
	return os.Remove(a)
}
`
	path := filepath.Join(t.TempDir(), "sample.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o600))

	found, err := changingCallsIn(path)
	require.NoError(t, err)
	assert.Equal(t, []changingCall{
		{file: path, function: "import", call: "syscall"},
		{file: path, function: "move", call: "lp.Rename"},
		{file: path, function: "move", call: "os.CopyFS"},
		{file: path, function: "move", call: "os.Remove"},
	}, found)
}

// Validates: R-6.4.8
//
// Every function localpath exports is declared as changing the filesystem or
// leaving it as it is, so a new changing function cannot slip past the
// executor check above undeclared.
func TestLocalpathFunctions_AreEachDeclaredReadingOrChanging(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("..", "..", "..", "localpath", "*.go"))
	require.NoError(t, err)
	var exported []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		for _, decl := range file.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.IsExported() {
				exported = append(exported, function.Name.Name)
			}
		}
	}
	require.NotEmpty(t, exported, "localpath's files are read")

	declared := slices.Concat(changingFunctions()[localpathImportPath], localpathReadingFunctions())
	slices.Sort(exported)
	slices.Sort(declared)
	assert.Equal(t, exported, declared, "each localpath function is declared once, as reading or changing")
}
