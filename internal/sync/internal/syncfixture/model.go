package syncfixture

import (
	"testing"
)

// PlannerSeedVars are the planner model's variables.
func PlannerSeedVars() SeedVars {
	return SeedVars{Single: PlannerModelSeedEnv, Count: plannerModelSeedsEnv, Start: plannerModelSeedStartEnv}
}

// PlannerModelSeeds is the planner model's seeds: the ones the environment
// names, else the first plannerModelDefaultSeeds (one under -race).
func PlannerModelSeeds(t *testing.T) []uint64 {
	t.Helper()

	count := plannerModelDefaultSeeds
	if RaceDetectorEnabled {
		// The planner model is single-goroutine apart from the store; one
		// seed covers what the race detector can find (see modelRaceSeeds).
		count = 1
	}

	return SeedsFromEnv(t, PlannerSeedVars(), count)
}

const (
	plannerModelDefaultSeeds = 8
	PlannerModelSeedEnv      = "ONEDRIVE_GO_PLANNER_SEED"
	plannerModelSeedsEnv     = "ONEDRIVE_GO_PLANNER_SEEDS"
	plannerModelSeedStartEnv = "ONEDRIVE_GO_PLANNER_SEED_START"
)
