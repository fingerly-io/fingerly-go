package fingerly

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrEventNotFound matches a read for an event that does not exist. One is
// readable as soon as identification answers, so this means a request id that
// was never issued rather than one that has not landed yet.
//
// Compare with errors.Is; the concrete error is always an *APIError.
var ErrEventNotFound = errors.New("fingerly: event not found")

// APIError is returned for any non-2xx response from the Fingerly API.
//
// The message carries only the HTTP status. Response bodies are never echoed,
// and any occurrence of the secret key in the metadata is redacted.
//
// Match it with errors.As:
//
//	var apiErr *fingerly.APIError
//	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
//		// ...
//	}
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Code is the stable machine-readable code, such as "event_not_found".
	Code string
	// Details carries extra context; its shape depends on Code.
	Details any
	// RequestID is the server-side trace id to quote in support requests.
	RequestID string
	// RetryAfter is how many seconds the API asked the caller to wait, from
	// its Retry-After header, or nil when it sent none. The SDK never retries
	// a read itself.
	RetryAfter *int
}

// Error implements error. It reports the status only, never the body.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("fingerly: API returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("fingerly: API returned HTTP %d", e.Status)
}

// Is reports whether this error matches a sentinel, so a caller can write
// errors.Is(err, fingerly.ErrEventNotFound) without unwrapping the status.
func (e *APIError) Is(target error) bool {
	return target == ErrEventNotFound && e.Status == 404 && e.Code == "event_not_found"
}

// ErrTimeout matches a read whose deadline passed, whether the caller's or
// WithTimeout's. Compare with errors.Is.
var ErrTimeout = errors.New("fingerly: request timed out")

// ErrResponseTooLarge is returned for an API answer over 4 MiB, which no
// event read produces. The body is not read past the limit.
var ErrResponseTooLarge = errors.New("fingerly: response body exceeds 4 MiB")

// ConnectionError reports a read that got no complete answer from the API:
// the connection was refused, reset or timed out, or the body broke off. Its
// message never carries the secret key.
//
// errors.Is(err, fingerly.ErrTimeout) is true when Timeout is, and the cause
// stays reachable, so errors.Is(err, context.Canceled) still works.
type ConnectionError struct {
	// Timeout reports whether a deadline passed.
	Timeout bool
	cause   error
}

// Error implements error.
func (e *ConnectionError) Error() string {
	if e.Timeout {
		return "fingerly: request timed out: " + e.cause.Error()
	}
	return "fingerly: request failed: " + e.cause.Error()
}

// Unwrap returns the cause.
func (e *ConnectionError) Unwrap() error { return e.cause }

// Is matches ErrTimeout for a timed-out read.
func (e *ConnectionError) Is(target error) bool { return target == ErrTimeout && e.Timeout }

// parseRetryAfter reads a Retry-After header as whole seconds: either a
// number of seconds or an HTTP date, which counts from now. Anything else is
// nil.
func parseRetryAfter(value string, now time.Time) *int {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return nil
		}
		return &seconds
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	seconds := int(at.Sub(now).Seconds() + 0.999)
	if seconds < 0 {
		seconds = 0
	}
	return &seconds
}
