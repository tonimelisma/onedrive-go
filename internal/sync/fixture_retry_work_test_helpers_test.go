package sync

func testRetryWorkKey(path string, oldPath string, actionType ActionType) RetryWorkKey {
	return RetryWorkKey{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func testRetryWorkFailure(path string, oldPath string, actionType ActionType) *RetryWorkFailure {
	return &RetryWorkFailure{
		Work: testRetryWorkKey(path, oldPath, actionType),
	}
}

func testRetryWorkRow(path string, oldPath string, actionType ActionType) RetryWorkRow {
	return RetryWorkRow{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}
