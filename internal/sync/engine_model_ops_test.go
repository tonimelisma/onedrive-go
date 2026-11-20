package sync

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

// Model-test operations beyond create, edit, delete, move, mkdir and type
// change (D-096): the shapes that found most of the hardening program's
// bugs. Each records the modelEdits the oracle needs; an operation that
// cannot apply to the current tree records nothing.

const (
	modelOpHardLink          = "hard-link"
	modelOpMoveThenRecreate  = "mv-then-recreate"
	modelOpRecreateFolder    = "recreate-folder"
	modelOpCaseRename        = "case-rename"
	modelOpSwap              = "swap"
	modelOpDeleteAndRecreate = "delete-and-recreate"
	modelCrossDivergent      = "divergent-renames"
	modelCrossLocalMoveEdit  = "local-move-remote-edit"
	modelCrossRemoteMoveEdit = "remote-move-local-edit"
	modelCrossCaseVariant    = "case-variant-create"
	modelCrossOpOdds         = 3
	modelWipeOdds            = 15
	modelResolvedPrefix      = "resolved-"
	modelSwapPrefix          = "swap-"
)

// applyBatchEdits makes one batch's edits. Most batches are a few random
// operations per side, sometimes a mirrored move and a cross-side
// operation; now and then a batch wipes one side instead, and the next sync
// must hold those deletes (D-002) until the model confirms them.
func (r *modelRun) applyBatchEdits(label string) []syncfixture.ModelEdit {
	if r.rng.IntN(modelWipeOdds) == 0 {
		if edits := r.wipeSide(); len(edits) > 0 {
			return edits
		}
	}

	var edits []syncfixture.ModelEdit
	for range r.rng.IntN(modelMaxOpsPerSide + 1) {
		edits = append(edits, r.localEdit()...)
	}
	for range r.rng.IntN(modelMaxOpsPerSide + 1) {
		edits = append(edits, r.remoteEdit()...)
	}
	if r.rng.IntN(modelMirroredMoveOdds) == 0 {
		edits = append(edits, r.mirroredMove()...)
	}
	if r.rng.IntN(modelCrossOpOdds) == 0 {
		edits = append(edits, r.crossEdit(label)...)
	}

	return edits
}

func (r *modelRun) localEdit() []syncfixture.ModelEdit {
	tree := r.localTree()
	kinds := []string{
		syncfixture.ModelOpCreateFile, syncfixture.ModelOpEditFile, syncfixture.ModelOpDelete, syncfixture.ModelOpMove, syncfixture.ModelOpMakeFolder, syncfixture.ModelOpReplace,
		modelOpHardLink, modelOpMoveThenRecreate, modelOpRecreateFolder,
	}
	if r.caseSensitive {
		kinds = append(kinds, modelOpCaseRename)
	}

	switch kind := r.pick(kinds); kind {
	case syncfixture.ModelOpEditFile:
		return r.localEditFile(tree, r.pick(modelEntriesOf(tree, false)))
	case modelOpHardLink:
		return r.localHardLink(tree)
	case modelOpMoveThenRecreate:
		return r.localMoveThenRecreate(tree)
	case modelOpRecreateFolder:
		return r.localRecreateFolder(tree)
	case modelOpCaseRename:
		return r.caseRename(syncfixture.ModelLocalSide, tree)
	default:
		return modelEditList(r.localBasicEdit(kind, tree))
	}
}

func (r *modelRun) remoteEdit() []syncfixture.ModelEdit {
	tree := r.remote.tree()
	switch kind := r.pick([]string{
		syncfixture.ModelOpCreateFile, syncfixture.ModelOpEditFile, syncfixture.ModelOpDelete, syncfixture.ModelOpMove, syncfixture.ModelOpMakeFolder, syncfixture.ModelOpReplace,
		modelOpSwap, modelOpDeleteAndRecreate, modelOpCaseRename,
	}); kind {
	case modelOpSwap:
		return r.remoteSwap(tree)
	case modelOpDeleteAndRecreate:
		return r.remoteDeleteAndRecreate(tree)
	case modelOpCaseRename:
		return r.caseRename(syncfixture.ModelRemoteSide, tree)
	default:
		return modelEditList(r.remoteBasicEdit(kind, tree))
	}
}

func modelEditList(edit syncfixture.ModelEdit, ok bool) []syncfixture.ModelEdit {
	if !ok {
		return nil
	}

	return []syncfixture.ModelEdit{edit}
}

