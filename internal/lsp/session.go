package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	defaultLSPStartupTimeout  = 30 * time.Second
	defaultLSPReadTimeout     = 30 * time.Second
	defaultLSPShutdownTimeout = 3 * time.Second
	maxLSPTimeout             = 2 * time.Minute
)

var (
	ErrLSPTimeout        = errors.New("LSP operation exceeded its deadline")
	ErrLSPNotReady       = errors.New("pinned LSP provider is not ready")
	ErrNotRenameable     = errors.New("selected source position is not renameable")
	ErrRenameConflict    = errors.New("LSP rename contains conflicting edits")
	ErrRenameUnsupported = errors.New("LSP rename contains an unsupported operation")
	lspSessionSlots      = make(chan struct{}, 1)
)

type SessionConfig struct {
	Snapshot        WorkspaceSnapshot
	Profile         GoProfile
	Policy          ExecutionPolicy
	StartupTimeout  time.Duration
	ReadTimeout     time.Duration
	RenameTimeout   time.Duration
	ShutdownTimeout time.Duration
}

type Session struct {
	ctx             context.Context
	cancel          context.CancelFunc
	workspace       *validatedWorkspace
	profile         GoProfile
	rootPath        string
	scope           model.ScopeVector
	encoding        PositionEncoding
	client          *rpcClient
	stdin           io.WriteCloser
	process         processTreeGuard
	processDone     chan struct{}
	waitMu          sync.Mutex
	waitErr         error
	stderrDone      chan struct{}
	shutdownTimeout time.Duration
	readTimeout     time.Duration
	renameTimeout   time.Duration
	opMu            sync.Mutex
	closeOnce       sync.Once
	closeErr        error
	releaseSlot     func()
}

type PreparedRename struct {
	Range       model.ByteRange `json:"range"`
	Placeholder string          `json:"placeholder"`
	Warnings    []string        `json:"warnings,omitempty"`
}

type lspTextDocumentIdentifier struct {
	URI string `json:"uri"`
}

