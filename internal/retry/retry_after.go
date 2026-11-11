package retry

import (
	"errors"
	"math"
	"net/http"
	"net/textproto"
	"strconv"
	"time"
)

const (
	headerRetryAfter = "Retry-After"
	headerDate       = "Date"

	// longestRetryAfter is the longest wait a time.Duration can express. A
	// Retry-After beyond it saturates here instead of wrapping to a negative
	// or tiny wait. It is a representation limit, not a policy cap: the
	// server's wait is honored as given (R-2.10.7, R-2.10.14).
	longestRetryAfter = time.Duration(math.MaxInt64)

	// longestRetryAfterSeconds is the largest delay-seconds value that
	// converts to a time.Duration without overflow.
	longestRetryAfterSeconds = int64(longestRetryAfter / time.Second)
)

// ResponseRetryAfter returns the wait a 429 or 503 response asks for in its
// Retry-After field, or 0 when the response has another status or carries no
// usable value. A zero result means "no Retry-After": the caller applies the
// fallback it uses when the field is absent.
//
// An HTTP-date is measured against the response's own Date field, so a skew
// between the server's clock and this host's neither stretches nor shortens
// the wait. now stands in for the server's clock only when Date is missing
// or unparsable; that is the one case where this host's clock decides.
func ResponseRetryAfter(resp *http.Response, now time.Time) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}

	reference := now
	if serverNow, err := http.ParseTime(resp.Header.Get(headerDate)); err == nil {
		reference = serverNow
	}

	return parseRetryAfter(resp.Header.Get(headerRetryAfter), reference)
}

// parseRetryAfter reads a Retry-After field value in either form RFC 9110
// section 10.2.3 allows: delay-seconds, or an HTTP-date measured against
// reference. Surrounding whitespace is ignored and a value too long for a
// time.Duration saturates at longestRetryAfter.
//
// A value in neither form, a negative or zero delay, and a date not after
// reference all return 0 and are deliberately not reported as errors: an
// unusable Retry-After is the same as none, and the caller's own backoff
// takes over.
func parseRetryAfter(value string, reference time.Time) time.Duration {
	value = textproto.TrimString(value)
	if value == "" {
		return 0
	}

	// ParseInt returns the int64 bound with ErrRange, which delaySeconds
	// then saturates (positive) or ignores (negative).
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil || errors.Is(err, strconv.ErrRange) {
		return delaySeconds(seconds)
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0
	}

	// Time.Sub saturates at the Duration bounds, so a date centuries ahead
	// cannot wrap either.
	wait := retryAt.Sub(reference)
	if wait <= 0 {
		return 0
	}

	return wait
}

func delaySeconds(seconds int64) time.Duration {
	switch {
	case seconds <= 0:
		return 0
	case seconds > longestRetryAfterSeconds:
		return longestRetryAfter
	default:
		return time.Duration(seconds) * time.Second
	}
}