// localEditFile rewrites p in place. A hard link shares the write, so every
// other path whose content changed with it is recorded as edited too.
func (r *modelRun) localEditFile(tree map[string]string, p string) []syncfixture.ModelEdit {
	if p == "" {
		return nil
	}
	r.logf("local edit %s", p)

	return r.writeLocalInPlace(tree, p)
}

// writeLocalInPlace rewrites p in place with new content. A hard link shares
// the write, so every other path whose content changed with it is recorded as
// edited too.
func (r *modelRun) writeLocalInPlace(tree map[string]string, p string) []syncfixture.ModelEdit {
	content := r.nextContent(syncfixture.ModelLocalSide)
	syncfixture.WriteLocalFile(r.t, r.syncRoot, filepath.FromSlash(p), content)
	edits := []syncfixture.ModelEdit{{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpEditFile, Path: p, Content: content}}
	for other, now := range r.localTree() {
		if other != p && now == content && tree[other] != content {
			edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpEditFile, Path: other, Content: content})
		}
	}

	return edits
}

func (r *modelRun) remoteEditFile(tree map[string]string) (syncfixture.ModelEdit, bool) {
	p := r.pick(modelEntriesOf(tree, false))
	if p == "" {
		return syncfixture.ModelEdit{}, false
	}
	content := r.nextContent(syncfixture.ModelRemoteSide)
	r.remote.putFileAt(r.t, p, content)
	r.logf("remote edit %s", p)

	return syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpEditFile, Path: p, Content: content}, true
}

// localHardLink gives a file a second name on the same inode.
func (r *modelRun) localHardLink(tree map[string]string) []syncfixture.ModelEdit {
	src := r.pick(modelEntriesOf(tree, false))
	dst := r.newPathIn(r.localFolders(), modelFileNamePool, tree)
	if src == "" || dst == "" {
		return nil
	}
	require.NoError(r.t, os.Link(r.localAbs(src), r.localAbs(dst)))
	r.logf("local hard link %s -> %s", dst, src)

	return []syncfixture.ModelEdit{{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpCreateFile, Path: dst, Content: tree[src]}}
}

// localMoveThenRecreate moves an entry away and creates a new one of the
// same type at the path it left.
func (r *modelRun) localMoveThenRecreate(tree map[string]string) []syncfixture.ModelEdit {
	from, to, ok := r.pickMove(tree)
	if !ok {
		return nil
	}
	require.NoError(r.t, os.Rename(r.localAbs(from), r.localAbs(to)))
	edits := []syncfixture.ModelEdit{{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: from, To: to}}
	if tree[from] == syncfixture.ModelFolderSentinel {
		require.NoError(r.t, os.MkdirAll(r.localAbs(from), 0o750))
		edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMakeFolder, Path: from})
	} else {
		content := r.nextContent(syncfixture.ModelLocalSide)
		syncfixture.WriteLocalFile(r.t, r.syncRoot, filepath.FromSlash(from), content)
		edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpCreateFile, Path: from, Content: content})
	}
	r.logf("local move %s -> %s then recreate %s", from, to, from)

	return edits
}

// localRecreateFolder deletes a folder and builds the same tree again: same
// names and content, new inodes. The copy is built beside the sync root
// while the original still exists and then swapped in: deleting first let
// the filesystem hand the freed inode numbers straight back, so on some
// runners the "rebuilt" folder kept its (device, inode) and one seed planned
// a move where another run of it planned a delete and a create. Identity now
// includes the birth time, which tells such a rebuild apart wherever the
// filesystem records one (R-2.17.1); building aside keeps the replay
// deterministic on filesystems that record none.
func (r *modelRun) localRecreateFolder(tree map[string]string) []syncfixture.ModelEdit {
	folder := r.pick(modelEntriesOf(tree, true))
	if folder == "" {
		return nil
	}
	rebuilt, err := os.MkdirTemp(filepath.Dir(r.syncRoot), "model-rebuild-")
	require.NoError(r.t, err)
	for _, p := range modelPaths(tree) {
		rel, inside := strings.CutPrefix(strings.TrimSuffix(p, "/"), folder+"/")
		if !inside {
			continue
		}
		if strings.HasSuffix(p, "/") {
			require.NoError(r.t, os.MkdirAll(filepath.Join(rebuilt, filepath.FromSlash(rel)), 0o750))
			continue
		}
		syncfixture.WriteLocalFile(r.t, rebuilt, filepath.FromSlash(rel), tree[folder+"/"+rel])
	}
	require.NoError(r.t, os.RemoveAll(r.localAbs(folder)))
	require.NoError(r.t, os.Rename(rebuilt, r.localAbs(folder)))
	r.logf("local recreate folder %s", folder)

	return []syncfixture.ModelEdit{{Side: syncfixture.ModelLocalSide, Kind: modelOpRecreateFolder, Path: folder}}
}

