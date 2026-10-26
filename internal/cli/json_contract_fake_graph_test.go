package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGraphDrive is a tiny in-memory OneDrive for command-level tests that
// need real request/response round trips (get, put, mv, cp, ...). It knows
// only the endpoints the file and recycle-bin commands call, and fails the
// test on anything else so a new call shows up as a clear test failure
// rather than a hang or a confusing decode error.
type fakeGraphDrive struct {
	t       *testing.T
	driveID string // as it appears in request paths

	mu      sync.Mutex
	nextID  int
	items   map[string]*fakeGraphItem // by ID
	recycle map[string]*fakeGraphItem // by ID
	baseURL string
	// deleteRefused makes every DELETE answer 403, for cleanup-failure tests.
	deleteRefused bool
	// hangRenameTo makes a PATCH renaming an item to this name hang until
	// the client gives up.
	hangRenameTo string
}

type fakeGraphItem struct {
	ID       string
	Name     string
	ParentID string
	Folder   bool
	Content  []byte
	// Modified is the item's fileSystemInfo.lastModifiedDateTime; empty
	// means the fixed fixture time.
	Modified string
}

// fakeGraphMoveBody is the PATCH (move/rename) and POST copy request body.
//
//nolint:tagliatelle // Graph's wire format is camelCase.
type fakeGraphMoveBody struct {
	Name            string `json:"name"`
	ParentReference struct {
		ID string `json:"id"`
	} `json:"parentReference"`
	FileSystemInfo struct {
		LastModifiedDateTime string `json:"lastModifiedDateTime"`
	} `json:"fileSystemInfo"`
}

// fakeGraphRequest is one parsed request against an item by ID.
type fakeGraphRequest struct {
	w      http.ResponseWriter
	r      *http.Request
	id     string
	item   *fakeGraphItem // nil when the ID is not a live item
	action string         // "", "children", "content", "copy", "restore", "permanentDelete"
}

const (
	fakeGraphRootID            = "root"
	fakeGraphCopyMonitorPrefix = "https://monitor.1drv.com/copy/"

	fakeGraphActionChildren        = "children"
	fakeGraphActionContent         = "content"
	fakeGraphActionCopy            = "copy"
	fakeGraphActionRestore         = "restore"
	fakeGraphActionPermanentDelete = "permanentDelete"
)

// fakeGraphTimeout bounds how long a JSON-contract case may run, so a fake
// that a command polls (visibility, copy monitor) fails fast instead of
// hanging the suite.
const fakeGraphTimeout = 30 * time.Second

func newFakeGraphDrive(t *testing.T, driveID string) *fakeGraphDrive {
	t.Helper()

	return &fakeGraphDrive{
		t:       t,
		driveID: driveID,
		items: map[string]*fakeGraphItem{
			fakeGraphRootID: {ID: fakeGraphRootID, Name: "root", Folder: true},
		},
		recycle: map[string]*fakeGraphItem{},
	}
}

// addFile adds a file at the top of the drive.
func (f *fakeGraphDrive) addFile(name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.addLocked(fakeGraphRootID, name, false, []byte(content))
}

func (f *fakeGraphDrive) addRecycled(name string) *fakeGraphItem {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	item := &fakeGraphItem{ID: fmt.Sprintf("recycled-%d", f.nextID), Name: name, ParentID: fakeGraphRootID}
	f.recycle[item.ID] = item

	return item
}

func (f *fakeGraphDrive) addLocked(parentID, name string, folder bool, content []byte) *fakeGraphItem {
	f.nextID++
	item := &fakeGraphItem{
		ID:       fmt.Sprintf("item-%d", f.nextID),
		Name:     name,
		ParentID: parentID,
		Folder:   folder,
		Content:  content,
	}
	f.items[item.ID] = item

	return item
}

func (f *fakeGraphDrive) childNamed(parentID, name string) *fakeGraphItem {
	for _, item := range f.items {
		if item.ParentID == parentID && strings.EqualFold(item.Name, name) && item.ID != fakeGraphRootID {
			return item
		}
	}

	return nil
}

func (f *fakeGraphDrive) byPath(p string) *fakeGraphItem {
	current := f.items[fakeGraphRootID]
	for _, segment := range strings.Split(strings.Trim(p, "/"), "/") {
		if segment == "" {
			continue
		}
		current = f.childNamed(current.ID, segment)
		if current == nil {
			return nil
		}
	}

	return current
}

