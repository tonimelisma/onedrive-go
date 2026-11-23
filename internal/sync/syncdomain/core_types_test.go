package syncdomain

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.5.3
//
// Item types are stored as TEXT: each member writes its name and parses back
// as itself; another name is refused, and a value outside the enum names its
// number.
func TestItemType_StoredNamesRoundTrip(t *testing.T) {
	t.Parallel()

	for member, name := range map[ItemType]string{ItemTypeFile: "file", ItemTypeFolder: "folder", ItemTypeRoot: "root"} {
		assert.Equal(t, name, member.String())
		parsed, err := ParseItemType(name)
		require.NoError(t, err, name)
		assert.Equal(t, member, parsed, name)
	}
	assert.Equal(t, "ItemType(99)", ItemType(99).String())
	_, err := ParseItemType("unknown")
	require.ErrorContains(t, err, `unknown item type "unknown"`)
}

// Validates: R-6.5.3
//
// Action type names are a stored format (retry_work.action_type), which
// startup reads back: each member writes its name and scans back as itself.
// The other enums' names are what logs and status print.
func TestEnumStrings_AreTheStoredAndLoggedNames(t *testing.T) {
	t.Parallel()

	for member, name := range map[ActionType]string{
		ActionDownload: "download", ActionUpload: "upload", ActionLocalDelete: "local_delete",
		ActionRemoteDelete: "remote_delete", ActionLocalMove: "local_move", ActionRemoteMove: "remote_move",
		ActionFolderCreate: "folder_create", ActionConflictCopy: "conflict_copy",
		ActionBaselineUpdate: "baseline_update", ActionCleanup: "cleanup",
	} {
		value, err := member.Value()
		require.NoError(t, err, name)
		assert.Equal(t, name, value)
		var scanned ActionType
		require.NoError(t, scanned.Scan(name))
		assert.Equal(t, member, scanned, name)
	}

	for value, name := range map[fmt.Stringer]string{
		SyncBidirectional: "bidirectional", SyncDownloadOnly: "download-only", SyncUploadOnly: "upload-only",
		CreateLocal: "local", CreateRemote: "remote",
	} {
		assert.Equal(t, name, value.String())
	}
}

// Validates: R-6.2.16
//
// Every decision on the action type names every member, which the
// exhaustive linter holds. A value outside the enum cannot reach one: the
// plan refuses it before any rule runs (validatePlannedActions) and the store
// refuses it on read and write. Should a programming error make one anyway,
// each decision panics with OutsideTheEnum instead of answering with a
// verdict no case states, as most used to: not blocked, no size. The planner's, the executor's and the engine's decisions have
// tests of their own (TestPlannerActionTypeDecisions_PanicOnATypeOutsideTheEnum,
// TestExecutorActionTypeDecisions_PanicOnATypeOutsideTheEnum,
// TestEngineActionTypeDecisions_PanicOnATypeOutsideTheEnum).
func TestActionTypeDecisions_PanicOnATypeOutsideTheEnum(t *testing.T) {
	t.Parallel()

	const unlisted = ActionCleanup + 1
	outside := func() *Action {
		return &Action{
			Type:    unlisted,
			Path:    "docs/a.txt",
			OldPath: "docs",
			ItemID:  "item-a",
			View:    &PathView{Path: "docs/a.txt", Baseline: &BaselineEntry{Path: "docs", ItemType: ItemTypeFolder}},
		}
	}

	decisions := map[string]func(){
		"DeletedItemType":              func() { DeletedItemType(outside()) },
		"DeferredCounts.AddAction":     func() { (&DeferredCounts{}).AddAction(outside()) },
		"IsPublicationOnlyActionType":  func() { IsPublicationOnlyActionType(unlisted) },
		"DeleteSafetySideOf":           func() { DeleteSafetySideOf(unlisted) },
		"localWriteScopeBlocksAction":  func() { localWriteScopeBlocksAction(unlisted) },
		"RemoteWriteScopeBlocksAction": func() { RemoteWriteScopeBlocksAction(unlisted) },
		"outcomeDisplaySize":           func() { outcomeDisplaySize(&ActionOutcome{Action: unlisted}) },
	}
	for name, decide := range decisions {
		assert.PanicsWithValue(t, OutsideTheEnum(unlisted), decide, name)
	}
}

// enumSwitchExceptions are the switches on the action type or the folder
// create side whose default is the decision's answer rather than a refusal,
// keyed by the function holding them, each with its reason.
func enumSwitchExceptions() map[string]string {
	return map[string]string{
		"ActionType.String":         "names the value, a value outside the enum included, for refusals and logs",
		"ActionType.IsMember":       "false is the membership answer the refusals ask for",
		"FolderCreateSide.String":   "names the value, a value outside the enum included, for refusals and logs",
		"FolderCreateSide.IsMember": "false is the membership answer the refusals ask for",
	}
}

