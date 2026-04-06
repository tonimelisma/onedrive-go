package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	slashpath "path"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type primaryObservationMode string

const (
	primaryObservationDelta     primaryObservationMode = "delta"
	primaryObservationEnumerate primaryObservationMode = "enumerate"
)

type deferredDeltaToken struct {
	driveID    string
	scopeID    string
	scopeDrive string
	token      string
}

type remoteFetchResult struct {
	events       []synctypes.ChangeEvent
	deferred     []deferredDeltaToken
	fullPrefixes []string
}

type primaryObservationScope struct {
	folderID  string
	localPath string
	mode      primaryObservationMode
}

func (e *Engine) usesPrimaryPathScopes() bool {
	cfg, err := syncscope.NormalizeConfig(e.syncScopeConfig)
	if err != nil {
		return false
	}

	return len(cfg.SyncPaths) > 0
}

func (e *Engine) primaryObservationMode() primaryObservationMode {
	if e.folderDelta != nil && (e.hasScopedRoot() || e.driveType == driveid.DriveTypePersonal) {
		return primaryObservationDelta
	}

	return primaryObservationEnumerate
}

func (e *Engine) resolvePrimaryObservationScopes(ctx context.Context) ([]primaryObservationScope, bool, error) {
	cfg, err := syncscope.NormalizeConfig(e.syncScopeConfig)
	if err != nil {
		return nil, false, fmt.Errorf("normalize sync scope config: %w", err)
	}
	if len(cfg.SyncPaths) == 0 {
		return nil, false, nil
	}

	byPath := make(map[string]primaryObservationScope, len(cfg.SyncPaths))
	for _, configuredPath := range cfg.SyncPaths {
		scope, err := e.resolvePrimaryObservationScope(ctx, configuredPath)
		if err != nil {
			return nil, false, fmt.Errorf("resolve configured sync path %q: %w", configuredPath, err)
		}

		if scope.localPath == "" {
			return nil, true, nil
		}

		byPath[scope.localPath] = scope
	}

	collapsedPaths := syncscope.CollapseRelativePaths(mapKeys(byPath))
	scopes := make([]primaryObservationScope, 0, len(collapsedPaths))
	for _, localPath := range collapsedPaths {
		scopes = append(scopes, byPath[localPath])
	}

	return scopes, false, nil
}

func (e *Engine) resolvePrimaryObservationScope(ctx context.Context, configuredPath string) (primaryObservationScope, error) {
	if configuredPath == "" {
		return primaryObservationScope{}, nil
	}

	if e.hasScopedRoot() {
		return e.resolveScopedRootObservationScope(ctx, configuredPath)
	}

	item, err := e.itemsClient.GetItemByPath(ctx, e.driveID, configuredPath)
	if err == nil {
		if item.IsFolder {
			return primaryObservationScope{
				folderID:  item.ID,
				localPath: configuredPath,
				mode:      e.primaryObservationMode(),
			}, nil
		}

		return e.resolveNearestExistingPrimaryAncestor(ctx, configuredPath)
	}
	if !errors.Is(err, graph.ErrNotFound) {
		return primaryObservationScope{}, fmt.Errorf("get item by path %q: %w", configuredPath, err)
	}

	return e.resolveNearestExistingPrimaryAncestor(ctx, configuredPath)
}

func (e *Engine) resolveNearestExistingPrimaryAncestor(ctx context.Context, configuredPath string) (primaryObservationScope, error) {
	for candidate := parentObservedPath(configuredPath); candidate != ""; candidate = parentObservedPath(candidate) {
		item, err := e.itemsClient.GetItemByPath(ctx, e.driveID, candidate)
		if err == nil {
			if !item.IsFolder {
				continue
			}

			return primaryObservationScope{
				folderID:  item.ID,
				localPath: candidate,
				mode:      e.primaryObservationMode(),
			}, nil
		}
		if !errors.Is(err, graph.ErrNotFound) {
			return primaryObservationScope{}, fmt.Errorf("get ancestor by path %q: %w", candidate, err)
		}
	}

	return primaryObservationScope{}, nil
}