func (f *fakeGraphDrive) itemJSON(item *fakeGraphItem) map[string]any {
	out := map[string]any{
		"id":                   item.ID,
		"name":                 item.Name,
		"size":                 len(item.Content),
		"eTag":                 "etag-" + item.ID,
		"cTag":                 "ctag-" + item.ID,
		"createdDateTime":      "2026-04-03T00:00:00Z",
		"lastModifiedDateTime": "2026-04-03T00:00:00Z",
	}
	if item.Modified != "" {
		out["lastModifiedDateTime"] = item.Modified
		out["fileSystemInfo"] = map[string]any{"lastModifiedDateTime": item.Modified}
	}
	if item.ID == fakeGraphRootID {
		out["root"] = map[string]any{}
	}
	if item.ParentID != "" {
		out["parentReference"] = map[string]any{"id": item.ParentID, "driveId": f.driveID}
	}
	if item.Folder {
		out["folder"] = map[string]any{"childCount": len(f.childrenOf(item.ID))}
	} else {
		out["file"] = map[string]any{"mimeType": "text/plain"}
		out["@microsoft.graph.downloadUrl"] = f.baseURL + "/download/" + item.ID
	}

	return out
}

func (f *fakeGraphDrive) childrenOf(id string) []map[string]any {
	values := []map[string]any{}
	for _, child := range f.items {
		if child.ParentID == id && child.ID != fakeGraphRootID {
			values = append(values, f.itemJSON(child))
		}
	}

	return values
}

func (f *fakeGraphDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if id, isDownload := strings.CutPrefix(r.URL.Path, "/download/"); isDownload && r.Method == http.MethodGet {
		f.serveContent(w, r, f.items[id])
		return
	}

	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/drives/"+f.driveID)
	switch {
	case !ok:
		f.unexpected(w, r)
	case r.Method == http.MethodGet && rest == "/special/recyclebin/children":
		values := make([]map[string]any, 0, len(f.recycle))
		for _, item := range f.recycle {
			values = append(values, f.itemJSON(item))
		}
		f.writeJSON(w, http.StatusOK, map[string]any{"value": values})
	case strings.HasPrefix(rest, "/root:") || strings.HasPrefix(rest, "/items/root:"):
		f.serveByPath(w, r, rest)
	case strings.HasPrefix(rest, "/items/"):
		f.serveByID(w, r, strings.TrimPrefix(rest, "/items/"))
	case rest == "/root" || rest == "/root/children":
		f.serveByID(w, r, strings.TrimPrefix(rest, "/"))
	default:
		f.unexpected(w, r)
	}
}

func (f *fakeGraphDrive) serveByPath(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.TrimPrefix(strings.TrimPrefix(rest, "/items"), "/root:")
	escapedPath, suffix, _ := strings.Cut(rest, ":")
	itemPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		f.unexpected(w, r)
		return
	}

	switch {
	case r.Method == http.MethodGet && suffix == "":
		item := f.byPath(itemPath)
		if item == nil {
			f.notFound(w)
			return
		}
		f.writeJSON(w, http.StatusOK, f.itemJSON(item))
	case r.Method == http.MethodGet && suffix == "/children":
		item := f.byPath(itemPath)
		if item == nil {
			f.notFound(w)
			return
		}
		f.serveItemGet(fakeGraphRequest{w: w, r: r, id: item.ID, item: item, action: fakeGraphActionChildren})
	case r.Method == http.MethodPut && suffix == "/content":
		f.upload(w, r, fakeGraphRootID, itemPath)
	default:
		f.unexpected(w, r)
	}
}

func (f *fakeGraphDrive) serveByID(w http.ResponseWriter, r *http.Request, rest string) {
	if id, uploadPath, isUpload := strings.Cut(rest, ":/"); isUpload {
		name, _, _ := strings.Cut(uploadPath, ":")
		name, err := url.PathUnescape(name)
		if err != nil || r.Method != http.MethodPut {
			f.unexpected(w, r)
			return
		}
		f.upload(w, r, id, name)
		return
	}

	id, action, _ := strings.Cut(rest, "/")
	req := fakeGraphRequest{w: w, r: r, id: id, item: f.items[id], action: action}

	if action == fakeGraphActionRestore || action == fakeGraphActionPermanentDelete {
		f.serveRecycleAction(req)
		return
	}

	if req.item == nil {
		f.notFound(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.serveItemGet(req)
	case http.MethodPost:
		f.serveItemPost(req)
	case http.MethodPatch:
		f.serveMove(req)
	case http.MethodDelete:
		if f.deleteRefused {
			f.writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "accessDenied", "message": "no"}})
			return
		}
		delete(f.items, id)
		f.recycle[id] = req.item
		w.WriteHeader(http.StatusNoContent)
	default:
		f.unexpected(w, r)
	}
}

