package fingerly

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const consumedTime = "2026-11-17T10:00:00Z"

// consumePath is where a consume of the test event is sent.
const consumePath = "/api/v1/events/" + testRequestID + "/consume"

// literalConsumed is the 409 body in the shape R9 describes.
var literalConsumed = map[string]any{"code": "event_already_consumed", "consumed_at": consumedTime}

// envelopeConsumed is the 409 body in the standard error envelope.
var envelopeConsumed = map[string]any{
	"error": map[string]any{
		"code": "event_already_consumed", "message": "Synthetic", "status": 409,
		"details": []any{map[string]any{"field": "consumed_at", "issue": consumedTime, "location": "body"}},
	},
}

// freshEvent is an identifiable, scored event that occurred age ago.
func freshEvent(age time.Duration, overrides map[string]any) map[string]any {
	body := eventBody(map[string]any{
		"occurred_at":   time.Now().Add(-age).UTC().Format(time.RFC3339),
		"tag":           "checkout",
		"suspect_score": 12,
		"suspect_level": "low",
	})
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

// routes answers a GET with read and a POST with consume.
func routes(read, consume http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			consume(w, r)
			return
		}
		read(w, r)
	}
}

func consumedOK() http.HandlerFunc {
	return respondJSON(http.StatusOK, map[string]any{
		"request_id": testRequestID, "consumed_at": consumedTime, "first": true,
	})
}

// consumes counts the consume requests the recorder saw.
func (r *recorder) consumes() int {
	n := 0
	for _, request := range r.requests {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/consume") {
			n++
		}
	}
	return n
}

func TestConsumeSendsAnEmptyPostAndDecodesTheResult(t *testing.T) {
	rec := newRecorder(t, consumedOK())
	result, err := testClient(rec).Events.Consume(context.Background(), testRequestID)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if result.RequestID != testRequestID || result.ConsumedAt != consumedTime || !result.First {
		t.Errorf("result is %+v", result)
	}
	r := rec.requests[0]
	if r.Method != http.MethodPost || r.URL.Path != consumePath {
		t.Errorf("sent %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("X-API-Key") != testSecretKey {
		t.Error("the secret key was not sent")
	}
	if rec.body(0) != "" {
		t.Errorf("a consume sent a body %q", rec.body(0))
	}
}

func TestConsumeEscapesTheRequestID(t *testing.T) {
	rec := newRecorder(t, consumedOK())
	if _, err := testClient(rec).Events.Consume(context.Background(), "a/b?c"); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := rec.requests[0].URL.EscapedPath(); got != "/api/v1/events/a%2Fb%3Fc/consume" {
		t.Errorf("path is %q", got)
	}
}

func TestConsumeTwiceIsATypedError(t *testing.T) {
	for name, body := range map[string]map[string]any{"literal": literalConsumed, "envelope": envelopeConsumed} {
		t.Run(name, func(t *testing.T) {
			rec := newRecorder(t, respondJSON(http.StatusConflict, body))
			_, err := testClient(rec).Events.Consume(context.Background(), testRequestID)
			var consumed *EventAlreadyConsumedError
			if !errors.As(err, &consumed) {
				t.Fatalf("error is %T %v", err, err)
			}
			if consumed.ConsumedAt != consumedTime || consumed.Status != http.StatusConflict || consumed.Code != "event_already_consumed" {
				t.Errorf("error is %+v (%+v)", consumed, consumed.APIError)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
				t.Error("the error does not reach its *APIError")
			}
			if !errors.Is(err, ErrEventAlreadyConsumed) {
				t.Error("the error does not match ErrEventAlreadyConsumed")
			}
			if strings.Contains(err.Error(), consumedTime) {
				t.Errorf("the message echoes the body: %q", err.Error())
			}
			if len(rec.requests) != 1 {
				t.Errorf("a consume was retried: %d requests", len(rec.requests))
			}
		})
	}
}

func TestConsumeOfAnUnknownEventIsAnAPIError(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusNotFound, errorBody("event_not_found", 404)))
	_, err := testClient(rec).Events.Consume(context.Background(), testRequestID)
	var consumed *EventAlreadyConsumedError
	if errors.As(err, &consumed) {
		t.Fatal("a 404 became an already-consumed error")
	}
	if !errors.Is(err, ErrEventNotFound) {
		t.Errorf("error is %v", err)
	}
}

