package e2e

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
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

// This file holds a rule of what the live suite prints, read from its sources
// like the rules beside it: CI logs are public, so no printing call takes a
// raw command output, a command line or an identity value. A command's output
// and command line go through printable (masked and bounded) or an output
// assertion (assertOutputContains, assertOutputEqual), an identity value
// through its role (suiteDriveRoles.Role or Redact) or assertSameIdentity,
// and a local path that may hold a fixture's name through assertLocalPath,
// assertSamePath or requireNoLocalError.
//
// The rule reads the files built only with the e2e tag, and in them every
// function except a test that checks planted values only: it reaches no
// makeCmd, the one place the suite starts the CLI, and names none of the
// suite's live values (suiteLiveValues). Of the other files it reads every
// function but a test that one of those reaches: a helper the live suite
// shares with the unit tests that run without an account (scannedFuncs). A
// printing call is a testify assertion given t, a method of t in the Log,
// Error, Fatal and Skip families, t.Run's name (go test prints it on every
// result line), fmt.Errorf, fmt.Print*, fmt.Fprint* to os.Stderr or
// os.Stdout, and a suite function's msgAndArgs or what argument (the output
// assertions and assertSameIdentity print their message as it is given). An
// argument prints a name or a field, an element or a part of one, and what +,
// string(), a conversion to a slice, a String() or an Error() method, a
// fmt.Sprint* call, a strings Trim, Join or case call, filepath.Join,
// path.Join, filepath.FromSlash, append, an os call (whose error quotes the
// paths it is given) or a testutil function (testutil.TokenFileName of a
// drive) makes of one; any other call (the maskers among them) prints its own
// result. True and False print no value, NotEmpty, NotNil and NotZero only an
// empty one, and a constant is the suite's own. An argument the call's
// constant format, at its message position, prints with %T prints no text,
// and so does one under %t or %d that is no element of a map or a slice, no
// ranged value and no variable set from either, and under %d also neither a
// conversion to a slice nor an address (printsNoText).
//
// A value is known by its name, the suite's convention: a command output is
// stdout, stderr, out or output, or ends in Stdout, Stderr, Out or Output; a
// command line is args or ends in Args; an identity value is drive, email,
// selector or id, or ends in ID, Email, Selector, Link, Drive, ShortcutName,
// SharedFolderName or SentinelPath, or in IDs, Emails, Selectors or Links for
// a list of them (not in Drives, which ends the names of counts such as
// TotalDrives), a trailing number aside. A Graph request ID names no account,
// drive or item, so a name ending in RequestID is none, and neither is
// testify's msgAndArgs. A value is known as well by where it comes from,
// whatever its name (printed_value_flow_test.go): what a JSON decode fills is
// a command's output, what a CLI runner returns (suiteCLIRunners) its output
// or its command line, and a variable set or ranged from a known value, the
// error of an os call given one, a suite function's parameter a caller
// passes one, and a suite function's text result (a string, a []string or an
// error) one of its return statements returns one hold its class, followed
// to a fixed point through any number of calls.
//
// What the rule cannot see: a struct, a map or a pointer a suite function
// decodes or reads and returns, since without types the scan cannot tell a
// count or a state field of it from a name field, and would class every
// decoded count the suite compares; a method's parameters and results, since
// the scan builds no types and does not know a call's receiver; and a value
// any other call makes of a known one (a method's result,
// liveConfig.CandidateDriveIDs()) or a strings.Builder collects. Results are
// followed per function, not per call: a helper that returns what it is
// given returns the class of what any caller gives it. A composite literal ends the flow: the fields of
// a struct or slice literal built from a known value are not followed. A
// function literal's parameters and a range key are never classed. A field
// named Name, DisplayName or OrgName holds no identity by its name, so a
// shared folder fixture's FolderItem.Name passes unless the flow classes it.
// Printing calls outside the list are not read: the log package, panic, and
// a testing.T not named t. A test that reads a live value only through a
// helper that starts no CLI, and names none itself, is skipped as one that
// checks planted values. A []byte or pointer variable printed with %d passes
// as a number, since without types the scan tells only a conversion or an
// address there. Any other value of interface type passes under %d and %t
// too, though fmt prints the text it holds (%!d(string=...)): a struct
// field, a variable or a parameter declared as one (a decode target among
// them), a type assertion to one and a slice of one. The scan tells only an
// element, a ranged value and a variable set from either.
//
// The sites of the live suite a hand fix masks and the rule cannot see are
// accepted, for these reasons. Structs a suite function decodes and returns:
// the status issues readStatusSyncState returns, the control socket's
// mounts, a status drive's issues and the shortcut root waits' poll state;
// and a method's result, liveConfig.CandidateDriveIDs(). The messages a suite
// function builds from its parameters and returns (outputMismatch,
// printableIssues, redactedSummary, sharedRootListingDescription, the status
// poll descriptions, formatAuthPreflightFailure) and the errors the shared
// fixture helpers return are followed, and pass because each masks what it
// is given. The local paths
// assertSamePath compares, classed only when built from a fixture's name. The
// error of a product call or a CLI run (the catalog learnSuiteRoles loads, a
// drive's token source, the baseline pass of the standing conditions), which
// names no value the scan can class. And the
// artifacts, which no printing call writes: the timing and quirk events and a
// debug log's name and lines, which TestE2ERecordersRecordRolesNotDrives
// holds.
//
// Shapes of the product's output the suite does not print today and the
// masks would not catch: the SHARED BY column of shared's text table (the
// suite reads shared --json), a SharePoint site name in drive list's text
// label (its JSON site_name is masked by key), and status --json's
// mount_ref, a digest of the mount ID anyone can check against a guessed ID.

