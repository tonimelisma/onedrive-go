package devtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// The nightly report makes a run's verdict visible on GitHub: a commit
// status on the commit it ran on and, for a scheduled run of main, one issue
// that is open exactly while the nightly is red, and an issue of its own for
// each failing model seed (nightly_seed_issues.go). The workflow's
// nightly-report job runs it after every other job of the run.
const (
	nightlyStatusContext = "nightly"
	nightlyStatusSuccess = "success"
	nightlyStatusFailure = "failure"

	nightlyIssueLabel            = "nightly-failure"
	nightlyIssueLabelColor       = "B60205"
	nightlyIssueLabelDescription = "The scheduled CI run failed"
	nightlyIssueTitle            = "Nightly CI is failing"
	nightlyLabelListLimit        = "100"

	// nightlyIssueMaxTests bounds the tests one job's line names, so a run in
	// which a whole package failed still fits one comment.
	nightlyIssueMaxTests = 50

	// nightlyStatusDescriptionMax is the longest commit status description
	// GitHub takes.
	nightlyStatusDescriptionMax = 140
)

// nightlyRunReport is what the report knows about one run. The issue text is
// built from runURL and jobs alone: job logs can hold fixture names, and
// only the names of the failed tests are taken from them.
type nightlyRunReport struct {
	// runID is the run the command was given, whatever its view decodes.
	runID   int64
	runURL  string
	headSHA string
	jobs    []nightlyJobReport
}

func (r nightlyRunReport) failed() []nightlyJobReport {
	return nightlyFailedJobs(r.jobs)
}

func (r nightlyRunReport) statusState() string {
	if len(r.failed()) > 0 {
		return nightlyStatusFailure
	}

	return nightlyStatusSuccess
}

// statusDescription counts the jobs the night ran or requires, and names
// the ones that did not run; the PR lane's jobs, skipped on the schedule by
// their own condition, are not the night's. It fits GitHub's bound.
func (r nightlyRunReport) statusDescription() string {
	failed, counted := 0, 0
	notRun := []string{}
	for _, job := range r.jobs {
		if job.result != nightlyJobSkipped {
			counted++
		}
		switch {
		case job.result == nightlyJobNotRun:
			notRun = append(notRun, job.name)
		case job.result.failsTheNight():
			failed++
		}
	}
	if failed == 0 && len(notRun) == 0 {
		return "every nightly job passed"
	}
	description := ""
	if failed > 0 {
		description = fmt.Sprintf("%d of %d nightly jobs failed", failed, counted)
	}
	if len(notRun) == 0 {
		return description
	}
	if description != "" {
		description += "; "
	}

	return description + describeNotRun(notRun, nightlyStatusDescriptionMax-len(description))
}

// describeNotRun names the jobs that did not run, as many as fit in limit
// bytes, and says how many more there are.
func describeNotRun(names []string, limit int) string {
	const prefix = "did not run: "
	for shown := len(names); shown > 0; shown-- {
		text := prefix + strings.Join(names[:shown], ", ")
		if hidden := len(names) - shown; hidden > 0 {
			text += fmt.Sprintf(", and %d more", hidden)
		}
		if len(text) <= limit {
			return text
		}
	}

	return fmt.Sprintf("%s%d jobs", prefix, len(names))
}

