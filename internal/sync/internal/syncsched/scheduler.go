package syncsched

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Scheduler is one run's scheduling state, behind one API: the dependency
// graph of the installed plan, which of its actions are queued and running,
// the plan's slow start, the work held for a retry or a block scope, the
// active block scopes and the windows that detect new ones.
//
// It has no lock: only the goroutine that runs the engine's RunOnce or
// RunWatch calls it. Workers report outcomes on the completion channel and
// never touch it.
type Scheduler struct {
	logger *slog.Logger
	// windows is scope detection's sliding windows. They outlive a plan: a
	// watch keeps them across replans, so a failure streak that started
	// before one is still counted after it.
	windows *ScopeState
	now     func() time.Time

	graph        *DepGraph
	nextActionID int64
	queued       map[int64]struct{}
	running      map[int64]struct{}
	runningCount int

	// The slow start of the current plan (R-6.8.25, cold_start.go): one
	// ramp per throttle target, the worker pool size they double toward
	// (zero until a plan is installed, which disables gating), and whether a
	// service-wide scope took every target over.
	coldStart                  map[string]*coldStartRamp
	coldStartCeiling           int
	coldStartServiceHandedOver bool

	// Held work (held.go): the actions held for a retry deadline or a
	// blocked scope, by retry-work key and by scope, in held order.
	held          map[syncdomain.RetryWorkKey]*Held
	heldByScope   map[syncdomain.ScopeKey][]syncdomain.RetryWorkKey
	nextHeldOrder uint64

	// active is the run's active block scopes (scopes.go).
	active []syncdomain.ActiveScope

	// ownerCheck, when set, runs at the top of every scope accessor and
	// every graph method; nil in production. Tests install a check that
	// fails when the scheduler is used from a goroutine other than the
	// run's.
	ownerCheck func()
}

// New is a scheduler with no plan installed: an empty graph, nothing queued,
// running or held, no active scope and no slow start. now is the run's clock,
// which scope detection's windows read.
func New(now func() time.Time, logger *slog.Logger, ownerCheck func()) *Scheduler {
	s := &Scheduler{
		logger:     logger,
		now:        now,
		ownerCheck: ownerCheck,
	}
	s.resetPlanState()
	s.ResetGraph()

	return s
}

func (s *Scheduler) resetPlanState() {
	s.queued = make(map[int64]struct{})
	s.running = make(map[int64]struct{})
	s.runningCount = 0
	s.held = make(map[syncdomain.RetryWorkKey]*Held)
	s.heldByScope = make(map[syncdomain.ScopeKey][]syncdomain.RetryWorkKey)
	s.nextHeldOrder = 0
}

// Install starts a new plan: every per-plan structure starts over (the
// queued and running sets, the held work, the slow start with poolSize as
// the ceiling its allowances double toward) and the active scopes become
// scopes. The dependency graph is not replaced here (NewGraph), and the
// scope-detection windows and the action ID counter outlive the plan.
func (s *Scheduler) Install(poolSize int, scopes []syncdomain.ActiveScope) {
	s.resetPlanState()
	s.ResetColdStart(poolSize)
	s.ReplaceScopes(scopes)
	if s.windows == nil {
		s.windows = NewScopeState(s.now, s.logger)
	}
}

// NewGraph replaces the dependency graph with an empty one that carries the
// scheduler's owner check; the plan's actions are registered on it next.
func (s *Scheduler) NewGraph() {
	s.graph = NewDepGraph(s.logger)
	s.graph.SetOwnerCheck(s.ownerCheck)
}

// ResetGraph replaces the graph with an empty one and numbers the next
// plan's actions from zero: a watch's start.
func (s *Scheduler) ResetGraph() {
	s.NewGraph()
	s.nextActionID = 0
}

// ResetWindows starts scope detection over with fresh windows.
func (s *Scheduler) ResetWindows() {
	s.windows = NewScopeState(s.now, s.logger)
}

// SetOwnerCheck installs the check every scope accessor and graph method
// runs first; nil removes it.
func (s *Scheduler) SetOwnerCheck(check func()) {
	s.ownerCheck = check
	if s.graph != nil {
		s.graph.SetOwnerCheck(check)
	}
}

