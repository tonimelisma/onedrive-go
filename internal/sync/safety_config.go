package sync

// DefaultDeleteSafetyThreshold is the default absolute delete count threshold.
const DefaultDeleteSafetyThreshold = 1000

// SafetyConfig controls delete safety protection thresholds.
// Single absolute count threshold — no percentages, no per-folder checks.
type SafetyConfig struct {
	DeleteSafetyThreshold int // max number of delete actions before triggering (0 = disabled)
}

func DefaultSafetyConfig() *SafetyConfig {
	return &SafetyConfig{
		DeleteSafetyThreshold: DefaultDeleteSafetyThreshold,
	}
}
