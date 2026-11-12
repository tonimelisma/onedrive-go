package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Nightly failures are owned through claim refs on the shared remote. A claim
// is one commit on the empty tree whose subject is a JSON record, stored at
// refs/nightly-claims/<key>. Every transition (create, renew, take over, mark
// fixed, delete) is a compare-and-swap push: git rejects it atomically when
// the ref no longer holds the value the agent last read, so two agents
// checking the same nightly at the same moment cannot both win a claim.
const (
	nightlyClaimRefPrefix  = "refs/nightly-claims/"
	nightlyClaimRefspec    = "+" + nightlyClaimRefPrefix + "*:" + nightlyClaimRefPrefix + "*"
	nightlyWorkflowFile    = "ci.yml"
	nightlyRunListLimit    = "10"
	nightlyJobKeyPrefix    = "job-"
	nightlyAggregateJob    = "verify"
	nightlyEmptyTree       = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	nightlyCASAttempts     = 3
	nightlyStateClaimed    = "claimed"
	nightlyStateFixed      = "fixed"
	checkConclusionFailure = "failure"

	// NightlyClaimLease is how long a claim blocks other increments without
	// being renewed. Every nightly check by the holder renews it.
	NightlyClaimLease = 24 * time.Hour
)

type NightlyAction string

const (
	NightlyActionCheck   NightlyAction = "check"
	NightlyActionRelease NightlyAction = "release"
)

type NightlyOptions struct {
	RepoRoot string
	Action   NightlyAction
	// Branch is the increment branch that owns claims taken by this call.
	Branch string
	// Key names the claim to give up for NightlyActionRelease.
	Key    string
	Stdout io.Writer
	Now    func() time.Time
	// Sleep waits out the backoff before a GitHub request is repeated; nil
	// waits on the real clock.
	Sleep func(context.Context, time.Duration) error
}

type nightlyClaim struct {
	State     string `json:"state"`
	Key       string `json:"key"`
	Owner     string `json:"owner"`
	Host      string `json:"host,omitempty"`
	RunID     int64  `json:"run_id,omitempty"`
	RunHead   string `json:"run_head,omitempty"`
	At        string `json:"at"`
	FixCommit string `json:"fix_commit,omitempty"`
}

type nightlyClaimRef struct {
	SHA   string
	Claim nightlyClaim
}

type nightlyDisposition string

const (
	nightlyMine      nightlyDisposition = "mine"
	nightlyHeld      nightlyDisposition = "held"
	nightlyAwaiting  nightlyDisposition = "awaiting"
	nightlyUnclaimed nightlyDisposition = "unclaimed"
)

type nightlyKeyResult struct {
	Key         string
	Disposition nightlyDisposition
	Detail      string
}

var errNightlyClaimLost = errors.New("nightly claim changed on the remote")

// RunNightly is the entry point of `devtool nightly`.
func RunNightly(ctx context.Context, runner commandRunner, opts NightlyOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	switch opts.Action {
	case NightlyActionCheck:
		owner := opts.Branch
		if owner == "" {
			branch, err := currentIncrementBranch(ctx, runner, opts.RepoRoot)
			if err != nil {
				return err
			}
			owner = branch
		}
		return checkNightly(ctx, runner, newGHClient(runner, opts.Sleep), opts.RepoRoot, owner, now, stdout)
	case NightlyActionRelease:
		return releaseNightlyClaim(ctx, runner, opts.RepoRoot, opts.Branch, opts.Key, stdout)
	default:
		return fmt.Errorf("nightly: unknown action %q", opts.Action)
	}
}

