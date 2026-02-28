package main

import (
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestFindConflict(t *testing.T) {
	t.Parallel()

	conflicts := []sync.ConflictRecord{
		{ID: "aabb1122-dead-beef-cafe-000000000001", Path: "/foo/bar.txt"},
		{ID: "aabb1122-dead-beef-cafe-000000000002", Path: "/baz/qux.txt"},
		{ID: "ccdd3344-dead-beef-cafe-000000000003", Path: "/other/file.txt"},
	}

	tests := []struct {
		name     string
		idOrPath string
		wantID   string
		wantNil  bool
		wantErr  bool
	}{
		{name: "exact ID match", idOrPath: "aabb1122-dead-beef-cafe-000000000001", wantID: "aabb1122-dead-beef-cafe-000000000001"},
		{name: "exact path match", idOrPath: "/foo/bar.txt", wantID: "aabb1122-dead-beef-cafe-000000000001"},
		{name: "unique prefix", idOrPath: "ccdd", wantID: "ccdd3344-dead-beef-cafe-000000000003"},
		{name: "ambiguous prefix", idOrPath: "aabb", wantErr: true},
		{name: "no match", idOrPath: "zzzz", wantNil: true},
		{name: "full ID exact takes priority over prefix", idOrPath: "aabb1122-dead-beef-cafe-000000000002", wantID: "aabb1122-dead-beef-cafe-000000000002"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := findConflict(conflicts, tt.idOrPath)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}

				return
			}

			if got == nil {
				t.Fatal("expected non-nil result, got nil")
			}

			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}
