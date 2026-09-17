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
	defaultProxyPrefix  = "/api/fingerly"
	defaultMaxBodyBytes = 1 << 20 // 1 MiB
	defaultProxyTimeout = 5 * time.Second
)

// connectionHeaders describe the upstream connection rather than our response.
// Go's transport has already framed and decoded the body, so replaying them
// would misdescribe the bytes handed back.
var connectionHeaders = []string{
	"Connection",
	"Content-Encoding",
	"Content-Length",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
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

	// Prefix is the path the proxy is mounted under. It defaults to
	// "/api/fingerly" and must match the mount.
	Prefix string
	// APIVersion is accepted locally and forwarded upstream. Defaults to "v1".
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

// Proxy serves the browser SDK's identify and supplement calls from a path on
// your own domain, so content blockers cannot refuse them as third-party.
//
// It implements http.Handler, so every Go router takes it directly:
//
//	proxy, err := fingerly.NewProxy(fingerly.ProxyOptions{
//		ProxyKey: os.Getenv("FINGERLY_PROXY_KEY"),
//		Prefix:   "/metrics",
//		ClientIP: func(r *http.Request) string { return r.Header.Get("CF-Connecting-IP") },
//	})
//	mux.Handle("/metrics/", proxy)
//
// Only those two routes are forwarded, never event reads: reading events needs
// a secret key and must stay unreachable from a browser. Customer cookies and
// authorization headers are never passed on.
type Proxy struct {
	key          string
	scope        keyScope
	usesProxyKey bool
	upstream     string
	prefix       string
	apiVersion   string
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
		upstream = regionalOrigin(scope)
	}
	if proxy.upstream, err = normalizeOrigin(upstream); err != nil {
		return nil, err
	}

	version := options.APIVersion
	if version == "" {
		version = defaultAPIVersion
	}
	if proxy.apiVersion, err = normalizeAPIVersion(version); err != nil {
		return nil, err
	}

	prefix := options.Prefix
	if prefix == "" {
		prefix = defaultProxyPrefix
	}
	if proxy.prefix, err = normalizePrefix(prefix); err != nil {
		return nil, err
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
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		refuse(w, http.StatusMethodNotAllowed)
		return
	}

	upstreamPath, ok := p.routeFor(r.URL.Path)
	if !ok {
		refuse(w, http.StatusNotFound)
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
		r.Context(), http.MethodPost, p.upstream+upstreamPath, bytes.NewReader(body),
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

// routeFor maps a local request path to its upstream path.
//
// Only identify and supplement are proxyable. The prefix is tolerated as
// either present or already stripped, so the handler works whether it is
// mounted directly or behind http.StripPrefix.
func (p *Proxy) routeFor(path string) (string, bool) {
	versioned := "/api/" + p.apiVersion
	resource, ok := strings.CutPrefix(path, p.prefix+versioned)
	if !ok {
		// http.StripPrefix removed the mount path before we saw it.
		if resource, ok = strings.CutPrefix(path, versioned); !ok {
			return "", false
		}
	}
	if resource != "/identify" && !isSupplementPath(resource) {
		return "", false
	}
	return versioned + resource, true
}

// isSupplementPath reports whether a resource is /events/{request_id}/supplement
// with exactly one id segment.
func isSupplementPath(resource string) bool {
	requestID, ok := strings.CutPrefix(resource, "/events/")
	if !ok {
		return false
	}
	requestID, ok = strings.CutSuffix(requestID, "/supplement")
	return ok && requestID != "" && !strings.Contains(requestID, "/")
}

// upstreamKeyFor decides which key to present upstream.
//
// With a proxy key the browser must supply a public key for the same region
// and environment, which the proxy forwards alongside its own. The deprecated
// secret-key mode authenticates on its own.
func (p *Proxy) upstreamKeyFor(presented string) (string, bool) {
	if !p.usesProxyKey {
		return p.key, true
	}
	browser, err := parseKey(presented, keyPublic)
	if err != nil {
		return "", false
	}
	if browser.Region != p.scope.Region || browser.Environment != p.scope.Environment {
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
	request.Header.Set("User-Agent", userAgent)
	if p.usesProxyKey {
		request.Header.Set("X-Fingerly-Proxy-Key", p.key)
	}
	if idempotency := incoming.Header.Get("Idempotency-Key"); idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	return nil
}

// relay returns the upstream response to the browser, uncacheable and without
// the headers that described the upstream connection.
func (p *Proxy) relay(w http.ResponseWriter, response *http.Response) {
	header := w.Header()
	for name, values := range response.Header {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	for _, name := range connectionHeaders {
		header.Del(name)
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

// normalizePrefix validates the local mount path and drops trailing slashes.
func normalizePrefix(value string) (string, error) {
	prefix := strings.TrimRight(value, "/")
	if !strings.HasPrefix(prefix, "/") ||
		strings.ContainsAny(prefix, "?#") {
		return "", errors.New("fingerly: Prefix must be an absolute URL path")
	}
	return prefix, nil
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
