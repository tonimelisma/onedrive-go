// Package syncscope normalizes configured sync scope and computes effective
// scope snapshots from configured paths plus locally discovered ignore markers.
package syncscope

import (
	"encoding/json"
	"fmt"
	slashpath "path"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const persistedVersion = 1

// ExclusionReason records why a path falls outside the effective sync scope.
// The zero value means the path is currently inside scope.
type ExclusionReason string

const (
	ExclusionNone        ExclusionReason = ""
	ExclusionPathScope   ExclusionReason = "path_scope"
	ExclusionMarkerScope ExclusionReason = "marker_scope"
)

// Config is the normalized user-configured sync scope. SyncPaths are stored
// as NFC-normalized relative paths from the drive root with ancestor overlaps
// collapsed. The empty string means the drive root.
type Config struct {
	SyncPaths    []string
	IgnoreMarker string
}

// Snapshot is one effective sync-scope view: normalized config plus the set
// of currently active locally discovered marker directories.
type Snapshot struct {
	config     Config
	markerDirs []string
}

// Diff describes the scope regions that became reachable or unreachable when
// moving from one snapshot to another.
type Diff struct {
	EnteredPaths []string
	ExitedPaths  []string
}

// Change is one observed transition from an old effective snapshot to a new
// one, together with the normalized entered/exited roots.
type Change struct {
	Old  Snapshot
	New  Snapshot
	Diff Diff
}

// PersistedSnapshot is the durable JSON projection stored in sync_metadata.
type PersistedSnapshot struct {
	Version      int      `json:"version"`
	SyncPaths    []string `json:"sync_paths,omitempty"`
	IgnoreMarker string   `json:"ignore_marker,omitempty"`
	MarkerDirs   []string `json:"marker_dirs,omitempty"`
}

// NormalizeConfig canonicalizes config input. sync_paths remain absolute in
// the user config, but the normalized Config stores relative drive-root paths.
func NormalizeConfig(cfg Config) (Config, error) {
	normalized := Config{
		IgnoreMarker: norm.NFC.String(cfg.IgnoreMarker),
	}

	if len(cfg.SyncPaths) == 0 {
		return normalized, nil
	}

	paths := make([]string, 0, len(cfg.SyncPaths))
	for _, raw := range cfg.SyncPaths {
		path, err := normalizeConfiguredPath(raw)
		if err != nil {
			return Config{}, err
		}

		paths = append(paths, path)
	}

	normalized.SyncPaths = collapseAncestorPaths(paths)

	return normalized, nil
}

// NormalizeRelativePath canonicalizes one sync-root-relative path using the
// same NFC and slash-cleaning rules as snapshot matching.
func NormalizeRelativePath(raw string) string {
	return normalizeRelativePath(raw)
}

// CollapseRelativePaths removes duplicate and ancestor-covered relative paths
// using the same case-insensitive component rules as sync-scope matching.
func CollapseRelativePaths(paths []string) []string {
	return collapseAncestorPaths(paths)
}

// CoversPath reports whether scopePath contains the same relative path or one
// of its descendants under the same case-insensitive component rules used by
// sync-scope matching.
func CoversPath(scopePath, candidate string) bool {
	scopePath = normalizeRelativePath(scopePath)
	candidate = normalizeRelativePath(candidate)

	return scopePath == "" || samePathFold(scopePath, candidate) || isAncestorFold(scopePath, candidate)
}

// NewSnapshot builds a normalized effective snapshot from config plus
// discovered marker directories.
func NewSnapshot(cfg Config, markerDirs []string) (Snapshot, error) {
	normalizedCfg, err := NormalizeConfig(cfg)
	if err != nil {
		return Snapshot{}, err
	}

	snapshot := Snapshot{config: normalizedCfg}
	if normalizedCfg.IgnoreMarker == "" || len(markerDirs) == 0 {
		return snapshot, nil
	}

	normalizedMarkers := make([]string, 0, len(markerDirs))
	for _, raw := range markerDirs {
		normalizedMarkers = append(normalizedMarkers, normalizeRelativePath(raw))
	}

	snapshot.markerDirs = collapseAncestorPaths(normalizedMarkers)

	return snapshot, nil
}

// Config returns a defensive copy of the snapshot config.
func (s Snapshot) Config() Config {
	return Config{
		SyncPaths:    append([]string(nil), s.config.SyncPaths...),
		IgnoreMarker: s.config.IgnoreMarker,
	}
}

// SyncPaths returns the normalized configured scope paths.
func (s Snapshot) SyncPaths() []string {
	return append([]string(nil), s.config.SyncPaths...)
}

// MarkerDirs returns the normalized active marker directories.
func (s Snapshot) MarkerDirs() []string {
	return append([]string(nil), s.markerDirs...)
}

// IgnoreMarker returns the normalized marker filename.
func (s Snapshot) IgnoreMarker() string {
	return s.config.IgnoreMarker
}

// HasPathRules reports whether sync_paths restrict the observed scope.
func (s Snapshot) HasPathRules() bool {
	return len(s.config.SyncPaths) > 0
}

// IsMarkerFile reports whether relPath is the configured marker file itself.
func (s Snapshot) IsMarkerFile(relPath string) bool {
	if s.config.IgnoreMarker == "" {
		return false
	}

	path := normalizeRelativePath(relPath)

	return norm.NFC.String(slashpath.Base(path)) == s.config.IgnoreMarker
}

// HasMarkerDir reports whether relPath is itself a directory currently gated
// by an ignore marker.
func (s Snapshot) HasMarkerDir(relPath string) bool {
	path := normalizeRelativePath(relPath)

	for _, markerDir := range s.markerDirs {
		if samePathFold(markerDir, path) {
			return true
		}
	}

	return false
}

// AllowsPath reports whether the given relative path is inside the effective
// sync scope. Marker files themselves are never synced.
func (s Snapshot) AllowsPath(relPath string) bool {
	return s.ExclusionReason(relPath) == ExclusionNone
}

// ExclusionReason reports why relPath is currently outside the effective
// sync scope. The zero value means the path is in scope.
func (s Snapshot) ExclusionReason(relPath string) ExclusionReason {
	path := normalizeRelativePath(relPath)

	if s.IsMarkerFile(path) {
		return ExclusionMarkerScope
	}

	if s.isUnderMarker(path) {
		return ExclusionMarkerScope
	}

	if !s.allowedBySyncPaths(path) {
		return ExclusionPathScope
	}

	return ExclusionNone
}

// ShouldTraverseDir reports whether a directory should stay observable for
// local walking/watch setup. Marker-bearing directories themselves stay
// observable so marker deletion is visible; their descendants do not.
func (s Snapshot) ShouldTraverseDir(relPath string) bool {
	path := normalizeRelativePath(relPath)

	if s.HasMarkerDir(path) {
		return true
	}

	if s.isUnderMarker(path) {
		return false
	}

	return s.allowedBySyncPaths(path)
}

func (s Snapshot) allowedBySyncPaths(path string) bool {
	if len(s.config.SyncPaths) == 0 {
		return true
	}

	if path == "" {
		return true
	}

	for _, scopePath := range s.config.SyncPaths {
		if pathsOverlapFold(path, scopePath) {
			return true
		}
	}

	return false
}

func (s Snapshot) isUnderMarker(path string) bool {
	for _, markerDir := range s.markerDirs {
		if samePathFold(markerDir, path) || isAncestorFold(markerDir, path) {
			return true
		}
	}

	return false
}

// DiffSnapshots compares two snapshots and reports entered/exited scope roots.
func DiffSnapshots(oldSnapshot, newSnapshot Snapshot) Diff {
	entered := make(map[string]struct{})
	exited := make(map[string]struct{})

	oldFull := len(oldSnapshot.config.SyncPaths) == 0
	newFull := len(newSnapshot.config.SyncPaths) == 0

	switch {
	case !oldFull && newFull:
		entered[""] = struct{}{}
	case oldFull && !newFull:
		exited[""] = struct{}{}
	}

	for _, path := range newSnapshot.config.SyncPaths {
		if !oldSnapshot.coversConfiguredPath(path) {
			entered[path] = struct{}{}
		}
	}

	for _, path := range oldSnapshot.config.SyncPaths {
		if !newSnapshot.coversConfiguredPath(path) {
			exited[path] = struct{}{}
		}
	}

	oldMarkers := stringSet(oldSnapshot.markerDirs)
	newMarkers := stringSet(newSnapshot.markerDirs)

	for _, dir := range oldSnapshot.markerDirs {
		if _, ok := newMarkers[dir]; !ok {
			entered[dir] = struct{}{}
		}
	}

	for _, dir := range newSnapshot.markerDirs {
		if _, ok := oldMarkers[dir]; !ok {
			exited[dir] = struct{}{}
		}
	}

	return Diff{
		EnteredPaths: collapseAncestorPaths(setKeys(entered)),
		ExitedPaths:  collapseAncestorPaths(setKeys(exited)),
	}
}

func (d Diff) HasChanges() bool {
	return len(d.EnteredPaths) > 0 || len(d.ExitedPaths) > 0
}

func (d Diff) HasEntered() bool {
	return len(d.EnteredPaths) > 0
}

func (s Snapshot) coversConfiguredPath(path string) bool {
	if len(s.config.SyncPaths) == 0 {
		return true
	}

	for _, scopePath := range s.config.SyncPaths {
		if scopePath == "" || samePathFold(scopePath, path) || isAncestorFold(scopePath, path) {
			return true
		}
	}

	return false
}

// Persisted returns the durable JSON representation of the snapshot.
func (s Snapshot) Persisted() PersistedSnapshot {
	return PersistedSnapshot{
		Version:      persistedVersion,
		SyncPaths:    s.SyncPaths(),
		IgnoreMarker: s.config.IgnoreMarker,
		MarkerDirs:   s.MarkerDirs(),
	}
}

// MarshalSnapshot encodes a snapshot for storage in sync_metadata.
func MarshalSnapshot(snapshot Snapshot) (string, error) {
	data, err := json.Marshal(snapshot.Persisted())
	if err != nil {
		return "", fmt.Errorf("marshal persisted snapshot: %w", err)
	}

	return string(data), nil
}

// UnmarshalSnapshot decodes a snapshot previously written by MarshalSnapshot.
func UnmarshalSnapshot(raw string) (Snapshot, error) {
	if strings.TrimSpace(raw) == "" {
		return NewSnapshot(Config{}, nil)
	}

	var persisted PersistedSnapshot
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal persisted snapshot: %w", err)
	}

	if persisted.Version != 0 && persisted.Version != persistedVersion {
		return Snapshot{}, fmt.Errorf("unsupported persisted snapshot version %d", persisted.Version)
	}

	syncPaths := make([]string, 0, len(persisted.SyncPaths))
	for _, path := range persisted.SyncPaths {
		if path == "" {
			syncPaths = append(syncPaths, "/")
			continue
		}

		syncPaths = append(syncPaths, "/"+path)
	}

	return NewSnapshot(Config{
		SyncPaths:    syncPaths,
		IgnoreMarker: persisted.IgnoreMarker,
	}, persisted.MarkerDirs)
}

