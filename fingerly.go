// Package fingerly is the server-side Fingerly SDK.
//
// It reads stored identification events with a secret key, verifies signed
// webhooks, and serves the browser SDK's requests from a route on your own
// domain. It has no dependencies outside the standard library.
//
//	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))
//	event, err := client.Events.Get(ctx, requestID)
//
// Secret keys and webhook secrets belong in server-side code only. A browser
// must never see either.
package fingerly

import (
	"net/http"
	"time"
)

// Version is the SDK version, reported to the API on every request.
const Version = "0.1.0"

const (
	defaultTimeout    = 10 * time.Second
	defaultAPIVersion = "v1"
)

// Client reads events for one organization and environment.
//
// Build one with New and reuse it: it holds a pooled HTTP connection and is
// safe for concurrent use.
type Client struct {
	// Events reads stored events under this client's secret key.
	Events *EventsService

	secretKey  string
	baseURL    string
	timeout    time.Duration
	httpClient *http.Client

	// configErr records an invalid option. Every call returns it instead of
	// reaching the network, so a bad key can never leave the process.
	configErr error
}

// Option configures a Client. Pass options to New.
type Option func(*clientConfig)

type clientConfig struct {
	endpoint   string
	apiVersion string
	timeout    time.Duration
	httpClient *http.Client
}

// WithEndpoint overrides the regional origin implied by the key, for a local
// or self-hosted API. The key still decides which kind of key is accepted.
func WithEndpoint(endpoint string) Option {
	return func(c *clientConfig) { c.endpoint = endpoint }
}

// WithAPIVersion selects a route version, such as "v2". The default is "v1",
// and the server must support whichever is chosen.
func WithAPIVersion(version string) Option {
	return func(c *clientConfig) { c.apiVersion = version }
}

// WithTimeout sets the deadline for each request, covering the response body.
// The default is 10 seconds. A deadline already on the caller's context still
// applies, whichever is sooner.
func WithTimeout(timeout time.Duration) Option {
	return func(c *clientConfig) { c.timeout = timeout }
}

// WithHTTPClient supplies the HTTP client, for a custom transport or for tests.
//
// The client is copied and its CheckRedirect replaced, so a redirect can never
// replay the secret key to another host. The Transport is shared, so
// connection pooling is preserved.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *clientConfig) { c.httpClient = httpClient }
}

// New builds a client for a server-only secret key,
// fly_sk_{eu|us}_{environment}_…, whose prefix selects the regional API and
// the environment read.
//
// New never returns an error, so it composes in a package-level var. An
// invalid key or option is reported by the first call made on the client,
// before any request is sent.
func New(secretKey string, opts ...Option) *Client {
	config := clientConfig{apiVersion: defaultAPIVersion, timeout: defaultTimeout}
	for _, apply := range opts {
		apply(&config)
	}

	client := &Client{secretKey: secretKey}
	client.Events = &EventsService{client: client}

	// Validate the key even when an endpoint is given, so a public, proxy or
	// management key can never reach an API that would accept it.
	scope, err := parseKey(secretKey, keySecret)
	if err != nil {
		client.configErr = err
		return client
	}
	origin := config.endpoint
	if origin == "" {
		origin = regionalOrigin(scope)
	}
	origin, err = normalizeOrigin(origin)
	if err != nil {
		client.configErr = err
		return client
	}
	version, err := normalizeAPIVersion(config.apiVersion)
	if err != nil {
		client.configErr = err
		return client
	}
	timeout, err := positiveDuration(config.timeout, "Timeout")
	if err != nil {
		client.configErr = err
		return client
	}

	client.baseURL = origin + "/api/" + version
	client.timeout = timeout
	client.httpClient = noRedirectClient(config.httpClient)
	return client
}

// noRedirectClient returns a client that refuses to follow redirects, so an
// authenticated request is never replayed to another host.
func noRedirectClient(base *http.Client) *http.Client {
	client := &http.Client{}
	if base != nil {
		copied := *base
		client = &copied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}
