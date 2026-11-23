package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// A failing model seed gets an issue of its own, labeled seed-failure and
// titled by the seed's key, and no report ever closes it: the nightly rotates
// its seeds, so the next night runs other seeds and its green says nothing
// about one that failed. A seed that fails again, a later night or a fixed
// seed of another job, adds one comment naming the run. The change that fixes
// the seed closes the issue. Only the newest scheduled run of main writes the
// issues, like the nightly issue. Their text is built from the seed's replay
// line, rebuilt from the words devtool parsed, the names of the CI jobs it
// failed in and the run's URL alone: never a log excerpt.
const (
	seedIssueLabel            = "seed-failure"
	seedIssueLabelColor       = "D93F0B"
	seedIssueLabelDescription = "A seeded model test fails on this seed; the change that fixes it closes the issue"
	seedIssueTitlePrefix      = "Seed failure: "
	// seedIssueListLimit bounds one read of the open seed issues. A list
	// that full may be missing some, so it fails the read.
	seedIssueListLimit = 500
	// seedIssueWhat names the seed issue reads in their requests'
	// descriptions.
	seedIssueWhat = "seed"
)

// seedIssue is one open seed issue.
type seedIssue struct {
	number int
	// replay is the replay line its body holds for its key, nil for none.
	replay *seedReplay
	// texts are its body and comments, read for a seed the run failed.
	texts []string
}

// seedIssues are the open seed issues by the key each title names.
type seedIssues struct {
	labelExists bool
	open        map[string]seedIssue
	// unclaimable are the open issues with the label whose title names no
	// key, or a key an issue listed before it names: no claim stands for them.
	unclaimable []int
}

// seedIssueKey is the key a seed issue's title names.
func seedIssueKey(title string) (string, bool) {
	key, found := strings.CutPrefix(title, seedIssueTitlePrefix)
	key = strings.TrimSpace(key)

	return key, found && key != ""
}

// findSeedIssues reads whether the label exists and, when it does, every
// open seed issue with its body, and the comments of those whose keys
// withTexts names. A closed issue is not read: its seed is fixed.
func findSeedIssues(ctx context.Context, gh ghClient, repoRoot string, withTexts []string) (seedIssues, error) {
	exists, err := githubLabelExists(ctx, gh, repoRoot, seedIssueWhat, seedIssueLabel)
	if err != nil {
		return seedIssues{}, err
	}
	issues := seedIssues{labelExists: exists, open: map[string]seedIssue{}}
	if !exists {
		return issues, nil
	}
	out, err := gh.output(ctx, repoRoot, "seed issue list (GET repos/{owner}/{repo}/issues)",
		"issue", "list", "--label", seedIssueLabel, "--state", "open", "--json", "number,title,body",
		"--limit", strconv.Itoa(seedIssueListLimit))
	if err != nil {
		return seedIssues{}, err
	}
	var listed []struct {
		Number int
		Title  string
		Body   string
	}
	if decodeErr := decodeGHList("seed issue list", out, &listed); decodeErr != nil {
		return seedIssues{}, decodeErr
	}
	if len(listed) >= seedIssueListLimit {
		// A seed whose issue the list missed would get a second issue, and
		// its claim would end with no one having fixed it.
		return seedIssues{}, fmt.Errorf(
			"seed issue list: GitHub listed %d open %s issues, as many as one read takes, so some may be missing; close the fixed ones",
			len(listed), seedIssueLabel)
	}
	for _, entry := range listed {
		key, named := seedIssueKey(entry.Title)
		if _, known := issues.open[key]; !named || known {
			issues.unclaimable = append(issues.unclaimable, entry.Number)
			continue
		}
		issue := seedIssue{number: entry.Number}
		for _, replay := range seedReplaysFromLog(entry.Body) {
			if replay.key() == key {
				issue.replay = &replay
			}
		}
		issues.open[key] = issue
	}
	for _, key := range withTexts {
		issue, open := issues.open[key]
		if !open {
			continue
		}
		if issue.texts, err = githubIssueTexts(ctx, gh, repoRoot, seedIssueWhat, issue.number); err != nil {
			return seedIssues{}, err
		}
		issues.open[key] = issue
	}

	return issues, nil
}

// nightlySeedReplayLines is what the check prints under each failing seed's
// key: the command that replays it, from the run's log or else from its
// issue's body, and its open issue (seedIssueCloseLine says how to close it).
func nightlySeedReplayLines(reports []nightlyJobReport, issues seedIssues) map[string]string {
	commands := map[string]string{}
	for i := range reports {
		for _, seed := range reports[i].seeds {
			commands[seed.key()] = seed.command()
		}
	}
	for key, issue := range issues.open {
		if _, fromRun := commands[key]; fromRun {
			continue
		}
		commands[key] = "none: the issue holds no replay line"
		if issue.replay != nil {
			commands[key] = issue.replay.command()
		}
	}
	for key := range commands {
		if issue, open := issues.open[key]; open {
			commands[key] += fmt.Sprintf(" (seed issue #%d)", issue.number)
		}
	}

	return commands
}

