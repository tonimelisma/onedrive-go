package syncfixture

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func TestRetryWorkKey(path string, oldPath string, actionType syncdomain.ActionType) syncdomain.RetryWorkKey {
	return syncdomain.RetryWorkKey{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func TestRetryWorkFailure(path string, oldPath string, actionType syncdomain.ActionType) *syncdomain.RetryWorkFailure {
	return &syncdomain.RetryWorkFailure{
		Work: TestRetryWorkKey(path, oldPath, actionType),
	}
}

func TestRetryWorkRow(path string, oldPath string, actionType syncdomain.ActionType) syncdomain.RetryWorkRow {
	return syncdomain.RetryWorkRow{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}
