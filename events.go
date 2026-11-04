package fingerly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// EventsService reads events stored for the organization and environment of
// the client's secret key. Reach it as client.Events.
type EventsService struct {
	client *Client
}

// Get fetches one event by request id.
//
// An error matching ErrEventPending means the request id was accepted but the
// event is not recorded yet; APIError.RetryAfter says when to try again. One
// matching ErrEventNotFound means it was never issued or is past retention.
// Neither is retried.
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