// printedValueFinding is one value a printing call prints as it is.
type printedValueFinding struct {
	// at is the value's file and line.
	at string
	// call is the printing call.
	call string
	// value is the printed name as the source spells it.
	value string
	// class is what the name says the value is.
	class string
}

const (
	printedClassOutput   = "command output"
	printedClassCommand  = "command line"
	printedClassIdentity = "identity value"
)

func (finding printedValueFinding) message() string {
	fix := "print it through printable, or assert on it with assertOutputContains"
	if finding.class == printedClassIdentity {
		fix = "print its role (suiteDriveRoles.Role or Redact), or compare it with assertSameIdentity"
	}

	return fmt.Sprintf("%s: %s prints the %s %s as it is, and CI logs are public: %s",
		finding.at, finding.call, finding.class, finding.value, fix)
}

// findPrintedValues lists, function by function in the order scannedFuncs
// gives them, the values the live suite's functions in dir print as they
// are.
func findPrintedValues(dir string) ([]printedValueFinding, error) {
	src, err := readSuiteSources(dir)
	if err != nil {
		return nil, err
	}
	fset, files, err := parseSuiteFiles(dir)
	if err != nil {
		return nil, err
	}

	scan := printedValueScan{
		fset: fset, constants: constantNames(files), runners: runnerResultsOf(files),
		packages: importedPackages(files), messages: messageParameters(files), paramNames: parameterNames(files),
	}
	live := scannedFuncs(files, src)

	scan.followFunctions(live)
	for _, fn := range live {
		scan.flow, scan.elements = scan.flowOf(fn), elementNames(fn.Body)
		ast.Inspect(fn.Body, scan.inspect)
	}

	return scan.findings, nil
}

// scannedFuncs lists the functions the rule reads: in file and source order,
// each function of a live suite file but a test that checks planted values
// only, then each function but a test of the other files that one of those
// reaches, a helper the live suite shares with the unit tests.
func scannedFuncs(files []*ast.File, src *suiteSources) []*ast.FuncDecl {
	var live, shared []*ast.FuncDecl
	for _, file := range files {
		liveFile := isLiveSuiteFile(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			switch {
			case !ok || fn.Body == nil:
			case liveFile:
				if !isPlantedValueTest(fn, src) {
					live = append(live, fn)
				}
			case !isTest(fn.Name.Name):
				shared = append(shared, fn)
			}
		}
	}

	roots := make([]string, 0, len(live))
	for _, fn := range live {
		roots = append(roots, funcKey(fn))
	}
	reached := src.reachedFrom(roots)
	for _, fn := range shared {
		if reached[funcKey(fn)] {
			live = append(live, fn)
		}
	}

	return live
}

