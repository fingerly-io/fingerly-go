package fingerly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DefaultVerifyMaxAge is the age, in seconds, past which Verify reports an
// event as too old when VerifyOptions.MaxAgeSeconds is zero.
const DefaultVerifyMaxAge = 300

// VerifyReason says which check of EventsService.Verify failed. Its values
// are the API's wire strings; it is "" when every check passed.
type VerifyReason string

// The reasons Verify reports, in the order it checks them.
const (
	// VerifyReasonOK: every check passed. It is the zero value.
	VerifyReasonOK VerifyReason = ""
	// VerifyNotFound: the event does not exist, or was gone when consumed.
	// VerifyResult.Event is nil.
	VerifyNotFound VerifyReason = "not_found"
	// VerifyTooOld: the event is older than MaxAgeSeconds, or its
	// occurred_at is missing or unreadable.
	VerifyTooOld VerifyReason = "too_old"
	// VerifyTagMismatch: a Tag was asked for and the event's tag differs.
	VerifyTagMismatch VerifyReason = "tag_mismatch"
	// VerifyUnidentifiable: the event has no visitor identifier.
	VerifyUnidentifiable VerifyReason = "unidentifiable"
	// VerifyUnscored: the event has no suspect score.
	VerifyUnscored VerifyReason = "unscored"
	// VerifyAlreadyConsumed: every other check passed, but the event was
	// already consumed, so it already backed a decision.
	VerifyAlreadyConsumed VerifyReason = "already_consumed"
)

// VerifyOptions tunes EventsService.Verify. A nil *VerifyOptions, or a zero
// field within it, takes the default.
type VerifyOptions struct {
	// MaxAgeSeconds is how old, in seconds, the event may be. Zero means
	// DefaultVerifyMaxAge (300); a negative value is an error.
	MaxAgeSeconds int
	// Tag, when not nil, is the tag the event must carry exactly. Nil skips
	// the tag check. An empty tag is an error, because it would match an
	// untagged event.
	Tag *string
	// SkipConsume leaves the event unconsumed after the checks pass. By
	// default Verify consumes it, so one event backs one decision.
	SkipConsume bool
}

// VerifyResult is the outcome of EventsService.Verify.
type VerifyResult struct {
	// OK is true when every check passed and, unless skipped, the event was
	// consumed by this call.
	OK bool
	// Reason is the first check that failed, or "" when OK.
	Reason VerifyReason
	// Event is the event read. It is nil only for VerifyNotFound.
	Event *Event
}

// ConsumeResult is the answer to a first consume of an event.
type ConsumeResult struct {
	RequestID string `json:"request_id"`
	// ConsumedAt is when the event was consumed, RFC 3339 as the API sent it.
	ConsumedAt string `json:"consumed_at"`
	// First is true on the call that consumed the event.
	First bool `json:"first"`
}

// Verify reads an event and checks, in this order, stopping at the first
// failure, that it exists, is no older than MaxAgeSeconds, carries Tag when
// one is given, is identifiable and is scored. When every check passes it
// consumes the event, unless SkipConsume is set, so one event backs one
// decision; an event consumed before reports VerifyAlreadyConsumed.
//
// A failed check is a VerifyResult, not an error. Any other failure, including
// ErrEventPending, a rate limit, a server error or a timeout, is returned
// unchanged as the error. Verify never reads the score's value, the level or
// the triggers: that policy stays with the caller.
//
// A negative MaxAgeSeconds or an empty Tag returns ErrInvalidVerifyOptions,
// and an invalid request id ErrInvalidRequestID, without sending anything.
func (s *EventsService) Verify(ctx context.Context, requestID string, opts *VerifyOptions) (*VerifyResult, error) {
	var options VerifyOptions
	if opts != nil {
		options = *opts
	}
	if options.MaxAgeSeconds < 0 {
		return nil, fmt.Errorf("%w: MaxAgeSeconds must be positive", ErrInvalidVerifyOptions)
	}
	if options.MaxAgeSeconds == 0 {
		options.MaxAgeSeconds = DefaultVerifyMaxAge
	}
	if options.Tag != nil && *options.Tag == "" {
		return nil, fmt.Errorf("%w: Tag must not be empty", ErrInvalidVerifyOptions)
	}
	if !validRequestID(requestID) {
		return nil, ErrInvalidRequestID
	}

	body, err := s.client.get(ctx, eventPath(requestID))
	if errors.Is(err, ErrEventNotFound) {
		return &VerifyResult{Reason: VerifyNotFound}, nil
	}
	if err != nil {
		return nil, err
	}
	event, err := decodeEventForVerify(body)
	if err != nil {
		return nil, err
	}

	maxAge := time.Duration(options.MaxAgeSeconds) * time.Second
	if event.OccurredAt.IsZero() || time.Since(event.OccurredAt) > maxAge {
		return &VerifyResult{Reason: VerifyTooOld, Event: event}, nil
	}
	if options.Tag != nil && event.Tag != *options.Tag {
		return &VerifyResult{Reason: VerifyTagMismatch, Event: event}, nil
	}
	if event.VisitorID == "" {
		return &VerifyResult{Reason: VerifyUnidentifiable, Event: event}, nil
	}
	if event.SuspectScore == nil {
		return &VerifyResult{Reason: VerifyUnscored, Event: event}, nil
	}

	if !options.SkipConsume {
		_, err := s.Consume(ctx, requestID)
		switch {
		case errors.Is(err, ErrEventAlreadyConsumed):
			return &VerifyResult{Reason: VerifyAlreadyConsumed, Event: event}, nil
		case errors.Is(err, ErrEventNotFound):
			return &VerifyResult{Reason: VerifyNotFound}, nil
		case err != nil:
			return nil, err
		}
	}
	return &VerifyResult{OK: true, Event: event}, nil
}

// Consume marks an event as used, once, so it can back only one decision.
// It is a POST with no body and is never retried.
//
// Consuming an event a second time returns an *EventAlreadyConsumedError,
// which matches ErrEventAlreadyConsumed with errors.Is and carries when it
// was first consumed. An unknown event matches ErrEventNotFound. An invalid
// request id returns ErrInvalidRequestID without sending anything.
func (s *EventsService) Consume(ctx context.Context, requestID string) (*ConsumeResult, error) {
	if !validRequestID(requestID) {
		return nil, ErrInvalidRequestID
	}
	body, err := s.client.send(ctx, http.MethodPost, eventPath(requestID)+"/consume", nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && errors.Is(apiErr, ErrEventAlreadyConsumed) {
			return nil, &EventAlreadyConsumedError{APIError: apiErr, ConsumedAt: apiErr.consumedAt}
		}
		return nil, err
	}
	var result ConsumeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("fingerly: decode consume result: %w", err)
	}
	return &result, nil
}

func eventPath(requestID string) string {
	return "/events/" + url.PathEscape(requestID)
}

// eventFields is Event without its methods, so a wrapper can shadow one of
// its fields while decoding.
type eventFields Event

// decodeEventForVerify decodes an event as Get does, except that an
// occurred_at which is not RFC 3339 decodes as the zero time instead of
// failing the read, so Verify reports it as too old.
func decodeEventForVerify(body json.RawMessage) (*Event, error) {
	var wire struct {
		eventFields
		OccurredAt json.RawMessage `json:"occurred_at"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("fingerly: decode event: %w", err)
	}
	event := Event(wire.eventFields)
	var occurredAt time.Time
	if json.Unmarshal(wire.OccurredAt, &occurredAt) == nil {
		event.OccurredAt = occurredAt
	}
	event.Raw = body
	return &event, nil
}