// caseRename renames an entry to the same name in another case, a rename
// OneDrive treats as one item keeping its name.
func (r *modelRun) caseRename(side string, tree map[string]string) []syncfixture.ModelEdit {
	from := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if from == "" {
		return nil
	}
	renamed := modelSwapCase(path.Base(from))
	if renamed == path.Base(from) {
		return nil
	}
	to := path.Join(path.Dir(from), renamed)
	if side == syncfixture.ModelLocalSide {
		require.NoError(r.t, os.Rename(r.localAbs(from), r.localAbs(to)))
	} else {
		r.remote.moveAt(r.t, from, to)
	}
	r.logf("%s case rename %s -> %s", side, from, to)

	return []syncfixture.ModelEdit{{Side: side, Kind: syncfixture.ModelOpMove, Path: from, To: to}}
}

// remoteSwap exchanges two files' paths through a temporary name, as a
// user would.
func (r *modelRun) remoteSwap(tree map[string]string) []syncfixture.ModelEdit {
	files := modelEntriesOf(tree, false)
	if len(files) < 2 {
		return nil
	}
	first := r.pick(files)
	second := r.pick(files)
	tmp := path.Join(path.Dir(first), modelSwapPrefix+path.Base(first))
	if first == second || modelTreeHasFold(tree, tmp) {
		return nil
	}
	r.remote.moveAt(r.t, first, tmp)
	r.remote.moveAt(r.t, second, first)
	r.remote.moveAt(r.t, tmp, second)
	r.logf("remote swap %s <-> %s", first, second)

	return []syncfixture.ModelEdit{
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMove, Path: first, To: second},
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMove, Path: second, To: first},
	}
}

// remoteDeleteAndRecreate replaces an entry with a new item of the same
// type at the same path: same path, new item ID.
func (r *modelRun) remoteDeleteAndRecreate(tree map[string]string) []syncfixture.ModelEdit {
	p := r.pick(append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...))
	if p == "" {
		return nil
	}
	r.remote.deleteAt(r.t, p)
	edits := []syncfixture.ModelEdit{{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpDelete, Path: p}}
	if tree[p] == syncfixture.ModelFolderSentinel {
		r.remote.mkdirAt(r.t, p)
		edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMakeFolder, Path: p})
	} else {
		content := r.nextContent(syncfixture.ModelRemoteSide)
		r.remote.putFileAt(r.t, p, content)
		edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpCreateFile, Path: p, Content: content})
	}
	r.logf("remote delete and recreate %s", p)

	return edits
}

// crossEdit applies one operation that touches the same item on both sides.
func (r *modelRun) crossEdit(label string) []syncfixture.ModelEdit {
	kinds := []string{modelCrossDivergent, modelCrossLocalMoveEdit, modelCrossRemoteMoveEdit}
	if r.caseSensitive {
		kinds = append(kinds, modelCrossCaseVariant)
	}
	shared := r.sharedEntries()

	switch r.pick(kinds) {
	case modelCrossDivergent:
		return r.divergentRenames(shared)
	case modelCrossLocalMoveEdit:
		return r.moveOneSideEditOther(shared, syncfixture.ModelLocalSide)
	case modelCrossRemoteMoveEdit:
		return r.moveOneSideEditOther(shared, syncfixture.ModelRemoteSide)
	default:
		return r.caseVariantCreate(label)
	}
}

// sharedEntries is every entry both sides hold at the same path with the
// same content.
func (r *modelRun) sharedEntries() map[string]string {
	local := r.localTree()
	remote := r.remote.tree()
	shared := map[string]string{}
	for p, content := range local {
		if remote[p] == content {
			shared[p] = content
		}
	}

	return shared
}

