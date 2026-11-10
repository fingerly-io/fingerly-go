package fingerly

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultsShareOnePooledTransport(t *testing.T) {
	proxy, err := NewProxy(ProxyOptions{ProxyKey: testProxyKey, ClientIP: func(*http.Request) string { return "203.0.113.5" }})
	if err != nil {
		t.Fatal(err)
	}
	if proxy.httpClient.Transport != sharedPool {
		t.Errorf("proxy does not use the shared pool")
	}
	if New(testSecretKey).httpClient.Transport != sharedPool {
		t.Errorf("client does not use the shared pool")
	}
	if New(testSecretKey, WithHTTPClient(&http.Client{Timeout: time.Second})).httpClient.Transport != sharedPool {
		t.Errorf("a supplied client without a transport does not use the shared pool")
	}
	if got := sharedPool.transport().MaxIdleConnsPerHost; got < 200 {
		t.Errorf("MaxIdleConnsPerHost is %d; 200 requests in flight would churn connections", got)
	}
}

type replacedTransport struct{ http.RoundTripper }

// Instrumentation and test libraries replace http.DefaultTransport with a
// wrapper; the pool must not assume its concrete type.
func TestPoolSurvivesAReplacedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = replacedTransport{original}

	transport := newPooledTransport()
	if transport.MaxIdleConnsPerHost != poolIdlePerHost {
		t.Errorf("MaxIdleConnsPerHost is %d", transport.MaxIdleConnsPerHost)
	}
	if transport.ForceAttemptHTTP2 != true || transport.Proxy == nil {
		t.Errorf("the fallback lost the standard transport's settings")
	}
}

// TestConcurrentRequestsReuseConnections is the regression for the field
// test: 200 requests in flight through http.DefaultTransport opened a socket
// for most of them. Starting cold, the count is not stable: each request that
// finds no idle connection dials its own, and a dial overtaken by a connection
// coming back still lands in the pool, so 200 at once can open 350. Only what
// happens once the pool is warm says whether connections are reused.
func TestConcurrentRequestsReuseConnections(t *testing.T) {
	var conns atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r"}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	proxy, err := NewProxy(ProxyOptions{ProxyKey: testProxyKey, Upstream: server.URL,
		ClientIP: func(*http.Request) string { return "203.0.113.5" }})
	if err != nil {
		t.Fatal(err)
	}
	const inFlight, warmUp, rounds = 200, 5, 10
	load := func(rounds int) {
		var wg sync.WaitGroup
		for g := 0; g < inFlight; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < rounds; i++ {
					request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
					request.Header.Set("X-API-Key", testPublicKey)
					request.Header.Set("Origin", "https://shop.test")
					recorder := httptest.NewRecorder()
					proxy.ServeHTTP(recorder, request)
					if recorder.Code != http.StatusOK {
						t.Errorf("status %d", recorder.Code)
						return
					}
				}
			}()
		}
		wg.Wait()
	}
	load(warmUp)
	warm := conns.Load()
	load(rounds)
	// A pool that keeps only two idle connections opens well over 500 here; a
	// warm one opens almost none, about a hundred at worst under the race
	// detector, whose slower scheduling lets more dials race.
	if opened := conns.Load() - warm; opened > inFlight {
		t.Errorf("%d requests on a warm pool opened %d connections; expected almost none", inFlight*rounds, opened)
	}
}

func TestPoolRotatesSoDNSIsResolvedAgain(t *testing.T) {
	now := time.Unix(0, 0)
	pool := &rotatingTransport{now: func() time.Time { return now }}
	first := pool.transport()
	if pool.transport() != first {
		t.Fatalf("rotated before the interval")
	}
	now = now.Add(poolRotateAfter)
	if pool.transport() == first {
		t.Fatalf("did not rotate after %v", poolRotateAfter)
	}
}