// seedIssueCloseLine is what the check prints under a seed key this
// increment claimed whose issue is open: a seed fails for as long as its
// issue is open, so the pull request that fixes it must close the issue, and
// the pre-merge gate holds it to that (seedIssueCloseMissing).
func seedIssueCloseLine(issue int) string {
	number := strconv.Itoa(issue)

	return "the pull request that fixes the seed closes seed issue #" + number + ": put `" +
		seedIssueCloseKeyword + " #" + number + "` in its body"
}

// seedIssueCloseKeyword is the word before an issue number in a pull
// request's body that closes the issue when the pull request merges.
const seedIssueCloseKeyword = "Closes"

// seedIssueCloseMissing names, of keys, each seed key with an open issue
// that body does not close with `Closes #N`, N the issue's number and not
// the head of a longer one.
func seedIssueCloseMissing(keys []string, issues seedIssues, body string) []string {
	missing := []string{}
	for _, key := range keys {
		issue, open := issues.open[key]
		if !open || bodyClosesIssue(body, issue.number) {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (%s #%d)", key, seedIssueCloseKeyword, issue.number))
	}

	return missing
}

// bodyClosesIssue reports whether body holds `Closes #N` for issue N, the
// keyword in any case as GitHub reads it, and no digit after the number.
func bodyClosesIssue(body string, issue int) bool {
	text := strings.ToLower(body)
	want := strings.ToLower(seedIssueCloseKeyword) + " #" + strconv.Itoa(issue)
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], want)
		if i < 0 {
			return false
		}
		end := offset + i + len(want)
		if end == len(text) || text[end] < '0' || text[end] > '9' {
			return true
		}
		offset = end
	}

	return false
}

// failingSeed is one failing seed of a run and the jobs it failed in, by
// their names in the run: a seed can fail on one platform only.
type failingSeed struct {
	replay seedReplay
	jobs   []string
}

// failingSeeds are the run's failing seeds, each key once, in job order,
// each with every job it failed in.
func (r nightlyRunReport) failingSeeds() []failingSeed {
	seeds := []failingSeed{}
	index := map[string]int{}
	for _, job := range r.jobs {
		for _, seed := range job.seeds {
			i, seen := index[seed.key()]
			if !seen {
				i = len(seeds)
				index[seed.key()] = i
				seeds = append(seeds, failingSeed{replay: seed})
			}
			if !slices.Contains(seeds[i].jobs, job.name) {
				seeds[i].jobs = append(seeds[i].jobs, job.name)
			}
		}
	}

	return seeds
}

// seedIssueAction is what the report does to one failing seed's issue.
type seedIssueAction int

const (
	// seedIssueOpen: the seed has no open issue.
	seedIssueOpen seedIssueAction = iota
	// seedIssueComment: its open issue names no earlier failure in this run.
	seedIssueComment
	// seedIssueReported: its open issue already names this run, from an
	// earlier attempt of the same report.
	seedIssueReported
)

// seedIssueWrite is the report's write for one failing seed.
type seedIssueWrite struct {
	action seedIssueAction
	key    string
	issue  int
	text   string
}

// seedIssuePlan is every write one report makes to the seed issues, decided
// before any is made, so a dry run prints exactly what a real run writes.
type seedIssuePlan struct {
	createLabel bool
	writes      []seedIssueWrite
	// skipped names a run that writes no seed issue, and unwritten counts
	// its failing seeds.
	skipped   string
	unwritten int
}

// planSeedIssues decides the write for each of seeds, given the open seed
// issues: an issue for a seed that has none, a comment naming the run for
// one that has, nothing when the issue already names the run.
func planSeedIssues(seeds []failingSeed, runURL string, issues seedIssues) seedIssuePlan {
	plan := seedIssuePlan{}
	for _, seed := range seeds {
		key := seed.replay.key()
		issue, open := issues.open[key]
		switch {
		case !open:
			plan.writes = append(plan.writes, seedIssueWrite{action: seedIssueOpen, key: key, text: renderSeedIssue(seed, runURL)})
			plan.createLabel = !issues.labelExists
		case textsNameRun(issue.texts, runURL):
			plan.writes = append(plan.writes, seedIssueWrite{action: seedIssueReported, key: key, issue: issue.number})
		default:
			plan.writes = append(plan.writes, seedIssueWrite{
				action: seedIssueComment, key: key, issue: issue.number, text: renderSeedRecurrence(runURL),
			})
		}
	}

	return plan
}