func StartSession(parent context.Context, config SessionConfig) (*Session, error) {
	if parent == nil {
		return nil, ErrInvalidLSPProfile
	}
	startupTimeout := config.StartupTimeout
	if startupTimeout == 0 {
		startupTimeout = defaultLSPStartupTimeout
	}
	readTimeout := config.ReadTimeout
	if readTimeout == 0 {
		readTimeout = defaultLSPReadTimeout
	}
	shutdownTimeout := config.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = defaultLSPShutdownTimeout
	}
	renameTimeout := config.RenameTimeout
	if renameTimeout == 0 {
		renameTimeout = defaultLSPStartupTimeout
	}
	if startupTimeout < time.Second || startupTimeout > maxLSPTimeout || readTimeout < time.Second || readTimeout > maxLSPTimeout ||
		renameTimeout < time.Second || renameTimeout > maxLSPTimeout || shutdownTimeout < time.Second || shutdownTimeout > maxLSPTimeout {
		return nil, ErrInvalidLSPProfile
	}
	if err := validateProfileSummary(config.Profile); err != nil || config.Profile.MaxFileBytes > config.Policy.MaxFileBytes || config.Profile.MaxSourceBytes > config.Policy.MaxSourceBytes {
		return nil, ErrInvalidLSPProfile
	}
	rootPath, env, err := validateGoProfile(config.Profile, config.Policy, config.Snapshot.Root)
	if err != nil {
		return nil, err
	}
	select {
	case lspSessionSlots <- struct{}{}:
	case <-parent.Done():
		return nil, parent.Err()
	}
	releaseSlot := func() { <-lspSessionSlots }
	keepSlot := false
	defer func() {
		if !keepSlot {
			releaseSlot()
		}
	}()
	readCtx, cancelRead := context.WithTimeout(parent, readTimeout)
	workspace, err := validateWorkspaceSnapshot(readCtx, config.Snapshot, config.Profile)
	if err == nil {
		err = verifyLSPModuleClosure(readCtx, workspace, config.Profile)
		if err != nil {
			_ = workspace.close()
			workspace = nil
		}
	}
	cancelRead()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
			return nil, ErrLSPTimeout
		}
		return nil, err
	}
	startupCtx, startupCancel := context.WithTimeout(parent, startupTimeout)
	defer startupCancel()
	if err := verifyExecutableVersion(startupCtx, config.Profile.Go, rootPath, env); err != nil {
		_ = workspace.close()
		if errors.Is(startupCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrLSPTimeout
		}
		if startupCtx.Err() != nil {
			return nil, startupCtx.Err()
		}
		return nil, fmt.Errorf("verify pinned Go tool: %w", ErrExecutableUnavailable)
	}
	if err := verifyExecutableVersion(startupCtx, config.Profile.Gopls, rootPath, env); err != nil {
		_ = workspace.close()
		if errors.Is(startupCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrLSPTimeout
		}
		if startupCtx.Err() != nil {
			return nil, startupCtx.Err()
		}
		return nil, fmt.Errorf("verify pinned gopls tool: %w", ErrExecutableUnavailable)
	}

	sessionCtx, cancel := context.WithCancel(parent)
	command := executableCommand(config.Profile.Gopls)
	command = exec.CommandContext(sessionCtx, command.Path, command.Args[1:]...)
	command.Dir = rootPath
	command.Env = environmentList(env)
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		_ = workspace.close()
		return nil, ErrLSPNotReady
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cancel()
		_ = workspace.close()
		return nil, ErrLSPNotReady
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cancel()
		_ = workspace.close()
		return nil, ErrLSPNotReady
	}
	process, err := newProcessTreeGuard(command)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		cancel()
		_ = workspace.close()
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = process.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		cancel()
		_ = workspace.close()
		return nil, ErrLSPNotReady
	}
	if err := process.Attach(command); err != nil {
		_ = command.Process.Kill()
		_ = process.Kill()
		_ = command.Wait()
		_ = process.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		cancel()
		_ = workspace.close()
		return nil, ErrLSPNotReady
	}
	session := &Session{
		ctx: sessionCtx, cancel: cancel, workspace: workspace, profile: config.Profile,
		rootPath: rootPath, scope: config.Snapshot.Scope, stdin: stdin, process: process,
		processDone: make(chan struct{}), stderrDone: make(chan struct{}),
		shutdownTimeout: shutdownTimeout, readTimeout: readTimeout, renameTimeout: renameTimeout, releaseSlot: releaseSlot,
	}
	keepSlot = true
	go func() {
		defer close(session.stderrDone)
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		err := command.Wait()
		session.waitMu.Lock()
		session.waitErr = err
		session.waitMu.Unlock()
		close(session.processDone)
	}()
	transport := &stdioTransport{reader: stdout, writer: stdin, closers: []io.Closer{stdout, stdin}}
	client, err := newRPCClient(sessionCtx, transport, session.handleServerRequest)
	if err != nil {
		_ = session.Close()
		return nil, ErrLSPNotReady
	}
	session.client = client
	go func() {
		select {
		case <-sessionCtx.Done():
			_ = process.Kill()
		case <-session.processDone:
			cancel()
		}
	}()
	if err := session.initialize(startupCtx); err != nil {
		_ = session.Close()
		if errors.Is(startupCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrLSPTimeout
		}
		return nil, err
	}
	return session, nil
}

