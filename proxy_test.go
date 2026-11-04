package fingerly

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testProxyKey  = "fly_px_development_test"
	testPublicKey = "fly_pk_development_test"
	proxyPath     = "/k7x2qm4a"
	ingestPath    = "/api/v1/ingest"
)

// newProxy builds a proxy answering proxyPath whose upstream is a recorder.
func newProxy(t *testing.T, apply func(*ProxyOptions)) (*Proxy, *recorder) {
	t.Helper()
	rec := newRecorder(t, respondJSON(http.StatusOK, map[string]any{"request_id": "identified"}))
	options := ProxyOptions{
		ProxyKey: testProxyKey,
		Path:     proxyPath,
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
		"relative path": {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "k7x2qm4a"},
		"root path":     {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "/"},
		"slashes path":  {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "///"},
		"query path":    {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "/a?b"},
		"fragment path": {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "/a#b"},
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

func TestProxyForwardsBothCallsToTheOneIngestURL(t *testing.T) {
	// The identification and the deferred report share the path; the API
	// tells them apart by the body.
	for _, body := range []string{`{"signals":{}}`, `{"token":"t"}`} {
		for _, path := range []string{proxyPath, proxyPath + "/"} {
			proxy, rec := newProxy(t, nil)
			response := call(proxy, http.MethodPost, path, browserHeaders(), body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status %d", path, body, response.StatusCode)
			}
			if got := rec.requests[0].URL.Path; got != ingestPath {
				t.Errorf("%s %s: forwarded to %q, want %q", path, body, got, ingestPath)
			}
			if got := rec.body(0); got != body {
				t.Errorf("%s: body is %q", path, got)
			}
		}
	}
}

func TestProxyForwardsToTheConfiguredVersionsIngestURL(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) { o.APIVersion = "v2" })
	response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if got := rec.requests[0].URL.Path; got != "/api/v2/ingest" {
		t.Errorf("forwarded to %q", got)
	}
}

func TestProxySendsOnlyVettedVisitorMetadata(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	call(proxy, http.MethodPost, proxyPath, browserHeaders(), `{"signals":{}}`)

	forwarded := rec.requests[0]
	for name, want := range map[string]string{
		"X-API-Key":                testPublicKey,
		"X-Fingerly-Proxy-Key":     testProxyKey,
		"X-Fingerly-Client-IP":     "203.0.113.5",
		"X-Fingerly-Origin":        "https://shop.test",
		"X-Fingerly-User-Agent":    "Firefox",
		"X-Fingerly-Proxy-Version": "go/" + Version,
		"Idempotency-Key":          "idem-1",
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
	response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control is %q", got)
	}
}

func TestProxyRelaysOnlyAllowlistedHeaders(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-1")
		w.Header().Set("RateLimit-Limit", "100")
		w.Header().Set("RateLimit-Remaining", "99")
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Fingerly-Balance-Micros", "100")
		w.Header().Set("Set-Cookie", "a=b")
		// Not gzip, which the transport would decode and drop by itself.
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Cache-Control", "public, max-age=600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":1}`))
	})
	proxy, err := NewProxy(ProxyOptions{
		ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL,
		ClientIP: func(*http.Request) string { return "203.0.113.5" },
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
	for name, want := range map[string]string{
		"Content-Type":        "application/json",
		"X-Request-Id":        "req-1",
		"RateLimit-Limit":     "100",
		"RateLimit-Remaining": "99",
		"Retry-After":         "3",
		"Cache-Control":       "no-store",
	} {
		if got := response.Header.Get(name); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Fingerly-Balance-Micros", "Set-Cookie", "Content-Encoding"} {
		if got := response.Header.Get(name); got != "" {
			t.Errorf("%s was relayed: %q", name, got)
		}
	}
	// Nothing relayed may name Fingerly: a blocker can match a header name.
	for name, values := range response.Header {
		if strings.Contains(strings.ToLower(name+" "+strings.Join(values, " ")), "fingerly") {
			t.Errorf("relayed header %s: %q names Fingerly", name, values)
		}
	}
}

func TestProxyRefusesAnythingButPost(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	response := call(proxy, http.MethodGet, proxyPath, browserHeaders(), "")
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

func TestProxyRefusesEveryOtherPath(t *testing.T) {
	for _, path := range []string{
		"/",
		"/other",
		"/k7x2qm4a/admin",
		"/k7x2qm4a/api/v1/identify",
		"/k7x2qm4ab",
		"/api/v1/ingest",
		"/elsewhere/k7x2qm4a",
	} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			proxy, rec := newProxy(t, nil)
			response := call(proxy, method, path, browserHeaders(), "{}")
			if response.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: status %d, want 404", method, path, response.StatusCode)
			}
			if len(rec.requests) != 0 {
				t.Errorf("%s %s: reached the upstream", method, path)
			}
		}
	}
}

func TestProxyWithoutAPathAnswersWhereverItIsRouted(t *testing.T) {
	for _, path := range []string{proxyPath, "/", "/anything/else"} {
		proxy, rec := newProxy(t, func(o *ProxyOptions) { o.Path = "" })
		response := call(proxy, http.MethodPost, path, browserHeaders(), "{}")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, response.StatusCode)
		}
		if got := rec.requests[0].URL.Path; got != ingestPath {
			t.Errorf("%s: forwarded to %q", path, got)
		}
	}
}

