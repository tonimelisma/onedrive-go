package sync

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Model-based randomized testing in the style of Dropbox's CanopyCheck: each
// batch applies random independent edits to the local tree and to the fake
// OneDrive, syncs until quiet, and checks invariants every correct
// bidirectional sync must hold:
//
//  1. Convergence: the local and remote trees are identical.
//  2. No content loss: every file version written during the batch, on
//     either side, still exists somewhere (an edit beats a delete; a
//     conflict keeps both versions).
//  3. Untouched files are untouched: a file neither side changed, and whose
//     ancestors neither side moved or deleted, keeps its path and content.
//  4. Quiescence: once converged, the next plan is empty.
//
// Failures print the seed; rerun one with ONEDRIVE_GO_MODEL_SEED=<seed> and
// widen the search with ONEDRIVE_GO_MODEL_SEEDS=<count>.
//
// Under -race the default drops to modelRaceSeeds. The race detector makes each
// seed 1-2 minutes on a 2-core host (4 seeds measured 204 s), and 12 pushed
// internal/sync past go test's 10-minute package timeout. The race run is there
// to catch data races, which two parallel seeds exercise as well as twelve; seed
// breadth belongs to non-race runs and to ONEDRIVE_GO_MODEL_SEEDS.

const (
	modelBatches             = 6
	modelMaxOpsPerSide       = 3
	modelMaxSyncPasses       = 8
	modelDefaultSeeds        = 12
	modelRaceSeeds           = 2
	modelInitialFolders      = 3
	modelInitialFiles        = 5
	modelFolderSentinel      = "/"
	modelSeedEnv             = "ONEDRIVE_GO_MODEL_SEED"
	modelSeedCountEnv        = "ONEDRIVE_GO_MODEL_SEEDS"
	modelTraceEnv            = "ONEDRIVE_GO_MODEL_TRACE"
	modelConflictMarker      = ".conflict-"
	modelLocalSide           = "local"
	modelRemoteSide          = "remote"
	modelFileNamePool        = "a.txt b.txt c.md"
	modelFolderNamePool      = "docs photos x"
	modelOpCreateFile        = "create-file"
	modelOpEditFile          = "edit-file"
	modelOpDelete            = "delete"
	modelOpMove              = "move"
	modelOpMakeFolder        = "mkdir"
	modelNotConvergedMessage = "sync did not converge"
)

// Validates: R-2.3.1, R-2.17.1, R-2.17.2, R-2.17.3, R-2.17.6, R-2.17.7, R-2.17.8
func TestRunOnce_RandomizedBidirectionalEditsConverge(t *testing.T) {
	t.Parallel()

	for _, seed := range modelSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			runModelScenario(t, seed)
		})
	}
}

func modelSeeds(t *testing.T) []uint64 {
	t.Helper()

	if raw := os.Getenv(modelSeedEnv); raw != "" {
		seed, err := strconv.ParseUint(raw, 10, 64)
		require.NoError(t, err, modelSeedEnv)
		return []uint64{seed}
	}
	count := modelDefaultSeeds
	if raceDetectorEnabled {
		count = modelRaceSeeds
	}
	if raw := os.Getenv(modelSeedCountEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		require.NoError(t, err, modelSeedCountEnv)
		count = parsed
	}
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = uint64(i + 1)
	}

	return seeds
}

// modelEdit is one operation a side applied, recorded for the invariants.
type modelEdit struct {
	side    string
	kind    string
	path    string
	to      string // destination of a move
	content string // bytes written by create/edit
}

// touches reports whether the edit affected p, its ancestors or its subtree.
func (e modelEdit) touches(p string) bool {
	for _, affected := range []string{e.path, e.to} {
		if affected == "" {
			continue
		}
		if p == affected || strings.HasPrefix(p, affected+"/") || strings.HasPrefix(affected, p+"/") {
			return true
		}
	}

	return false
}

type modelRun struct {
	t        *testing.T
	seed     uint64
	rng      *rand.Rand
	remote   *moveScenarioRemote
	eng      *testEngine
	syncRoot string
	writes   int
	log      []string
}

