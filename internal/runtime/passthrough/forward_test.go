package passthrough

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/geminicli"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type rawForwardTestExecutor struct {
	id string
}

func (e *rawForwardTestExecutor) Identifier() string { return e.id }

func (e *rawForwardTestExecutor) Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("execute should not be called")}, nil
}

func (e *rawForwardTestExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *rawForwardTestExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *rawForwardTestExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *rawForwardTestExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("executor HttpRequest should not be called by native passthrough")
}

type capturedNativeRequest struct {
	url     string
	body    string
	headers http.Header
}

func TestExecuteRawForwardPreservesBodyAndBuildsNativeURL(t *testing.T) {
	ctx := context.Background()
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&rawForwardTestExecutor{id: ProviderGemini})

	var captured capturedNativeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("ReadAll() error = %v", errRead)
		}
		captured = capturedNativeRequest{
			url:     r.URL.String(),
			body:    string(body),
			headers: r.Header.Clone(),
		}
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	if _, err := manager.Register(ctx, &cliproxyauth.Auth{
		ID:       "gemini-auth",
		Provider: ProviderGemini,
		Attributes: map[string]string{
			"base_url":          server.URL + "/gemini",
			"api_key":           "selected-key",
			"header:User-Agent": "must-not-leak",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	registerTestModel(t, "gemini-auth", ProviderGemini)
	manager.RefreshSchedulerEntry("gemini-auth")

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	resp, err := Execute(ctx, manager, Request{
		Surface:  SurfaceGemini,
		Model:    "gemini-2.5-pro",
		Method:   http.MethodPost,
		Path:     "/v1beta/models/gemini-2.5-pro:generateContent",
		RawQuery: "alt=json",
		Body:     body,
		Headers: http.Header{
			"Authorization":       {"Bearer downstream"},
			"Connection":          {"keep-alive, X-Hop"},
			"Content-Type":        {"application/json"},
			"Proxy-Authorization": {"Basic downstream"},
			"User-Agent":          {"official-client"},
			"X-Api-Key":           {"downstream-key"},
			"X-Goog-Api-Key":      {"downstream-google-key"},
			"X-Hop":               {"remove-me"},
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if resp == nil {
		t.Fatalf("Execute() response = nil")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("Body = %q, want upstream body", string(resp.Body))
	}
	if resp.Headers.Get("X-Upstream") != "ok" {
		t.Fatalf("X-Upstream = %q, want ok", resp.Headers.Get("X-Upstream"))
	}
	if captured.url != "/gemini/v1beta/models/gemini-2.5-pro:generateContent?alt=json" {
		t.Fatalf("upstream URL = %q", captured.url)
	}
	if captured.body != string(body) {
		t.Fatalf("upstream body = %q, want %q", captured.body, string(body))
	}
	if got := captured.headers.Get("Authorization"); got != "" {
		t.Fatalf("forwarded Authorization = %q, want stripped downstream auth", got)
	}
	if got := captured.headers.Get("x-goog-api-key"); got != "selected-key" {
		t.Fatalf("x-goog-api-key = %q, want selected auth key", got)
	}
	if got := captured.headers.Get("User-Agent"); got != "official-client" {
		t.Fatalf("User-Agent = %q, want original inbound header only", got)
	}
	for _, key := range []string{"Connection", "Proxy-Authorization", "X-Api-Key", "X-Hop"} {
		if got := captured.headers.Get(key); got != "" {
			t.Fatalf("forwarded %s = %q, want stripped", key, got)
		}
	}
}

func TestExecuteRawForwardRetriesNextCredentialOnQuotaStatus(t *testing.T) {
	ctx := context.Background()
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&rawForwardTestExecutor{id: ProviderGemini})

	var mu sync.Mutex
	authHeaders := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("x-goog-api-key"))
		attempt := len(authHeaders)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"quota"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	for _, entry := range []struct {
		id  string
		key string
	}{
		{id: "gemini-auth-a", key: "key-a"},
		{id: "gemini-auth-b", key: "key-b"},
	} {
		if _, err := manager.Register(ctx, &cliproxyauth.Auth{
			ID:       entry.id,
			Provider: ProviderGemini,
			Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  entry.key,
			},
		}); err != nil {
			t.Fatalf("Register(%s) error = %v", entry.id, err)
		}
		registerTestModel(t, entry.id, ProviderGemini)
		manager.RefreshSchedulerEntry(entry.id)
	}

	resp, err := Execute(ctx, manager, Request{
		Surface: SurfaceGemini,
		Model:   "gemini-2.5-pro",
		Method:  http.MethodPost,
		Path:    "/v1beta/models/gemini-2.5-pro:generateContent",
		Body:    []byte(`{"contents":[]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("StatusCode = %d, want retry success %d", resp.StatusCode, http.StatusAccepted)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("auth attempts = %v, want two credentials", authHeaders)
	}
	if authHeaders[0] == authHeaders[1] {
		t.Fatalf("auth attempts = %v, want failover to another credential", authHeaders)
	}
}

func TestExecuteRawForwardReturnsFinalUpstreamErrorResponse(t *testing.T) {
	ctx := context.Background()
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&rawForwardTestExecutor{id: ProviderGemini})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	if _, err := manager.Register(ctx, &cliproxyauth.Auth{
		ID:       "gemini-auth",
		Provider: ProviderGemini,
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "selected-key",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	registerTestModel(t, "gemini-auth", ProviderGemini)
	manager.RefreshSchedulerEntry("gemini-auth")

	resp, err := Execute(ctx, manager, Request{
		Surface: SurfaceGemini,
		Model:   "gemini-2.5-pro",
		Method:  http.MethodPost,
		Path:    "/v1beta/models/gemini-2.5-pro:generateContent",
		Body:    []byte(`{"contents":[]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("StatusCode = %d, want final upstream status %d", resp.StatusCode, http.StatusInternalServerError)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Fatalf("Body = %q, want preserved upstream error body", string(resp.Body))
	}
}

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (failingReadCloser) Close() error {
	return nil
}

func TestExecuteRawForwardRetriesNextCredentialWhenBodyReadFails(t *testing.T) {
	ctx := context.Background()
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&rawForwardTestExecutor{id: ProviderGemini})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "key-b" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("ResponseWriter does not implement Hijacker")
		}
		conn, _, errHijack := hj.Hijack()
		if errHijack != nil {
			t.Fatalf("Hijack() error = %v", errHijack)
		}
		_, _ = conn.Write([]byte("HTTP/1.1 202 Accepted\r\nContent-Length: 100\r\n\r\npartial"))
		_ = conn.Close()
	}))
	defer server.Close()

	for _, entry := range []struct {
		id  string
		key string
	}{
		{id: "gemini-auth-a", key: "key-a"},
		{id: "gemini-auth-b", key: "key-b"},
	} {
		if _, err := manager.Register(ctx, &cliproxyauth.Auth{
			ID:       entry.id,
			Provider: ProviderGemini,
			Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  entry.key,
			},
		}); err != nil {
			t.Fatalf("Register(%s) error = %v", entry.id, err)
		}
		registerTestModel(t, entry.id, ProviderGemini)
		manager.RefreshSchedulerEntry(entry.id)
	}

	resp, err := Execute(ctx, manager, Request{
		Surface: SurfaceGemini,
		Model:   "gemini-2.5-pro",
		Method:  http.MethodPost,
		Path:    "/v1beta/models/gemini-2.5-pro:generateContent",
		Body:    []byte(`{"contents":[]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusAccepted || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("Execute() response = %#v, want retry success", resp)
	}
	authA, ok := manager.GetByID("gemini-auth-a")
	if !ok {
		t.Fatalf("GetByID(gemini-auth-a) ok = false")
	}
	if authA.Success != 0 || authA.Failed != 1 {
		t.Fatalf("auth-a result counts = success %d failed %d, want 0/1", authA.Success, authA.Failed)
	}
	authB, ok := manager.GetByID("gemini-auth-b")
	if !ok {
		t.Fatalf("GetByID(gemini-auth-b) ok = false")
	}
	if authB.Success != 1 || authB.Failed != 0 {
		t.Fatalf("auth-b result counts = success %d failed %d, want 1/0", authB.Success, authB.Failed)
	}
}

func TestExecuteRawForwardRejectsUnsupportedSurface(t *testing.T) {
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	resp, err := Execute(context.Background(), manager, Request{
		Surface: "vertex",
		Model:   "gemini-2.5-pro",
	})
	if err == nil {
		t.Fatalf("Execute() error = nil, want error")
	}
	if resp != nil {
		t.Fatalf("Execute() response = %#v, want nil", resp)
	}
}

func registerTestModel(t *testing.T, authID, provider string) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: "gemini-2.5-pro"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})
}

func TestMetaStringReadsNestedTokenMap(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"token": map[string]any{"access_token": " nested-token "},
	}}
	if got := metaString(auth, "access_token"); got != "nested-token" {
		t.Fatalf("metaString() = %q, want nested-token", got)
	}
}

func TestApplyAuthOnlyDoesNotApplyCustomHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://example.test", strings.NewReader("{}"))
	auth := &cliproxyauth.Auth{
		Provider: ProviderClaude,
		Attributes: map[string]string{
			"api_key":           "selected-key",
			"header:User-Agent": "must-not-leak",
		},
	}
	if err := applyAuthOnly(context.Background(), ProviderClaude, auth, req); err != nil {
		t.Fatalf("applyAuthOnly() error = %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "selected-key" {
		t.Fatalf("x-api-key = %q, want selected-key", got)
	}
	if got := req.Header.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %q, want no custom header injection", got)
	}
}

func TestNativeHTTPClientUsesContextRoundTripper(t *testing.T) {
	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	}))
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test", nil)
	if errReq != nil {
		t.Fatalf("NewRequestWithContext() error = %v", errReq)
	}
	resp, errDo := nativeHTTPClient(ctx, nil).Do(req)
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

func TestGeminiCLIAccessTokenRefreshesExpiredTokenWithContextRoundTripper(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"access_token":  "expired-token",
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
		"expiry":        "2000-01-01T00:00:00Z",
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != googleTokenEndpoint {
			t.Fatalf("token endpoint = %q, want %q", req.URL.String(), googleTokenEndpoint)
		}
		body, errBody := io.ReadAll(req.Body)
		if errBody != nil {
			t.Fatalf("ReadAll() error = %v", errBody)
		}
		if !strings.Contains(string(body), "refresh_token=refresh-token") {
			t.Fatalf("refresh body = %q, want refresh token", string(body))
		}
		payload, errJSON := json.Marshal(map[string]any{
			"access_token":  "fresh-token",
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
		if errJSON != nil {
			t.Fatalf("Marshal() error = %v", errJSON)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
		}, nil
	}))

	token, errToken := geminiCLIAccessToken(ctx, auth)
	if errToken != nil {
		t.Fatalf("geminiCLIAccessToken() error = %v", errToken)
	}
	if token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", token)
	}
	if got := metaString(auth, "access_token"); got != "fresh-token" {
		t.Fatalf("metadata access_token = %q, want fresh-token", got)
	}
}

func TestGeminiCLIAccessTokenRefreshesWithoutExistingAccessToken(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		payload, errJSON := json.Marshal(map[string]any{
			"access_token":  "fresh-token",
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
		if errJSON != nil {
			t.Fatalf("Marshal() error = %v", errJSON)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
		}, nil
	}))

	token, errToken := geminiCLIAccessToken(ctx, auth)
	if errToken != nil {
		t.Fatalf("geminiCLIAccessToken() error = %v", errToken)
	}
	if token != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", token)
	}
	if got := metaString(auth, "access_token"); got != "fresh-token" {
		t.Fatalf("metadata access_token = %q, want fresh-token", got)
	}
}

func TestGeminiCLIAccessTokenUsesSharedVirtualCredential(t *testing.T) {
	shared := geminicli.NewSharedCredential("primary", "user@example.test", map[string]any{
		"access_token": "shared-token",
		"token_type":   "Bearer",
		"expiry":       time.Now().Add(time.Hour).Format(time.RFC3339),
	}, []string{"project-a"})
	auth := &cliproxyauth.Auth{
		Metadata: map[string]any{"access_token": "stale-virtual-token"},
		Runtime:  geminicli.NewVirtualCredential("project-a", shared),
	}

	token, errToken := geminiCLIAccessToken(context.Background(), auth)
	if errToken != nil {
		t.Fatalf("geminiCLIAccessToken() error = %v", errToken)
	}
	if token != "shared-token" {
		t.Fatalf("token = %q, want shared-token", token)
	}
}

func TestGeminiCLIAccessTokenRefreshUpdatesSharedVirtualCredential(t *testing.T) {
	shared := geminicli.NewSharedCredential("primary", "user@example.test", map[string]any{
		"refresh_token": "refresh-token",
		"token_type":    "Bearer",
	}, []string{"project-a"})
	auth := &cliproxyauth.Auth{
		Metadata: map[string]any{"access_token": "stale-virtual-token"},
		Runtime:  geminicli.NewVirtualCredential("project-a", shared),
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		payload, errJSON := json.Marshal(map[string]any{
			"access_token":  "fresh-shared-token",
			"refresh_token": "refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
		if errJSON != nil {
			t.Fatalf("Marshal() error = %v", errJSON)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(payload))),
		}, nil
	}))

	token, errToken := geminiCLIAccessToken(ctx, auth)
	if errToken != nil {
		t.Fatalf("geminiCLIAccessToken() error = %v", errToken)
	}
	if token != "fresh-shared-token" {
		t.Fatalf("token = %q, want fresh-shared-token", token)
	}
	if got := metaStringFrom(shared.MetadataSnapshot(), "access_token"); got != "fresh-shared-token" {
		t.Fatalf("shared access_token = %q, want fresh-shared-token", got)
	}
	if got := metaString(auth, "access_token"); got != "stale-virtual-token" {
		t.Fatalf("virtual auth metadata access_token = %q, want unchanged virtual metadata", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
