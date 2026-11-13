package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The empty-list rule: a list in a --json document or line prints [], never
// null. Each output type that can hold a list has one normalizing step that
// fills its nil lists, and its printer hands the value over through it: the
// type's withEmptyLists method, or a function that builds the whole document
// (newStatusOutput). The two tests below hold the rule from both ends: every
// type a command prints (declared by the --json contract cases) is probed
// with nil lists at every depth, and every value the package hands to
// writeJSONDocument or writeJSONLine is found in the source, typed, and
// checked against those declarations.

// jsonResolveDepth bounds how far the source scan follows a value back
// through variables and calls before it gives up.
const jsonResolveDepth = 8

const withEmptyListsMethod = "withEmptyLists"

// jsonSchema is the Go type of one document, or one line, a command prints
// under --json, with the type's normalizing step when it has one: fill runs
// it on a value of the type, and filledBy is its name as the source calls it.
type jsonSchema struct {
	typ      reflect.Type
	fill     func(any) any
	filledBy string
}

// schemaOf declares T as a type a command prints, normalized by its
// withEmptyLists when it has one. The method may take a value or a pointer
// receiver; a pointer's method set holds both.
func schemaOf[T any]() jsonSchema {
	schema := jsonSchema{typ: reflect.TypeFor[T]()}

	var zero T
	if _, ok := any(&zero).(interface{ withEmptyLists() T }); ok {
		schema.filledBy = withEmptyListsMethod
		schema.fill = func(value any) any {
			typed, isT := value.(T)
			if !isT {
				return value
			}
			lists, isLists := any(&typed).(interface{ withEmptyLists() T })
			if !isLists {
				return value
			}

			return lists.withEmptyLists()
		}
	}

	return schema
}

// schemaBuiltBy declares T as a type a command prints whole from what
// builder returns: the function named builder is T's normalizing step, and
// build runs it again on the lists of a value of T.
func schemaBuiltBy[T any](builder string, build func(*T) T) jsonSchema {
	return jsonSchema{
		typ:      reflect.TypeFor[T](),
		filledBy: builder,
		fill: func(value any) any {
			typed, isT := value.(T)
			if !isT {
				return value
			}

			return build(&typed)
		},
	}
}

// statusOutputSchema declares the status document, which newStatusOutput
// builds whole from the accounts and normalizes once.
func statusOutputSchema() jsonSchema {
	return schemaBuiltBy("newStatusOutput", func(out *statusOutput) statusOutput {
		return newStatusOutput(out.Accounts)
	})
}

// schemaKey names a type the way the source scan does: pointers dropped,
// package-qualified (cli.lsJSONOutput, synccontrol.PerfCaptureResponse).
func schemaKey(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t.String()
}

// declaredJSONSchemas is every type the --json contract cases say a command
// prints, keyed by schemaKey.
func declaredJSONSchemas() map[string]jsonSchema {
	declared := map[string]jsonSchema{}
	for _, tc := range jsonContractCases() {
		for _, schema := range append([]jsonSchema{tc.schema}, tc.otherSchemas...) {
			if schema.typ != nil {
				declared[schemaKey(schema.typ)] = schema
			}
		}
	}

	return declared
}

// Every type a command prints is probed with its lists nil at each depth in
// turn, down to its deepest list (listDepth), and filled with one element
// above that depth, so the nested lists are reached. After the type's
// normalizing step, the encoded value holds no null where the type has a
// list. A type that can hold a list and has no normalizing step fails here,
// and so does one with a shape listDepth refuses: a list inside a recursive
// type or under a map not keyed by strings, an interface, a pointer to a
// list, an embedded unexported struct, a type that encodes itself. Only a
// type known to print a scalar whatever it holds (time.Time) may encode
// itself.
//
// Validates: R-6.6.33
func TestJSONOutputTypes_NoListPrintsNull(t *testing.T) {
	t.Parallel()

	declared := declaredJSONSchemas()
	keys := make([]string, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		schema := declared[key]
		if canPrintNullList(schema.typ) {
			require.NotNilf(t, schema.fill,
				"%s can hold a list but has no withEmptyLists: give it one that turns each nil list into an empty one", key)
		}

		deepest, err := listDepth(schema.typ)
		require.NoErrorf(t, err, "%s", key)
		for depth := range deepest + 2 {
			probe := listProbe(schema.typ, depth).Interface()
			if schema.fill != nil {
				probe = schema.fill(probe)
			}

			var encoded bytes.Buffer
			require.NoError(t, writeJSONDocument(&encoded, probe))
			var decoded any
			require.NoError(t, json.Unmarshal(encoded.Bytes(), &decoded))
			assert.Emptyf(t, nullListsIn(schema.typ, decoded, "$"),
				"%s with lists filled %d deep prints a list as null:\n%s", key, depth, encoded.String())
		}
	}
}

