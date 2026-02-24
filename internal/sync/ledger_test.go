package sync

import (
	"context"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// newTestLedger creates a Ledger backed by a temp BaselineManager DB.
func newTestLedger(t *testing.T) *Ledger {
	t.Helper()

	mgr := newTestManager(t)

	return NewLedger(mgr.DB(), testLogger(t))
}

func TestLedger_WriteAndLoadPending(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{Type: ActionDownload, Path: "file1.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
		{Type: ActionUpload, Path: "file2.txt", DriveID: driveid.New("d1"), ItemID: "i2"},
	}
	deps := [][]int{{}, {0}} // action 1 depends on action 0

	ids, err := ledger.WriteActions(ctx, actions, deps, "cycle-1")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("got %d IDs, want 2", len(ids))
	}

	rows, err := ledger.LoadPending(ctx, "cycle-1")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("got %d pending rows, want 2", len(rows))
	}

	if rows[0].ActionType != "download" {
		t.Errorf("row 0 type = %q, want %q", rows[0].ActionType, "download")
	}

	if rows[0].Path != "file1.txt" {
		t.Errorf("row 0 path = %q, want %q", rows[0].Path, "file1.txt")
	}

	if rows[1].ActionType != "upload" {
		t.Errorf("row 1 type = %q, want %q", rows[1].ActionType, "upload")
	}

	if len(rows[1].DependsOn) != 1 || rows[1].DependsOn[0] != 0 {
		t.Errorf("row 1 deps = %v, want [0]", rows[1].DependsOn)
	}
}

func TestLedger_Lifecycle(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{Type: ActionDownload, Path: "lifecycle.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
	}

	ids, err := ledger.WriteActions(ctx, actions, nil, "cycle-lc")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	id := ids[0]

	// Claim.
	claimErr := ledger.Claim(ctx, id)
	if claimErr != nil {
		t.Fatalf("Claim: %v", claimErr)
	}

	// Double claim should fail.
	doubleClaim := ledger.Claim(ctx, id)
	if doubleClaim == nil {
		t.Error("double Claim should fail")
	}

	// Complete.
	completeErr := ledger.Complete(ctx, id)
	if completeErr != nil {
		t.Fatalf("Complete: %v", completeErr)
	}

	// Double complete should fail.
	doubleComplete := ledger.Complete(ctx, id)
	if doubleComplete == nil {
		t.Error("double Complete should fail")
	}

	// No more pending.
	rows, loadErr := ledger.LoadPending(ctx, "cycle-lc")
	if loadErr != nil {
		t.Fatalf("LoadPending: %v", loadErr)
	}

	if len(rows) != 0 {
		t.Errorf("got %d pending rows, want 0", len(rows))
	}
}

func TestLedger_Fail(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{Type: ActionUpload, Path: "fail.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
	}

	ids, err := ledger.WriteActions(ctx, actions, nil, "cycle-fail")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	claimErr := ledger.Claim(ctx, ids[0])
	if claimErr != nil {
		t.Fatalf("Claim: %v", claimErr)
	}

	failErr := ledger.Fail(ctx, ids[0], "network error")
	if failErr != nil {
		t.Fatalf("Fail: %v", failErr)
	}

	// Failed actions should not appear as pending.
	rows, loadErr := ledger.LoadPending(ctx, "cycle-fail")
	if loadErr != nil {
		t.Fatalf("LoadPending: %v", loadErr)
	}

	if len(rows) != 0 {
		t.Errorf("got %d pending rows, want 0", len(rows))
	}
}

func TestLedger_Cancel(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{Type: ActionLocalDelete, Path: "cancel.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
	}

	ids, err := ledger.WriteActions(ctx, actions, nil, "cycle-cancel")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	cancelErr := ledger.Cancel(ctx, ids[0])
	if cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}

	rows, loadErr := ledger.LoadPending(ctx, "cycle-cancel")
	if loadErr != nil {
		t.Fatalf("LoadPending: %v", loadErr)
	}

	if len(rows) != 0 {
		t.Errorf("got %d pending rows, want 0", len(rows))
	}
}

func TestLedger_ReclaimStale(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{Type: ActionDownload, Path: "stale.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
	}

	ids, err := ledger.WriteActions(ctx, actions, nil, "cycle-stale")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	claimErr := ledger.Claim(ctx, ids[0])
	if claimErr != nil {
		t.Fatalf("Claim: %v", claimErr)
	}

	// Backdate the claimed_at to make it stale.
	past := time.Now().Add(-2 * time.Hour).UnixNano()
	_, backdateErr := ledger.db.ExecContext(ctx,
		"UPDATE action_queue SET claimed_at = ? WHERE id = ?", past, ids[0])
	if backdateErr != nil {
		t.Fatalf("backdate: %v", backdateErr)
	}

	reclaimed, reclaimErr := ledger.ReclaimStale(ctx, 1*time.Hour)
	if reclaimErr != nil {
		t.Fatalf("ReclaimStale: %v", reclaimErr)
	}

	if reclaimed != 1 {
		t.Errorf("reclaimed = %d, want 1", reclaimed)
	}

	// Should be pending again.
	rows, loadErr := ledger.LoadPending(ctx, "cycle-stale")
	if loadErr != nil {
		t.Fatalf("LoadPending: %v", loadErr)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d pending rows, want 1", len(rows))
	}

	if rows[0].Status != "pending" {
		t.Errorf("status = %q, want %q", rows[0].Status, "pending")
	}
}