// parseSuiteFiles parses every Go file of dir with its comments, whatever its
// build tags.
func parseSuiteFiles(dir string) (*token.FileSet, []*ast.File, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, nil, fmt.Errorf("list Go files of %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		files = append(files, file)
	}

	return fset, files, nil
}

// isPlantedValueTest reports a test that checks planted values only: it
// reaches no makeCmd, the one place the suite starts the CLI, and names none
// of the suite's live values (suiteLiveValues).
func isPlantedValueTest(fn *ast.FuncDecl, src *suiteSources) bool {
	if fn.Recv != nil || !isTest(fn.Name.Name) || src.reaches(fn.Name.Name, "makeCmd") {
		return false
	}

	namesLiveValue := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && slices.Contains(suiteLiveValues(), ident.Name) {
			namesLiveValue = true
		}

		return !namesLiveValue
	})

	return !namesLiveValue
}

// suiteLiveValues are the suite's variables that TestMain sets from the live
// environment: the configured drives, the fixtures and the test credentials'
// folder.
func suiteLiveValues() []string {
	return []string{"drive", "drive2", "liveConfig", "testDataDir"}
}

// printedValueScan collects the findings of one read of the sources.
type printedValueScan struct {
	fset      *token.FileSet
	constants map[string]bool
	// runners are the result classes of each CLI runner (runnerResultsOf).
	runners map[string][]string
	// packages are the names the files import packages under.
	packages map[string]bool
	// messages are the suite functions that print a message argument
	// (messageParameters).
	messages map[string]messageParameter
	// paramNames are the parameter names of each suite function, params
	// the classes its callers pass them (followParameters) and results the
	// classes its return statements return (followResults).
	paramNames map[string][]string
	params     map[string]valueFlow
	results    map[string][]string
	// flow is what the variables of the function being read hold, and
	// elements those of its variables set from an element (elementNames).
	flow     valueFlow
	elements map[string]bool
	findings []printedValueFinding
}

func (scan *printedValueScan) inspect(node ast.Node) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return true
	}

	name, args := scan.printingCallArgs(call)
	var verbs map[int]rune
	if formatAt, formats := scan.formatIndex(name); formats {
		verbs = typedVerbArgs(args, formatAt)
	}
	for index, arg := range args {
		if index == 0 && printsNoFirstValue(name) || scan.printsNoText(verbs[index], arg) {
			continue
		}
		for _, leaf := range scan.printedLeaves(arg) {
			class := scan.leafClass(scan.flow, leaf)
			if class == "" || scan.isConstant(leaf) {
				continue
			}
			position := scan.fset.Position(leaf.Pos())
			scan.findings = append(scan.findings, printedValueFinding{
				at:   fmt.Sprintf("%s:%d", filepath.Base(position.Filename), position.Line),
				call: name, value: exprText(leaf), class: class,
			})
		}
	}

	return true
}

func (scan *printedValueScan) isConstant(leaf ast.Expr) bool {
	ident, ok := leaf.(*ast.Ident)

	return ok && scan.constants[ident.Name]
}

// constantNames lists the package-level constants of files.
func constantNames(files []*ast.File) map[string]bool {
	constants := map[string]bool{}
	for _, file := range files {
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
				for _, name := range values.Names {
					constants[name.Name] = true
				}
			}
		}
	}

	return constants
}

// isLiveSuiteFile reports a file built with the e2e tag and not without it.
func isLiveSuiteFile(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			return false
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				return false
			}

			return expr.Eval(func(tag string) bool { return tag == "e2e" || tag == "e2e_full" }) &&
				!expr.Eval(func(string) bool { return false })
		}
	}

	return false
}

