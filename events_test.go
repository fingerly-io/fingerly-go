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

func TestWaitForPollsThroughStructuredMisses(t *testing.T) {
	var calls int
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			respondJSON(http.StatusNotFound, errorBody("event_not_found", 404))(w, r)
			return
		}
		respondJSON(http.StatusOK, eventBody(nil))(w, r)
	})

	event, err := testClient(rec).Events.WaitFor(
		context.Background(), testRequestID, WithPollInterval(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if event.RequestID != testRequestID {
		t.Errorf("request id is %q", event.RequestID)
	}
	if calls != 3 {
		t.Errorf("polled %d times, want 3", calls)
	}
}

func TestWaitForBoundsAPermanentlyMissingEvent(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusNotFound, errorBody("event_not_found", 404)))
	_, err := testClient(rec).Events.WaitFor(
		context.Background(), testRequestID,
		WithWaitTimeout(60*time.Millisecond), WithPollInterval(10*time.Millisecond),
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error is %v, want a deadline", err)
	}
	// The reason it gave up is still reachable.
	if !errors.Is(err, ErrEventNotFound) {
		t.Error("the timeout should still report why it kept waiting")
	}
}

func TestWaitForDoesNotRetryOtherFailures(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500} {
		var calls int
		rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			respondJSON(status, errorBody("other", status))(w, r)
		})
		_, err := testClient(rec).Events.WaitFor(
			context.Background(), testRequestID, WithPollInterval(time.Millisecond),
		)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Errorf("status %d: error is %v", status, err)
		}
		if calls != 1 {
			t.Errorf("status %d: polled %d times, want 1", status, calls)
		}
	}
}

func TestWaitForStopsWhenTheCallerCancels(t *testing.T) {
	var calls int
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		respondJSON(http.StatusNotFound, errorBody("event_not_found", 404))(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := testClient(rec).Events.WaitFor(
		ctx, testRequestID,
		WithWaitTimeout(5*time.Second), WithPollInterval(time.Hour),
	)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v despite a long interval", elapsed)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("polled %d times, want 1", calls)
	}
}

func TestWaitForRejectsUnusableOptions(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
	client := testClient(rec)
	for name, option := range map[string]WaitOption{
		"timeout":  WithWaitTimeout(0),
		"interval": WithPollInterval(-time.Second),
	} {
		if _, err := client.Events.WaitFor(context.Background(), testRequestID, option); err == nil {
			t.Errorf("%s: option was accepted", name)
		}
	}
	if len(rec.requests) != 0 {
		t.Error("an unusable option still reached the network")
	}
}
