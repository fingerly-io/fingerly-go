package fingerly

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrEventNotFound matches a read for an event that does not exist: the
// request id was never issued for this key's environment, or the event is
// past retention. An event that is still being recorded matches
// ErrEventPending instead.
//
// Compare with errors.Is; the concrete error is always an *APIError.
var ErrEventNotFound = errors.New("fingerly: event not found")

// ErrEventPending matches a read for an event whose request id was accepted
// but which is not recorded yet (404 event_pending). It is not a verdict: try
// again after APIError.RetryAfter seconds, or verify asynchronously. The SDK
// never retries on its own.
//
// Compare with errors.Is; the concrete error is always an *APIError.
var ErrEventPending = errors.New("fingerly: event pending")

// ErrInvalidRequestID is returned by EventsService.Get, before any request is
// made, for a request id that cannot name one event: the empty string, "."
// or "..". The id usually comes from the browser, so treat it as bad input.
var ErrInvalidRequestID = errors.New("fingerly: request id is not a Fingerly request id")

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
	// AppleStatus is Apple's HTTP status on an apple_rejected answer from a
	// DeviceCheck call, read from the details entry named apple_status, and 0
	// on any other error.
	AppleStatus int

	// consumedAt is when the event was first consumed, on an
	// event_already_consumed answer. EventAlreadyConsumedError surfaces it.
	consumedAt string
}

// Error implements error. It reports the status only, never the body.
func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("fingerly: API returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("fingerly: API returned HTTP %d", e.Status)
}

// Is reports whether this error matches a sentinel, so a caller can write
// errors.Is(err, fingerly.ErrEventNotFound) or
// errors.Is(err, fingerly.ErrDeviceCheckSpent) without unwrapping the status.
func (e *APIError) Is(target error) bool {
	want, ok := sentinelAnswers[target]
	return ok && e.Status == want.status && e.Code == want.code
}

// sentinelAnswers maps each sentinel to the status and code it matches.
var sentinelAnswers = map[error]struct {
	status int
	code   string
}{
	ErrEventNotFound:          {http.StatusNotFound, "event_not_found"},
	ErrEventPending:           {http.StatusNotFound, "event_pending"},
	ErrDeviceCheckNotHeld:     {http.StatusNotFound, "device_check_not_held"},
	ErrDeviceCheckSpent:       {http.StatusConflict, "device_check_spent"},
	ErrDeviceCheckBusy:        {http.StatusConflict, "device_check_busy"},
	ErrCredentialUnavailable:  {http.StatusConflict, "credential_unavailable"},
	ErrBitsRequired:           {http.StatusUnprocessableEntity, "bits_required"},
	ErrAppleRejected:          {http.StatusUnprocessableEntity, codeAppleRejected},
	ErrDeviceCheckUnavailable: {http.StatusServiceUnavailable, "device_check_unavailable"},
	ErrEventAlreadyConsumed:   {http.StatusConflict, codeEventAlreadyConsumed},
}

const (
	codeAppleRejected        = "apple_rejected"
	codeEventAlreadyConsumed = "event_already_consumed"
)

// ErrEventAlreadyConsumed matches a consume of an event that was already
// consumed (409 event_already_consumed). Compare with errors.Is; the concrete
// error is an *EventAlreadyConsumedError, which also matches *APIError with
// errors.As.
var ErrEventAlreadyConsumed = errors.New("fingerly: event already consumed")

// EventAlreadyConsumedError is returned by EventsService.Consume for an event
// that was already consumed (409 event_already_consumed). It carries
// everything the *APIError does, which errors.As also reaches, plus when the
// event was first consumed.
//
//	var consumed *fingerly.EventAlreadyConsumedError
//	if errors.As(err, &consumed) {
//		// consumed.ConsumedAt, consumed.Status (409), consumed.Code
//	}
type EventAlreadyConsumedError struct {
	*APIError
	// ConsumedAt is when the event was first consumed, RFC 3339 as the API
	// sent it, or "" when the answer did not say.
	ConsumedAt string
}

// Unwrap returns the underlying *APIError.
func (e *EventAlreadyConsumedError) Unwrap() error { return e.APIError }

// ErrInvalidVerifyOptions is returned by EventsService.Verify, before any
// request is made, for a negative MaxAgeSeconds or an empty Tag. Compare with
// errors.Is.
var ErrInvalidVerifyOptions = errors.New("fingerly: invalid verify options")

// The DeviceCheck answers, one sentinel per code. Compare with errors.Is; the
// concrete error is always an *APIError, except ErrBitsRequired, which a
// write naming neither bit returns before anything is sent.
var (
	// ErrDeviceCheckNotHeld (404 device_check_not_held): no token is held for
	// that event. Not a verdict: decide without the bits.
	ErrDeviceCheckNotHeld = errors.New("fingerly: no device check token is held for that event")
	// ErrDeviceCheckSpent (409 device_check_spent): the token was already
	// used, or this call is not the request the token was first used for.
	ErrDeviceCheckSpent = errors.New("fingerly: the event's device check token was already used")
	// ErrDeviceCheckBusy (409 device_check_busy): another call is using the
	// token right now.
	ErrDeviceCheckBusy = errors.New("fingerly: the event's device check token is being used")
	// ErrCredentialUnavailable (409 credential_unavailable): the project's
	// Apple key is missing, refused by Apple, or has DeviceCheck off. The
	// token is kept.
	ErrCredentialUnavailable = errors.New("fingerly: the project's Apple credential is unavailable")
	// ErrBitsRequired (422 bits_required): a write named neither bit.
	ErrBitsRequired = errors.New("fingerly: name bit0, bit1 or both")
	// ErrAppleRejected (422 apple_rejected): Apple rejected the token, which
	// is now used. APIError.AppleStatus holds Apple's HTTP status.
	ErrAppleRejected = errors.New("fingerly: Apple rejected the device check request")
	// ErrDeviceCheckUnavailable (503 device_check_unavailable): Apple or
	// Fingerly's storage could not answer, after the SDK's own retries. The
	// token is kept; retry the same call within the hour.
	ErrDeviceCheckUnavailable = errors.New("fingerly: device check is unavailable just now")
)

// ErrTimeout matches a read whose deadline passed, whether the caller's or
// WithTimeout's. Compare with errors.Is.
var ErrTimeout = errors.New("fingerly: request timed out")

// ErrResponseTooLarge is returned for an API answer over 4 MiB, which no
// event read produces. The body is not read past the limit.
var ErrResponseTooLarge = errors.New("fingerly: response body exceeds 4 MiB")

// ConnectionError reports a read that got no complete answer from the API:
// the connection was refused, reset or timed out, or the body broke off. Its
// message names neither the secret key nor the endpoint; the transport error
// stays reachable through errors.Unwrap for a caller that wants it.
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
	// A transport error names the URL, host and port, so only the kind of
	// failure is printed.
	if e.Timeout {
		return "fingerly: request timed out"
	}
	return "fingerly: request failed"
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
