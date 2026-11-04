package fingerly

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxBodyBytes = 1 << 20 // 1 MiB
	defaultProxyTimeout = 5 * time.Second
	// maxRelayBytes caps the upstream answer the proxy holds. An ingest answer
	// is a few kilobytes; anything past this is not one.
	maxRelayBytes = 4 << 20
)

// errUpstreamTooLarge reports an upstream answer over maxRelayBytes.
var errUpstreamTooLarge = errors.New("upstream answer exceeds 4 MiB")

// proxyVersion is what the proxy reports itself as upstream; the dashboard
// shows it per proxy.
const proxyVersion = "go/" + Version

// relayedHeaders are the response headers relayed to the browser, and nothing
// else. A content blocker can match a response header by name, so an
// allowlist is what keeps anything naming Fingerly off the page, whatever the
// API adds in future.
var relayedHeaders = []string{
	"Content-Type",
	"X-Request-Id",
	"RateLimit-Limit",
	"RateLimit-Remaining",
	"Retry-After",
}

// ProxyOptions configures a Proxy. ProxyKey and ClientIP are required.
type ProxyOptions struct {
	// ProxyKey is a write-only credential, fly_px_…, held only on your server.
	ProxyKey string
	// SecretKey is the deprecated alternative to ProxyKey. Secret read keys
	// will be refused in the next major version; do not pass both.
	SecretKey string

	// ClientIP resolves the visitor's address, which must be exactly one IPv4
	// or IPv6 literal. Read it from a header your own infrastructure sets,
	// such as your load balancer's or CDN's, and fall back to the TCP peer in
	// r.RemoteAddr. Never use the left-most X-Forwarded-For value: the visitor
	// writes it. Anything that is not one address is a configuration error:
	// the proxy answers 500 without calling upstream and reports it once.
	ClientIP func(*http.Request) string

	// Path is the path the dashboard issued for this proxy, such as
	// "/k7x2qm4a", matched exactly: any other path gets 404. Leave it empty
	// when the route already sends only that path to the handler, and behind
	// http.StripPrefix, which changes the path the handler sees.
	Path string
	// APIVersion selects the upstream ingest URL. Defaults to "v1".
	APIVersion string
	// Upstream overrides the API origin implied by the key.
	Upstream string
	// MaxBodyBytes refuses larger request bodies with 413. Defaults to 1 MiB.
	MaxBodyBytes int64
	// Timeout bounds the upstream call. Defaults to 5 seconds.
	Timeout time.Duration
	// OnError is called once for every request the proxy answered with a 5xx
	// of its own: 504 when the upstream deadline passed, 502 for any other
	// upstream failure, 500 for a ClientIP that returned no valid address.
	// Leave it nil to log one line per failure through slog.Default(). A
	// visitor who disconnects is not a failure and is not reported.
	OnError func(*http.Request, *ProxyError)
	// Transport supplies the upstream round tripper, for tests or a custom
	// HTTP stack. Left nil, the SDK's shared connection pool is used, sized
	// for hundreds of requests in flight. Redirects are never followed either
	// way.
	Transport http.RoundTripper
}

// Proxy serves the browser SDK from a path on your own domain, so content
// blockers cannot refuse its calls as third-party.
//
// It implements http.Handler, so every Go router takes it directly:
//
//	proxy, err := fingerly.NewProxy(fingerly.ProxyOptions{
//		ProxyKey: os.Getenv("FINGERLY_PROXY_KEY"),
//		Path:     "/k7x2qm4a",
//		ClientIP: func(r *http.Request) string { return r.Header.Get("X-Real-IP") },
//	})
//	mux.Handle("/k7x2qm4a", proxy)
//
// The browser SDK posts both of its calls to that one path, and the proxy
// forwards each to Fingerly's one ingest URL, never anywhere else and never to
// event reads: reading events needs a secret key and must stay unreachable
// from a browser. Customer cookies and authorization headers are never passed
// on, and nothing relayed back names Fingerly.
type Proxy struct {
	key          string
	scope        keyScope
	usesProxyKey bool
	upstreamURL  string
	path         string
	maxBodyBytes int64
	clientIP     func(*http.Request) string
	httpClient   *http.Client
	onError      func(*http.Request, *ProxyError)
	// badClientIP reports an invalid ClientIP once, not once per request.
	badClientIP sync.Once
}

// errNoClientIP is the configuration error for a ClientIP that returned
// something other than one address.
var errNoClientIP = errors.New("the client IP callback returned no valid address")

