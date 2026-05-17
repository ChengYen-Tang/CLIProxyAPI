package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type nativePassthroughGeminiExecutor struct {
	body          []byte
	url           string
	authorization string
	apiKey        string
	executeCalls  int
	streamCalls   int
}

func (e *nativePassthroughGeminiExecutor) Identifier() string { return "gemini" }

func (e *nativePassthroughGeminiExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.executeCalls++
	return coreexecutor.Response{}, errors.New("Execute should not be called in native passthrough")
}

func (e *nativePassthroughGeminiExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.streamCalls++
	return nil, errors.New("ExecuteStream should not be called in native passthrough")
}

func (e *nativePassthroughGeminiExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *nativePassthroughGeminiExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("CountTokens should not be called in native passthrough")
}

func (e *nativePassthroughGeminiExecutor) HttpRequest(_ context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("HttpRequest should not be called in native passthrough")
}

func TestGeminiGenerateContentNativePassthroughPreservesBodyAndReplacesAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	executor := &nativePassthroughGeminiExecutor{}
	upstream := newGeminiNativePassthroughUpstream(t, executor)
	defer upstream.Close()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:       "gemini-native-passthrough-auth",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "selected-api-key",
			"base_url": upstream.URL,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	model := "gemini-native-passthrough-model"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		NativePassthrough:  true,
		PassthroughHeaders: true,
	}, manager)
	handler := NewGeminiAPIHandler(base)
	router := gin.New()
	router.POST("/v1beta/models/*action", handler.GeminiHandler)

	body := `{"contents":[{"role":"user","parts":[{"text":"keep me exactly"}]}],"generationConfig":{"temperature":0.2}}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":generateContent?alt=json", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer downstream-token")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if strings.TrimSpace(resp.Body.String()) != `{"native":true}` {
		t.Fatalf("response body = %q, want upstream body", strings.TrimSpace(resp.Body.String()))
	}
	if resp.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("X-Upstream = %q, want ok", resp.Header().Get("X-Upstream"))
	}
	if executor.executeCalls != 0 {
		t.Fatalf("Execute calls = %d, want 0", executor.executeCalls)
	}
	if executor.streamCalls != 0 {
		t.Fatalf("ExecuteStream calls = %d, want 0", executor.streamCalls)
	}
	if string(executor.body) != body {
		t.Fatalf("upstream body = %q, want %q", string(executor.body), body)
	}
	wantURL := upstream.URL + "/v1beta/models/" + model + ":generateContent?alt=json"
	if executor.url != wantURL {
		t.Fatalf("upstream URL = %q, want %q", executor.url, wantURL)
	}
	if executor.authorization != "" {
		t.Fatalf("Authorization = %q, want empty after auth replacement", executor.authorization)
	}
	if executor.apiKey != "selected-api-key" {
		t.Fatalf("X-Goog-Api-Key = %q, want selected credential", executor.apiKey)
	}
}

func TestGeminiStreamGenerateContentNativePassthroughForwardsRawChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := context.Background()
	executor := &nativePassthroughGeminiExecutor{}
	upstream := newGeminiNativePassthroughUpstream(t, executor)
	defer upstream.Close()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:       "gemini-native-passthrough-stream-auth",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "selected-stream-api-key",
			"base_url": upstream.URL,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	model := "gemini-native-passthrough-stream-model"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		NativePassthrough:  true,
		PassthroughHeaders: true,
	}, manager)
	handler := NewGeminiAPIHandler(base)
	router := gin.New()
	router.POST("/v1beta/models/*action", handler.GeminiHandler)

	body := `{"contents":[{"role":"user","parts":[{"text":"stream me exactly"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":streamGenerateContent?alt=sse", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer downstream-stream-token")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	wantBody := "data: first\n\ndata: second\n\n"
	if resp.Body.String() != wantBody {
		t.Fatalf("response body = %q, want raw upstream stream %q", resp.Body.String(), wantBody)
	}
	if strings.Contains(resp.Body.String(), "data: data:") {
		t.Fatalf("response body was wrapped again: %q", resp.Body.String())
	}
	if resp.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("X-Upstream = %q, want ok", resp.Header().Get("X-Upstream"))
	}
	if executor.executeCalls != 0 {
		t.Fatalf("Execute calls = %d, want 0", executor.executeCalls)
	}
	if executor.streamCalls != 0 {
		t.Fatalf("ExecuteStream calls = %d, want 0", executor.streamCalls)
	}
	if string(executor.body) != body {
		t.Fatalf("upstream body = %q, want %q", string(executor.body), body)
	}
	wantURL := upstream.URL + "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	if executor.url != wantURL {
		t.Fatalf("upstream URL = %q, want %q", executor.url, wantURL)
	}
	if executor.authorization != "" {
		t.Fatalf("Authorization = %q, want empty after auth replacement", executor.authorization)
	}
	if executor.apiKey != "selected-stream-api-key" {
		t.Fatalf("X-Goog-Api-Key = %q, want selected credential", executor.apiKey)
	}
}

func newGeminiNativePassthroughUpstream(t *testing.T, executor *nativePassthroughGeminiExecutor) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("ReadAll() error = %v", errRead)
		}
		executor.body = body
		executor.url = "http://" + r.Host + r.URL.String()
		executor.authorization = r.Header.Get("Authorization")
		executor.apiKey = r.Header.Get("X-Goog-Api-Key")

		bodyText := `{"native":true}`
		contentType := "application/json"
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			bodyText = "data: first\n\ndata: second\n\n"
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte(bodyText))
	}))
}