// The source side of the rule: every value the package hands to
// writeJSONDocument or writeJSONLine is found and typed from the source. Its
// type must be one a --json contract case declares, so the probes above cover
// it, and a type that can hold a list must be handed over through its
// normalizing step: x.withEmptyLists(), or a call of the function its
// declaration names. Every JSON-tagged type of the package must sit inside a
// declared type, so none escapes the probes, and every declared type must be
// printed somewhere, so the declarations do not go stale.
//
// Validates: R-6.6.33
func TestJSONOutputCalls_PrintDeclaredTypesWithTheirListsFilled(t *testing.T) {
	t.Parallel()

	src := parseCLISource(t)
	declared := declaredJSONSchemas()

	calls := src.encoderCalls()
	require.NotEmpty(t, calls, "the scan found no call of writeJSONDocument or writeJSONLine")

	printed := map[string]bool{}
	for _, call := range calls {
		typeName, filledBy, err := src.printedType(call)
		require.NoErrorf(t, err, "%s", call.pos)
		printed[typeName] = true

		schema, ok := declared[typeName]
		if !assert.Truef(t, ok, "%s prints %s, which no --json contract case declares: "+
			"add it to the case of the command that prints it", call.pos, typeName) {
			continue
		}
		if canPrintNullList(schema.typ) {
			assert.Equalf(t, schema.filledBy, filledBy,
				"%s prints %s, which can hold a list, without its normalizing step", call.pos, typeName)
		}
	}

	for typeName := range declared {
		assert.Truef(t, printed[typeName], "a --json contract case declares %s, which nothing in the package prints", typeName)
	}

	reachable := reachableCLITypes(declared)
	for _, name := range src.jsonTaggedTypes() {
		assert.Truef(t, reachable[name], "%s has JSON tags but no declared --json type holds it: "+
			"declare the type that prints it in the command's contract case", name)
	}
}

// --- what a type can print ---

type jsonField struct {
	name      string
	typ       reflect.Type
	omitEmpty bool
}

// jsonFields lists the fields encoding/json writes for struct type t, with
// embedded structs flattened as encoding/json does.
func jsonFields(t reflect.Type) []jsonField {
	var fields []jsonField
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}

		name, options, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				fields = append(fields, jsonFields(embedded)...)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}

		optionList := strings.Split(options, ",")
		fields = append(fields, jsonField{
			name:      name,
			typ:       field.Type,
			omitEmpty: slices.Contains(optionList, "omitempty") || slices.Contains(optionList, "omitzero"),
		})
	}

	return fields
}

func isByteSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
}

// isStructOrPointerToStruct reports whether an embedded field of type t has
// its fields flattened by encoding/json: a struct, or a pointer to one.
func isStructOrPointerToStruct(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t.Kind() == reflect.Struct
}

func encodesItself(t reflect.Type) bool {
	marshaler := reflect.TypeFor[json.Marshaler]()

	return t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler)
}

// printsScalarItself reports whether t, a type that encodes itself, is one
// known to print a scalar whatever it holds: time.Time, or a pointer to one.
// Every other type that encodes itself can print a list or a null in a list's
// place, and listDepth refuses it.
func printsScalarItself(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t == reflect.TypeFor[time.Time]()
}

// printsNullItself encodes itself and prints null: what a list behind a
// MarshalJSON method prints when the method passes a nil list on.
type printsNullItself struct{}

func (*printsNullItself) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// canPrintNullList reports whether a value of type t can print a list as
// null, judged by type: t is a list itself (a nil one prints null), or
// somewhere inside it a list is written without omitempty. Maps and pointers
// that print null are absent by meaning and are not lists. A type that
// encodes itself and an interface count as holding no list here; listDepth
// refuses the interface, and every type that encodes itself except one known
// to print a scalar.
func canPrintNullList(t reflect.Type) bool {
	return canPrintNullListSeen(t, map[reflect.Type]bool{})
}

