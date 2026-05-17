package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type nativePassthroughCodexExecutor struct {
	body          []byte
	url           string
	authorization string
	executeCalls  int
	streamCalls   int
	httpCalls     int
}

func (e *nativePassthroughCodexExecutor) Identifier() string { return "codex" }

func (e *nativePassthroughCodexExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.executeCalls++
	return coreexecutor.Response{}, errors.New("Execute should not be called in native passthrough")
}

func (e *nativePassthroughCodexExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.streamCalls++
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-ws","output":[{"type":"message","id":"out-ws"}]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *nativePassthroughCodexExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *nativePassthroughCodexExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("CountTokens should not be called in native passthrough")
}

func (e *nativePassthroughCodexExecutor) HttpRequest(_ context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	e.httpCalls++
	return nil, errors.New("HttpRequest should not be called in native passthrough")
}

func TestOpenAIResponsesNativePassthroughPreservesBodyAndReplacesAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newOpenAIResponsesNativePassthroughRouter(t, "codex-native-passthrough-model")

	body := `{"model":"codex-native-passthrough-model","input":"keep me"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	assertCodexNativePassthroughRequest(t, executor, body, "https://example.test/responses")
}

func TestOpenAIResponsesNativePassthroughForwardsRawStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newOpenAIResponsesNativePassthroughRouter(t, "codex-native-passthrough-stream-model")

	body := `{"model":"codex-native-passthrough-stream-model","stream":true,"input":"stream me"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer downstream-stream-token")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	wantBody := "event: response.created\ndata: {}\n\n"
	if resp.Body.String() != wantBody {
		t.Fatalf("response body = %q, want raw upstream stream %q", resp.Body.String(), wantBody)
	}
	assertCodexNativePassthroughRequest(t, executor, body, "https://example.test/responses")
}

func TestOpenAIResponsesCompactNativePassthroughPreservesStreamFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newOpenAIResponsesNativePassthroughRouter(t, "codex-native-passthrough-compact-model")

	body := `{"model":"codex-native-passthrough-compact-model","stream":false,"input":"compact me"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer downstream-token")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	assertCodexNativePassthroughRequest(t, executor, body, "https://example.test/responses/compact")
}

func TestOpenAIResponsesWebsocketExcludedFromNativePassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newOpenAIResponsesNativePassthroughRouter(t, "codex-native-passthrough-ws-model")

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	request := `{"type":"response.create","model":"codex-native-passthrough-ws-model","input":[{"type":"message","id":"msg-1"}]}`
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
		t.Fatalf("write websocket message: %v", errWrite)
	}
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read websocket message: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != wsEventTypeCompleted {
		t.Fatalf("payload type = %q, want %q; payload = %s", got, wsEventTypeCompleted, string(payload))
	}
	if executor.httpCalls != 0 {
		t.Fatalf("HttpRequest calls = %d, want 0 for websocket exclusion", executor.httpCalls)
	}
	if executor.streamCalls != 1 {
		t.Fatalf("ExecuteStream calls = %d, want 1 for websocket path", executor.streamCalls)
	}
}

func newOpenAIResponsesNativePassthroughRouter(t *testing.T, model string) (*nativePassthroughCodexExecutor, *gin.Engine) {
	t.Helper()

	ctx := context.Background()
	executor := &nativePassthroughCodexExecutor{}
	upstream := newCodexNativePassthroughUpstream(t, executor)
	t.Cleanup(upstream.Close)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:       "codex-native-passthrough-auth-" + model,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "selected-codex-api-key",
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
	handler := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	router.POST("/v1/responses/compact", handler.Compact)
	return executor, router
}

func assertCodexNativePassthroughRequest(t *testing.T, executor *nativePassthroughCodexExecutor, body, wantURL string) {
	t.Helper()
	if executor.executeCalls != 0 {
		t.Fatalf("Execute calls = %d, want 0", executor.executeCalls)
	}
	if executor.streamCalls != 0 {
		t.Fatalf("ExecuteStream calls = %d, want 0", executor.streamCalls)
	}
	if executor.httpCalls != 0 {
		t.Fatalf("HttpRequest calls = %d, want 0", executor.httpCalls)
	}
	if string(executor.body) != body {
		t.Fatalf("upstream body = %q, want %q", string(executor.body), body)
	}
	if !strings.HasSuffix(executor.url, strings.TrimPrefix(wantURL, "https://example.test")) {
		t.Fatalf("upstream URL = %q, want path from %q", executor.url, wantURL)
	}
	if executor.authorization != "Bearer selected-codex-api-key" {
		t.Fatalf("Authorization = %q, want selected credential", executor.authorization)
	}
}

func newCodexNativePassthroughUpstream(t *testing.T, executor *nativePassthroughCodexExecutor) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("ReadAll() error = %v", errRead)
		}
		executor.body = body
		executor.url = "http://" + r.Host + r.URL.String()
		executor.authorization = r.Header.Get("Authorization")

		bodyText := `{"native":true}`
		contentType := "application/json"
		if strings.Contains(string(body), `"stream":true`) {
			bodyText = "event: response.created\ndata: {}\n\n"
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte(bodyText))
	}))
}
