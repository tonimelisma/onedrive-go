package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	scopeExplainIncluded         = "included"
	scopeExplainExcludedByPath   = "excluded_by_path_scope"
	scopeExplainExcludedByMarker = "excluded_by_marker_scope"
	scopeExplainNotObserved      = "not_observed"
)

type scopeStateInspector interface {
	ReadScopeStateSnapshot(ctx context.Context) (syncstore.ScopeStateSnapshot, error)
	Close() error
}

type syncScopeModel struct {
	Drive             string                         `json:"drive"`
	SyncDir           string                         `json:"sync_dir"`
	IgnoreMarker      string                         `json:"ignore_marker,omitempty"`
	SyncPaths         []string                       `json:"sync_paths"`
	MarkerDirs        []string                       `json:"marker_dirs,omitempty"`
	Generation        int64                          `json:"generation"`
	ObservationMode   synctypes.ScopeObservationMode `json:"observation_mode"`
	WebsocketEnabled  bool                           `json:"websocket_enabled"`
	PendingReentry    bool                           `json:"pending_reentry"`
	LastReconcileKind synctypes.ScopeReconcileKind   `json:"last_reconcile_kind"`
	Persisted         bool                           `json:"persisted"`
}

type syncScopeExplainModel struct {
	InputPath        string                         `json:"input_path"`
	EffectivePath    string                         `json:"effective_path,omitempty"`
	Status           string                         `json:"status"`
	MatchingRule     string                         `json:"matching_rule,omitempty"`
	ObservationMode  synctypes.ScopeObservationMode `json:"observation_mode"`
	WebsocketEnabled bool                           `json:"websocket_enabled"`
}

type syncScopeService struct {
	cc                 *CLIContext
	openInspector      func(dbPath string, logger *slog.Logger) (scopeStateInspector, error)
	buildScopeSnapshot func(ctx context.Context, tree *synctree.Root, cfg syncscope.Config, logger *slog.Logger) (syncscope.Snapshot, error)
}

func newSyncScopeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scope",
		Short: "Inspect the effective sync scope",
		Long: `Show the effective sync scope for the selected drive, including normalized
sync_paths, active ignore_marker exclusions, and the current observation mode.`,
		RunE: runSyncScope,
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "explain <path>",
		Short: "Explain why a path is or is not in sync scope",
		Args:  cobra.ExactArgs(1),
		RunE:  runSyncScopeExplain,
	})

	return cmd
}

func runSyncScope(cmd *cobra.Command, _ []string) error {
	return newSyncScopeService(mustCLIContext(cmd.Context())).runList(cmd.Context())
}

func runSyncScopeExplain(cmd *cobra.Command, args []string) error {
	return newSyncScopeService(mustCLIContext(cmd.Context())).runExplain(cmd.Context(), args[0])
}

func newSyncScopeService(cc *CLIContext) *syncScopeService {
	return &syncScopeService{
		cc: cc,
		openInspector: func(dbPath string, logger *slog.Logger) (scopeStateInspector, error) {
			return syncstore.OpenInspector(dbPath, logger)
		},
		buildScopeSnapshot: syncobserve.BuildScopeSnapshot,
	}
}

func (s *syncScopeService) runList(ctx context.Context) error {
	model, _, _, err := s.loadScopeModel(ctx)
	if err != nil {
		return err
	}

	if s.cc.Flags.JSON {
		return printSyncScopeJSON(s.cc.Output(), &model)
	}

	return printSyncScopeText(s.cc.Output(), &model)
}

func (s *syncScopeService) runExplain(ctx context.Context, inputPath string) error {
	model, snapshot, tree, err := s.loadScopeModel(ctx)
	if err != nil {
		return err
	}

	explain := s.buildExplainModel(inputPath, snapshot, tree, &model)
	if s.cc.Flags.JSON {
		return printSyncScopeExplainJSON(s.cc.Output(), explain)
	}

	return printSyncScopeExplainText(s.cc.Output(), explain)
}