// divergentRenames renames one item to different names on the two sides.
func (r *modelRun) divergentRenames(shared map[string]string) []syncfixture.ModelEdit {
	from, localTo, ok := r.pickMove(shared)
	if !ok {
		return nil
	}
	remoteTo := path.Join(path.Dir(localTo), "remote-"+path.Base(localTo))
	if modelTreeHasFold(r.localTree(), localTo) || modelTreeHasFold(r.remote.tree(), localTo) ||
		modelTreeHasFold(r.remote.tree(), remoteTo) || modelTreeHasFold(r.localTree(), remoteTo) {
		return nil
	}
	require.NoError(r.t, os.Rename(r.localAbs(from), r.localAbs(localTo)))
	r.remote.moveAt(r.t, from, remoteTo)
	r.logf("divergent rename %s -> local %s, remote %s", from, localTo, remoteTo)

	return []syncfixture.ModelEdit{
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: from, To: localTo},
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMove, Path: from, To: remoteTo},
	}
}

// moveOneSideEditOther moves a shared file on moveSide while the other side
// edits it in place.
func (r *modelRun) moveOneSideEditOther(shared map[string]string, moveSide string) []syncfixture.ModelEdit {
	files := map[string]string{}
	for p, content := range shared {
		if content != syncfixture.ModelFolderSentinel {
			files[p] = content
		}
	}
	from, to, ok := r.pickMove(files)
	if !ok || modelTreeHasFold(r.localTree(), to) || modelTreeHasFold(r.remote.tree(), to) {
		return nil
	}
	if moveSide == syncfixture.ModelLocalSide {
		require.NoError(r.t, os.Rename(r.localAbs(from), r.localAbs(to)))
		content := r.nextContent(syncfixture.ModelRemoteSide)
		r.remote.putFileAt(r.t, from, content)
		r.logf("local move %s -> %s while remote edits it", from, to)
		return []syncfixture.ModelEdit{
			{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: from, To: to},
			{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpEditFile, Path: from, Content: content},
		}
	}
	r.remote.moveAt(r.t, from, to)
	r.logf("remote move %s -> %s while local edits it", from, to)

	return append([]syncfixture.ModelEdit{{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpMove, Path: from, To: to}},
		r.writeLocalInPlace(r.localTree(), from)...)
}

// caseVariantCreate creates a file locally and its case variant on
// OneDrive, which treats the two names as one. The client must keep both:
// the local one is a durable collision for the user to rename, never
// overwritten. The model is that user: after the first pass it
// checks the local file is intact and renames it.
func (r *modelRun) caseVariantCreate(label string) []syncfixture.ModelEdit {
	local := r.localTree()
	remote := r.remote.tree()
	name := path.Join(r.pick(modelFoldersOf(r.sharedFolders())), r.pick(strings.Fields(modelFileNamePool)))
	variant := path.Join(path.Dir(name), modelSwapCase(path.Base(name)))
	if variant == name || modelTreeHasFold(local, name) || modelTreeHasFold(remote, name) {
		return nil
	}
	localContent := r.nextContent(syncfixture.ModelLocalSide)
	syncfixture.WriteLocalFile(r.t, r.syncRoot, filepath.FromSlash(name), localContent)
	remoteContent := r.nextContent(syncfixture.ModelRemoteSide)
	r.remote.putFileAt(r.t, variant, remoteContent)
	r.logf("case variants: local create %s, remote create %s", name, variant)

	r.afterFirstPass = func() []syncfixture.ModelEdit {
		data, err := os.ReadFile(r.localAbs(name))
		require.NoError(r.t, err, "%s: the local case variant %s must survive the collision; %s", label, name, r.failureContext())
		assert.Equal(r.t, localContent, string(data), "%s: the local case variant %s was overwritten; %s", label, name, r.failureContext())
		resolved := r.freeResolvedName(name)
		require.NoError(r.t, os.Rename(r.localAbs(name), r.localAbs(resolved)))
		r.logf("user renames the local case variant %s -> %s", name, resolved)
		return []syncfixture.ModelEdit{{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: name, To: resolved}}
	}

	return []syncfixture.ModelEdit{
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpCreateFile, Path: name, Content: localContent},
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpCreateFile, Path: variant, Content: remoteContent},
	}
}

