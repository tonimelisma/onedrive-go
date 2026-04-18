package sync

// retryWorkKey constructs the exact semantic identity for one retryable unit
// of work. All retry-state persistence and runtime reconciliation flows must
// derive identity through this helper family so OldPath-aware work stays exact.
func retryWorkKey(path string, oldPath string, actionType ActionType) RetryWorkKey {
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

func retryWorkKeyForCompletion(completion *ActionCompletion) RetryWorkKey {
	if completion == nil {
		return RetryWorkKey{}
	}

	return retryWorkKey(completion.Path, completion.OldPath, completion.ActionType)
}

func retryWorkKeyForRetryState(row *RetryStateRow) RetryWorkKey {
	if row == nil {
		return RetryWorkKey{}
	}

	return retryWorkKey(row.Path, row.OldPath, row.ActionType)
}
