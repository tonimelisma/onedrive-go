package sync

// SafetyConfig reserves planner-time safety policy inputs. Batch delete
// protection has been removed; per-item executor-time safety remains elsewhere.
type SafetyConfig struct{}

func DefaultSafetyConfig() *SafetyConfig {
	return &SafetyConfig{}
}
