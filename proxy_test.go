package fingerly

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testProxyKey  = "fly_px_us_development_test"
	testPublicKey = "fly_pk_us_development_test"
	identifyPath  = "/metrics/api/v1/identify"
)

// newProxy builds a proxy mounted at /metrics whose upstream is a recorder.
func newProxy(t *testing.T, apply func(*ProxyOptions)) (*Proxy, *recorder) {
	t.Helper()
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{"request_id": "identified"}))
	options := ProxyOptions{
		ProxyKey: testProxyKey,
		Prefix:   "/metrics",
		Upstream: rec.server.URL,
		ClientIP: func(r *http.Request) string { return r.Header.Get("CF-Connecting-IP") },
	}
	if apply != nil {
		apply(&options)
	}
	proxy, err := NewProxy(options)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	return proxy, rec
}

// call drives a proxy as a mux would and returns its response.
func call(proxy *Proxy, method, path string, headers map[string]string, body string) *http.Response {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	return response.Result()
}

func browserHeaders() map[string]string {
	return map[string]string{
		"X-API-Key":        testPublicKey,
		"Origin":           "https://shop.test",
		"User-Agent":       "Firefox",
		"CF-Connecting-IP": "203.0.113.5",
		"Cookie":           "session=secret",
		"Authorization":    "Bearer secret",
		"Idempotency-Key":  "idem-1",
	}
}

func TestNewProxyRejectsInvalidOptions(t *testing.T) {
	clientIP := func(*http.Request) string { return "203.0.113.5" }
	for name, options := range map[string]ProxyOptions{
		"no key":        {ClientIP: clientIP},
		"both keys":     {ProxyKey: testProxyKey, SecretKey: testSecretKey, ClientIP: clientIP},
		"no client ip":  {ProxyKey: testProxyKey},
		"secret as prx": {ProxyKey: testSecretKey, ClientIP: clientIP},
		"public as prx": {ProxyKey: testPublicKey, ClientIP: clientIP},
		"bad prefix":    {ProxyKey: testProxyKey, ClientIP: clientIP, Prefix: "metrics"},
		"query prefix":  {ProxyKey: testProxyKey, ClientIP: clientIP, Prefix: "/a?b"},
		"bad version":   {ProxyKey: testProxyKey, ClientIP: clientIP, APIVersion: "v0"},
		"bad upstream":  {ProxyKey: testProxyKey, ClientIP: clientIP, Upstream: "file:///private"},
		"negative body": {ProxyKey: testProxyKey, ClientIP: clientIP, MaxBodyBytes: -1},
		"bad timeout":   {ProxyKey: testProxyKey, ClientIP: clientIP, Timeout: -time.Second},
	} {
		if _, err := NewProxy(options); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestProxyForwardsBothProxyableRoutes(t *testing.T) {
	for _, path := range []string{
		identifyPath, "/metrics/api/v1/events/request-1/supplement",
	} {
		proxy, rec := newProxy(t, nil)
		response := call(proxy, http.MethodPost, path, browserHeaders(), "{}")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, response.StatusCode)
		}
		want := strings.TrimPrefix(path, "/metrics")
		if got := rec.requests[0].URL.Path; got != want {
			t.Errorf("%s: forwarded to %q, want %q", path, got, want)
		}
	}
}

func TestProxySendsOnlyVettedVisitorMetadata(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	call(proxy, http.MethodPost, identifyPath, browserHeaders(), `{"signals":{}}`)

	forwarded := rec.requests[0]
	for name, want := range map[string]string{
		"X-API-Key":             testPublicKey,
		"X-Fingerly-Proxy-Key":  testProxyKey,
		"X-Fingerly-Client-IP":  "203.0.113.5",
		"X-Fingerly-Origin":     "https://shop.test",
		"X-Fingerly-User-Agent": "Firefox",
		"Idempotency-Key":       "idem-1",
	} {
		if got := forwarded.Header.Get(name); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Cookie", "Authorization"} {
		if got := forwarded.Header.Get(name); got != "" {
			t.Errorf("%s was forwarded: %q", name, got)
		}
	}
	if body := rec.body(0); body != `{"signals":{}}` {
		t.Errorf("body is %q", body)
	}
}

func TestProxyMarksResponsesUncacheable(t *testing.T) {
	proxy, _ := newProxy(t, nil)
	response := call(proxy, http.MethodPost, identifyPath, browserHeaders(), "{}")
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control is %q", got)
	}
}

