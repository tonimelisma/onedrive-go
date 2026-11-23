package syncplan

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// exStartNamed is the start of the exhaustive check with that name.
func exStartNamed(t *testing.T, name string) exStart {
	t.Helper()

	for _, start := range exStarts() {
		if start.name == name {
			return start
		}
	}
	require.FailNow(t, "no start named "+name)

	return exStart{}
}

// requirePairKeepsEveryInvariant runs one pair of the exhaustive check, and
// the pair again with each of its refusals, and fails on any invariant a run
// breaks, logging what the run did.
func requirePairKeepsEveryInvariant(t *testing.T, pair *exPair) {
	t.Helper()

	registerExCachingDriver(t)
	store := exQuietStore(t)
	r := newExRun(t, store, pair)
	r.run()
	results := []exResult{r.result(pair.id())}
	for _, refusal := range r.exRefusals() {
		refused := newExRun(t, store, pair)
		refused.refusal = refusal
		refused.run()
		results = append(results, refused.result(pair.id()+" "+refusal.String()))
	}
	for i := range results {
		assert.Empty(t, results[i].broken, "%s: %s", results[i].id, results[i].detail)
	}
}

// Validates: R-2.17.11
//
// From the held start (x renamed x2 here, a new x made and f moved into it,
// while OneDrive keeps the old x under that name), f renamed here to x/g in
// the new folder while OneDrive made a file x/g in the old one: the two x
// folders are different items, so the comparison sets f's new place beside
// OneDrive's new file and nothing uploads f this pass. The delete of f's old
// copy on OneDrive waits until a pass uploads it, on every run, with every
// create of every pass refused once too.
func TestPlannerPairs_AFileRenamedIntoAFolderInAHeldFoldersPlaceKeepsItsOneDriveCopy(t *testing.T) {
	t.Parallel()

	requirePairKeepsEveryInvariant(t, &exPair{
		start: exStartNamed(t, "held"),
		local: []exEdit{{kind: exMoveFile, from: "x/f", to: "x/g"}},
		remote: []exEdit{
			{kind: exCreateFile, to: "x/g"},
		},
	})
}

// Validates: R-2.17.11
//
// OneDrive renamed f to p/g and moved z onto p/f while a file was made here
// as p/g: f's download to p/g follows a conflict copy of the new file, and
// z's content arrives at p/f over f's synced copy. That overwrite waits for
// f's download, so when the download is refused f stays here.
func TestPlannerPairs_AFileOverwrittenHereWaitsForTheDownloadOfItsMovedContent(t *testing.T) {
	t.Parallel()

	requirePairKeepsEveryInvariant(t, &exPair{
		start: exStartNamed(t, "synced"),
		local: []exEdit{{kind: exCreateFile, to: "p/g"}},
		remote: []exEdit{
			{kind: exMoveFile, from: "p/f", to: "p/g"},
			{kind: exMoveFile, from: "z", to: "p/f"},
		},
	})
}

