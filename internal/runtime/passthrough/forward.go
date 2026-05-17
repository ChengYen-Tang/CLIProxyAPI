package passthrough

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/geminicli"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/httpclient"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	geminiCLIOAuthClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiCLIOAuthClientSecret = "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"
	googleTokenEndpoint        = "https://oauth2.googleapis.com/token"
)

var geminiCLIOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

// Request describes an inbound request that should be forwarded without body translation.
type Request struct {
	Surface  string
	Model    string
	Method   string
	Path     string
	RawQuery string
	Body     []byte
	Headers  http.Header
	Metadata map[string]any
}

// Response contains the raw upstream response.
type Response struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

// StreamResponse contains a raw upstream streaming response.
type StreamResponse struct {
	StatusCode int
	Body       io.ReadCloser
	Headers    http.Header
}

// Execute forwards req to a native upstream using the existing auth selection machinery.
func Execute(ctx context.Context, manager *cliproxyauth.Manager, req Request) (*Response, error) {
	resp, selection, err := executeHTTP(ctx, manager, req, false)
	if err != nil {
		return nil, err
	}
	body, errRead := readHTTPBody(resp, false)
	if errRead != nil {
		markReadResult(ctx, manager, selection, req.Model, false, errRead)
		return nil, errRead
	}
	if resp.StatusCode < http.StatusBadRequest {
		markReadResult(ctx, manager, selection, req.Model, true, nil)
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Body:       body.payload,
		Headers:    resp.Header.Clone(),
	}, nil
}

// ExecuteStream forwards req to a native upstream and returns its raw response body.
func ExecuteStream(ctx context.Context, manager *cliproxyauth.Manager, req Request) (*StreamResponse, error) {
	resp, selection, err := executeHTTP(ctx, manager, req, true)
	if err != nil {
		return nil, err
	}
	body, errRead := readHTTPBody(resp, true)
	if errRead != nil {
		markReadResult(ctx, manager, selection, req.Model, false, errRead)
		return nil, errRead
	}
	stream := body.stream
	if stream == nil {
		stream = io.NopCloser(bytes.NewReader(body.payload))
	}
	if resp.StatusCode < http.StatusBadRequest {
		stream = trackStreamReadResult(ctx, manager, selection, req.Model, stream)
	}

	return &StreamResponse{
		StatusCode: resp.StatusCode,
		Body:       stream,
		Headers:    resp.Header.Clone(),
	}, nil
}

type rawBody struct {
	payload []byte
	stream  io.ReadCloser
}

func readHTTPBody(resp *http.Response, stream bool) (rawBody, error) {
	if resp == nil || resp.Body == nil {
		return rawBody{}, fmt.Errorf("native passthrough: upstream response body is nil")
	}
	if stream && resp.StatusCode < http.StatusBadRequest {
		return rawBody{stream: resp.Body}, nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return rawBody{}, err
	}
	return rawBody{payload: payload}, nil
}

func executeHTTP(ctx context.Context, manager *cliproxyauth.Manager, req Request, stream bool) (*http.Response, *cliproxyauth.Selection, error) {
	if manager == nil {
		return nil, nil, &cliproxyauth.Error{Code: "auth_not_found", Message: "auth manager is nil"}
	}
	providers := FilterProviders(req.Surface, ProvidersForSurface(req.Surface))
	if len(providers) == 0 {
		return nil, nil, &cliproxyauth.Error{Code: "auth_not_found", Message: "no native provider available"}
	}

	return manager.ExecuteSelectedHTTP(ctx, providers, req.Model, passthroughOptions(req, stream), func(execCtx context.Context, selection *cliproxyauth.Selection) (*http.Response, error) {
		if selection == nil || selection.Auth == nil {
			return nil, &cliproxyauth.Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		targetURL, errURL := targetURLFor(selection.Provider, selection.Auth, req)
		if errURL != nil {
			return nil, errURL
		}
		method := strings.TrimSpace(req.Method)
		if method == "" {
			method = http.MethodPost
		}
		httpReq, errHTTPReq := http.NewRequestWithContext(execCtx, method, targetURL, bytes.NewReader(req.Body))
		if errHTTPReq != nil {
			return nil, errHTTPReq
		}
		httpReq.Header = sanitizedHeader(req.Headers)
		if errAuth := applyAuthOnly(execCtx, selection.Provider, selection.Auth, httpReq); errAuth != nil {
			return nil, errAuth
		}
		resp, errDo := nativeHTTPClient(execCtx, selection.Auth).Do(httpReq)
		if errDo != nil || stream || resp == nil || resp.StatusCode >= http.StatusBadRequest {
			return resp, errDo
		}
		buffered, errBuffer := bufferHTTPResponseBody(resp)
		if errBuffer != nil {
			return nil, errBuffer
		}
		return buffered, nil
	})
}

func passthroughOptions(req Request, stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Stream:          stream,
		Headers:         sanitizedHeader(req.Headers),
		OriginalRequest: cloneBytes(req.Body),
		Metadata:        cloneMetadata(req.Metadata),
	}
}

