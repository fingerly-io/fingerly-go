package fingerly

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAcceptsOnlyABareHTTPSEndpoint(t *testing.T) {
	configErr := func(opts ...Option) error {
		return New(testSecretKey, opts...).configErr
	}
	for _, endpoint := range []string{
		"http://upstream.invalid",
		"http://192.0.2.10:8080",
		"https://upstream.invalid/prefix",
		"https://upstream.invalid/a/../b",
		"https://upstream.invalid//",
		"https://upstream.invalid\n",
		"https://upstream.invalid ",
		"https://upstream.invalid\r\nx-injected: 1",
		"https://upstream.invalid?",
		"https://upstream.invalid#",
		"https://user:pass@upstream.invalid",
		"https://upstream.invalid:0",
		"ftp://upstream.invalid",
		"upstream.invalid",
	} {
		for _, opts := range [][]Option{
			{WithEndpoint(endpoint)},
			{WithEndpoint(endpoint), WithAllowInsecureLoopback()},
		} {
			err := configErr(opts...)
			if err == nil {
				t.Errorf("%q was accepted", endpoint)
				continue
			}
			for _, forbidden := range []string{"upstream.invalid", "192.0.2.10", testSecretKey} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("%q: the error repeats %q: %v", endpoint, forbidden, err)
				}
			}
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://localhost", "http://[::1]:8080"} {
		if configErr(WithEndpoint(endpoint)) == nil {
			t.Errorf("%q was accepted without WithAllowInsecureLoopback", endpoint)
		}
	}
	for endpoint, want := range map[string]string{
		"https://upstream.invalid/":     "https://upstream.invalid/api/v1",
		"https://upstream.invalid:8443": "https://upstream.invalid:8443/api/v1",
	} {
		client := New(testSecretKey, WithEndpoint(endpoint))
		if client.configErr != nil || client.baseURL != want {
			t.Errorf("%q: base URL %q, error %v", endpoint, client.baseURL, client.configErr)
		}
	}
	for endpoint, want := range map[string]string{
		"http://127.0.0.1:8080": "http://127.0.0.1:8080/api/v1",
		"http://localhost":      "http://localhost/api/v1",
		"http://[::1]:8080/":    "http://[::1]:8080/api/v1",
	} {
		client := New(testSecretKey, WithEndpoint(endpoint), WithAllowInsecureLoopback())
		if client.configErr != nil || client.baseURL != want {
			t.Errorf("%q: base URL %q, error %v", endpoint, client.baseURL, client.configErr)
		}
	}
}

// A refused or failed read names neither the endpoint nor the key.
func TestConnectionErrorsNameNeitherTheEndpointNorTheKey(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	endpoint := closed.URL
	closed.Close()
	_, err := New(testSecretKey, WithEndpoint(endpoint), WithAllowInsecureLoopback()).
		Events.Get(context.Background(), "evt_1")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, forbidden := range []string{"127.0.0.1", endpoint[strings.LastIndex(endpoint, ":")+1:], "/api/", "evt_1", testSecretKey} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error %q names %q", err, forbidden)
		}
	}
}