// Validates: R-6.2.16
//
// The exhaustive linter holds every switch on the action type and the folder
// create side to every member; this test holds what their defaults answer.
// It reads the package's product files: a switch is any switch statement one
// of whose case clauses names a constant of either type (the constants are
// read from the const blocks of enums.go), tagged or not. Its default must
// refuse: the clause ends in a panic or a return, and it calls
// OutsideTheEnum, as panic(OutsideTheEnum(v)) where the decision returns a
// plain value, or inside the error it returns where it returns one. A switch
// without a default fails, and so does one whose default answers. The
// exceptions are named with their reason, and an exception that matches no
// switch fails too.
func TestActionTypeSwitches_EveryDefaultRefuses(t *testing.T) {
	t.Parallel()

	members := enumConstantTypes(t, "enums.go", "ActionType", "FolderCreateSide")
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	exceptions := enumSwitchExceptions()
	excepted := map[string]bool{}
	switches := 0
	var violations []string
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := funcDeclName(fn)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				sw, isSwitch := node.(*ast.SwitchStmt)
				if !isSwitch || !switchNamesAConstant(sw, members) {
					return true
				}
				switches++
				if _, isException := exceptions[name]; isException {
					excepted[name] = true
					return true
				}
				if problem := defaultRefusalProblem(sw); problem != "" {
					violations = append(violations, fmt.Sprintf("%s at %s: %s", name, fset.Position(sw.Pos()), problem))
				}
				return true
			})
		}
	}

	assert.Positive(t, switches, "no switch on the action type was found")
	assert.Empty(t, violations)
	for name := range exceptions {
		assert.True(t, excepted[name], "the exception %s matches no switch", name)
	}
}

// enumConstantTypes maps the name of every constant of the named types that
// file declares to its type, read from the source. In a const block a spec
// with neither type nor value repeats the spec before it, as an iota block
// does; a spec with a value and no type is untyped.
func enumConstantTypes(t *testing.T, file string, typeNames ...string) map[string]string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	constants := make(map[string]string)
	found := make(map[string]bool)
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		current := ""
		for _, spec := range gen.Specs {
			value, isValue := spec.(*ast.ValueSpec)
			if !isValue {
				continue
			}
			if value.Type != nil || len(value.Values) > 0 {
				current = ""
				if ident, isIdent := value.Type.(*ast.Ident); isIdent {
					current = ident.Name
				}
			}
			if !slices.Contains(typeNames, current) {
				continue
			}
			found[current] = true
			for _, name := range value.Names {
				constants[name.Name] = current
			}
		}
	}
	for _, typeName := range typeNames {
		require.True(t, found[typeName], "%s declares no %s constant", file, typeName)
	}

	return constants
}

// switchNamesAConstant reports whether a case clause of sw names one of the
// constants, anywhere in its expressions.
func switchNamesAConstant(sw *ast.SwitchStmt, constants map[string]string) bool {
	names := false
	for _, stmt := range sw.Body.List {
		clause, isClause := stmt.(*ast.CaseClause)
		if !isClause {
			continue
		}
		for _, expr := range clause.List {
			ast.Inspect(expr, func(node ast.Node) bool {
				if ident, isIdent := node.(*ast.Ident); isIdent && constants[ident.Name] != "" {
					names = true
				}
				return !names
			})
		}
	}

	return names
}

// defaultRefusalProblem says what keeps sw's default from refusing, or "".
func defaultRefusalProblem(sw *ast.SwitchStmt) string {
	for _, stmt := range sw.Body.List {
		clause, isClause := stmt.(*ast.CaseClause)
		if !isClause || clause.List != nil {
			continue
		}
		if len(clause.Body) == 0 || !endsInPanicOrReturn(clause.Body[len(clause.Body)-1]) {
			return "the default does not end in a panic or a return"
		}
		calls := false
		for _, bodyStmt := range clause.Body {
			ast.Inspect(bodyStmt, func(node ast.Node) bool {
				if call, isCall := node.(*ast.CallExpr); isCall && isCallTo(call, "OutsideTheEnum") {
					calls = true
				}
				return !calls
			})
		}
		if !calls {
			return "the default answers instead of refusing with OutsideTheEnum"
		}

		return ""
	}

	return "no default"
}

func endsInPanicOrReturn(stmt ast.Stmt) bool {
	switch last := stmt.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.ExprStmt:
		call, isCall := last.X.(*ast.CallExpr)
		return isCall && isCallTo(call, "panic")
	default:
		return false
	}
}

func isCallTo(call *ast.CallExpr, name string) bool {
	ident, isIdent := call.Fun.(*ast.Ident)
	return isIdent && ident.Name == name
}