func runModelScenario(t *testing.T, seed uint64) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	run := &modelRun{
		t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // deterministic test RNG
		remote: remote, eng: eng, syncRoot: syncRoot,
	}

	run.seedInitialTree()
	run.syncUntilQuiet("initial")
	run.requireConverged("initial")

	for batch := 1; batch <= modelBatches; batch++ {
		before := run.localTree()
		var edits []modelEdit
		for range run.rng.IntN(modelMaxOpsPerSide + 1) {
			if edit, ok := run.localEdit(); ok {
				edits = append(edits, edit)
			}
		}
		for range run.rng.IntN(modelMaxOpsPerSide + 1) {
			if edit, ok := run.remoteEdit(); ok {
				edits = append(edits, edit)
			}
		}

		label := fmt.Sprintf("batch %d", batch)
		run.syncUntilQuiet(label)
		converged := run.requireConverged(label)
		run.requireWrittenContentSurvives(label, edits, converged)
		run.requireUntouchedFilesUnchanged(label, before, edits, converged)
		requireScenarioQuietPlan(t, eng)
	}
}

func (r *modelRun) logf(format string, args ...any) {
	r.log = append(r.log, fmt.Sprintf(format, args...))
}

func (r *modelRun) failureContext() string {
	return fmt.Sprintf("seed %d (rerun with %s=%d); operations:\n  %s",
		r.seed, modelSeedEnv, r.seed, strings.Join(r.log, "\n  "))
}

func (r *modelRun) nextContent(side string) string {
	r.writes++
	return fmt.Sprintf("seed %d %s write %d\n", r.seed, side, r.writes)
}

// seedInitialTree builds a random starting tree on OneDrive.
func (r *modelRun) seedInitialTree() {
	for range modelInitialFolders {
		if folder := r.newPathIn(r.remoteFolders(), modelFolderNamePool, r.remoteTreeView()); folder != "" {
			r.remote.mkdirAt(r.t, folder)
			r.logf("setup remote mkdir %s", folder)
		}
	}
	for range modelInitialFiles {
		if file := r.newPathIn(r.remoteFolders(), modelFileNamePool, r.remoteTreeView()); file != "" {
			r.remote.putFileAt(r.t, file, r.nextContent(modelRemoteSide))
			r.logf("setup remote put %s", file)
		}
	}
}

// syncUntilQuiet runs passes until one transfers and changes nothing. Early
// passes may report failures (a move racing a delete is retried); the last
// one must be clean.
//
// The model's trees are small, so random deletes often empty a side and trip
// the delete-safety hold. The model plays a user who confirms: a pass that
// held deletes is followed by a `--confirm-deletes` pass, and the held pass
// itself counts as a change so the loop keeps going.
func (r *modelRun) syncUntilQuiet(label string) {
	confirm := false
	for pass := 1; pass <= modelMaxSyncPasses; pass++ {
		report, err := r.eng.RunOnce(r.t.Context(), SyncBidirectional, RunOptions{ConfirmDeletes: confirm})
		require.NoError(r.t, err, "%s pass %d: %s", label, pass, r.failureContext())
		held := report.HeldLocalDeletes + report.HeldRemoteDeletes
		confirm = held > 0
		changed := report.Uploads + report.Downloads + report.LocalDeletes + report.RemoteDeletes +
			report.FolderCreates + report.Moves + held
		if os.Getenv(modelTraceEnv) != "" {
			r.t.Logf("%s pass %d: up=%d down=%d ldel=%d rdel=%d mkdir=%d mv=%d ok=%d failed=%d errors=%v\n  local:  %v\n  remote: %v",
				label, pass, report.Uploads, report.Downloads, report.LocalDeletes, report.RemoteDeletes,
				report.FolderCreates, report.Moves, report.Succeeded, report.Failed, report.Errors,
				modelPaths(r.localTree()), modelPaths(r.remote.tree()))
		}
		if changed == 0 && report.Failed == 0 {
			return
		}
	}
	require.Failf(r.t, modelNotConvergedMessage, "%s: still changing after %d passes; %s",
		label, modelMaxSyncPasses, r.failureContext())
}

func (r *modelRun) requireConverged(label string) map[string]string {
	local := r.localTree()
	remote := r.remote.tree()
	require.Equal(r.t, remote, local, "%s: local and OneDrive differ; %s", label, r.failureContext())

	return local
}