// renderNightlyFailure is the issue text of a red night: the run, then each
// job that made it red, how it ended, and the failed tests its log named.
func renderNightlyFailure(r nightlyRunReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The scheduled CI run failed: %s\n\n", r.runURL)
	for _, job := range r.failed() {
		fmt.Fprintf(&b, "- `%s`: %s", job.name, job.result)
		if len(job.tests) > 0 {
			shown := job.tests[:min(len(job.tests), nightlyIssueMaxTests)]
			fmt.Fprintf(&b, "; failed tests: `%s`", strings.Join(shown, "`, `"))
			if hidden := len(job.tests) - len(shown); hidden > 0 {
				fmt.Fprintf(&b, ", and %d more", hidden)
			}
		}
		if job.logUnreadable {
			b.WriteString("; its log could not be read")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nEach failure is claimed and fixed by an increment through `go run ./cmd/devtool nightly check`. " +
		"Only a scheduled run of main writes this issue, and it closes on the next one that passes; " +
		"a manual run posts its commit status only.\n")

	return b.String()
}

// renderNightlyPass is the comment that closes the issue on a green night.
func renderNightlyPass(r nightlyRunReport) string {
	return fmt.Sprintf("The scheduled CI run passed: %s\n\nEvery job the night requires passed, so this issue is closed.\n", r.runURL)
}

// nightlyIssueAction is what the report does to the nightly issue.
type nightlyIssueAction int

const (
	// nightlyIssueNone: a green night with no open issue.
	nightlyIssueNone nightlyIssueAction = iota
	// nightlyIssueOpen, nightlyIssueComment, nightlyIssueClose: a red night
	// opens the issue or comments on the open one; a green one closes it.
	nightlyIssueOpen
	nightlyIssueComment
	nightlyIssueClose
	// nightlyIssueReported: the open issue already holds this run's verdict
	// line, so a red night adds nothing.
	nightlyIssueReported
	// nightlyIssueNotTheNightly: a run that is not a scheduled run of main
	// never touches the issue.
	nightlyIssueNotTheNightly
	// nightlyIssueNotTheNewest: a scheduled run of main that is not the
	// newest, such as a rerun of an older night, leaves the issue to the
	// newest run's report.
	nightlyIssueNotTheNewest
)

// nightlyIssueState is what the report reads of the nightly issue.
type nightlyIssueState struct {
	// open is the open nightly issue, zero for none.
	open        int
	labelExists bool
	// texts are the open issue's body and comments.
	texts []string
}

// nightlyReportPlan is every write one report makes to the issue and its
// label, decided before any is made, so a dry run prints exactly what a real
// run writes.
type nightlyReportPlan struct {
	action      nightlyIssueAction
	issue       int
	createLabel bool
	text        string
	// reported marks a close whose comment the issue already holds, from an
	// earlier attempt of the same report.
	reported bool
	// run names the run, for a plan that does not touch the issue.
	run string
	// newest is the newest scheduled run of main, for a plan of an older one.
	newest int64
}

// isNightlyRun reports whether run is a nightly whose verdict the issue
// tells: a scheduled run of main, the runs an increment's check reads.
func isNightlyRun(run *githubRun) bool {
	return run.Event == ciEventSchedule && run.HeadBranch == mainBranchName
}

// planNightlyReport decides the writes to the nightly issue for a run, given
// the newest scheduled run of main and the issue. A run that is not the
// nightly, or not the newest one, leaves the issue alone. A red night opens
// the issue, or adds one comment to the open one; a green night closes the
// open one with a comment. A comment the issue already holds, from a rerun
// of the same report, is not posted again.
func planNightlyReport(report nightlyRunReport, run *githubRun, newest int64, issue nightlyIssueState) nightlyReportPlan {
	plan := nightlyReportPlan{issue: issue.open}
	if !isNightlyRun(run) {
		plan.action = nightlyIssueNotTheNightly
		plan.run = fmt.Sprintf("a %s run of %s", run.Event, run.HeadBranch)
		return plan
	}
	if newest != report.runID {
		plan.action, plan.newest = nightlyIssueNotTheNewest, newest
		plan.run = fmt.Sprintf("scheduled run %d of %s", report.runID, run.HeadBranch)
		return plan
	}
	switch {
	case len(report.failed()) == 0 && issue.open != 0:
		plan.action, plan.text = nightlyIssueClose, renderNightlyPass(report)
		plan.reported = holdsVerdictLine(issue.texts, plan.text)
	case len(report.failed()) == 0:
		plan.action = nightlyIssueNone
	case issue.open != 0:
		plan.action, plan.text = nightlyIssueComment, renderNightlyFailure(report)
		if holdsVerdictLine(issue.texts, plan.text) {
			plan.action = nightlyIssueReported
		}
	default:
		plan.action, plan.text = nightlyIssueOpen, renderNightlyFailure(report)
		plan.createLabel = !issue.labelExists
	}

	return plan
}

// holdsVerdictLine reports whether one of texts holds the first line of
// text, the sentence that names the run's URL and its verdict, as a line of
// its own: a whole line, so a run whose URL starts with another's is not
// taken for it.
func holdsVerdictLine(texts []string, text string) bool {
	verdict, _, _ := strings.Cut(text, "\n")
	for _, held := range texts {
		for line := range strings.Lines(held) {
			if strings.TrimRight(line, "\r\n") == verdict {
				return true
			}
		}
	}

	return false
}

// describeStatus states the commit status the report posts, starting with
// verb as describe does.
func (r nightlyRunReport) describeStatus(verb string) string {
	return fmt.Sprintf("%spost commit status %s=%s on %s: %s (%s)\n",
		verb, nightlyStatusContext, r.statusState(), r.headSHA, r.statusDescription(), r.runURL)
}

// describe states the plan's writes, each line starting with verb: "would "
// for a dry run, "" for the writes a real run is about to make.
func (p *nightlyReportPlan) describe(verb string) string {
	var b strings.Builder
	if p.createLabel {
		fmt.Fprintf(&b, "%screate the label %s\n", verb, nightlyIssueLabel)
	}
	switch p.action {
	case nightlyIssueNone:
		fmt.Fprintf(&b, "no open %s issue to close\n", nightlyIssueLabel)
	case nightlyIssueOpen:
		fmt.Fprintf(&b, "%sopen an issue %q labeled %s with:\n%s", verb, nightlyIssueTitle, nightlyIssueLabel, p.text)
	case nightlyIssueComment:
		fmt.Fprintf(&b, "%scomment on issue #%d with:\n%s", verb, p.issue, p.text)
	case nightlyIssueClose:
		if p.reported {
			fmt.Fprintf(&b, "%sclose issue #%d, which already holds the comment\n", verb, p.issue)
			break
		}
		fmt.Fprintf(&b, "%sclose issue #%d with the comment:\n%s", verb, p.issue, p.text)
	case nightlyIssueReported:
		fmt.Fprintf(&b, "issue #%d already holds this run's verdict; nothing to add\n", p.issue)
	case nightlyIssueNotTheNightly:
		fmt.Fprintf(&b, "%s does not write the %s issue: only a scheduled run of %s does\n", p.run, nightlyIssueLabel, mainBranchName)
	case nightlyIssueNotTheNewest:
		fmt.Fprintf(&b, "%s is not the newest (run %d is) and leaves the %s issue as it is: only the newest scheduled run of %s writes it\n",
			p.run, p.newest, nightlyIssueLabel, mainBranchName)
	}

	return b.String()
}

// reportNightly reports one run: it reads the run, its jobs and the failing
// jobs' logs and posts the commit status, which depends on the run alone;
// then, for a scheduled run of main, it reads which one is the newest and,
// for the newest, the nightly issue and the open issues of the run's failing
// seeds, decides what happens to each and writes it. With dryRun it makes the
// same reads and prints each write instead. A read of the run fails the
// report before any write; a read of the newest run or the nightly issue
// fails it with the status posted and the issues as they are. A read of the
// seed issues that fails leaves them as they are and fails the report too,
// but only after the night's verdict is written: the status and the nightly
// issue follow a plan of their own, which the seed issues never hold up.
func reportNightly(ctx context.Context, gh ghClient, repoRoot string, runID int64, dryRun bool, stdout io.Writer) error {
	run, err := loadNightlyRun(ctx, gh, repoRoot, runID)
	if err != nil {
		return err
	}
	jobs := nightlyJobReports(run.Jobs, nightlyReportJob)
	if attributeErr := attributeNightlyFailures(ctx, gh, repoRoot, jobs); attributeErr != nil {
		return attributeErr
	}
	report := nightlyRunReport{runID: runID, runURL: run.URL, headSHA: run.HeadSHA, jobs: jobs}
	if dryRun {
		status := "nightly report: dry run, nothing is written\n" + report.describeStatus("would ")
		if writeErr := writeStatus(stdout, status); writeErr != nil {
			return writeErr
		}
	} else if statusErr := postNightlyStatus(ctx, gh, repoRoot, report, stdout); statusErr != nil {
		return statusErr
	}

	newest, issue := int64(0), nightlyIssueState{}
	if isNightlyRun(&run) {
		if newest, err = newestNightlyRun(ctx, gh, repoRoot); err != nil {
			return err
		}
	}
	if isNightlyRun(&run) && newest == runID {
		if issue, err = findNightlyIssue(ctx, gh, repoRoot); err != nil {
			return err
		}
	}
	plan := planNightlyReport(report, &run, newest, issue)
	seedPlan, seedErr := planNightlySeedIssues(ctx, gh, repoRoot, report, &plan)
	if seedErr != nil {
		seedErr = fmt.Errorf("nightly report: read the seed issues, so none is written: %w", seedErr)
	}

	if dryRun {
		return errors.Join(writeStatus(stdout, plan.describe("would ")+seedPlan.describe("would ")), seedErr)
	}
	if err := writeStatus(stdout, "nightly report: about to\n"+plan.describe("")+seedPlan.describe("")); err != nil {
		return errors.Join(err, seedErr)
	}

	return errors.Join(
		applyNightlyReport(ctx, gh, repoRoot, &plan, stdout),
		applySeedIssues(ctx, gh, repoRoot, &seedPlan, stdout),
		seedErr,
	)
}

// planNightlySeedIssues decides the writes to the run's failing seeds'
// issues. A run that writes no nightly issue writes no seed issue either and
// says so; for the newest scheduled run of main it reads the open seed issues
// before any write, and a read that fails plans no write.
func planNightlySeedIssues(
	ctx context.Context, gh ghClient, repoRoot string, report nightlyRunReport, plan *nightlyReportPlan,
) (seedIssuePlan, error) {
	seeds := report.failingSeeds()
	if len(seeds) == 0 {
		return seedIssuePlan{}, nil
	}
	if plan.action == nightlyIssueNotTheNightly || plan.action == nightlyIssueNotTheNewest {
		return seedIssuePlan{skipped: plan.run, unwritten: len(seeds)}, nil
	}
	keys := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		keys = append(keys, seed.replay.key())
	}
	issues, err := findSeedIssues(ctx, gh, repoRoot, keys)
	if err != nil {
		return seedIssuePlan{}, err
	}

	return planSeedIssues(seeds, report.runURL, issues), nil
}

