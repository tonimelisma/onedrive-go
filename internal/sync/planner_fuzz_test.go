package sync

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
)

const (
	maxPlannerFuzzPaths   = 6
	maxPlannerFuzzEvents  = 3
	maxPlannerFuzzPayload = 8192

	plannerDownloadWord  = "download"
	plannerUploadWord    = "upload"
	plannerFolderWord    = "folder"
	plannerRootWord      = "root"
	plannerDirWord       = "dir"
	plannerParentSegment = ".."
)

type plannerFuzzCase struct {
	Mode                  string                   `json:"mode"`
	DeleteSafetyThreshold int                      `json:"delete_safety_threshold"`
	DeniedPrefixes        []string                 `json:"denied_prefixes"`
	Baseline              []plannerFuzzBaselineRow `json:"baseline"`
	Changes               []plannerFuzzPathChanges `json:"changes"`
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
	changes        []PathChanges
	baseline       *Baseline
	mode           Mode
	config         *SafetyConfig
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
	plan, err := planner.Plan(decoded.changes, decoded.baseline, decoded.mode, decoded.config, decoded.deniedPrefixes)

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

	baselineEntries := make([]*BaselineEntry, 0, minInt(len(raw.Baseline), maxPlannerFuzzPaths))
	seenBaselinePaths := make(map[string]struct{}, maxPlannerFuzzPaths)
	for i := 0; i < len(raw.Baseline) && len(baselineEntries) < maxPlannerFuzzPaths; i++ {
		entry := &raw.Baseline[i]

		fallbackPath := fmt.Sprintf("baseline-%d.txt", i)
		safePath := sanitizePlannerPath(entry.Path, fallbackPath)
		if _, exists := seenBaselinePaths[safePath]; exists {
			continue
		}
		seenBaselinePaths[safePath] = struct{}{}

		itemType := parsePlannerItemType(entry.ItemType)
		if isFolderLikePath(safePath) {
			itemType = ItemTypeFolder
		}

		localHash := entry.LocalHash
		remoteHash := entry.RemoteHash
		if itemType != ItemTypeFile {
			localHash = ""
			remoteHash = ""
		}

		baselineEntries = append(baselineEntries, &BaselineEntry{
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

	baseline := newBaselineForTest(baselineEntries)
	changes := make([]PathChanges, 0, minInt(len(raw.Changes), maxPlannerFuzzPaths))
	seenChangePaths := make(map[string]struct{}, maxPlannerFuzzPaths)
	for i := 0; i < len(raw.Changes) && len(changes) < maxPlannerFuzzPaths; i++ {
		change := &raw.Changes[i]

		fallbackPath := fmt.Sprintf("change-%d.txt", i)
		safePath := sanitizePlannerPath(change.Path, fallbackPath)
		if _, exists := seenChangePaths[safePath]; exists {
			continue
		}
		seenChangePaths[safePath] = struct{}{}

		pc := PathChanges{Path: safePath}
		pc.RemoteEvents = decodePlannerEvents(change.RemoteEvents, SourceRemote, safePath)
		pc.LocalEvents = decodePlannerEvents(change.LocalEvents, SourceLocal, safePath)
		changes = append(changes, pc)
	}

	if len(changes) == 0 && len(baselineEntries) == 0 {
		return decodedPlannerFuzzCase{}, false
	}

	return decodedPlannerFuzzCase{
		changes:        changes,
		baseline:       baseline,
		mode:           parsePlannerMode(raw.Mode),
		config:         &SafetyConfig{DeleteSafetyThreshold: raw.DeleteSafetyThreshold},
		deniedPrefixes: sanitizePlannerPrefixes(raw.DeniedPrefixes),
	}, true
}

func decodePlannerEvents(
	rawEvents []plannerFuzzEvent,
	source ChangeSource,
	fallbackPath string,
) []ChangeEvent {
	if len(rawEvents) == 0 {
		return nil
	}

	events := make([]ChangeEvent, 0, minInt(len(rawEvents), maxPlannerFuzzEvents))
	for i := 0; i < len(rawEvents) && len(events) < maxPlannerFuzzEvents; i++ {
		raw := &rawEvents[i]

		itemType := parsePlannerItemType(raw.ItemType)
		if isFolderLikePath(fallbackPath) {
			itemType = ItemTypeFolder
		}

		eventType := parsePlannerChangeType(raw.Type)
		event := ChangeEvent{
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
			IsDeleted:     raw.IsDeleted || eventType == ChangeDelete,
			RemoteDriveID: raw.RemoteDriveID,
			RemoteItemID:  raw.RemoteItemID,
		}

		if itemType != ItemTypeFile {
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

func parsePlannerMode(raw string) Mode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case plannerDownloadWord + "-only", plannerDownloadWord:
		return SyncDownloadOnly
	case plannerUploadWord + "-only", plannerUploadWord:
		return SyncUploadOnly
	default:
		return SyncBidirectional
	}
}

func parsePlannerItemType(raw string) ItemType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case plannerFolderWord:
		return ItemTypeFolder
	case plannerRootWord:
		return ItemTypeRoot
	default:
		return ItemTypeFile
	}
}

func parsePlannerChangeType(raw string) ChangeType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "create":
		return ChangeCreate
	case "delete":
		return ChangeDelete
	case "move":
		return ChangeMove
	default:
		return ChangeModify
	}
}

func parsePlannerActionType(raw string) (ActionType, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case plannerDownloadWord:
		return ActionDownload, true
	case plannerUploadWord:
		return ActionUpload, true
	case "local_delete":
		return ActionLocalDelete, true
	case "remote_delete":
		return ActionRemoteDelete, true
	case "local_move":
		return ActionLocalMove, true
	case "remote_move":
		return ActionRemoteMove, true
	case "folder_create":
		return ActionFolderCreate, true
	case "conflict":
		return ActionConflict, true
	case "update_synced":
		return ActionUpdateSynced, true
	case "cleanup":
		return ActionCleanup, true
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

	result := make([]string, 0, minInt(len(prefixes), maxPlannerFuzzPaths))
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
	if cleaned == "." || cleaned == "" || cleaned == plannerParentSegment || strings.HasPrefix(cleaned, plannerParentSegment+"/") {
		return ""
	}

	parts := strings.Split(cleaned, "/")
	for i := range parts {
		part := parts[i]
		if part == "" || part == "." || part == plannerParentSegment {
			return ""
		}
	}

	return cleaned
}

func isFolderLikePath(p string) bool {
	base := strings.ToLower(path.Base(p))
	return strings.Contains(base, plannerFolderWord) || strings.Contains(base, plannerDirWord)
}

func defaultPlannerString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}

func plannerOutcomeFingerprint(plan *ActionPlan, err error) string {
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

func summarizePlannerPlan(plan *ActionPlan) *plannerPlanSummary {
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
			"delete_safety_threshold":1000,
			"baseline":[
				{"path":"converge.txt","drive_id":"drive-1","item_id":"item-1","item_type":"file","local_hash":"old","remote_hash":"old"}
			],
			"changes":[
				{"path":"converge.txt","remote_events":[{"type":"modify","item_type":"file","hash":"new","item_id":"item-1","drive_id":"drive-1"}],"local_events":[{"type":"modify","item_type":"file","hash":"new"}]}
			]
		}`),
		[]byte(`{
			"mode":"bidirectional",
			"delete_safety_threshold":1000,
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
			"delete_safety_threshold":1000,
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
			"delete_safety_threshold":1000,
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

func minInt(a, b int) int {
	if a < b {
		return a
	}

	return b
}