func (s *Scheduler) checkOwner() {
	if s.ownerCheck != nil {
		s.ownerCheck()
	}
}

// AllocateIDs reserves count consecutive action IDs for a plan.
func (s *Scheduler) AllocateIDs(count int) []int64 {
	ids := make([]int64, count)
	base := s.nextActionID
	s.nextActionID += int64(count)
	for i := range count {
		ids[i] = base + int64(i)
	}

	return ids
}

// Register adds an action to the graph; WireDeps wires its dependencies once
// every action of the plan is registered, so they may come in any order.
func (s *Scheduler) Register(action *syncdomain.Action, id int64) {
	s.graph.Register(action, id)
}

// WireDeps wires a registered action's dependencies and returns it when it
// is ready at once.
func (s *Scheduler) WireDeps(id int64, depIDs []int64) *syncdomain.TrackedAction {
	return s.graph.WireDeps(id, depIDs)
}

// Tracked is the graph's action with id, if the graph holds it.
func (s *Scheduler) Tracked(id int64) (*syncdomain.TrackedAction, bool) {
	if s.graph == nil {
		return nil, false
	}

	return s.graph.Get(id)
}

// TrackedForCompletion is the graph's action a completion reports on.
func (s *Scheduler) TrackedForCompletion(r *syncdomain.ActionCompletion) *syncdomain.TrackedAction {
	if r == nil {
		return nil
	}
	ta, ok := s.Tracked(r.ActionID)
	if !ok {
		return nil
	}

	return ta
}

// Complete resolves action id in the graph and returns the dependents it
// made ready; ok is false for an id the graph does not hold.
func (s *Scheduler) Complete(id int64) ([]*syncdomain.TrackedAction, bool) {
	if s.graph == nil {
		return nil, false
	}

	return s.graph.Complete(id)
}

// InFlight is how many of the graph's actions are not yet resolved.
func (s *Scheduler) InFlight() int {
	if s.graph == nil {
		return 0
	}

	return s.graph.InFlightCount()
}

// Queued records that ta was admitted and waits for a worker.
func (s *Scheduler) Queued(ta *syncdomain.TrackedAction) {
	if ta == nil {
		return
	}
	s.queued[ta.ID] = struct{}{}
}

// Running records that ta went to a worker.
func (s *Scheduler) Running(ta *syncdomain.TrackedAction) {
	if ta == nil {
		return
	}
	delete(s.queued, ta.ID)
	ta.AdmitAt = time.Time{}
	if _, ok := s.running[ta.ID]; ok {
		return
	}
	s.running[ta.ID] = struct{}{}
	s.runningCount++
	if target := ColdStartTarget(ta); target != "" && s.coldStartCeiling > 0 {
		s.coldStartRampFor(target).inFlight++
	}
}

// Finished takes ta out of the queued and running sets and reports whether a
// worker had it: only a worker-run action counts toward the slow start (its
// success is a request the drive accepted), and its in-flight slot is given
// back here whatever became of it.
func (s *Scheduler) Finished(ta *syncdomain.TrackedAction) bool {
	if ta == nil {
		return false
	}
	_, wasRunning := s.running[ta.ID]
	if wasRunning {
		delete(s.running, ta.ID)
		if s.runningCount > 0 {
			s.runningCount--
		}
		if ramp, ok := s.coldStart[ColdStartTarget(ta)]; ok && ramp.inFlight > 0 {
			ramp.inFlight--
		}
	}
	delete(s.queued, ta.ID)

	return wasRunning
}

// RunningCount is how many actions are with a worker.
func (s *Scheduler) RunningCount() int {
	return s.runningCount
}

// QueuedCount is how many admitted actions wait for a worker.
func (s *Scheduler) QueuedCount() int {
	return len(s.queued)
}

// IsQueued reports whether action id is admitted and waiting for a worker.
func (s *Scheduler) IsQueued(id int64) bool {
	_, ok := s.queued[id]
	return ok
}

// IsRunning reports whether action id is with a worker.
func (s *Scheduler) IsRunning(id int64) bool {
	_, ok := s.running[id]
	return ok
}
