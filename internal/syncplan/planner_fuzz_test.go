package syncplan

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	maxPlannerFuzzPaths          = 6
	maxPlannerFuzzEvents         = 3
	maxPlannerFuzzPayload        = 8192
	plannerFuzzFolderToken       = "folder"
	plannerFuzzDownloadToken     = "download"
	plannerFuzzUploadToken       = "upload"
	plannerFuzzParentDirToken    = ".."
	plannerFuzzDownloadOnlyToken = "download-only"
	plannerFuzzUploadOnlyToken   = "upload-only"
)

type plannerFuzzCase struct {
	Mode               string                   `json:"mode"`
	BigDeleteThreshold int                      `json:"big_delete_threshold"`
	DeniedPrefixes     []string                 `json:"denied_prefixes"`
	Baseline           []plannerFuzzBaselineRow `json:"baseline"`
	Changes            []plannerFuzzPathChanges `json:"changes"`
}

type plannerFuzzBaselineRow struct {
	Path            string `json:"path"`
	DriveID         string `json:"drive_id"`
	ItemID          string `json:"item_id"`
	ItemType        string `json:"item_type"`
	LocalHash       string `json:"local_hash"`
	RemoteHash      string `json:"remote_hash"`
	LocalSize       int64  `json:"local_size"`
	LocalSizeKnown  bool   `json:"local_size_known"`
	RemoteSize      int64  `json:"remote_size"`
	RemoteSizeKnown bool   `json:"remote_size_known"`
	LocalMtime      int64  `json:"local_mtime"`
	RemoteMtime     int64  `json:"remote_mtime"`
	ETag            string `json:"etag"`
}

type plannerFuzzPathChanges struct {
	Path         string             `json:"path"`
	RemoteEvents []plannerFuzzEvent `json:"remote_events"`
	LocalEvents  []plannerFuzzEvent `json:"local_events"`
}

type plannerFuzzEvent struct {
	Type          string `json:"type"`
	ItemType      string `json:"item_type"`
	OldPath       string `json:"old_path"`
	Hash          string `json:"hash"`
	ItemID        string `json:"item_id"`
	DriveID       string `json:"drive_id"`
	ParentID      string `json:"parent_id"`
	Size          int64  `json:"size"`
	Mtime         int64  `json:"mtime"`
	ETag          string `json:"etag"`
	IsDeleted     bool   `json:"is_deleted"`
	ForcedAction  string `json:"forced_action"`
	RemoteDriveID string `json:"remote_drive_id"`
	RemoteItemID  string `json:"remote_item_id"`
}

type decodedPlannerFuzzCase struct {
	changes        []synctypes.PathChanges
	baseline       *synctypes.Baseline
	mode           synctypes.SyncMode
	config         *synctypes.SafetyConfig
	deniedPrefixes []string
}

func FuzzPlannerPlan_DAGInvariants(f *testing.F) {
	for _, seed := range plannerFuzzSeeds() {
		f.Add(seed)
	}

	logger := slog.New(slog.DiscardHandler)

	f.Fuzz(func(t *testing.T, data []byte) {
		outcome1, ok := runPlannerFuzzCase(data, logger)
		if !ok {
			return
		}
		assertPlannerFuzzOutcome(t, outcome1)

		outcome2, ok := runPlannerFuzzCase(data, logger)
		if !ok {
			return
		}
		assertPlannerFuzzOutcome(t, outcome2)

		assert.Equal(t, outcome1, outcome2, "planner output is nondeterministic for identical input")
	})
}

func runPlannerFuzzCase(data []byte, logger *slog.Logger) (string, bool) {
	decoded, ok := decodePlannerFuzzCase(data)
	if !ok {
		return "", false
	}

	planner := NewPlanner(logger)
	plan, err := planner.Plan(
		decoded.changes,
		decoded.baseline,
		decoded.mode,
		decoded.config,
		decoded.deniedPrefixes,
	)

	return plannerOutcomeFingerprint(plan, err), true
}