// messageParameter is where a suite function's message arguments start: at
// a variadic msgAndArgs, or the one argument a parameter named what takes.
type messageParameter struct {
	index    int
	variadic bool
}

// messageParameters lists the suite functions with a msgAndArgs or a what
// parameter: an output assertion (assertOutputContains and its siblings)
// prints the output masked itself, and prints its message as it is given,
// as assertSameIdentity prints what it compares.
func messageParameters(files []*ast.File) map[string]messageParameter {
	messages := map[string]messageParameter{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			index := 0
			for _, field := range fn.Type.Params.List {
				for _, param := range field.Names {
					switch param.Name {
					case "msgAndArgs":
						messages[fn.Name.Name] = messageParameter{index: index, variadic: true}
					case "what":
						messages[fn.Name.Name] = messageParameter{index: index}
					}
					index++
				}
			}
		}
	}

	return messages
}

// printingCallArgs names call and returns the arguments it prints, or none
// when it prints nothing.
func (scan *printedValueScan) printingCallArgs(call *ast.CallExpr) (string, []ast.Expr) {
	if fun, isIdent := call.Fun.(*ast.Ident); isIdent {
		return scan.messageArgs(fun.Name, call.Args)
	}

	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", nil
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", nil
	}

	name := receiver.Name + "." + selector.Sel.Name
	switch {
	case isTestifyPackage(receiver.Name) && len(call.Args) > 0 && isIdentNamed(call.Args[0], "t"):
		return name, call.Args[1:]
	case receiver.Name == "t" && slices.Contains([]string{"Log", "Logf", "Error", "Errorf", "Fatal", "Fatalf", "Skip", "Skipf"}, selector.Sel.Name):
		return name, call.Args
	case receiver.Name == "t" && selector.Sel.Name == "Run" && len(call.Args) > 0:
		// go test prints a subtest's name on every result line.
		return name, call.Args[:1]
	case receiver.Name == fmtPackage:
		if printed, prints := fmtPrintedArgs(selector.Sel.Name, call.Args); prints {
			return name, printed
		}
	}

	return "", nil
}

// fmtPrintedArgs returns the arguments a call of the fmt function function
// prints, and false for one whose output the rule does not read: fmt.Errorf
// and fmt.Print* print all of theirs, fmt.Fprint* those after os.Stderr or
// os.Stdout.
func fmtPrintedArgs(function string, args []ast.Expr) ([]ast.Expr, bool) {
	switch {
	case function == "Errorf", strings.HasPrefix(function, "Print"):
		return args, true
	case strings.HasPrefix(function, "Fprint") && len(args) > 0 && isProcessStream(args[0]):
		return args[1:], true
	}

	return nil, false
}

func isProcessStream(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	return isIdentNamed(selector.X, "os") && (selector.Sel.Name == "Stderr" || selector.Sel.Name == "Stdout")
}

// formatIndex is the index, among the arguments printingCallArgs returns for
// call, of the format the call prints its later arguments with, and false for
// a call that formats none: a testify assertion's message after its values
// (testifyValueCount), the first argument of t's and fmt's f functions, and a
// suite function's msgAndArgs, which it hands to testify.
func (scan *printedValueScan) formatIndex(call string) (int, bool) {
	pkg, function, isPackageFunction := strings.Cut(call, ".")
	switch {
	case !isPackageFunction:
		return 0, scan.messages[call].variadic
	case isTestifyPackage(pkg):
		return testifyValueCount(function)
	}

	return 0, strings.HasSuffix(function, "f")
}

