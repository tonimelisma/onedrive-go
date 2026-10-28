package sync

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden reconciliation matrix (D-093). Every combination of what the
// baseline, the local tree and OneDrive hold at one path (and, for moves, at
// a second), planned by the real comparison SQL and planner, with the plan
// checked in as testdata/reconcile/matrix.golden. A change to any decision
// shows up as a reviewable diff of that table; refresh it with
// `go test ./internal/sync -run TestReconcileMatrix -update`.

//nolint:gochecknoglobals // flag registration is package-level by definition.
var updateGolden = flag.Bool("update", false, "rewrite golden files from current output")

const (
	reconcileMatrixGolden = "testdata/reconcile/matrix.golden"
	reconcileConvergeEnv  = "ONEDRIVE_GO_RECONCILE_CONVERGE"
)

// matrixSide is one side's state at the matrix path: absent, a file with
// one of the contents, or a folder.
type matrixSide string

const (
	matrixAbsent matrixSide = "-"
	matrixA      matrixSide = "A"
	matrixB      matrixSide = "B"
	matrixC      matrixSide = "C"
	matrixFolder matrixSide = "dir"
)

const matrixPath = "doc"

func (m matrixSide) content() string {
	switch m {
	case matrixA:
		return "content A"
	case matrixB:
		return "content B"
	case matrixC:
		return "content C"
	case matrixAbsent, matrixFolder:
	}

	return ""
}

// matrixCell builds one cell: the baseline's item (A or a folder) and each
// side's state at the path. A side matching the baseline is that synced
// item; anything else is a new item there.
func matrixCell(t *testing.T, baseline, local, remote matrixSide) *pmState {
	t.Helper()

	s := newPMState(t)
	if baseline != matrixAbsent {
		s.addSynced(matrixPath, baseline == matrixFolder, baseline.content())
	}
	s.setSide(true, matrixPath, baseline, local)
	s.setSide(false, matrixPath, baseline, remote)

	return s
}

// setSide makes one side hold want at p, editing the synced item in place
// when both are files (a content edit keeps the item) and replacing it
// otherwise.
func (s *pmState) setSide(isLocal bool, p string, baseline, want matrixSide) {
	side := s.remote
	if isLocal {
		side = s.local
	}
	current, exists := side[p]
	switch {
	case want == matrixAbsent:
		delete(side, p)
	case exists && want == baseline:
	case exists && !current.folder && want != matrixFolder:
		current.content = want.content()
		if !isLocal {
			current.etag = s.next()
		}
		side[p] = current
	default:
		item := pmItem{folder: want == matrixFolder, content: want.content()}
		if isLocal {
			item.inode = uint64(s.next())
		} else {
			item.id = fmt.Sprintf("item-%d", s.next())
			item.etag = s.next()
		}
		side[p] = item
	}
}

func renderMatrixPlan(plan *actionPlan) string {
	lines := make([]string, 0, len(plan.Actions)+len(plan.HeldDeletes))
	for i := range plan.Actions {
		lines = append(lines, actionLine(&plan.Actions[i]))
	}
	for _, held := range plan.HeldDeletes {
		lines = append(lines, fmt.Sprintf("held %v delete %s", held.Side, held.Path))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "(quiet)"
	}

	return strings.Join(lines, "; ")
}

// Validates: R-2.3.1, R-2.2.7, R-2.17.1
//
// Every single-path cell's plan is also checked for safety against the
// reference planner (assertPlanKeepsReferenceContent); with
// ONEDRIVE_GO_RECONCILE_CONVERGE=1 each is instead run to convergence and must
// end at exactly the reference's content (requireReferenceConvergence).
func TestReconcileMatrix_Golden(t *testing.T) {
	t.Parallel()

	// Each plan compiles the comparison SQL (about 0.1 s here), so running
	// every cell to convergence triples the cost: on demand only.
	converge := os.Getenv(reconcileConvergeEnv) != ""
	var out strings.Builder
	out.WriteString("# baseline | local | remote -> plan (one path; files A/B/C, dir = folder, - = absent)\n")
	sides := []matrixSide{matrixAbsent, matrixA, matrixB, matrixC, matrixFolder}
	for _, baseline := range []matrixSide{matrixAbsent, matrixA, matrixFolder} {
		for _, local := range sides {
			for _, remote := range sides {
				s := matrixCell(t, baseline, local, remote)
				plan := s.plan()
				fmt.Fprintf(&out, "%-3s | %-3s | %-3s -> %s\n", baseline, local, remote, renderMatrixPlan(plan))
				if converge {
					requireReferenceConvergence(t, s, plan, baseline, local, remote)
				} else {
					assertPlanKeepsReferenceContent(t, s, plan, baseline, local, remote)
				}
			}
		}
	}

	out.WriteString("\n# moves: baseline file A at doc; where each side holds it now\n")
	for _, local := range moveMatrixLocal() {
		for _, remote := range moveMatrixRemote() {
			s := newPMState(t)
			s.addSynced(matrixPath, false, matrixA.content())
			local.apply(s, true)
			remote.apply(s, false)
			fmt.Fprintf(&out, "local %-16s | remote %-16s -> %s\n", local.name, remote.name, renderMatrixPlan(s.plan()))
		}
	}

	requireGolden(t, reconcileMatrixGolden, out.String())
}

