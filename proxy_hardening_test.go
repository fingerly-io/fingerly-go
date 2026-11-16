package fingerly

import (
	"net/http"
	"net/http/httptest"
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
	for _, upstream := range []string{"http://upstream.invalid", "http://192.0.2.10:8080"} {
		for _, loopback := range []bool{false, true} {
			if _, err := build(upstream, loopback); err == nil {
				t.Errorf("%q (loopback %v) was accepted", upstream, loopback)
			}
		}
	}
	for _, upstream := range []string{"http://127.0.0.1:8080", "http://localhost", "http://[::1]:8080"} {
		if _, err := build(upstream, false); err == nil {
			t.Errorf("%q was accepted without AllowInsecureLoopback", upstream)
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
	if _, err := build("https://upstream.invalid", false); err != nil {
		t.Errorf("https upstream refused: %v", err)
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