// postNightlyStatus posts the run's commit status. Posting it again is safe,
// so it goes through the retrying client.
func postNightlyStatus(ctx context.Context, gh ghClient, repoRoot string, report nightlyRunReport, stdout io.Writer) error {
	if err := writeStatus(stdout, "nightly report: about to\n"+report.describeStatus("")); err != nil {
		return err
	}
	if _, err := gh.output(ctx, repoRoot, "nightly commit status (POST repos/{owner}/{repo}/statuses/"+report.headSHA+")",
		"api", "-X", "POST", "repos/{owner}/{repo}/statuses/"+report.headSHA,
		"-f", "state="+report.statusState(),
		"-f", "context="+nightlyStatusContext,
		"-f", "target_url="+report.runURL,
		"-f", "description="+report.statusDescription(),
	); err != nil {
		return err
	}

	return writeStatus(stdout, "nightly report: status posted\n")
}

// newestNightlyRun is the id of the newest scheduled run of main, read with
// the nightly check's run list. A list that names no run leaves it unknown,
// which fails the report: the run reported on is a scheduled run of main, so
// GitHub should list at least that one.
func newestNightlyRun(ctx context.Context, gh ghClient, repoRoot string) (int64, error) {
	runs, err := listNightlyRuns(ctx, gh, repoRoot, nightlyRunListLimit)
	if err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		return 0, fmt.Errorf("nightly report: GitHub lists no scheduled run of %s, so the newest one is unknown and the issue is left as it is",
			mainBranchName)
	}

	return runs[0].DatabaseID, nil
}

