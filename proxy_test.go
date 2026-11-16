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
		ClientIP: func(r *http.Request) string { return r.Header.Get("X-Real-IP") },
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
		"X-API-Key":       testPublicKey,
		"Origin":          "https://shop.test",
		"User-Agent":      "Firefox",
		"X-Real-IP":       "203.0.113.5",
		"Cookie":          "session=secret",
		"Authorization":   "Bearer secret",
		"Idempotency-Key": "idem-1",
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
		"double slash":  {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "/a//b"},
		"dot segment":   {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "/a/../b"},
		"encoded path":  {ProxyKey: testProxyKey, ClientIP: clientIP, Path: "/a%2Fb"},
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

func TestProxyForwardsRawV3PayloadVerbatimToIngestURL(t *testing.T) {
	for _, body := range []string{
		`{"v":3,"signals":{"latency":142.50,"sci":1e-05}}`,
		`{"v":3,"sdk":{"platform":"web","version":"0.3.0"},"consent":"granted","signals":{}}`,
	} {
		proxy, rec := newProxy(t, nil)
		response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", body, response.StatusCode)
		}
		if got := rec.requests[0].URL.Path; got != ingestPath {
			t.Errorf("%s: forwarded to %q, want %q", body, got, ingestPath)
		}
		if got := rec.body(0); got != body {
			t.Errorf("body mutated: got %q, want %q", got, body)
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
		"/k7x2qm4a/supplement",
		"/k7x2qm4a/events/req_1/supplement",
		"/k7x2qm4a/api/v1/identify",
		"/k7x2qm4ab",
		"/api/v1/ingest",
		"/elsewhere/k7x2qm4a",
		"/k7x2qm4a/",
		"/k7x2qm4a//",
		"//k7x2qm4a",
		"/k7x2qm4a/.",
		"/./k7x2qm4a",
		"/K7X2QM4A",
		"/k7x2qm4a%2F",
		"/%6B7x2qm4a",
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

func TestProxyRefusesAClientIPThatIsNotOneAddress(t *testing.T) {
	for name, address := range map[string]string{
		"crlf":      "1.2.3.4\r\nX-Evil: 1",
		"newline":   "1.2.3.4\n",
		"unicode":   "hé",
		"empty":     "",
		"null":      "null",
		"list":      "203.0.113.5, 198.51.100.7",
		"with port": "203.0.113.5:443",
		"spaces":    " 203.0.113.5",
		"zone":      "fe80::1%en0",
		"garbage":   "not-an-ip",
	} {
		var reports int
		proxy, rec := newProxy(t, func(o *ProxyOptions) {
			o.ClientIP = func(*http.Request) string { return address }
			o.OnError = func(_ *http.Request, failure *ProxyError) {
				if failure.Status == http.StatusInternalServerError {
					reports++
				}
			}
		})
		for i := 0; i < 3; i++ {
			response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusInternalServerError ||
				string(body) != "The client IP callback returned no valid address" {
				t.Errorf("%s: %d %q", name, response.StatusCode, body)
			}
		}
		if len(rec.requests) != 0 {
			t.Errorf("%s: reached the upstream", name)
		}
		if reports != 1 {
			t.Errorf("%s: reported %d times, want once", name, reports)
		}
	}
}

func TestProxyForwardsAValidClientIPCanonically(t *testing.T) {
	for given, want := range map[string]string{
		"203.0.113.5":        "203.0.113.5",
		"2001:DB8::1":        "2001:db8::1",
		"::ffff:203.0.113.5": "::ffff:203.0.113.5",
	} {
		proxy, rec := newProxy(t, func(o *ProxyOptions) {
			o.ClientIP = func(*http.Request) string { return given }
		})
		if response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}"); response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", given, response.StatusCode)
		}
		if got := rec.requests[0].Header.Get("X-Fingerly-Client-IP"); got != want {
			t.Errorf("%s forwarded as %q, want %q", given, got, want)
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
	// No public key: the deprecated secret-key mode authenticates on its own.
	response := call(proxy, http.MethodPost, proxyPath, map[string]string{
		"X-Real-IP": "203.0.113.5",
		"Origin":    "https://shop.test",
	}, "{}")
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

// A deadline middleware only sets a deadline on the request context. Nothing
// else writes the answer, and a handler that writes nothing is a 200.
func TestProxyAnswersARequestDeadlineAsATimeout(t *testing.T) {
	rec := newRecorder(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	var reported []*ProxyError
	proxy, err := NewProxy(ProxyOptions{
		ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL,
		ClientIP: func(*http.Request) string { return "203.0.113.5" },
		OnError:  func(_ *http.Request, failure *ProxyError) { reported = append(reported, failure) },
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, proxyPath, strings.NewReader("{}")).WithContext(ctx)
	for name, value := range browserHeaders() {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout || recorder.Body.String() != "Gateway timeout" {
		t.Errorf("status %d body %q, want 504", recorder.Code, recorder.Body.String())
	}
	if len(reported) != 1 || reported[0].Status != http.StatusGatewayTimeout {
		t.Errorf("OnError got %v", reported)
	}
}

// http.Transport accepts any three digits as a status, and WriteHeader panics
// on a code under 100, so a broken upstream must not reach it.
func TestProxyAnswersAnUnrelayableUpstreamStatusAsBadGateway(t *testing.T) {
	for _, status := range []string{"099 Odd", "101 Switching Protocols", "999 Odd"} {
		rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.Close()
			_, _ = connection.Write([]byte("HTTP/1.1 " + status + "\r\nContent-Length: 0\r\n\r\n"))
		})
		proxy, err := NewProxy(ProxyOptions{
			ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL,
			ClientIP: func(*http.Request) string { return "203.0.113.5" },
			OnError:  func(*http.Request, *ProxyError) {},
		})
		if err != nil {
			t.Fatalf("NewProxy: %v", err)
		}
		response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
		if response.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", status, response.StatusCode)
		}
	}
}

func TestProxySanitizeAndRestoreOriginGateAndMetadata(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) {
		o.AllowedOrigins = []string{"https://app.shop.test"}
	})

	// 1. Originless browser POST -> 400 missing_origin, never synthesizes X-Fingerly-Origin.
	noOrigin := call(proxy, http.MethodPost, proxyPath, map[string]string{
		"X-API-Key": testPublicKey,
		"X-Real-IP": "203.0.113.5",
		"Host":      "shop.test",
		"Referer":   "https://shop.test/checkout",
	}, `{}`)
	if noOrigin.StatusCode != http.StatusBadRequest {
		t.Fatalf("originless POST status = %d, want 400", noOrigin.StatusCode)
	}
	if got := noOrigin.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	noOriginBody, _ := io.ReadAll(noOrigin.Body)
	if !strings.Contains(string(noOriginBody), `"missing_origin"`) {
		t.Errorf("expected missing_origin error body, got %q", noOriginBody)
	}

	// 2. Spoofed X-Fingerly-* headers and invalid X-Fingerly-SDK-Platform values -> 400.
	for _, invalid := range []string{
		"",
		"web",
		"iOS",
		"ANDROID",
		" ios",
		"ios ",
		"ios, android",
		"unknown",
	} {
		resp := call(proxy, http.MethodPost, proxyPath, map[string]string{
			"X-API-Key":               testPublicKey,
			"X-Real-IP":               "203.0.113.5",
			"X-Fingerly-SDK-Platform": invalid,
			"X-Fingerly-Origin":       "https://forged.test",
			"X-Fingerly-Proxy-Key":    "fly_px_development_forged",
			"X-Fingerly-Client-IP":    "1.2.3.4",
		}, `{}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid platform %q: status %d, want 400", invalid, resp.StatusCode)
		}
	}

	// Multiple X-Fingerly-SDK-Platform header lines must also be discarded -> 400.
	multiReq := httptest.NewRequest(http.MethodPost, proxyPath, strings.NewReader(`{}`))
	multiReq.Header.Set("X-API-Key", testPublicKey)
	multiReq.Header.Set("X-Real-IP", "203.0.113.5")
	multiReq.Header.Add("X-Fingerly-SDK-Platform", "ios")
	multiReq.Header.Add("X-Fingerly-SDK-Platform", "android")
	multiRes := httptest.NewRecorder()
	proxy.ServeHTTP(multiRes, multiReq)
	if multiRes.Code != http.StatusBadRequest {
		t.Errorf("duplicate X-Fingerly-SDK-Platform headers: status %d, want 400", multiRes.Code)
	}
	if len(rec.requests) != 0 {
		t.Fatalf("refused requests reached upstream: %d", len(rec.requests))
	}

	// 3. Valid native/bridge platforms without Origin forward with X-Fingerly-SDK-Platform and NO X-Fingerly-Origin.
	validPlatforms := []string{
		"ios",
		"android",
		"react-native",
		"react-native-ios",
		"react-native-android",
		"flutter",
		"flutter-ios",
		"flutter-android",
		"lynx-ios",
		"lynx-android",
		"capacitor",
		"kotlin-multiplatform",
	}
	rawPayload := `{"v":3,"signals":{"latency":142.50,"sci":1e-05}}`
	for _, platform := range validPlatforms {
		resp := call(proxy, http.MethodPost, proxyPath, map[string]string{
			"X-API-Key":                   testPublicKey,
			"X-Real-IP":                   "203.0.113.5",
			"x-fingerly-sdk-platform":     platform,
			"Idempotency-Key":             "idem-go-native-0001",
			"X-Fingerly-Origin":           "https://forged.test",
			"X-Fingerly-Proxy-Key":        "fly_px_development_forged",
			"X-Fingerly-Client-IP":        "1.2.3.4",
			"X-Fingerly-Edge-TLS-Version": "TLSv1.3",
			"Cookie":                      "sid=secret",
			"Authorization":               "Bearer secret",
		}, rawPayload)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("platform %s: status %d, want 200", platform, resp.StatusCode)
		}
		fwd := rec.requests[len(rec.requests)-1]
		if got := fwd.Header.Get("X-Fingerly-SDK-Platform"); got != platform {
			t.Errorf("X-Fingerly-SDK-Platform = %q, want %q", got, platform)
		}
		if vals := fwd.Header.Values("X-Fingerly-Origin"); len(vals) != 0 {
			t.Errorf("native request carried X-Fingerly-Origin: %v", vals)
		}
		if got := fwd.Header.Get("X-Fingerly-Proxy-Key"); got != testProxyKey {
			t.Errorf("X-Fingerly-Proxy-Key = %q, want %q", got, testProxyKey)
		}
		if got := fwd.Header.Get("X-Fingerly-Client-IP"); got != "203.0.113.5" {
			t.Errorf("X-Fingerly-Client-IP = %q, want 203.0.113.5", got)
		}
		if got := fwd.Header.Get("X-Fingerly-Edge-TLS-Version"); got != "" {
			t.Errorf("spoofed X-Fingerly-Edge-TLS-Version forwarded: %q", got)
		}
		if got := rec.body(len(rec.requests) - 1); got != rawPayload {
			t.Errorf("raw body mutated: got %q, want %q", got, rawPayload)
		}
	}

	// 4. Cross-origin CORS preflight and Sec-Fetch-* / Sec-CH-UA-* forwarding.
	preflight := call(proxy, http.MethodOptions, proxyPath, map[string]string{
		"Origin": "https://app.shop.test",
	}, "")
	if preflight.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204", preflight.StatusCode)
	}
	if !strings.Contains(preflight.Header.Get("Access-Control-Allow-Headers"), "x-fingerly-sdk-platform") {
		t.Errorf("Access-Control-Allow-Headers = %q", preflight.Header.Get("Access-Control-Allow-Headers"))
	}
	if !strings.Contains(preflight.Header.Get("Access-Control-Expose-Headers"), "retry-after") {
		t.Errorf("Access-Control-Expose-Headers = %q", preflight.Header.Get("Access-Control-Expose-Headers"))
	}

	webResp := call(proxy, http.MethodPost, proxyPath, map[string]string{
		"X-API-Key":               testPublicKey,
		"X-Real-IP":               "203.0.113.5",
		"Origin":                  "https://app.shop.test",
		"Idempotency-Key":         "idem-go-web-00001",
		"Sec-Fetch-Site":          "same-site",
		"Sec-Fetch-Mode":          "cors",
		"Sec-Fetch-Dest":          "empty",
		"Sec-CH-UA":               `"Chromium";v="130"`,
		"Sec-CH-UA-Mobile":        "?0",
		"Sec-CH-UA-Platform":      `"macOS"`,
		"X-Fingerly-SDK-Platform": "web",
	}, rawPayload)
	if webResp.StatusCode != http.StatusOK {
		t.Fatalf("web POST status = %d, want 200", webResp.StatusCode)
	}
	if got := webResp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.shop.test" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	fwd := rec.requests[len(rec.requests)-1]
	if got := fwd.Header.Get("X-Fingerly-Origin"); got != "https://app.shop.test" {
		t.Errorf("X-Fingerly-Origin = %q", got)
	}
	if vals := fwd.Header.Values("X-Fingerly-SDK-Platform"); len(vals) != 0 {
		t.Errorf("invalid X-Fingerly-SDK-Platform forwarded: %v", vals)
	}
	for name, want := range map[string]string{
		"Sec-Fetch-Site":     "same-site",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Dest":     "empty",
		"Sec-CH-UA":          `"Chromium";v="130"`,
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"macOS"`,
	} {
		if got := fwd.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