func (s *syncScopeService) loadScopeModel(
	ctx context.Context,
) (syncScopeModel, syncscope.Snapshot, *synctree.Root, error) {
	tree, err := synctree.Open(s.cc.Cfg.SyncDir)
	if err != nil {
		return syncScopeModel{}, syncscope.Snapshot{}, nil, fmt.Errorf("open sync dir %s: %w", s.cc.Cfg.SyncDir, err)
	}

	scopeCfg := syncscope.Config{
		SyncPaths:    append([]string(nil), s.cc.Cfg.SyncPaths...),
		IgnoreMarker: s.cc.Cfg.IgnoreMarker,
	}
	liveSnapshot, err := s.buildScopeSnapshot(ctx, tree, scopeCfg, s.cc.Logger)
	if err != nil {
		return syncScopeModel{}, syncscope.Snapshot{}, nil, fmt.Errorf("build effective sync scope: %w", err)
	}

	persistedState, persistedSnapshot, err := s.readPersistedScope(ctx)
	if err != nil {
		return syncScopeModel{}, syncscope.Snapshot{}, nil, err
	}

	diff := syncscope.DiffSnapshots(persistedSnapshot, liveSnapshot)
	mode := inferScopeObservationMode(s.cc.Cfg.CanonicalID, liveSnapshot)
	model := syncScopeModel{
		Drive:             s.cc.Cfg.CanonicalID.String(),
		SyncDir:           s.cc.Cfg.SyncDir,
		IgnoreMarker:      liveSnapshot.IgnoreMarker(),
		SyncPaths:         displayScopePaths(liveSnapshot.SyncPaths()),
		MarkerDirs:        displayScopePaths(liveSnapshot.MarkerDirs()),
		Generation:        predictedScopeGeneration(persistedState, diff),
		ObservationMode:   mode,
		WebsocketEnabled:  s.cc.Cfg.Websocket && mode == synctypes.ScopeObservationRootDelta,
		PendingReentry:    persistedState.PendingReentry || diff.HasEntered(),
		LastReconcileKind: persistedLastReconcileKind(persistedState),
		Persisted:         persistedState.Found,
	}

	if len(model.SyncPaths) == 0 {
		model.SyncPaths = []string{"/"}
	}

	return model, liveSnapshot, tree, nil
}

func (s *syncScopeService) readPersistedScope(
	ctx context.Context,
) (syncstore.ScopeStateSnapshot, syncscope.Snapshot, error) {
	statePath := s.cc.Cfg.StatePath()
	if !managedPathExists(statePath) {
		return syncstore.ScopeStateSnapshot{}, syncscope.Snapshot{}, nil
	}

	inspector, err := s.openInspector(statePath, s.cc.Logger)
	if err != nil {
		return syncstore.ScopeStateSnapshot{}, syncscope.Snapshot{}, fmt.Errorf("open scope inspector: %w", err)
	}
	defer func() {
		if closeErr := inspector.Close(); closeErr != nil {
			s.cc.Logger.Debug("close scope inspector", slog.String("error", closeErr.Error()))
		}
	}()

	state, err := inspector.ReadScopeStateSnapshot(ctx)
	if err != nil {
		return syncstore.ScopeStateSnapshot{}, syncscope.Snapshot{}, fmt.Errorf("read scope state snapshot: %w", err)
	}
	if !state.Found {
		return syncstore.ScopeStateSnapshot{}, syncscope.Snapshot{}, nil
	}

	persistedSnapshot, err := syncscope.UnmarshalSnapshot(state.EffectiveSnapshotJSON)
	if err != nil {
		return syncstore.ScopeStateSnapshot{}, syncscope.Snapshot{}, fmt.Errorf("decode persisted scope snapshot: %w", err)
	}

	return state, persistedSnapshot, nil
}

func predictedScopeGeneration(state syncstore.ScopeStateSnapshot, diff syncscope.Diff) int64 {
	if !state.Found || state.Generation <= 0 {
		return 1
	}

	if diff.HasChanges() {
		return state.Generation + 1
	}

	return state.Generation
}

func persistedLastReconcileKind(state syncstore.ScopeStateSnapshot) synctypes.ScopeReconcileKind {
	if !state.Found || state.LastReconcileKind == "" {
		return synctypes.ScopeReconcileNone
	}

	return state.LastReconcileKind
}

func inferScopeObservationMode(
	canonicalID driveid.CanonicalID,
	snapshot syncscope.Snapshot,
) synctypes.ScopeObservationMode {
	if !snapshot.HasPathRules() {
		return synctypes.ScopeObservationRootDelta
	}

	if canonicalID.DriveType() == driveid.DriveTypePersonal {
		return synctypes.ScopeObservationScopedDelta
	}

	return synctypes.ScopeObservationScopedEnumerate
}

func displayScopePaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, displayScopePath(path))
	}
	sort.Strings(out)
	return out
}

func displayScopePath(path string) string {
	if path == "" {
		return "/"
	}

	return "/" + path
}

func (s *syncScopeService) buildExplainModel(
	inputPath string,
	snapshot syncscope.Snapshot,
	tree *synctree.Root,
	model *syncScopeModel,
) syncScopeExplainModel {
	effectivePath, observed := normalizeExplainPath(inputPath, tree)
	if !observed {
		return syncScopeExplainModel{
			InputPath:        inputPath,
			Status:           scopeExplainNotObserved,
			ObservationMode:  model.ObservationMode,
			WebsocketEnabled: model.WebsocketEnabled,
		}
	}

	status := scopeExplainIncluded
	switch snapshot.ExclusionReason(effectivePath) {
	case syncscope.ExclusionNone:
		status = scopeExplainIncluded
	case syncscope.ExclusionPathScope:
		status = scopeExplainExcludedByPath
	case syncscope.ExclusionMarkerScope:
		status = scopeExplainExcludedByMarker
	default:
		panic(fmt.Sprintf("unknown exclusion reason for %q", effectivePath))
	}

	return syncScopeExplainModel{
		InputPath:        inputPath,
		EffectivePath:    displayScopePath(effectivePath),
		Status:           status,
		MatchingRule:     explainMatchingRule(snapshot, effectivePath, status),
		ObservationMode:  model.ObservationMode,
		WebsocketEnabled: model.WebsocketEnabled,
	}
}

