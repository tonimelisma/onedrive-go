package devtool

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.8
//
// Which tests the sync step runs fifty times is declared, and every
// declaration holds against the test sources: each top-level test of the
// step's target that its placement rule names is a member or a non-member,
// each declared name is a test of its package, and no name is declared
// twice. It runs in the quick lane, so a new test is placed in the change
// that adds it. The multisync step declares nothing and runs every test of
// its package, so a new multisync test needs no declaration.
func TestStressRepeatedStepsDeclareEveryTestTheirRuleNames(t *testing.T) {
	t.Parallel()

	root := repoRootFromDevtoolPackage(t)
	steps := stressRepeatedSteps()
	require.Len(t, steps, 2, "multisync, then the sync tree")
	assert.Nil(t, steps[0].selection, "the multisync step runs every test of its package")
	require.NotNil(t, steps[1].selection, "the sync step runs declared members")
	step := &steps[1]
	assert.Empty(t, stressDeclarationProblems(t, step, stressTargetTests(t, root, step.target)), "step %s", step.name)
}

// plantedWidgetMembers and plantedWidgetNonMembers are the lists of the
// planted step.
func plantedWidgetMembers() []string {
	return []string{"TestGizmo_SpinsUnderLoad"}
}

func plantedWidgetNonMembers() []stressNonMember {
	return []stressNonMember{{test: "TestGizmo_DrainCountIsPure", reason: "a pure count"}}
}

// plantedStressStep is a repeated step over an invented package: one member,
// one non-member, and a rule that names a prefix and a word.
func plantedStressStep() stressRepeatedStep {
	return stressRepeatedStep{
		name:   "planted-race-repeated",
		target: "./internal/widget/...",
		selection: &stressSelection{
			packages: []stressRepeatedPackage{{
				dir:           "internal/widget",
				memberLists:   []func() []string{plantedWidgetMembers},
				nonMemberList: plantedWidgetNonMembers,
			}},
			placement: stressPlacementRule{prefixes: []string{"TestGizmoWatch"}, phrases: []string{"Drain"}},
		},
	}
}

// Validates: R-6.10.8
//
// A test does not join or leave a repeated step by the spelling of its name:
// one whose name holds a word or starts with a prefix of the step's rule and
// that no list declares fails the check, which names the test, the step and
// the functions of the lists it may go to. A test the rule does not name may
// stay undeclared.
func TestStressDeclarationCheckNamesAnUndeclaredTestAndItsLists(t *testing.T) {
	t.Parallel()

	step := plantedStressStep()
	problems := stressDeclarationProblems(t, &step, map[string][]string{"internal/widget": {
		"TestGizmo_SpinsUnderLoad", "TestGizmo_DrainCountIsPure",
		"TestGizmo_DrainsTheQueueOnStop", "TestGizmoWatch_TicksTwice", "TestGizmo_ParsesItsName",
	}})

	require.Len(t, problems, 2, "%v", problems)
	for i, test := range []string{"TestGizmo_DrainsTheQueueOnStop", "TestGizmoWatch_TicksTwice"} {
		assert.Equal(t, "internal/widget."+test+" is neither a member nor a non-member of step planted-race-repeated: "+
			"add it to a member list, plantedWidgetMembers, or with its reason to the non-members, "+
			"plantedWidgetNonMembers, in verify_stress_members.go", problems[i])
	}

	syncStep := stressRepeatedSteps()[1]
	assert.Equal(t, "a member list, stressSyncRunWatchMembers, stressSyncWatchRuntimeMembers, stressSyncWatchMembers, "+
		"stressSyncOneShotAndWorkerPoolMembers or stressSyncShutdownAndCancellationMembers, or with its reason "+
		"to the non-members, stressSyncConcurrencyNonMembers,", stressDeclarationLists(t, &syncStep.selection.packages[0]))
}