func normalizeConfiguredPath(raw string) (string, error) {
	value := norm.NFC.String(strings.ReplaceAll(raw, "\\", "/"))
	if value == "" {
		return "", fmt.Errorf("normalize sync path %q: path must not be empty", raw)
	}

	if !strings.HasPrefix(value, "/") {
		cleaned := normalizeRelativePath(value)
		if cleaned == "" {
			return "", nil
		}

		return cleaned, nil
	}

	cleaned := slashpath.Clean(value)
	if cleaned == "/" || cleaned == "." {
		return "", nil
	}

	return strings.TrimPrefix(cleaned, "/"), nil
}

func normalizeRelativePath(raw string) string {
	value := norm.NFC.String(strings.ReplaceAll(raw, "\\", "/"))
	if value == "" || value == "." || value == "/" {
		return ""
	}

	cleaned := slashpath.Clean("/" + strings.TrimPrefix(value, "/"))
	if cleaned == "/" || cleaned == "." {
		return ""
	}

	return strings.TrimPrefix(cleaned, "/")
}

func collapseAncestorPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	sorted := append([]string(nil), paths...)
	sort.Slice(sorted, func(i, j int) bool {
		li := strings.ToLower(sorted[i])
		lj := strings.ToLower(sorted[j])
		if li == lj {
			if len(sorted[i]) == len(sorted[j]) {
				return sorted[i] < sorted[j]
			}

			return len(sorted[i]) < len(sorted[j])
		}

		return li < lj
	})

	collapsed := make([]string, 0, len(sorted))
	for _, path := range sorted {
		if len(collapsed) > 0 {
			last := collapsed[len(collapsed)-1]
			if samePathFold(last, path) || isAncestorFold(last, path) {
				continue
			}
		}

		collapsed = append(collapsed, path)
	}

	return collapsed
}

func pathsOverlapFold(a, b string) bool {
	if a == "" || b == "" {
		return true
	}

	return samePathFold(a, b) || isAncestorFold(a, b) || isAncestorFold(b, a)
}

func samePathFold(a, b string) bool {
	return strings.EqualFold(a, b)
}

func isAncestorFold(ancestor, descendant string) bool {
	if ancestor == "" {
		return descendant != ""
	}

	ancestorLower := strings.ToLower(ancestor)
	descendantLower := strings.ToLower(descendant)

	return strings.HasPrefix(descendantLower, ancestorLower+"/")
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}

	return set
}

func setKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}

	return keys
}
