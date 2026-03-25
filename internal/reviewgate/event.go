package reviewgate

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type githubEventPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number int  `json:"number"`
		Draft  bool `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// LoadPullRequest reads the GitHub event payload and extracts the PR data the
// gate needs. The event is the source of truth for the head SHA that must be
// reviewed, so the gate does not infer it from secondary API calls.
func LoadPullRequest(eventPath string, fallbackRepository string) (PullRequest, error) {
	rawEvent, err := os.ReadFile(eventPath)
	if err != nil {
		return PullRequest{}, fmt.Errorf("read event payload: %w", err)
	}

	var payload githubEventPayload
	if err := json.Unmarshal(rawEvent, &payload); err != nil {
		return PullRequest{}, fmt.Errorf("decode event payload: %w", err)
	}

	repository := strings.TrimSpace(payload.PullRequest.Base.Repo.FullName)
	if repository == "" {
		repository = strings.TrimSpace(payload.Repository.FullName)
	}
	if repository == "" {
		repository = strings.TrimSpace(fallbackRepository)
	}
	if repository == "" {
		return PullRequest{}, fmt.Errorf("event payload missing repository")
	}

	headSHA := strings.TrimSpace(payload.PullRequest.Head.SHA)
	if headSHA == "" {
		return PullRequest{}, fmt.Errorf("event payload missing pull request head SHA")
	}
	if payload.PullRequest.Number <= 0 {
		return PullRequest{}, fmt.Errorf("event payload missing pull request number")
	}

	return PullRequest{
		Repository: repository,
		Number:     payload.PullRequest.Number,
		Draft:      payload.PullRequest.Draft,
		HeadSHA:    headSHA,
	}, nil
}
