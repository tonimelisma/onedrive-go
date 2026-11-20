package sync

import (
	"testing"
)

// plannerSeedVars are the planner model's variables.
func plannerSeedVars() seedVars {
	return seedVars{single: plannerModelSeedEnv, count: plannerModelSeedsEnv, start: plannerModelSeedStartEnv}
}

// plannerModelSeeds is the planner model's seeds: the ones the environment
// names, else the first plannerModelDefaultSeeds (one under -race).
func plannerModelSeeds(t *testing.T) []uint64 {
	t.Helper()

	count := plannerModelDefaultSeeds
	if raceDetectorEnabled {
		// The planner model is single-goroutine apart from the store; one
		// seed covers what the race detector can find (see modelRaceSeeds).
		count = 1
	}

	return seedsFromEnv(t, plannerSeedVars(), count)
}

const (
	plannerModelDefaultSeeds = 8
	plannerModelSeedEnv      = "ONEDRIVE_GO_PLANNER_SEED"
	plannerModelSeedsEnv     = "ONEDRIVE_GO_PLANNER_SEEDS"
	plannerModelSeedStartEnv = "ONEDRIVE_GO_PLANNER_SEED_START"
)
