package fingerly

import (
	"errors"
	"fmt"
)

// ErrEventNotFound matches the one retryable read failure: the event has not
// been archived yet. Events.WaitFor polls on exactly this and nothing else.
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