func normalizeExplainPath(inputPath string, tree *synctree.Root) (string, bool) {
	if filepath.IsAbs(inputPath) {
		relPath, err := tree.Rel(inputPath)
		if err != nil {
			return "", false
		}

		return syncscope.NormalizeRelativePath(relPath), true
	}

	normalized := syncscope.NormalizeRelativePath(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(inputPath)), "/"))
	return normalized, true
}

func explainMatchingRule(snapshot syncscope.Snapshot, relPath string, status string) string {
	switch status {
	case scopeExplainExcludedByMarker:
		if markerRule := deepestAncestorRule(relPath, snapshot.MarkerDirs()); markerRule != "" {
			return displayScopePath(markerRule)
		}
		if snapshot.IsMarkerFile(relPath) {
			parent := syncscope.NormalizeRelativePath(filepath.ToSlash(filepath.Dir(relPath)))
			if parent == "." {
				parent = ""
			}
			return displayScopePath(parent)
		}
	case scopeExplainIncluded:
		if syncRule := deepestOverlappingRule(relPath, snapshot.SyncPaths()); syncRule != "" {
			return displayScopePath(syncRule)
		}
		if len(snapshot.SyncPaths()) == 0 {
			return "/"
		}
	}

	return ""
}

func deepestAncestorRule(relPath string, rules []string) string {
	best := ""
	for _, rule := range rules {
		if rule == relPath || syncscope.CoversPath(rule, relPath) {
			if len(rule) > len(best) {
				best = rule
			}
		}
	}

	return best
}

func deepestOverlappingRule(relPath string, rules []string) string {
	best := ""
	for _, rule := range rules {
		if syncscope.CoversPath(rule, relPath) || syncscope.CoversPath(relPath, rule) {
			if len(rule) > len(best) {
				best = rule
			}
		}
	}

	return best
}

func printSyncScopeJSON(w io.Writer, model *syncScopeModel) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(model); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printSyncScopeExplainJSON(w io.Writer, model syncScopeExplainModel) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(model); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printSyncScopeText(w io.Writer, model *syncScopeModel) error {
	if err := writeScopeLine(w, "Drive", model.Drive); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Sync dir", model.SyncDir); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Generation", model.Generation); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Observation mode", model.ObservationMode); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Websocket enabled", model.WebsocketEnabled); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Pending re-entry", model.PendingReentry); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Last reconcile", model.LastReconcileKind); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Ignore marker", emptyOrNone(model.IgnoreMarker)); err != nil {
		return err
	}
	if err := writeScopePathSection(w, "Included paths", model.SyncPaths); err != nil {
		return err
	}
	if err := writeScopePathSection(w, "Marker exclusions", model.MarkerDirs); err != nil {
		return err
	}

	return nil
}

func printSyncScopeExplainText(w io.Writer, model syncScopeExplainModel) error {
	if err := writeScopeLine(w, "Input path", model.InputPath); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Status", model.Status); err != nil {
		return err
	}
	if model.EffectivePath != "" {
		if err := writeScopeLine(w, "Effective path", model.EffectivePath); err != nil {
			return err
		}
	}
	if model.MatchingRule != "" {
		if err := writeScopeLine(w, "Matching rule", model.MatchingRule); err != nil {
			return err
		}
	}
	if err := writeScopeLine(w, "Observation mode", model.ObservationMode); err != nil {
		return err
	}
	if err := writeScopeLine(w, "Websocket enabled", model.WebsocketEnabled); err != nil {
		return err
	}

	return nil
}

func writeScopeLine(w io.Writer, label string, value any) error {
	if _, err := fmt.Fprintf(w, "%s: %v\n", label, value); err != nil {
		return fmt.Errorf("write %s: %w", strings.ToLower(label), err)
	}

	return nil
}

func writeScopePathSection(w io.Writer, label string, paths []string) error {
	if _, err := fmt.Fprintf(w, "%s:\n", label); err != nil {
		return fmt.Errorf("write %s header: %w", strings.ToLower(label), err)
	}
	if len(paths) == 0 {
		if _, err := io.WriteString(w, "- (none)\n"); err != nil {
			return fmt.Errorf("write %s empty entry: %w", strings.ToLower(label), err)
		}

		return nil
	}

	for _, path := range paths {
		if _, err := fmt.Fprintf(w, "- %s\n", path); err != nil {
			return fmt.Errorf("write %s entry: %w", strings.ToLower(label), err)
		}
	}

	return nil
}

func emptyOrNone(value string) string {
	if value == "" {
		return "(none)"
	}

	return value
}
