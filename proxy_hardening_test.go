package fingerly

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A request naming both channels is forwarded on the browser channel only: the
// API refuses a proxied request that carries both.
func TestProxyForwardsOnlyTheOriginWhenBothChannelsArePresent(t *testing.T) {
	proxy, rec := newProxy(t, nil)
	headers := browserHeaders()
	headers["X-Fingerly-SDK-Platform"] = "ios"
	response := call(proxy, http.MethodPost, proxyPath, headers, "{}")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	forwarded := rec.requests[len(rec.requests)-1]
	if got := forwarded.Header.Get("X-Fingerly-Origin"); got != "https://shop.test" {
		t.Errorf("X-Fingerly-Origin = %q", got)
	}
	if got := forwarded.Header.Values("X-Fingerly-SDK-Platform"); len(got) != 0 {
		t.Errorf("X-Fingerly-SDK-Platform forwarded next to the origin: %v", got)
	}
}

func TestNewProxyAcceptsOnlyABareHTTPSUpstream(t *testing.T) {
	clientIP := func(*http.Request) string { return "203.0.113.5" }
	build := func(upstream string, loopback bool) (*Proxy, error) {
		return NewProxy(ProxyOptions{
			ProxyKey: testProxyKey, ClientIP: clientIP,
			Upstream: upstream, AllowInsecureLoopback: loopback,
		})
	}
	for _, upstream := range []string{
		"http://upstream.invalid",
		"http://192.0.2.10:8080",
		"https://upstream.invalid/prefix",
		"https://upstream.invalid/a/../b",
		"https://upstream.invalid//",
		"https://upstream.invalid/%2e",
		"https://upstream.invalid\n",
		"https://upstream.invalid ",
		" https://upstream.invalid",
		"https://upstream.invalid\r\nx-injected: 1",
		"https://upstream.invalid\t",
		"https://upstream.invalid?",
		"https://upstream.invalid?a=b",
		"https://upstream.invalid#",
		"https://upstream.invalid#a",
		"https://user:pass@upstream.invalid",
		"https://upstream.invalid:0",
		"https://upstream.invalid:99999",
		"https://",
		"ftp://upstream.invalid",
		"file:///private",
		"upstream.invalid",
	} {
		for _, loopback := range []bool{false, true} {
			if _, err := build(upstream, loopback); err == nil {
				t.Errorf("%q (loopback %v) was accepted", upstream, loopback)
			} else if strings.Contains(err.Error(), "upstream.invalid") || strings.Contains(err.Error(), "192.0.2.10") {
				t.Errorf("%q: the error repeats the value: %v", upstream, err)
			}
		}
	}
	for _, upstream := range []string{"http://127.0.0.1:8080", "http://localhost", "http://[::1]:8080"} {
		if _, err := build(upstream, false); err == nil {
			t.Errorf("%q was accepted without AllowInsecureLoopback", upstream)
		}
	}
	for upstream, want := range map[string]string{
		"https://upstream.invalid":      "https://upstream.invalid/api/v1/ingest",
		"https://upstream.invalid/":     "https://upstream.invalid/api/v1/ingest",
		"https://upstream.invalid:8443": "https://upstream.invalid:8443/api/v1/ingest",
		"HTTPS://upstream.invalid":      "https://upstream.invalid/api/v1/ingest",
	} {
		proxy, err := build(upstream, false)
		if err != nil {
			t.Errorf("%q: %v", upstream, err)
			continue
		}
		if proxy.upstreamURL != want {
			t.Errorf("%q: upstream URL %q, want %q", upstream, proxy.upstreamURL, want)
		}
	}
	for upstream, want := range map[string]string{
		"http://127.0.0.1:8080": "http://127.0.0.1:8080/api/v1/ingest",
		"http://localhost":      "http://localhost/api/v1/ingest",
		"http://[::1]:8080/":    "http://[::1]:8080/api/v1/ingest",
	} {
		proxy, err := build(upstream, true)
		if err != nil {
			t.Errorf("%q: %v", upstream, err)
			continue
		}
		if proxy.upstreamURL != want {
			t.Errorf("%q: upstream URL %q, want %q", upstream, proxy.upstreamURL, want)
		}
	}
}