// Validates: R-6.10.8
//
// A declared name that no test of its package carries any longer fails the
// check, a member or a non-member alike, and so do a name declared twice and
// a non-member without a reason. A package of the target that the step does
// not declare holds its rule-named tests to the rule all the same.
func TestStressDeclarationCheckFailsWhatTheSourcesDoNotHold(t *testing.T) {
	t.Parallel()

	step := plantedStressStep()
	widget := &step.selection.packages[0]
	widget.memberLists = append(widget.memberLists, func() []string { return []string{"TestGizmo_DrainCountIsPure"} })
	widget.nonMemberList = func() []stressNonMember {
		return append(plantedWidgetNonMembers(), stressNonMember{test: "TestGizmo_RenamedAway"})
	}
	problems := stressDeclarationProblems(t, &step, map[string][]string{"internal/widget": {
		"TestGizmo_DrainCountIsPure",
	}})

	assert.Equal(t, []string{
		"internal/widget.TestGizmo_RenamedAway is a non-member of step planted-race-repeated without a reason",
		"internal/widget.TestGizmo_DrainCountIsPure is declared 2 times for step planted-race-repeated; " +
			"a test is one member or one non-member",
		"internal/widget.TestGizmo_RenamedAway is declared for step planted-race-repeated, " +
			"and internal/widget defines no such test",
		"internal/widget.TestGizmo_SpinsUnderLoad is declared for step planted-race-repeated, " +
			"and internal/widget defines no such test",
	}, problems)

	step = plantedStressStep()
	problems = stressDeclarationProblems(t, &step, map[string][]string{
		"internal/widget":       {"TestGizmo_SpinsUnderLoad", "TestGizmo_DrainCountIsPure"},
		"internal/widget/inner": {"TestInner_Counts", "TestInner_DrainsOnStop"},
	})
	assert.Equal(t, []string{
		"internal/widget/inner.TestInner_DrainsOnStop is neither a member nor a non-member of step " +
			"planted-race-repeated: add it to a declaration of internal/widget/inner in verify_stress_members.go",
	}, problems, "a package without a declaration")
}

// Validates: R-6.10.8
//
// Each member list and the non-members of the sync step are in name order, so
// a new name has one place to go, and two changes that add names do not both
// add them at the end of a list. A package may declare no non-members, and
// then its member lists alone are held to name order.
func TestStressDeclarationsAreInNameOrder(t *testing.T) {
	t.Parallel()

	for _, step := range stressRepeatedSteps() {
		if step.selection != nil {
			assert.Empty(t, stressDeclarationOrderProblems(t, &step), "step %s", step.name)
		}
	}

	step := plantedStressStep()
	widget := &step.selection.packages[0]
	widget.memberLists = append(widget.memberLists, plantedWidgetUnsortedMembers)
	widget.nonMemberList = nil
	assert.Equal(t, []string{"plantedWidgetUnsortedMembers: TestGizmo_Holds stands after TestGizmo_Spins"},
		stressDeclarationOrderProblems(t, &step), "a package without non-members")
}

// plantedWidgetUnsortedMembers is a member list out of name order.
func plantedWidgetUnsortedMembers() []string {
	return []string{"TestGizmo_Spins", "TestGizmo_Holds"}
}

// stressDeclarationOrderProblems names each list of step's packages whose
// names leave name order, with where they first do: every member list, and
// the non-members of a package that declares them.
func stressDeclarationOrderProblems(t *testing.T, step *stressRepeatedStep) []string {
	t.Helper()

	var problems []string
	for i := range step.selection.packages {
		pkg := &step.selection.packages[i]
		for _, list := range pkg.memberLists {
			problems = appendStressOrderProblem(problems, stressFuncName(t, list), list())
		}
		if pkg.nonMemberList == nil {
			continue
		}
		nonMembers := pkg.nonMembers()
		names := make([]string, 0, len(nonMembers))
		for _, nonMember := range nonMembers {
			names = append(names, nonMember.test)
		}
		problems = appendStressOrderProblem(problems, stressFuncName(t, pkg.nonMemberList), names)
	}

	return problems
}

// appendStressOrderProblem appends to problems where the names of list first
// leave name order, when they do.
func appendStressOrderProblem(problems []string, list string, names []string) []string {
	if where := stressNameOutOfOrder(names); where != "" {
		return append(problems, list+": "+where)
	}

	return problems
}

// stressNameOutOfOrder says where names first leave name order, empty when
// they keep it.
func stressNameOutOfOrder(names []string) string {
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			return names[i] + " stands after " + names[i-1]
		}
	}

	return ""
}

// stressDeclarationProblems lists what step's declaration gets wrong against
// defined, the top-level tests of each package directory of its target: a
// test the placement rule names that no list holds, a declared name its
// package does not define, a name declared twice, and a non-member without a
// reason. A package of the target the step does not declare has empty lists.
func stressDeclarationProblems(t *testing.T, step *stressRepeatedStep, defined map[string][]string) []string {
	t.Helper()

	declared := map[string]*stressRepeatedPackage{}
	for i := range step.selection.packages {
		declared[step.selection.packages[i].dir] = &step.selection.packages[i]
	}
	dirs := slices.Sorted(maps.Keys(defined))
	for dir := range declared {
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)

	var problems []string
	for _, dir := range dirs {
		pkg := declared[dir]
		if pkg == nil {
			pkg = &stressRepeatedPackage{dir: dir}
		}
		problems = append(problems, stressPackageProblems(t, step, pkg, defined[dir])...)
	}

	return problems
}