// ProxyError is an upstream failure the proxy answered for. It never carries
// a key or a response body.
type ProxyError struct {
	// Status is what the browser was answered: 502, 504 or 500.
	Status int
	// Err is the underlying cause.
	Err error
}

// Error implements error.
func (e *ProxyError) Error() string {
	return fmt.Sprintf("fingerly proxy: answered %d: %v", e.Status, e.Err)
}

// Unwrap returns the underlying cause.
func (e *ProxyError) Unwrap() error { return e.Err }

// NewProxy validates every option and builds the handler.
//
// Unlike New, this reports configuration errors immediately: a proxy is wired
// up once at start-up, where an error is natural to handle.
func NewProxy(options ProxyOptions) (*Proxy, error) {
	if options.ProxyKey == "" && options.SecretKey == "" {
		return nil, errors.New("fingerly: a Fingerly proxy key is required")
	}
	if options.ProxyKey != "" && options.SecretKey != "" {
		return nil, errors.New("fingerly: pass ProxyKey or deprecated SecretKey, not both")
	}
	if options.ClientIP == nil {
		return nil, errors.New("fingerly: ClientIP is required")
	}

	proxy := &Proxy{
		usesProxyKey: options.ProxyKey != "",
		clientIP:     options.ClientIP,
		maxBodyBytes: defaultMaxBodyBytes,
		onError:      options.OnError,
	}
	proxy.key = options.ProxyKey
	kind := keyProxy
	if !proxy.usesProxyKey {
		proxy.key, kind = options.SecretKey, keySecret
	}

	scope, err := parseKey(proxy.key, kind)
	if err != nil {
		return nil, err
	}
	proxy.scope = scope

	upstream := options.Upstream
	if upstream == "" {
		upstream = defaultEndpoint
	}
	origin, err := normalizeOrigin(upstream)
	if err != nil {
		return nil, err
	}

	version := options.APIVersion
	if version == "" {
		version = defaultAPIVersion
	}
	if version, err = normalizeAPIVersion(version); err != nil {
		return nil, err
	}
	proxy.upstreamURL = origin + "/api/" + version + "/ingest"

	if options.Path != "" {
		if proxy.path, err = normalizePath(options.Path); err != nil {
			return nil, err
		}
	}

	if options.MaxBodyBytes < 0 {
		return nil, errors.New("fingerly: MaxBodyBytes must not be negative")
	}
	if options.MaxBodyBytes > 0 {
		proxy.maxBodyBytes = options.MaxBodyBytes
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultProxyTimeout
	}
	if _, err := positiveDuration(timeout, "Timeout"); err != nil {
		return nil, err
	}
	transport := options.Transport
	if transport == nil {
		transport = sharedPool
	}
	proxy.httpClient = &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return proxy, nil
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.path != "" && strings.TrimRight(r.URL.Path, "/") != p.path {
		refuse(w, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		refuse(w, http.StatusMethodNotAllowed)
		return
	}

	apiKey, ok := p.upstreamKeyFor(r.Header.Get("X-API-Key"))
	if !ok {
		refuse(w, http.StatusUnauthorized)
		return
	}

	clientIP, ok := p.visitorIP(r)
	if !ok {
		p.badClientIP.Do(func() {
			p.report(r, &ProxyError{Status: http.StatusInternalServerError, Err: errNoClientIP})
		})
		refuseWith(w, http.StatusInternalServerError, "The client IP callback returned no valid address")
		return
	}

	// Refuse on the declared size first; MaxBytesReader enforces the real one.
	if r.ContentLength > p.maxBodyBytes {
		refuse(w, http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuse(w, http.StatusRequestEntityTooLarge)
			return
		}
		refuse(w, http.StatusBadRequest)
		return
	}

	request, err := http.NewRequestWithContext(
		r.Context(), http.MethodPost, p.upstreamURL, bytes.NewReader(body),
	)
	if err != nil {
		refuse(w, http.StatusInternalServerError)
		return
	}
	p.setUpstreamHeaders(request, r, apiKey, clientIP)

	response, err := p.httpClient.Do(request)
	if err != nil {
		p.fail(w, r, err)
		return
	}
	upstreamBody, err := readUpstream(response)
	_ = response.Body.Close()
	if err != nil {
		p.fail(w, r, err)
		return
	}
	p.relay(w, response, upstreamBody)
}

// fail answers an upstream failure: 504 when the deadline passed, 502 for
// anything else. A visitor who went away gets nothing: the upstream call was
// cancelled with their request, and there is no one left to answer.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	status := http.StatusBadGateway
	if isTimeout(err) {
		status = http.StatusGatewayTimeout
	}
	p.report(r, &ProxyError{Status: status, Err: err})
	refuse(w, status)
}

