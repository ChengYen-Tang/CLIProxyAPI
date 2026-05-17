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

type nativePassthroughGeminiCLIExecutor struct {
	body          []byte
	url           string
	authorization string
	executeCalls  int
	streamCalls   int
}

func (e *nativePassthroughGeminiCLIExecutor) Identifier() string { return "gemini-cli" }

func (e *nativePassthroughGeminiCLIExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.executeCalls++
	return coreexecutor.Response{}, errors.New("Execute should not be called in native passthrough")
}

func (e *nativePassthroughGeminiCLIExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.streamCalls++
	return nil, errors.New("ExecuteStream should not be called in native passthrough")
}

func (e *nativePassthroughGeminiCLIExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *nativePassthroughGeminiCLIExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("CountTokens should not be called in native passthrough")
}

func (e *nativePassthroughGeminiCLIExecutor) HttpRequest(_ context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("HttpRequest should not be called in native passthrough")
}

func TestGeminiCLIGenerateContentNativePassthroughPreservesBodyAndReplacesAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newGeminiCLINativePassthroughRouter(t, "gemini-cli-native-passthrough-model")

	body := `{"model":"gemini-cli-native-passthrough-model","contents":[{"role":"user","parts":[{"text":"keep cli"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:34567"
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
	assertGeminiCLINativePassthroughRequest(t, executor, body, "/v1internal:generateContent")
}

func TestGeminiCLIStreamGenerateContentNativePassthroughForwardsRawChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor, router := newGeminiCLINativePassthroughRouter(t, "gemini-cli-native-passthrough-stream-model")

	body := `{"model":"gemini-cli-native-passthrough-stream-model","contents":[{"role":"user","parts":[{"text":"stream cli"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1internal:streamGenerateContent", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:34567"
	req.Header.Set("Authorization", "Bearer downstream-stream-token")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	wantBody := "data: cli-first\n\ndata: cli-second\n\n"
	if resp.Body.String() != wantBody {
		t.Fatalf("response body = %q, want raw upstream stream %q", resp.Body.String(), wantBody)
	}
	if strings.Contains(resp.Body.String(), "data: data:") {
		t.Fatalf("response body was wrapped again: %q", resp.Body.String())
	}
	assertGeminiCLINativePassthroughRequest(t, executor, body, "/v1internal:streamGenerateContent")
}

func newGeminiCLINativePassthroughRouter(t *testing.T, model string) (*nativePassthroughGeminiCLIExecutor, *gin.Engine) {
	t.Helper()

	ctx := context.Background()
	executor := &nativePassthroughGeminiCLIExecutor{}
	upstream := newGeminiCLINativePassthroughUpstream(t, executor)
	t.Cleanup(upstream.Close)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{
		ID:       "gemini-cli-native-passthrough-auth-" + model,
		Provider: "gemini-cli",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url": upstream.URL,
		},
		Metadata: map[string]any{
			"access_token": "selected-cli-token",
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
		EnableGeminiCLIEndpoint: true,
		NativePassthrough:       true,
		PassthroughHeaders:      true,
	}, manager)
	handler := NewGeminiCLIAPIHandler(base)
	router := gin.New()
	router.POST("/v1internal:method", handler.CLIHandler)
	return executor, router
}

func assertGeminiCLINativePassthroughRequest(t *testing.T, executor *nativePassthroughGeminiCLIExecutor, body, wantPath string) {
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
	if !strings.HasSuffix(executor.url, wantPath) {
		t.Fatalf("upstream URL = %q, want suffix %q", executor.url, wantPath)
	}
	if executor.authorization != "Bearer selected-cli-token" {
		t.Fatalf("Authorization = %q, want selected credential", executor.authorization)
	}
}

func newGeminiCLINativePassthroughUpstream(t *testing.T, executor *nativePassthroughGeminiCLIExecutor) *httptest.Server {
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
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			bodyText = "data: cli-first\n\ndata: cli-second\n\n"
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte(bodyText))
	}))
}
