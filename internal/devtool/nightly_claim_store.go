package devtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The nightly claims live in GitHub issues, read and written over REST
// alone, so the check takes a claim wherever gh reaches GitHub's REST API: a
// Claude Code cloud session pushes only its own branch and refuses GraphQL,
// but its REST calls reach the repository's issues.
//
// One issue holds one lineage of a claim. Its body holds the first record,
// version "b"; every later state is a comment naming the version it replaces,
// version "c<comment ID>". The head of an issue starts at its body and walks
// its comments in ascending ID order, a comment whose prev is the current
// head becoming the new head; every other comment lost a race and is ignored
// for good. GitHub numbers issues and comment IDs in the order it stores
// them, so of two writers that started from one version the first one stored
// wins, and each learns which it was by reading the issue back: an update is
// a compare-and-swap built from that order.
//
// Creating a claim opens an issue and then lists the claims again: the
// lowest-numbered open issue of a name whose head is not deleted is the
// claim, so of two agents creating one key's claim the issue opened first
// wins and the other closes its own. A delete appends a deleted head and then
// closes the issue; a lineage whose head is deleted is no claim, and a later
// claim of its key opens a new issue.
const (
	// nightlyClaimLabel marks a claim's issue; GitHub creates the label the
	// first time an issue is opened with it.
	nightlyClaimLabel = "nightly-claim"
	// nightlyClaimTitlePrefix starts a claim issue's title, which names the
	// key for a person: the claim's identity is the key of its record.
	nightlyClaimTitlePrefix = "Nightly claim: "
	// nightlyClaimTitleMaxLen is GitHub's bound on an issue's title.
	nightlyClaimTitleMaxLen = 256
	// nightlyClaimIssueNamePrefix names a claim by its issue, as the check
	// prints a stale claim and `nightly release --key` takes it.
	nightlyClaimIssueNamePrefix = "issue-"
	// nightlyClaimBodyVersion is the version of the record in the body.
	nightlyClaimBodyVersion = "b"
	// nightlyClaimCommentVersionPrefix starts the version of a comment's
	// record, followed by the comment's ID.
	nightlyClaimCommentVersionPrefix = "c"
	// nightlyClaimRecordFence opens the fenced block that holds a record.
	nightlyClaimRecordFence = "```json"
	// nightlyClaimFenceEnd closes it.
	nightlyClaimFenceEnd = "```"

	nightlyClaimBodyLead = "This issue holds a nightly failure claim (R-6.10.23), written by " +
		"`go run ./cmd/devtool nightly`; its comments are the claim's later states. " +
		"Give it up with `go run ./cmd/devtool nightly release`, never by editing or closing it by hand."
	nightlyClaimCommentLead = "A new state of this nightly claim."

	// nightlyClaimStateClosed is the issue state a deleted claim's issue
	// is moved to.
	nightlyClaimStateClosed = "closed"
)

// nightlyClaimTransition is the record of a comment: the version it replaces
// and either the claim's next record or its deletion.
type nightlyClaimTransition struct {
	Prev    string        `json:"prev"`
	Claim   *nightlyClaim `json:"claim,omitempty"`
	Deleted bool          `json:"deleted,omitempty"`
}

// githubIssueComment is an issue comment as REST lists it.
type githubIssueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// nightlyClaimHead is the head of one claim issue.
type nightlyClaimHead struct {
	version string
	claim   nightlyClaim
	// readable is false when the body holds no record.
	readable bool
	deleted  bool
}

// nightlyClaimIssueName names a claim by its issue.
func nightlyClaimIssueName(issue int) string {
	return nightlyClaimIssueNamePrefix + strconv.Itoa(issue)
}

// syncNightlyClaims reads the claims from their issues and returns them by
// name; nightlyClaimName(key) finds a key's. An entry's Version is the issue
// and its head, the value a later compare-and-swap expects the issue to still
// hold.
func syncNightlyClaims(ctx context.Context, gh ghClient, repoRoot string) (map[string]nightlyClaimEntry, error) {
	issues, err := listOpenLabeledIssues(ctx, gh, repoRoot, "nightly claim", nightlyClaimLabel)
	if err != nil {
		return nil, err
	}
	claims := map[string]nightlyClaimEntry{}
	for _, issue := range issues {
		var comments []githubIssueComment
		if issue.Comments > 0 {
			comments, err = listNightlyClaimComments(ctx, gh, repoRoot, issue.Number)
			if err != nil {
				return nil, err
			}
		}
		head := readNightlyClaimHead(issue.Body, comments)
		if head.deleted {
			continue
		}
		entry := nightlyClaimEntry{
			Name:    nightlyClaimIssueName(issue.Number),
			Issue:   issue.Number,
			Version: strconv.Itoa(issue.Number) + "/" + head.version,
			Claim:   head.claim,
		}
		var first nightlyClaim
		if parseNightlyClaimRecord(issue.Body, &first) {
			entry.Name = nightlyClaimName(first.Key)
		}
		if held, taken := claims[entry.Name]; taken && held.Issue < entry.Issue {
			continue
		}
		claims[entry.Name] = entry
	}

	return claims, nil
}