func canPrintNullListSeen(t reflect.Type, seen map[reflect.Type]bool) bool {
	if encodesItself(t) {
		return false
	}

	// Only the container kinds can hold a list by type; every other kind,
	// an interface included, counts as holding none.
	switch kind := t.Kind(); {
	case kind == reflect.Pointer || kind == reflect.Array || kind == reflect.Map:
		return canPrintNullListSeen(t.Elem(), seen)
	case kind == reflect.Slice:
		return !isByteSlice(t)
	case kind == reflect.Struct && !seen[t]:
		seen[t] = true
		for _, field := range jsonFields(t) {
			if fieldCanPrintNullList(field, seen) {
				return true
			}
		}
	}

	return false
}

func fieldCanPrintNullList(field jsonField, seen map[reflect.Type]bool) bool {
	if field.typ.Kind() == reflect.Slice && !isByteSlice(field.typ) && field.omitEmpty {
		// An omitted empty list prints nothing; its elements still can.
		return canPrintNullListSeen(field.typ.Elem(), seen)
	}

	return canPrintNullListSeen(field.typ, seen)
}

// nullListsIn walks a decoded JSON value alongside its Go type and returns
// the path of every list that printed as null. What a type that encodes
// itself printed is not walked: listDepth lets through only one known to
// print a scalar.
func nullListsIn(t reflect.Type, value any, path string) []string {
	if encodesItself(t) {
		return nil
	}

	switch kind := t.Kind(); {
	case kind == reflect.Pointer && value != nil:
		// A null pointer is absent by meaning. A pointer to a list, whose
		// null would be a list, is refused by listDepth before any probe.
		return nullListsIn(t.Elem(), value, path)
	case (kind == reflect.Slice || kind == reflect.Array) && !isByteSlice(t):
		return nullListsInList(t, value, path)
	case kind == reflect.Map:
		return nullListsInMap(t, value, path)
	case kind == reflect.Struct:
		return nullListsInObject(t, value, path)
	}

	// Every other kind holds no list by type; listDepth refuses an
	// interface before any probe.
	return nil
}

// nullListsInList reports a slice that printed null and walks each element.
// An array always prints as an array.
func nullListsInList(t reflect.Type, value any, path string) []string {
	if value == nil && t.Kind() == reflect.Slice {
		return []string{path}
	}

	items, ok := value.([]any)
	if !ok {
		return nil
	}

	var found []string
	for i, item := range items {
		found = append(found, nullListsIn(t.Elem(), item, fmt.Sprintf("%s[%d]", path, i))...)
	}

	return found
}

func nullListsInMap(t reflect.Type, value any, path string) []string {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	var found []string
	for key, item := range object {
		found = append(found, nullListsIn(t.Elem(), item, path+"."+key)...)
	}

	return found
}

func nullListsInObject(t reflect.Type, value any, path string) []string {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	var found []string
	for _, field := range jsonFields(t) {
		if item, present := object[field.name]; present {
			found = append(found, nullListsIn(field.typ, item, path+"."+field.name)...)
		}
	}

	return found
}

// listDepth is how many container levels (pointers, lists, maps) listProbe
// must fill to reach the deepest list of t, or -1 when t holds no list: the
// probes at levels 0 to that number leave each of t's lists nil in turn. It
// walks t by type, with no depth limit, through the struct fields that do
// not carry json:"-", and fails, naming the type and the field, on a shape
// the probes cannot check: a list inside a recursive type, which no finite
// value fills to the bottom; a list under a map whose keys are not strings,
// which listProbe leaves nil; an interface, whose value its type does not
// tell; a pointer to a list, which prints null when the pointer is set and
// the list is nil; and an embedded unexported struct, whose exported fields
// encoding/json prints but listProbe cannot fill; and a type that encodes
// itself (json.RawMessage, any type with a MarshalJSON method), whose type
// does not tell what it prints. The one exception is a type printsScalarItself
// names (time.Time): it can only print a scalar, so it holds no list.
func listDepth(t reflect.Type) (int, error) {
	return listDepthFrom(t, map[reflect.Type]bool{}, map[reflect.Type]int{})
}

