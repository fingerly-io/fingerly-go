package fingerly

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestGetDecodesAnEventAndKeepsTheRawBody(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(map[string]any{
		"tag":           "checkout:8412",
		"suspect_score": 37,
		"suspect_level": "high",
		"triggers": []any{
			map[string]any{"signal": "tor", "weight": 14, "confidence": "high"},
		},
		"future_field": "kept",
	})))

	event, err := testClient(rec).Events.Get(context.Background(), testRequestID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if event.Tag != "checkout:8412" || event.SuspectLevel != "high" {
		t.Errorf("tag %q level %q", event.Tag, event.SuspectLevel)
	}
	if score, ok := event.Score(); !ok || score != 37 {
		t.Errorf("Score() = %d, %v", score, ok)
	}
	if len(event.Triggers) != 1 || event.Triggers[0].Signal != "tor" {
		t.Errorf("triggers are %v", event.Triggers)
	}
	want := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if !event.OccurredAt.Equal(want) {
		t.Errorf("occurred at %v, want %v", event.OccurredAt, want)
	}
	// A field added after this SDK shipped is still reachable through Raw.
	if !bytes.Contains(event.Raw, []byte("future_field")) {
		t.Error("Raw does not carry the undeclared field")
	}
}

func TestUnscoredEventIsDistinctFromZero(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(map[string]any{
		"suspect_score": nil,
	})))
	event, err := testClient(rec).Events.Get(context.Background(), testRequestID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if event.Scored() {
		t.Error("an unscored event reports itself as scored")
	}
	if score, ok := event.Score(); ok || score != 0 {
		t.Errorf("Score() = %d, %v; want 0, false", score, ok)
	}

	rec = newRecorder(t, respondJSON(http.StatusOK, eventBody(map[string]any{
		"suspect_score": 0,
	})))
	event, err = testClient(rec).Events.Get(context.Background(), testRequestID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !event.Scored() {
		t.Error("a zero score must still count as scored")
	}
}

func TestListEncodesEveryFilter(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{
		"rows": []any{}, "page": 2, "page_size": 20,
	}))
	from := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)

	page, err := testClient(rec).Events.List(context.Background(), &EventListParams{
		From: from, To: from.Add(24 * time.Hour),
		Page: 2, Limit: 20, Visitor: "visitor-test", Level: "high",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.Page != 2 || page.PageSize != 20 {
		t.Errorf("page %d size %d", page.Page, page.PageSize)
	}

	query := rec.requests[0].URL.Query()
	for name, want := range map[string]string{
		"from": "2026-09-01T08:30:00Z", "to": "2026-09-02T08:30:00Z",
		"page": "2", "limit": "20", "visitor": "visitor-test", "level": "high",
	} {
		if got := query.Get(name); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
}

func TestListOmitsUnsetFilters(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{
		"rows": []any{}, "page": 1, "page_size": 10,
	}))
	client := testClient(rec)
	for _, params := range []*EventListParams{nil, {}} {
		if _, err := client.Events.List(context.Background(), params); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	for i, request := range rec.requests {
		if request.URL.RawQuery != "" {
			t.Errorf("request %d carried a query: %q", i, request.URL.RawQuery)
		}
	}
}

func TestListSendsWindowBoundsInUTC(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{
		"rows": []any{}, "page": 1, "page_size": 10,
	}))
	berlin := time.FixedZone("CEST", 2*60*60)
	from := time.Date(2026, 9, 1, 10, 30, 0, 0, berlin)

	if _, err := testClient(rec).Events.List(context.Background(), &EventListParams{From: from}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := rec.requests[0].URL.Query().Get("from"); got != "2026-09-01T08:30:00Z" {
		t.Errorf("from is %q, want the same instant in UTC", got)
	}
}

// A missing event is a request id that was never issued or is past
// retention. It is reported at once, in one request, rather than waited
// through.
// A dot segment survives percent-escaping and is resolved by servers and
// proxies, so "" and "." would read the list and ".." a route above /events,
// secret key attached. Each is refused before anything is sent.
func TestGetRefusesIDsThatAreNotOneEvent(t *testing.T) {
	for _, id := range []string{"", ".", ".."} {
		rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
		event, err := testClient(rec).Events.Get(context.Background(), id)
		if !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("id %q: error is %v, want ErrInvalidRequestID", id, err)
		}
		if event != nil {
			t.Errorf("id %q: returned an event", id)
		}
		if len(rec.requests) != 0 {
			t.Errorf("id %q: made %d requests, want 0", id, len(rec.requests))
		}
	}
}

// An id that only contains dots among other characters is an ordinary id.
func TestGetSendsAnIDContainingADot(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
	if _, err := testClient(rec).Events.Get(context.Background(), "a.b"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.requests) != 1 {
		t.Fatalf("made %d requests, want 1", len(rec.requests))
	}
	if got := rec.requests[0].URL.EscapedPath(); got != "/api/v1/events/a.b" {
		t.Errorf("path is %q", got)
	}
}

func TestGetReportsAMissingEventWithoutRetrying(t *testing.T) {
	var calls int
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		respondJSON(http.StatusNotFound, errorBody("event_not_found", 404))(w, r)
	})

	_, err := testClient(rec).Events.Get(context.Background(), testRequestID)
	if !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("error is %v, want ErrEventNotFound", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1", calls)
	}
}

func TestGetPropagatesEveryOtherFailure(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500} {
		var calls int
		rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			respondJSON(status, errorBody("other", status))(w, r)
		})
		_, err := testClient(rec).Events.Get(context.Background(), testRequestID)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Errorf("status %d: error is %v", status, err)
		}
		if calls != 1 {
			t.Errorf("status %d: made %d requests, want 1", status, calls)
		}
	}
}

// A cancelled context stops the read, rather than the read outliving the
// caller that asked for it.
func TestGetStopsWhenTheCallerCancels(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := testClient(rec).Events.Get(ctx, testRequestID); err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the read ran for %v after being cancelled", elapsed)
	}
}

// A pending event (accepted, not recorded yet) is reported at once too, with
// the Retry-After seconds, and never matches ErrEventNotFound.
func TestGetReportsAPendingEventWithoutRetrying(t *testing.T) {
	var calls int
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		respondJSON(http.StatusNotFound, errorBody("event_pending", 404))(w, r)
	})

	_, err := testClient(rec).Events.Get(context.Background(), testRequestID)
	if !errors.Is(err, ErrEventPending) {
		t.Fatalf("error is %v, want ErrEventPending", err)
	}
	if errors.Is(err, ErrEventNotFound) {
		t.Error("a pending event matched ErrEventNotFound")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "event_pending" {
		t.Errorf("status %d code %q", apiErr.Status, apiErr.Code)
	}
	if apiErr.RetryAfter == nil || *apiErr.RetryAfter != 2 {
		t.Errorf("RetryAfter is %v, want 2", apiErr.RetryAfter)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1", calls)
	}
}