// checkNightly reads the latest finished nightly run, claims every failure no
// other increment holds, and fails when a failure is left without an owner.
func checkNightly(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	repoRoot string,
	owner string,
	now func() time.Time,
	stdout io.Writer,
) error {
	run, found, err := loadLatestNightlyRun(ctx, gh, repoRoot)
	if err != nil {
		return err
	}
	if !found {
		return writeStatus(stdout, "nightly: no finished scheduled run found; nothing to claim\n")
	}

	keys := []string{}
	if run.Conclusion != checkConclusionSuccess {
		keys, err = loadNightlyFailureKeys(ctx, gh, repoRoot, &run)
		if err != nil {
			return err
		}
	}
	if fetchErr := runner.Run(
		ctx, repoRoot, os.Environ(), io.Discard, io.Discard, "git", "fetch", "--quiet", "origin", mainBranchName,
	); fetchErr != nil {
		return fmt.Errorf("nightly fetch %s: %w", mainBranchName, fetchErr)
	}

	results := make([]nightlyKeyResult, 0, len(keys))
	for _, key := range keys {
		result, resolveErr := resolveNightlyKey(ctx, runner, repoRoot, key, owner, &run, now)
		if resolveErr != nil {
			return resolveErr
		}
		results = append(results, result)
	}
	if err := pruneNightlyClaims(ctx, runner, repoRoot, keys, &run, now); err != nil {
		return err
	}

	if err := writeStatus(stdout, formatNightlyReport(&run, results)); err != nil {
		return err
	}
	unclaimed := []string{}
	for _, result := range results {
		if result.Disposition == nightlyUnclaimed {
			unclaimed = append(unclaimed, result.Key)
		}
	}
	if len(unclaimed) > 0 {
		return fmt.Errorf(
			"nightly: %d failure(s) have no owner (%s); rerun with --branch <increment-branch> to claim and fix them in this increment",
			len(unclaimed),
			strings.Join(unclaimed, ", "),
		)
	}
	return nil
}

func formatNightlyReport(run *githubRun, results []nightlyKeyResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "nightly: run %d on %s concluded %s (%s)\n", run.DatabaseID, shortSHA(run.HeadSHA), run.Conclusion, run.URL)
	for _, result := range results {
		switch result.Disposition {
		case nightlyMine:
			fmt.Fprintf(&b, "  %s: claimed by this increment; fix it here (%s)\n", result.Key, result.Detail)
		case nightlyHeld:
			fmt.Fprintf(&b, "  %s: being fixed elsewhere; skip (%s)\n", result.Key, result.Detail)
		case nightlyAwaiting:
			fmt.Fprintf(&b, "  %s: fix merged, awaiting the next nightly; skip (%s)\n", result.Key, result.Detail)
		case nightlyUnclaimed:
			fmt.Fprintf(&b, "  %s: UNCLAIMED\n", result.Key)
		}
	}
	return b.String()
}

func shortSHA(sha string) string {
	const shortLen = 8
	if len(sha) > shortLen {
		return sha[:shortLen]
	}
	return sha
}

// resolveNightlyKey decides who owns one failing key and performs the claim
// transition when this increment should own it. A lost compare-and-swap means
// another agent moved first, so the decision is retaken from the new state.
func resolveNightlyKey(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	key string,
	owner string,
	run *githubRun,
	now func() time.Time,
) (nightlyKeyResult, error) {
	for attempt := 0; attempt < nightlyCASAttempts; attempt++ {
		claims, err := syncNightlyClaims(ctx, runner, repoRoot)
		if err != nil {
			return nightlyKeyResult{}, err
		}
		current, exists := claims[key]
		var held *nightlyClaim
		if exists {
			held = &current.Claim
		}
		take, result, err := decideNightlyKey(ctx, runner, repoRoot, key, owner, held, run, now())
		if err != nil {
			return nightlyKeyResult{}, err
		}
		if !take {
			return result, nil
		}
		next := nightlyClaim{
			State:   nightlyStateClaimed,
			Key:     key,
			Owner:   owner,
			Host:    hostName(),
			RunID:   run.DatabaseID,
			RunHead: run.HeadSHA,
			At:      now().UTC().Format(time.RFC3339),
		}
		err = swapNightlyClaim(ctx, runner, repoRoot, key, current.SHA, &next)
		if errors.Is(err, errNightlyClaimLost) {
			continue
		}
		if err != nil {
			return nightlyKeyResult{}, err
		}
		return result, nil
	}
	return nightlyKeyResult{}, fmt.Errorf("nightly: claim %s kept changing on the remote; run the check again", key)
}

