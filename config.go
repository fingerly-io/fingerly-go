package fingerly

import (
	"fmt"
	"net/url"
	"regexp"
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
