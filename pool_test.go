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

// TestConcurrentRequestsReuseConnections is the regression for the field
// test: 200 requests in flight through http.DefaultTransport opened a socket
// for most of them.
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
	const inFlight, rounds = 200, 10
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
	if got := conns.Load(); got > inFlight*3/2 {
		t.Errorf("%d requests opened %d connections; expected about %d", inFlight*rounds, got, inFlight)
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
