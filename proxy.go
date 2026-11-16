package fingerly

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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

// clientMetadataHeaders are browser fetch-metadata and client-hint headers
// forwarded verbatim when present on the incoming request.
var clientMetadataHeaders = []string{
	"Sec-Fetch-Site",
	"Sec-Fetch-Mode",
	"Sec-Fetch-Dest",
	"Sec-CH-UA",
	"Sec-CH-UA-Mobile",
	"Sec-CH-UA-Platform",
}

const (
	corsAllowHeaders  = "content-type, x-api-key, idempotency-key, x-fingerly-sdk-platform"
	corsExposeHeaders = "x-request-id, ratelimit-limit, ratelimit-remaining, retry-after"
	missingOriginBody = `{"error":{"code":"missing_origin","message":"Origin header is required","status":400}}`
)

// validSDKPlatform returns the validated native or bridge platform identifier
// when incoming X-Fingerly-SDK-Platform contains a single exact lowercase match.
func validSDKPlatform(values []string) string {
	if len(values) != 1 {
		return ""
	}
	switch values[0] {
	case "ios",
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
		"kotlin-multiplatform":
		return values[0]
	default:
		return ""
	}
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
	// Upstream overrides the API origin. It must be a bare https:// origin:
	// scheme, host and optional port, with no path, query, fragment,
	// credentials, space or control byte.
	Upstream string
	// AllowInsecureLoopback accepts a plain http:// Upstream on 127.0.0.1,
	// localhost or [::1], for a local test harness only. Any other http://
	// host is refused whatever this is set to.
	AllowInsecureLoopback bool
	// MaxBodyBytes refuses larger request bodies with 413. Defaults to 1 MiB.
	MaxBodyBytes int64
	// Timeout bounds the upstream call. Defaults to 5 seconds.
	Timeout time.Duration
	// AllowedOrigins lists origins permitted to make cross-origin browser
	// requests to this proxy. Leave empty for same-origin routes.
	AllowedOrigins []string
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
// It implements http.Handler, so every router built on net/http takes it
// directly:
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
	key            string
	scope          keyScope
	usesProxyKey   bool
	upstreamURL    string
	path           string
	maxBodyBytes   int64
	allowedOrigins map[string]struct{}
	clientIP       func(*http.Request) string
	httpClient     *http.Client
	onError        func(*http.Request, *ProxyError)
	// badClientIP reports an invalid ClientIP once, not once per request. A
	// pointer so that Proxy has no lock to copy and Format can take a value.
	badClientIP *sync.Once
}

// errNoClientIP is the configuration error for a ClientIP that returned
// something other than one address.
var errNoClientIP = errors.New("the client IP callback returned no valid address")