func TestProxyRefusesAPublicKeyFromAnotherDeployment(t *testing.T) {
	for name, key := range map[string]string{
		"missing":    "",
		"other env":  "fly_pk_production_test",
		"secret key": testSecretKey,
		"nonsense":   "invalid",
	} {
		proxy, rec := newProxy(t, nil)
		headers := browserHeaders()
		headers["X-API-Key"] = key
		response := call(proxy, http.MethodPost, proxyPath, headers, "{}")
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
	response := call(proxy, http.MethodPost, proxyPath, browserHeaders(),
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
		response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
		if response.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", name, response.StatusCode)
		}
		if len(rec.requests) != 0 {
			t.Errorf("%s: reached the upstream", name)
		}
	}
}

func TestProxyWorksBehindStripPrefixWithoutAPath(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) { o.Path = "" })
	mux := http.NewServeMux()
	mux.Handle(proxyPath, http.StripPrefix(proxyPath, proxy))

	request := httptest.NewRequest(http.MethodPost, proxyPath, strings.NewReader("{}"))
	for name, value := range browserHeaders() {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if got := rec.requests[0].URL.Path; got != ingestPath {
		t.Errorf("forwarded to %q", got)
	}
}

func TestProxyMountsOnAServeMux(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	mux := http.NewServeMux()
	mux.Handle(proxyPath, proxy)
	server := httptest.NewServer(mux)
	defer server.Close()

	request, _ := http.NewRequest(http.MethodPost, server.URL+proxyPath, strings.NewReader("{}"))
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
	if got := rec.requests[0].URL.Path; got != ingestPath {
		t.Errorf("forwarded to %q", got)
	}
}

func TestProxyForwardsTheSecretKeyWithoutAPublicKey(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) {
		o.ProxyKey, o.SecretKey = "", testSecretKey
	})
	response := call(proxy, http.MethodPost, proxyPath, map[string]string{}, "{}")
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

func TestProxyNeverRelaysATruncatedAnswerAsASuccess(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"breaks mid-body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"request_id":`))
		},
		"over the cap": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bytes.Repeat([]byte("a"), maxRelayBytes+1))
		},
		"declares more than the cap": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(maxRelayBytes+1))
			w.WriteHeader(http.StatusOK)
		},
	}
	for name, handler := range cases {
		rec := newRecorder(t, handler)
		proxy, err := NewProxy(ProxyOptions{
			ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL,
			ClientIP: func(*http.Request) string { return "203.0.113.5" },
		})
		if err != nil {
			t.Fatalf("NewProxy: %v", err)
		}
		response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
		if response.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", name, response.StatusCode)
		}
		if got := response.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", name, got)
		}
	}
}

func TestProxyAnswersUpstreamFailuresWithoutNamingAnyone(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	slow := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	})
	trickle := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 1000; i++ {
			if _, err := w.Write([]byte("a")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	for name, c := range map[string]struct {
		upstream string
		status   int
		body     string
	}{
		"slow":    {slow.server.URL, http.StatusGatewayTimeout, "Gateway timeout"},
		"trickle": {trickle.server.URL, http.StatusGatewayTimeout, "Gateway timeout"},
		"refused": {closedURL, http.StatusBadGateway, "Bad gateway"},
	} {
		var reported []*ProxyError
		proxy, err := NewProxy(ProxyOptions{
			ProxyKey: testProxyKey, Path: proxyPath, Upstream: c.upstream, Timeout: 200 * time.Millisecond,
			ClientIP: func(*http.Request) string { return "203.0.113.5" },
			OnError:  func(_ *http.Request, failure *ProxyError) { reported = append(reported, failure) },
		})
		if err != nil {
			t.Fatalf("NewProxy: %v", err)
		}
		started := time.Now()
		response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != c.status || string(body) != c.body {
			t.Errorf("%s: %d %q, want %d %q", name, response.StatusCode, body, c.status, c.body)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Errorf("%s: took %v; the deadline did not bound the exchange", name, elapsed)
		}
		if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%s: headers %v", name, response.Header)
		}
		if len(reported) != 1 || reported[0].Status != c.status {
			t.Errorf("%s: OnError got %v", name, reported)
		}
		if len(reported) == 1 && strings.Contains(reported[0].Error(), testProxyKey) {
			t.Errorf("%s: the reported error carries the proxy key", name)
		}
	}
}

func TestProxyEndsQuietlyWhenTheVisitorDisconnects(t *testing.T) {
	cancelled := make(chan struct{})
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(cancelled)
	})
	var reported int
	proxy, err := NewProxy(ProxyOptions{
		ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL,
		ClientIP: func(*http.Request) string { return "203.0.113.5" },
		OnError:  func(*http.Request, *ProxyError) { reported++ },
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, proxyPath, strings.NewReader("{}")).WithContext(ctx)
	for name, value := range browserHeaders() {
		request.Header.Set(name, value)
	}
	time.AfterFunc(100*time.Millisecond, cancel)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream call was not cancelled")
	}
	if reported != 0 || recorder.Body.Len() != 0 {
		t.Errorf("a disconnect was reported (%d) or answered (%q)", reported, recorder.Body.String())
	}
}
