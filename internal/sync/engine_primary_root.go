package sync

import "context"

type remoteObservationMode string

const (
	remoteObservationModeDelta     remoteObservationMode = "delta"
	remoteObservationModeEnumerate remoteObservationMode = "enumerate"
)

type remoteObservationBatchSource string

const (
	remoteObservationBatchPrimaryWatch remoteObservationBatchSource = "primary_watch"
	remoteObservationBatchMountRoot    remoteObservationBatchSource = "mount_root_watch"
	remoteObservationBatchFullRefresh  remoteObservationBatchSource = "full_refresh"
)

type remoteObservationBatch struct {
	source                remoteObservationBatchSource
	observationMode       remoteObservationMode
	observed              []ObservedItem
	emitted               []ChangeEvent
	cursorToken           string
	markFullRemoteRefresh bool
	findings              ObservationFindingsBatch
	shortcutTopology      shortcutTopologyBatch
	armFullRefreshTimer   bool
	markFullRefreshIfIdle bool
	applyAck              chan error
}

func (batch *remoteObservationBatch) hasObservationFindings() bool {
	if batch == nil {
		return false
	}

	return len(batch.findings.Issues) > 0 ||
		len(batch.findings.ManagedIssueTypes) > 0
}

func (batch *remoteObservationBatch) deferredProgress() *remoteObservationBatch {
	if batch == nil ||
		(batch.cursorToken == "" &&
			!batch.markFullRemoteRefresh &&
			batch.observationMode != remoteObservationModeEnumerate) {
		return nil
	}

	clone := *batch
	clone.source = ""
	clone.observed = nil
	clone.emitted = nil
	clone.findings = ObservationFindingsBatch{}
	clone.shortcutTopology = shortcutTopologyBatch{}
	clone.armFullRefreshTimer = false
	clone.markFullRefreshIfIdle = false
	clone.applyAck = nil

	return &clone
}

func buildRemoteObservationBatch(
	engine *Engine,
	mode remoteObservationMode,
	events []ChangeEvent,
	token string,
	markFullRemoteRefresh bool,
	findings ObservationFindingsBatch,
) remoteObservationBatch {
	projected := projectRemoteObservations(engine.logger, events)

	return remoteObservationBatch{
		observationMode:       mode,
		observed:              projected.observed,
		emitted:               append([]ChangeEvent(nil), projected.emitted...),
		cursorToken:           primaryObservationCursorToken(token, markFullRemoteRefresh, len(projected.emitted), mode),
		markFullRemoteRefresh: markFullRemoteRefresh,
		findings:              findings,
	}
}

func primaryObservationCursorToken(
	token string,
	markFullRemoteRefresh bool,
	eventCount int,
	mode remoteObservationMode,
) string {
	needsEnumerateClamp := mode == remoteObservationModeEnumerate
	if token == "" && !markFullRemoteRefresh && !needsEnumerateClamp {
		return ""
	}
	if !markFullRemoteRefresh && eventCount == 0 && !needsEnumerateClamp {
		return ""
	}

	return token
}

func buildPrimaryWatchBatch(
	engine *Engine,
	primaryEvents []ChangeEvent,
	newToken string,
) remoteObservationBatch {
	batch := buildRemoteObservationBatch(
		engine,
		remoteObservationModeDelta,
		primaryEvents,
		newToken,
		false,
		newRemoteObservationFindingsBatch(),
	)
	batch.source = remoteObservationBatchPrimaryWatch
	batch.applyAck = make(chan error, 1)

	return batch
}

func (e *Engine) preferredMountRootObservationMode() remoteObservationMode {
	if e.mountRootDeltaSupported() {
		return remoteObservationModeDelta
	}

	return remoteObservationModeEnumerate
}

func (flow *engineFlow) executePrimaryRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteObservationBatch, error) {
	if flow.engine.hasRemoteMountRoot() {
		return flow.executeMountRootObservation(ctx, bl, fullReconcile)
	}

	return flow.executeDriveRootObservation(ctx, bl, fullReconcile)
}

func (flow *engineFlow) executeDriveRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteObservationBatch, error) {
	if fullReconcile {
		events, token, topology, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl)
		if err != nil && isObservationRemoteReadDenied(err) {
			return buildRemoteObservationBatch(
				flow.engine,
				remoteObservationModeDelta,
				nil,
				"",
				false,
				rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
			), nil
		}

		batch := buildRemoteObservationBatch(
			flow.engine,
			remoteObservationModeDelta,
			events,
			token,
			true,
			newRemoteObservationFindingsBatch(),
		)
		batch.shortcutTopology = topology
		return batch, err
	}

	events, token, topology, err := flow.observeRemoteWithShortcutTopology(ctx, bl)
	if err != nil && isObservationRemoteReadDenied(err) {
		return buildRemoteObservationBatch(
			flow.engine,
			remoteObservationModeDelta,
			nil,
			"",
			false,
			rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		remoteObservationModeDelta,
		events,
		token,
		false,
		newRemoteObservationFindingsBatch(),
	)
	batch.shortcutTopology = topology
	return batch, err
}

func (flow *engineFlow) executeMountRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteObservationBatch, error) {
	events, token, mode, err := flow.observeMountRootRemote(ctx, bl, fullReconcile)
	if err != nil && isObservationRemoteReadDenied(err) {
		return buildRemoteObservationBatch(
			flow.engine,
			mode,
			nil,
			"",
			false,
			rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		), nil
	}

	return buildRemoteObservationBatch(
		flow.engine,
		mode,
		events,
		token,
		fullReconcile,
		newRemoteObservationFindingsBatch(),
	), err
}
