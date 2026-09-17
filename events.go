package fingerly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultWaitTimeout  = 10 * time.Second
	defaultPollInterval = 250 * time.Millisecond
)

// EventsService reads events stored for the organization and environment of
// the client's secret key. Reach it as client.Events.
type EventsService struct {
	client *Client
}

// Get fetches one event by request id.
//
// An event that has not been archived yet returns an error matching
// ErrEventNotFound; use WaitFor right after identifying a visitor.
func (s *EventsService) Get(ctx context.Context, requestID string) (*Event, error) {
	body, err := s.client.get(ctx, "/events/"+url.PathEscape(requestID))
	if err != nil {
		return nil, err
	}
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("fingerly: decode event: %w", err)
	}
	event.Raw = body
	return &event, nil
}

// List returns events within the retention window, newest first.
//
// A nil params, or a zero field within it, leaves that filter unset so the
// server applies its own default.
func (s *EventsService) List(ctx context.Context, params *EventListParams) (*EventPage, error) {
	body, err := s.client.get(ctx, "/events"+params.query())
	if err != nil {
		return nil, err
	}
	var page EventPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("fingerly: decode events: %w", err)
	}
	page.Raw = body
	return &page, nil
}

// WaitOption configures WaitFor.
type WaitOption func(*waitConfig)

type waitConfig struct {
	timeout  time.Duration
	interval time.Duration
}

// WithWaitTimeout bounds the total time spent polling. The default is 10
// seconds. A deadline already on the caller's context still applies, whichever
// is sooner.
func WithWaitTimeout(timeout time.Duration) WaitOption {
	return func(c *waitConfig) { c.timeout = timeout }
}

// WithPollInterval sets the delay between polls. The default is 250ms.
func WithPollInterval(interval time.Duration) WaitOption {
	return func(c *waitConfig) { c.interval = interval }
}

// WaitFor polls until a newly identified event becomes readable.
//
// Identification answers before the event is archived, so reach for this only
// when you have just identified a visitor. Only ErrEventNotFound is retried;
// every other failure is returned at once.
//
// When the event is still missing at the deadline, the returned error wraps
// context.DeadlineExceeded and the last ErrEventNotFound, so either can be
// matched with errors.Is.
func (s *EventsService) WaitFor(
	ctx context.Context, requestID string, opts ...WaitOption,
) (*Event, error) {
	config := waitConfig{timeout: defaultWaitTimeout, interval: defaultPollInterval}
	for _, apply := range opts {
		apply(&config)
	}
	if _, err := positiveDuration(config.timeout, "WaitTimeout"); err != nil {
		return nil, err
	}
	if _, err := positiveDuration(config.interval, "PollInterval"); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, config.timeout)
	defer cancel()

	ticker := time.NewTicker(config.interval)
	defer ticker.Stop()

	// lastMiss keeps the reason polling continued, so the error that ends it
	// says why rather than only that a deadline passed.
	var lastMiss error
	for {
		event, err := s.Get(ctx, requestID)
		switch {
		case err == nil:
			return event, nil
		case errors.Is(err, ErrEventNotFound):
			lastMiss = err
		case ctx.Err() == nil:
			// A genuine failure, not this deadline cutting a poll short.
			return nil, err
		}

		if ctx.Err() != nil {
			return nil, stoppedWaiting(ctx, requestID, lastMiss)
		}
		select {
		case <-ctx.Done():
			return nil, stoppedWaiting(ctx, requestID, lastMiss)
		case <-ticker.C:
		}
	}
}

// stoppedWaiting explains why polling ended, keeping both the context error
// and the last miss matchable with errors.Is.
func stoppedWaiting(ctx context.Context, requestID string, lastMiss error) error {
	reason := ctx.Err()
	if lastMiss != nil {
		reason = errors.Join(reason, lastMiss)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("fingerly: cancelled while waiting for event %q: %w", requestID, reason)
	}
	return fmt.Errorf("fingerly: timed out waiting for event %q: %w", requestID, reason)
}

// query renders the filters as a URL query, omitting every unset one.
func (p *EventListParams) query() string {
	if p == nil {
		return ""
	}
	values := url.Values{}
	if !p.From.IsZero() {
		values.Set("from", p.From.UTC().Format(time.RFC3339))
	}
	if !p.To.IsZero() {
		values.Set("to", p.To.UTC().Format(time.RFC3339))
	}
	if p.Page > 0 {
		values.Set("page", strconv.Itoa(p.Page))
	}
	if p.Limit > 0 {
		values.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Visitor != "" {
		values.Set("visitor", p.Visitor)
	}
	if p.Level != "" {
		values.Set("level", p.Level)
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}
