package syncfixture

import (
	"context"
	"net/http"
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ScriptedDeltaFetcher answers each delta call from a script and records the
// token every call carried.
type ScriptedDeltaFetcher struct {
	mu     stdsync.Mutex // guards tokens
	Answer func(call int, token string) (*graph.DeltaPage, error)
	tokens []string
}

func (f *ScriptedDeltaFetcher) Delta(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
	f.mu.Lock()
	f.tokens = append(f.tokens, token)
	call := len(f.tokens)
	f.mu.Unlock()

	return f.Answer(call, token)
}

func (f *ScriptedDeltaFetcher) Seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.tokens...)
}

func CursorTestItem(id string) graph.Item {
	return graph.Item{
		ID: id, Name: id + ".txt", ParentID: "root", ParentPathKnown: true,
		DriveID: driveid.New(EngineTestDriveID), QuickXorHash: "h-" + id, Size: 1,
	}
}

func GraphStatusError(status int, sentinel error) error {
	return &graph.GraphError{StatusCode: status, Code: http.StatusText(status), Err: sentinel}
}
