package fingerly

import (
	"net/http"
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