func decideNightlyKey(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	key string,
	owner string,
	claim *nightlyClaim,
	run *githubRun,
	at time.Time,
) (bool, nightlyKeyResult, error) {
	mine := nightlyKeyResult{Key: key, Disposition: nightlyMine, Detail: "new claim"}
	if claim == nil {
		if owner == "" {
			return false, nightlyKeyResult{Key: key, Disposition: nightlyUnclaimed}, nil
		}
		return true, mine, nil
	}

	if claim.State == nightlyStateFixed {
		failed, err := fixIsInRun(ctx, runner, repoRoot, claim.FixCommit, run.HeadSHA)
		if err != nil {
			return false, nightlyKeyResult{}, err
		}
		if !failed {
			detail := fmt.Sprintf("%s by %s", shortSHA(claim.FixCommit), claim.Owner)
			return false, nightlyKeyResult{Key: key, Disposition: nightlyAwaiting, Detail: detail}, nil
		}
		if owner == "" {
			return false, nightlyKeyResult{Key: key, Disposition: nightlyUnclaimed}, nil
		}
		mine.Detail = fmt.Sprintf("the earlier fix %s by %s did not hold", shortSHA(claim.FixCommit), claim.Owner)
		return true, mine, nil
	}

	if owner != "" && claim.Owner == owner {
		mine.Detail = "claim renewed"
		return true, mine, nil
	}
	if !nightlyClaimExpired(*claim, at) {
		detail := fmt.Sprintf("%s on %s since %s", claim.Owner, claim.Host, claim.At)
		return false, nightlyKeyResult{Key: key, Disposition: nightlyHeld, Detail: detail}, nil
	}
	if owner == "" {
		return false, nightlyKeyResult{Key: key, Disposition: nightlyUnclaimed}, nil
	}
	mine.Detail = fmt.Sprintf("taken over from %s, whose claim of %s expired", claim.Owner, claim.At)
	return true, mine, nil
}

func nightlyClaimExpired(claim nightlyClaim, at time.Time) bool {
	claimedAt, err := time.Parse(time.RFC3339, claim.At)
	if err != nil {
		return true
	}
	return at.Sub(claimedAt) >= NightlyClaimLease
}