// renderSeedIssue is the body of a failing seed's issue.
func renderSeedIssue(seed failingSeed, runURL string) string {
	replay := seed.replay
	assignment := replay.assignments[0]
	test := nightlyFailedTestKey(goTestFailure{pkg: replay.pkg, test: replay.test})
	origin := "The test printed this line"
	caveat := ""
	if replay.fromSubtestName {
		origin = "The test printed no replay line, since go test's deadline ended the run before its cleanup ran, " +
			"so this line is built from the name of its subtest"
		caveat = "The deadline ended the run while this seed ran: it may be the seed that hung or one that ran beside it. " +
			"The log does not say whether the run had the race detector, so the command has no `" + seedReplayRace + "`: " +
			"add it when the job ran its tests under the race detector.\n\n"
	}
	jobs := "the CI job `"
	if len(seed.jobs) > 1 {
		jobs = "the CI jobs `"
	}

	return fmt.Sprintf("The seeded model test `%s` fails on `%s=%s` in the scheduled CI run %s.\n",
		test, assignment.name, assignment.value, runURL) +
		"It failed in " + jobs + strings.Join(seed.jobs, "`, `") + "`.\n\n" +
		origin + "; the command after `" + seedReplayPrefix + "` replays the seed. " +
		"Run it at the commit the run tested: what a seed does changes with the code.\n\n" +
		caveat +
		"    " + replay.line() + "\n\n" +
		"No report closes this issue: each night runs other seeds, so a green night says nothing about this one. " +
		"The change that fixes the seed closes it and keeps the seed as a named test.\n"
}

// renderSeedRecurrence is the comment of a seed that failed again.
func renderSeedRecurrence(runURL string) string {
	return "The seed failed again in the scheduled CI run " + runURL + ".\n"
}

// textsNameRun reports whether one of texts names runURL whole: not as the
// head of a longer URL, such as another run whose id starts with its id.
func textsNameRun(texts []string, runURL string) bool {
	for _, text := range texts {
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], runURL)
			if i < 0 {
				break
			}
			end := offset + i + len(runURL)
			if end == len(text) || !isURLPathChar(text[end]) {
				return true
			}
			offset = end
		}
	}

	return false
}

func isURLPathChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("/_-", c) >= 0
}

// describe states the plan's writes, each line starting with verb as the
// nightly plan's describe does.
func (p *seedIssuePlan) describe(verb string) string {
	var b strings.Builder
	if p.skipped != "" {
		fmt.Fprintf(&b, "%s does not write the %s issues of its %d failing seeds: only the newest scheduled run of %s does\n",
			p.skipped, seedIssueLabel, p.unwritten, mainBranchName)
		return b.String()
	}
	if p.createLabel {
		fmt.Fprintf(&b, "%screate the label %s\n", verb, seedIssueLabel)
	}
	for _, write := range p.writes {
		switch write.action {
		case seedIssueOpen:
			fmt.Fprintf(&b, "%sopen an issue %q labeled %s with:\n%s", verb, seedIssueTitlePrefix+write.key, seedIssueLabel, write.text)
		case seedIssueComment:
			fmt.Fprintf(&b, "%scomment on seed issue #%d (%s) with:\n%s", verb, write.issue, write.key, write.text)
		case seedIssueReported:
			fmt.Fprintf(&b, "seed issue #%d (%s) already names this run; nothing to add\n", write.issue, write.key)
		}
	}

	return b.String()
}

// applySeedIssues makes the plan's writes. Each issue, comment and the label
// is made once, because a 5xx can come after GitHub made it; a write that
// fails does not stop the next seed's, and the failures are returned joined.
func applySeedIssues(ctx context.Context, gh ghClient, repoRoot string, plan *seedIssuePlan, stdout io.Writer) error {
	if plan.createLabel {
		if _, err := gh.outputUnlessDone(ctx, repoRoot, "seed label create (POST repos/{owner}/{repo}/labels)",
			ghRefusedAs("already exists"),
			"label", "create", seedIssueLabel, "--color", seedIssueLabelColor, "--description", seedIssueLabelDescription,
		); err != nil {
			return err
		}
	}
	var failures []error
	for _, write := range plan.writes {
		if err := applySeedIssueWrite(ctx, gh, repoRoot, write, stdout); err != nil {
			failures = append(failures, fmt.Errorf("seed issue %s: %w", write.key, err))
		}
	}

	return errors.Join(failures...)
}

func applySeedIssueWrite(ctx context.Context, gh ghClient, repoRoot string, write seedIssueWrite, stdout io.Writer) error {
	issue := strconv.Itoa(write.issue)
	switch write.action {
	case seedIssueOpen:
		out, err := gh.outputOnce(ctx, repoRoot, "seed issue create (POST repos/{owner}/{repo}/issues)",
			"issue", "create", "--title", seedIssueTitlePrefix+write.key, "--label", seedIssueLabel, "--body", write.text)
		if err != nil {
			return err
		}
		return writeStatus(stdout, "nightly report: opened "+strings.TrimSpace(string(out))+" for "+write.key+"\n")
	case seedIssueComment:
		if err := commentOnIssue(ctx, gh, repoRoot, seedIssueWhat, issue, write.text); err != nil {
			return err
		}
		return writeStatus(stdout, "nightly report: commented on seed issue #"+issue+"\n")
	case seedIssueReported:
		return writeStatus(stdout, "nightly report: seed issue #"+issue+" already names this run\n")
	}

	return nil
}