func TestConsumeRefusesABadRequestIDWithoutARequest(t *testing.T) {
	rec := newRecorder(t, consumedOK())
	for _, id := range []string{"", ".", ".."} {
		if _, err := testClient(rec).Events.Consume(context.Background(), id); !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("Consume(%q) = %v", id, err)
		}
	}
	if len(rec.requests) != 0 {
		t.Errorf("%d requests were sent", len(rec.requests))
	}
}

func TestVerifyPassesAndConsumesOnceAfterTheRead(t *testing.T) {
	rec := newRecorder(t, routes(respondJSON(http.StatusOK, freshEvent(10*time.Second, nil)), consumedOK()))
	tag := "checkout"
	result, err := testClient(rec).Events.Verify(context.Background(), testRequestID, &VerifyOptions{Tag: &tag})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.OK || result.Reason != "" || result.Event == nil || result.Event.RequestID != testRequestID {
		t.Errorf("result is %+v", result)
	}
	if len(rec.requests) != 2 || rec.requests[0].Method != http.MethodGet || rec.requests[1].URL.Path != consumePath {
		t.Fatalf("requests were %d", len(rec.requests))
	}
	if rec.consumes() != 1 {
		t.Errorf("%d consume requests", rec.consumes())
	}
}

func TestVerifyReasons(t *testing.T) {
	notFound := respondJSON(http.StatusNotFound, errorBody("event_not_found", 404))
	tests := []struct {
		name     string
		read     http.HandlerFunc
		consume  http.HandlerFunc
		tag      string
		want     VerifyReason
		consumes int
	}{
		{"not found", notFound, consumedOK(), "", VerifyNotFound, 0},
		{"too old", respondJSON(http.StatusOK, freshEvent(time.Hour, nil)), consumedOK(), "", VerifyTooOld, 0},
		{"unreadable occurred_at", respondJSON(http.StatusOK, freshEvent(0, map[string]any{"occurred_at": "yesterday"})), consumedOK(), "", VerifyTooOld, 0},
		{"missing occurred_at", respondJSON(http.StatusOK, freshEvent(0, map[string]any{"occurred_at": nil})), consumedOK(), "", VerifyTooOld, 0},
		{"tag mismatch", respondJSON(http.StatusOK, freshEvent(0, nil)), consumedOK(), "signup", VerifyTagMismatch, 0},
		{"missing tag", respondJSON(http.StatusOK, freshEvent(0, map[string]any{"tag": nil})), consumedOK(), "checkout", VerifyTagMismatch, 0},
		{"unidentifiable", respondJSON(http.StatusOK, freshEvent(0, map[string]any{"visitor_id": ""})), consumedOK(), "", VerifyUnidentifiable, 0},
		{"unscored", respondJSON(http.StatusOK, freshEvent(0, map[string]any{"suspect_score": nil})), consumedOK(), "", VerifyUnscored, 0},
		{"already consumed", respondJSON(http.StatusOK, freshEvent(0, nil)), respondJSON(http.StatusConflict, literalConsumed), "", VerifyAlreadyConsumed, 1},
		{"gone when consumed", respondJSON(http.StatusOK, freshEvent(0, nil)), notFound, "", VerifyNotFound, 1},
		// The first failing check wins.
		{"too old before the rest", respondJSON(http.StatusOK, freshEvent(time.Hour, map[string]any{
			"tag": "other", "visitor_id": "", "suspect_score": nil,
		})), consumedOK(), "checkout", VerifyTooOld, 0},
		{"tag before identity", respondJSON(http.StatusOK, freshEvent(0, map[string]any{
			"tag": "other", "visitor_id": "", "suspect_score": nil,
		})), consumedOK(), "checkout", VerifyTagMismatch, 0},
		{"identity before score", respondJSON(http.StatusOK, freshEvent(0, map[string]any{
			"visitor_id": "", "suspect_score": nil,
		})), consumedOK(), "", VerifyUnidentifiable, 0},
		// A clock running ahead never makes an event too old.
		{"future occurred_at passes", respondJSON(http.StatusOK, freshEvent(-time.Hour, nil)), consumedOK(), "", "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, routes(tt.read, tt.consume))
			var opts *VerifyOptions
			if tt.tag != "" {
				tag := tt.tag
				opts = &VerifyOptions{Tag: &tag}
			}
			result, err := testClient(rec).Events.Verify(context.Background(), testRequestID, opts)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if result.Reason != tt.want || result.OK != (tt.want == "") {
				t.Errorf("result is ok %v reason %q, want %q", result.OK, result.Reason, tt.want)
			}
			if (result.Event == nil) != (tt.want == VerifyNotFound) {
				t.Errorf("event is %v for reason %q", result.Event, result.Reason)
			}
			if rec.consumes() != tt.consumes {
				t.Errorf("%d consume requests, want %d", rec.consumes(), tt.consumes)
			}
		})
	}
}

