package sync

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

const (
	observationDocsPath   = "Docs"
	observationScopedRoot = "scoped-root"
)

type observationSessionPlanGolden struct {
	Primary          *observationPhasePlanGolden `json:"primary,omitempty"`
	Shortcut         *observationPhasePlanGolden `json:"shortcut,omitempty"`
	Reentry          reentryPlanGolden           `json:"reentry"`
	ObservationMode  string                      `json:"observation_mode,omitempty"`
	WebsocketEnabled bool                        `json:"websocket_enabled"`
	HasPrimaryHash   bool                        `json:"has_primary_hash"`
}

type observationPhasePlanGolden struct {
	Driver            string                        `json:"driver"`
	DispatchPolicy    string                        `json:"dispatch_policy"`
	ErrorPolicy       string                        `json:"error_policy"`
	FallbackPolicy    string                        `json:"fallback_policy"`
	TokenCommitPolicy string                        `json:"token_commit_policy"`
	Targets           []observationTargetPlanGolden `json:"targets,omitempty"`
}

type observationTargetPlanGolden struct {
	Kind           string `json:"kind"`
	LocalPath      string `json:"local_path,omitempty"`
	ScopeID        string `json:"scope_id,omitempty"`
	ScopeDrive     string `json:"scope_drive,omitempty"`
	Mode           string `json:"mode,omitempty"`
	ShortcutItemID string `json:"shortcut_item_id,omitempty"`
	RemoteDrive    string `json:"remote_drive,omitempty"`
	RemoteItem     string `json:"remote_item,omitempty"`
}

type reentryPlanGolden struct {
	Kind    string   `json:"kind"`
	Pending bool     `json:"pending"`
	Paths   []string `json:"paths,omitempty"`
}

type observationSessionPlanGoldenCase struct {
	name       string
	goldenPath string
	build      func(t *testing.T) (*engineFlow, *ObservationSessionPlan)
}

func renderObservationSessionPlanGolden(
	t *testing.T,
	flow *engineFlow,
	plan *ObservationSessionPlan,
) []byte {
	t.Helper()

	projection := observationSessionPlanGolden{
		Reentry: reentryPlanGolden{
			Kind:    string(plan.Reentry.Kind),
			Pending: plan.Reentry.Pending,
			Paths:   append([]string(nil), plan.Reentry.Paths...),
		},
		WebsocketEnabled: flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase),
		HasPrimaryHash:   plan.Hash != "",
	}
	if plan.PrimaryPhase.Driver != "" {
		primary := phasePlanGolden(plan.PrimaryPhase)
		projection.Primary = &primary
		projection.ObservationMode = string(flow.scopeObservationMode(plan))
	}
	if plan.ShortcutPhase.Driver != "" {
		shortcut := phasePlanGolden(plan.ShortcutPhase)
		projection.Shortcut = &shortcut
	}

	raw, err := json.MarshalIndent(projection, "", "  ")
	require.NoError(t, err)
	return append(raw, '\n')
}

func phasePlanGolden(phase ObservationPhasePlan) observationPhasePlanGolden {
	projection := observationPhasePlanGolden{
		Driver:            string(phase.Driver),
		DispatchPolicy:    string(phase.DispatchPolicy),
		ErrorPolicy:       string(phase.ErrorPolicy),
		FallbackPolicy:    string(phase.FallbackPolicy),
		TokenCommitPolicy: string(phase.TokenCommitPolicy),
		Targets:           make([]observationTargetPlanGolden, 0, len(phase.Targets)),
	}
	for _, target := range phase.Targets {
		entry := observationTargetPlanGolden{
			Kind:       string(target.kind),
			LocalPath:  target.localPath,
			ScopeID:    target.scopeID,
			ScopeDrive: target.scopeDrive,
			Mode:       string(target.mode),
		}
		if target.shortcut != nil {
			entry.ShortcutItemID = target.shortcut.ItemID
			entry.RemoteDrive = target.shortcut.RemoteDrive
			entry.RemoteItem = target.shortcut.RemoteItem
		}
		projection.Targets = append(projection.Targets, entry)
	}
	return projection
}

func newObservationDocsMock() *engineMockClient {
	return &engineMockClient{
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			if remotePath == observationDocsPath {
				return &graph.Item{ID: "docs-id", Name: observationDocsPath, IsFolder: true}, nil
			}
			return nil, graph.ErrNotFound
		},
	}
}

func buildObservationSessionPlanFullDrive(t *testing.T) (*engineFlow, *ObservationSessionPlan) {
	t.Helper()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	require.NoError(t, err)
	return flow, &plan
}