// stressDeclarationLists names, for a test that must be placed, the functions
// of pkg's lists, or a new declaration of pkg when the step declares none.
func stressDeclarationLists(t *testing.T, pkg *stressRepeatedPackage) string {
	t.Helper()

	if len(pkg.memberLists) == 0 {
		return "a declaration of " + pkg.dir
	}
	names := make([]string, 0, len(pkg.memberLists))
	for _, list := range pkg.memberLists {
		names = append(names, stressFuncName(t, list))
	}
	lists := names[0]
	if len(names) > 1 {
		lists = strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
	}
	if pkg.nonMemberList == nil {
		return "a member list, " + lists + ","
	}

	return "a member list, " + lists + ", or with its reason to the non-members, " + stressFuncName(t, pkg.nonMemberList) + ","
}

// stressFuncName is the name of a declaration function, without its package.
// A nil list has no name to give, and fails the test.
func stressFuncName(t *testing.T, fn any) string {
	t.Helper()

	name := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	require.NotEmpty(t, name, "a declaration list is a function")

	return name[strings.LastIndex(name, ".")+1:]
}

// stressPackageProblems is stressDeclarationProblems for one package.
func stressPackageProblems(t *testing.T, step *stressRepeatedStep, pkg *stressRepeatedPackage, defined []string) []string {
	t.Helper()

	var problems []string
	dir := pkg.dir
	counts := map[string]int{}
	for _, member := range pkg.members() {
		counts[member]++
	}
	for _, nonMember := range pkg.nonMembers() {
		counts[nonMember.test]++
		if strings.TrimSpace(nonMember.reason) == "" {
			problems = append(problems, fmt.Sprintf("%s.%s is a non-member of step %s without a reason", dir, nonMember.test, step.name))
		}
	}
	for _, test := range defined {
		if counts[test] == 0 && step.selection.placement.requires(test) {
			problems = append(problems, fmt.Sprintf("%s.%s is neither a member nor a non-member of step %s: add it to %s in %s",
				dir, test, step.name, stressDeclarationLists(t, pkg), "verify_stress_members.go"))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		if counts[name] > 1 {
			problems = append(problems, fmt.Sprintf(
				"%s.%s is declared %d times for step %s; a test is one member or one non-member", dir, name, counts[name], step.name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		if !slices.Contains(defined, name) {
			problems = append(problems, fmt.Sprintf(
				"%s.%s is declared for step %s, and %s defines no such test", dir, name, step.name, dir))
		}
	}

	return problems
}

// stressTargetTests are the top-level tests of every package directory of a
// go test target under root, by directory from the module root: the one
// directory, or every directory below a ./dir/... tree that holds test
// files. Every test file counts, whatever its build constraint, since a
// declaration names the tests of every platform.
func stressTargetTests(t *testing.T, root string, target string) map[string][]string {
	t.Helper()

	dir, isTree := strings.CutSuffix(strings.TrimPrefix(target, "./"), "/...")
	tests := map[string][]string{}
	walkErr := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != filepath.Join(root, dir) && (!isTree || entry.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		require.NoError(t, relErr)
		tests[filepath.ToSlash(rel)] = append(tests[filepath.ToSlash(rel)], topLevelTestNames(t, path)...)

		return nil
	})
	require.NoError(t, walkErr)
	for pkg := range tests {
		slices.Sort(tests[pkg])
	}

	return tests
}

// topLevelTestNames are the test functions one file declares, as go test
// finds them: a function without a receiver named Test, or Test followed by
// a character that is not a lower-case letter, other than TestMain.
func topLevelTestNames(t *testing.T, path string) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.NoError(t, err, path)
	var names []string
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Recv != nil || fn.Name.Name == "TestMain" {
			continue
		}
		rest, isTest := strings.CutPrefix(fn.Name.Name, "Test")
		next, _ := utf8.DecodeRuneInString(rest)
		if isTest && (rest == "" || !unicode.IsLower(next)) {
			names = append(names, fn.Name.Name)
		}
	}

	return names
}