// findNightlyIssue reads whether the label exists, without which no issue
// can carry it, and the open nightly issue with its body and comments.
func findNightlyIssue(ctx context.Context, gh ghClient, repoRoot string) (nightlyIssueState, error) {
	exists, err := githubLabelExists(ctx, gh, repoRoot, "nightly", nightlyIssueLabel)
	if err != nil {
		return nightlyIssueState{}, err
	}
	state := nightlyIssueState{labelExists: exists}
	if !state.labelExists {
		return state, nil
	}

	out, err := gh.output(ctx, repoRoot, "nightly issue list (GET repos/{owner}/{repo}/issues)",
		"issue", "list", "--label", nightlyIssueLabel, "--state", "open", "--json", "number", "--limit", "1")
	if err != nil {
		return nightlyIssueState{}, err
	}
	var issues []struct{ Number int }
	if decodeErr := decodeGHList("nightly issue list", out, &issues); decodeErr != nil {
		return nightlyIssueState{}, decodeErr
	}
	if len(issues) == 0 {
		return state, nil
	}
	state.open = issues[0].Number
	if state.texts, err = githubIssueTexts(ctx, gh, repoRoot, "nightly", state.open); err != nil {
		return nightlyIssueState{}, err
	}

	return state, nil
}

// githubLabelExists reads whether the repository has the label; what names
// the read in the request's description.
func githubLabelExists(ctx context.Context, gh ghClient, repoRoot string, what string, label string) (bool, error) {
	out, err := gh.output(ctx, repoRoot, what+" label list (GET repos/{owner}/{repo}/labels)",
		"label", "list", "--search", label, "--json", "name", "--limit", nightlyLabelListLimit)
	if err != nil {
		return false, err
	}
	var labels []struct{ Name string }
	if decodeErr := decodeGHList(what+" label list", out, &labels); decodeErr != nil {
		return false, decodeErr
	}

	return slices.ContainsFunc(labels, func(listed struct{ Name string }) bool { return listed.Name == label }), nil
}