func (f *fakeGraphDrive) serveItemGet(req fakeGraphRequest) {
	switch req.action {
	case "":
		f.writeJSON(req.w, http.StatusOK, f.itemJSON(req.item))
	case fakeGraphActionChildren:
		if !req.item.Folder {
			// What OneDrive answers when asked for a file's children.
			f.writeJSON(req.w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
				"code":    "invalidRequest",
				"message": "Children cannot be listed from an item that is not a folder",
			}})
			return
		}
		f.writeJSON(req.w, http.StatusOK, map[string]any{"value": f.childrenOf(req.id)})
	case fakeGraphActionContent:
		f.serveContent(req.w, req.r, req.item)
	default:
		f.unexpected(req.w, req.r)
	}
}

func (f *fakeGraphDrive) serveItemPost(req fakeGraphRequest) {
	switch req.action {
	case fakeGraphActionChildren:
		var body struct {
			Name string `json:"name"`
		}
		f.decode(req.r, &body)
		if f.childNamed(req.id, body.Name) != nil {
			f.writeJSON(req.w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "nameAlreadyExists", "message": "exists"}})
			return
		}
		f.writeJSON(req.w, http.StatusCreated, f.itemJSON(f.addLocked(req.id, body.Name, true, nil)))
	case fakeGraphActionCopy:
		var body fakeGraphMoveBody
		f.decode(req.r, &body)
		name := body.Name
		if name == "" {
			name = req.item.Name
		}
		copied := f.addLocked(body.ParentReference.ID, name, req.item.Folder, append([]byte(nil), req.item.Content...))
		// Monitor URLs must be HTTPS on a Microsoft host; nothing fetches
		// this one, because tests stand in for the poll (cliSeams.cpAwaitCopy).
		req.w.Header().Set("Location", fakeGraphCopyMonitorPrefix+copied.ID)
		req.w.WriteHeader(http.StatusAccepted)
	default:
		f.unexpected(req.w, req.r)
	}
}

func (f *fakeGraphDrive) serveMove(req fakeGraphRequest) {
	if req.action != "" {
		f.unexpected(req.w, req.r)
		return
	}

	var body fakeGraphMoveBody
	f.decode(req.r, &body)
	if f.hangRenameTo != "" && body.Name == f.hangRenameTo {
		<-req.r.Context().Done()
		return
	}
	if body.Name != "" {
		req.item.Name = body.Name
	}
	if body.ParentReference.ID != "" {
		req.item.ParentID = body.ParentReference.ID
	}
	if body.FileSystemInfo.LastModifiedDateTime != "" {
		req.item.Modified = body.FileSystemInfo.LastModifiedDateTime
	}
	f.writeJSON(req.w, http.StatusOK, f.itemJSON(req.item))
}

func (f *fakeGraphDrive) serveRecycleAction(req fakeGraphRequest) {
	recycled := f.recycle[req.id]
	if req.r.Method != http.MethodPost || recycled == nil {
		f.unexpected(req.w, req.r)
		return
	}

	delete(f.recycle, req.id)
	if req.action == fakeGraphActionPermanentDelete {
		req.w.WriteHeader(http.StatusNoContent)
		return
	}

	f.items[req.id] = recycled
	f.writeJSON(req.w, http.StatusOK, f.itemJSON(recycled))
}

func (f *fakeGraphDrive) serveContent(w http.ResponseWriter, r *http.Request, item *fakeGraphItem) {
	if item == nil {
		f.unexpected(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(item.Content); err != nil {
		f.t.Errorf("fake graph: write content: %v", err)
	}
}

func (f *fakeGraphDrive) upload(w http.ResponseWriter, r *http.Request, parentID, name string) {
	content, err := io.ReadAll(r.Body)
	if err != nil {
		f.unexpected(w, r)
		return
	}

	parent := f.items[parentID]
	if strings.Contains(name, "/") {
		parent = f.byPath(path.Dir(name))
		name = path.Base(name)
	}
	if parent == nil {
		f.unexpected(w, r)
		return
	}

	if existing := f.childNamed(parent.ID, name); existing != nil {
		existing.Content = content
		f.writeJSON(w, http.StatusOK, f.itemJSON(existing))
		return
	}

	f.writeJSON(w, http.StatusCreated, f.itemJSON(f.addLocked(parent.ID, name, false, content)))
}

func (f *fakeGraphDrive) decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		f.t.Errorf("fake graph: decode %s %s: %v", r.Method, r.URL.Path, err)
	}
}

func (f *fakeGraphDrive) notFound(w http.ResponseWriter) {
	f.writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "itemNotFound", "message": "not found"}})
}

func (f *fakeGraphDrive) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("fake graph: encode response: %v", err)
	}
}

func (f *fakeGraphDrive) unexpected(w http.ResponseWriter, r *http.Request) {
	f.t.Errorf("fake graph: unexpected request %s %s", r.Method, r.URL.String())
	http.Error(w, "unexpected request", http.StatusNotImplemented)
}
