package testutil

import (
	"regexp"
	"strconv"
	"strings"
)

// StandingSyncConditions holds what a one-shot sync of a live test account
// leaves unsynced before a suite has created anything: per mount, the number
// of items the summary reports as still not synced. Such a condition lives in
// the account itself (a shared folder whose owner took it away, say), no test
// made it and no test can clear it, yet the sync command rightly exits 1 on
// every pass that meets it.
//
// A suite records them once, from a baseline pass, and Settle then tells a
// later pass that left only those conditions from one that left work of its
// own. The zero value records nothing and settles nothing.
type StandingSyncConditions struct {
	stillOpen    map[string]int
	unattributed int
}

var (
	// standingOnlySummary matches a sync summary line whose only left work is
	// items still not synced: the mount prefix, the duration, the work done if
	// any, and the count. A line that also reports failures or retries has a
	// further "; ..." part and does not match.
	standingOnlySummary = regexp.MustCompile(
		`^((?:.*: )?)Done in ([^:;]+?)(: [^;]+)?; (\d+) still not synced \(see 'onedrive-go status'\)\.$`)

	// leftWorkSummary matches any summary line that reports left work.
	leftWorkSummary = regexp.MustCompile(`\d+ failed|will be retried on the next sync|still not synced`)

	// leftWorkDrivesError matches the command's closing error line for a run
	// over several mounts of which some left work.
	leftWorkDrivesError = regexp.MustCompile(
		`^Error: (\d+) of \d+ selected drives left files unsynced \(see above\)$`)
)

const commandErrorPrefix = "Error: "

// RecordBaseline adds the standing conditions a baseline pass printed on
// stdout. Only a line of a named mount is recorded; a standing line without a
// mount prefix is counted apart, because a run over one mount names none and
// its condition cannot be told from a later test's own.
func (c *StandingSyncConditions) RecordBaseline(stdout string) {
	for _, line := range strings.Split(stdout, "\n") {
		match := standingOnlySummary.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if match == nil {
			continue
		}

		count, err := strconv.Atoi(match[4])
		if err != nil || count == 0 {
			continue
		}

		prefix := match[1]
		if prefix == "" {
			c.unattributed += count
			continue
		}

		if c.stillOpen == nil {
			c.stillOpen = map[string]int{}
		}
		c.stillOpen[prefix] = max(c.stillOpen[prefix], count)
	}
}

// Empty reports whether no mount has a recorded standing condition.
func (c *StandingSyncConditions) Empty() bool {
	return len(c.stillOpen) == 0
}

// Mounts is the number of mounts with a recorded standing condition.
func (c *StandingSyncConditions) Mounts() int {
	return len(c.stillOpen)
}

// Items is the number of items the recorded standing conditions cover.
func (c *StandingSyncConditions) Items() int {
	total := 0
	for _, count := range c.stillOpen {
		total += count
	}

	return total
}

// Unattributed is the number of items baseline passes reported as still not
// synced on a line that names no mount.
func (c *StandingSyncConditions) Unattributed() int {
	return c.unattributed
}

// Settle decides whether a sync pass that exited 1 left nothing but recorded
// standing conditions. It does when every summary line on stdout that reports
// left work reports only items still not synced, for a recorded mount and no
// more of them than recorded, and stderr's one error line, its last line, is
// the count of exactly those mounts. It then returns the output the command
// prints for the same pass without them: each such summary line without its
// left work, and stderr without the error line. Otherwise it returns stdout
// and stderr unchanged and false.
func (c *StandingSyncConditions) Settle(stdout, stderr string) (settledStdout, settledStderr string, settled bool) {
	if c.Empty() {
		return stdout, stderr, false
	}

	outLines := strings.SplitAfter(stdout, "\n")
	standingLines := 0
	for i, line := range outLines {
		text, ending := splitLineEnding(line)
		if !leftWorkSummary.MatchString(text) {
			continue
		}

		match := standingOnlySummary.FindStringSubmatch(text)
		if match == nil {
			return stdout, stderr, false
		}

		count, err := strconv.Atoi(match[4])
		if err != nil || count == 0 || count > c.stillOpen[match[1]] {
			return stdout, stderr, false
		}

		outLines[i] = summaryWithoutLeftWork(match[1], match[2], match[3]) + ending
		standingLines++
	}
	if standingLines == 0 {
		return stdout, stderr, false
	}

	withoutError, ok := removeLeftWorkDrivesError(stderr, standingLines)
	if !ok {
		return stdout, stderr, false
	}

	return strings.Join(outLines, ""), withoutError, true
}

// summaryWithoutLeftWork is the summary line the sync command prints for a
// mount that left nothing: the work it did, or that it is up to date.
func summaryWithoutLeftWork(prefix, duration, work string) string {
	if work == "" {
		return prefix + "Up to date (" + duration + ")."
	}

	return prefix + "Done in " + duration + work + "."
}

// removeLeftWorkDrivesError removes stderr's closing error line when it is
// the only error line, the last line with any text, and counts exactly
// wantDrives mounts that left work.
func removeLeftWorkDrivesError(stderr string, wantDrives int) (string, bool) {
	lines := strings.SplitAfter(stderr, "\n")
	errorLine := -1
	lastText := -1
	for i, line := range lines {
		text, _ := splitLineEnding(line)
		if strings.TrimSpace(text) != "" {
			lastText = i
		}
		if !strings.HasPrefix(text, commandErrorPrefix) {
			continue
		}
		if errorLine >= 0 {
			return "", false
		}
		errorLine = i
	}
	if errorLine < 0 || errorLine != lastText {
		return "", false
	}

	text, _ := splitLineEnding(lines[errorLine])
	match := leftWorkDrivesError.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	if drives, err := strconv.Atoi(match[1]); err != nil || drives != wantDrives {
		return "", false
	}

	return strings.Join(append(lines[:errorLine:errorLine], lines[errorLine+1:]...), ""), true
}

// splitLineEnding splits a line kept by strings.SplitAfter into its text and
// its line ending.
func splitLineEnding(line string) (text, ending string) {
	text = strings.TrimSuffix(line, "\n")
	text = strings.TrimSuffix(text, "\r")

	return text, line[len(text):]
}
