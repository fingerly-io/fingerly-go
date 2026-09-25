package fingerly

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultMaxBodyBytes = 1 << 20 // 1 MiB
	defaultProxyTimeout = 5 * time.Second
)

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

	// ClientIP resolves the visitor's address. Read it from a header your own
	// infrastructure sets, such as your load balancer's or CDN's. Never use
	// the left-most X-Forwarded-For value: the visitor writes it.
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
	// Transport supplies the upstream round tripper, for tests or a custom
	// HTTP stack. Redirects are never followed either way.
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
//		ClientIP: func(r *http.Request) string { return r.Header.Get("CF-Connecting-IP") },
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
}

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
	proxy.httpClient = &http.Client{
		Timeout:   timeout,
		Transport: options.Transport,
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
	if err := p.setUpstreamHeaders(request, r, apiKey); err != nil {
		refuse(w, http.StatusInternalServerError)
		return
	}

	response, err := p.httpClient.Do(request)
	if err != nil {
		refuse(w, http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	p.relay(w, response)
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
func (p *Proxy) setUpstreamHeaders(request, incoming *http.Request, apiKey string) error {
	clientIP := p.clientIP(incoming)
	if !isHeaderSafe(clientIP) {
		return fmt.Errorf("fingerly: ClientIP must resolve to a plain address")
	}

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
	return nil
}

// relay returns the upstream response to the browser, uncacheable and with
// only the allowlisted headers.
func (p *Proxy) relay(w http.ResponseWriter, response *http.Response) {
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
	_, _ = io.Copy(w, response.Body)
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

// isHeaderSafe reports whether a value is printable ASCII, so it cannot inject
// a header of its own.
func isHeaderSafe(value string) bool {
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// refuse writes a plain-text refusal that never reaches Fingerly.
func refuse(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, http.StatusText(status))
}
