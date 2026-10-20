// Package explorer implements the authenticated loopback HTTP adapter for the
// Explorer. It delegates every workspace operation to application services.
package explorer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
)

const (
	apiPrefix              = "/api/explorer/v1"
	bootstrapOperation     = "POST /session/bootstrap"
	workspaceOperation     = "GET /workspace"
	graphOperation         = "POST /graph/query"
	sourceOperation        = "POST /source/read"
	previewStatusOperation = "POST /preview/status"
	maxBootstrapBytes      = 16 << 10
	maxCoreRequestBytes    = 32 << 10
	maxUIRequestBytes      = 64 << 10
	maxUIResponseBytes     = 1 << 20
	maxAuthHeaderBytes     = 128
	maxSessions            = 32
	bootstrapCodeTTL       = time.Minute
	maxSessionTTL          = 30 * time.Minute
	maxBootstrapAttempts   = 5
	bootstrapWindow        = time.Minute
	csrfCookieName         = "mce4d_csrf"
	csrfHeaderName         = "X-MCE4D-CSRF"
)

var (
	ErrInvalidListener = errors.New("Explorer requires a TCP listener bound to 127.0.0.1")
	ErrInvalidServices = errors.New("Explorer application services are required")
	ErrAccessDenied    = errors.New("Explorer access denied")
)

// AccessGrant is a current, server-derived source permission. ExpiresAt must
// be the expiry of that permission, not a client supplied session lifetime.
type AccessGrant struct {
	ExpiresAt    time.Time
	Capabilities []string
}

// Principal is derived from an in-memory server session. It deliberately does
// not contain the bearer credential.
type Principal struct {
	SessionID    string
	ExpiresAt    time.Time
	Capabilities []string
}

// Services is the boundary to application/query services. Bootstrap and
// Authorize return ErrAccessDenied for policy denials; other errors represent
// service failures. Implementations re-check source policy in Authorize on
// every request and return a full S43 response envelope from Execute. This
// package never opens a store.
type Services interface {
	Bootstrap(context.Context) (AccessGrant, error)
	Authorize(context.Context, Principal, string, json.RawMessage) (AccessGrant, error)
	Execute(context.Context, Principal, string, json.RawMessage) ([]byte, error)
}

type Server struct {
	listener  net.Listener
	server    *http.Server
	services  Services
	validator *contracts.Validator
	now       func() time.Time

	host   string
	origin string

	mu                   sync.Mutex
	bootstrapCodeHash    [32]byte
	bootstrapDeadline    time.Time
	bootstrapTaken       bool
	bootstrapUsed        bool
	closed               bool
	sessions             map[[32]byte]*session
	pendingBootstrapCode string
}

type session struct {
	id           string
	deadline     time.Time // Monotonic elapsed-time bound, never extended.
	expiresAt    time.Time // UTC time exposed to the browser.
	csrfHash     [32]byte
	capabilities []string
}

var bootstrapLimiter = struct {
	sync.Mutex
	attempts []time.Time
}{}

// ListenLoopback binds the Explorer only to IPv4 loopback. A zero port asks
// the OS to allocate an ephemeral port.
func ListenLoopback(port int) (net.Listener, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid Explorer port %d", port)
	}
	return net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// NewServer binds host and origin checks to the actual loopback listener.
// The caller owns server lifetime through Serve or Close; no target process is
// started by this adapter.
func NewServer(listener net.Listener, services Services) (*Server, error) {
	if listener == nil || services == nil {
		return nil, ErrInvalidServices
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP == nil || !address.IP.Equal(net.ParseIP("127.0.0.1")) || address.Port < 1 || address.Port > 65535 {
		return nil, ErrInvalidListener
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("create Explorer contract validator: %w", err)
	}
	code, err := randomToken(32)
	if err != nil {
		return nil, fmt.Errorf("create one-time Explorer bootstrap code: %w", err)
	}
	codeBytes, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		return nil, fmt.Errorf("encode Explorer bootstrap code: %w", err)
	}
	now := time.Now()
	host := net.JoinHostPort(address.IP.String(), strconv.Itoa(address.Port))
	server := &Server{
		listener: listener, services: services, validator: validator, now: time.Now,
		host: host, origin: "http://" + host,
		bootstrapCodeHash: sha256.Sum256(codeBytes), bootstrapDeadline: now.Add(bootstrapCodeTTL),
		sessions: make(map[[32]byte]*session),
	}
	server.server = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	// Keep the generated code available only through the one-shot accessor.
	server.pendingBootstrapCode = code
	return server, nil
}

