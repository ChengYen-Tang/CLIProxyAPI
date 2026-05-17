package httpclient

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNewDirectUsesSharedDefaultTransport(t *testing.T) {
	t.Parallel()

	client := New(context.Background(), nil, Options{})
	if client.Transport != nil {
		t.Fatalf("Transport = %T, want nil shared default transport", client.Transport)
	}
}

func TestNewUsesContextRoundTripper(t *testing.T) {
	t.Parallel()

	called := false
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(rt))

	client := New(ctx, nil, Options{})
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test", nil)
	if errReq != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errReq)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		t.Fatalf("Do() error = %v", errDo)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if !called {
		t.Fatalf("context round tripper was not used")
	}
}

func TestNewAuthProxyOverridesFallbackProxy(t *testing.T) {
	t.Parallel()

	client := New(context.Background(), &cliproxyauth.Auth{ProxyURL: "direct"}, Options{
		ProxyURL: "http://global-proxy.example.com:8080",
	})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("Proxy is configured, want direct transport")
	}
}

func TestNewCachesProxyTransport(t *testing.T) {
	t.Parallel()

	authEntry := &cliproxyauth.Auth{ProxyURL: "direct"}
	first := New(context.Background(), authEntry, Options{})
	second := New(context.Background(), authEntry, Options{})
	if first.Transport == nil || second.Transport == nil {
		t.Fatalf("transports = %T/%T, want cached direct transports", first.Transport, second.Transport)
	}
	if first.Transport != second.Transport {
		t.Fatalf("transports are not reused: %p != %p", first.Transport, second.Transport)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