type moveCase struct {
	name  string
	apply func(s *pmState, isLocal bool)
}

func moveTo(to string, edit matrixSide) func(s *pmState, isLocal bool) {
	return func(s *pmState, isLocal bool) {
		side := s.remote
		if isLocal {
			side = s.local
		}
		if to != matrixPath {
			moveTree(side, matrixPath, to)
			if !isLocal {
				s.bumpRemote(to)
			}
		}
		if edit != matrixA {
			s.setSide(isLocal, to, matrixA, edit)
		}
	}
}

func moveMatrixLocal() []moveCase {
	return []moveCase{
		{name: "unchanged", apply: moveTo(matrixPath, matrixA)},
		{name: "moved doc2", apply: moveTo("doc2", matrixA)},
		{name: "moved+edit doc2", apply: moveTo("doc2", matrixB)},
		{name: "deleted", apply: moveTo(matrixPath, matrixAbsent)},
	}
}

func moveMatrixRemote() []moveCase {
	return []moveCase{
		{name: "unchanged", apply: moveTo(matrixPath, matrixA)},
		{name: "moved doc2", apply: moveTo("doc2", matrixA)},
		{name: "moved doc3", apply: moveTo("doc3", matrixA)},
		{name: "edited", apply: moveTo(matrixPath, matrixC)},
		{name: "moved+edit doc3", apply: moveTo("doc3", matrixC)},
		{name: "deleted", apply: moveTo(matrixPath, matrixAbsent)},
	}
}

func requireGolden(t *testing.T, name, actual string) {
	t.Helper()

	goldenPath := filepath.FromSlash(name)
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o750))
		require.NoError(t, os.WriteFile(goldenPath, []byte(actual), 0o600))
	}
	expected, err := os.ReadFile(goldenPath) //nolint:gosec // a checked-in golden file under testdata
	require.NoError(t, err, "golden file missing; run with -update")
	assert.Equal(t, string(expected), actual, "reconciliation decisions changed; review the diff, then rerun with -update")
}

// referenceSurvivors is the reference planner's answer for one matrix cell,
// as a set of file contents: a side's file content survives when that side
// changed it (created, edited, or replaced the path) or the other side left
// the path as synced. Which actions get there is the planner's business; the
// set they must end at is not.
func referenceSurvivors(baseline, local, remote matrixSide) map[string]bool {
	survivors := map[string]bool{}
	for _, side := range []struct{ mine, other matrixSide }{{local, remote}, {remote, local}} {
		isFile := side.mine != matrixAbsent && side.mine != matrixFolder
		if isFile && (side.mine != baseline || side.other == baseline) {
			survivors[side.mine.content()] = true
		}
	}

	return survivors
}

// assertPlanKeepsReferenceContent is the default-run check of a cell's plan,
// cheap because it plans nothing more: applied once to the pure model, the
// plan may not take from both sides any content the reference planner says
// must survive (a conflict copy or a download keeps it; a delete of the only
// copy loses it). The golden file pins what the plan is; this pins that it is
// safe.
func assertPlanKeepsReferenceContent(t *testing.T, s *pmState, plan *actionPlan, baseline, local, remote matrixSide) {
	t.Helper()

	s.apply(plan)
	kept := map[string]bool{}
	for _, view := range []map[string]string{pmView(s.local), pmView(s.remote)} {
		for _, content := range view {
			kept[content] = true
		}
	}
	for content := range referenceSurvivors(baseline, local, remote) {
		assert.True(t, kept[content], "%s|%s|%s: the plan loses %q:\n  %s",
			baseline, local, remote, content, strings.Join(s.log, "\n  "))
	}
}

// requireReferenceConvergence is the differential test against the
// reference planner: the cell, planned and applied to the pure model until
// quiet, converges to the file contents the reference says must survive, and
// nothing else. plan is the cell's first plan.
func requireReferenceConvergence(t *testing.T, s *pmState, plan *actionPlan, baseline, local, remote matrixSide) {
	t.Helper()

	cell := fmt.Sprintf("%s|%s|%s", baseline, local, remote)
	quiet := len(plan.Actions) == 0 && len(plan.HeldDeletes) == 0
	for round := 0; round < plannerModelMaxRounds && !quiet; round++ {
		s.apply(plan)
		plan = s.plan()
		quiet = len(plan.Actions) == 0 && len(plan.HeldDeletes) == 0
	}
	require.True(t, quiet, "%s: the plan never went quiet:\n  %s", cell, strings.Join(s.log, "\n  "))

	localView, remoteView := pmView(s.local), pmView(s.remote)
	require.Equal(t, remoteView, localView, "%s: sides differ:\n  %s", cell, strings.Join(s.log, "\n  "))
	got := map[string]bool{}
	for _, content := range localView {
		if content != modelFolderSentinel {
			got[content] = true
		}
	}
	assert.Equal(t, referenceSurvivors(baseline, local, remote), got, "%s:\n  %s", cell, strings.Join(s.log, "\n  "))
}
