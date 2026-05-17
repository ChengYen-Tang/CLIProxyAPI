package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type selectionTestExecutor struct {
	id           string
	executeCalls int
}

func (e *selectionTestExecutor) Identifier() string { return e.id }

func (e *selectionTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeCalls++
	return cliproxyexecutor.Response{}, nil
}

func (e *selectionTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *selectionTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *selectionTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *selectionTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerSelectChoosesProviderWithoutExecuting(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	geminiExecutor := &selectionTestExecutor{id: "gemini"}
	claudeExecutor := &selectionTestExecutor{id: "claude"}
	manager.RegisterExecutor(geminiExecutor)
	manager.RegisterExecutor(claudeExecutor)

	if _, err := manager.Register(ctx, &Auth{ID: "gemini-auth", Provider: "gemini"}); err != nil {
		t.Fatalf("Register(gemini) error = %v", err)
	}
	if _, err := manager.Register(ctx, &Auth{ID: "claude-auth", Provider: "claude"}); err != nil {
		t.Fatalf("Register(claude) error = %v", err)
	}

	selection, errSelect := manager.Select(ctx, []string{"gemini"}, "", cliproxyexecutor.Options{}, nil)
	if errSelect != nil {
		t.Fatalf("Select() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil {
		t.Fatalf("Select() returned nil selection/auth")
	}
	if selection.Auth.ID != "gemini-auth" {
		t.Fatalf("selection.Auth.ID = %q, want %q", selection.Auth.ID, "gemini-auth")
	}
	if selection.Provider != "gemini" {
		t.Fatalf("selection.Provider = %q, want %q", selection.Provider, "gemini")
	}
	if geminiExecutor.executeCalls != 0 || claudeExecutor.executeCalls != 0 {
		t.Fatalf("Select() executed providers: gemini=%d claude=%d", geminiExecutor.executeCalls, claudeExecutor.executeCalls)
	}
}

func TestManagerSelectSkipsTriedAuths(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&selectionTestExecutor{id: "gemini"})

	if _, err := manager.Register(ctx, &Auth{ID: "auth-a", Provider: "gemini"}); err != nil {
		t.Fatalf("Register(auth-a) error = %v", err)
	}
	if _, err := manager.Register(ctx, &Auth{ID: "auth-b", Provider: "gemini"}); err != nil {
		t.Fatalf("Register(auth-b) error = %v", err)
	}

	selection, errSelect := manager.Select(ctx, []string{"gemini"}, "", cliproxyexecutor.Options{}, map[string]struct{}{
		"auth-a": {},
	})
	if errSelect != nil {
		t.Fatalf("Select() error = %v", errSelect)
	}
	if selection == nil || selection.Auth == nil {
		t.Fatalf("Select() returned nil selection/auth")
	}
	if selection.Auth.ID != "auth-b" {
		t.Fatalf("selection.Auth.ID = %q, want %q", selection.Auth.ID, "auth-b")
	}
}

func TestManagerSelectReturnsNormalProviderError(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	selection, errSelect := manager.Select(context.Background(), nil, "", cliproxyexecutor.Options{}, nil)
	if errSelect == nil {
		t.Fatalf("Select() error = nil, want error")
	}
	if selection != nil {
		t.Fatalf("Select() selection = %#v, want nil", selection)
	}
}

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func TestExecuteSelectedHTTPClosesResponseBodyWhenCallbackReturnsError(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&selectionTestExecutor{id: "gemini"})
	if _, err := manager.Register(ctx, &Auth{ID: "gemini-auth", Provider: "gemini"}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	body := &closeTrackingBody{Reader: strings.NewReader("partial")}
	resp, selection, err := manager.ExecuteSelectedHTTP(ctx, []string{"gemini"}, "", cliproxyexecutor.Options{}, func(context.Context, *Selection) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
		}, errors.New("callback failed after response")
	})
	if err == nil {
		t.Fatalf("ExecuteSelectedHTTP() error = nil, want callback error")
	}
	if resp != nil {
		t.Fatalf("ExecuteSelectedHTTP() response = %#v, want nil", resp)
	}
	if selection != nil {
		t.Fatalf("ExecuteSelectedHTTP() selection = %#v, want nil on callback error", selection)
	}
	if !body.closed {
		t.Fatalf("response body was not closed after callback error")
	}
}

func TestHomeProviderMismatchRetryLimit(t *testing.T) {
	if got := homeProviderMismatchRetryLimit(3); got != 3 {
		t.Fatalf("homeProviderMismatchRetryLimit(3) = %d, want 3", got)
	}
	if got := homeProviderMismatchRetryLimit(0); got != defaultHomeProviderMismatchRetryLimit {
		t.Fatalf("homeProviderMismatchRetryLimit(0) = %d, want default %d", got, defaultHomeProviderMismatchRetryLimit)
	}
}