// readNightlyClaimHead walks an issue's comments from its body to its head.
// A body that holds no record still starts the walk, so an unreadable claim
// can be deleted like any other; its claim names no key, which makes it
// stale, and it blocks no increment.
func readNightlyClaimHead(body string, comments []githubIssueComment) nightlyClaimHead {
	head := nightlyClaimHead{version: nightlyClaimBodyVersion}
	head.readable = parseNightlyClaimRecord(body, &head.claim)
	if !head.readable {
		head.claim = nightlyClaim{State: nightlyStateClaimed, Owner: "unreadable"}
	}
	ordered := slices.SortedFunc(slices.Values(comments), func(a, b githubIssueComment) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	for _, comment := range ordered {
		var next nightlyClaimTransition
		if !parseNightlyClaimRecord(comment.Body, &next) || next.Prev != head.version || (next.Claim != nil) == next.Deleted {
			continue
		}
		head.version = nightlyClaimCommentVersionPrefix + strconv.FormatInt(comment.ID, 10)
		if next.Deleted {
			// A deleted lineage is over: nothing this code writes names a
			// deleted head as the version it replaces.
			head.deleted = true
			return head
		}
		head.claim = *next.Claim
	}

	return head
}

// parseNightlyClaimRecord decodes the record of a claim's body or comment:
// the JSON of its first fenced json block. It reports false for text that
// holds none, as a comment a person wrote.
func parseNightlyClaimRecord(text string, into any) bool {
	_, rest, found := strings.Cut(text, nightlyClaimRecordFence)
	if !found {
		return false
	}
	record, _, closed := strings.Cut(rest, nightlyClaimFenceEnd)
	if !closed {
		return false
	}

	return json.Unmarshal([]byte(strings.TrimSpace(record)), into) == nil
}

// nightlyClaimRecordText is the text of a claim's body or comment: a sentence
// for a person, then the record in a fenced json block.
func nightlyClaimRecordText(lead string, record any) (string, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode nightly claim record: %w", err)
	}

	return lead + "\n\n" + nightlyClaimRecordFence + "\n" + string(data) + "\n" + nightlyClaimFenceEnd + "\n", nil
}

// nightlyClaimTitle names key in a claim issue's title, cut to GitHub's
// bound: the title is for a person, and no read takes the key from it.
func nightlyClaimTitle(key string) string {
	title := nightlyClaimTitlePrefix + key
	if len(title) > nightlyClaimTitleMaxLen {
		title = strings.ToValidUTF8(title[:nightlyClaimTitleMaxLen], "")
	}

	return title
}

// listNightlyClaimComments reads every comment of a claim issue.
func listNightlyClaimComments(ctx context.Context, gh ghClient, repoRoot string, issue int) ([]githubIssueComment, error) {
	number := strconv.Itoa(issue)
	out, err := gh.output(ctx, repoRoot, "nightly claim comments (GET repos/{owner}/{repo}/issues/"+number+"/comments)",
		"api", "--paginate", githubRepoPath+"/issues/"+number+"/comments?per_page=100")
	if err != nil {
		return nil, err
	}
	var pages [][]githubIssueComment
	if err := decodeGHPages("nightly claim comments", out, &pages); err != nil {
		return nil, err
	}

	return slices.Concat(pages...), nil
}

// swapNightlyClaim moves the claim named name from the version this agent
// last read (empty for "does not exist") to a new record, or deletes it when
// next is nil. It returns errNightlyClaimLost when another agent changed the
// claim meanwhile, and errNightlyClaimsForbidden when GitHub refused a
// request of it with 403.
func swapNightlyClaim(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	name string,
	expectVersion string,
	next *nightlyClaim,
) error {
	var err error
	if expectVersion == "" {
		if next == nil {
			return fmt.Errorf("nightly claim %s: no claim to delete", name)
		}
		err = createNightlyClaim(ctx, gh, repoRoot, name, next)
	} else {
		err = appendNightlyClaim(ctx, gh, repoRoot, name, expectVersion, next)
	}
	if err != nil && !errors.Is(err, errNightlyClaimLost) && isGitHubForbidden(err) {
		return fmt.Errorf("%w: %s: %w", errNightlyClaimsForbidden, name, err)
	}

	return err
}