// ProxyError is an upstream failure the proxy answered for. It never carries
// a key or a response body.
type ProxyError struct {
	// Status is what the browser was answered: 502, 504 or 500.
	Status int
	// Err is the kind of failure: a timeout, an unreachable upstream, a TLS
	// failure, an unrelayable status or answer. It never names the upstream
	// host, port or URL.
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
		badClientIP:  &sync.Once{},
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
	origin, err := normalizeUpstream(upstream, options.AllowInsecureLoopback)
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

	if len(options.AllowedOrigins) > 0 {
		allowed, err := parseAllowedOrigins(options.AllowedOrigins)
		if err != nil {
			return nil, err
		}
		proxy.allowedOrigins = allowed
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
	// EscapedPath, not Path: Path has already decoded %2F into a slash.
	if p.path != "" && r.URL.EscapedPath() != p.path {
		refuse(w, http.StatusNotFound)
		return
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	cors := p.matchedOrigin(origin)
	if cors != "" {
		applyCORS(w.Header(), cors)
	}
	if r.Method == http.MethodOptions && cors != "" {
		w.Header().Set("Allow", "POST, OPTIONS")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		if len(p.allowedOrigins) > 0 {
			w.Header().Set("Allow", "POST, OPTIONS")
		} else {
			w.Header().Set("Allow", http.MethodPost)
		}
		refuse(w, http.StatusMethodNotAllowed)
		return
	}

	apiKey, ok := p.upstreamKeyFor(r.Header.Get("X-API-Key"))
	if !ok {
		refuse(w, http.StatusUnauthorized)
		return
	}

	// Sanitize-and-restore Step 1-4: capture validated X-Fingerly-SDK-Platform
	// before stripping incoming X-Fingerly-* headers, and reject with 400
	// missing_origin iff both Origin and validated platform are empty.
	sdkPlatform := validSDKPlatform(r.Header.Values("X-Fingerly-SDK-Platform"))
	if origin == "" && sdkPlatform == "" {
		refuseJSON(w, http.StatusBadRequest, missingOriginBody)
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
	p.setUpstreamHeaders(request, r, apiKey, clientIP, origin, sdkPlatform)

	response, err := p.httpClient.Do(request)
	if err != nil {
		p.fail(w, r, err)
		return
	}
	// http.Transport accepts any three digits as a status, and WriteHeader
	// panics on a code under 100. Nothing outside 2xx to 5xx is an answer, and
	// the API never redirects: a 3xx is a wrong or tampered upstream.
	if response.StatusCode < 200 || response.StatusCode > 599 ||
		(response.StatusCode >= 300 && response.StatusCode < 400) {
		_ = response.Body.Close()
		p.fail(w, r, errUpstreamStatus)
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

// fail answers an upstream failure: 504 when a deadline passed, whether this
// proxy's or one a middleware put on the request, and 502 for anything else. A
// visitor who went away gets nothing: the upstream call was cancelled with
// their request, and there is no one left to answer.
func (p *Proxy) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	status := http.StatusBadGateway
	if isTimeout(err) {
		status = http.StatusGatewayTimeout
	}
	p.report(r, &ProxyError{Status: status, Err: failureKind(err)})
	refuse(w, status)
}

// Upstream failure kinds. A transport error names the upstream URL, host and
// port, so what is reported is the kind of failure only.
var (
	errUpstreamTimeout     = fmt.Errorf("upstream timed out: %w", context.DeadlineExceeded)
	errUpstreamUnreachable = errors.New("upstream unreachable")
	errUpstreamTLS         = errors.New("upstream TLS handshake failed")
	errUpstreamStatus      = errors.New("upstream answered an unrelayable status")
	errUpstreamUnreadable  = errors.New("upstream answer unreadable")
)

// failureKind maps an upstream failure to one of the kinds above.
func failureKind(err error) error {
	switch {
	case errors.Is(err, errUpstreamTooLarge), errors.Is(err, errUpstreamStatus),
		errors.Is(err, errUpstreamTimeout):
		return err
	case isTimeout(err):
		return errUpstreamTimeout
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		// Not a transport error: the body broke while it was read.
		return errUpstreamUnreadable
	}
	var tlsRecord tls.RecordHeaderError
	var tlsVerify *tls.CertificateVerificationError
	var opErr *net.OpError
	switch {
	case errors.As(err, &tlsRecord), errors.As(err, &tlsVerify):
		return errUpstreamTLS
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return errUpstreamUnreachable
	}
	return errUpstreamUnreadable
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

// setUpstreamHeaders builds the headers for the forwarded request from scratch
// so incoming X-Fingerly-*, Cookie, and Authorization headers never pass on.
func (p *Proxy) setUpstreamHeaders(
	request, incoming *http.Request,
	apiKey, clientIP, origin, sdkPlatform string,
) {
	contentType := incoming.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-API-Key", apiKey)
	request.Header.Set("X-Fingerly-Client-IP", clientIP)
	request.Header.Set("X-Fingerly-User-Agent", incoming.Header.Get("User-Agent"))
	request.Header.Set("X-Fingerly-Proxy-Version", proxyVersion)
	request.Header.Set("User-Agent", userAgent)
	if p.usesProxyKey {
		request.Header.Set("X-Fingerly-Proxy-Key", p.key)
	}
	// One channel only: the API refuses a proxied request that names both, and
	// a non-empty origin is the browser channel, which is what the API picks
	// for the same request sent directly.
	if origin != "" {
		request.Header.Set("X-Fingerly-Origin", origin)
	} else if sdkPlatform != "" {
		request.Header.Set("X-Fingerly-SDK-Platform", sdkPlatform)
	}
	host := strings.TrimSpace(incoming.Host)
	if host == "" {
		host = strings.TrimSpace(incoming.Header.Get("Host"))
	}
	if host != "" {
		request.Header.Set("X-Fingerly-Host", host)
	}
	if idempotency := incoming.Header.Get("Idempotency-Key"); idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	for _, name := range clientMetadataHeaders {
		if value := incoming.Header.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}
}

func (p *Proxy) matchedOrigin(origin string) string {
	if origin == "" || len(p.allowedOrigins) == 0 {
		return ""
	}
	if _, ok := p.allowedOrigins[origin]; ok {
		return origin
	}
	return ""
}

func parseAllowedOrigins(origins []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(origins))
	for _, raw := range origins {
		normalized, err := normalizeOrigin(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("fingerly: invalid AllowedOrigins entry: %w", err)
		}
		parsed, err := url.Parse(normalized)
		if err != nil || parsed.Path != "" {
			return nil, errors.New("fingerly: AllowedOrigins entries must be bare HTTP(S) origins")
		}
		out[normalized] = struct{}{}
	}
	return out, nil
}

func applyCORS(header http.Header, origin string) {
	header.Set("Access-Control-Allow-Origin", origin)
	header.Set("Vary", "Origin")
	header.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	header.Set("Access-Control-Allow-Headers", corsAllowHeaders)
	header.Set("Access-Control-Expose-Headers", corsExposeHeaders)
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

// normalizePath validates the path the dashboard issued, dropping a trailing
// slash from the configured value. Requests are matched against it exactly:
// no prefix, no extra or trailing slash, no dot segment, no encoded
// character, and case matters.
func normalizePath(value string) (string, error) {
	path := strings.TrimRight(strings.TrimSpace(value), "/")
	invalid := errors.New("fingerly: Path must be an absolute URL path such as /k7x2qm4a")
	if !strings.HasPrefix(path, "/") || len(path) < 2 {
		return "", invalid
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", invalid
		}
		for _, c := range segment {
			if !isUnreserved(c) {
				return "", invalid
			}
		}
	}
	return path, nil
}

// isUnreserved reports whether c may appear in a URL path unencoded.
func isUnreserved(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
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

// refuseJSON writes status and a structured JSON error body, uncacheable.
func refuseJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
