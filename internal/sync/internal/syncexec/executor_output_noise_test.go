package syncexec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The console prints this reason after the folder path; it used to dump the
// whole blocker list as a Go slice with internal "non-disposable" wording.
//
// Validates: R-6.6.11
func TestFolderNotEmptyError_NamesAFewBlockersAndTheAction(t *testing.T) {
	t.Parallel()

	err := folderNotEmptyError([]string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"})
	assert.Equal(t,
		"folder still contains files that are not on OneDrive (a.txt, b.txt, c.txt and 2 more); move or delete them to finish",
		err.Error())
	assert.Equal(t,
		"folder still contains files that are not on OneDrive (a.txt); move or delete them to finish",
		folderNotEmptyError([]string{"a.txt"}).Error())
}
