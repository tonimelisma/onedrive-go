package devtool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Nightly failures are owned through claims: JSON records of who fixes a
// failing key, kept in GitHub issues that nightly_claim_store.go reads and
// writes over REST. Every transition (create, renew, take over, mark fixed,
// delete) is a compare-and-swap on the record the agent last read, so two
// agents checking the same nightly at the same moment cannot both win a
// claim.
const (
	nightlyCASAttempts     = 3
	nightlyStateClaimed    = "claimed"
	nightlyStateFixed      = "fixed"
	checkConclusionFailure = "failure"

	// nightlyClaimFormat names the key format a claim record was written
	// for. A record that names another format, or none, or whose key does
	// not name the claim it belongs to, is a stale claim: its key may stand
	// for a test that fails under another key now. The check reports it by
	// its issue, a failing key whose claim it is replaces it, and `nightly
	// release` deletes it; it is never read as the claim of a key.
	nightlyClaimFormat = "package-qualified"

	// NightlyClaimLease is how long a claim blocks other increments without
	// being renewed. A nightly check by the holder renews each claim whose
	// key fails in the run it reads; the holder's pre-pr and pre-merge stages
	// renew every claim it holds.
	NightlyClaimLease = 24 * time.Hour
)

// A claim's name is nightlyClaimName of its key.
const (
	// nightlyClaimNameMaxLen bounds a claim's name, so a name stays short
	// enough to type as `nightly release --key` and to print on one line.
	nightlyClaimNameMaxLen = 180
	// nightlyClaimNameEscape starts every escape of the encoding.
	nightlyClaimNameEscape = '_'
	// nightlyClaimNameDigestMarker starts the digest that ends the name of a
	// key whose encoding is too long. An escape is always followed by a
	// letter or a digit, so no encoding holds the marker.
	nightlyClaimNameDigestMarker = "_-"
)

// nightlyClaimName is the name of key's claim, its identity among the
// claims: the same key always gives the same name and two keys never one.
// Lowercase letters, digits and '-' stand for themselves, except a '-' that
// would start the name; an uppercase letter is '_' and its lowercase, and
// every other byte, '_' included, is '_' and its three-digit decimal value,
// so every escape reads one way. The name therefore holds only lowercase
// letters, digits, '-' and '_': it needs no quoting on a command line, starts
// with no '-' a command line would read as an option, and two names never
// differ by case alone. A key with no name, or one whose name would pass
// nightlyClaimNameMaxLen, is named by the head of its encoding cut between
// escapes, nightlyClaimNameDigestMarker and the SHA-256 of the whole key.
func nightlyClaimName(key string) string {
	var b strings.Builder
	limit := nightlyClaimNameMaxLen
	head := limit - len(nightlyClaimNameDigestMarker) - hex.EncodedLen(sha256.Size)
	headLen := 0
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-' && i > 0:
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte(nightlyClaimNameEscape)
			b.WriteByte(c - 'A' + 'a')
		default:
			fmt.Fprintf(&b, "%c%03d", nightlyClaimNameEscape, c)
		}
		if b.Len() <= head {
			headLen = b.Len()
		}
	}
	if b.Len() > 0 && b.Len() <= limit {
		return b.String()
	}
	digest := sha256.Sum256([]byte(key))

	return b.String()[:headLen] + nightlyClaimNameDigestMarker + hex.EncodeToString(digest[:])
}

type NightlyAction string

const (
	NightlyActionCheck   NightlyAction = "check"
	NightlyActionRelease NightlyAction = "release"
	NightlyActionReport  NightlyAction = "report"
)

type NightlyOptions struct {
	RepoRoot string
	Action   NightlyAction
	// Branch is the increment branch that owns claims taken by this call.
	Branch string
	// Key names the claim to give up for NightlyActionRelease.
	Key string
	// RunID is the workflow run NightlyActionReport reports on.
	RunID int64
	// DryRun makes NightlyActionReport print what it would write to GitHub
	// and write nothing.
	DryRun bool
	Stdout io.Writer
	Now    func() time.Time
	// Sleep waits out the backoff before a GitHub request is repeated; nil
	// waits on the real clock.
	Sleep func(context.Context, time.Duration) error
}

