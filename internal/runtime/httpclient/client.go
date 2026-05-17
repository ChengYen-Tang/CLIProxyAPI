package httpclient

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

var proxyTransportCache sync.Map

// Options controls runtime HTTP client construction.
type Options struct {
	// ProxyURL is the fallback proxy setting used when auth.ProxyURL is empty.
	ProxyURL string
	// Timeout is applied to the returned client when greater than zero.
	Timeout time.Duration
}

// New returns an HTTP client honoring auth proxy, fallback proxy, context transport,
// and finally Go's shared default transport for direct requests.
func New(ctx context.Context, authEntry *auth.Auth, opts Options) *http.Client {
	client := &http.Client{}
	if opts.Timeout > 0 {
		client.Timeout = opts.Timeout
	}

	proxyURL := ""
	if authEntry != nil {
		proxyURL = strings.TrimSpace(authEntry.ProxyURL)
	}
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(opts.ProxyURL)
	}
	if proxyURL != "" {
		transport := proxyTransport(proxyURL)
		if transport != nil {
			client.Transport = transport
			return client
		}
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	if rt := RoundTripperFromContext(ctx); rt != nil {
		client.Transport = rt
	}
	return client
}

// RoundTripperFromContext returns the runtime-provided transport, if any.
func RoundTripperFromContext(ctx context.Context) http.RoundTripper {
	if ctx == nil {
		return nil
	}
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		return rt
	}
	return nil
}

func proxyTransport(proxyURL string) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	if cached, ok := proxyTransportCache.Load(proxyURL); ok {
		if transport, okTransport := cached.(*http.Transport); okTransport {
			return transport
		}
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	if transport == nil {
		return nil
	}
	actual, _ := proxyTransportCache.LoadOrStore(proxyURL, transport)
	if cachedTransport, ok := actual.(*http.Transport); ok {
		return cachedTransport
	}
	return transport
}