// testifyValueCount is how many values a testify assertion takes before its
// message, t aside, its f form as many, and false for an assertion this list
// does not hold, whose message then exempts no argument.
func testifyValueCount(assertion string) (int, bool) {
	switch strings.TrimSuffix(assertion, "f") {
	case "True", "False", "Nil", "NotNil", "Empty", "NotEmpty", "Zero", "NotZero", "NoError", "Error",
		"FileExists", "NoFileExists", "DirExists", "NoDirExists", "Fail", "FailNow", "Positive", "Negative":
		return 1, true
	case "Equal", "NotEqual", "EqualValues", "Exactly", "Contains", "NotContains", "Len", "ErrorIs", "NotErrorIs",
		"ErrorContains", "ErrorAs", "EqualError", "Greater", "GreaterOrEqual", "Less", "LessOrEqual", "Regexp",
		"NotRegexp", "Subset", "NotSubset", "ElementsMatch", "Same", "NotSame", "IsType", "JSONEq":
		return 2, true
	case "InDelta", "WithinDuration", "Eventually", "Never":
		return 3, true
	}

	return 0, false
}

// typedVerbArgs lists, by index, the arguments of args that the constant
// format at formatAt prints with %d, %t or %T, and the verb: go vet holds the
// first two to a number or a pointer and a bool unless the argument is an
// interface, and %T prints a type (printsNoText). A format that is not
// constant, or that the scan cannot map to its arguments (an explicit index,
// a * width), lists none.
func typedVerbArgs(args []ast.Expr, formatAt int) map[int]rune {
	if formatAt >= len(args) {
		return nil
	}
	format, ok := constantString(args[formatAt])
	if !ok {
		return nil
	}
	verbs, mapped := formatVerbs(format)
	if !mapped {
		return nil
	}

	typed := map[int]rune{}
	for j, verb := range verbs {
		if strings.ContainsRune("dtT", verb) {
			typed[formatAt+1+j] = verb
		}
	}

	return typed
}

// printsNoText reports an argument that verb prints no text of: any under %T,
// and under %t and %d one that is no element, no ranged value and no
// variable set from either (isElement), since an element of a decoded
// listing is an interface that fmt prints in its bad-verb form,
// %!d(string=...), text included. Under %d it is neither a conversion to a
// slice, whose bytes or runes %d prints as numbers, nor an address, whose
// struct %d prints field by field with each string field's text.
func (scan *printedValueScan) printsNoText(verb rune, arg ast.Expr) bool {
	switch verb {
	case 'T':
		return true
	case 't':
		return !isElement(arg, scan.elements)
	case 'd':
		if isElement(arg, scan.elements) {
			return false
		}
		if call, isCall := arg.(*ast.CallExpr); isCall {
			_, isSliceConversion := call.Fun.(*ast.ArrayType)
			return !isSliceConversion
		}
		unary, isUnary := arg.(*ast.UnaryExpr)

		return !isUnary || unary.Op != token.AND
	}

	return false
}

// constantString is the value of a string literal or a sum of them.
func constantString(expr ast.Expr) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expr.Value)

		return value, err == nil
	case *ast.BinaryExpr:
		left, leftOK := constantString(expr.X)
		right, rightOK := constantString(expr.Y)

		return left + right, expr.Op == token.ADD && leftOK && rightOK
	}

	return "", false
}

// formatVerbs lists the verbs of a printf format in argument order, or
// reports that its arguments cannot be mapped to them.
func formatVerbs(format string) ([]rune, bool) {
	var verbs []rune
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("+-# 0123456789.", format[i]) >= 0 {
			i++
		}
		switch {
		case i >= len(format), format[i] == '*', format[i] == '[':
			return nil, false
		case format[i] != '%':
			verbs = append(verbs, rune(format[i]))
		}
	}

	return verbs, true
}

// messageArgs names a call of the suite function fun and returns its
// message arguments (messageParameters), or none when fun has none.
func (scan *printedValueScan) messageArgs(fun string, args []ast.Expr) (string, []ast.Expr) {
	message, isMessage := scan.messages[fun]
	switch {
	case !isMessage || message.index >= len(args):
		return "", nil
	case message.variadic:
		return fun, args[message.index:]
	default:
		return fun, args[message.index : message.index+1]
	}
}