// listDepthFrom is listDepth with onPath, the struct types being walked
// (meeting one again is a recursion), and depths, the answer for each struct
// type already walked.
func listDepthFrom(t reflect.Type, onPath map[reflect.Type]bool, depths map[reflect.Type]int) (int, error) {
	if encodesItself(t) {
		if printsScalarItself(t) {
			return -1, nil
		}
		return 0, fmt.Errorf("%s encodes itself, so its type does not tell whether it prints a list: "+
			"print plain fields instead, or name it in printsScalarItself if it can only print a scalar", t)
	}

	// Only the container kinds can hold a list, and an interface can hold
	// anything; every other kind holds no list.
	switch kind := t.Kind(); {
	case kind == reflect.Interface:
		return 0, fmt.Errorf("%s is an interface, whose value its type does not tell", t)
	case kind == reflect.Pointer:
		if pointee := t.Elem(); pointee.Kind() == reflect.Slice && !isByteSlice(pointee) {
			return 0, fmt.Errorf("%s points to a list, which prints null when the pointer is set and the list is nil", t)
		}
		return levelBelow(t.Elem(), onPath, depths)
	case kind == reflect.Slice && !isByteSlice(t):
		below, err := levelBelow(t.Elem(), onPath, depths)
		return max(0, below), err
	case kind == reflect.Array:
		return listDepthFrom(t.Elem(), onPath, depths)
	case kind == reflect.Map:
		below, err := levelBelow(t.Elem(), onPath, depths)
		if err == nil && below >= 0 && t.Key().Kind() != reflect.String {
			return 0, fmt.Errorf("%s holds a list under a map keyed by %s, which the probe leaves nil", t, t.Key())
		}
		return below, err
	case kind == reflect.Struct:
		return structListDepth(t, onPath, depths)
	}

	return -1, nil
}

// levelBelow is the list depth of a container's element, one level down.
func levelBelow(elem reflect.Type, onPath map[reflect.Type]bool, depths map[reflect.Type]int) (int, error) {
	below, err := listDepthFrom(elem, onPath, depths)
	if err != nil || below < 0 {
		return below, err
	}

	return below + 1, nil
}

func structListDepth(t reflect.Type, onPath map[reflect.Type]bool, depths map[reflect.Type]int) (int, error) {
	if depth, walked := depths[t]; walked {
		return depth, nil
	}
	if onPath[t] {
		if canPrintNullList(t) {
			return 0, fmt.Errorf("%s holds a list inside a recursive type, which no probe fills to the bottom", t)
		}
		return -1, nil
	}

	onPath[t] = true
	deepest := -1
	for i := range t.NumField() {
		field := t.Field(i)
		if field.Tag.Get("json") == "-" {
			continue
		}
		if field.Anonymous && !field.IsExported() && isStructOrPointerToStruct(field.Type) {
			return 0, fmt.Errorf("%s.%s is an embedded unexported struct, whose exported fields encoding/json prints but listProbe cannot fill",
				t, field.Name)
		}
		if !field.IsExported() {
			continue
		}
		depth, err := listDepthFrom(field.Type, onPath, depths)
		if err != nil {
			return 0, fmt.Errorf("%s.%s: %w", t, field.Name, err)
		}
		deepest = max(deepest, depth)
	}
	delete(onPath, t)
	depths[t] = deepest

	return deepest, nil
}