// Validates: R-2.17.11, R-2.3.1
//
// z renamed g here and moved by OneDrive onto p/f, a path whose own synced
// item is gone here (p/f deleted here): z's item ID at p/f proves where it
// sits on OneDrive, so the two renames compose into one move and z is not
// downloaded at p/f beside its renamed copy g, which was then uploaded as a
// new file. Where OneDrive swapped the two (p/f's item now at z), that
// download also waited as a removal of p/f's content, which OneDrive still
// held at z, and sync never converged (chaos seed 55). Each converges with
// every content at one path, on every run, with every create of every pass
// refused once too.
func TestPlannerPairs_AFileRenamedHereAndMovedOnOneDriveOntoAPathDeletedHereMoves(t *testing.T) {
	t.Parallel()

	here := []exEdit{
		{kind: exMoveFile, from: "z", to: "g"},
		{kind: exDeleteFile, from: "p/f"},
	}
	for _, tc := range []struct {
		name     string
		onedrive []exEdit
	}{
		{
			name: "p/f deleted on OneDrive and z moved onto it",
			onedrive: []exEdit{
				{kind: exDeleteFile, from: "p/f"},
				{kind: exMoveFile, from: "z", to: "p/f"},
			},
		},
		{
			name: "z and p/f swapped on OneDrive",
			onedrive: []exEdit{
				{kind: exMoveFile, from: "z", to: "G"},
				{kind: exMoveFile, from: "p/f", to: "z"},
				{kind: exMoveFile, from: "G", to: "p/f"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requirePairKeepsEveryInvariant(t, &exPair{start: exStartNamed(t, "synced"), local: here, remote: tc.onedrive})
		})
	}
}

// Validates: R-2.17.11
//
// A download of another item over a synced file here removes that file's
// content from this side, as a delete does, so it waits for the download
// that writes the content where OneDrive moved the file; a download of the
// file's own item, or of a new path, removes nothing and waits for nothing.
func TestBuildDependencies_ADownloadOverASyncedFileWaitsForTheDownloadOfItsItem(t *testing.T) {
	t.Parallel()

	overwrite := syncdomain.Action{
		Type: syncdomain.ActionDownload, Path: "p/f", ItemID: "item-z",
		View: &syncdomain.PathView{Path: "p/f", Baseline: syncedFileView(3, "item-f").Baseline},
	}
	update := syncdomain.Action{
		Type: syncdomain.ActionDownload, Path: "z", ItemID: "item-z",
		View: &syncdomain.PathView{Path: "z", Baseline: syncedFileView(4, "item-z").Baseline},
	}
	moved := syncdomain.Action{Type: syncdomain.ActionDownload, Path: "p/g", ItemID: "item-f", View: &syncdomain.PathView{Path: "p/g"}}

	deps := buildDependencies([]syncdomain.Action{overwrite, update, moved})

	assert.Equal(t, [][]int{{2}, nil, nil}, deps)
}

// Validates: R-2.17.11
//
// A delete of a synced file the other side holds at another path waits while
// no create of the plan writes its content on the delete's side, and logs the
// path where the content is; a carrying create releases it, ordered first by
// its content edge; a file the other side holds nowhere else, or holds
// already synced at the path where the content is, holds nothing.
func TestPlanBuiltActions_ARemovalOfContentTheOtherSideMovedWaitsForItsCreate(t *testing.T) {
	t.Parallel()

	remoteDelete := syncdomain.Action{Type: syncdomain.ActionRemoteDelete, Path: "p/f", View: syncedFileView(7, "item-f")}
	localDelete := syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: "z", ItemID: "item-z", View: syncedFileView(8, "item-z")}
	gone := syncdomain.Action{Type: syncdomain.ActionRemoteDelete, Path: "q", View: syncedFileView(9, "item-q")}
	inputs := &moveInputs{filesByContent: filesByContent(
		[]syncdomain.RemoteStateRow{
			{Path: "p", ItemID: "item-z", ItemType: syncdomain.ItemTypeFile, Hash: "z"},
			{Path: "y", ItemID: "item-y", ItemType: syncdomain.ItemTypeFile, Hash: "y"},
		},
		[]syncdomain.LocalStateRow{
			{Path: "x/g", ItemType: syncdomain.ItemTypeFile, Hash: contentOfInode(7), LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
			{Path: "y", ItemType: syncdomain.ItemTypeFile, Hash: "y", LocalDevice: 1, LocalInode: 9, LocalHasIdentity: true},
		},
	)}

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	plan, err := NewPlanner(logger).planBuiltActions(
		[]syncdomain.Action{remoteDelete, localDelete, gone}, nil, nil, &moveComposition{}, inputs,
		PlannerMountContext{}, syncdomain.SyncBidirectional)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)
	assert.Equal(t, "q", plan.Actions[0].Path)
	assert.Equal(t, 2, plan.FollowUp.Dropped)
	var held []string
	for line := range strings.Lines(logs.String()) {
		if _, fields, found := strings.Cut(line, `msg="delete waits for the create that carries its content" `); found {
			held = append(held, strings.TrimSpace(fields))
		}
	}
	assert.Equal(t, []string{
		"action=remote_delete path=p/f content_path=x/g",
		"action=local_delete path=z content_path=p",
	}, held)

	carried, err := NewPlanner(slog.New(slog.DiscardHandler)).planBuiltActions(
		[]syncdomain.Action{remoteDelete, uploadOfIdentity("x/g", 7)}, nil, nil, &moveComposition{}, inputs,
		PlannerMountContext{}, syncdomain.SyncBidirectional)
	require.NoError(t, err)
	require.Len(t, carried.Actions, 2)
	assert.Equal(t, [][]int{{1}, nil}, carried.Deps)
}