// printsNoFirstValue reports a testify assertion that does not print its
// first value: True and False print none, and NotEmpty, NotNil and NotZero
// print it only when it holds nothing.
func printsNoFirstValue(call string) bool {
	_, assertion, _ := strings.Cut(call, ".")
	assertion = strings.TrimSuffix(assertion, "f")

	return slices.Contains([]string{"True", "False", "NotEmpty", "NotNil", "NotZero"}, assertion)
}

// printedLeaves lists the names and fields expr prints as they are, an
// element or a part of one standing for the value it is taken from, and a
// runner's call standing for its output.
func (scan *printedValueScan) printedLeaves(expr ast.Expr) []ast.Expr {
	switch expr := expr.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return []ast.Expr{expr}
	case *ast.IndexExpr:
		return scan.printedLeaves(expr.X)
	case *ast.SliceExpr:
		return scan.printedLeaves(expr.X)
	case *ast.TypeAssertExpr:
		return scan.printedLeaves(expr.X)
	case *ast.ParenExpr:
		return scan.printedLeaves(expr.X)
	case *ast.StarExpr:
		return scan.printedLeaves(expr.X)
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			return scan.printedLeaves(expr.X)
		}
	case *ast.BinaryExpr:
		if expr.Op == token.ADD {
			return append(scan.printedLeaves(expr.X), scan.printedLeaves(expr.Y)...)
		}
	case *ast.CallExpr:
		if scan.callResults(expr) != nil {
			return []ast.Expr{expr}
		}

		return scan.printedCallLeaves(expr)
	}

	return nil
}

const (
	// stringTypeName is the name of the string type, and of the conversion
	// to it.
	stringTypeName = "string"
	// fmtPackage is the name the suite imports fmt under.
	fmtPackage = "fmt"
	// errorMethodName is the error interface's method, which returns the
	// error's text.
	errorMethodName = "Error"
)

// handsOnEveryArg reports a package function whose result or error holds
// every argument it is given as it is: fmt.Sprint*, the path joins and
// filepath.FromSlash, any os function, whose error quotes its paths, and any
// testutil function, which builds a name of what it is given
// (testutil.TokenFileName of a drive) and masks nothing.
func handsOnEveryArg(fun *ast.SelectorExpr) bool {
	pkg, ok := fun.X.(*ast.Ident)
	if !ok {
		return false
	}

	switch pkg.Name + "." + fun.Sel.Name {
	case "filepath.Join", "filepath.FromSlash", "path.Join":
		return true
	}

	return pkg.Name == "os" || pkg.Name == "testutil" || (pkg.Name == fmtPackage && strings.HasPrefix(fun.Sel.Name, "Sprint"))
}

// printedCallLeaves lists what a call hands on as it is: a conversion to
// string or to a slice, append, and what a package function or method hands
// on (selectorHandedOn).
func (scan *printedValueScan) printedCallLeaves(call *ast.CallExpr) []ast.Expr {
	var handedOn []ast.Expr
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == stringTypeName || fun.Name == "append" {
			handedOn = call.Args
		}
	case *ast.ArrayType:
		// A conversion to []byte or []rune holds its argument's text.
		handedOn = call.Args
	case *ast.SelectorExpr:
		handedOn = selectorHandedOn(fun, call.Args)
	}

	var leaves []ast.Expr
	for _, arg := range handedOn {
		leaves = append(leaves, scan.printedLeaves(arg)...)
	}

	return leaves
}

// selectorHandedOn lists the arguments, or the receiver, that a call of fun
// with args hands on as they are: a String() or an Error() method's receiver,
// fmt.Sprint*, strings' trimming, joining and case calls' first argument, the
// path joins, and an os call's arguments, whose error quotes the paths it is
// given (that of os.WriteFile its file name, not the data it writes).
func selectorHandedOn(fun *ast.SelectorExpr, args []ast.Expr) []ast.Expr {
	switch {
	case (fun.Sel.Name == "String" || fun.Sel.Name == errorMethodName) && len(args) == 0:
		return []ast.Expr{fun.X}
	case isIdentNamed(fun.X, "os") && fun.Sel.Name == "WriteFile" && len(args) > 0:
		return args[:1]
	case handsOnEveryArg(fun):
		return args
	case isIdentNamed(fun.X, "strings") && len(args) > 0 && slices.Contains(
		[]string{"TrimSpace", "TrimSuffix", "TrimPrefix", "Trim", "TrimLeft", "TrimRight", "Join", "ToLower", "ToUpper"},
		fun.Sel.Name):
		return args[:1]
	}

	return nil
}