// listDepth finds the deepest list of a type however deep it sits, skips a
// field encoding/json never prints, and refuses, naming the type and the
// field, each shape the probes cannot check: a list inside a recursive type
// or under a map whose keys are not strings, an interface, a pointer to a
// list, an embedded unexported struct, and a type that encodes itself, a
// struct that does so by embedding a time included. A time, and a pointer to
// one, hold no list.
//
// Validates: R-6.6.33
func TestListDepth_FindsTheDeepestList(t *testing.T) {
	t.Parallel()

	type flat struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	type leaf struct {
		Names []string `json:"names"`
	}
	type deep struct {
		Levels [][][][][][][]leaf `json:"levels"`
	}
	type chain struct {
		Name string `json:"name"`
		Next *chain `json:"next"`
	}
	type tree struct {
		Children []tree `json:"children"`
	}
	type neverPrinted struct {
		Extra any      `json:"-"`
		Names []string `json:"-"`
	}
	type withAny struct {
		Extra any `json:"extra"`
	}
	type withListPointer struct {
		Names *[]string `json:"names"`
	}
	type hidden struct {
		Names []string `json:"names"`
	}
	type embedsHidden struct {
		hidden
	}
	type withTimes struct {
		At    time.Time  `json:"at"`
		Until *time.Time `json:"until"`
	}
	type withRaw struct {
		Raw json.RawMessage `json:"raw"`
	}
	type withOwnEncoding struct {
		Names *printsNullItself `json:"names"`
	}
	type embedsTime struct {
		time.Time

		Names []string `json:"names"`
	}

	reaches := map[string]struct {
		typ   reflect.Type
		depth int
	}{
		"no list":                         {typ: reflect.TypeFor[flat](), depth: -1},
		"a list":                          {typ: reflect.TypeFor[[]string](), depth: 0},
		"a pointer to a struct of lists":  {typ: reflect.TypeFor[*leaf](), depth: 1},
		"a map of lists":                  {typ: reflect.TypeFor[map[string][]string](), depth: 1},
		"an array holds no level":         {typ: reflect.TypeFor[[2][]string](), depth: 0},
		"seven lists deep":                {typ: reflect.TypeFor[deep](), depth: 7},
		"a recursion with no list":        {typ: reflect.TypeFor[chain](), depth: -1},
		"fields encoding/json never sees": {typ: reflect.TypeFor[neverPrinted](), depth: -1},
		"times, which print a scalar":     {typ: reflect.TypeFor[withTimes](), depth: -1},
	}
	for name, tc := range reaches {
		depth, err := listDepth(tc.typ)
		require.NoErrorf(t, err, "%s", name)
		assert.Equalf(t, tc.depth, depth, "%s", name)
	}

	refused := map[string]struct {
		typ   reflect.Type
		names string
	}{
		"a recursive type":               {typ: reflect.TypeFor[tree](), names: "tree.Children"},
		"a map keyed by a number":        {typ: reflect.TypeFor[map[int][]string](), names: "map[int][]string"},
		"a list in a pointer loop":       {typ: reflect.TypeFor[struct{ Root *tree }](), names: ".Root"},
		"an interface field":             {typ: reflect.TypeFor[withAny](), names: "withAny.Extra"},
		"a pointer to a list":            {typ: reflect.TypeFor[withListPointer](), names: "withListPointer.Names"},
		"an embedded unexported struct":  {typ: reflect.TypeFor[embedsHidden](), names: "embedsHidden.hidden"},
		"an interface below a container": {typ: reflect.TypeFor[map[string][]any](), names: "interface"},
		"raw JSON":                       {typ: reflect.TypeFor[withRaw](), names: "withRaw.Raw"},
		"a type with its own encoding":   {typ: reflect.TypeFor[withOwnEncoding](), names: "withOwnEncoding.Names"},
		"a struct that embeds a time":    {typ: reflect.TypeFor[embedsTime](), names: "embedsTime encodes itself"},
	}
	for name, tc := range refused {
		_, err := listDepth(tc.typ)
		require.ErrorContainsf(t, err, tc.names, "%s", name)
	}
}

// listProbe builds a value of type t whose lists, maps and pointers hold one
// element down to levels containers deep and are nil below that. An array
// is never nil, so it takes no level: its elements are probed at its own.
// Only exported struct fields are filled; listDepth refuses an embedded
// unexported struct, whose exported fields encoding/json prints.
func listProbe(t reflect.Type, levels int) reflect.Value {
	value := reflect.New(t).Elem()
	if encodesItself(t) {
		return value
	}

	// Only the container kinds are filled; every other kind, a type that
	// encodes itself and an interface included, keeps its zero.
	switch kind := t.Kind(); {
	case kind == reflect.Pointer && levels > 0:
		value.Set(listProbe(t.Elem(), levels-1).Addr())
	case kind == reflect.Slice && levels > 0 && !isByteSlice(t):
		list := reflect.MakeSlice(t, 1, 1)
		list.Index(0).Set(listProbe(t.Elem(), levels-1))
		value.Set(list)
	case kind == reflect.Map && levels > 0 && t.Key().Kind() == reflect.String:
		entries := reflect.MakeMapWithSize(t, 1)
		entries.SetMapIndex(reflect.New(t.Key()).Elem(), listProbe(t.Elem(), levels-1))
		value.Set(entries)
	case kind == reflect.Array:
		for i := range t.Len() {
			value.Index(i).Set(listProbe(t.Elem(), levels))
		}
	case kind == reflect.Struct:
		for i := range t.NumField() {
			if t.Field(i).IsExported() {
				value.Field(i).Set(listProbe(t.Field(i).Type, levels))
			}
		}
	}

	return value
}

