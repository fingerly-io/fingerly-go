package fingerly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testSecretKey = "fly_sk_development_test"
	testRequestID = "0199a963-e400-7000-8000-000000000001"
)

// eventBody is a minimal event carrying every field the API always sends.
func eventBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"request_id":   testRequestID,
		"visitor_id":   "visitor-test",
		"occurred_at":  "2026-09-17T12:00:00Z",
		"state":        "enriched",
		"environment":  "development",
		"platform":     "web",
		"sdk_platform": "web",
		"sdk_version":  "0.1.0",
		"ip_network":   "203.0.113.0/24",
		"triggers":     []any{},
		"has_document": false,
	}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

// recorder is a stand-in API that records what reached it.
type recorder struct {
	requests []*http.Request
	// bodies holds each request's body, which the clone cannot carry because a
	// request body is a stream the handler consumes.
	bodies [][]byte
	server *httptest.Server
}

// newRecorder starts a server answering every request with handler.
func newRecorder(t *testing.T, handler http.HandlerFunc) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec.requests = append(rec.requests, r.Clone(context.Background()))
		rec.bodies = append(rec.bodies, body)
		handler(w, r)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

// body returns the body of the nth recorded request.
func (r *recorder) body(n int) string { return string(r.bodies[n]) }

// respondJSON answers every request with one JSON body and status.
func respondJSON(status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// errorBody is the API's structured error envelope.
func errorBody(code string, status int) map[string]any {
	return map[string]any{
		"error":      map[string]any{"code": code, "message": "Synthetic", "status": status},
		"request_id": "trace",
	}
}

// testClient points a client at the recorder's server.
func testClient(rec *recorder, opts ...Option) *Client {
	return New(testSecretKey, append([]Option{WithEndpoint(rec.server.URL)}, opts...)...)
}

func TestNewRejectsUnsupportedKeys(t *testing.T) {
	for _, secretKey := range []string{
		"not-a-fingerly-key",
		"fly_pk_production_test",
		"fly_px_development_test",
		"fly_mk_test",
		"fly_sk_unknown_test",
		"fly_sk_production_",
		"fly_sk_production_test\n",
	} {
		client := New(secretKey, WithEndpoint("https://custom.test"))
		if _, err := client.Events.Get(context.Background(), "id"); err == nil {
			t.Errorf("key %q was accepted", secretKey)
		}
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for name, option := range map[string]Option{
		"scheme":      WithEndpoint("file:///private"),
		"credentials": WithEndpoint("https://user:pass@host"),
		"query":       WithEndpoint("https://host?key=value"),
		"fragment":    WithEndpoint("https://host#fragment"),
		"version":     WithAPIVersion("v0"),
		"newline":     WithAPIVersion("v2\n"),
		"traversal":   WithAPIVersion("v1/../v2"),
		"timeout":     WithTimeout(0),
	} {
		client := New(testSecretKey, option)
		if _, err := client.Events.Get(context.Background(), "id"); err == nil {
			t.Errorf("option %s was accepted", name)
		}
	}
}

func TestConfigurationErrorsNeverReachTheNetwork(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
	client := New("not-a-key", WithEndpoint(rec.server.URL))
	if _, err := client.Events.Get(context.Background(), "id"); err == nil {
		t.Fatal("expected a configuration error")
	}
	if len(rec.requests) != 0 {
		t.Fatalf("sent %d requests despite an invalid key", len(rec.requests))
	}
}

func TestTheDefaultEndpointIsTheFingerlyAPI(t *testing.T) {
	client := New(testSecretKey)
	if want := "https://api.fingerly.io/api/v1"; client.baseURL != want {
		t.Errorf("base URL is %q, want %q", client.baseURL, want)
	}
}

func TestRequestCarriesTheKeyAndIdentifiesTheSDK(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
	if _, err := testClient(rec).Events.Get(context.Background(), testRequestID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	request := rec.requests[0]
	if got := request.Header.Get("X-API-Key"); got != testSecretKey {
		t.Errorf("X-API-Key is %q", got)
	}
	if got := request.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept is %q", got)
	}
	if got := request.Header.Get("User-Agent"); !strings.HasPrefix(got, "fingerly-go/") {
		t.Errorf("User-Agent is %q", got)
	}
	if request.URL.Path != "/api/v1/events/"+testRequestID {
		t.Errorf("path is %q", request.URL.Path)
	}
}

func TestRequestIDIsEscapedIntoOneSegment(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
	if _, err := testClient(rec).Events.Get(context.Background(), "id/a"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := rec.requests[0].URL.EscapedPath(); got != "/api/v1/events/id%2Fa" {
		t.Errorf("escaped path is %q", got)
	}
}

func TestAPIVersionAppliesToEveryRequest(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, eventBody(nil)))
	client := testClient(rec, WithAPIVersion("v2"))
	if _, err := client.Events.Get(context.Background(), testRequestID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := client.Events.List(context.Background(), &EventListParams{Limit: 10}); err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"/api/v2/events/" + testRequestID, "/api/v2/events"}
	for i, request := range rec.requests {
		if request.URL.Path != want[i] {
			t.Errorf("request %d path is %q, want %q", i, request.URL.Path, want[i])
		}
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	var reached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, elsewhere.URL, http.StatusFound)
	})
	_, err := testClient(rec).Events.Get(context.Background(), testRequestID)
	if err == nil {
		t.Fatal("expected the redirect to surface as an error")
	}
	if reached {
		t.Fatal("the secret key was replayed to another host")
	}
}