func decodePlannerFuzzCase(data []byte) (decodedPlannerFuzzCase, bool) {
	if len(data) == 0 || len(data) > maxPlannerFuzzPayload {
		return decodedPlannerFuzzCase{}, false
	}

	var raw plannerFuzzCase
	if err := json.Unmarshal(data, &raw); err != nil {
		return decodedPlannerFuzzCase{}, false
	}

	baselineEntries := make([]*synctypes.BaselineEntry, 0, min(len(raw.Baseline), maxPlannerFuzzPaths))
	seenBaselinePaths := make(map[string]struct{}, maxPlannerFuzzPaths)
	for i := range raw.Baseline {
		if len(baselineEntries) == maxPlannerFuzzPaths {
			break
		}

		entry := &raw.Baseline[i]
		fallbackPath := fmt.Sprintf("baseline-%d.txt", i)
		safePath := sanitizePlannerPath(entry.Path, fallbackPath)
		if _, exists := seenBaselinePaths[safePath]; exists {
			continue
		}
		seenBaselinePaths[safePath] = struct{}{}

		itemType := parsePlannerItemType(entry.ItemType)
		if isFolderLikePath(safePath) {
			itemType = synctypes.ItemTypeFolder
		}

		localHash := entry.LocalHash
		remoteHash := entry.RemoteHash
		if itemType != synctypes.ItemTypeFile {
			localHash = ""
			remoteHash = ""
		}

		baselineEntries = append(baselineEntries, &synctypes.BaselineEntry{
			Path:            safePath,
			DriveID:         parsePlannerDriveID(entry.DriveID, fmt.Sprintf("drive-%d", i%2+1)),
			ItemID:          defaultPlannerString(entry.ItemID, fmt.Sprintf("baseline-item-%d", i)),
			ItemType:        itemType,
			LocalHash:       localHash,
			RemoteHash:      remoteHash,
			LocalSize:       entry.LocalSize,
			LocalSizeKnown:  entry.LocalSizeKnown,
			RemoteSize:      entry.RemoteSize,
			RemoteSizeKnown: entry.RemoteSizeKnown,
			LocalMtime:      entry.LocalMtime,
			RemoteMtime:     entry.RemoteMtime,
			ETag:            entry.ETag,
		})
	}

	baseline := synctypes.NewBaselineForTest(baselineEntries)
	changes := make([]synctypes.PathChanges, 0, min(len(raw.Changes), maxPlannerFuzzPaths))
	seenChangePaths := make(map[string]struct{}, maxPlannerFuzzPaths)
	for i := range raw.Changes {
		if len(changes) == maxPlannerFuzzPaths {
			break
		}

		change := &raw.Changes[i]
		fallbackPath := fmt.Sprintf("change-%d.txt", i)
		safePath := sanitizePlannerPath(change.Path, fallbackPath)
		if _, exists := seenChangePaths[safePath]; exists {
			continue
		}
		seenChangePaths[safePath] = struct{}{}

		pc := synctypes.PathChanges{Path: safePath}
		pc.RemoteEvents = decodePlannerEvents(change.RemoteEvents, synctypes.SourceRemote, safePath)
		pc.LocalEvents = decodePlannerEvents(change.LocalEvents, synctypes.SourceLocal, safePath)
		changes = append(changes, pc)
	}

	if len(changes) == 0 && len(baselineEntries) == 0 {
		return decodedPlannerFuzzCase{}, false
	}

	return decodedPlannerFuzzCase{
		changes:        changes,
		baseline:       baseline,
		mode:           parsePlannerMode(raw.Mode),
		config:         &synctypes.SafetyConfig{BigDeleteThreshold: raw.BigDeleteThreshold},
		deniedPrefixes: sanitizePlannerPrefixes(raw.DeniedPrefixes),
	}, true
}