// requireWrittenContentSurvives checks invariant 2: content a side wrote and
// still held when the batch ended exists somewhere after sync.
func (r *modelRun) requireWrittenContentSurvives(label string, edits []modelEdit, converged map[string]string) {
	present := make(map[string]bool, len(converged))
	for _, content := range converged {
		present[content] = true
	}
	for i, edit := range edits {
		if edit.content == "" || modelOverwrittenLater(edits, i) {
			continue
		}
		assert.True(r.t, present[edit.content],
			"%s: %s %s of %s was lost; %s", label, edit.side, edit.kind, edit.path, r.failureContext())
	}
}

// modelOverwrittenLater reports whether the same side later replaced or
// removed what edits[i] wrote, in which case losing it is correct.
func modelOverwrittenLater(edits []modelEdit, i int) bool {
	for _, later := range edits[i+1:] {
		if later.side != edits[i].side {
			continue
		}
		if later.touches(edits[i].path) && later.kind != modelOpMakeFolder {
			return true
		}
	}

	return false
}

// requireUntouchedFilesUnchanged checks invariant 3.
func (r *modelRun) requireUntouchedFilesUnchanged(
	label string,
	before map[string]string,
	edits []modelEdit,
	converged map[string]string,
) {
	for p, content := range before {
		if content == modelFolderSentinel {
			continue
		}
		touched := false
		for _, edit := range edits {
			if edit.touches(p) {
				touched = true
				break
			}
		}
		if touched {
			continue
		}
		assert.Equal(r.t, content, converged[p], "%s: untouched %s changed; %s", label, p, r.failureContext())
	}
}

// localTree is the sync root as path -> content, folders as "/", ignoring
// the engine's own transfer artifacts.
func (r *modelRun) localTree() map[string]string {
	out := map[string]string{}
	require.NoError(r.t, filepath.WalkDir(r.syncRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(r.syncRoot, p)
		if relErr != nil {
			return fmt.Errorf("relative path of %s: %w", p, relErr)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel] = modelFolderSentinel
			return nil
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // test reads its own temp tree
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		out[rel] = string(data)
		return nil
	}))

	return out
}

func (r *modelRun) remoteTreeView() map[string]string { return r.remote.tree() }

func (r *modelRun) remoteFolders() []string { return modelFoldersOf(r.remote.tree()) }

func (r *modelRun) localFolders() []string { return modelFoldersOf(r.localTree()) }

func modelFoldersOf(tree map[string]string) []string {
	folders := []string{""}
	for p, content := range tree {
		if content == modelFolderSentinel {
			folders = append(folders, p)
		}
	}
	sort.Strings(folders)

	return folders
}

func modelEntriesOf(tree map[string]string, wantFolders bool) []string {
	var entries []string
	for p, content := range tree {
		if (content == modelFolderSentinel) == wantFolders {
			entries = append(entries, p)
		}
	}
	sort.Strings(entries)

	return entries
}

func (r *modelRun) pick(options []string) string {
	if len(options) == 0 {
		return ""
	}

	return options[r.rng.IntN(len(options))]
}

// newPathIn picks a random folder and a pool name not yet taken there. Names
// are compared case-insensitively, as OneDrive does.
func (r *modelRun) newPathIn(folders []string, pool string, tree map[string]string) string {
	folder := r.pick(folders)
	name := r.pick(strings.Fields(pool))
	candidate := path.Join(folder, name)
	for p := range tree {
		if strings.EqualFold(p, candidate) {
			return ""
		}
	}

	return candidate
}