func (e *Engine) resolveScopedRootObservationScope(ctx context.Context, configuredPath string) (primaryObservationScope, error) {
	currentID := e.rootItemID
	currentPath := ""
	lastFolderID := currentID
	lastFolderPath := ""

	for _, segment := range strings.Split(configuredPath, "/") {
		children, err := e.itemsClient.ListChildren(ctx, e.driveID, currentID)
		if err != nil {
			return primaryObservationScope{}, fmt.Errorf("list scoped children for %q: %w", currentPath, err)
		}

		child, ok := matchObservedChild(children, segment)
		if !ok {
			return primaryObservationScope{
				folderID:  lastFolderID,
				localPath: lastFolderPath,
				mode:      e.primaryObservationMode(),
			}, nil
		}

		currentPath = joinScopePath(currentPath, segment)
		if !child.IsFolder {
			return primaryObservationScope{
				folderID:  lastFolderID,
				localPath: lastFolderPath,
				mode:      e.primaryObservationMode(),
			}, nil
		}

		currentID = child.ID
		lastFolderID = child.ID
		lastFolderPath = currentPath
	}

	return primaryObservationScope{
		folderID:  lastFolderID,
		localPath: lastFolderPath,
		mode:      e.primaryObservationMode(),
	}, nil
}

func (flow *engineFlow) observePlannedPrimaryScopes(
	ctx context.Context,
	bl *synctypes.Baseline,
	scopes []primaryObservationScope,
	fullReconcile bool,
) (remoteFetchResult, error) {
	result := remoteFetchResult{
		events:       make([]synctypes.ChangeEvent, 0),
		deferred:     make([]deferredDeltaToken, 0),
		fullPrefixes: make([]string, 0),
	}

	for i := range scopes {
		scopeResult, err := flow.observeSinglePrimaryScope(ctx, bl, scopes[i], fullReconcile)
		if err != nil {
			return remoteFetchResult{}, err
		}

		result.events = append(result.events, scopeResult.events...)
		result.deferred = append(result.deferred, scopeResult.deferred...)
		result.fullPrefixes = append(result.fullPrefixes, scopeResult.fullPrefixes...)
	}

	return result, nil
}

func (flow *engineFlow) observeSinglePrimaryScope(
	ctx context.Context,
	bl *synctypes.Baseline,
	scope primaryObservationScope,
	fullReconcile bool,
) (remoteFetchResult, error) {
	switch scope.mode {
	case primaryObservationDelta:
		return flow.observeSinglePrimaryScopeDelta(ctx, bl, scope, fullReconcile)
	case primaryObservationEnumerate:
		return flow.observeSinglePrimaryScopeEnumerate(ctx, bl, scope)
	default:
		return remoteFetchResult{}, fmt.Errorf("unknown primary observation mode %q", scope.mode)
	}
}

func (flow *engineFlow) observeSinglePrimaryScopeDelta(
	ctx context.Context,
	bl *synctypes.Baseline,
	scope primaryObservationScope,
	fullReconcile bool,
) (remoteFetchResult, error) {
	eng := flow.engine

	savedToken := ""
	if !fullReconcile {
		token, err := eng.baseline.GetDeltaToken(ctx, eng.driveID.String(), scope.folderID)
		if err != nil {
			return remoteFetchResult{}, fmt.Errorf("get scoped delta token for %q: %w", scope.localPath, err)
		}

		savedToken = token
	}

	items, newToken, err := eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, scope.folderID, savedToken)
	fullScope := fullReconcile || savedToken == ""
	if err != nil && errors.Is(err, graph.ErrGone) && !fullScope {
		items, newToken, err = eng.folderDelta.DeltaFolderAll(ctx, eng.driveID, scope.folderID, "")
		fullScope = true
	}
	if err != nil {
		return remoteFetchResult{}, fmt.Errorf("observe scoped delta %q: %w", scope.localPath, err)
	}

	events := convertPrimaryScopeItems(bl, eng.driveID, eng.logger, scope, items)
	if fullScope {
		events = append(events, observePrimaryScopeOrphans(events, bl, eng.driveID, scope.localPath)...)
	}

	result := remoteFetchResult{
		events:       events,
		fullPrefixes: nil,
	}
	if fullScope {
		result.fullPrefixes = append(result.fullPrefixes, scope.localPath)
	}
	if newToken != "" && (fullScope || len(events) > 0) {
		result.deferred = append(result.deferred, deferredDeltaToken{
			driveID:    eng.driveID.String(),
			scopeID:    scope.folderID,
			scopeDrive: eng.driveID.String(),
			token:      newToken,
		})
	}

	return result, nil
}