func decodePlannerEvents(
	rawEvents []plannerFuzzEvent,
	source synctypes.ChangeSource,
	fallbackPath string,
) []synctypes.ChangeEvent {
	if len(rawEvents) == 0 {
		return nil
	}

	events := make([]synctypes.ChangeEvent, 0, min(len(rawEvents), maxPlannerFuzzEvents))
	for i := range rawEvents {
		if len(events) == maxPlannerFuzzEvents {
			break
		}

		raw := &rawEvents[i]
		itemType := parsePlannerItemType(raw.ItemType)
		if isFolderLikePath(fallbackPath) {
			itemType = synctypes.ItemTypeFolder
		}

		eventType := parsePlannerChangeType(raw.Type)
		event := synctypes.ChangeEvent{
			Source:        source,
			Type:          eventType,
			Path:          fallbackPath,
			OldPath:       sanitizePlannerOptionalPath(raw.OldPath),
			ItemID:        defaultPlannerString(raw.ItemID, fmt.Sprintf("event-item-%d", i)),
			ParentID:      raw.ParentID,
			DriveID:       parsePlannerDriveID(raw.DriveID, fmt.Sprintf("drive-%d", i%2+1)),
			ItemType:      itemType,
			Name:          path.Base(fallbackPath),
			Size:          raw.Size,
			Hash:          raw.Hash,
			Mtime:         raw.Mtime,
			ETag:          raw.ETag,
			IsDeleted:     raw.IsDeleted || eventType == synctypes.ChangeDelete,
			RemoteDriveID: raw.RemoteDriveID,
			RemoteItemID:  raw.RemoteItemID,
		}

		if itemType != synctypes.ItemTypeFile {
			event.Hash = ""
		}

		if actionType, hasForced := parsePlannerActionType(raw.ForcedAction); hasForced {
			event.ForcedAction = actionType
			event.HasForcedAction = true
		}

		events = append(events, event)
	}

	return events
}

func parsePlannerMode(raw string) synctypes.SyncMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case plannerFuzzDownloadOnlyToken, plannerFuzzDownloadToken:
		return synctypes.SyncDownloadOnly
	case plannerFuzzUploadOnlyToken, plannerFuzzUploadToken:
		return synctypes.SyncUploadOnly
	default:
		return synctypes.SyncBidirectional
	}
}

func parsePlannerItemType(raw string) synctypes.ItemType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case plannerFuzzFolderToken:
		return synctypes.ItemTypeFolder
	case "root":
		return synctypes.ItemTypeRoot
	default:
		return synctypes.ItemTypeFile
	}
}

func parsePlannerChangeType(raw string) synctypes.ChangeType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "create":
		return synctypes.ChangeCreate
	case "delete":
		return synctypes.ChangeDelete
	case "move":
		return synctypes.ChangeMove
	default:
		return synctypes.ChangeModify
	}
}

func parsePlannerActionType(raw string) (synctypes.ActionType, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case plannerFuzzDownloadToken:
		return synctypes.ActionDownload, true
	case plannerFuzzUploadToken:
		return synctypes.ActionUpload, true
	case "local_delete":
		return synctypes.ActionLocalDelete, true
	case "remote_delete":
		return synctypes.ActionRemoteDelete, true
	case "local_move":
		return synctypes.ActionLocalMove, true
	case "remote_move":
		return synctypes.ActionRemoteMove, true
	case "folder_create":
		return synctypes.ActionFolderCreate, true
	case "conflict":
		return synctypes.ActionConflict, true
	case "update_synced":
		return synctypes.ActionUpdateSynced, true
	case "cleanup":
		return synctypes.ActionCleanup, true
	default:
		return 0, false
	}
}

func parsePlannerDriveID(raw, fallback string) driveid.ID {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = fallback
	}
	if strings.EqualFold(value, "zero") {
		return driveid.ID{}
	}

	return driveid.New(value)
}

func sanitizePlannerPrefixes(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}

	result := make([]string, 0, min(len(prefixes), maxPlannerFuzzPaths))
	seen := make(map[string]struct{}, maxPlannerFuzzPaths)
	for i, prefix := range prefixes {
		if len(result) == maxPlannerFuzzPaths {
			break
		}

		safePrefix := sanitizePlannerOptionalPath(prefix)
		if safePrefix == "" {
			safePrefix = sanitizePlannerPath(prefix, fmt.Sprintf("denied-%d", i))
		}
		if safePrefix == "" {
			continue
		}
		if _, exists := seen[safePrefix]; exists {
			continue
		}
		seen[safePrefix] = struct{}{}
		result = append(result, safePrefix)
	}

	return result
}