// TakeBootstrapCode returns the startup code once, for direct presentation to
// the local user. The code is never written to logs or HTTP errors.
func (s *Server) TakeBootstrapCode() (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bootstrapTaken || s.bootstrapUsed || !s.now().Before(s.bootstrapDeadline) {
		return "", false
	}
	s.bootstrapTaken = true
	code := s.pendingBootstrapCode
	s.pendingBootstrapCode = ""
	return code, code != ""
}

// Origin returns the exact browser origin derived from the listener address.
func (s *Server) Origin() string {
	if s == nil {
		return ""
	}
	return s.origin
}

func (s *Server) Host() string {
	if s == nil {
		return ""
	}
	return s.host
}

// Serve runs the loopback API until the context is canceled or the listener
// fails. Cancellation performs bounded HTTP shutdown and then revokes all
// in-memory sessions.
func (s *Server) Serve(ctx context.Context) error {
	if s == nil || ctx == nil || s.server == nil || s.listener == nil {
		return errors.New("Explorer server and context are required")
	}
	defer s.revokeAll()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.server.Serve(s.listener) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := s.server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			_ = s.server.Close()
		}
		<-serveErr
		if shutdownErr != nil {
			return fmt.Errorf("shutdown Explorer server: %w", shutdownErr)
		}
		return nil
	}
}

// Close stops the listener and clears every in-memory credential.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	if s.server != nil {
		closeErr = s.server.Close()
	}
	s.revokeAll()
	return closeErr
}

func (s *Server) revokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.sessions)
	s.sessions = make(map[[32]byte]*session)
	s.closed = true
	s.pendingBootstrapCode = ""
	s.bootstrapUsed = true
	s.bootstrapCodeHash = [32]byte{}
}