func applyAuthOnly(ctx context.Context, provider string, auth *cliproxyauth.Auth, req *http.Request) error {
	if auth == nil {
		return &cliproxyauth.Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &cliproxyauth.Error{Code: "invalid_request", Message: "http request is nil"}
	}
	switch normalize(provider) {
	case ProviderGemini:
		apiKey := attrString(auth, "api_key")
		if apiKey != "" {
			req.Header.Set("x-goog-api-key", apiKey)
			req.Header.Del("Authorization")
			return nil
		}
		return setBearerAuth(req, auth)
	case ProviderGeminiCLI:
		return setGeminiCLIBearerAuth(ctx, req, auth)
	case ProviderClaude:
		apiKey := attrString(auth, "api_key")
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Del("Authorization")
			return nil
		}
		return setBearerAuth(req, auth)
	case ProviderOpenAI, ProviderCodex:
		apiKey := attrString(auth, "api_key")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
			return nil
		}
		return setBearerAuth(req, auth)
	default:
		return &cliproxyauth.Error{Code: "provider_not_found", Message: "unsupported native passthrough provider: " + provider}
	}
}

func setBearerAuth(req *http.Request, auth *cliproxyauth.Auth) error {
	token := metaString(auth, "access_token")
	if token == "" {
		return &cliproxyauth.Error{Code: "auth_not_found", Message: "missing native passthrough access token"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	return nil
}

func setGeminiCLIBearerAuth(ctx context.Context, req *http.Request, auth *cliproxyauth.Auth) error {
	token, errToken := geminiCLIAccessToken(ctx, auth)
	if errToken != nil {
		return errToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	return nil
}

func geminiCLIAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	token, base := oauthTokenFromAuth(auth)
	if token.AccessToken != "" && token.Valid() {
		return token.AccessToken, nil
	}
	if token.RefreshToken == "" {
		return "", &cliproxyauth.Error{Code: "auth_expired", Message: "native passthrough Gemini CLI access token missing or expired and refresh token is missing", Retryable: true, HTTPStatus: http.StatusUnauthorized}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conf := &oauth2.Config{
		ClientID:     geminiCLIOAuthClientID,
		ClientSecret: geminiCLIOAuthClientSecret,
		Scopes:       geminiCLIOAuthScopes,
		Endpoint:     google.Endpoint,
	}
	ctxToken := context.WithValue(ctx, oauth2.HTTPClient, nativeHTTPClient(ctx, auth))
	refreshed, errRefresh := conf.TokenSource(ctxToken, &token).Token()
	if errRefresh != nil {
		return "", errRefresh
	}
	updateOAuthTokenMetadata(auth, base, refreshed)
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		return "", &cliproxyauth.Error{Code: "auth_not_found", Message: "refreshed Gemini CLI access token is empty"}
	}
	return strings.TrimSpace(refreshed.AccessToken), nil
}

func oauthTokenFromAuth(auth *cliproxyauth.Auth) (oauth2.Token, map[string]any) {
	var base map[string]any
	metadata := geminiCLIMetadata(auth)
	if raw, ok := metadata["token"].(map[string]any); ok && raw != nil {
		base = cloneMetadata(raw)
	}
	var token oauth2.Token
	if len(base) > 0 {
		if raw, err := json.Marshal(base); err == nil {
			_ = json.Unmarshal(raw, &token)
		}
	}
	if token.AccessToken == "" {
		token.AccessToken = metaStringFrom(metadata, "access_token")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = metaStringFrom(metadata, "refresh_token")
	}
	if token.TokenType == "" {
		token.TokenType = metaStringFrom(metadata, "token_type")
	}
	if token.Expiry.IsZero() {
		if expiry := metaStringFrom(metadata, "expiry"); expiry != "" {
			if parsed, err := time.Parse(time.RFC3339, expiry); err == nil {
				token.Expiry = parsed
			}
		}
	}
	return token, base
}

func updateOAuthTokenMetadata(auth *cliproxyauth.Auth, base map[string]any, token *oauth2.Token) {
	if auth == nil || token == nil {
		return
	}
	merged := cloneMetadata(base)
	if merged == nil {
		merged = make(map[string]any)
	}
	if raw, err := json.Marshal(token); err == nil {
		var tokenMap map[string]any
		if err = json.Unmarshal(raw, &tokenMap); err == nil {
			for key, value := range tokenMap {
				merged[key] = value
			}
		}
	}
	fields := oauthTokenMetadataFields(token, merged)
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		snapshot := shared.MergeMetadata(fields)
		if !geminicli.IsVirtual(auth.Runtime) {
			auth.Metadata = snapshot
		}
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	for key, value := range fields {
		auth.Metadata[key] = value
	}
}

func geminiCLIMetadata(auth *cliproxyauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		return shared.MetadataSnapshot()
	}
	return cloneMetadata(auth.Metadata)
}

func oauthTokenMetadataFields(token *oauth2.Token, merged map[string]any) map[string]any {
	fields := make(map[string]any, 5)
	if token.AccessToken != "" {
		fields["access_token"] = token.AccessToken
	}
	if token.RefreshToken != "" {
		fields["refresh_token"] = token.RefreshToken
	}
	if token.TokenType != "" {
		fields["token_type"] = token.TokenType
	}
	if !token.Expiry.IsZero() {
		fields["expiry"] = token.Expiry.Format(time.RFC3339)
	}
	if len(merged) > 0 {
		fields["token"] = merged
	}
	return fields
}

func nativeHTTPClient(ctx context.Context, auth *cliproxyauth.Auth) *http.Client {
	return httpclient.New(ctx, auth, httpclient.Options{})
}

func bufferHTTPResponseBody(resp *http.Response) (*http.Response, error) {
	if resp == nil {
		return nil, nil
	}
	buffered := new(http.Response)
	*buffered = *resp
	buffered.Header = resp.Header.Clone()
	var body []byte
	if resp.Body != nil {
		var errRead error
		body, errRead = io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errRead == nil && errClose != nil {
			errRead = errClose
		}
		if errRead != nil {
			return nil, errRead
		}
	}
	buffered.Body = io.NopCloser(bytes.NewReader(body))
	buffered.ContentLength = int64(len(body))
	return buffered, nil
}

func attrString(auth *cliproxyauth.Auth, key string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes[key])
}