func (r *modelRun) localEdit() (modelEdit, bool) {
	tree := r.localTree()
	switch r.pick([]string{modelOpCreateFile, modelOpEditFile, modelOpDelete, modelOpMove, modelOpMakeFolder}) {
	case modelOpCreateFile:
		p := r.newPathIn(r.localFolders(), modelFileNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		content := r.nextContent(modelLocalSide)
		writeLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
		r.logf("local create %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpCreateFile, path: p, content: content}, true
	case modelOpEditFile:
		p := r.pick(modelEntriesOf(tree, false))
		if p == "" {
			return modelEdit{}, false
		}
		content := r.nextContent(modelLocalSide)
		writeLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
		r.logf("local edit %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpEditFile, path: p, content: content}, true
	case modelOpDelete:
		p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
		if p == "" {
			return modelEdit{}, false
		}
		require.NoError(r.t, os.RemoveAll(filepath.Join(r.syncRoot, filepath.FromSlash(p))))
		r.logf("local delete %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpDelete, path: p}, true
	case modelOpMove:
		return r.localMove(tree)
	default:
		p := r.newPathIn(r.localFolders(), modelFolderNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		require.NoError(r.t, os.MkdirAll(filepath.Join(r.syncRoot, filepath.FromSlash(p)), 0o750))
		r.logf("local mkdir %s", p)
		return modelEdit{side: modelLocalSide, kind: modelOpMakeFolder, path: p}, true
	}
}

func (r *modelRun) localMove(tree map[string]string) (modelEdit, bool) {
	from, to, ok := r.pickMove(tree)
	if !ok {
		return modelEdit{}, false
	}
	require.NoError(r.t, os.Rename(
		filepath.Join(r.syncRoot, filepath.FromSlash(from)),
		filepath.Join(r.syncRoot, filepath.FromSlash(to)),
	))
	r.logf("local move %s -> %s", from, to)

	return modelEdit{side: modelLocalSide, kind: modelOpMove, path: from, to: to}, true
}

func (r *modelRun) remoteEdit() (modelEdit, bool) {
	tree := r.remote.tree()
	switch r.pick([]string{modelOpCreateFile, modelOpEditFile, modelOpDelete, modelOpMove, modelOpMakeFolder}) {
	case modelOpCreateFile:
		p := r.newPathIn(modelFoldersOf(tree), modelFileNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		content := r.nextContent(modelRemoteSide)
		r.remote.putFileAt(r.t, p, content)
		r.logf("remote create %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpCreateFile, path: p, content: content}, true
	case modelOpEditFile:
		p := r.pick(modelEntriesOf(tree, false))
		if p == "" {
			return modelEdit{}, false
		}
		content := r.nextContent(modelRemoteSide)
		r.remote.putFileAt(r.t, p, content)
		r.logf("remote edit %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpEditFile, path: p, content: content}, true
	case modelOpDelete:
		p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
		if p == "" {
			return modelEdit{}, false
		}
		r.remote.deleteAt(r.t, p)
		r.logf("remote delete %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpDelete, path: p}, true
	case modelOpMove:
		from, to, ok := r.pickMove(tree)
		if !ok {
			return modelEdit{}, false
		}
		r.remote.moveAt(r.t, from, to)
		r.logf("remote move %s -> %s", from, to)
		return modelEdit{side: modelRemoteSide, kind: modelOpMove, path: from, to: to}, true
	default:
		p := r.newPathIn(modelFoldersOf(tree), modelFolderNamePool, tree)
		if p == "" {
			return modelEdit{}, false
		}
		r.remote.mkdirAt(r.t, p)
		r.logf("remote mkdir %s", p)
		return modelEdit{side: modelRemoteSide, kind: modelOpMakeFolder, path: p}, true
	}
}

// pickMove chooses an existing entry and a free destination outside its own
// subtree, renaming within a folder or moving to another one.
func (r *modelRun) pickMove(tree map[string]string) (string, string, bool) {
	from := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if from == "" {
		return "", "", false
	}
	pool := modelFileNamePool
	if tree[from] == modelFolderSentinel {
		pool = modelFolderNamePool
	}
	var targets []string
	for _, folder := range modelFoldersOf(tree) {
		if folder == from || strings.HasPrefix(folder, from+"/") {
			continue
		}
		targets = append(targets, folder)
	}
	to := r.newPathIn(targets, pool+" renamed-"+path.Base(from), tree)
	if to == "" || to == from {
		return "", "", false
	}

	return from, to, true
}

// modelPaths lists a tree's paths, folders with a trailing slash.
func modelPaths(tree map[string]string) []string {
	paths := make([]string, 0, len(tree))
	for p, content := range tree {
		if content == modelFolderSentinel {
			p += "/"
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	return paths
}