// report hands a failure to OnError, or logs one line about it.
func (p *Proxy) report(r *http.Request, failure *ProxyError) {
	if p.onError != nil {
		p.onError(r, failure)
		return
	}
	slog.Default().Warn("fingerly proxy: upstream failed",
		"status", failure.Status, "error", failure.Err.Error())
}

// isTimeout reports whether err is a deadline passing rather than a refusal,
// reset or malformed answer.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// readUpstream reads the whole upstream answer, up to maxRelayBytes, before
// anything is written to the browser. An answer that stalls or breaks
// mid-body therefore becomes a 502 instead of a 200 with half a body.
func readUpstream(response *http.Response) ([]byte, error) {
	if response.ContentLength > maxRelayBytes {
		return nil, errUpstreamTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRelayBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRelayBytes {
		return nil, errUpstreamTooLarge
	}
	return body, nil
}

// upstreamKeyFor decides which key to present upstream.
//
// With a proxy key the browser must supply a public key for the same
// environment, which the proxy forwards alongside its own. The deprecated
// secret-key mode authenticates on its own.
func (p *Proxy) upstreamKeyFor(presented string) (string, bool) {
	if !p.usesProxyKey {
		return p.key, true
	}
	browser, err := parseKey(presented, keyPublic)
	if err != nil {
		return "", false
	}
	if browser.Environment != p.scope.Environment {
		return "", false
	}
	return presented, true
}

// setUpstreamHeaders builds the headers for the forwarded request.
//
// Nothing is copied wholesale: only the body's content type, the visitor
// metadata Fingerly needs, and an idempotency key when the SDK sent one.
func (p *Proxy) setUpstreamHeaders(request, incoming *http.Request, apiKey, clientIP string) {
	contentType := incoming.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-API-Key", apiKey)
	request.Header.Set("X-Fingerly-Client-IP", clientIP)
	request.Header.Set("X-Fingerly-Origin", incoming.Header.Get("Origin"))
	request.Header.Set("X-Fingerly-User-Agent", incoming.Header.Get("User-Agent"))
	request.Header.Set("X-Fingerly-Proxy-Version", proxyVersion)
	request.Header.Set("User-Agent", userAgent)
	if p.usesProxyKey {
		request.Header.Set("X-Fingerly-Proxy-Key", p.key)
	}
	if idempotency := incoming.Header.Get("Idempotency-Key"); idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
}

// visitorIP runs the ClientIP callback and accepts exactly one IPv4 or IPv6
// literal: no list, no port, no zone, no surrounding space.
func (p *Proxy) visitorIP(r *http.Request) (string, bool) {
	address, err := netip.ParseAddr(p.clientIP(r))
	if err != nil || address.Zone() != "" {
		return "", false
	}
	return address.String(), true
}

// relay returns the upstream answer to the browser, uncacheable and with only
// the allowlisted headers.
func (p *Proxy) relay(w http.ResponseWriter, response *http.Response, body []byte) {
	header := w.Header()
	for _, name := range relayedHeaders {
		for _, value := range response.Header.Values(name) {
			header.Add(name, value)
		}
	}
	header.Set("Cache-Control", "no-store")

	w.WriteHeader(response.StatusCode)
	if response.StatusCode == http.StatusNoContent ||
		response.StatusCode == http.StatusResetContent ||
		response.StatusCode == http.StatusNotModified {
		return
	}
	_, _ = w.Write(body)
}

// normalizePath validates the path the dashboard issued and drops trailing
// slashes. It is matched exactly: the proxy answers that path and nothing
// under it.
func normalizePath(value string) (string, error) {
	path := strings.TrimRight(strings.TrimSpace(value), "/")
	if !strings.HasPrefix(path, "/") || len(path) < 2 ||
		strings.ContainsAny(path, "?#") {
		return "", errors.New("fingerly: Path must be an absolute URL path such as /k7x2qm4a")
	}
	return path, nil
}

// refuse writes a short plain-text answer that names nobody.
func refuse(w http.ResponseWriter, status int) {
	text := http.StatusText(status)
	switch status {
	case http.StatusBadGateway:
		text = "Bad gateway"
	case http.StatusGatewayTimeout:
		text = "Gateway timeout"
	}
	refuseWith(w, status, text)
}

// refuseWith writes status and a one-line text body, uncacheable.
func refuseWith(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}
