package explorer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeServices struct {
	mu             sync.Mutex
	grant          func() AccessGrant
	bootstrapFn    func(context.Context) (AccessGrant, error)
	authorizeErr   error
	authorizeCalls int
	executeCalls   int
	lastOperation  string
	lastPrincipal  Principal
	lastRequest    json.RawMessage
	responseByOp   map[string][]byte
}

func (f *fakeServices) Bootstrap(ctx context.Context) (AccessGrant, error) {
	if f.bootstrapFn != nil {
		return f.bootstrapFn(ctx)
	}
	return f.currentGrant(), nil
}

func (f *fakeServices) Authorize(_ context.Context, principal Principal, operation string, _ json.RawMessage) (AccessGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorizeCalls++
	if f.authorizeErr != nil {
		return AccessGrant{}, f.authorizeErr
	}
	f.lastPrincipal = principal
	f.lastOperation = operation
	return f.currentGrant(), nil
}

func (f *fakeServices) Execute(_ context.Context, principal Principal, operation string, request json.RawMessage) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executeCalls++
	f.lastPrincipal = principal
	f.lastOperation = operation
	f.lastRequest = append(json.RawMessage(nil), request...)
	response := f.responseByOp[operation]
	return append([]byte(nil), response...), nil
}

func (f *fakeServices) currentGrant() AccessGrant {
	if f.grant != nil {
		return f.grant()
	}
	return AccessGrant{ExpiresAt: time.Now().Add(time.Hour), Capabilities: []string{"workspace.read", "graph.read", "source.read"}}
}

func newTestServer(t *testing.T, services *fakeServices) *Server {
	t.Helper()
	resetBootstrapLimiter()
	return newTestServerWithoutLimiterReset(t, services)
}