func TestVerifyDefaultMaxAgeIs300Seconds(t *testing.T) {
	for _, tt := range []struct {
		age  time.Duration
		want VerifyReason
	}{{200 * time.Second, ""}, {400 * time.Second, VerifyTooOld}} {
		rec := newRecorder(t, routes(respondJSON(http.StatusOK, freshEvent(tt.age, nil)), consumedOK()))
		result, err := testClient(rec).Events.Verify(context.Background(), testRequestID, nil)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if result.Reason != tt.want {
			t.Errorf("an event %v old gave %q, want %q", tt.age, result.Reason, tt.want)
		}
	}
}

func TestVerifyHonoursMaxAgeSeconds(t *testing.T) {
	rec := newRecorder(t, routes(respondJSON(http.StatusOK, freshEvent(400*time.Second, nil)), consumedOK()))
	result, err := testClient(rec).Events.Verify(context.Background(), testRequestID, &VerifyOptions{MaxAgeSeconds: 600})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.OK {
		t.Errorf("reason %q", result.Reason)
	}
}

func TestVerifySkipConsumeNeverPosts(t *testing.T) {
	rec := newRecorder(t, routes(respondJSON(http.StatusOK, freshEvent(0, nil)), consumedOK()))
	result, err := testClient(rec).Events.Verify(context.Background(), testRequestID, &VerifyOptions{SkipConsume: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !result.OK || rec.consumes() != 0 || len(rec.requests) != 1 {
		t.Errorf("ok %v with %d requests", result.OK, len(rec.requests))
	}
}

func TestVerifyRaisesEveryOtherError(t *testing.T) {
	fresh := respondJSON(http.StatusOK, freshEvent(0, nil))
	tests := []struct {
		name    string
		read    http.HandlerFunc
		consume http.HandlerFunc
		match   func(error) bool
	}{
		{"pending read", respondJSON(http.StatusNotFound, errorBody("event_pending", 404)), consumedOK(),
			func(err error) bool { return errors.Is(err, ErrEventPending) }},
		{"rate limited read", respondJSON(http.StatusTooManyRequests, errorBody("rate_limited", 429)), consumedOK(),
			func(err error) bool { var e *APIError; return errors.As(err, &e) && e.Status == 429 }},
		{"failed consume", fresh, respondJSON(http.StatusInternalServerError, errorBody("internal", 500)),
			func(err error) bool { var e *APIError; return errors.As(err, &e) && e.Status == 500 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(t, routes(tt.read, tt.consume))
			result, err := testClient(rec).Events.Verify(context.Background(), testRequestID, nil)
			if result != nil || !tt.match(err) {
				t.Errorf("result %+v error %v", result, err)
			}
		})
	}
}

func TestVerifyRejectsInvalidInputWithoutARequest(t *testing.T) {
	rec := newRecorder(t, routes(respondJSON(http.StatusOK, freshEvent(0, nil)), consumedOK()))
	empty := ""
	for _, opts := range []*VerifyOptions{{MaxAgeSeconds: -1}, {Tag: &empty}} {
		if _, err := testClient(rec).Events.Verify(context.Background(), testRequestID, opts); !errors.Is(err, ErrInvalidVerifyOptions) {
			t.Errorf("Verify(%+v) = %v", opts, err)
		}
	}
	for _, id := range []string{"", ".", ".."} {
		if _, err := testClient(rec).Events.Verify(context.Background(), id, nil); !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("Verify(%q) = %v", id, err)
		}
	}
	if len(rec.requests) != 0 {
		t.Errorf("%d requests were sent", len(rec.requests))
	}
}