func (s *Server) removeSession(key [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
}

// ServeHTTP is public for embedded use and contract tests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if s == nil || s.validator == nil || s.services == nil || r == nil || r.URL == nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.validHost(r) {
		s.writeError(w, "", http.StatusForbidden, "ACCESS_DENIED", "Request host is not allowed.", false, "none")
		return
	}
	path := r.URL.EscapedPath()
	operation, methodOK := operationFor(r.Method, path)
	if operation == "" {
		if methodForPath(path) != "" {
			w.Header().Set("Allow", methodForPath(path))
			s.writeError(w, "", http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "HTTP method is not allowed.", false, "fix_request")
			return
		}
		s.writeError(w, "", http.StatusNotFound, "INVALID_ARGUMENT", "Explorer route was not found.", false, "fix_request")
		return
	}
	if !methodOK {
		s.writeError(w, operation, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "HTTP method is not allowed.", false, "fix_request")
		return
	}
	if requiresOrigin(r.Method) && !s.validOrigin(r) {
		s.writeError(w, operation, http.StatusForbidden, "ACCESS_DENIED", "Request origin is not allowed.", false, "none")
		return
	}
	if r.Method == http.MethodGet && r.Header.Get("Origin") != "" && !s.validOrigin(r) {
		s.writeError(w, operation, http.StatusForbidden, "ACCESS_DENIED", "Request origin is not allowed.", false, "none")
		return
	}
	if r.URL.RawQuery != "" && operation != workspaceOperation {
		s.writeError(w, operation, http.StatusBadRequest, "INVALID_ARGUMENT", "Query parameters are not allowed.", false, "fix_request")
		return
	}
	if operation == bootstrapOperation {
		s.bootstrap(w, r)
		return
	}
	s.handleAuthenticated(w, r, operation)
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r, maxBootstrapBytes, bootstrapOperation)
	if !ok {
		return
	}
	if err := s.validator.ValidateRequest(bootstrapOperation, body); err != nil {
		s.writeError(w, bootstrapOperation, http.StatusBadRequest, "INVALID_ARGUMENT", "Bootstrap request is invalid.", false, "fix_request")
		return
	}
	value, err := contracts.DecodeJSON(body)
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusBadRequest, "INVALID_ARGUMENT", "Bootstrap request is invalid.", false, "fix_request")
		return
	}
	request, ok := value.(map[string]any)
	code, codeOK := request["code"].(string)
	if !ok || !codeOK {
		s.writeError(w, bootstrapOperation, http.StatusBadRequest, "INVALID_ARGUMENT", "Bootstrap request is invalid.", false, "fix_request")
		return
	}
	if !allowBootstrapAttempt(s.now()) {
		s.writeError(w, bootstrapOperation, http.StatusTooManyRequests, "RATE_LIMITED", "Bootstrap rate limit exceeded.", true, "retry_bounded")
		return
	}
	codeBytes, decodeErr := base64.RawURLEncoding.DecodeString(code)
	providedHash := sha256.Sum256(codeBytes)
	s.mu.Lock()
	valid := decodeErr == nil && len(codeBytes) == 32 && !s.closed && !s.bootstrapUsed && s.now().Before(s.bootstrapDeadline) && subtle.ConstantTimeCompare(providedHash[:], s.bootstrapCodeHash[:]) == 1
	if valid {
		s.bootstrapUsed = true
		s.pendingBootstrapCode = ""
		s.bootstrapCodeHash = [32]byte{}
	}
	s.mu.Unlock()
	if !valid {
		s.writeError(w, bootstrapOperation, http.StatusUnauthorized, "AUTH_INVALID", "Bootstrap code is invalid or expired.", false, "reauthenticate")
		return
	}
	grant, err := s.services.Bootstrap(r.Context())
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			s.writeError(w, bootstrapOperation, http.StatusForbidden, "ACCESS_DENIED", "Local source authorization was denied.", false, "none")
		} else {
			s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Explorer could not check local source authorization.", false, "none")
		}
		return
	}
	now := s.now()
	deadline, expiresAt, capabilities, ok := grantDeadline(now, grant, maxSessionTTL)
	if !ok {
		s.writeError(w, bootstrapOperation, http.StatusForbidden, "ACCESS_DENIED", "No valid source permission is available.", false, "none")
		return
	}
	bearer, err := randomToken(32)
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create an Explorer session.", false, "none")
		return
	}
	csrf, err := randomToken(32)
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create an Explorer session.", false, "none")
		return
	}
	sessionID, err := randomUUID()
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create an Explorer session.", false, "none")
		return
	}
	bearerBytes, err := base64.RawURLEncoding.DecodeString(bearer)
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create an Explorer session.", false, "none")
		return
	}
	csrfBytes, err := base64.RawURLEncoding.DecodeString(csrf)
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not create an Explorer session.", false, "none")
		return
	}
	key := sha256.Sum256(bearerBytes)
	csrfHash := sha256.Sum256(csrfBytes)
	data := map[string]any{"operation": "bootstrap", "bearer": bearer, "expires_at": expiresAt.UTC().Format(time.RFC3339Nano), "capabilities": capabilities}
	response, err := s.successResponse(bootstrapOperation, data)
	if err != nil {
		s.writeError(w, bootstrapOperation, http.StatusInternalServerError, "INTERNAL_ERROR", "Could not serialize an Explorer session.", false, "none")
		return
	}
	now = s.now()
	if !now.Before(deadline) || !now.Before(expiresAt) || deadline.Sub(now) < time.Second {
		s.writeError(w, bootstrapOperation, http.StatusForbidden, "ACCESS_DENIED", "Source permission expired during bootstrap.", false, "reauthenticate")
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.writeError(w, bootstrapOperation, http.StatusForbidden, "ACCESS_DENIED", "Explorer server is closed.", false, "none")
		return
	}
	for sessionKey, current := range s.sessions {
		if !now.Before(current.deadline) || !now.Before(current.expiresAt) {
			delete(s.sessions, sessionKey)
		}
	}
	if len(s.sessions) >= maxSessions {
		s.mu.Unlock()
		s.writeError(w, bootstrapOperation, http.StatusTooManyRequests, "RATE_LIMITED", "Explorer session capacity is full.", true, "retry_bounded")
		return
	}
	s.sessions[key] = &session{id: sessionID, deadline: deadline, expiresAt: expiresAt, csrfHash: csrfHash, capabilities: capabilities}
	s.mu.Unlock()
	maxAge := int(deadline.Sub(now).Seconds())
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: csrf, Path: "/", MaxAge: maxAge, SameSite: http.SameSiteStrictMode})
	if err := s.writeJSON(w, http.StatusOK, response); err != nil {
		s.removeSession(key)
		http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: "", Path: "/", MaxAge: -1, SameSite: http.SameSiteStrictMode})
	}
}