func newTestServerWithoutLimiterReset(t *testing.T, services *fakeServices) *Server {
	t.Helper()
	listener, err := ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(listener, services)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func resetBootstrapLimiter() {
	bootstrapLimiter.Lock()
	bootstrapLimiter.attempts = nil
	bootstrapLimiter.Unlock()
}

func newFakeServices() *fakeServices {
	return &fakeServices{responseByOp: map[string][]byte{
		graphOperation: testResponse("graph_query", map[string]any{
			"operation": "graph_query", "nodes": []any{}, "edges": []any{}, "subgraph_digest": strings.Repeat("a", 64),
		}),
		sourceOperation: testResponse("source_read", map[string]any{
			"operation": "source_read", "source_ref": testSourceRef(), "content": "package main\n",
		}),
		workspaceOperation: testResponse("GET /workspace", map[string]any{
			"operation": "status", "scopes": []any{}, "capabilities": []any{}, "project_stage": "unknown", "index_state": "missing", "registered_scopes": []any{},
		}),
		previewStatusOperation: testResponse("POST /preview/status", map[string]any{
			"operation": "preview_status", "session_id": "12345678-1234-4234-8234-123456789abc",
			"revision": 1, "authorization_epoch": 0, "document_epoch": 1, "hmr_epoch": 0,
			"state": "detached", "mode_state": "inspect", "binding_id": nil,
			"permission_profile_id": "test-profile", "permission_version": 1, "permission_status": "expired",
			"expires_at": "2026-10-20T00:00:00Z", "server_time": "2026-10-20T00:00:00Z", "interaction_allowed": false,
		}),
	}}
}

func testSourceRef() map[string]any {
	return map[string]any{
		"worktree_id": "12345678-1234-4234-8234-123456789abc",
		"profile_id":  strings.Repeat("b", 64), "generation_id": "87654321-4321-4321-8321-cba987654321",
		"path": "main.go", "content_hash": strings.Repeat("c", 64),
		"range": map[string]any{"start_byte": 0, "end_byte": 13},
	}
}

func testResponse(operation string, data any) []byte {
	response := map[string]any{
		"schema_version": 1, "request_id": "12345678-1234-4234-8234-123456789abc", "status": "ok", "scope": nil,
		"freshness": map[string]any{"status": "not_checked", "scope": "none", "checked_at": nil, "buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage":  map[string]any{"status": "unknown", "capabilities": []any{}, "gaps": []any{}, "scope_description": "Authorized test scope."},
		"readiness": "needs_context", "data": data,
		"pagination": map[string]any{"truncated": false, "enumeration_complete": true, "returned_count": 0, "required_pending": nil, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil},
		"budget":     map[string]any{"max_output_bytes": maxUIResponseBytes, "serialized_bytes": 0, "token_count": nil, "token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "http"},
		"warnings":   []any{}, "error": nil,
	}
	for range 4 {
		raw, _ := json.Marshal(response)
		response["budget"].(map[string]any)["serialized_bytes"] = len(raw)
	}
	raw, _ := json.Marshal(response)
	return raw
}

func request(server *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, server.Origin()+path, strings.NewReader(body))
	req.Host = server.Host()
	req.Header.Set("Origin", server.Origin())
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	return response
}

func bootstrap(t *testing.T, server *Server) (string, *http.Cookie) {
	t.Helper()
	code, ok := server.TakeBootstrapCode()
	if !ok {
		t.Fatal("bootstrap code was not available")
	}
	return exchangeBootstrap(t, server, code)
}

func exchangeBootstrap(t *testing.T, server *Server, code string) (string, *http.Cookie) {
	t.Helper()
	response := request(server, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, code))
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Bearer string `json:"bearer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != csrfCookieName {
		t.Fatalf("bootstrap cookies = %#v", cookies)
	}
	return envelope.Data.Bearer, cookies[0]
}

func authenticatedRequest(server *Server, method, path, body, bearer string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, server.Origin()+path, strings.NewReader(body))
	req.Host = server.Host()
	req.Header.Set("Origin", server.Origin())
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	return response
}

func TestBootstrapIssuesOpaqueScopedSessionAndReadRoutesDelegate(t *testing.T) {
	services := newFakeServices()
	server := newTestServer(t, services)
	code, ok := server.TakeBootstrapCode()
	if !ok || len(code) != 43 || strings.Contains(code, "=") {
		t.Fatalf("bootstrap code = %q, available=%v", code, ok)
	}
	if _, ok := server.TakeBootstrapCode(); ok {
		t.Fatal("bootstrap code was returned twice")
	}
	bearer, cookie := exchangeBootstrap(t, server, code)
	if len(bearer) != 43 {
		t.Fatalf("bearer length = %d", len(bearer))
	}
	if cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode || cookie.HttpOnly {
		t.Fatalf("CSRF cookie policy = %#v", cookie)
	}

	response := authenticatedRequest(server, http.MethodPost, apiPrefix+"/graph/query", `{"root":{"kind":"workspace"}}`, bearer, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("graph status = %d, body=%s", response.Code, response.Body.String())
	}
	services.mu.Lock()
	defer services.mu.Unlock()
	if services.authorizeCalls != 1 || services.executeCalls != 1 || services.lastOperation != graphOperation {
		t.Fatalf("authorize=%d execute=%d operation=%q", services.authorizeCalls, services.executeCalls, services.lastOperation)
	}
	if services.lastPrincipal.SessionID == "" || strings.Contains(services.lastPrincipal.SessionID, bearer) {
		t.Fatalf("invalid server-derived principal: %#v", services.lastPrincipal)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unsafe response headers: %#v", response.Header())
	}
}

func TestForgedHostOriginExpiredBearerAndCLIOnlyRouteFailClosed(t *testing.T) {
	services := newFakeServices()
	server := newTestServer(t, services)
	bearer, cookie := bootstrap(t, server)

	for name, mutate := range map[string]func(*http.Request){
		"forged host":    func(req *http.Request) { req.Host = "localhost:12345" },
		"forged origin":  func(req *http.Request) { req.Header.Set("Origin", "http://localhost:12345") },
		"missing origin": func(req *http.Request) { req.Header.Del("Origin") },
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, server.Origin()+apiPrefix+"/source/read", strings.NewReader(testSourceReadRequest()))
			req.Host = server.Host()
			req.Header.Set("Origin", server.Origin())
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Content-Type", "application/json")
			mutate(req)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want forbidden; body=%s", response.Code, response.Body.String())
			}
		})
	}

	server.mu.Lock()
	server.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	server.mu.Unlock()
	expired := authenticatedRequest(server, http.MethodPost, apiPrefix+"/graph/query", `{"root":{"kind":"workspace"}}`, bearer, cookie)
	if expired.Code != http.StatusUnauthorized || !strings.Contains(expired.Body.String(), "AUTH_EXPIRED") {
		t.Fatalf("expired session status=%d body=%s", expired.Code, expired.Body.String())
	}

	server.mu.Lock()
	server.now = time.Now
	server.mu.Unlock()
	before := services.executeCalls
	admin := request(server, http.MethodPost, apiPrefix+"/permissions/set", `{}`)
	if admin.Code != http.StatusNotFound || services.executeCalls != before {
		t.Fatalf("CLI-only route status=%d execute calls=%d before=%d", admin.Code, services.executeCalls, before)
	}
}

func TestOversizedSourceRequestIsRejectedBeforeAuthorization(t *testing.T) {
	services := newFakeServices()
	server := newTestServer(t, services)
	bearer, cookie := bootstrap(t, server)
	body := testSourceReadRequest() + strings.Repeat(" ", int(maxCoreRequestBytes))
	response := authenticatedRequest(server, http.MethodPost, apiPrefix+"/source/read", body, bearer, cookie)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status=%d body=%s", response.Code, response.Body.String())
	}
	services.mu.Lock()
	defer services.mu.Unlock()
	if services.authorizeCalls != 0 || services.executeCalls != 0 {
		t.Fatalf("oversized request reached services: authorize=%d execute=%d", services.authorizeCalls, services.executeCalls)
	}
}

func TestAuthorizationIsCheckedOnEveryReadAndCanRevokeSession(t *testing.T) {
	services := newFakeServices()
	server := newTestServer(t, services)
	bearer, cookie := bootstrap(t, server)
	first := authenticatedRequest(server, http.MethodGet, apiPrefix+"/workspace?verify=none", "", bearer, cookie)
	second := authenticatedRequest(server, http.MethodPost, apiPrefix+"/graph/query", `{"root":{"kind":"workspace"}}`, bearer, cookie)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("read route statuses workspace=%d graph=%d", first.Code, second.Code)
	}
	services.mu.Lock()
	services.authorizeErr = fmt.Errorf("permission revoked")
	services.mu.Unlock()
	revoked := authenticatedRequest(server, http.MethodPost, apiPrefix+"/source/read", testSourceReadRequest(), bearer, cookie)
	if revoked.Code != http.StatusForbidden || !strings.Contains(revoked.Body.String(), "ACCESS_DENIED") {
		t.Fatalf("revoked status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	services.mu.Lock()
	defer services.mu.Unlock()
	if services.authorizeCalls != 3 || services.executeCalls != 2 {
		t.Fatalf("authorize=%d execute=%d", services.authorizeCalls, services.executeCalls)
	}
}

func TestPreviewStatusReadDoesNotExtendSessionExpiry(t *testing.T) {
	services := newFakeServices()
	now := time.Now()
	services.grant = func() AccessGrant {
		return AccessGrant{ExpiresAt: now.Add(time.Hour), Capabilities: []string{"workspace.read", "graph.read", "source.read"}}
	}
	server := newTestServer(t, services)
	server.now = func() time.Time { return now }
	bearer, cookie := bootstrap(t, server)
	statusRequest := `{"session_id":"12345678-1234-4234-8234-123456789abc"}`
	now = now.Add(29 * time.Minute)
	response := authenticatedRequest(server, http.MethodPost, apiPrefix+"/preview/status", statusRequest, bearer, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", response.Code, response.Body.String())
	}
	now = now.Add(2 * time.Minute)
	expired := authenticatedRequest(server, http.MethodPost, apiPrefix+"/preview/status", statusRequest, bearer, cookie)
	if expired.Code != http.StatusUnauthorized || !strings.Contains(expired.Body.String(), "AUTH_EXPIRED") {
		t.Fatalf("polling extended session: status=%d body=%s", expired.Code, expired.Body.String())
	}
}

func TestBootstrapRateLimitAndSingleUseCode(t *testing.T) {
	services := newFakeServices()
	server := newTestServer(t, services)
	code, ok := server.TakeBootstrapCode()
	if !ok {
		t.Fatal("bootstrap code unavailable")
	}
	wrong := strings.Repeat("A", len(code))
	for attempt := 0; attempt < maxBootstrapAttempts; attempt++ {
		response := request(server, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, wrong))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
	}
	limited := request(server, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, code))
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt status=%d body=%s", limited.Code, limited.Body.String())
	}
}

func TestBootstrapRateLimitIsProcessWide(t *testing.T) {
	first := newTestServer(t, newFakeServices())
	second := newTestServerWithoutLimiterReset(t, newFakeServices())
	firstCode, ok := first.TakeBootstrapCode()
	if !ok {
		t.Fatal("first bootstrap code unavailable")
	}
	secondCode, ok := second.TakeBootstrapCode()
	if !ok {
		t.Fatal("second bootstrap code unavailable")
	}
	wrong := strings.Repeat("A", len(firstCode))
	for attempt := 0; attempt < maxBootstrapAttempts-2; attempt++ {
		response := request(first, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, wrong))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("first server attempt %d status=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := request(second, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, wrong))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("second server attempt %d status=%d body=%s", attempt+1, response.Code, response.Body.String())
		}
	}
	limited := request(second, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, secondCode))
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("cross-server sixth attempt status=%d body=%s", limited.Code, limited.Body.String())
	}
}

func TestCloseDuringBootstrapCannotCreateSession(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	services := newFakeServices()
	services.bootstrapFn = func(context.Context) (AccessGrant, error) {
		close(entered)
		<-release
		return services.currentGrant(), nil
	}
	server := newTestServer(t, services)
	code, ok := server.TakeBootstrapCode()
	if !ok {
		t.Fatal("bootstrap code unavailable")
	}
	type result struct {
		response *httptest.ResponseRecorder
	}
	done := make(chan result, 1)
	go func() {
		done <- result{response: request(server, http.MethodPost, apiPrefix+"/session/bootstrap", fmt.Sprintf(`{"code":%q}`, code))}
	}()
	<-entered
	_ = server.Close()
	close(release)
	response := (<-done).response
	if response.Code == http.StatusOK || len(response.Result().Cookies()) != 0 {
		t.Fatalf("bootstrap created a session after Close: status=%d cookies=%#v", response.Code, response.Result().Cookies())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.closed || len(server.sessions) != 0 {
		t.Fatalf("closed=%v sessions=%d after concurrent bootstrap", server.closed, len(server.sessions))
	}
}

func TestBootstrapRejectsAmbiguousAndEncodedRequestHeaders(t *testing.T) {
	server := newTestServer(t, newFakeServices())
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{
			name: "duplicate content type",
			mutate: func(request *http.Request) {
				request.Header.Add("Content-Type", "application/json")
			},
		},
		{
			name: "content encoding",
			mutate: func(request *http.Request) {
				request.Header.Set("Content-Encoding", "identity")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, server.Origin()+apiPrefix+"/session/bootstrap", strings.NewReader(`{"code":"invalid"}`))
			request.Host = server.Host()
			request.Header.Set("Origin", server.Origin())
			request.Header.Set("Content-Type", "application/json")
			test.mutate(request)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func testSourceReadRequest() string {
	request, _ := json.Marshal(map[string]any{"source_ref": testSourceRef()})
	return string(request)
}

func TestCSRFDoubleSubmitIsSessionBound(t *testing.T) {
	services := newFakeServices()
	server := newTestServer(t, services)
	bearer, cookie := bootstrap(t, server)
	requestKey := sha256Bytes(bearer)
	req := httptest.NewRequest(http.MethodPost, server.Origin()+apiPrefix+"/records/review", nil)
	req.Host = server.Host()
	req.Header.Set("Origin", server.Origin())
	req.Header.Set(csrfHeaderName, cookie.Value)
	req.AddCookie(cookie)
	if !server.validCSRFForMutation(req, requestKey) {
		t.Fatal("matching same-origin CSRF token was rejected")
	}
	req.Header.Set(csrfHeaderName, strings.Repeat("A", len(cookie.Value)))
	if server.validCSRFForMutation(req, requestKey) {
		t.Fatal("mismatched CSRF token was accepted")
	}
}

func sha256Bytes(token string) [32]byte {
	decoded, _ := base64.RawURLEncoding.DecodeString(token)
	return sha256.Sum256(decoded)
}
