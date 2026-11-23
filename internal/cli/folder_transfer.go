package cli

import (
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/humanunit"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// folderTransferReport turns the events of a folder get or put into what the
// user sees: a running count on the progress line (R-6.6.29), the failures
// bulkTransferError names, and under --json the file entries of the one
// document of the whole transfer (R-6.6.33), which the command keeps and
// prints. driveops calls add from one goroutine, so nothing in it needs a
// lock.
type folderTransferReport struct {
	cc *CLIContext
	// verb starts the progress and summary lines ("Downloaded"), and noun
	// ends the failure error ("download").
	verb, noun string
	// keepFile adds a transferred file to the command's --json document. It
	// is called under --json only: a text run needs the count alone.
	keepFile  func(file *driveops.TreeEvent)
	failures  []string
	filesDone int
}

// add takes one event.
func (r *folderTransferReport) add(event driveops.TreeEvent) {
	switch event.Kind {
	case driveops.TreeFileDone:
		r.filesDone++
		if r.cc.Flags.JSON {
			r.keepFile(&event)
		}
		r.cc.Progressf("%s %s", r.verb, countOf(r.filesDone, "file"))
	case driveops.TreeFileFailed, driveops.TreeFolderFailed:
		r.failures = append(r.failures, failureLine(&event))
	case driveops.TreeFolderCreated:
	}
}

// failureLine is how a folder transfer names one failed file or folder: the
// local path the walk found it under, then the cause with every local path
// left out, so the path is named once whatever the cause held.
func failureLine(event *driveops.TreeEvent) string {
	return event.LocalPath + ": " + localpath.WithoutPath(event.Err)
}

// finish prints the result once the transfer has returned: under --json the
// document printJSON prints, given the folders created and the failures, and
// otherwise the summary line, then every failure. treeErr, the transfer's own
// error after it reported (a canceled run, or a panic while it reported),
// outranks the failure count as the command's error.
func (r *folderTransferReport) finish(
	summary driveops.TreeSummary, treeErr error,
	printJSON func(foldersCreated int, failures []string) error,
) error {
	var failed error
	if r.cc.Flags.JSON {
		if err := printJSON(summary.Folders, r.failures); err != nil {
			return err
		}
		failed = bulkTransferError(nil, r.noun, r.failures)
	} else {
		r.cc.Statusf("%s %s, %s (%s)\n", r.verb,
			countOf(summary.Files, "file"), countOf(summary.Folders, "folder"), humanunit.Size(summary.Bytes))
		failed = bulkTransferError(r.cc, r.noun, r.failures)
	}

	if treeErr != nil {
		return treeErr
	}

	return failed
}
