package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

type quirkRetrySpec struct {
	name   string
	policy retry.Policy
	match  func(error) (*GraphError, bool)
}

func doQuirkRetry[T any](ctx context.Context, c *Client, spec quirkRetrySpec, op func() (T, error)) (T, error) {
	var zero T
	attempts := make([]QuirkRetryAttempt, 0, spec.policy.MaxAttempts)

	for attempt := range spec.policy.MaxAttempts {
		value, err := op()
		if err == nil {
			return value, nil
		}

		graphErr, retryable := spec.match(err)
		if !retryable {
			return zero, err
		}
		attempts = append(attempts, buildQuirkRetryAttempt(attempt+1, graphErr))

		if attempt >= spec.policy.MaxAttempts-1 {
			quirkErr := &QuirkRetryError{
				Quirk:    spec.name,
				Attempts: attempts,
				Err:      err,
			}

			c.logger.Warn("graph quirk retry exhausted",
				slog.String("graph_quirk", quirkErr.Quirk),
				slog.Int("graph_quirk_attempt_count", len(quirkErr.Attempts)),
				slog.Any("graph_quirk_attempts", quirkErr.Attempts),
				slog.Any("error", err),
			)

			return zero, quirkErr
		}

		backoff := spec.policy.Delay(attempt)
		attrs := []any{
			slog.String("quirk", spec.name),
			slog.Int("attempt", attempt+1),
			slog.Int("max_attempts", spec.policy.MaxAttempts),
			slog.Duration("backoff", backoff),
		}
		if graphErr != nil && graphErr.RequestID != "" {
			attrs = append(attrs, slog.String("request_id", graphErr.RequestID))
		}

		c.logger.Debug("retrying after documented graph quirk", attrs...)

		if sleepErr := retry.TimeSleep(ctx, backoff); sleepErr != nil {
			return zero, fmt.Errorf("graph: %s retry canceled: %w", spec.name, sleepErr)
		}
	}

	return zero, fmt.Errorf("graph: quirk retry exhausted without returning")
}

func buildQuirkRetryAttempt(attempt int, graphErr *GraphError) QuirkRetryAttempt {
	record := QuirkRetryAttempt{Attempt: attempt}
	if graphErr == nil {
		return record
	}

	record.StatusCode = graphErr.StatusCode
	record.GraphCode = graphErr.MostSpecificCode()
	if record.GraphCode == "" {
		record.GraphCode = graphErr.Code
	}
	record.RequestID = graphErr.RequestID

	return record
}

func isTransientDrivesDiscoveryError(err error) (*GraphError, bool) {
	if !errors.Is(err, ErrForbidden) {
		return nil, false
	}

	var graphErr *GraphError
	if !errors.As(err, &graphErr) || graphErr.StatusCode != http.StatusForbidden || !graphErr.HasCode("accessDenied") {
		return nil, false
	}

	return graphErr, true
}

func isExactRootChildrenCollectionPath(path string) bool {
	collectionPath, _, _ := strings.Cut(path, "?")
	segments := strings.Split(strings.Trim(collectionPath, "/"), "/")

	return len(segments) == 5 &&
		segments[0] == "drives" &&
		segments[1] != "" &&
		segments[2] == "items" &&
		segments[3] == "root" &&
		segments[4] == "children"
}

func isTransientRootChildrenError(err error) (*GraphError, bool) {
	return isTransientItemNotFoundError(err)
}

func isTransientDownloadMetadataError(err error) (*GraphError, bool) {
	return isTransientItemNotFoundError(err)
}

func isTransientSimpleUploadMtimeError(err error) (*GraphError, bool) {
	return isTransientItemNotFoundError(err)
}

func isTransientSimpleUploadCreateError(err error) (*GraphError, bool) {
	return isTransientItemNotFoundError(err)
}

func isTransientUploadSessionCreateError(err error) (*GraphError, bool) {
	return isTransientItemNotFoundError(err)
}

func isTransientCopyDestinationError(err error) (*GraphError, bool) {
	if !errors.Is(err, ErrNotFound) {
		return nil, false
	}

	var graphErr *GraphError
	if !errors.As(err, &graphErr) || graphErr.StatusCode != http.StatusNotFound {
		return nil, false
	}

	if !strings.Contains(strings.ToLower(graphErr.Message), "destination location") {
		return nil, false
	}

	return graphErr, true
}

func isTransientItemNotFoundError(err error) (*GraphError, bool) {
	if !errors.Is(err, ErrNotFound) {
		return nil, false
	}

	var graphErr *GraphError
	if !errors.As(err, &graphErr) || graphErr.StatusCode != http.StatusNotFound || !graphErr.HasCode("itemNotFound") {
		return nil, false
	}

	return graphErr, true
}
