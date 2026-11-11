package retry_test

import (
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// The HTTP-date rows are measured against serverDate, the response's own Date
// field. The local clock (retryAfterLocalNow) runs ten minutes ahead of it, so
// a row that used the local clock where it should use Date gives a visibly
// different wait.
const (
	serverDate             = "Wed, 11 Nov 2026 10:00:00 GMT"
	ninetySecondsIMF       = "Wed, 11 Nov 2026 10:01:30 GMT"
	ninetySecondsRFC850    = "Wednesday, 11-Nov-26 10:01:30 GMT"
	ninetySecondsANSIC     = "Wed Nov 11 10:01:30 2026"
	twelveMinutesIMF       = "Wed, 11 Nov 2026 10:12:00 GMT"
	oneMinuteBeforeIMF     = "Wed, 11 Nov 2026 09:59:00 GMT"
	oneYearAheadIMF        = "Thu, 11 Nov 2027 10:00:00 GMT"
	beyondDurationIMF      = "Fri, 31 Dec 9999 23:59:59 GMT"
	beyondInt64Seconds     = "99999999999999999999"
	beyondDurationSeconds  = "9223372037"
	largestDurationSeconds = "9223372036"
	retryAfterField        = "Retry-After"
	dateField              = "Date"
)

func retryAfterLocalNow() time.Time {
	return time.Date(2026, time.November, 11, 10, 10, 0, 0, time.UTC)
}

// Validates: R-6.8.1, R-6.8.6, R-2.10.14
func TestResponseRetryAfter(t *testing.T) {
	t.Parallel()

	const (
		throttled   = http.StatusTooManyRequests
		unavailable = http.StatusServiceUnavailable
		longestWait = time.Duration(math.MaxInt64)
		oneYear     = 365 * 24 * time.Hour
	)

	tests := []struct {
		name       string
		status     int
		retryAfter string // empty leaves the field out
		date       string // empty leaves the field out
		want       time.Duration
	}{
		{name: "seconds", status: throttled, retryAfter: "5", want: 5 * time.Second},
		{name: "surrounding spaces are trimmed", status: throttled, retryAfter: " 5 \t", want: 5 * time.Second},
		{name: "a leading plus is still read", status: throttled, retryAfter: "+5", want: 5 * time.Second},
		{name: "zero asks for no wait", status: throttled, retryAfter: "0", want: 0},
		{name: "negative is ignored", status: throttled, retryAfter: "-1", want: 0},
		{name: "garbage is ignored", status: throttled, retryAfter: "abc", want: 0},
		{name: "a fraction is ignored", status: throttled, retryAfter: "1.5", want: 0},
		{name: "absent", status: throttled, want: 0},
		{name: "only spaces", status: throttled, retryAfter: "   ", want: 0},
		{name: "largest seconds a duration holds", status: throttled, retryAfter: largestDurationSeconds, want: 9223372036 * time.Second},
		{name: "seconds beyond a duration saturate", status: throttled, retryAfter: beyondDurationSeconds, want: longestWait},
		{name: "seconds beyond int64 saturate", status: throttled, retryAfter: beyondInt64Seconds, want: longestWait},
		{name: "negative beyond int64 is ignored", status: throttled, retryAfter: "-" + beyondInt64Seconds, want: 0},
		{name: "IMF-fixdate is measured against Date", status: throttled, retryAfter: ninetySecondsIMF, date: serverDate, want: 90 * time.Second},
		{name: "RFC 850 date", status: throttled, retryAfter: ninetySecondsRFC850, date: serverDate, want: 90 * time.Second},
		{name: "ANSI C asctime date", status: throttled, retryAfter: ninetySecondsANSIC, date: serverDate, want: 90 * time.Second},
		{name: "Date wins over a skewed local clock", status: throttled, retryAfter: twelveMinutesIMF, date: serverDate, want: 12 * time.Minute},
		{name: "no Date falls back to the local clock", status: throttled, retryAfter: twelveMinutesIMF, want: 2 * time.Minute},
		{name: "unparsable Date falls back to the local clock", status: throttled, retryAfter: twelveMinutesIMF, date: "yesterday", want: 2 * time.Minute},
		{name: "a past date asks for no wait", status: throttled, retryAfter: oneMinuteBeforeIMF, date: serverDate, want: 0},
		{name: "a date equal to Date asks for no wait", status: throttled, retryAfter: serverDate, date: serverDate, want: 0},
		{name: "a year ahead is honored without a cap", status: throttled, retryAfter: oneYearAheadIMF, date: serverDate, want: oneYear},
		{name: "a date beyond a duration saturates", status: throttled, retryAfter: beyondDurationIMF, date: serverDate, want: longestWait},
		{name: "503 carries Retry-After too", status: unavailable, retryAfter: "5", want: 5 * time.Second},
		{name: "a success ignores Retry-After", status: http.StatusOK, retryAfter: "5", want: 0},
		{name: "a 500 ignores Retry-After", status: http.StatusInternalServerError, retryAfter: "5", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			if tt.retryAfter != "" {
				resp.Header.Set(retryAfterField, tt.retryAfter)
			}
			if tt.date != "" {
				resp.Header.Set(dateField, tt.date)
			}

			assert.Equal(t, tt.want, retry.ResponseRetryAfter(resp, retryAfterLocalNow()))
		})
	}
}