// fixIsInRun reports whether the failing run already contained the fix
// commit. If it did, the fix did not hold and the failure is open again.
func fixIsInRun(ctx context.Context, runner commandRunner, repoRoot string, fixCommit string, runHead string) (bool, error) {
	if fixCommit == "" || runHead == "" {
		return true, nil
	}
	err := runner.Run(
		ctx, repoRoot, os.Environ(), io.Discard, io.Discard, "git", "merge-base", "--is-ancestor", fixCommit, runHead,
	)
	if err == nil {
		return true, nil
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("nightly ancestry of fix %s: %w", fixCommit, err)
}

// pruneNightlyClaims deletes claims that no longer describe an open failure:
// a fix the latest nightly ran and passed, and an expired claim of a key that
// no longer fails. Losing a delete race is harmless, so it is ignored.
func pruneNightlyClaims(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	failing []string,
	run *githubRun,
	now func() time.Time,
) error {
	claims, err := syncNightlyClaims(ctx, runner, repoRoot)
	if err != nil {
		return err
	}
	stillFailing := make(map[string]bool, len(failing))
	for _, key := range failing {
		stillFailing[key] = true
	}
	for _, key := range sortedClaimKeys(claims) {
		if stillFailing[key] {
			continue
		}
		current := claims[key]
		obsolete := false
		switch current.Claim.State {
		case nightlyStateFixed:
			obsolete, err = fixIsInRun(ctx, runner, repoRoot, current.Claim.FixCommit, run.HeadSHA)
			if err != nil {
				return err
			}
		default:
			obsolete = nightlyClaimExpired(current.Claim, now())
		}
		if !obsolete {
			continue
		}
		err = swapNightlyClaim(ctx, runner, repoRoot, key, current.SHA, nil)
		if err != nil && !errors.Is(err, errNightlyClaimLost) {
			return err
		}
	}
	return nil
}

func sortedClaimKeys(claims map[string]nightlyClaimRef) []string {
	keys := make([]string, 0, len(claims))
	for key := range claims {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func releaseNightlyClaim(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	owner string,
	key string,
	stdout io.Writer,
) error {
	if owner == "" || key == "" {
		return fmt.Errorf("nightly release: --branch and --key are required")
	}
	claims, err := syncNightlyClaims(ctx, runner, repoRoot)
	if err != nil {
		return err
	}
	current, exists := claims[key]
	if !exists || current.Claim.State != nightlyStateClaimed || current.Claim.Owner != owner {
		return fmt.Errorf("nightly release: %s holds no claim on %s", owner, key)
	}
	if err := swapNightlyClaim(ctx, runner, repoRoot, key, current.SHA, nil); err != nil {
		return err
	}
	return writeStatus(stdout, fmt.Sprintf("nightly: released %s; the next increment that checks the nightly will claim it\n", key))
}

// heldNightlyClaims lists the keys an increment branch has claimed.
func heldNightlyClaims(ctx context.Context, runner commandRunner, repoRoot string, owner string) (map[string]nightlyClaimRef, error) {
	claims, err := syncNightlyClaims(ctx, runner, repoRoot)
	if err != nil {
		return nil, err
	}
	held := map[string]nightlyClaimRef{}
	for _, key := range sortedClaimKeys(claims) {
		current := claims[key]
		if current.Claim.State == nightlyStateClaimed && current.Claim.Owner == owner {
			held[key] = current
		}
	}
	return held, nil
}

// requireNightlyClaimsInPR is the pre-merge gate: an increment may not merge
// while it holds a nightly claim its pull request does not account for.
func requireNightlyClaimsInPR(ctx context.Context, runner commandRunner, gh ghClient, repoRoot string, prNumber int) error {
	out, err := gh.output(
		ctx, repoRoot, fmt.Sprintf("nightly gh pr view %d", prNumber),
		"pr", "view", strconv.Itoa(prNumber), "--json", "body,headRefName",
	)
	if err != nil {
		return err
	}
	var pr struct {
		Body        string `json:"body"`
		HeadRefName string `json:"headRefName"` //nolint:tagliatelle // gh owns this JSON key.
	}
	if decodeErr := json.Unmarshal(out, &pr); decodeErr != nil {
		return fmt.Errorf("decode gh pr view %d: %w", prNumber, decodeErr)
	}
	held, err := heldNightlyClaims(ctx, runner, repoRoot, pr.HeadRefName)
	if err != nil {
		return err
	}
	missing := []string{}
	for _, key := range sortedClaimKeys(held) {
		if !strings.Contains(pr.Body, key) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"dod pre-merge: %s holds nightly claims the PR body does not name (%s); "+
				"describe each fix under a \"Nightly failures\" heading, or give one up with "+
				"`devtool nightly release --branch %s --key <key>`",
			pr.HeadRefName,
			strings.Join(missing, ", "),
			pr.HeadRefName,
		)
	}
	return nil
}

// markNightlyClaimsFixed turns an increment's claims into fixed records once
// its pull request has merged. The record keeps other increments off the key
// until a nightly that contains the merge commit has run.
func markNightlyClaimsFixed(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	owner string,
	mergeCommit string,
	now time.Time,
) error {
	held, err := heldNightlyClaims(ctx, runner, repoRoot, owner)
	if err != nil {
		return err
	}
	for _, key := range sortedClaimKeys(held) {
		current := held[key]
		next := current.Claim
		next.State = nightlyStateFixed
		next.FixCommit = mergeCommit
		next.At = now.UTC().Format(time.RFC3339)
		if err := swapNightlyClaim(ctx, runner, repoRoot, key, current.SHA, &next); err != nil {
			return fmt.Errorf("dod post-merge mark nightly claim %s fixed: %w", key, err)
		}
	}
	return nil
}

// syncNightlyClaims mirrors the remote claim refs into the local repository
// and returns them by key. The mirrored commit ID is the value a later
// compare-and-swap expects the remote to still hold.
func syncNightlyClaims(ctx context.Context, runner commandRunner, repoRoot string) (map[string]nightlyClaimRef, error) {
	if err := runner.Run(
		ctx, repoRoot, os.Environ(), io.Discard, io.Discard,
		"git", "fetch", "--quiet", "--prune", "origin", nightlyClaimRefspec,
	); err != nil {
		return nil, fmt.Errorf("nightly fetch claims: %w", err)
	}
	out, err := runner.Output(
		ctx, repoRoot, os.Environ(),
		"git", "for-each-ref", "--format=%(objectname)%09%(refname)%09%(contents:subject)", nightlyClaimRefPrefix,
	)
	if err != nil {
		return nil, fmt.Errorf("nightly list claims: %w", err)
	}
	claims := map[string]nightlyClaimRef{}
	for _, line := range strings.Split(string(out), "\n") {
		const claimFields = 3
		fields := strings.SplitN(strings.TrimSpace(line), "\t", claimFields)
		if len(fields) != claimFields {
			continue
		}
		key := strings.TrimPrefix(fields[1], nightlyClaimRefPrefix)
		var claim nightlyClaim
		if decodeErr := json.Unmarshal([]byte(fields[2]), &claim); decodeErr != nil {
			// An unreadable record is treated as an expired claim, so the
			// next increment replaces it instead of being blocked forever.
			claim = nightlyClaim{State: nightlyStateClaimed, Key: key, Owner: "unreadable"}
		}
		claims[key] = nightlyClaimRef{SHA: fields[0], Claim: claim}
	}
	return claims, nil
}

// swapNightlyClaim moves one claim ref from the commit this agent last read
// (empty for "does not exist") to a new record, or deletes it when next is
// nil. It returns errNightlyClaimLost when the remote ref changed meanwhile.
func swapNightlyClaim(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	key string,
	expectSHA string,
	next *nightlyClaim,
) error {
	ref := nightlyClaimRefPrefix + key
	source := ""
	if next != nil {
		record, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode nightly claim %s: %w", key, err)
		}
		out, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "commit-tree", nightlyEmptyTree, "-m", string(record))
		if err != nil {
			return fmt.Errorf("nightly claim commit %s: %w", key, err)
		}
		source = strings.TrimSpace(string(out))
	}
	out, err := runner.CombinedOutput(
		ctx, repoRoot, os.Environ(),
		"git", "push", "--porcelain", "--force-with-lease="+ref+":"+expectSHA, "origin", source+":"+ref,
	)
	if err == nil {
		return nil
	}
	text := string(out)
	if strings.Contains(text, "[rejected]") || strings.Contains(text, "stale info") {
		return fmt.Errorf("%w: %s", errNightlyClaimLost, key)
	}
	return fmt.Errorf("nightly claim push %s: %s: %w", key, strings.TrimSpace(text), err)
}