// The API never redirects. A 3xx is answered 502 and its target is never
// called, with the SDK's pool or a caller-supplied transport.
func TestProxyAnswersARedirectAsBadGatewayAndNeverFollowsIt(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
	}))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308, 300, 304} {
		for _, transport := range []http.RoundTripper{nil, &http.Transport{}} {
			rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target.URL+"/collect")
				w.WriteHeader(status)
			})
			proxy, err := NewProxy(ProxyOptions{
				ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL, AllowInsecureLoopback: true,
				ClientIP:  func(*http.Request) string { return "203.0.113.5" },
				OnError:   func(*http.Request, *ProxyError) {},
				Transport: transport,
			})
			if err != nil {
				t.Fatalf("NewProxy: %v", err)
			}
			response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
			if response.StatusCode != http.StatusBadGateway {
				t.Errorf("%d: status %d, want 502", status, response.StatusCode)
			}
			if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Location") != "" {
				t.Errorf("%d: headers %v", status, response.Header)
			}
		}
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect target was called %d times", n)
	}
}

func TestProxyAnswersAnUpstreamStatusOutsideTheRangeAsBadGateway(t *testing.T) {
	for _, status := range []string{"000 Odd", "099 Odd", "199 Odd", "600 Odd", "700 Odd", "999 Odd"} {
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
			ProxyKey: testProxyKey, Path: proxyPath, Upstream: rec.server.URL, AllowInsecureLoopback: true,
			ClientIP: func(*http.Request) string { return "203.0.113.5" },
			OnError:  func(*http.Request, *ProxyError) {},
		})
		if err != nil {
			t.Fatalf("NewProxy: %v", err)
		}
		response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
		if response.StatusCode != http.StatusBadGateway || response.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: status %d, Cache-Control %q", status, response.StatusCode, response.Header.Get("Cache-Control"))
		}
	}
}

func TestProxyMethodIsCaseSensitiveAndEveryRefusalIsUncacheable(t *testing.T) {
	proxy, rec := newProxy(t, func(o *ProxyOptions) { o.AllowedOrigins = []string{"https://shop.test"} })
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"post", proxyPath, http.StatusMethodNotAllowed},
		{"Post", proxyPath, http.StatusMethodNotAllowed},
		{"options", proxyPath, http.StatusMethodNotAllowed},
		{http.MethodGet, proxyPath, http.StatusMethodNotAllowed},
		{http.MethodPut, proxyPath, http.StatusMethodNotAllowed},
		{http.MethodPost, proxyPath + "/", http.StatusNotFound},
		{http.MethodPost, "/" + proxyPath, http.StatusNotFound},
		{http.MethodPost, proxyPath + "/..", http.StatusNotFound},
		{http.MethodPost, "/k7x2qm4%61", http.StatusNotFound},
		{http.MethodPost, "/K7X2QM4A", http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{}"))
		request.Method = c.method
		for name, value := range browserHeaders() {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)
		if recorder.Code != c.status {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, recorder.Code, c.status)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s: Cache-Control %q", c.method, c.path, got)
		}
	}
	if len(rec.requests) != 0 {
		t.Errorf("%d refused requests reached upstream", len(rec.requests))
	}
}

// Failure reports name the kind of failure only, never the upstream host, port
// or URL, whether they go to OnError or to the default log line.
func TestProxyFailureReportsNameNeitherTheUpstreamNorTheKey(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	port := closedURL[strings.LastIndex(closedURL, ":")+1:]
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("port %q", port)
	}
	redirect := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	huge := newRecorder(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxRelayBytes+1))
	})

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	})))
	defer slog.SetDefault(previous)

	for _, upstream := range []string{closedURL, redirect.server.URL, huge.server.URL} {
		var hooked []string
		for _, hook := range []func(*http.Request, *ProxyError){
			func(_ *http.Request, failure *ProxyError) { hooked = append(hooked, failure.Error()) },
			nil,
		} {
			proxy, err := NewProxy(ProxyOptions{
				ProxyKey: testProxyKey, Path: proxyPath, Upstream: upstream, AllowInsecureLoopback: true,
				ClientIP: func(*http.Request) string { return "203.0.113.5" },
				OnError:  hook,
			})
			if err != nil {
				t.Fatalf("NewProxy: %v", err)
			}
			response := call(proxy, http.MethodPost, proxyPath, browserHeaders(), "{}")
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusBadGateway || string(body) != "Bad gateway" {
				t.Errorf("%s: %d %q", upstream, response.StatusCode, body)
			}
		}
		if len(hooked) != 1 {
			t.Fatalf("%s: OnError got %v", upstream, hooked)
		}
		for _, text := range []string{hooked[0], logged.String()} {
			for _, forbidden := range []string{"127.0.0.1", port, "ingest", testProxyKey, testPublicKey, "elsewhere"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("%s: report %q names %q", upstream, text, forbidden)
				}
			}
		}
		logged.Reset()
	}
}