func TestLedger_CountPending(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
		{Type: ActionUpload, Path: "b.txt", DriveID: driveid.New("d1"), ItemID: "i2"},
		{Type: ActionLocalDelete, Path: "c.txt", DriveID: driveid.New("d1"), ItemID: "i3"},
	}

	ids, err := ledger.WriteActions(ctx, actions, nil, "cycle-count")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	count, err := ledger.CountPendingForCycle(ctx, "cycle-count")
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}

	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	// Complete one, count should decrease.
	claimErr := ledger.Claim(ctx, ids[0])
	if claimErr != nil {
		t.Fatalf("Claim: %v", claimErr)
	}

	completeErr := ledger.Complete(ctx, ids[0])
	if completeErr != nil {
		t.Fatalf("Complete: %v", completeErr)
	}

	count, err = ledger.CountPendingForCycle(ctx, "cycle-count")
	if err != nil {
		t.Fatalf("CountPending: %v", err)
	}

	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestLedger_LastCycleID(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	// Empty table.
	id, err := ledger.LastCycleID(ctx)
	if err != nil {
		t.Fatalf("LastCycleID: %v", err)
	}

	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}

	// After write.
	actions := []Action{
		{Type: ActionDownload, Path: "last.txt", DriveID: driveid.New("d1"), ItemID: "i1"},
	}

	if _, writeErr := ledger.WriteActions(ctx, actions, nil, "cycle-last"); writeErr != nil {
		t.Fatalf("WriteActions: %v", writeErr)
	}

	id, err = ledger.LastCycleID(ctx)
	if err != nil {
		t.Fatalf("LastCycleID: %v", err)
	}

	if id != "cycle-last" {
		t.Errorf("got %q, want %q", id, "cycle-last")
	}
}

// TestLedger_MovePathColumns verifies that move actions store
// destination in the 'path' column and source in the 'old_path' column,
// matching the ledger spec (concurrent-execution.md).
// Regression test for: path and old_path were swapped for move actions.
func TestLedger_MovePathColumns(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{
			Type:    ActionLocalMove,
			Path:    "old/location.txt", // source (where it was)
			NewPath: "new/location.txt", // destination (where it moved to)
			DriveID: driveid.New("d1"),
			ItemID:  "move-id",
		},
	}

	ids, err := ledger.WriteActions(ctx, actions, nil, "cycle-move")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	rows, err := ledger.LoadPending(ctx, "cycle-move")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	row := rows[0]
	if row.ID != ids[0] {
		t.Errorf("row ID = %d, want %d", row.ID, ids[0])
	}

	// Per spec: path column = destination, old_path column = source.
	if row.Path != "new/location.txt" {
		t.Errorf("path = %q, want %q (destination)", row.Path, "new/location.txt")
	}

	if row.OldPath != "old/location.txt" {
		t.Errorf("old_path = %q, want %q (source)", row.OldPath, "old/location.txt")
	}
}

// TestLedger_UploadHashFromLocal verifies that upload actions store the
// local hash (not empty) in the ledger's hash column.
// Regression test for: resolveHashFromView only checked Remote, returning
// empty for uploads where Remote is nil.
func TestLedger_UploadHashFromLocal(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{
			Type:    ActionUpload,
			Path:    "local-file.txt",
			DriveID: driveid.New("d1"),
			ItemID:  "upload-id",
			View: &PathView{
				Local: &LocalState{
					Hash: "local-quickxor-hash",
					Size: 42,
				},
				// Remote is nil — this is a new upload.
			},
		},
	}

	_, err := ledger.WriteActions(ctx, actions, nil, "cycle-upload-hash")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	rows, err := ledger.LoadPending(ctx, "cycle-upload-hash")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	if rows[0].Hash != "local-quickxor-hash" {
		t.Errorf("hash = %q, want %q", rows[0].Hash, "local-quickxor-hash")
	}
}

// TestLedger_DownloadHashFromRemote verifies that non-upload actions still
// prefer the remote hash (unchanged behavior).
func TestLedger_DownloadHashFromRemote(t *testing.T) {
	t.Parallel()

	ledger := newTestLedger(t)
	ctx := context.Background()

	actions := []Action{
		{
			Type:    ActionDownload,
			Path:    "remote-file.txt",
			DriveID: driveid.New("d1"),
			ItemID:  "dl-id",
			View: &PathView{
				Remote: &RemoteState{
					Hash: "remote-quickxor-hash",
					Size: 100,
				},
				Local: &LocalState{
					Hash: "old-local-hash",
					Size: 50,
				},
			},
		},
	}

	_, err := ledger.WriteActions(ctx, actions, nil, "cycle-dl-hash")
	if err != nil {
		t.Fatalf("WriteActions: %v", err)
	}

	rows, err := ledger.LoadPending(ctx, "cycle-dl-hash")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	if rows[0].Hash != "remote-quickxor-hash" {
		t.Errorf("hash = %q, want %q", rows[0].Hash, "remote-quickxor-hash")
	}
}

func TestParseActionType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  ActionType
	}{
		{"download", ActionDownload},
		{"upload", ActionUpload},
		{"local_delete", ActionLocalDelete},
		{"remote_delete", ActionRemoteDelete},
		{"local_move", ActionLocalMove},
		{"remote_move", ActionRemoteMove},
		{"folder_create", ActionFolderCreate},
		{"conflict", ActionConflict},
		{"update_synced", ActionUpdateSynced},
		{"cleanup", ActionCleanup},
	}

	for _, tt := range tests {
		got, err := ParseActionType(tt.input)
		if err != nil {
			t.Errorf("ParseActionType(%q): %v", tt.input, err)
		}

		if got != tt.want {
			t.Errorf("ParseActionType(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}

	// Invalid.
	_, err := ParseActionType("invalid")
	if err == nil {
		t.Error("expected error for invalid action type")
	}
}
