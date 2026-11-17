package fingerly

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// noDeviceCheckDelay removes the waits between DeviceCheck attempts.
func noDeviceCheckDelay(t *testing.T) {
	t.Helper()
	saved := deviceCheckRetryDelays
	deviceCheckRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { deviceCheckRetryDelays = saved })
}

func TestDeviceCheckQuerySendsAnEmptyPostWithTheSecretKey(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{
		"found": true, "bit0": true, "bit1": false, "last_update": "2026-10",
	}))
	bits, err := testClient(rec).DeviceCheck.Query(context.Background(), testRequestID)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !bits.Found || !bits.Bit0 || bits.Bit1 || bits.LastUpdate != "2026-10" {
		t.Errorf("bits are %+v", bits)
	}
	r := rec.requests[0]
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/events/"+testRequestID+"/device-check/query" {
		t.Errorf("sent %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("X-API-Key") != testSecretKey {
		t.Error("the secret key was not sent")
	}
	if rec.body(0) != "" || r.Header.Get("Content-Type") != "" {
		t.Errorf("a query sent a body %q (%s)", rec.body(0), r.Header.Get("Content-Type"))
	}
}

func TestDeviceCheckWriteSendsOnlyTheNamedBits(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{"bit1": false}))
	result, err := testClient(rec).DeviceCheck.Write(context.Background(), testRequestID, DeviceCheckWrite{Bit1: Bool(false)})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if result.Bit0 != nil || result.Bit1 == nil || *result.Bit1 {
		t.Errorf("result is %+v", result)
	}
	r := rec.requests[0]
	if r.Method != http.MethodPut || r.URL.Path != "/api/v1/events/"+testRequestID+"/device-check" {
		t.Errorf("sent %s %s", r.Method, r.URL.Path)
	}
	if rec.body(0) != `{"bit1":false}` || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("sent %q (%s)", rec.body(0), r.Header.Get("Content-Type"))
	}
}

func TestDeviceCheckWriteWithNoBitSendsNothing(t *testing.T) {
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{}))
	if _, err := testClient(rec).DeviceCheck.Write(context.Background(), testRequestID, DeviceCheckWrite{}); !errors.Is(err, ErrBitsRequired) {
		t.Fatalf("no bits: %v", err)
	}
	for _, id := range []string{"", ".", ".."} {
		if _, err := testClient(rec).DeviceCheck.Query(context.Background(), id); !errors.Is(err, ErrInvalidRequestID) {
			t.Errorf("request id %q: %v", id, err)
		}
	}
	if len(rec.requests) != 0 {
		t.Fatalf("%d requests were sent", len(rec.requests))
	}
}

func TestDeviceCheckErrorsMatchTheirCodes(t *testing.T) {
	noDeviceCheckDelay(t)
	for _, tc := range []struct {
		status   int
		code     string
		sentinel error
	}{
		{http.StatusNotFound, "device_check_not_held", ErrDeviceCheckNotHeld},
		{http.StatusConflict, "device_check_spent", ErrDeviceCheckSpent},
		{http.StatusConflict, "device_check_busy", ErrDeviceCheckBusy},
		{http.StatusConflict, "credential_unavailable", ErrCredentialUnavailable},
		{http.StatusUnprocessableEntity, "bits_required", ErrBitsRequired},
		{http.StatusUnprocessableEntity, "apple_rejected", ErrAppleRejected},
		{http.StatusServiceUnavailable, "device_check_unavailable", ErrDeviceCheckUnavailable},
	} {
		rec := newRecorder(t, respondJSON(tc.status, errorBody(tc.code, tc.status)))
		_, err := testClient(rec).DeviceCheck.Write(context.Background(), testRequestID, DeviceCheckWrite{Bit0: Bool(true)})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status || apiErr.Code != tc.code {
			t.Errorf("%s: %v", tc.code, err)
		}
		if !errors.Is(err, tc.sentinel) {
			t.Errorf("%s does not match its sentinel", tc.code)
		}
		if errors.Is(err, ErrEventNotFound) || (tc.sentinel != ErrDeviceCheckSpent && errors.Is(err, ErrDeviceCheckSpent)) {
			t.Errorf("%s matches another sentinel", tc.code)
		}
	}
}