func (session *Session) initialize(ctx context.Context) error {
	if session == nil || session.client == nil || ctx == nil {
		return ErrLSPNotReady
	}
	rootURI, err := pathToFileURI(session.rootPath)
	if err != nil {
		return ErrLSPNotReady
	}
	capabilities := map[string]any{
		"general":      map[string]any{"positionEncodings": []string{"utf-16", "utf-8", "utf-32"}},
		"textDocument": map[string]any{"rename": map[string]any{"dynamicRegistration": false, "prepareSupport": true}},
		"workspace": map[string]any{
			"applyEdit": false, "configuration": true, "workspaceFolders": true,
			"workspaceEdit": map[string]any{"documentChanges": true},
		},
	}
	result, err := session.client.Call(ctx, "initialize", map[string]any{
		"processId": os.Getpid(), "clientInfo": map[string]string{"name": "maps-context-engine-4d", "version": "1"},
		"locale": "en-US", "rootUri": rootURI, "workspaceFolders": []map[string]string{{"uri": rootURI, "name": filepath.Base(session.rootPath)}},
		"capabilities": capabilities,
	})
	if err != nil {
		return fmt.Errorf("initialize gopls: %w", ErrLSPNotReady)
	}
	var initialized struct {
		Capabilities struct {
			PositionEncoding PositionEncoding `json:"positionEncoding"`
			RenameProvider   json.RawMessage  `json:"renameProvider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		return ErrLSPProtocol
	}
	session.encoding = initialized.Capabilities.PositionEncoding
	if session.encoding == "" {
		session.encoding = PositionUTF16
	}
	if ValidatePositionEncoding(session.encoding) != nil || !supportsPreparedRename(initialized.Capabilities.RenameProvider) {
		return ErrLSPNotReady
	}
	if err := session.client.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return ErrLSPNotReady
	}
	settings := map[string]any{"gopls": map[string]any{
		"staticcheck": false, "analyses": map[string]bool{}, "tests": session.profile.TestsEnabled,
	}}
	if err := session.client.Notify(ctx, "workspace/didChangeConfiguration", map[string]any{"settings": settings}); err != nil {
		return ErrLSPNotReady
	}
	return nil
}

func supportsPreparedRename(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return false
	}
	if string(raw) == "true" {
		return true
	}
	var provider struct {
		PrepareProvider bool `json:"prepareProvider"`
	}
	return json.Unmarshal(raw, &provider) == nil && provider.PrepareProvider
}

func (session *Session) Rename(ctx context.Context, source model.SourceRef, newName string) (PreparedRename, json.RawMessage, error) {
	if session == nil || ctx == nil {
		return PreparedRename{}, nil, ErrLSPClosed
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if session.client == nil || session.ctx.Err() != nil {
		return PreparedRename{}, nil, ErrLSPClosed
	}
	if !tokenIsIdentifier(newName) {
		return PreparedRename{}, nil, ErrInvalidRenameName
	}
	readCtx, cancelRead := context.WithTimeout(ctx, session.readTimeout)
	sourceBytes, err := session.workspace.readSource(readCtx, source.Path, source.ContentHash, session.profile.MaxFileBytes)
	cancelRead()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return PreparedRename{}, nil, ErrLSPTimeout
		}
		return PreparedRename{}, nil, err
	}
	defer clear(sourceBytes)
	if err := validateSourceForScope(source, sourceBytes, session.workspaceScope()); err != nil {
		return PreparedRename{}, nil, err
	}
	warnings, packageName := assessRenameSite(sourceBytes, source.Path, source.Range.StartByte)
	if packageName {
		return PreparedRename{}, nil, ErrRenameUnsupported
	}
	uri, err := pathToFileURI(filepath.Join(session.rootPath, filepath.FromSlash(source.Path)))
	if err != nil {
		return PreparedRename{}, nil, ErrCapturedWorkspace
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, session.readTimeout)
	openErr := session.client.Notify(openCtx, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": int64(1), "text": string(sourceBytes)},
	})
	openContextErr := openCtx.Err()
	cancelOpen()
	if openErr != nil {
		if errors.Is(openContextErr, context.DeadlineExceeded) && ctx.Err() == nil {
			return PreparedRename{}, nil, ErrLSPTimeout
		}
		return PreparedRename{}, nil, openErr
	}
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(session.ctx, session.shutdownTimeout)
		defer cancelClose()
		_ = session.client.Notify(closeCtx, "textDocument/didClose", map[string]any{"textDocument": lspTextDocumentIdentifier{URI: uri}})
	}()
	position, err := ByteOffsetToPosition(sourceBytes, source.Range.StartByte, session.encoding)
	if err != nil {
		return PreparedRename{}, nil, err
	}
	documentPosition := map[string]any{"textDocument": lspTextDocumentIdentifier{URI: uri}, "position": position}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, session.renameTimeout)
	prepareRaw, err := session.client.Call(prepareCtx, "textDocument/prepareRename", documentPosition)
	prepareContextErr := prepareCtx.Err()
	cancelPrepare()
	if err != nil {
		if errors.Is(prepareContextErr, context.DeadlineExceeded) {
			return PreparedRename{}, nil, ErrLSPTimeout
		}
		if ctx.Err() != nil {
			return PreparedRename{}, nil, ctx.Err()
		}
		return PreparedRename{}, nil, fmt.Errorf("prepare Go rename: %w", ErrNotRenameable)
	}
	prepared, err := decodePreparedRename(prepareRaw, sourceBytes, session.encoding, source.Range.StartByte)
	if err != nil {
		return PreparedRename{}, nil, err
	}
	prepared.Warnings = warnings
	renameCtx, cancelRename := context.WithTimeout(ctx, session.renameTimeout)
	renameRaw, err := session.client.Call(renameCtx, "textDocument/rename", map[string]any{
		"textDocument": lspTextDocumentIdentifier{URI: uri}, "position": position, "newName": newName,
	})
	renameContextErr := renameCtx.Err()
	cancelRename()
	if err != nil {
		if errors.Is(renameContextErr, context.DeadlineExceeded) {
			return PreparedRename{}, nil, ErrLSPTimeout
		}
		if ctx.Err() != nil {
			return PreparedRename{}, nil, ctx.Err()
		}
		return PreparedRename{}, nil, fmt.Errorf("compute Go rename: %w", ErrLSPNotReady)
	}
	if !json.Valid(renameRaw) || len(renameRaw) > maxLSPMessageBytes {
		return PreparedRename{}, nil, ErrLSPProtocol
	}
	return prepared, append(json.RawMessage(nil), renameRaw...), nil
}

func (session *Session) workspaceScope() model.ScopeVector {
	return session.scope
}

func (session *Session) handleServerRequest(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *rpcError) {
	switch method {
	case "workspace/configuration":
		var request struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(params, &request) != nil || len(request.Items) > 4096 {
			return nil, &rpcError{Code: -32602, Message: "invalid workspace configuration request"}
		}
		values := make([]json.RawMessage, len(request.Items))
		for index := range values {
			values[index] = json.RawMessage("null")
		}
		encoded, _ := json.Marshal(values)
		return encoded, nil
	case "workspace/workspaceFolders":
		uri, err := pathToFileURI(session.rootPath)
		if err != nil {
			return nil, &rpcError{Code: -32603, Message: "workspace is unavailable"}
		}
		encoded, _ := json.Marshal([]map[string]string{{"uri": uri, "name": filepath.Base(session.rootPath)}})
		return encoded, nil
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		return json.RawMessage("null"), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "server request is not supported"}
	}
}

func (session *Session) Close() error {
	if session == nil {
		return nil
	}
	session.closeOnce.Do(func() {
		session.opMu.Lock()
		defer session.opMu.Unlock()
		session.closeErr = session.close()
	})
	return session.closeErr
}

func (session *Session) close() error {
	if session.client != nil && session.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), session.shutdownTimeout)
		_, _ = session.client.Call(ctx, "shutdown", map[string]any{})
		_ = session.client.Notify(ctx, "exit", map[string]any{})
		cancel()
	}
	if session.stdin != nil {
		_ = session.stdin.Close()
	}
	var waitErr error
	timedOut := false
	select {
	case <-session.processDone:
		waitErr = session.processWaitError()
	case <-time.After(session.shutdownTimeout):
		timedOut = true
		_ = session.process.Kill()
		select {
		case <-session.processDone:
			waitErr = session.processWaitError()
		case <-time.After(session.shutdownTimeout):
			waitErr = ErrLSPTimeout
		}
	}
	if session.client != nil {
		_ = session.client.Close()
	}
	if session.process != nil {
		if err := session.process.Close(); err != nil && waitErr == nil {
			waitErr = ErrLSPNotReady
		}
	}
	session.cancel()
	select {
	case <-session.stderrDone:
	case <-time.After(session.shutdownTimeout):
		if waitErr == nil {
			waitErr = ErrLSPTimeout
		}
	}
	if err := session.workspace.close(); err != nil && waitErr == nil {
		waitErr = ErrCapturedWorkspace
	}
	if session.releaseSlot != nil {
		session.releaseSlot()
		session.releaseSlot = nil
	}
	if timedOut {
		return ErrLSPTimeout
	}
	if waitErr != nil && !errors.Is(waitErr, os.ErrProcessDone) {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) && exitError.ExitCode() == 0 {
			return nil
		}
		if errors.Is(waitErr, ErrLSPTimeout) || errors.Is(waitErr, ErrCapturedWorkspace) {
			return waitErr
		}
		return ErrLSPNotReady
	}
	return nil
}

func decodePreparedRename(raw json.RawMessage, source []byte, encoding PositionEncoding, cursor int64) (PreparedRename, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return PreparedRename{}, ErrNotRenameable
	}
	var response struct {
		Range       Range  `json:"range"`
		Placeholder string `json:"placeholder"`
	}
	if decodeStrictJSON(raw, &response) != nil || response.Placeholder == "" || !utf8.ValidString(response.Placeholder) {
		return PreparedRename{}, ErrLSPProtocol
	}
	start, err := PositionToByteOffset(source, response.Range.Start, encoding)
	if err != nil {
		return PreparedRename{}, err
	}
	end, err := PositionToByteOffset(source, response.Range.End, encoding)
	if err != nil || start >= end || cursor < start || cursor >= end || string(source[start:end]) != response.Placeholder {
		return PreparedRename{}, ErrNotRenameable
	}
	return PreparedRename{Range: model.ByteRange{StartByte: start, EndByte: end}, Placeholder: response.Placeholder}, nil
}

func validateSourceForScope(source model.SourceRef, content []byte, scope model.ScopeVector) error {
	if source.ValidateAgainst(content) != nil || scope.Validate() != nil || len(scope.Members) != 1 {
		return ErrCapturedWorkspace
	}
	member := scope.Members[0]
	if source.WorktreeID != member.WorktreeID || source.ProfileID != member.ProfileID || source.GenerationID != member.GenerationID {
		return ErrCapturedWorkspace
	}
	return nil
}

func pathToFileURI(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrCapturedWorkspace
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", ErrCapturedWorkspace
	}
	slashed := filepath.ToSlash(filepath.Clean(absolute))
	if len(slashed) >= 2 && slashed[1] == ':' {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String(), nil
}

func fileURIToRelative(root, uri string, files map[string]SnapshotFile) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" {
		return "", ErrCapturedWorkspace
	}
	path := parsed.Path
	if len(path) >= 3 && path[0] == '/' && ((path[1] >= 'A' && path[1] <= 'Z') || (path[1] >= 'a' && path[1] <= 'z')) && path[2] == ':' {
		path = path[1:]
	}
	path = filepath.FromSlash(path)
	if !filepath.IsAbs(path) {
		return "", ErrCapturedWorkspace
	}
	absolute, err := filepath.Abs(path)
	if err != nil || !pathWithin(root, absolute) {
		return "", ErrCapturedWorkspace
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return "", ErrCapturedWorkspace
	}
	relative = filepath.ToSlash(relative)
	if model.ValidateRelativePath(relative) != nil {
		return "", ErrCapturedWorkspace
	}
	for declared := range files {
		if samePath(declared, relative) {
			return declared, nil
		}
	}
	return "", ErrCapturedWorkspace
}

type stdioTransport struct {
	reader  io.ReadCloser
	writer  io.WriteCloser
	closers []io.Closer
	mu      sync.Once
}

func (transport *stdioTransport) Read(value []byte) (int, error) { return transport.reader.Read(value) }
func (transport *stdioTransport) Write(value []byte) (int, error) {
	return transport.writer.Write(value)
}
func (transport *stdioTransport) Close() error {
	var errs []error
	transport.mu.Do(func() {
		for _, closer := range transport.closers {
			if err := closer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				errs = append(errs, err)
			}
		}
	})
	return errors.Join(errs...)
}

func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func (session *Session) processWaitError() error {
	session.waitMu.Lock()
	defer session.waitMu.Unlock()
	return session.waitErr
}