// reachableCLITypes names every type of this package that a declared type
// holds, at any depth. A generic type is named without its type arguments,
// as its declaration names it.
func reachableCLITypes(declared map[string]jsonSchema) map[string]bool {
	pkgPath := reflect.TypeFor[jsonSchema]().PkgPath()
	reached := map[string]bool{}
	seen := map[reflect.Type]bool{}

	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		if t.PkgPath() == pkgPath && t.Name() != "" {
			declaredName, _, _ := strings.Cut(t.Name(), "[")
			reached[declaredName] = true
		}
		for _, inner := range innerTypes(t) {
			walk(inner)
		}
	}

	for _, schema := range declared {
		walk(schema.typ)
	}

	return reached
}

// innerTypes lists the types t is built from: a container's element, a
// map's key as well, a struct's fields. Any other kind is built from none.
func innerTypes(t reflect.Type) []reflect.Type {
	kind := t.Kind()
	if kind == reflect.Struct {
		inner := make([]reflect.Type, 0, t.NumField())
		for i := range t.NumField() {
			inner = append(inner, t.Field(i).Type)
		}
		return inner
	}
	if kind == reflect.Map {
		return []reflect.Type{t.Key(), t.Elem()}
	}
	if kind == reflect.Pointer || kind == reflect.Slice || kind == reflect.Array {
		return []reflect.Type{t.Elem()}
	}

	return nil
}

// --- the package source ---

// cliSource is the package's non-test source, indexed for the two questions
// the scan asks: which values reach the encoder, and of what type.
//
// The scan reads syntax, not checked types, and trusts three things it does
// not prove. A name in a function means its first declaration there (a
// receiver, parameter or named result, else the first variable of that name
// the body declares), so a later variable that shadows it is typed as the
// first. An encoder call is a call of writeJSONDocument or writeJSONLine by
// name inside a function declaration's body, so a call through a function
// value, or in a function literal outside any function, is not found. And
// x.m() returns what the package's method m returns when the package has
// exactly one method of that name, whatever x is, even another package's
// value.
type cliSource struct {
	fset    *token.FileSet
	files   []*ast.File
	types   map[string]*ast.TypeSpec
	funcs   map[string]*ast.FuncDecl
	methods map[string][]*ast.FuncDecl
}

// jsonEncoderCall is one call of writeJSONDocument or writeJSONLine.
type jsonEncoderCall struct {
	pos   token.Position
	fn    *ast.FuncDecl
	value ast.Expr
}

func parseCLISource(t *testing.T) *cliSource {
	t.Helper()

	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)

	src := &cliSource{
		fset:    token.NewFileSet(),
		types:   map[string]*ast.TypeSpec{},
		funcs:   map[string]*ast.FuncDecl{},
		methods: map[string][]*ast.FuncDecl{},
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(src.fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)
		src.files = append(src.files, file)
		src.index(file)
	}

	return src
}

func (src *cliSource) index(file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					src.types[typeSpec.Name.Name] = typeSpec
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				src.funcs[d.Name.Name] = d
			} else {
				src.methods[d.Name.Name] = append(src.methods[d.Name.Name], d)
			}
		}
	}
}

func (src *cliSource) encoderCalls() []jsonEncoderCall {
	var calls []jsonEncoderCall
	for _, file := range src.files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall || len(call.Args) != 2 {
					return true
				}
				if name, isIdent := call.Fun.(*ast.Ident); isIdent && (name.Name == "writeJSONDocument" || name.Name == "writeJSONLine") {
					calls = append(calls, jsonEncoderCall{pos: src.fset.Position(call.Pos()), fn: fn, value: call.Args[1]})
				}
				return true
			})
		}
	}

	return calls
}