func (flow *engineFlow) observeSinglePrimaryScopeEnumerate(
	ctx context.Context,
	bl *synctypes.Baseline,
	scope primaryObservationScope,
) (remoteFetchResult, error) {
	eng := flow.engine
	if eng.recursiveLister == nil {
		return remoteFetchResult{}, fmt.Errorf("recursive lister not available for %q", scope.localPath)
	}

	items, err := eng.recursiveLister.ListChildrenRecursive(ctx, eng.driveID, scope.folderID)
	if err != nil {
		return remoteFetchResult{}, fmt.Errorf("observe scoped enumeration %q: %w", scope.localPath, err)
	}

	events := convertPrimaryScopeItems(bl, eng.driveID, eng.logger, scope, items)
	events = append(events, observePrimaryScopeOrphans(events, bl, eng.driveID, scope.localPath)...)

	return remoteFetchResult{
		events:       events,
		fullPrefixes: []string{scope.localPath},
	}, nil
}

func convertPrimaryScopeItems(
	bl *synctypes.Baseline,
	driveID driveid.ID,
	logger *slog.Logger,
	scope primaryObservationScope,
	items []graph.Item,
) []synctypes.ChangeEvent {
	converter := syncobserve.NewPrimaryConverter(bl, driveID, logger, nil)
	converter.PathPrefix = scope.localPath
	converter.ScopeRootID = scope.folderID

	return converter.ConvertItems(items)
}

func observePrimaryScopeOrphans(
	events []synctypes.ChangeEvent,
	bl *synctypes.Baseline,
	driveID driveid.ID,
	localPath string,
) []synctypes.ChangeEvent {
	seen := make(map[driveid.ItemKey]struct{}, len(events))
	for i := range events {
		if events[i].IsDeleted {
			continue
		}

		seen[driveid.NewItemKey(events[i].DriveID, events[i].ItemID)] = struct{}{}
	}

	return bl.FindOrphans(seen, driveID, localPath)
}

func (flow *engineFlow) reconcilePrimaryScopeEntries(
	ctx context.Context,
	bl *synctypes.Baseline,
	enteredPaths []string,
	fullPrefixes []string,
) (remoteFetchResult, error) {
	if len(enteredPaths) == 0 {
		return remoteFetchResult{}, nil
	}

	result := remoteFetchResult{
		events:       make([]synctypes.ChangeEvent, 0),
		deferred:     make([]deferredDeltaToken, 0),
		fullPrefixes: make([]string, 0),
	}
	for _, enteredPath := range enteredPaths {
		if pathCoveredByAny(fullPrefixes, enteredPath) {
			continue
		}

		reconciled, err := flow.reconcileEnteredPrimaryPath(ctx, bl, enteredPath)
		if err != nil {
			return remoteFetchResult{}, err
		}

		result.events = append(result.events, reconciled.events...)
		result.deferred = append(result.deferred, reconciled.deferred...)
		result.fullPrefixes = append(result.fullPrefixes, reconciled.fullPrefixes...)
	}

	return result, nil
}

func (flow *engineFlow) reconcileEnteredPrimaryPath(
	ctx context.Context,
	bl *synctypes.Baseline,
	enteredPath string,
) (remoteFetchResult, error) {
	eng := flow.engine

	if enteredPath == "" {
		if eng.hasScopedRoot() {
			events, token, err := flow.observeScopedRemote(ctx, bl, true)
			if err != nil {
				return remoteFetchResult{}, fmt.Errorf("full scoped re-entry reconciliation: %w", err)
			}

			return remoteFetchResult{
				events:       events,
				deferred:     deferredTokensForPrimary(token, eng, true, len(events)),
				fullPrefixes: []string{""},
			}, nil
		}

		events, token, err := flow.observeRemoteFull(ctx, bl)
		if err != nil {
			return remoteFetchResult{}, fmt.Errorf("full re-entry reconciliation: %w", err)
		}

		return remoteFetchResult{
			events:       events,
			deferred:     deferredTokensForPrimary(token, eng, true, len(events)),
			fullPrefixes: []string{""},
		}, nil
	}

	scope, err := eng.resolvePrimaryObservationScope(ctx, enteredPath)
	if err != nil {
		return remoteFetchResult{}, fmt.Errorf("resolve entered path %q: %w", enteredPath, err)
	}
	if scope.localPath == "" {
		return flow.reconcileEnteredPrimaryPath(ctx, bl, "")
	}
	if scope.localPath == enteredPath {
		scopeResult, scopeErr := flow.observeSinglePrimaryScope(ctx, bl, scope, true)
		if scopeErr != nil {
			return remoteFetchResult{}, scopeErr
		}

		return scopeResult, nil
	}

	item, err := flow.resolveEnteredPrimaryItem(ctx, enteredPath)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return remoteFetchResult{
				events: bl.FindOrphans(nil, eng.driveID, enteredPath),
			}, nil
		}

		return remoteFetchResult{}, fmt.Errorf("resolve entered item %q: %w", enteredPath, err)
	}

	if item.IsFolder {
		scope := primaryObservationScope{
			folderID:  item.ID,
			localPath: enteredPath,
			mode:      eng.primaryObservationMode(),
		}

		scopeResult, scopeErr := flow.observeSinglePrimaryScope(ctx, bl, scope, true)
		if scopeErr != nil {
			return remoteFetchResult{}, scopeErr
		}

		return scopeResult, nil
	}

	parentPath := parentObservedPath(enteredPath)
	converter := syncobserve.NewPrimaryConverter(bl, eng.driveID, eng.logger, nil)
	converter.PathPrefix = parentPath
	converter.ScopeRootID = item.ParentID

	events := converter.ConvertItems([]graph.Item{*item})
	events = append(events, bl.FindOrphans(seenKeysFromEvents(events), eng.driveID, enteredPath)...)

	return remoteFetchResult{events: events}, nil
}