func sanitizePlannerPath(raw, fallback string) string {
	cleaned := sanitizePlannerOptionalPath(raw)
	if cleaned == "" {
		cleaned = sanitizePlannerOptionalPath(fallback)
	}
	if cleaned == "" {
		return "fuzz.txt"
	}

	return cleaned
}

func sanitizePlannerOptionalPath(raw string) string {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if raw == "" {
		return ""
	}

	cleaned := path.Clean(strings.TrimPrefix(raw, "/"))
	if cleaned == "." || cleaned == "" || cleaned == plannerFuzzParentDirToken || strings.HasPrefix(cleaned, "../") {
		return ""
	}

	parts := strings.Split(cleaned, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == plannerFuzzParentDirToken {
			return ""
		}
	}

	return cleaned
}

func isFolderLikePath(path string) bool {
	base := strings.ToLower(pathpkgBase(path))
	return strings.Contains(base, plannerFuzzFolderToken) || strings.Contains(base, "dir")
}

func defaultPlannerString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}

func plannerOutcomeFingerprint(plan *synctypes.ActionPlan, err error) string {
	outcome := struct {
		Error string              `json:"error,omitempty"`
		Plan  *plannerPlanSummary `json:"plan,omitempty"`
	}{
		Error: plannerErrorFingerprint(err),
	}
	if plan != nil {
		outcome.Plan = summarizePlannerPlan(plan)
	}

	data, marshalErr := json.Marshal(outcome)
	if marshalErr != nil {
		return fmt.Sprintf(`{"marshal_error":%q}`, marshalErr.Error())
	}

	return string(data)
}

func summarizePlannerPlan(plan *synctypes.ActionPlan) *plannerPlanSummary {
	if plan == nil {
		return nil
	}

	summary := &plannerPlanSummary{
		Actions: make([]plannerActionSummary, len(plan.Actions)),
		Deps:    make([][]int, len(plan.Deps)),
	}
	for i := range plan.Actions {
		action := &plan.Actions[i]
		summary.Actions[i] = plannerActionSummary{
			Type:                action.Type.String(),
			Path:                action.Path,
			OldPath:             action.OldPath,
			DriveID:             action.DriveID.String(),
			ItemID:              action.ItemID,
			TargetShortcutKey:   action.TargetShortcutKey,
			TargetDriveID:       action.TargetDriveID.String(),
			TargetRootItemID:    action.TargetRootItemID,
			TargetRootLocalPath: action.TargetRootLocalPath,
		}
		if action.View != nil {
			summary.Actions[i].HasView = true
		}
		if action.ConflictInfo != nil {
			summary.Actions[i].ConflictType = action.ConflictInfo.ConflictType
		}
	}
	for i := range plan.Deps {
		summary.Deps[i] = append([]int(nil), plan.Deps[i]...)
	}

	return summary
}

type plannerPlanSummary struct {
	Actions []plannerActionSummary `json:"actions"`
	Deps    [][]int                `json:"deps"`
}

type plannerActionSummary struct {
	Type                string `json:"type"`
	Path                string `json:"path"`
	OldPath             string `json:"old_path,omitempty"`
	DriveID             string `json:"drive_id,omitempty"`
	ItemID              string `json:"item_id,omitempty"`
	HasView             bool   `json:"has_view,omitempty"`
	ConflictType        string `json:"conflict_type,omitempty"`
	TargetShortcutKey   string `json:"target_shortcut_key,omitempty"`
	TargetDriveID       string `json:"target_drive_id,omitempty"`
	TargetRootItemID    string `json:"target_root_item_id,omitempty"`
	TargetRootLocalPath string `json:"target_root_local_path,omitempty"`
}

