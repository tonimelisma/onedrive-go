package reviewgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const githubPageSize = 100

const maxErrorBodyBytes = 4096

// PullRequestReader provides the remote data needed by the gate.
type PullRequestReader interface {
	ListChangedFiles(ctx context.Context, repository string, pullNumber int) ([]string, error)
	ListReviews(ctx context.Context, repository string, pullNumber int) ([]Review, error)
}

// HTTPClient is the small subset of *http.Client used by the API client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client queries the GitHub REST API for PR files and reviews.
type Client struct {
	baseURL    string
	httpClient HTTPClient
	token      string
}

// NewClient constructs a GitHub REST client for the review gate.
func NewClient(httpClient HTTPClient, baseURL string, token string) *Client {
	trimmedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmedBaseURL == "" {
		trimmedBaseURL = "https://api.github.com"
	}

	return &Client{
		baseURL:    trimmedBaseURL,
		httpClient: httpClient,
		token:      strings.TrimSpace(token),
	}
}

type githubChangedFile struct {
	Filename string `json:"filename"`
}

type githubReview struct {
	ID          int64  `json:"id"`
	CommitID    string `json:"commit_id"`
	State       string `json:"state"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c *Client) ListChangedFiles(ctx context.Context, repository string, pullNumber int) ([]string, error) {
	var changedFiles []string

	for pageNumber := 1; ; pageNumber++ {
		var pageFiles []githubChangedFile
		if err := c.getPage(ctx, repository, pullNumber, "files", pageNumber, &pageFiles); err != nil {
			return nil, fmt.Errorf("list changed files: %w", err)
		}

		for _, file := range pageFiles {
			filename := strings.TrimSpace(file.Filename)
			if filename == "" {
				continue
			}
			changedFiles = append(changedFiles, filename)
		}

		if len(pageFiles) < githubPageSize {
			return changedFiles, nil
		}
	}
}

func (c *Client) ListReviews(ctx context.Context, repository string, pullNumber int) ([]Review, error) {
	var reviews []Review

	for pageNumber := 1; ; pageNumber++ {
		var pageReviews []githubReview
		if err := c.getPage(ctx, repository, pullNumber, "reviews", pageNumber, &pageReviews); err != nil {
			return nil, fmt.Errorf("list reviews: %w", err)
		}

		for _, rawReview := range pageReviews {
			review, err := normalizeReview(rawReview)
			if err != nil {
				return nil, fmt.Errorf("normalize review %d: %w", rawReview.ID, err)
			}
			reviews = append(reviews, review)
		}

		if len(pageReviews) < githubPageSize {
			return reviews, nil
		}
	}
}

func normalizeReview(rawReview githubReview) (Review, error) {
	submittedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(rawReview.SubmittedAt))
	if err != nil && strings.TrimSpace(rawReview.SubmittedAt) != "" {
		return Review{}, fmt.Errorf("parse submitted_at %q: %w", rawReview.SubmittedAt, err)
	}

	return Review{
		ID:            rawReview.ID,
		ReviewerLogin: strings.TrimSpace(rawReview.User.Login),
		CommitID:      strings.TrimSpace(rawReview.CommitID),
		State:         ReviewState(strings.ToUpper(strings.TrimSpace(rawReview.State))),
		SubmittedAt:   submittedAt,
	}, nil
}

func (c *Client) getPage(
	ctx context.Context,
	repository string,
	pullNumber int,
	resource string,
	pageNumber int,
	target any,
) (err error) {
	endpoint, err := c.buildURL(repository, pullNumber, resource, pageNumber)
	if err != nil {
		return fmt.Errorf("build URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET %s: %w", endpoint, err)
	}
	defer func() {
		closeErr := resp.Body.Close()
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close body: %w", closeErr)
		}
	}()

	if err := decodeJSONResponse(resp, target); err != nil {
		return fmt.Errorf("decode %s page %d: %w", resource, pageNumber, err)
	}

	return nil
}

func (c *Client) buildURL(repository string, pullNumber int, resource string, pageNumber int) (string, error) {
	parsedBaseURL, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}

	parsedBaseURL.Path = path.Join(parsedBaseURL.Path, "repos", repository, "pulls", strconv.Itoa(pullNumber), resource)
	query := parsedBaseURL.Query()
	query.Set("per_page", strconv.Itoa(githubPageSize))
	query.Set("page", strconv.Itoa(pageNumber))
	parsedBaseURL.RawQuery = query.Encode()

	return parsedBaseURL.String(), nil
}

func decodeJSONResponse(resp *http.Response, target any) error {
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			return fmt.Errorf("HTTP %s and read body: %w", resp.Status, readErr)
		}
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}

	return nil
}