// freeResolvedName is a name for the user's rename of a colliding local
// file that neither side holds.
func (r *modelRun) freeResolvedName(name string) string {
	local, remote := r.localTree(), r.remote.tree()
	for i := 1; ; i++ {
		candidate := path.Join(path.Dir(name), modelResolvedPrefix+strconv.Itoa(i)+"-"+path.Base(name))
		if !modelTreeHasFold(local, candidate) && !modelTreeHasFold(remote, candidate) {
			return candidate
		}
	}
}

// resolveNameCollisions plays the user the engine asks to "rename one of
// them": a local entry whose name OneDrive treats as the same as a different
// entry beside it (online, or locally) is a durable collision the engine
// keeps out of sync by design, never overwrites. The model renames each such
// local entry once sync is otherwise quiet, so the batch can converge.
func (r *modelRun) resolveNameCollisions() []syncfixture.ModelEdit {
	local, remote := r.localTree(), r.remote.tree()
	var edits []syncfixture.ModelEdit
	for _, p := range append(modelEntriesOf(local, false), modelEntriesOf(local, true)...) {
		if _, exists := local[p]; !exists {
			// Inside a folder renamed earlier in this loop.
			continue
		}
		if _, synced := remote[p]; synced || !r.collidesUnderFolding(p, local, remote) {
			continue
		}
		resolved := r.freeResolvedName(p)
		require.NoError(r.t, os.Rename(r.localAbs(p), r.localAbs(resolved)))
		r.logf("user renames colliding local %s -> %s", p, resolved)
		edits = append(edits, syncfixture.ModelEdit{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: p, To: resolved})
		local = r.localTree()
	}

	return edits
}

// collidesUnderFolding reports whether another entry of the same folder,
// locally or online, folds to p's name.
func (r *modelRun) collidesUnderFolding(p string, local, remote map[string]string) bool {
	for _, tree := range []map[string]string{local, remote} {
		for other := range tree {
			if other != p && strings.EqualFold(other, p) {
				return true
			}
		}
	}

	return false
}

// sharedFolders is the folders both sides hold, for picking a parent both
// sides have (modelFoldersOf adds the root).
func (r *modelRun) sharedFolders() map[string]string {
	shared := r.sharedEntries()
	for p, content := range shared {
		if content != syncfixture.ModelFolderSentinel {
			delete(shared, p)
		}
	}

	return shared
}

// wipeSide deletes everything on one side. The next pass must hold the
// deletes it would propagate (D-002: the other side vanished).
func (r *modelRun) wipeSide() []syncfixture.ModelEdit {
	side := r.pick([]string{syncfixture.ModelLocalSide, syncfixture.ModelRemoteSide})
	tree := r.localTree()
	if side == syncfixture.ModelRemoteSide {
		tree = r.remote.tree()
	}
	var edits []syncfixture.ModelEdit
	for _, p := range append(modelEntriesOf(tree, false), modelEntriesOf(tree, true)...) {
		if strings.Contains(p, "/") {
			continue
		}
		if side == syncfixture.ModelLocalSide {
			require.NoError(r.t, os.RemoveAll(r.localAbs(p)))
		} else {
			r.remote.deleteAt(r.t, p)
		}
		edits = append(edits, syncfixture.ModelEdit{Side: side, Kind: syncfixture.ModelOpDelete, Path: p})
	}
	if len(edits) > 0 {
		r.expectHoldOf = side
		r.logf("%s wipe: %d top-level entries deleted", side, len(edits))
	}

	return edits
}

func (r *modelRun) localAbs(p string) string {
	return filepath.Join(r.syncRoot, filepath.FromSlash(p))
}

// modelSwapCase flips the case of a name's first letter.
func modelSwapCase(name string) string {
	runes := []rune(name)
	for i, c := range runes {
		switch {
		case unicode.IsLower(c):
			runes[i] = unicode.ToUpper(c)
		case unicode.IsUpper(c):
			runes[i] = unicode.ToLower(c)
		default:
			continue
		}
		return string(runes)
	}

	return name
}

// modelTreeHasFold reports whether tree holds p under OneDrive's
// case-insensitive comparison.
func modelTreeHasFold(tree map[string]string, p string) bool {
	for existing := range tree {
		if strings.EqualFold(existing, p) {
			return true
		}
	}

	return false
}

// localFSIsCaseSensitive reports whether dir's filesystem tells names apart
// by case. Case-only renames and cross-side case variants are only
// meaningful on one that does.
func localFSIsCaseSensitive(dir string) bool {
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))

	return os.IsNotExist(err)
}