// createNightlyClaim opens an issue for name's claim and reads the claims
// back: when the claim of name is then another issue, opened first by a
// racing agent, it closes its own and the claim is lost.
func createNightlyClaim(ctx context.Context, gh ghClient, repoRoot string, name string, next *nightlyClaim) error {
	body, err := nightlyClaimRecordText(nightlyClaimBodyLead, next)
	if err != nil {
		return err
	}
	// Opened once: a 5xx can come after GitHub opened the issue, and a
	// repeat would open a second one.
	out, err := gh.outputOnce(ctx, repoRoot, "nightly claim create (POST repos/{owner}/{repo}/issues)",
		"api", "-X", "POST", githubRepoPath+"/issues",
		"-f", "title="+nightlyClaimTitle(next.Key), "-f", "body="+body, "-f", "labels[]="+nightlyClaimLabel)
	if err != nil {
		return err
	}
	var created struct {
		Number int `json:"number"`
	}
	if decodeErr := json.Unmarshal(out, &created); decodeErr != nil || created.Number <= 0 {
		return fmt.Errorf("decode nightly claim issue of %s: %q", name, strings.TrimSpace(string(out)))
	}
	claims, err := syncNightlyClaims(ctx, gh, repoRoot)
	if err != nil {
		return err
	}
	held, listed := claims[name]
	if listed && held.Issue == created.Number {
		return nil
	}
	if err := closeNightlyClaimIssue(ctx, gh, repoRoot, created.Number); err != nil {
		return err
	}
	if !listed {
		// The list did not show the new issue yet; closing it and
		// retaking the decision is safe, where keeping an issue the list
		// may later rank behind another's would not be.
		return fmt.Errorf("%w: %s: issue #%d was not listed back", errNightlyClaimLost, name, created.Number)
	}

	return fmt.Errorf("%w: %s: issue #%d was opened first", errNightlyClaimLost, name, held.Issue)
}

// appendNightlyClaim posts the claim's next state on its issue, naming the
// version it replaces, and reads the issue back: the claim is this agent's
// only when the issue is open and its head is the new comment. A delete that
// won closes the issue.
func appendNightlyClaim(
	ctx context.Context,
	gh ghClient,
	repoRoot string,
	name string,
	expectVersion string,
	next *nightlyClaim,
) error {
	issueText, prev, found := strings.Cut(expectVersion, "/")
	issue, convErr := strconv.Atoi(issueText)
	if !found || convErr != nil || issue <= 0 || prev == "" {
		return fmt.Errorf("nightly claim %s: malformed version %q", name, expectVersion)
	}
	text, err := nightlyClaimRecordText(nightlyClaimCommentLead, nightlyClaimTransition{
		Prev: prev, Claim: next, Deleted: next == nil,
	})
	if err != nil {
		return err
	}
	number := strconv.Itoa(issue)
	// Posted once: a repeat would be a second write, which loses to the
	// first and says nothing more.
	out, err := gh.outputOnce(ctx, repoRoot, "nightly claim update (POST repos/{owner}/{repo}/issues/"+number+"/comments)",
		"api", "-X", "POST", githubRepoPath+"/issues/"+number+"/comments", "-f", "body="+text)
	if err != nil {
		return err
	}
	var posted githubIssueComment
	if decodeErr := json.Unmarshal(out, &posted); decodeErr != nil || posted.ID <= 0 {
		return fmt.Errorf("decode nightly claim comment on issue #%s: %q", number, strings.TrimSpace(string(out)))
	}

	out, err = gh.output(ctx, repoRoot, "nightly claim issue (GET repos/{owner}/{repo}/issues/"+number+")",
		"api", githubRepoPath+"/issues/"+number)
	if err != nil {
		return err
	}
	var state struct {
		State string `json:"state"`
		Body  string `json:"body"`
	}
	if decodeErr := json.Unmarshal(out, &state); decodeErr != nil {
		return fmt.Errorf("decode nightly claim issue #%s: %w", number, decodeErr)
	}
	comments, err := listNightlyClaimComments(ctx, gh, repoRoot, issue)
	if err != nil {
		return err
	}
	head := readNightlyClaimHead(state.Body, comments)
	if state.State == nightlyClaimStateClosed ||
		head.version != nightlyClaimCommentVersionPrefix+strconv.FormatInt(posted.ID, 10) {
		return fmt.Errorf("%w: %s: issue #%s moved past %s", errNightlyClaimLost, name, number, prev)
	}
	if next == nil {
		return closeNightlyClaimIssue(ctx, gh, repoRoot, issue)
	}

	return nil
}

// closeNightlyClaimIssue closes a claim's issue. Closing an issue that is
// already closed changes nothing, so the request is repeated like a read.
func closeNightlyClaimIssue(ctx context.Context, gh ghClient, repoRoot string, issue int) error {
	number := strconv.Itoa(issue)
	_, err := gh.output(ctx, repoRoot, "nightly claim close (PATCH repos/{owner}/{repo}/issues/"+number+")",
		"api", "-X", "PATCH", githubRepoPath+"/issues/"+number, "-f", "state="+nightlyClaimStateClosed)

	return err
}