type nightlyClaim struct {
	State string `json:"state"`
	Key   string `json:"key"`
	// Format is nightlyClaimFormat for every claim this code writes.
	Format string `json:"format"`
	Owner  string `json:"owner"`
	Host   string `json:"host,omitempty"`
	// Jobs are the ci.yml jobs the key failed in: a fixed claim is confirmed
	// only by a run in which each of them passed.
	Jobs      []string `json:"jobs,omitempty"`
	RunID     int64    `json:"run_id,omitempty"`
	RunHead   string   `json:"run_head,omitempty"`
	At        string   `json:"at"`
	FixCommit string   `json:"fix_commit,omitempty"`
}

// nightlyClaimEntry is one claim as the issue store holds it.
type nightlyClaimEntry struct {
	// Name is the claim's identity: nightlyClaimName of the key its issue
	// was opened for, or issue-<number> for an issue whose record cannot be
	// read.
	Name string
	// Issue is the number of the issue that holds the claim.
	Issue int
	// Version is the state of the claim this agent read, which a later
	// compare-and-swap expects the issue to still hold.
	Version string
	Claim   nightlyClaim
}

// isStale reports whether the record was written for another key format, or
// names a key that is not the claim's.
func (r *nightlyClaimEntry) isStale() bool {
	return r.Claim.Format != nightlyClaimFormat || nightlyClaimName(r.Claim.Key) != r.Name
}

// displayKey is what the check prints for the claim: its key, or for a stale
// claim its issue, issue-<number>, which is what `nightly release --key`
// takes for it.
func (r *nightlyClaimEntry) displayKey() string {
	if r.isStale() {
		return nightlyClaimIssueName(r.Issue)
	}

	return r.Claim.Key
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

// errNightlyClaimsForbidden is a claim write GitHub refused with 403: the
// credentials this checkout's gh holds may not write the repository's issues.
var errNightlyClaimsForbidden = errors.New("nightly claims may not be written with these GitHub credentials")

// RunNightly is the entry point of `devtool nightly`. A GitHub request that
// still fails after the client's retries ends the command with one plain
// sentence naming GitHub and the request.
func RunNightly(ctx context.Context, runner commandRunner, opts NightlyOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	gh := newGHClient(runner, opts.Sleep)

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
		return plainGitHubFailure(checkNightly(ctx, runner, gh, opts.RepoRoot, owner, now, stdout))
	case NightlyActionRelease:
		return plainGitHubFailure(releaseNightlyClaim(ctx, gh, opts.RepoRoot, opts.Branch, opts.Key, stdout))
	case NightlyActionReport:
		if opts.RunID <= 0 {
			return errors.New("nightly report: --run-id is required")
		}
		return plainGitHubFailure(reportNightly(ctx, gh, opts.RepoRoot, opts.RunID, opts.DryRun, stdout))
	default:
		return fmt.Errorf("nightly: unknown action %q", opts.Action)
	}
}

// plainGitHubFailure replaces a request GitHub kept failing with the one
// plain sentence that says so, and returns any other error as it is.
func plainGitHubFailure(err error) error {
	if sentence, ok := githubUnavailableSentence(err); ok {
		return errors.New(sentence)
	}

	return err
}