// printedType names the type of the value call prints, spelled as schemaKey
// spells it, and the step the value comes through, as a jsonSchema's
// filledBy names it: withEmptyLists for x.withEmptyLists(), a function's name
// for that function's result, empty for anything else.
func (src *cliSource) printedType(call jsonEncoderCall) (typeName, filledBy string, err error) {
	value := call.value
	if method, ok := value.(*ast.CallExpr); ok {
		switch f := method.Fun.(type) {
		case *ast.SelectorExpr:
			if f.Sel.Name == withEmptyListsMethod && len(method.Args) == 0 {
				filledBy = withEmptyListsMethod
				value = f.X
			}
		case *ast.Ident:
			filledBy = f.Name
		}
	}

	typeExpr, err := src.typeOf(call.fn, value, 0)
	if err != nil {
		return "", "", err
	}

	typeName, err = src.typeName(typeExpr)

	return typeName, filledBy, err
}

// typeOf reads the type of expr inside fn from the source: a composite
// literal's type, a parameter's or variable's declared type, a struct field's
// declared type, or what the called function or method returns.
func (src *cliSource) typeOf(fn *ast.FuncDecl, expr ast.Expr, depth int) (ast.Expr, error) {
	if depth > jsonResolveDepth {
		return nil, fmt.Errorf("cannot tell the type of %s", types.ExprString(expr))
	}

	switch e := expr.(type) {
	case *ast.ParenExpr:
		return src.typeOf(fn, e.X, depth+1)
	case *ast.CompositeLit:
		if e.Type != nil {
			return e.Type, nil
		}
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return src.typeOf(fn, e.X, depth+1)
		}
	case *ast.CallExpr:
		return src.resultOf(fn, e, 0, depth+1)
	case *ast.Ident:
		return src.localType(fn, e.Name, depth+1)
	case *ast.SelectorExpr:
		return src.fieldType(fn, e, depth+1)
	}

	return nil, fmt.Errorf("cannot tell the type of %s: print a variable, a field, a composite literal or a call's result",
		types.ExprString(expr))
}

// fieldType is the declared type of the field selector names, in the
// package's struct type that selector.X holds (or points to).
func (src *cliSource) fieldType(fn *ast.FuncDecl, selector *ast.SelectorExpr, depth int) (ast.Expr, error) {
	holder, err := src.typeOf(fn, selector.X, depth)
	if err != nil {
		return nil, err
	}
	if pointer, isPointer := holder.(*ast.StarExpr); isPointer {
		holder = pointer.X
	}

	var structType *ast.StructType
	if name, isName := holder.(*ast.Ident); isName {
		if spec, declared := src.types[name.Name]; declared {
			if declaredStruct, isStruct := spec.Type.(*ast.StructType); isStruct {
				structType = declaredStruct
			}
		}
	}
	if structType == nil {
		return nil, fmt.Errorf("cannot tell the type of %s: %s is not a struct type of this package",
			types.ExprString(selector), types.ExprString(holder))
	}

	for _, field := range structType.Fields.List {
		for _, ident := range field.Names {
			if ident.Name == selector.Sel.Name {
				return field.Type, nil
			}
		}
	}

	return nil, fmt.Errorf("cannot tell the type of %s: %s has no field %s",
		types.ExprString(selector), types.ExprString(holder), selector.Sel.Name)
}

// resultOf is the type of the index-th result of call.
func (src *cliSource) resultOf(fn *ast.FuncDecl, call *ast.CallExpr, index, depth int) (ast.Expr, error) {
	var callee *ast.FuncDecl
	switch f := call.Fun.(type) {
	case *ast.Ident:
		if f.Name == "make" && len(call.Args) > 0 {
			return call.Args[0], nil
		}
		callee = src.funcs[f.Name]
	case *ast.SelectorExpr:
		if f.Sel.Name == withEmptyListsMethod {
			return src.typeOf(fn, f.X, depth+1)
		}
		if candidates := src.methods[f.Sel.Name]; len(candidates) == 1 {
			callee = candidates[0]
		}
	}
	if callee == nil || callee.Type.Results == nil {
		return nil, fmt.Errorf("cannot tell what %s returns", types.ExprString(call.Fun))
	}

	var results []ast.Expr
	for _, field := range callee.Type.Results.List {
		for range max(1, len(field.Names)) {
			results = append(results, field.Type)
		}
	}
	if index >= len(results) {
		return nil, fmt.Errorf("%s returns fewer than %d values", callee.Name.Name, index+1)
	}

	return results[index], nil
}