func buildObservationSessionPlanScopedRoot(t *testing.T) (*engineFlow, *ObservationSessionPlan) {
	t.Helper()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.rootItemID = observationScopedRoot
	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	require.NoError(t, err)
	return flow, &plan
}

func buildObservationSessionPlanScopedTargets(t *testing.T) (*engineFlow, *ObservationSessionPlan) {
	t.Helper()

	eng, _ := newTestEngine(t, newObservationDocsMock())
	eng.driveType = driveid.DriveTypePersonal
	eng.syncScopeConfig = syncscope.Config{SyncPaths: []string{"/" + observationDocsPath}}
	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	require.NoError(t, err)
	return flow, &plan
}

func buildObservationSessionPlanShortcutOnly(t *testing.T) (*engineFlow, *ObservationSessionPlan) {
	t.Helper()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Baseline: emptyBaseline(),
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeOneShot,
		Shortcuts: []Shortcut{{
			ItemID:      "sc-1",
			RemoteDrive: "drv-a",
			RemoteItem:  "item-a",
			LocalPath:   "Shared",
		}},
	})
	require.NoError(t, err)
	return flow, &plan
}

func buildObservationSessionPlanCombined(
	t *testing.T,
	shortcuts []Shortcut,
	suppressed map[string]struct{},
) (*engineFlow, *ObservationSessionPlan) {
	t.Helper()

	eng, _ := newTestEngine(t, newObservationDocsMock())
	eng.driveType = driveid.DriveTypePersonal
	eng.syncScopeConfig = syncscope.Config{SyncPaths: []string{"/" + observationDocsPath}}
	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:                   &session,
		Baseline:                  emptyBaseline(),
		SyncMode:                  SyncBidirectional,
		Purpose:                   observationPlanPurposeWatch,
		Shortcuts:                 shortcuts,
		SuppressedShortcutTargets: suppressed,
	})
	require.NoError(t, err)
	return flow, &plan
}

func observationSessionPlanGoldenCases() []observationSessionPlanGoldenCase {
	return []observationSessionPlanGoldenCase{
		{
			name:       "full drive root delta",
			goldenPath: "observation_session_plan/full_drive_root_delta.golden.json",
			build:      buildObservationSessionPlanFullDrive,
		},
		{
			name:       "scoped root",
			goldenPath: "observation_session_plan/scoped_root.golden.json",
			build:      buildObservationSessionPlanScopedRoot,
		},
		{
			name:       "scoped targets",
			goldenPath: "observation_session_plan/scoped_targets.golden.json",
			build:      buildObservationSessionPlanScopedTargets,
		},
		{
			name:       "shortcut only follow up",
			goldenPath: "observation_session_plan/shortcut_only_follow_up.golden.json",
			build:      buildObservationSessionPlanShortcutOnly,
		},
		{
			name:       "combined primary plus shortcuts",
			goldenPath: "observation_session_plan/combined_primary_plus_shortcuts.golden.json",
			build: func(t *testing.T) (*engineFlow, *ObservationSessionPlan) {
				t.Helper()
				return buildObservationSessionPlanCombined(t, []Shortcut{{
					ItemID:      "sc-1",
					RemoteDrive: "drv-a",
					RemoteItem:  "item-a",
					LocalPath:   "Shared",
				}}, nil)
			},
		},
		{
			name:       "combined with suppressed shortcuts",
			goldenPath: "observation_session_plan/combined_with_suppressed_shortcuts.golden.json",
			build: func(t *testing.T) (*engineFlow, *ObservationSessionPlan) {
				t.Helper()
				return buildObservationSessionPlanCombined(t, []Shortcut{
					{
						ItemID:      "sc-1",
						RemoteDrive: "drv-a",
						RemoteItem:  "item-a",
						LocalPath:   "Shared",
					},
					{
						ItemID:      "sc-2",
						RemoteDrive: "drv-b",
						RemoteItem:  "item-b",
						LocalPath:   "Other",
					},
				}, map[string]struct{}{"drv-b:item-b": {}})
			},
		},
	}
}

// Validates: R-2.4.5, R-3.4.2
func TestObservationSessionPlan_Goldens(t *testing.T) {
	t.Parallel()

	for _, tc := range observationSessionPlanGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flow, plan := tc.build(t)
			assertSyncGoldenFile(t, tc.goldenPath, renderObservationSessionPlanGolden(t, flow, plan))
		})
	}
}