// checkNightly reads the newest completed scheduled run, claims every
// failure no other increment holds, and fails when a failure is left without
// an owner or when the nightly has no verdict to act on.
func checkNightly(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	repoRoot string,
	owner string,
	now func() time.Time,
	stdout io.Writer,
) error {
	verdict, err := loadNightlyVerdict(ctx, gh, repoRoot, now())
	if err != nil {
		return err
	}
	if verdict.finding != "" {
		return errors.New(verdict.finding)
	}
	if !verdict.found {
		return writeStatus(stdout, "nightly: none of the newest scheduled runs has finished yet; nothing to claim\n")
	}
	run := &verdict.run
	if attributeErr := attributeNightlyFailures(ctx, gh, repoRoot, verdict.reports); attributeErr != nil {
		return attributeErr
	}
	failures := nightlyFailureKeys(verdict.reports)
	if len(nightlyFailedJobs(verdict.reports)) == 0 && run.Conclusion != checkConclusionSuccess {
		return fmt.Errorf("nightly: run %d (%s) concluded %s, but no job of it failed, so the failure has no key to claim",
			run.DatabaseID, run.URL, run.Conclusion)
	}
	seeds, err := addOpenSeedIssues(ctx, gh, repoRoot, failures)
	if err != nil {
		return err
	}
	if fetchErr := runner.Run(
		ctx, repoRoot, os.Environ(), io.Discard, io.Discard, "git", "fetch", "--quiet", "origin", mainBranchName,
	); fetchErr != nil {
		return fmt.Errorf("nightly fetch %s: %w", mainBranchName, fetchErr)
	}

	results := make([]nightlyKeyResult, 0, len(failures))
	for _, key := range sortedFailureKeys(failures) {
		result, resolveErr := resolveNightlyKey(ctx, runner, gh, repoRoot, key, failures[key], owner, run, now)
		if resolveErr != nil {
			return resolveErr
		}
		results = append(results, result)
	}
	stale, kept, err := pruneNightlyClaims(ctx, runner, gh, repoRoot, failures, run, verdict.reports, now)
	if err != nil {
		return err
	}

	if err := writeStatus(stdout, formatNightlyCheck(run, verdict.reports, results, stale, seeds)); err != nil {
		return err
	}
	for _, what := range kept {
		if err := writeStatus(stdout, "nightly: kept "+what+
			": GitHub refused to delete it with these credentials (HTTP 403); "+
			"it blocks no one, and a check whose credentials may write issues deletes it\n"); err != nil {
			return err
		}
	}

	return unclaimedNightlyFailures(results)
}

// addOpenSeedIssues adds the key of every open seed issue to failures, with
// no job, and returns the issues. A seed fails until the change that fixes it
// closes its issue, whatever later nights did: each night runs other seeds.
func addOpenSeedIssues(ctx context.Context, gh ghClient, repoRoot string, failures map[string][]string) (seedIssues, error) {
	seeds, err := findSeedIssues(ctx, gh, repoRoot, nil)
	if err != nil {
		return seedIssues{}, err
	}
	for key := range seeds.open {
		if _, failing := failures[key]; !failing {
			failures[key] = nil
		}
	}

	return seeds, nil
}