func TestDeviceCheckAppleRejectedCarriesAppleStatus(t *testing.T) {
	body := errorBody("apple_rejected", http.StatusUnprocessableEntity)
	body["error"].(map[string]any)["details"] = []any{
		map[string]any{"field": "apple_status", "location": "upstream", "issue": "400"},
	}
	rec := newRecorder(t, respondJSON(http.StatusUnprocessableEntity, body))
	_, err := testClient(rec).DeviceCheck.Query(context.Background(), testRequestID)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !errors.Is(err, ErrAppleRejected) || apiErr.AppleStatus != 400 {
		t.Fatalf("apple_rejected: %v (%+v)", err, apiErr)
	}
	if len(rec.requests) != 1 {
		t.Errorf("a 422 was retried: %d requests", len(rec.requests))
	}
}

func TestDeviceCheckRetriesA503WithTheSameRequest(t *testing.T) {
	noDeviceCheckDelay(t)
	var calls atomic.Int32
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			respondJSON(http.StatusServiceUnavailable, errorBody("device_check_unavailable", 503))(w, r)
			return
		}
		respondJSON(http.StatusOK, map[string]any{"bit0": true})(w, r)
	})
	if _, err := testClient(rec).DeviceCheck.Write(context.Background(), testRequestID, DeviceCheckWrite{Bit0: Bool(true)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(rec.requests) != 3 {
		t.Fatalf("%d requests, want 3", len(rec.requests))
	}
	for i := range rec.requests {
		if rec.body(i) != `{"bit0":true}` || rec.requests[i].Method != http.MethodPut ||
			rec.requests[i].URL.Path != rec.requests[0].URL.Path {
			t.Errorf("attempt %d sent %s %s %q", i, rec.requests[i].Method, rec.requests[i].URL.Path, rec.body(i))
		}
	}
}

func TestDeviceCheckGivesUpAfterThreeAttempts(t *testing.T) {
	noDeviceCheckDelay(t)
	rec := newRecorder(t, respondJSON(http.StatusServiceUnavailable, errorBody("device_check_unavailable", 503)))
	if _, err := testClient(rec).DeviceCheck.Query(context.Background(), testRequestID); !errors.Is(err, ErrDeviceCheckUnavailable) {
		t.Fatalf("Query: %v", err)
	}
	if len(rec.requests) != 3 {
		t.Fatalf("%d requests, want 3", len(rec.requests))
	}
}

func TestDeviceCheckRetriesOnlyA503(t *testing.T) {
	noDeviceCheckDelay(t)
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusConflict, "device_check_spent"},
		{http.StatusConflict, "device_check_busy"},
		{http.StatusUnprocessableEntity, "bits_required"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "internal_error"},
	} {
		rec := newRecorder(t, respondJSON(tc.status, errorBody(tc.code, tc.status)))
		if _, err := testClient(rec).DeviceCheck.Query(context.Background(), testRequestID); err == nil {
			t.Fatalf("%s: no error", tc.code)
		}
		if len(rec.requests) != 1 {
			t.Errorf("%d %s was retried: %d requests", tc.status, tc.code, len(rec.requests))
		}
	}
}

func TestDeviceCheckRetriesATransportFailure(t *testing.T) {
	noDeviceCheckDelay(t)
	var calls atomic.Int32
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Drop the connection without an answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.(*net.TCPConn).SetLinger(0)
				_ = conn.Close()
			}
			return
		}
		respondJSON(http.StatusOK, map[string]any{"found": false, "bit0": false, "bit1": false})(w, r)
	})
	bits, err := testClient(rec).DeviceCheck.Query(context.Background(), testRequestID)
	if err != nil || bits.Found {
		t.Fatalf("Query: %+v, %v", bits, err)
	}
	if len(rec.requests) != 2 || rec.body(1) != "" {
		t.Fatalf("%d requests", len(rec.requests))
	}
}

func TestDeviceCheckStopsRetryingWhenTheContextEnds(t *testing.T) {
	saved := deviceCheckRetryDelays
	deviceCheckRetryDelays = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { deviceCheckRetryDelays = saved })
	rec := newRecorder(t, respondJSON(http.StatusServiceUnavailable, errorBody("device_check_unavailable", 503)))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := testClient(rec).DeviceCheck.Query(ctx, testRequestID); !errors.Is(err, ErrDeviceCheckUnavailable) {
		t.Fatalf("Query: %v", err)
	}
	if len(rec.requests) != 1 {
		t.Fatalf("%d requests", len(rec.requests))
	}
}
