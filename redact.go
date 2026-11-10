package fingerly

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// redacted stands in for a credential wherever an SDK value is printed.
const redacted = "[redacted]"

// Format keeps the secret key out of every fmt verb. Without it, fmt prints
// the struct's unexported fields, key included, for %v, %+v, %#v and %s.
func (c Client) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "fingerly.Client{secretKey:"+redacted+" baseURL:"+c.baseURL+"}")
}

// Format keeps the proxy key out of every fmt verb. A value receiver, so that
// printing a dereferenced Proxy is covered too.
func (p Proxy) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "fingerly.Proxy{key:"+redacted+" path:"+p.path+" upstream:"+p.upstreamURL+"}")
}

// Format keeps the keys out of every fmt verb, so logging the options a proxy
// was built with is safe.
func (o ProxyOptions) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "fingerly.ProxyOptions{ProxyKey:%s SecretKey:%s Path:%s Upstream:%s}",
		redactIfSet(o.ProxyKey), redactIfSet(o.SecretKey), o.Path, o.Upstream)
}

// MarshalJSON keeps the keys out of JSON, and leaves out the callbacks, which
// cannot be encoded.
func (o ProxyOptions) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"ProxyKey":     redactIfSet(o.ProxyKey),
		"SecretKey":    redactIfSet(o.SecretKey),
		"Path":         o.Path,
		"APIVersion":   o.APIVersion,
		"Upstream":     o.Upstream,
		"MaxBodyBytes": o.MaxBodyBytes,
		"Timeout":      o.Timeout.String(),
	})
}

// GobEncode refuses to serialise the options: gob skips the callbacks but
// would copy the keys in the clear.
func (o ProxyOptions) GobEncode() ([]byte, error) {
	return nil, errors.New("fingerly: ProxyOptions cannot be serialised")
}

func redactIfSet(value string) string {
	if value == "" {
		return ""
	}
	return redacted
}

// Format keeps the client's key out of every fmt verb; the service holds the
// client in an unexported field, which fmt prints without its methods.
func (s EventsService) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "fingerly.EventsService{}")
}