func (flow *engineFlow) resolveEnteredPrimaryItem(ctx context.Context, enteredPath string) (*graph.Item, error) {
	eng := flow.engine
	if !eng.hasScopedRoot() {
		item, err := eng.itemsClient.GetItemByPath(ctx, eng.driveID, enteredPath)
		if err != nil {
			return nil, fmt.Errorf("get item by path %q: %w", enteredPath, err)
		}

		return item, nil
	}

	currentID := eng.rootItemID
	var current *graph.Item
	for _, segment := range strings.Split(enteredPath, "/") {
		children, err := eng.itemsClient.ListChildren(ctx, eng.driveID, currentID)
		if err != nil {
			return nil, fmt.Errorf("list scoped children for %q: %w", enteredPath, err)
		}

		child, ok := matchObservedChild(children, segment)
		if !ok {
			return nil, graph.ErrNotFound
		}

		currentID = child.ID
		current = &child
	}

	if current != nil {
		return current, nil
	}

	return nil, graph.ErrNotFound
}

func seenKeysFromEvents(events []synctypes.ChangeEvent) map[driveid.ItemKey]struct{} {
	seen := make(map[driveid.ItemKey]struct{}, len(events))
	for i := range events {
		if events[i].IsDeleted {
			continue
		}

		seen[driveid.NewItemKey(events[i].DriveID, events[i].ItemID)] = struct{}{}
	}

	return seen
}

func pathCoveredByAny(prefixes []string, candidate string) bool {
	for _, prefix := range prefixes {
		if syncscope.CoversPath(prefix, candidate) {
			return true
		}
	}

	return false
}

func joinScopePath(parent, child string) string {
	if parent == "" {
		return syncscope.NormalizeRelativePath(child)
	}

	return syncscope.NormalizeRelativePath(slashpath.Join(parent, child))
}

func parentObservedPath(relPath string) string {
	normalized := syncscope.NormalizeRelativePath(relPath)
	if normalized == "" {
		return ""
	}

	parent := slashpath.Dir("/" + normalized)
	if parent == "/" || parent == "." {
		return ""
	}

	return strings.TrimPrefix(parent, "/")
}

func matchObservedChild(children []graph.Item, name string) (graph.Item, bool) {
	for i := range children {
		if children[i].Name == name {
			return children[i], true
		}
	}

	for i := range children {
		if strings.EqualFold(children[i].Name, name) {
			return children[i], true
		}
	}

	return graph.Item{}, false
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	return keys
}

func deferredTokensForPrimary(token string, eng *Engine, fullReconcile bool, eventCount int) []deferredDeltaToken {
	if token == "" {
		return nil
	}
	if !fullReconcile && eventCount == 0 {
		return nil
	}

	scopeID := ""
	if eng.hasScopedRoot() {
		scopeID = eng.rootItemID
	}

	return []deferredDeltaToken{{
		driveID:    eng.driveID.String(),
		scopeID:    scopeID,
		scopeDrive: eng.driveID.String(),
		token:      token,
	}}
}
