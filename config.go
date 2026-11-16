package fingerly

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Key kinds Fingerly issues: browser public, server secret, and proxy.
const (
	keyPublic = "pk"
	keySecret = "sk"
	keyProxy  = "px"
)

// keyPattern is anchored with \A and \z rather than ^ and $, because Go's $
// also matches before a trailing newline.
var keyPattern = regexp.MustCompile(
	`\Afly_(pk|sk|px)_(production|staging|development)_[A-Za-z0-9_-]+\z`,
)

// defaultEndpoint is the one Fingerly API origin.
const defaultEndpoint = "https://api.fingerly.io"

var apiVersionPattern = regexp.MustCompile(`\Av[1-9][0-9]*\z`)

var keyLabels = map[string]string{
	keyPublic: "public",
	keySecret: "secret",
	keyProxy:  "proxy",
}

// keyScope is the environment a Fingerly key is scoped to.
type keyScope struct {
	Environment string // production, staging or development.
}

// parseKey parses a Fingerly key and asserts it is of the expected kind.
//
// It returns an error rather than a scope when the key is malformed or of
// another kind, so a public, proxy or management key can never reach an API
// that would accept it.
func parseKey(key, expected string) (keyScope, error) {
	match := keyPattern.FindStringSubmatch(key)
	if match == nil || match[1] != expected {
		return keyScope{}, fmt.Errorf(
			"fingerly: a valid Fingerly %s key is required", keyLabels[expected],
		)
	}
	return keyScope{Environment: match[2]}, nil
}

// normalizeOrigin validates a caller-supplied API origin.
//
// Credentials, queries and fragments are refused: they would be attached to
// every authenticated request, and embedded credentials would leak on a
// redirect. The returned origin has no trailing slash.
func normalizeOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("fingerly: endpoint is not a URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf(
			"fingerly: endpoint must be an HTTP(S) URL without credentials, query, or fragment",
		)
	}
	return strings.TrimRight(value, "/"), nil
}

// upstreamPattern is the byte range a proxy upstream may use: printable ASCII,
// no space, no control byte, so a stray newline or CRLF cannot pass.
var upstreamPattern = regexp.MustCompile(`\A[\x21-\x7e]+\z`)

// errUpstreamOption is the one error for a malformed proxy upstream. It never
// repeats the value, which may be a mistyped secret or an internal address.
var errUpstreamOption = errors.New(
	"fingerly: Upstream must be a bare https:// origin (scheme, host and optional port only)",
)

// normalizeUpstream validates the proxy's upstream origin: scheme, host and
// optional port only, rebuilt from the parsed parts. HTTPS is required; plain
// HTTP is accepted only for 127.0.0.1, localhost or [::1], and only when
// allowInsecureLoopback is set for a local test harness.
func normalizeUpstream(value string, allowInsecureLoopback bool) (string, error) {
	if !upstreamPattern.MatchString(value) ||
		strings.ContainsAny(value, "?#") {
		return "", errUpstreamOption
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return "", errUpstreamOption
	}
	host := parsed.Hostname()
	if host == "" {
		return "", errUpstreamOption
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errUpstreamOption
		}
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowInsecureLoopback || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			return "", errors.New(
				"fingerly: a plain http:// Upstream is allowed only for 127.0.0.1, localhost or [::1] with AllowInsecureLoopback",
			)
		}
	default:
		return "", errUpstreamOption
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// normalizeAPIVersion validates a route version before it is put in a URL.
func normalizeAPIVersion(version string) (string, error) {
	if !apiVersionPattern.MatchString(version) {
		return "", fmt.Errorf(`fingerly: APIVersion must be a version such as "v1" or "v2"`)
	}
	return version, nil
}

// positiveDuration validates a duration option.
func positiveDuration(value time.Duration, name string) (time.Duration, error) {
	if value <= 0 {
		return 0, fmt.Errorf("fingerly: %s must be a positive duration", name)
	}
	return value, nil
}