func leafName(leaf ast.Expr) string {
	switch leaf := leaf.(type) {
	case *ast.Ident:
		return leaf.Name
	case *ast.SelectorExpr:
		return leaf.Sel.Name
	}

	return ""
}

func exprText(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.SelectorExpr:
		return exprText(expr.X) + "." + expr.Sel.Name
	case *ast.Ident:
		return expr.Name
	case *ast.CallExpr:
		return exprText(expr.Fun) + "(...)"
	}

	return "(expression)"
}

// isUnclassedName reports a name that holds no account, drive or item
// whatever it is taken from: a Graph request ID, and testify's msgAndArgs.
func isUnclassedName(name string) bool {
	trimmed := strings.TrimRight(name, "0123456789")

	return strings.HasSuffix(strings.ToLower(trimmed), "requestid") || strings.HasSuffix(trimmed, "AndArgs")
}

// printedValueClass names what a value's name says it is, or the empty
// string for a name the rule does not hold.
func printedValueClass(name string) string {
	trimmed := strings.TrimRight(name, "0123456789")
	hasSuffix := func(suffixes ...string) bool {
		return slices.ContainsFunc(suffixes, func(suffix string) bool { return strings.HasSuffix(trimmed, suffix) })
	}

	switch {
	case isUnclassedName(name):
		return ""
	case slices.Contains([]string{"stdout", "stderr", "out", "output"}, trimmed) || hasSuffix("Stdout", "Stderr", "Out", "Output"):
		return printedClassOutput
	case trimmed == "args" || hasSuffix("Args"):
		return printedClassCommand
	case slices.Contains([]string{"drive", "email", "selector", "id"}, trimmed) ||
		hasSuffix("ID", "Email", "Selector", "Link", "Drive", "ShortcutName", "SharedFolderName", "SentinelPath") ||
		hasSuffix("IDs", "Emails", "Selectors", "Links"):
		return printedClassIdentity
	}

	return ""
}

// reaches reports whether from, or a suite function it reaches, is target.
func (src *suiteSources) reaches(from, target string) bool {
	seen := map[string]bool{}
	pending := []string{from}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == target {
			return true
		}
		if fn, ok := src.funcs[current]; ok && !seen[current] {
			seen[current] = true
			pending = append(pending, fn.refs...)
		}
	}

	return false
}

// reachedFrom lists the suite functions that one of roots is or reaches.
func (src *suiteSources) reachedFrom(roots []string) map[string]bool {
	reached := map[string]bool{}
	pending := slices.Clone(roots)
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if fn, ok := src.funcs[current]; ok && !reached[current] {
			reached[current] = true
			pending = append(pending, fn.refs...)
		}
	}

	return reached
}

// Validates: R-6.10.27
func TestE2EPrintsNoRawOutputOrIdentity(t *testing.T) {
	t.Parallel()

	src, err := readSuiteSources(".")
	require.NoError(t, err)
	require.Contains(t, src.funcs, "makeCmd",
		"the rule tells live tests by their reaching makeCmd, which starts the CLI: follow its new name")

	findings, err := findPrintedValues(".")
	require.NoError(t, err)
	for _, finding := range findings {
		assert.Fail(t, finding.message())
	}
}

