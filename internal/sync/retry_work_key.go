package sync

// RetryWorkKey identifies semantic work that may be retried across replans.
// It intentionally stays smaller than a runtime action because retry_work
// persists only delayed obligations, not the executable action set.
//
// It is domain vocabulary rather than a persistence row: the runtime keys held
// work by it in memory, and it was declared beside the *Row types only because
// the store is what writes it down.
type RetryWorkKey struct {
	Path       string
	OldPath    string
	ActionType actionType
}

// retryWorkKey constructs the exact semantic identity for one retryable unit
// of work. All retry-state persistence and runtime reconciliation flows must
// derive identity through this helper family so OldPath-aware work stays exact.
func retryWorkKey(path string, oldPath string, actionType actionType) RetryWorkKey {
	return RetryWorkKey{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func retryWorkKeyForAction(action *Action) RetryWorkKey {
	if action == nil {
		return RetryWorkKey{}
	}

	return retryWorkKey(action.Path, action.OldPath, action.Type)
}

func retryWorkKeyForCompletion(completion *actionCompletion) RetryWorkKey {
	if completion == nil {
		return RetryWorkKey{}
	}

	return retryWorkKey(completion.Path, completion.OldPath, completion.ActionType)
}
