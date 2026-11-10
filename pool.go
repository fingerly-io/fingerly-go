package fingerly

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// poolIdlePerHost is how many idle connections to one host are kept warm.
	// http.DefaultTransport keeps two, so with any real concurrency nearly
	// every request opened a new connection, paid a new handshake, and left a
	// TIME_WAIT socket behind when it closed. At a few hundred requests in
	// flight that ran the host out of local ports.
	poolIdlePerHost = 256
	poolIdleTotal   = 512
	// poolRotateAfter bounds how long one set of connections serves, so a
	// change to the API's DNS records is picked up even on a host that never
	// goes idle. Rotating opens one new connection per request in flight,
	// once, which is negligible at this interval.
	poolRotateAfter = 5 * time.Minute
	// poolRetireAfter is how long a retired set keeps its idle connections
	// before they are closed; requests still in flight on it finish normally.
	poolRetireAfter = 30 * time.Second
)

// sharedPool is the one connection pool every Client and Proxy in the process
// uses when the caller supplies no transport.
var sharedPool = &rotatingTransport{}

// rotatingTransport is an http.RoundTripper over a pooled *http.Transport that
// is replaced every poolRotateAfter. The swap is a pointer change under a
// mutex; nothing on the request path waits on the network for it.
type rotatingTransport struct {
	mu      sync.Mutex
	current *http.Transport
	born    time.Time
	// now is replaced in tests.
	now func() time.Time
}

func (t *rotatingTransport) transport() *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.now != nil {
		now = t.now()
	}
	if t.current != nil && now.Sub(t.born) < poolRotateAfter {
		return t.current
	}
	if retired := t.current; retired != nil {
		time.AfterFunc(poolRetireAfter, retired.CloseIdleConnections)
	}
	t.current = newPooledTransport()
	t.born = now
	return t.current
}

// RoundTrip implements http.RoundTripper.
func (t *rotatingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.transport().RoundTrip(request)
}

// newPooledTransport clones the standard transport, keeping its dialer,
// timeouts, HTTP/2 support and environment proxy settings, and sizes the idle
// pool for a busy server.
func newPooledTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Instrumentation and test libraries replace the default with a
		// wrapper. Use the same settings net/http ships with.
		base = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	transport := base.Clone()
	transport.MaxIdleConns = poolIdleTotal
	transport.MaxIdleConnsPerHost = poolIdlePerHost
	return transport
}