// localType is the declared type of name in fn: a receiver, parameter or
// named result, or the first variable of that name the body declares.
func (src *cliSource) localType(fn *ast.FuncDecl, name string, depth int) (ast.Expr, error) {
	for _, list := range []*ast.FieldList{fn.Recv, fn.Type.Params, fn.Type.Results} {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, ident := range field.Names {
				if ident.Name == name {
					return field.Type, nil
				}
			}
		}
	}

	var (
		found    ast.Expr
		foundErr error
		decided  bool
	)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if decided {
			return false
		}
		found, decided, foundErr = src.declaredIn(fn, node, name, depth)

		return !decided
	})
	if !decided {
		return nil, fmt.Errorf("cannot find where %s is declared in %s", name, fn.Name.Name)
	}

	return found, foundErr
}

// declaredIn types name when node declares it (name := ..., var name T);
// decided is false when node declares no variable of that name.
func (src *cliSource) declaredIn(fn *ast.FuncDecl, node ast.Node, name string, depth int) (typeExpr ast.Expr, decided bool, err error) {
	switch n := node.(type) {
	case *ast.AssignStmt:
		if n.Tok != token.DEFINE {
			return nil, false, nil
		}
		for i, lhs := range n.Lhs {
			if ident, ok := lhs.(*ast.Ident); !ok || ident.Name != name {
				continue
			}
			if len(n.Rhs) == len(n.Lhs) {
				typeExpr, err = src.typeOf(fn, n.Rhs[i], depth+1)
				return typeExpr, true, err
			}
			if call, isCall := n.Rhs[0].(*ast.CallExpr); isCall {
				typeExpr, err = src.resultOf(fn, call, i, depth+1)
				return typeExpr, true, err
			}
		}
	case *ast.ValueSpec:
		for i, ident := range n.Names {
			if ident.Name != name {
				continue
			}
			if n.Type != nil {
				return n.Type, true, nil
			}
			if i < len(n.Values) {
				typeExpr, err = src.typeOf(fn, n.Values[i], depth+1)
				return typeExpr, true, err
			}
		}
	}

	return nil, false, nil
}

// typeName spells a type expression as schemaKey does.
func (src *cliSource) typeName(expr ast.Expr) (string, error) {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return src.typeName(e.X)
	case *ast.Ident:
		if _, ok := src.types[e.Name]; ok {
			return "cli." + e.Name, nil
		}
		return "", fmt.Errorf("prints a value of type %s, which is not a type of this package", e.Name)
	case *ast.SelectorExpr:
		if pkg, ok := e.X.(*ast.Ident); ok {
			return pkg.Name + "." + e.Sel.Name, nil
		}
	case *ast.ArrayType:
		if e.Len == nil {
			elem, err := src.typeName(e.Elt)
			return "[]" + elem, err
		}
	case *ast.IndexExpr:
		return src.instanceName(e)
	}

	return "", fmt.Errorf("cannot name the type %s", types.ExprString(expr))
}

// instanceName spells a generic type of this package with one type argument
// of this package as reflect does: the argument carries the package's import
// path (cli.folderTransferJSONOutput[<import path>.getJSONOutput]).
func (src *cliSource) instanceName(instance *ast.IndexExpr) (string, error) {
	generic, err := src.typeName(instance.X)
	if err != nil {
		return "", err
	}

	argument, isName := instance.Index.(*ast.Ident)
	if !isName {
		return "", fmt.Errorf("cannot name the type %s: its type argument is not a type of this package",
			types.ExprString(instance))
	}
	if _, err := src.typeName(argument); err != nil {
		return "", err
	}

	return generic + "[" + reflect.TypeFor[jsonSchema]().PkgPath() + "." + argument.Name + "]", nil
}

// jsonTaggedTypes names every struct type of the package with a field
// encoding/json writes under a tag.
func (src *cliSource) jsonTaggedTypes() []string {
	var names []string
	for name, spec := range src.types {
		structType, ok := spec.Type.(*ast.StructType)
		if !ok {
			continue
		}
		for _, field := range structType.Fields.List {
			if field.Tag != nil && hasJSONName(field.Tag.Value) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)

	return names
}

func hasJSONName(rawTag string) bool {
	tag := reflect.StructTag(strings.Trim(rawTag, "`")).Get("json")
	name, _, _ := strings.Cut(tag, ",")

	return tag != "" && name != "-"
}