func TestProxyDropsConnectionHeadersFromTheUpstream(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("RateLimit-Remaining", "99")
		w.Header().Set("Cache-Control", "public, max-age=600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":1}`))
	})
	proxy, err := NewProxy(ProxyOptions{
		ProxyKey: testProxyKey, Prefix: "/metrics", Upstream: rec.server.URL,
		ClientIP: func(*http.Request) string { return "203.0.113.5" },
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	response := call(proxy, http.MethodPost, identifyPath, browserHeaders(), "{}")
	if got := response.Header.Get("RateLimit-Remaining"); got != "99" {
		t.Errorf("a useful upstream header was dropped: %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control is %q, want no-store", got)
	}
	for _, name := range []string{"Content-Encoding", "Transfer-Encoding"} {
		if got := response.Header.Get(name); got != "" {
			t.Errorf("%s was relayed: %q", name, got)
		}
	}
}

func TestProxyRefusesAnythingButPost(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	response := call(proxy, http.MethodGet, identifyPath, browserHeaders(), "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d", response.StatusCode)
	}
	if got := response.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow is %q", got)
	}
	if len(rec.requests) != 0 {
		t.Error("a non-POST reached the upstream")
	}
}

func TestProxyRefusesEveryRouteThatIsNotProxyable(t *testing.T) {
	for _, path := range []string{
		"/metrics/api/v1/events",
		"/metrics/api/v1/events/request-1",
		"/metrics/api/v1/events/a/b/supplement",
		"/metrics/api/v1/events//supplement",
		"/metrics/api/v1/identify/extra",
		"/metrics/api/v2/identify",
		"/metrics/admin",
		"/elsewhere/api/v1/identify",
	} {
		proxy, rec := newProxy(t, nil)
		response := call(proxy, http.MethodPost, path, browserHeaders(), "{}")
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, response.StatusCode)
		}
		if len(rec.requests) != 0 {
			t.Errorf("%s: reached the upstream", path)
		}
	}
}

func TestProxyRefusesAPublicKeyFromAnotherDeployment(t *testing.T) {
	for name, key := range map[string]string{
		"missing":      "",
		"other region": "fly_pk_eu_development_test",
		"other env":    "fly_pk_us_production_test",
		"secret key":   testSecretKey,
		"nonsense":     "invalid",
	} {
		proxy, rec := newProxy(t, nil)
		headers := browserHeaders()
		headers["X-API-Key"] = key
		response := call(proxy, http.MethodPost, identifyPath, headers, "{}")
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, response.StatusCode)
		}
		if len(rec.requests) != 0 {
			t.Errorf("%s: reached the upstream", name)
		}
	}
}

func TestProxyRefusesAnOversizedBody(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) { o.MaxBodyBytes = 8 })
	response := call(proxy, http.MethodPost, identifyPath, browserHeaders(),
		"a body well past the limit")
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", response.StatusCode)
	}
	if len(rec.requests) != 0 {
		t.Error("an oversized body reached the upstream")
	}
}

func TestProxyRefusesAClientIPThatCouldInjectAHeader(t *testing.T) {
	for name, address := range map[string]string{
		"crlf":    "1.2.3.4\r\nX-Evil: 1",
		"newline": "1.2.3.4\n",
		"unicode": "hé",
	} {
		proxy, rec := newProxy(t, func(o *ProxyOptions) {
			o.ClientIP = func(*http.Request) string { return address }
		})
		response := call(proxy, http.MethodPost, identifyPath, browserHeaders(), "{}")
		if response.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", name, response.StatusCode)
		}
		if len(rec.requests) != 0 {
			t.Errorf("%s: reached the upstream", name)
		}
	}
}

func TestProxyWorksBehindStripPrefix(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	mux := http.NewServeMux()
	mux.Handle("/metrics/", http.StripPrefix("/metrics", proxy))

	request := httptest.NewRequest(http.MethodPost, identifyPath, strings.NewReader("{}"))
	for name, value := range browserHeaders() {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if got := rec.requests[0].URL.Path; got != "/api/v1/identify" {
		t.Errorf("forwarded to %q", got)
	}
}

func TestProxyMountsOnAServeMux(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	mux := http.NewServeMux()
	mux.Handle("/metrics/", proxy)
	server := httptest.NewServer(mux)
	defer server.Close()

	request, _ := http.NewRequest(http.MethodPost, server.URL+identifyPath, strings.NewReader("{}"))
	for name, value := range browserHeaders() {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if got := rec.requests[0].URL.Path; got != "/api/v1/identify" {
		t.Errorf("forwarded to %q", got)
	}
}

func TestProxyForwardsTheSecretKeyWithoutAPublicKey(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) {
		o.ProxyKey, o.SecretKey = "", testSecretKey
	})
	response := call(proxy, http.MethodPost, identifyPath, map[string]string{}, "{}")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	forwarded := rec.requests[0]
	if got := forwarded.Header.Get("X-API-Key"); got != testSecretKey {
		t.Errorf("X-API-Key is %q", got)
	}
	if got := forwarded.Header.Get("X-Fingerly-Proxy-Key"); got != "" {
		t.Errorf("a proxy key was sent in secret-key mode: %q", got)
	}
}
