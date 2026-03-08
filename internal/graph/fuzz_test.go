package graph

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

// FuzzDriveItemUnmarshal feeds arbitrary JSON to json.Unmarshal into a
// driveItemResponse, then calls toItem(). Verifies no panics from nil
// pointer dereferences in the normalization logic.
func FuzzDriveItemUnmarshal(f *testing.F) {
	// Seed corpus: representative API responses including real quirks from ci_issues.md.
	f.Add([]byte(`{"id":"abc","name":"test.txt","size":100}`))
	f.Add([]byte(`{"id":"x","name":"folder","folder":{"childCount":5},"root":{}}`))
	f.Add([]byte(`{"id":"y","name":"deleted","deleted":{}}`))
	f.Add([]byte(`{"id":"z","name":"shortcut","remoteItem":{"id":"ri","parentReference":{"driveId":"d1"}}}`))
	f.Add([]byte(`{"id":"","name":"","size":-1,"file":{"hashes":{"quickXorHash":"abc"}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	// ci_issues §3: null hashes object.
	f.Add([]byte(`{"id":"a","name":"f.txt","file":{"hashes":null}}`))
	// ci_issues §5: zero-byte file with hash.
	f.Add([]byte(`{"id":"b","name":"empty.txt","size":0,"file":{"hashes":{"quickXorHash":"AAAAAAAAAAAAAAAAAAAAAAAAAAA="}}}`))
	// ci_issues §8: package facet (OneNote).
	f.Add([]byte(`{"id":"c","name":"notebook","package":{},"folder":{"childCount":2}}`))
	// Large size field.
	f.Add([]byte(`{"id":"d","name":"big.zip","size":10737418240,"file":{"hashes":{"quickXorHash":"xyz="}}}`))
	// Shared facet.
	f.Add([]byte(`{"id":"e","name":"shared.txt","shared":{"scope":"users"},"file":{}}`))

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	f.Fuzz(func(_ *testing.T, data []byte) {
		var resp driveItemResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return // invalid JSON — not interesting
		}

		// Must not panic.
		_ = resp.toItem(logger)
	})
}

// FuzzDeltaResponseUnmarshal feeds arbitrary JSON to json.Unmarshal into a
// deltaResponse, then calls toItem() on each item. Verifies no panics.
func FuzzDeltaResponseUnmarshal(f *testing.F) {
	f.Add([]byte(`{"value":[{"id":"a","name":"file.txt"}],"@odata.deltaLink":"token"}`))
	f.Add([]byte(`{"value":[]}`))
	f.Add([]byte(`{"value":[{"id":"","name":"","file":{"hashes":null}}]}`))
	f.Add([]byte(`{}`))

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	f.Fuzz(func(_ *testing.T, data []byte) {
		var resp deltaResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}

		for i := range resp.Value {
			// Must not panic.
			_ = resp.Value[i].toItem(logger)
		}
	})
}

// FuzzPermissionUnmarshal feeds arbitrary JSON to json.Unmarshal into a
// listPermissionsResponse and then calls HasWriteAccess. Verifies no panics.
func FuzzPermissionUnmarshal(f *testing.F) {
	f.Add([]byte(`{"value":[{"id":"p1","roles":["read"]}]}`))
	f.Add([]byte(`{"value":[{"id":"p1","roles":["write","read"]}]}`))
	f.Add([]byte(`{"value":[]}`))
	f.Add([]byte(`{}`))

	f.Fuzz(func(_ *testing.T, data []byte) {
		var resp listPermissionsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}

		// Must not panic.
		_ = HasWriteAccess(resp.Value)
	})
}