func TestAPIErrorCarriesStructuredMetadata(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusNotFound, errorBody("event_not_found", 404)))
	_, err := testClient(rec).Events.Get(context.Background(), "missing")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "event_not_found" {
		t.Errorf("status %d code %q", apiErr.Status, apiErr.Code)
	}
	if apiErr.RequestID != "trace" {
		t.Errorf("request id is %q", apiErr.RequestID)
	}
	if !errors.Is(err, ErrEventNotFound) {
		t.Error("a structured 404 should match ErrEventNotFound")
	}
}

func TestErrEventNotFoundIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"other code", http.StatusNotFound, "other"},
		{"pending", http.StatusNotFound, "event_pending"},
		{"other status", http.StatusGone, "event_not_found"},
	} {
		rec := newRecorder(t, respondJSON(tc.status, errorBody(tc.code, tc.status)))
		_, err := testClient(rec).Events.Get(context.Background(), "missing")
		if errors.Is(err, ErrEventNotFound) {
			t.Errorf("%s: matched ErrEventNotFound", tc.name)
		}
	}
}

func TestErrEventPendingIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"not found", http.StatusNotFound, "event_not_found"},
		{"other code", http.StatusNotFound, "other"},
		{"other status", http.StatusConflict, "event_pending"},
	} {
		rec := newRecorder(t, respondJSON(tc.status, errorBody(tc.code, tc.status)))
		_, err := testClient(rec).Events.Get(context.Background(), "missing")
		if errors.Is(err, ErrEventPending) {
			t.Errorf("%s: matched ErrEventPending", tc.name)
		}
	}
}

func TestErrorsNeverExposeTheBodyOrTheKey(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(testSecretKey))
	})
	_, err := testClient(rec).Events.Get(context.Background(), "missing")
	if err == nil || strings.Contains(err.Error(), testSecretKey) {
		t.Fatalf("error leaks the body or key: %v", err)
	}
	if want := "fingerly: API returned HTTP 502"; err.Error() != want {
		t.Errorf("error is %q, want %q", err.Error(), want)
	}
}

func TestSecretKeyIsRedactedFromMetadata(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusUnauthorized, map[string]any{
		"error": map[string]any{
			"code":    "unauthorized",
			"details": map[string]any{"presented": testSecretKey},
		},
		"request_id": "trace-" + testSecretKey,
	}))
	_, err := testClient(rec).Events.Get(context.Background(), "missing")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T", err)
	}
	if apiErr.RequestID != "trace-[redacted]" {
		t.Errorf("request id is %q", apiErr.RequestID)
	}
	details, _ := apiErr.Details.(map[string]any)
	if details["presented"] != "[redacted]" {
		t.Errorf("details are %v", apiErr.Details)
	}
}

func TestDeadlineCoversTheResponseBody(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // Headers sent, body never finishes.
	})
	_, err := testClient(rec, WithTimeout(50*time.Millisecond)).
		Events.Get(context.Background(), testRequestID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error is %v, want a deadline", err)
	}
}

func TestCallerCancellationIsHonoured(t *testing.T) {
	rec := newRecorder(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := testClient(rec).Events.Get(ctx, testRequestID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error is %v, want context.Canceled", err)
	}
}

func TestReadsAreNeverRetried(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusNotFound, errorBody("event_not_found", 404)))
	client := testClient(rec)
	if _, err := client.Events.Get(context.Background(), "id"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := client.Events.List(context.Background(), nil); err == nil {
		t.Fatal("expected an error")
	}
	if len(rec.requests) != 2 {
		t.Fatalf("made %d requests, want 2", len(rec.requests))
	}
}

func TestAPIErrorCarriesRetryAfter(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	for header, want := range map[string]any{"7": 7, "0": 0, future: 90, "": nil, "soon": nil, "-3": nil} {
		rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.Header().Set("X-Request-Id", "req-from-header")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("<html>not json</html>"))
		})
		_, err := testClient(rec).Events.Get(context.Background(), "id")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%q: error is %T", header, err)
		}
		switch want := want.(type) {
		case nil:
			if apiErr.RetryAfter != nil {
				t.Errorf("%q: RetryAfter %d, want nil", header, *apiErr.RetryAfter)
			}
		case int:
			// An HTTP date counts from now, so allow a second of drift.
			if apiErr.RetryAfter == nil || *apiErr.RetryAfter < want-1 || *apiErr.RetryAfter > want {
				t.Errorf("%q: RetryAfter %v, want %d", header, apiErr.RetryAfter, want)
			}
		}
		if apiErr.RequestID != "req-from-header" || strings.Contains(err.Error(), "html") {
			t.Errorf("%q: request id %q, message %q", header, apiErr.RequestID, err.Error())
		}
	}
}

func TestTransportFailuresAreConnectionErrors(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	_, err := New(testSecretKey, WithEndpoint(url)).Events.Get(context.Background(), "id")
	var connErr *ConnectionError
	if !errors.As(err, &connErr) || connErr.Timeout || errors.Is(err, ErrTimeout) {
		t.Fatalf("refused connection gave %T %v", err, err)
	}

	rec := newRecorder(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	_, err = testClient(rec, WithTimeout(50*time.Millisecond)).Events.Get(context.Background(), "id")
	if !errors.As(err, &connErr) || !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout gave %T %v", err, err)
	}
}

func TestOversizedAnswersAreRefused(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxResponseBody+1))
	})
	if _, err := testClient(rec).Events.Get(context.Background(), "id"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error is %v, want ErrResponseTooLarge", err)
	}
}