func (s *Server) handleAuthenticated(w http.ResponseWriter, r *http.Request, operation string) {
	var request []byte
	var ok bool
	if operation == workspaceOperation {
		request, ok = workspaceRequest(r)
		if !ok {
			s.writeError(w, operation, http.StatusBadRequest, "INVALID_ARGUMENT", "Workspace query is invalid.", false, "fix_request")
			return
		}
		if err := s.validator.ValidateRequest(operation, request); err != nil {
			s.writeError(w, operation, http.StatusBadRequest, "INVALID_ARGUMENT", "Workspace query is invalid.", false, "fix_request")
			return
		}
	} else {
		limit := requestLimit(operation)
		request, ok = s.readJSONBody(w, r, limit, operation)
		if !ok {
			return
		}
		if err := s.validator.ValidateRequest(operation, request); err != nil {
			s.writeError(w, operation, http.StatusBadRequest, "INVALID_ARGUMENT", "Operation request is invalid.", false, "fix_request")
			return
		}
	}
	principal, _, status, code := s.authenticate(r.Context(), r, operation, request)
	if status != 0 {
		s.writeError(w, operation, status, code, authMessage(code), status == http.StatusTooManyRequests, authAction(code))
		return
	}
	response, err := s.services.Execute(r.Context(), principal, operation, append(json.RawMessage(nil), request...))
	if err != nil {
		s.writeError(w, operation, http.StatusInternalServerError, "INTERNAL_ERROR", "Explorer operation failed.", false, "none")
		return
	}
	if len(response) > maxUIResponseBytes {
		s.writeError(w, operation, http.StatusRequestEntityTooLarge, "RESULT_TOO_LARGE", "Explorer response exceeded its byte limit.", false, "fix_request")
		return
	}
	if err := s.validator.ValidateResponse(operation, response); err != nil {
		s.writeError(w, operation, http.StatusInternalServerError, "INTERNAL_ERROR", "Explorer service returned an invalid response.", false, "none")
		return
	}
	if !validResponseBudget(response) {
		s.writeError(w, operation, http.StatusInternalServerError, "INTERNAL_ERROR", "Explorer service exceeded its declared response budget.", false, "none")
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) authenticate(ctx context.Context, r *http.Request, operation string, request []byte) (Principal, [32]byte, int, string) {
	var empty [32]byte
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxAuthHeaderBytes {
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_REQUIRED"
	}
	parts := strings.Split(values[0], " ")
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_REQUIRED"
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(tokenBytes) != 32 {
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_INVALID"
	}
	key := sha256.Sum256(tokenBytes)
	now := s.now()
	s.mu.Lock()
	current, exists := s.sessions[key]
	if s.closed {
		s.mu.Unlock()
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_INVALID"
	}
	if !exists {
		s.mu.Unlock()
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_INVALID"
	}
	if !now.Before(current.deadline) || !now.Before(current.expiresAt) {
		delete(s.sessions, key)
		s.mu.Unlock()
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_EXPIRED"
	}
	principal := Principal{SessionID: current.id, ExpiresAt: current.expiresAt, Capabilities: append([]string(nil), current.capabilities...)}
	s.mu.Unlock()
	grant, err := s.services.Authorize(ctx, principal, operation, append(json.RawMessage(nil), request...))
	if err != nil {
		if errors.Is(err, ErrAccessDenied) {
			return Principal{}, empty, http.StatusForbidden, "ACCESS_DENIED"
		}
		return Principal{}, empty, http.StatusInternalServerError, "INTERNAL_ERROR"
	}
	checkNow := s.now()
	newDeadline, newExpiry, currentCapabilities, valid := grantDeadline(checkNow, grant, maxSessionTTL)
	if !valid {
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_EXPIRED"
	}
	s.mu.Lock()
	current, exists = s.sessions[key]
	if !exists || !checkNow.Before(current.deadline) || !checkNow.Before(current.expiresAt) {
		delete(s.sessions, key)
		s.mu.Unlock()
		return Principal{}, empty, http.StatusUnauthorized, "AUTH_EXPIRED"
	}
	if newDeadline.Before(current.deadline) {
		current.deadline = newDeadline
	}
	if newExpiry.Before(current.expiresAt) {
		current.expiresAt = newExpiry
	}
	current.capabilities = intersect(current.capabilities, currentCapabilities)
	principal = Principal{SessionID: current.id, ExpiresAt: current.expiresAt, Capabilities: append([]string(nil), current.capabilities...)}
	s.mu.Unlock()
	return principal, key, 0, ""
}

func allowBootstrapAttempt(now time.Time) bool {
	bootstrapLimiter.Lock()
	defer bootstrapLimiter.Unlock()
	keep := bootstrapLimiter.attempts[:0]
	for _, attempt := range bootstrapLimiter.attempts {
		if now.Sub(attempt) < bootstrapWindow {
			keep = append(keep, attempt)
		}
	}
	bootstrapLimiter.attempts = keep
	if len(bootstrapLimiter.attempts) >= maxBootstrapAttempts {
		return false
	}
	bootstrapLimiter.attempts = append(bootstrapLimiter.attempts, now)
	return true
}

func (s *Server) readJSONBody(w http.ResponseWriter, r *http.Request, limit int64, operation string) ([]byte, bool) {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		s.writeError(w, operation, http.StatusUnsupportedMediaType, "INVALID_ARGUMENT", "Request must use UTF-8 JSON.", false, "fix_request")
		return nil, false
	}
	mediaType, params, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		s.writeError(w, operation, http.StatusUnsupportedMediaType, "INVALID_ARGUMENT", "Request must use UTF-8 JSON.", false, "fix_request")
		return nil, false
	}
	if len(r.Header.Values("Content-Encoding")) != 0 {
		s.writeError(w, operation, http.StatusUnsupportedMediaType, "INVALID_ARGUMENT", "Encoded request bodies are not supported.", false, "fix_request")
		return nil, false
	}
	if r.ContentLength > limit {
		s.writeError(w, operation, http.StatusRequestEntityTooLarge, "RESULT_TOO_LARGE", "Request body exceeded its byte limit.", false, "fix_request")
		return nil, false
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, operation, http.StatusRequestEntityTooLarge, "RESULT_TOO_LARGE", "Request body exceeded its byte limit.", false, "fix_request")
		} else {
			s.writeError(w, operation, http.StatusBadRequest, "INVALID_ARGUMENT", "Request body could not be read.", false, "fix_request")
		}
		return nil, false
	}
	if len(body) == 0 {
		s.writeError(w, operation, http.StatusBadRequest, "INVALID_ARGUMENT", "Request body is required.", false, "fix_request")
		return nil, false
	}
	return body, true
}