func plannerErrorFingerprint(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func assertPlannerFuzzOutcome(t *testing.T, fingerprint string) {
	t.Helper()

	var outcome struct {
		Error string              `json:"error"`
		Plan  *plannerPlanSummary `json:"plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(fingerprint), &outcome), "decode planner outcome fingerprint")
	require.True(t, outcome.Plan != nil || outcome.Error != "", "planner returned nil plan without an error")

	if outcome.Plan == nil {
		return
	}

	require.Len(t, outcome.Plan.Deps, len(outcome.Plan.Actions), "plan deps/action length mismatch")

	for i, deps := range outcome.Plan.Deps {
		for _, dep := range deps {
			assert.GreaterOrEqualf(t, dep, 0, "dependency index out of range: action %d depends on %d", i, dep)
			assert.Lessf(t, dep, len(outcome.Plan.Actions), "dependency index out of range: action %d depends on %d with %d actions", i, dep, len(outcome.Plan.Actions))
			assert.NotEqualf(t, i, dep, "self-dependency detected at action %d", i)
		}
	}

	assert.False(t, plannerDepsHaveCycle(outcome.Plan.Deps), "dependency cycle detected in planner output")
}

func plannerDepsHaveCycle(deps [][]int) bool {
	const (
		unseen = iota
		visiting
		done
	)

	state := make([]int, len(deps))
	var visit func(int) bool
	visit = func(node int) bool {
		switch state[node] {
		case visiting:
			return true
		case done:
			return false
		}

		state[node] = visiting
		for _, dep := range deps[node] {
			if visit(dep) {
				return true
			}
		}
		state[node] = done

		return false
	}

	for i := range deps {
		if visit(i) {
			return true
		}
	}

	return false
}

func plannerFuzzSeeds() [][]byte {
	return [][]byte{
		[]byte(`{
			"mode":"bidirectional",
			"big_delete_threshold":1000,
			"baseline":[
				{"path":"converge.txt","drive_id":"drive-1","item_id":"item-1","item_type":"file","local_hash":"old","remote_hash":"old"}
			],
			"changes":[
				{"path":"converge.txt","remote_events":[{"type":"modify","item_type":"file","hash":"new","item_id":"item-1","drive_id":"drive-1"}],"local_events":[{"type":"modify","item_type":"file","hash":"new"}]}
			]
		}`),
		[]byte(`{
			"mode":"bidirectional",
			"big_delete_threshold":1000,
			"baseline":[
				{"path":"folder","drive_id":"drive-1","item_id":"folder-1","item_type":"folder"},
				{"path":"folder/child.txt","drive_id":"drive-1","item_id":"file-1","item_type":"file","local_hash":"same","remote_hash":"same"}
			],
			"changes":[
				{"path":"folder","remote_events":[{"type":"delete","item_type":"folder","item_id":"folder-1","drive_id":"drive-1","is_deleted":true}]}
			]
		}`),
		[]byte(`{
			"mode":"bidirectional",
			"big_delete_threshold":1000,
			"baseline":[
				{"path":"own/file.txt","drive_id":"drive-a","item_id":"item-1","item_type":"file","local_hash":"hash-1","remote_hash":"hash-1"},
				{"path":"shared","drive_id":"drive-b","item_id":"shortcut-root","item_type":"folder"}
			],
			"changes":[
				{"path":"own/file.txt","local_events":[{"type":"delete","item_type":"file","is_deleted":true}]},
				{"path":"shared/file.txt","local_events":[{"type":"create","item_type":"file","hash":"hash-1"}]}
			]
		}`),
		[]byte(`{
			"mode":"download-only",
			"big_delete_threshold":1000,
			"denied_prefixes":["shared"],
			"baseline":[
				{"path":"planner-replay.txt","drive_id":"drive-1","item_id":"item-1","item_type":"file","local_hash":"hash-a","remote_hash":"hash-a","etag":"etag-a"}
			],
			"changes":[
				{"path":"planner-replay.txt","remote_events":[{"type":"modify","item_type":"file","hash":"hash-a","item_id":"item-1","drive_id":"drive-1","forced_action":"download","etag":"etag-a"}]}
			]
		}`),
	}
}

func pathpkgBase(p string) string {
	if p == "" {
		return ""
	}

	return path.Base(p)
}
