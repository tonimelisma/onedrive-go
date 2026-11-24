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
	// plannerModelDefaultSeeds is what a plain full-tier run plans: a seed
	// costs about 0.07 s without the race detector (November 2026), so fifty
	// cost a few seconds; the test is exhaustive and never runs under -short.
	plannerModelDefaultSeeds = 50
	PlannerModelSeedEnv      = "ONEDRIVE_GO_PLANNER_SEED"
	plannerModelSeedsEnv     = "ONEDRIVE_GO_PLANNER_SEEDS"
	plannerModelSeedStartEnv = "ONEDRIVE_GO_PLANNER_SEED_START"
)