// githubIssueTexts reads an issue's body and comments, the body first; what
// names the read in the request's description.
func githubIssueTexts(ctx context.Context, gh ghClient, repoRoot string, what string, issue int) ([]string, error) {
	number := strconv.Itoa(issue)
	out, err := gh.output(ctx, repoRoot, what+" issue view (GET repos/{owner}/{repo}/issues/"+number+")",
		"issue", "view", number, "--json", "body,comments")
	if err != nil {
		return nil, err
	}
	var view struct {
		Body     string
		Comments []struct{ Body string }
	}
	if decodeErr := json.Unmarshal(out, &view); decodeErr != nil {
		return nil, fmt.Errorf("decode %s issue view %s: %w", what, number, decodeErr)
	}
	texts := []string{view.Body}
	for _, comment := range view.Comments {
		texts = append(texts, comment.Body)
	}

	return texts, nil
}

// decodeGHList decodes a gh list printed as JSON. A list command whose
// search matched nothing can print nothing at all instead of `[]`, which is
// an empty list.
func decodeGHList(what string, out []byte, list any) error {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	if err := json.Unmarshal(out, list); err != nil {
		return fmt.Errorf("decode %s: %w", what, err)
	}

	return nil
}

// applyNightlyReport makes the plan's writes to the issue and its label. An
// issue, a comment and a label are made once, because a 5xx can come after
// GitHub made them.
func applyNightlyReport(ctx context.Context, gh ghClient, repoRoot string, plan *nightlyReportPlan, stdout io.Writer) error {
	if plan.createLabel {
		if _, err := gh.outputUnlessDone(ctx, repoRoot, "nightly label create (POST repos/{owner}/{repo}/labels)",
			ghRefusedAs("already exists"),
			"label", "create", nightlyIssueLabel, "--color", nightlyIssueLabelColor, "--description", nightlyIssueLabelDescription,
		); err != nil {
			return err
		}
	}

	issue := strconv.Itoa(plan.issue)
	switch plan.action {
	case nightlyIssueNone, nightlyIssueReported, nightlyIssueNotTheNightly, nightlyIssueNotTheNewest:
		return writeStatus(stdout, "nightly report: nothing to write to the issue\n")
	case nightlyIssueOpen:
		out, err := gh.outputOnce(ctx, repoRoot, "nightly issue create (POST repos/{owner}/{repo}/issues)",
			"issue", "create", "--title", nightlyIssueTitle, "--label", nightlyIssueLabel, "--body", plan.text)
		if err != nil {
			return err
		}
		return writeStatus(stdout, "nightly report: opened "+strings.TrimSpace(string(out))+"\n")
	case nightlyIssueComment:
		if err := commentOnIssue(ctx, gh, repoRoot, "nightly", issue, plan.text); err != nil {
			return err
		}
		return writeStatus(stdout, "nightly report: commented on issue #"+issue+"\n")
	case nightlyIssueClose:
		if !plan.reported {
			if err := commentOnIssue(ctx, gh, repoRoot, "nightly", issue, plan.text); err != nil {
				return err
			}
		}
		if _, err := gh.outputUnlessDone(ctx, repoRoot, "nightly issue close (PATCH repos/{owner}/{repo}/issues/"+issue+")",
			ghRefusedAs("already closed"), "issue", "close", issue); err != nil {
			return err
		}
		return writeStatus(stdout, "nightly report: closed issue #"+issue+"\n")
	}

	return nil
}

// commentOnIssue posts one comment on an issue, once; what names the write in
// the request's description.
func commentOnIssue(ctx context.Context, gh ghClient, repoRoot string, what string, issue string, text string) error {
	_, err := gh.outputOnce(ctx, repoRoot, what+" issue comment (POST repos/{owner}/{repo}/issues/"+issue+"/comments)",
		"issue", "comment", issue, "--body", text)

	return err
}

// ghRefusedAs recognizes gh's refusal of a change that is already in place,
// by the words gh uses for it.
func ghRefusedAs(words string) func(error) bool {
	return func(err error) bool {
		return strings.Contains(err.Error(), words)
	}
}
