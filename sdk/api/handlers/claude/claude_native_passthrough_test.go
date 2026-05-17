package claude

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

type nativePassthroughClaudeExecutor struct {
	body          []byte
	url           string
	authorization string
	apiKey        string
	executeCalls  int
	streamCalls   int
}

func (e *nativePassthroughClaudeExecutor) Identifier() string { return "claude" }

func (e *nativePassthroughClaudeExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.executeCalls++
	return coreexecutor.Response{}, errors.New("Execute should not be called in native passthrough")
}

func (e *nativePassthroughClaudeExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.streamCalls++
	return nil, errors.New("ExecuteStream should not be called in native passthrough")
}

func (e *nativePassthroughClaudeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *nativePassthroughClaudeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("CountTokens should not be called in native passthrough")
}

func (e *nativePassthroughClaudeExecutor) HttpRequest(_ context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("HttpRequest should not be called in native passthrough")
}

func TestClaudeMessagesNativePassthroughPreservesBodyAndReplacesAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newClaudeNativePassthroughRouter(t, "claude-native-passthrough-model")

	body := `{"model":"claude-native-passthrough-model","messages":[{"role":"user","content":"keep me"}],"max_tokens":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
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
	assertClaudeNativePassthroughRequest(t, executor, body, "https://example.test/v1/messages")
}

func TestClaudeMessagesNativePassthroughForwardsRawStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newClaudeNativePassthroughRouter(t, "claude-native-passthrough-stream-model")

	body := `{"model":"claude-native-passthrough-stream-model","stream":true,"messages":[{"role":"user","content":"stream me"}],"max_tokens":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer downstream-stream-token")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	wantBody := "event: message_start\ndata: {}\n\n"
	if resp.Body.String() != wantBody {
		t.Fatalf("response body = %q, want raw upstream stream %q", resp.Body.String(), wantBody)
	}
	assertClaudeNativePassthroughRequest(t, executor, body, "https://example.test/v1/messages")
}

func newClaudeNativePassthroughRouter(t *testing.T, model string) (*nativePassthroughClaudeExecutor, *gin.Engine) {
	t.Helper()

	ctx := context.Background()
	executor := &nativePassthroughClaudeExecutor{}
	upstream := newClaudeNativePassthroughUpstream(t, executor)
	t.Cleanup(upstream.Close)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:       "claude-native-passthrough-auth-" + model,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "selected-claude-api-key",
			"base_url": upstream.URL,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		NativePassthrough:  true,
		PassthroughHeaders: true,
	}, manager)
	handler := NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)
	router.POST("/v1/messages/count_tokens", handler.ClaudeCountTokens)
	return executor, router
}

func assertClaudeNativePassthroughRequest(t *testing.T, executor *nativePassthroughClaudeExecutor, body, wantURL string) {
	t.Helper()
	if executor.executeCalls != 0 {
		t.Fatalf("Execute calls = %d, want 0", executor.executeCalls)
	}
	if executor.streamCalls != 0 {
		t.Fatalf("ExecuteStream calls = %d, want 0", executor.streamCalls)
	}
	if string(executor.body) != body {
		t.Fatalf("upstream body = %q, want %q", string(executor.body), body)
	}
	if !strings.HasSuffix(executor.url, strings.TrimPrefix(wantURL, "https://example.test")) {
		t.Fatalf("upstream URL = %q, want path from %q", executor.url, wantURL)
	}
	if executor.authorization != "" {
		t.Fatalf("Authorization = %q, want empty after auth replacement", executor.authorization)
	}
	if executor.apiKey != "selected-claude-api-key" {
		t.Fatalf("X-Api-Key = %q, want selected credential", executor.apiKey)
	}
}

func newClaudeNativePassthroughUpstream(t *testing.T, executor *nativePassthroughClaudeExecutor) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("ReadAll() error = %v", errRead)
		}
		executor.body = body
		executor.url = "http://" + r.Host + r.URL.String()
		executor.authorization = r.Header.Get("Authorization")
		executor.apiKey = r.Header.Get("X-Api-Key")

		bodyText := `{"native":true}`
		contentType := "application/json"
		if strings.Contains(r.URL.Path, "/v1/messages") && strings.Contains(string(body), `"stream":true`) {
			bodyText = "event: message_start\ndata: {}\n\n"
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte(bodyText))
	}))
}