// Validates: R-6.10.27
func TestPrintedValueRuleReportsRawPrints(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "planted_live_test.go"), []byte(plantedLivePrintSource), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "planted_unit_test.go"), []byte(plantedUnitPrintSource), 0o600))

	findings, err := findPrintedValues(dir)
	require.NoError(t, err)

	assert.Equal(t, []printedValueFinding{
		{at: "planted_live_test.go:13", call: "assert.Contains", value: "stdout", class: printedClassOutput},
		{at: "planted_live_test.go:15", call: "t.Logf", value: "daemonStderr", class: printedClassOutput},
		{at: "planted_live_test.go:16", call: "assert.Equal", value: "attempt.Stdout", class: printedClassOutput},
		{at: "planted_live_test.go:19", call: "t.Logf", value: "lsOut", class: printedClassOutput},
		{at: "planted_live_test.go:19", call: "t.Logf", value: "fixture.ShortcutName", class: printedClassIdentity},
		{at: "planted_live_test.go:21", call: "fmt.Fprintln", value: "drive2", class: printedClassIdentity},
		{at: "planted_live_test.go:22", call: "fmt.Println", value: "stdout", class: printedClassOutput},
		{at: "planted_live_test.go:23", call: "t.Logf", value: "candidateDriveIDs", class: printedClassIdentity},
		{at: "planted_live_test.go:23", call: "t.Logf", value: "recipientEmails", class: printedClassIdentity},
		{at: "planted_live_test.go:23", call: "t.Logf", value: "shareSelectors", class: printedClassIdentity},
		{at: "planted_live_test.go:23", call: "t.Logf", value: "sharingLinks", class: printedClassIdentity},
		{at: "planted_live_test.go:28", call: "fmt.Errorf", value: "syncArgs", class: printedClassCommand},
		{
			at: "planted_unit_test.go:6", call: "fmt.Errorf", value: "candidate.fixture.RecipientEmail",
			class: printedClassIdentity,
		},
	}, findings,
		"a raw output as a value, through String(), as a field and through strings.TrimSpace, a fixture name, a "+
			"drive on os.Stderr, an output on os.Stdout, lists of IDs, addresses, selectors and links, a command "+
			"line in an error and an address in an untagged helper a live test calls; not a masked output, a "+
			"constant, a length, request IDs, a value NotEmpty prints only when empty, a config builder, a masked "+
			"address, a count of drives, a unit test that runs no CLI, or an untagged function no live test reaches")
}

// plantedLivePrintSource is a live suite file in miniature; its lines are the
// ones TestPrintedValueRuleReportsRawPrints names.
const plantedLivePrintSource = `//go:build e2e

package e2e

const quirkSourceStderr = "stderr"

func makeCmd() {}

func runPlanted() (string, string) { makeCmd(); return "", "" }

func TestPlantedLive(t *testing.T) {
	stdout, stderr := runPlanted()
	assert.Contains(t, stdout, "Uploaded")
	require.NoErrorf(t, err, "stderr: %s", printable(stderr))
	t.Logf("daemon: %s", daemonStderr.String())
	assert.Equal(t, want, attempt.Stdout)
	t.Log(quirkSourceStderr, len(stdout), requestID, lastRequestID)
	require.NotEmpty(t, itemID)
	t.Logf("%s %s", strings.TrimSpace(lsOut), fixture.ShortcutName)
	fmt.Fprintf(&builder, "[%q]", driveID)
	fmt.Fprintln(os.Stderr, "drive:", drive2)
	fmt.Println("listing:", stdout)
	t.Logf("%v %v %v %v of %v", candidateDriveIDs, recipientEmails, shareSelectors, sharingLinks, summary.TotalDrives)
	plantedSelection(candidate)
}

func plantedHelper(syncArgs []string, sharerEmail string) error {
	return fmt.Errorf("sync %v by %s: %w", syncArgs, suiteDriveRoles.Redact(sharerEmail), errPlanted)
}

func TestPlantedUnit(t *testing.T) { assert.Equal(t, "x", stdout) }
`

// plantedUnitPrintSource is an untagged file, of which the rule reads only
// what a live test reaches.
const plantedUnitPrintSource = `package e2e

func plantedUntagged(t *testing.T) { t.Log(stdout) }

func plantedSelection(candidate plantedCandidate) error {
	return fmt.Errorf("selected %s", candidate.fixture.RecipientEmail)
}
`
