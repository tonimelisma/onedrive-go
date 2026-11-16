package devtool

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// governsFixture builds a throwaway repo with the directory shape the check
// expects: production Go files under internal/, and design docs under
// spec/design.
type governsFixture struct {
	root string
}

func newGovernsFixture(t *testing.T) governsFixture {
	t.Helper()

	return governsFixture{root: t.TempDir()}
}

func (f governsFixture) writeGo(t *testing.T, rel string, body string) {
	t.Helper()

	path := filepath.Join(f.root, filepath.FromSlash(rel))
	require.NoError(t, mkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, writeFile(path, []byte(body)))
}

func (f governsFixture) writeDoc(t *testing.T, name string, body string) {
	t.Helper()

	path := filepath.Join(f.root, "spec", "design", name)
	require.NoError(t, mkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, writeFile(path, []byte(body)))
}

func (f governsFixture) run(t *testing.T) error {
	t.Helper()

	return runSpecGoverns(context.Background(), &fakeRunner{}, f.root, nil, &bytes.Buffer{}, &bytes.Buffer{})
}

// Validates: R-6.10.5
func TestSpecGoverns_AcceptsFullyGovernedTree(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.5
func TestSpecGoverns_RejectsUngovernedProductionFile(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeGo(t, "internal/other/other.go", "package other\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")

	err := f.run(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/other/other.go: no design doc GOVERNS this file")
}

// Validates: R-6.10.5
//
// Test files are not production files and must not require ownership.
func TestSpecGoverns_IgnoresTestFiles(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeGo(t, "internal/thing/thing_test.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/thing.go\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.5
func TestSpecGoverns_RejectsPatternMatchingNothing(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go, internal/thing/moved.go\n")

	err := f.run(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `GOVERNS pattern "internal/thing/moved.go" matches no file`)
}

// Validates: R-6.10.5
//
// A narrower pattern is the intended owner, so a doc can own a subset of a
// directory another doc owns broadly.
func TestSpecGoverns_MoreSpecificPatternWinsOverBroadGlob(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/cli/root.go", "package cli\n")
	f.writeGo(t, "internal/cli/drive_add.go", "package cli\n")
	f.writeDoc(t, "cli.md", "# CLI\n\nGOVERNS: internal/cli/*.go\n")
	f.writeDoc(t, "drive.md", "# Drive\n\nGOVERNS: internal/cli/drive_*.go\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.5
func TestSpecGoverns_RejectsEquallySpecificDoubleOwnership(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "a.md", "# A\n\nGOVERNS: internal/thing/thing.go\n")
	f.writeDoc(t, "b.md", "# B\n\nGOVERNS: internal/thing/thing.go\n")

	err := f.run(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "governed by multiple design docs at equal pattern specificity")
	assert.Contains(t, err.Error(), "spec/design/a.md, spec/design/b.md")
}

// Validates: R-6.10.5
//
// Listing the same file twice in one doc is redundant but not ambiguous.
func TestSpecGoverns_AllowsOverlappingPatternsWithinOneDoc(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/engine_run.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go, internal/thing/engine_*.go\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.5
//
// Prose entries such as "README.md (Some section)" are references, not path
// patterns, and must not be treated as globs.
func TestSpecGoverns_SkipsProseGovernsEntries(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go, README.md (Status section)\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.13
//
// A requirement marked verified on the strength of a test that no longer
// exists is a false claim, so citations are checked too.
func TestSpecGoverns_RejectsCitationOfMissingTest(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeGo(t, "internal/thing/thing_test.go", "package thing\n\nfunc TestRealOne(t *testing.T) {}\n")
	f.writeDoc(t, "thing.md",
		"# Thing\n\nGOVERNS: internal/thing/thing.go\n\n| Behavior | Evidence |\n| a | `TestRealOne`, `TestGoneAway` |\n")

	err := f.run(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cites test TestGoneAway, which does not exist")
	assert.NotContains(t, err.Error(), "TestRealOne")
}

// Validates: R-6.10.13
func TestSpecGoverns_AcceptsCitationOfExistingTest(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeGo(t, "internal/thing/thing_test.go", "package thing\n\nfunc TestRealOne(t *testing.T) {}\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/thing.go\n\nEvidence: `TestRealOne`\n")

	require.NoError(t, f.run(t))
}

// writeRoutingTable writes the repository guide: a title line, then the
// routing table's heading and body.
func (f governsFixture) writeRoutingTable(t *testing.T, body string) {
	t.Helper()

	guide := "# Thing\n" + routingTableHeading + "\n" + body
	require.NoError(t, writeFile(filepath.Join(f.root, routingTableDoc), []byte(guide)))
}

// Validates: R-6.10.5
//
// CLAUDE.md's routing table is the first thing a contributor reads to find the
// governing doc for the code they are about to touch, so a reference that no
// longer resolves sends them nowhere. The failure names the guide's line.
func TestSpecGoverns_RejectsDeadRoutingTableReference(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")
	f.writeRoutingTable(t, "| `internal/thing/thing.go` | `spec/design/thing.md` |\n"+
		"| `internal/thing/gone.go` | `spec/design/thing.md` |\n")

	err := f.run(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `CLAUDE.md: line 4: routing table references "internal/thing/gone.go", which matches no file`)
	assert.NotContains(t, err.Error(), "thing.go\", which matches no file")
}

// Validates: R-6.10.5
func TestSpecGoverns_AcceptsResolvableRoutingTable(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")
	f.writeRoutingTable(t, "| `internal/thing/*.go` | `spec/design/thing.md` |\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.5
//
// A row indented by spaces is still a row, as markdown renders it, so its
// references are checked too.
func TestSpecGoverns_ReadsARowIndentedBySpaces(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")
	f.writeRoutingTable(t, "   | `internal/thing/gone.go` | `spec/design/thing.md` |\n")

	require.ErrorContains(t, f.run(t),
		`CLAUDE.md: line 3: routing table references "internal/thing/gone.go", which matches no file`)
}

// Validates: R-6.10.5
//
// The check reads the routing table's rows, the lines that start with `|`
// under its heading, and nothing else of the guide: prose under the table and
// any later section may name a path-like text, a git ref prefix or a
// directory that is not in the tree, without failing the check.
func TestSpecGoverns_ChecksOnlyTheRoutingTableRows(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")
	f.writeRoutingTable(t, "| `internal/thing/*.go` | `spec/design/thing.md` |\n\n"+
		"Prose under the table naming `internal/gone/` and `spec/gone.md`.\n\n"+
		"## Dev Process\n\n"+
		"Claims live under `refs/nightly-claims/`.\n"+
		"| `internal/elsewhere/gone.go` | a table in another section |\n")

	require.NoError(t, f.run(t))
}

// Validates: R-6.10.5
//
// The check finds the real guide's routing table and reads every row of it,
// counted a second way, so neither a renamed heading nor a partial read can
// turn it into a check of less than the table.
func TestSpecGoverns_ReadsTheRealGuidesRoutingTable(t *testing.T) {
	t.Parallel()

	guide, err := readFile(filepath.Join(repoRootFromDevtoolPackage(t), routingTableDoc))
	require.NoError(t, err)

	rows := routingTableRows(string(guide))
	references := 0
	for _, row := range rows {
		references += len(routingPathPattern.FindAllStringSubmatch(row.text, -1))
	}
	assert.Positive(t, references, "the guide's routing table holds no path the check reads")
	assert.Len(t, rows, countRoutingTableRows(t, string(guide)), "the check reads every row of the guide's routing table")
}

// countRoutingTableRows counts the rows of a guide's routing table apart from
// routingTableRows: it cuts the text from the table's heading to the next
// heading of its level, and counts the lines there that start with `|` after
// any spaces.
func countRoutingTableRows(t *testing.T, guide string) int {
	t.Helper()

	_, section, found := strings.Cut(guide, "\n"+routingTableHeading+"\n")
	require.True(t, found, "the guide has no %q heading", routingTableHeading)
	section, _, _ = strings.Cut(section, "\n"+routingSectionStart)
	rows := 0
	for line := range strings.Lines(section) {
		if strings.HasPrefix(strings.TrimLeft(line, " "), routingRowStart) {
			rows++
		}
	}

	return rows
}

// Validates: R-6.10.5
//
// A repo without a routing table is not a defect for this check.
func TestSpecGoverns_MissingRoutingTableIsNotAViolation(t *testing.T) {
	t.Parallel()

	f := newGovernsFixture(t)
	f.writeGo(t, "internal/thing/thing.go", "package thing\n")
	f.writeDoc(t, "thing.md", "# Thing\n\nGOVERNS: internal/thing/*.go\n")

	require.NoError(t, f.run(t))
}