// unclaimedNightlyFailures fails the check when a failure is left without an
// owner.
func unclaimedNightlyFailures(results []nightlyKeyResult) error {
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

func sortedFailureKeys(failures map[string][]string) []string {
	keys := make([]string, 0, len(failures))
	for key := range failures {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatNightlyCheck(
	run *githubRun, reports []nightlyJobReport, results []nightlyKeyResult, stale []string, seeds seedIssues,
) string {
	var b strings.Builder
	writeNightlyRunVerdict(&b, run, reports)
	replays := nightlySeedReplayLines(reports, seeds)
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
		if replay, isSeed := replays[result.Key]; isSeed {
			fmt.Fprintf(&b, "      replay: %s\n", replay)
		}
		if issue, open := seeds.open[result.Key]; open && result.Disposition == nightlyMine {
			fmt.Fprintf(&b, "      %s\n", seedIssueCloseLine(issue.number))
		}
	}
	for _, issue := range seeds.unclaimable {
		fmt.Fprintf(&b, "  seed issue #%d: its title names no seed key, or one an earlier issue names; nobody can claim it\n", issue)
	}
	for _, key := range stale {
		fmt.Fprintf(&b,
			"  %s: stale claim, of an old key format, unreadable, or naming another key than its issue; "+
				"delete it with `go run ./cmd/devtool nightly release --branch <branch> --key %s`\n",
			key, key)
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
	gh ghClient,
	repoRoot string,
	key string,
	jobs []string,
	owner string,
	run *githubRun,
	now func() time.Time,
) (nightlyKeyResult, error) {
	name := nightlyClaimName(key)
	for attempt := 0; attempt < nightlyCASAttempts; attempt++ {
		claims, err := syncNightlyClaims(ctx, gh, repoRoot)
		if err != nil {
			return nightlyKeyResult{}, err
		}
		current, exists := claims[name]
		var held *nightlyClaimEntry
		if exists {
			held = &current
		}
		take, result, err := decideNightlyKey(ctx, runner, repoRoot, key, owner, held, run, now())
		if err != nil {
			return nightlyKeyResult{}, err
		}
		if !take {
			return result, nil
		}
		// A key only an open seed issue names did not fail in this run, so
		// the claim keeps the jobs it failed in when it was claimed.
		failedIn := jobs
		if len(failedIn) == 0 && held != nil && !held.isStale() {
			failedIn = held.Claim.Jobs
		}
		next := nightlyClaim{
			State:   nightlyStateClaimed,
			Key:     key,
			Format:  nightlyClaimFormat,
			Owner:   owner,
			Host:    hostName(),
			Jobs:    failedIn,
			RunID:   run.DatabaseID,
			RunHead: run.HeadSHA,
			At:      now().UTC().Format(time.RFC3339),
		}
		err = swapNightlyClaim(ctx, gh, repoRoot, name, current.Version, &next)
		if errors.Is(err, errNightlyClaimLost) {
			continue
		}
		if err != nil {
			return nightlyKeyResult{}, fmt.Errorf("claim %s: %w", key, err)
		}
		return result, nil
	}
	return nightlyKeyResult{}, fmt.Errorf("nightly: claim %s kept changing on the remote; run the check again", key)
}

// decideNightlyKey decides who owns key, given its claim (nil for none), and whether this increment takes it.
func decideNightlyKey(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	key string,
	owner string,
	held *nightlyClaimEntry,
	run *githubRun,
	at time.Time,
) (bool, nightlyKeyResult, error) {
	mine := nightlyKeyResult{Key: key, Disposition: nightlyMine, Detail: "new claim"}
	unclaimed := nightlyKeyResult{Key: key, Disposition: nightlyUnclaimed}
	if held == nil {
		if owner == "" {
			return false, unclaimed, nil
		}
		return true, mine, nil
	}

	if held.isStale() {
		if owner == "" {
			return false, unclaimed, nil
		}
		mine.Detail = "replaced a stale claim"
		return true, mine, nil
	}
	claim := &held.Claim

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
			return false, unclaimed, nil
		}
		mine.Detail = fmt.Sprintf("the earlier fix %s by %s did not hold", shortSHA(claim.FixCommit), claim.Owner)
		return true, mine, nil
	}

	if owner != "" && claim.Owner == owner {
		mine.Detail = "claim renewed"
		return true, mine, nil
	}
	if !nightlyClaimExpired(claim, at) {
		detail := fmt.Sprintf("%s on %s since %s", claim.Owner, claim.Host, claim.At)
		return false, nightlyKeyResult{Key: key, Disposition: nightlyHeld, Detail: detail}, nil
	}
	if owner == "" {
		return false, unclaimed, nil
	}
	mine.Detail = fmt.Sprintf("taken over from %s, whose claim of %s expired", claim.Owner, claim.At)
	return true, mine, nil
}

func nightlyClaimExpired(claim *nightlyClaim, at time.Time) bool {
	claimedAt, err := time.Parse(time.RFC3339, claim.At)
	if err != nil {
		return true
	}
	return at.Sub(claimedAt) >= NightlyClaimLease
}

// fixIsInRun reports whether the run's head contains the fix commit. For a
// key that still fails in that run, the fix did not hold.
//
// A clone that lacks either commit (a shallow session clone, or a fix commit
// from before a history rewrite) cannot answer, so the check fetches both by
// SHA once and asks again. A commit that still cannot be found is not
// provably in the run: a fixed claim then keeps awaiting a nightly and is not
// retired, which errs toward keeping a claim, never toward dropping one, and
// never fails the whole nightly check for one claim it cannot judge.
func fixIsInRun(ctx context.Context, runner commandRunner, repoRoot string, fixCommit string, runHead string) (bool, error) {
	if fixCommit == "" || runHead == "" {
		return true, nil
	}
	inRun, answered, err := askFixAncestry(ctx, runner, repoRoot, fixCommit, runHead)
	if answered || err != nil {
		return inRun, err
	}
	// A refused fetch leaves the commit unknown, which reads as not in the
	// run (see above), so its error is not returned.
	if runner.Run(
		ctx, repoRoot, os.Environ(), io.Discard, io.Discard,
		"git", "fetch", "--quiet", "--no-tags", "origin", fixCommit, runHead,
	) != nil {
		return false, nil
	}
	inRun, _, err = askFixAncestry(ctx, runner, repoRoot, fixCommit, runHead)

	return inRun, err
}

// gitUnknownObjectExit is git's exit status for a command given a commit the
// repository does not hold.
const gitUnknownObjectExit = 128

// askFixAncestry runs git merge-base --is-ancestor once. answered is false
// when git could not judge (exit 128: a commit the clone does not hold).
func askFixAncestry(ctx context.Context, runner commandRunner, repoRoot string, fixCommit string, runHead string) (inRun bool, answered bool, err error) {
	runErr := runner.Run(
		ctx, repoRoot, os.Environ(), io.Discard, io.Discard, "git", "merge-base", "--is-ancestor", fixCommit, runHead,
	)
	if runErr == nil {
		return true, true, nil
	}
	var exit interface{ ExitCode() int }
	if !errors.As(runErr, &exit) {
		return false, false, fmt.Errorf("nightly ancestry of fix %s: %w", fixCommit, runErr)
	}
	switch exit.ExitCode() {
	case 1:
		return false, true, nil
	case gitUnknownObjectExit:
		return false, false, nil
	default:
		return false, false, fmt.Errorf("nightly ancestry of fix %s: %w", fixCommit, runErr)
	}
}

// pruneNightlyClaims deletes claims that no longer describe an open failure:
// a fix that a run containing it confirmed, every job the key failed in
// having passed there, and an expired claim of a key that no longer fails.
// It returns the stale claims, by the issue names `nightly release` takes
// for them, and leaves them to it. Losing a delete race is harmless, so it is
// ignored.
func pruneNightlyClaims(
	ctx context.Context,
	runner commandRunner,
	gh ghClient,
	repoRoot string,
	failing map[string][]string,
	run *githubRun,
	reports []nightlyJobReport,
	now func() time.Time,
) ([]string, []string, error) {
	claims, duplicates, err := readNightlyClaims(ctx, gh, repoRoot)
	if err != nil {
		return nil, nil, err
	}
	// A duplicate never holds the claim, so closing it decides nothing; an
	// agent still racing for it finds, when it reads the claims back, that it
	// lost, and its own close of the same issue changes nothing.
	stale := []string{}
	kept := []string{}
	for _, issue := range duplicates {
		closeErr := closeNightlyClaimIssue(ctx, gh, repoRoot, issue)
		switch {
		case closeErr == nil:
		case isGitHubForbidden(closeErr):
			kept = append(kept, fmt.Sprintf("the duplicate claim issue #%d", issue))
		default:
			return nil, nil, fmt.Errorf("close duplicate nightly claim issue #%d: %w", issue, closeErr)
		}
	}
	for _, name := range sortedClaimKeys(claims) {
		current := claims[name]
		if current.isStale() {
			stale = append(stale, current.displayKey())
			continue
		}
		if _, stillFailing := failing[current.Claim.Key]; stillFailing {
			continue
		}
		obsolete := false
		switch current.Claim.State {
		case nightlyStateFixed:
			inRun, ancestryErr := fixIsInRun(ctx, runner, repoRoot, current.Claim.FixCommit, run.HeadSHA)
			if ancestryErr != nil {
				return nil, nil, ancestryErr
			}
			obsolete = inRun && nightlyJobsPassed(current.Claim.Jobs, reports)
		default:
			obsolete = nightlyClaimExpired(&current.Claim, now())
		}
		if !obsolete {
			continue
		}
		err = swapNightlyClaim(ctx, gh, repoRoot, name, current.Version, nil)
		switch {
		case err == nil, errors.Is(err, errNightlyClaimLost):
		case errors.Is(err, errNightlyClaimsForbidden):
			// A retired claim holds no failure, so keeping it blocks no
			// increment; only taking or renewing a claim must reach GitHub.
			kept = append(kept, "the retired claim on "+current.Claim.Key)
		default:
			return nil, nil, fmt.Errorf("delete claim %s: %w", current.Claim.Key, err)
		}
	}
	return stale, kept, nil
}

func sortedClaimKeys(claims map[string]nightlyClaimEntry) []string {
	keys := make([]string, 0, len(claims))
	for key := range claims {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// releaseNightlyClaim gives up the claim owner holds on key. A stale claim
// belongs to no current increment, so any branch may release it; it is named
// by its key or by its issue, issue-<number>, as the check prints it.
func releaseNightlyClaim(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	owner string,
	key string,
	stdout io.Writer,
) error {
	if owner == "" || key == "" {
		return errors.New("nightly release: --branch and --key are required")
	}
	claims, err := syncNightlyClaims(ctx, gh, repoRoot)
	if err != nil {
		return err
	}
	current, exists := claims[nightlyClaimName(key)]
	if !exists {
		for _, name := range sortedClaimKeys(claims) {
			if stale := claims[name]; stale.isStale() && stale.displayKey() == key {
				current, exists = stale, true
			}
		}
	}
	isStale := exists && current.isStale()
	if !isStale && (!exists || current.Claim.State != nightlyStateClaimed || current.Claim.Owner != owner) {
		return fmt.Errorf("nightly release: %s holds no claim on %s", owner, key)
	}
	if err := swapNightlyClaim(ctx, gh, repoRoot, current.Name, current.Version, nil); err != nil {
		return fmt.Errorf("nightly release %s: %w", key, err)
	}
	if isStale {
		return writeStatus(stdout, fmt.Sprintf("nightly: deleted %s, a stale claim\n", key))
	}
	return writeStatus(stdout, fmt.Sprintf("nightly: released %s; the next increment that checks the nightly will claim it\n", key))
}

// heldNightlyClaims lists, by key, the claims an increment branch holds.
func heldNightlyClaims(ctx context.Context, gh ghClient, repoRoot string, owner string) (map[string]nightlyClaimEntry, error) {
	claims, err := syncNightlyClaims(ctx, gh, repoRoot)
	if err != nil {
		return nil, err
	}
	held := map[string]nightlyClaimEntry{}
	for _, name := range sortedClaimKeys(claims) {
		current := claims[name]
		if !current.isStale() && current.Claim.State == nightlyStateClaimed && current.Claim.Owner == owner {
			held[current.Claim.Key] = current
		}
	}
	return held, nil
}

// renewNightlyClaims restamps every claim owner holds with the time now, so
// an increment that runs longer than the lease keeps the failures it is
// fixing. It returns the keys it renewed; a claim another agent moved
// meanwhile is reported in the error and the others are still renewed.
func renewNightlyClaims(ctx context.Context, gh ghClient, repoRoot string, owner string, now time.Time) ([]string, error) {
	held, err := heldNightlyClaims(ctx, gh, repoRoot, owner)
	if err != nil {
		return nil, err
	}
	renewed := []string{}
	var lost []error
	for _, key := range sortedClaimKeys(held) {
		current := held[key]
		next := current.Claim
		next.Host = hostName()
		next.At = now.UTC().Format(time.RFC3339)
		swapErr := swapNightlyClaim(ctx, gh, repoRoot, current.Name, current.Version, &next)
		if errors.Is(swapErr, errNightlyClaimLost) {
			lost = append(lost, fmt.Errorf("renew claim %s: %w", key, swapErr))
			continue
		}
		if swapErr != nil {
			return renewed, fmt.Errorf("renew claim %s: %w", key, swapErr)
		}
		renewed = append(renewed, key)
	}
	return renewed, errors.Join(lost...)
}

// requireNightlyClaimsInPR is the pre-merge gate: an increment may not merge
// while it holds a nightly claim its pull request does not account for. A
// claimed seed whose issue is open stays failing until the issue closes, so
// the pull request must also close that issue (`Closes #N`); the gate reads
// the open seed issues as the check does, and only when it holds a seed key.
// It returns the pull request's branch, the owner of its claims.
func requireNightlyClaimsInPR(ctx context.Context, gh ghClient, repoRoot string, prNumber int) (string, error) {
	pr, err := loadPullRequest(ctx, gh, repoRoot, prNumber)
	if err != nil {
		return "", err
	}
	held, err := heldNightlyClaims(ctx, gh, repoRoot, pr.Head.Ref)
	if err != nil {
		return "", err
	}
	missing := []string{}
	seedKeys := []string{}
	for _, key := range sortedClaimKeys(held) {
		if !nightlyBodyNamesKey(pr.Body, key) {
			missing = append(missing, key)
		}
		if isSeedKey(key) {
			seedKeys = append(seedKeys, key)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf(
			"dod pre-merge: %s holds nightly claims the PR body does not name (%s); "+
				"describe each fix under a \"Nightly failures\" heading, or give one up with "+
				"`devtool nightly release --branch %s --key <key>`",
			pr.Head.Ref,
			strings.Join(missing, ", "),
			pr.Head.Ref,
		)
	}
	if len(seedKeys) == 0 {
		return pr.Head.Ref, nil
	}
	issues, err := findSeedIssues(ctx, gh, repoRoot, nil)
	if err != nil {
		return "", err
	}
	if unclosed := seedIssueCloseMissing(seedKeys, issues, pr.Body); len(unclosed) > 0 {
		return "", fmt.Errorf(
			"dod pre-merge: %s holds seed claims whose open issues the PR body does not close (%s); "+
				"a seed fails for as long as its issue is open, so add the issue's `%s #N` line to the body",
			pr.Head.Ref,
			strings.Join(unclosed, ", "),
			seedIssueCloseKeyword,
		)
	}

	return pr.Head.Ref, nil
}

// nightlyBodyNamesKey reports whether body names key as a whole word, not as
// part of a longer key. A key's package holds the characters of a Go import
// path (letters, digits, '_', '-', '.', '~', '+' and '/'), and no key ends
// with '.', so a '.' after the key continues it only when a key character
// follows the '.'; otherwise it ends a sentence.
func nightlyBodyNamesKey(body string, key string) bool {
	for offset := 0; offset < len(body); {
		i := strings.Index(body[offset:], key)
		if i < 0 {
			return false
		}
		start, end := offset+i, offset+i+len(key)
		continuesBefore := start > 0 && isNightlyKeyChar(body[start-1])
		continuesAfter := end < len(body) && isNightlyKeyChar(body[end]) &&
			(body[end] != '.' || end+1 < len(body) && isNightlyKeyChar(body[end+1]) && body[end+1] != '.')
		if !continuesBefore && !continuesAfter {
			return true
		}
		offset = start + 1
	}
	return false
}

func isNightlyKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_-.~+/", c) >= 0
}

// markNightlyClaimsFixed turns an increment's claims into fixed records once
// its pull request has merged. The record keeps other increments off the key
// until a nightly that contains the merge commit has run.
func markNightlyClaimsFixed(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	owner string,
	mergeCommit string,
	now time.Time,
) error {
	held, err := heldNightlyClaims(ctx, gh, repoRoot, owner)
	if err != nil {
		return err
	}
	for _, key := range sortedClaimKeys(held) {
		current := held[key]
		next := current.Claim
		next.State = nightlyStateFixed
		next.FixCommit = mergeCommit
		next.At = now.UTC().Format(time.RFC3339)
		if err := swapNightlyClaim(ctx, gh, repoRoot, current.Name, current.Version, &next); err != nil {
			return fmt.Errorf("dod post-merge mark nightly claim %s fixed: %w", key, err)
		}
	}
	return nil
}

// hostName is this machine's name for a claim record, or "" when the system
// does not say: the host only helps a person find where a claim is held, and
// no decision reads it.
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
