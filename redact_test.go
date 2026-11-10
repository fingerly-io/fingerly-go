package fingerly

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestPrintingSDKValuesNeverShowsAKey(t *testing.T) {
	options := ProxyOptions{ProxyKey: testProxyKey, ClientIP: func(*http.Request) string { return "" }}
	proxy, err := NewProxy(options)
	if err != nil {
		t.Fatal(err)
	}
	client := New(testSecretKey)
	values := map[string]any{
		"*Proxy": proxy, "Proxy": *proxy, "*Client": client, "Client": *client,
		"ProxyOptions": options, "*ProxyOptions": &options, "Events": client.Events,
	}
	for name, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
			out := fmt.Sprintf(verb, value)
			if strings.Contains(out, testProxyKey) || strings.Contains(out, testSecretKey) ||
				strings.Contains(out, fmt.Sprintf("%x", testSecretKey)) || strings.Contains(out, fmt.Sprintf("%x", testProxyKey)) {
				t.Errorf("%s with %s prints a key: %s", name, verb, out)
			}
		}
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), testProxyKey) || strings.Contains(string(encoded), testSecretKey) {
			t.Errorf("%s as JSON carries a key: %s", name, encoded)
		}
		var logged strings.Builder
		slog.New(slog.NewTextHandler(&logged, nil)).Info("x", "value", value)
		slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "value", value)
		if strings.Contains(logged.String(), testProxyKey) || strings.Contains(logged.String(), testSecretKey) {
			t.Errorf("%s in slog carries a key: %s", name, logged.String())
		}
	}
	if !strings.Contains(fmt.Sprintf("%v", options), "[redacted]") {
		t.Errorf("options do not show the key as redacted")
	}
}

// gob copies exported string fields and skips the callbacks, so without a
// refusal the options would serialise with their keys in the clear.
func TestSerialisingKeysIsRefused(t *testing.T) {
	options := ProxyOptions{ProxyKey: testProxyKey, ClientIP: func(*http.Request) string { return "" }}
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(options); err == nil {
		t.Errorf("ProxyOptions was serialised")
	}
	if strings.Contains(encoded.String(), testProxyKey) {
		t.Errorf("the proxy key was serialised")
	}
	encoded.Reset()
	if err := gob.NewEncoder(&encoded).Encode(New(testSecretKey)); err == nil {
		t.Errorf("a Client was serialised")
	}
	if strings.Contains(encoded.String(), testSecretKey) {
		t.Errorf("the secret key was serialised")
	}
}