func workspaceRequest(r *http.Request) ([]byte, bool) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		return nil, false
	}
	if r.ContentLength < 0 && r.Body != nil && r.Body != http.NoBody {
		var first [1]byte
		read, err := io.ReadFull(r.Body, first[:])
		if read != 0 || !errors.Is(err, io.EOF) {
			return nil, false
		}
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) > 1 {
		return nil, false
	}
	for key, items := range values {
		if key != "verify" || len(items) != 1 || (items[0] != "none" && items[0] != "workspace") {
			return nil, false
		}
	}
	value := map[string]any{}
	if items, ok := values["verify"]; ok {
		value["verify"] = items[0]
	}
	body, err := json.Marshal(value)
	return body, err == nil
}

func (s *Server) validHost(r *http.Request) bool {
	return r.Host != "" && r.Host == s.host
}

func (s *Server) validOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	return len(values) == 1 && values[0] == s.origin
}

func requiresOrigin(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func operationFor(method, path string) (string, bool) {
	switch path {
	case apiPrefix + "/session/bootstrap":
		return bootstrapOperation, method == http.MethodPost
	case apiPrefix + "/workspace":
		return workspaceOperation, method == http.MethodGet
	case apiPrefix + "/graph/query":
		return graphOperation, method == http.MethodPost
	case apiPrefix + "/source/read":
		return sourceOperation, method == http.MethodPost
	case apiPrefix + "/preview/status":
		return previewStatusOperation, method == http.MethodPost
	default:
		return "", false
	}
}

func methodForPath(path string) string {
	switch path {
	case apiPrefix + "/session/bootstrap", apiPrefix + "/graph/query", apiPrefix + "/source/read", apiPrefix + "/preview/status":
		return http.MethodPost
	case apiPrefix + "/workspace":
		return http.MethodGet
	default:
		return ""
	}
}

func (s *Server) successResponse(operation string, data any) ([]byte, error) {
	requestID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	maxBytes := int64(maxUIResponseBytes)
	if operation == bootstrapOperation {
		maxBytes = maxBootstrapBytes
	}
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": "ok", "scope": nil,
		"freshness": map[string]any{"status": "not_checked", "scope": "none", "checked_at": nil, "buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage":  map[string]any{"status": "unknown", "capabilities": []any{}, "gaps": []any{}, "scope_description": "Scope is supplied by the authorized application service."},
		"readiness": "needs_context", "data": data,
		"pagination": map[string]any{"truncated": false, "enumeration_complete": true, "returned_count": 0, "required_pending": nil, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil},
		"budget":     map[string]any{"max_output_bytes": maxBytes, "serialized_bytes": 0, "token_count": nil, "token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "http"},
		"warnings":   []any{}, "error": nil,
	}
	return s.marshalResponse(operation, response)
}

func (s *Server) writeError(w http.ResponseWriter, operation string, status int, code, message string, retryable bool, action string) {
	if operation == "" {
		http.Error(w, message, status)
		return
	}
	requestID, err := randomUUID()
	if err != nil {
		http.Error(w, "request failed", http.StatusInternalServerError)
		return
	}
	maxBytes := int64(4096)
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": "error", "scope": nil,
		"freshness": map[string]any{"status": "not_checked", "scope": "none", "checked_at": nil, "buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage":  map[string]any{"status": "unknown", "capabilities": []any{}, "gaps": []any{}, "scope_description": "Operation was not completed."},
		"readiness": "blocked", "data": nil,
		"pagination": map[string]any{"truncated": false, "enumeration_complete": true, "returned_count": 0, "required_pending": nil, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil},
		"budget":     map[string]any{"max_output_bytes": maxBytes, "serialized_bytes": 0, "token_count": nil, "token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "http"},
		"warnings":   []any{}, "error": map[string]any{"code": code, "message": message, "retryable": retryable, "suggested_action": action, "details": []any{}},
	}
	raw, err := s.marshalResponse(operation, response)
	if err != nil {
		http.Error(w, message, status)
		return
	}
	s.writeJSON(w, status, raw)
}

func (s *Server) marshalResponse(operation string, response map[string]any) ([]byte, error) {
	for range 8 {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		budget := response["budget"].(map[string]any)
		if budget["serialized_bytes"] == len(raw) {
			if operation != "" {
				if err := s.validator.ValidateResponse(operation, raw); err != nil {
					return nil, err
				}
			}
			return raw, nil
		}
		budget["serialized_bytes"] = len(raw)
	}
	return nil, errors.New("Explorer response byte count did not converge")
}

func requestLimit(operation string) int64 {
	switch operation {
	case bootstrapOperation, previewStatusOperation:
		return maxBootstrapBytes
	case sourceOperation:
		return maxCoreRequestBytes
	default:
		return maxUIRequestBytes
	}
}

func validResponseBudget(raw []byte) bool {
	var envelope struct {
		Budget struct {
			MaxOutputBytes  int64  `json:"max_output_bytes"`
			SerializedBytes int64  `json:"serialized_bytes"`
			DeliveryMode    string `json:"delivery_mode"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false
	}
	return envelope.Budget.DeliveryMode == "http" && envelope.Budget.MaxOutputBytes >= 4096 &&
		envelope.Budget.MaxOutputBytes <= maxUIResponseBytes && envelope.Budget.SerializedBytes == int64(len(raw)) &&
		int64(len(raw)) <= envelope.Budget.MaxOutputBytes
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, raw []byte) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(raw)
	return err
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func grantDeadline(now time.Time, grant AccessGrant, limit time.Duration) (time.Time, time.Time, []string, bool) {
	if grant.ExpiresAt.IsZero() || !grant.ExpiresAt.After(now) || limit <= 0 {
		return time.Time{}, time.Time{}, nil, false
	}
	remaining := grant.ExpiresAt.Sub(now)
	if remaining > limit {
		remaining = limit
	}
	deadline := now.Add(remaining)
	expiresAt := now.Add(remaining).UTC()
	capabilities, ok := normalizeCapabilities(grant.Capabilities)
	if !ok {
		return time.Time{}, time.Time{}, nil, false
	}
	return deadline, expiresAt, capabilities, true
}

func normalizeCapabilities(values []string) ([]string, bool) {
	if len(values) > 64 {
		return nil, false
	}
	result := append([]string{}, values...)
	sort.Strings(result)
	for index, value := range result {
		if value == "" || len(value) > 128 || (index > 0 && result[index-1] == value) {
			return nil, false
		}
	}
	return result, true
}

func intersect(left, right []string) []string {
	allowed := make(map[string]struct{}, len(right))
	for _, value := range right {
		allowed[value] = struct{}{}
	}
	result := make([]string, 0, len(left))
	for _, value := range left {
		if _, ok := allowed[value]; ok {
			result = append(result, value)
		}
	}
	return result
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	hex := "0123456789abcdef"
	var out [36]byte
	positions := [...]int{8, 13, 18, 23}
	for _, position := range positions {
		out[position] = '-'
	}
	valueIndex := 0
	for index := range out {
		if out[index] == '-' {
			continue
		}
		byteValue := value[valueIndex/2]
		if valueIndex%2 == 0 {
			out[index] = hex[byteValue>>4]
		} else {
			out[index] = hex[byteValue&0x0f]
		}
		valueIndex++
	}
	return string(out[:]), nil
}

func authMessage(code string) string {
	switch code {
	case "AUTH_REQUIRED":
		return "Explorer authentication is required."
	case "AUTH_EXPIRED":
		return "Explorer session has expired."
	default:
		return "Explorer session is invalid or no longer authorized."
	}
}

func authAction(code string) string {
	if code == "AUTH_REQUIRED" || code == "AUTH_EXPIRED" {
		return "reauthenticate"
	}
	return "none"
}

// validCSRFForMutation enforces a double-submit cookie/header token bound to
// the authenticated session. Read-only POST operations intentionally do not
// call it; every mutating route must call it before dispatch.
func (s *Server) validCSRFForMutation(r *http.Request, key [32]byte) bool {
	if r == nil || !s.validOrigin(r) {
		return false
	}
	csrfHeaders := r.Header.Values(csrfHeaderName)
	cookies := r.Cookies()
	var csrfCookie string
	cookieCount := 0
	for _, cookie := range cookies {
		if cookie.Name == csrfCookieName {
			cookieCount++
			csrfCookie = cookie.Value
		}
	}
	if len(csrfHeaders) != 1 || cookieCount != 1 || subtle.ConstantTimeCompare([]byte(csrfHeaders[0]), []byte(csrfCookie)) != 1 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(csrfHeaders[0])
	if err != nil || len(decoded) != 32 {
		return false
	}
	providedHash := sha256.Sum256(decoded)
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.sessions[key]
	now := s.now()
	return ok && now.Before(current.deadline) && now.Before(current.expiresAt) && subtle.ConstantTimeCompare(providedHash[:], current.csrfHash[:]) == 1
}