// loadLatestNightlyRun returns the newest scheduled run on main that reached
// a verdict. Canceled and in-flight runs say nothing about the tree.
func loadLatestNightlyRun(ctx context.Context, gh ghClient, repoRoot string) (githubRun, bool, error) {
	out, err := gh.output(
		ctx, repoRoot, "nightly run list",
		"run", "list",
		"--workflow", nightlyWorkflowFile,
		"--event", "schedule",
		"--branch", mainBranchName,
		"--status", checkStatusCompleted,
		"--limit", nightlyRunListLimit,
		"--json", "databaseId,conclusion,headSha,createdAt,url",
	)
	if err != nil {
		return githubRun{}, false, err
	}
	var runs []githubRun
	if err := json.Unmarshal(out, &runs); err != nil {
		return githubRun{}, false, fmt.Errorf("decode nightly run list: %w", err)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt > runs[j].CreatedAt })
	for i := range runs {
		if runs[i].Conclusion == checkConclusionSuccess || runs[i].Conclusion == checkConclusionFailure {
			return runs[i], true, nil
		}
	}
	return githubRun{}, false, nil
}

// loadNightlyFailureKeys names what failed in a run: each failing top-level
// test, or the job itself when its log names no test (a build or setup
// failure). The aggregate job only repeats the jobs it waits for.
func loadNightlyFailureKeys(ctx context.Context, gh ghClient, repoRoot string, run *githubRun) ([]string, error) {
	runID := strconv.FormatInt(run.DatabaseID, 10)
	out, err := gh.output(ctx, repoRoot, "nightly run view "+runID, "run", "view", runID, "--json", "jobs")
	if err != nil {
		return nil, err
	}
	var view struct {
		Jobs []githubJob `json:"jobs"`
	}
	if decodeErr := json.Unmarshal(out, &view); decodeErr != nil {
		return nil, fmt.Errorf("decode nightly run view %d: %w", run.DatabaseID, decodeErr)
	}

	failed := []githubJob{}
	for _, job := range view.Jobs {
		if job.Conclusion == checkConclusionFailure {
			failed = append(failed, job)
		}
	}
	seen := map[string]bool{}
	for _, job := range failed {
		if job.Name == nightlyAggregateJob && len(failed) > 1 {
			continue
		}
		log, logErr := gh.output(
			ctx, repoRoot, "nightly job log "+job.Name,
			"api", "repos/{owner}/{repo}/actions/jobs/"+strconv.FormatInt(job.DatabaseID, 10)+"/logs",
		)
		if logErr != nil {
			return nil, logErr
		}
		tests := parseGoTestLog(string(log)).failedTopLevelTests()
		if len(tests) == 0 {
			seen[nightlyJobKeyPrefix+sanitizeNightlyKey(job.Name)] = true
			continue
		}
		for _, test := range tests {
			seen[sanitizeNightlyKey(test)] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// sanitizeNightlyKey maps a test or job name onto one git ref path component.
func sanitizeNightlyKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func hostName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// currentIncrementBranch returns the checked-out branch, or "" on main or a
// detached HEAD, where no increment owns the work yet.
func currentIncrementBranch(ctx context.Context, runner commandRunner, repoRoot string) (string, error) {
	out, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("nightly current branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == mainBranchName || branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}