func metaString(auth *cliproxyauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	return metaStringFrom(auth.Metadata, key)
}

func metaStringFrom(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	if token, ok := metadata["token"].(map[string]any); ok {
		if value, okValue := token[key].(string); okValue {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func markReadResult(ctx context.Context, manager *cliproxyauth.Manager, selection *cliproxyauth.Selection, model string, success bool, err error) {
	if manager == nil || selection == nil || selection.Auth == nil {
		return
	}
	result := cliproxyauth.Result{
		AuthID:   selection.Auth.ID,
		Provider: selection.Provider,
		Model:    model,
		Success:  success,
	}
	if !success && err != nil {
		result.Error = &cliproxyauth.Error{Message: err.Error()}
	}
	manager.MarkResult(ctx, result)
}

type resultTrackingReadCloser struct {
	io.ReadCloser
	ctx       context.Context
	manager   *cliproxyauth.Manager
	selection *cliproxyauth.Selection
	model     string
	once      sync.Once
}

func trackStreamReadResult(ctx context.Context, manager *cliproxyauth.Manager, selection *cliproxyauth.Selection, model string, body io.ReadCloser) io.ReadCloser {
	return &resultTrackingReadCloser{
		ReadCloser: body,
		ctx:        ctx,
		manager:    manager,
		selection:  selection,
		model:      model,
	}
}

func (r *resultTrackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	switch {
	case err == io.EOF:
		r.once.Do(func() {
			markReadResult(r.ctx, r.manager, r.selection, r.model, true, nil)
		})
	case err != nil:
		r.once.Do(func() {
			markReadResult(r.ctx, r.manager, r.selection, r.model, false, err)
		})
	}
	return n, err
}

func (r *resultTrackingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() {
		if err != nil {
			markReadResult(r.ctx, r.manager, r.selection, r.model, false, err)
			return
		}
		markReadResult(r.ctx, r.manager, r.selection, r.model, false, fmt.Errorf("native passthrough stream closed before EOF"))
	})
	return err
}

// ProvidersForSurface returns native upstream providers allowed for an input surface.
func ProvidersForSurface(surface string) []string {
	switch normalize(surface) {
	case SurfaceOpenAI:
		return []string{ProviderOpenAI, ProviderCodex}
	case SurfaceOpenAIResponse:
		return []string{ProviderCodex}
	case SurfaceClaude:
		return []string{ProviderClaude}
	case SurfaceGemini:
		return []string{ProviderGemini}
	case SurfaceGeminiCLI:
		return []string{ProviderGeminiCLI}
	default:
		return nil
	}
}

func targetURLFor(provider string, auth *cliproxyauth.Auth, req Request) (string, error) {
	base := baseURLFor(provider, auth)
	if base == "" {
		return "", fmt.Errorf("native passthrough: no base URL for provider %s", provider)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("native passthrough: invalid base URL %q: %w", base, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("native passthrough: invalid base URL %q", base)
	}

	path := upstreamPathFor(provider, req)
	if path == "" {
		path = "/"
	}
	parsed.Path = joinURLPath(parsed.Path, path)
	parsed.RawQuery = strings.TrimPrefix(strings.TrimSpace(req.RawQuery), "?")
	return parsed.String(), nil
}

func baseURLFor(provider string, auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		for _, key := range []string{"base_url", "base-url"} {
			if value := strings.TrimRight(strings.TrimSpace(auth.Attributes[key]), "/"); value != "" {
				return value
			}
		}
	}

	switch normalize(provider) {
	case ProviderOpenAI:
		return "https://api.openai.com"
	case ProviderCodex:
		return "https://chatgpt.com/backend-api/codex"
	case ProviderClaude:
		return "https://api.anthropic.com"
	case ProviderGemini:
		return "https://generativelanguage.googleapis.com"
	case ProviderGeminiCLI:
		return "https://cloudcode-pa.googleapis.com"
	default:
		return ""
	}
}

func upstreamPathFor(provider string, req Request) string {
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return "/"
	}
	if normalize(provider) == ProviderCodex {
		switch path {
		case "/v1/responses":
			return "/responses"
		case "/v1/responses/compact":
			return "/responses/compact"
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func joinURLPath(basePath, requestPath string) string {
	basePath = strings.TrimRight(strings.TrimSpace(basePath), "/")
	requestPath = strings.TrimLeft(strings.TrimSpace(requestPath), "/")
	switch {
	case basePath == "" && requestPath == "":
		return "/"
	case basePath == "":
		return "/" + requestPath
	case requestPath == "":
		return basePath
	default:
		return basePath + "/" + requestPath
	}
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func sanitizedHeader(src http.Header) http.Header {
	dst := cloneHeader(src)
	if dst == nil {
		dst = http.Header{}
	}

	for _, token := range dst.Values("Connection") {
		for _, part := range strings.Split(token, ",") {
			if name := strings.TrimSpace(part); name != "" {
				dst.Del(name)
			}
		}
	}

	for _, key := range []string{
		"Authorization",
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"X-Api-Key",
		"X-Goog-Api-Key",
	} {
		dst.Del(key)
	}
	return dst
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func cloneMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
