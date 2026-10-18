package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxLSPMessageBytes = 32 << 20
	maxLSPHeaderBytes  = 8 << 10
	maxPendingLSPCalls = 64
)

var (
	ErrLSPProtocol              = errors.New("invalid LSP JSON-RPC protocol message")
	ErrLSPClosed                = errors.New("LSP session is closed")
	ErrForbiddenServerOperation = errors.New("LSP server requested a forbidden operation")
	ErrUnsupportedMethod        = errors.New("LSP method is not allowed")
	ErrLSPResourceLimit         = errors.New("LSP request limit exceeded")
)

type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcTransport interface {
	io.Reader
	io.Writer
	io.Closer
}

type serverRequestHandler func(context.Context, string, json.RawMessage) (json.RawMessage, *rpcError)

type rpcClient struct {
	transport rpcTransport
	reader    *bufio.Reader
	ctx       context.Context
	cancel    context.CancelFunc
	handler   serverRequestHandler

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan rpcMessage
	nextID    atomic.Uint64
	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func newRPCClient(parent context.Context, transport rpcTransport, handler serverRequestHandler) (*rpcClient, error) {
	if parent == nil || transport == nil {
		return nil, ErrLSPProtocol
	}
	ctx, cancel := context.WithCancel(parent)
	client := &rpcClient{
		transport: transport, reader: bufio.NewReaderSize(transport, 32<<10), ctx: ctx, cancel: cancel,
		handler: handler, pending: make(map[string]chan rpcMessage), done: make(chan struct{}),
	}
	go client.readLoop()
	go func() {
		select {
		case <-ctx.Done():
			client.fail(ctx.Err())
			_ = transport.Close()
		case <-client.done:
		}
	}()
	return client, nil
}

func (client *rpcClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if client == nil || ctx == nil || !allowedRequestMethod(method) {
		return nil, ErrUnsupportedMethod
	}
	requestID := strconv.FormatUint(client.nextID.Add(1), 10)
	id := json.RawMessage(requestID)
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode LSP request params: %w", ErrLSPProtocol)
	}
	response := make(chan rpcMessage, 1)
	pendingKey := "n:" + requestID
	client.pendingMu.Lock()
	select {
	case <-client.done:
		client.pendingMu.Unlock()
		return nil, client.closedError()
	default:
	}
	if len(client.pending) >= maxPendingLSPCalls {
		client.pendingMu.Unlock()
		return nil, ErrLSPResourceLimit
	}
	client.pending[pendingKey] = response
	client.pendingMu.Unlock()

	if err := client.writeMessage(ctx, rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: encodedParams}); err != nil {
		client.removePending(pendingKey)
		return nil, err
	}
	select {
	case message := <-response:
		if message.Error != nil {
			return nil, &RemoteError{Code: message.Error.Code, Message: safeRemoteMessage(message.Error.Message)}
		}
		if message.Result == nil {
			return nil, ErrLSPProtocol
		}
		return append(json.RawMessage(nil), message.Result...), nil
	case <-ctx.Done():
		client.removePending(pendingKey)
		_ = client.Notify(context.Background(), "$/cancelRequest", map[string]any{"id": json.Number(requestID)})
		return nil, ctx.Err()
	case <-client.done:
		client.removePending(pendingKey)
		return nil, client.closedError()
	}
}

func (client *rpcClient) Notify(ctx context.Context, method string, params any) error {
	if client == nil || ctx == nil || !allowedNotificationMethod(method) {
		return ErrUnsupportedMethod
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode LSP notification params: %w", ErrLSPProtocol)
	}
	return client.writeMessage(ctx, rpcMessage{JSONRPC: "2.0", Method: method, Params: encodedParams})
}

func (client *rpcClient) Close() error {
	if client == nil {
		return nil
	}
	client.fail(ErrLSPClosed)
	client.cancel()
	return client.transport.Close()
}

func (client *rpcClient) Wait() error {
	if client == nil {
		return ErrLSPClosed
	}
	<-client.done
	return client.closedError()
}

func (client *rpcClient) writeMessage(ctx context.Context, message rpcMessage) error {
	if ctx == nil || message.JSONRPC != "2.0" {
		return ErrLSPProtocol
	}
	select {
	case <-client.done:
		return client.closedError()
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	encoded, err := json.Marshal(message)
	if err != nil || len(encoded) == 0 || len(encoded) > maxLSPMessageBytes {
		return ErrLSPProtocol
	}
	frame := make([]byte, 0, len(encoded)+40)
	frame = append(frame, "Content-Length: "...)
	frame = strconv.AppendInt(frame, int64(len(encoded)), 10)
	frame = append(frame, '\r', '\n', '\r', '\n')
	frame = append(frame, encoded...)
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	select {
	case <-client.done:
		return client.closedError()
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	for len(frame) > 0 {
		written, writeErr := client.transport.Write(frame)
		if writeErr != nil {
			client.fail(writeErr)
			return fmt.Errorf("write LSP frame: %w", ErrLSPProtocol)
		}
		if written <= 0 || written > len(frame) {
			client.fail(io.ErrShortWrite)
			return ErrLSPProtocol
		}
		frame = frame[written:]
	}
	return nil
}

func (client *rpcClient) readLoop() {
	for {
		body, err := readLSPFrame(client.reader)
		if err != nil {
			client.fail(err)
			return
		}
		message, err := decodeRPCMessage(body)
		if err != nil {
			client.fail(err)
			return
		}
		if message.Method != "" {
			if len(message.ID) == 0 {
				if message.Method == "workspace/applyEdit" || message.Method == "workspace/executeCommand" {
					client.fail(ErrForbiddenServerOperation)
					return
				}
				continue
			}
			client.handleServerRequest(message)
			continue
		}
		if len(message.ID) == 0 {
			client.fail(ErrLSPProtocol)
			return
		}
		key, ok := rpcIDKey(message.ID)
		if !ok {
			client.fail(ErrLSPProtocol)
			return
		}
		client.pendingMu.Lock()
		response := client.pending[key]
		delete(client.pending, key)
		client.pendingMu.Unlock()
		if response != nil {
			response <- message
		}
	}
}

func (client *rpcClient) handleServerRequest(message rpcMessage) {
	result, rpcErr := client.handleServerRequestResult(message)
	response := rpcMessage{JSONRPC: "2.0", ID: append(json.RawMessage(nil), message.ID...)}
	if rpcErr != nil {
		response.Error = rpcErr
	} else {
		if result == nil {
			result = json.RawMessage("null")
		}
		response.Result = result
	}
	if err := client.writeMessage(client.ctx, response); err != nil {
		client.fail(err)
	}
}

func (client *rpcClient) handleServerRequestResult(message rpcMessage) (json.RawMessage, *rpcError) {
	if message.Method == "workspace/applyEdit" {
		result, _ := json.Marshal(map[string]any{"applied": false, "failureReason": "source mutation is not permitted"})
		return result, nil
	}
	if message.Method == "workspace/executeCommand" {
		return nil, &rpcError{Code: -32601, Message: "workspace commands are not permitted"}
	}
	if client.handler == nil {
		return nil, &rpcError{Code: -32601, Message: "server request is not supported"}
	}
	return client.handler(client.ctx, message.Method, message.Params)
}

func (client *rpcClient) removePending(id string) {
	client.pendingMu.Lock()
	delete(client.pending, id)
	client.pendingMu.Unlock()
}

func (client *rpcClient) fail(err error) {
	if client == nil {
		return
	}
	client.closeOnce.Do(func() {
		client.errMu.Lock()
		if err == nil {
			err = ErrLSPClosed
		}
		client.err = err
		client.errMu.Unlock()
		close(client.done)
		client.cancel()
	})
}

func (client *rpcClient) closedError() error {
	client.errMu.Lock()
	defer client.errMu.Unlock()
	if client.err == nil {
		return ErrLSPClosed
	}
	return client.err
}

func readLSPFrame(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, ErrLSPProtocol
	}
	contentLength := -1
	headerBytes := 0
	for {
		line, err := readHeaderLine(reader)
		if err != nil {
			return nil, fmt.Errorf("read LSP header: %w", ErrLSPProtocol)
		}
		headerBytes += len(line)
		if headerBytes > maxLSPHeaderBytes {
			return nil, ErrLSPProtocol
		}
		lineText := strings.TrimSuffix(string(line), "\n")
		lineText = strings.TrimSuffix(lineText, "\r")
		if lineText == "" {
			break
		}
		name, value, ok := strings.Cut(lineText, ":")
		if !ok {
			return nil, ErrLSPProtocol
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			if contentLength >= 0 {
				return nil, ErrLSPProtocol
			}
			parsed, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
			if parseErr != nil || parsed < 0 || parsed > maxLSPMessageBytes {
				return nil, ErrLSPProtocol
			}
			contentLength = int(parsed)
		}
	}
	if contentLength < 0 {
		return nil, ErrLSPProtocol
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read LSP body: %w", ErrLSPProtocol)
	}
	return body, nil
}

func readHeaderLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 128)
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxLSPHeaderBytes {
			return nil, ErrLSPProtocol
		}
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func decodeRPCMessage(body []byte) (rpcMessage, error) {
	var message rpcMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return rpcMessage{}, ErrLSPProtocol
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || message.JSONRPC != "2.0" {
		return rpcMessage{}, ErrLSPProtocol
	}
	if message.Method != "" {
		if message.Result != nil || message.Error != nil || len(message.Params) == 0 || !json.Valid(message.Params) {
			return rpcMessage{}, ErrLSPProtocol
		}
		if len(message.ID) > 0 {
			if _, ok := rpcIDKey(message.ID); !ok {
				return rpcMessage{}, ErrLSPProtocol
			}
		}
		return message, nil
	}
	if len(message.ID) == 0 || ((message.Error == nil) == (message.Result == nil)) {
		return rpcMessage{}, ErrLSPProtocol
	}
	if _, ok := rpcIDKey(message.ID); !ok || (message.Result != nil && !json.Valid(message.Result)) {
		return rpcMessage{}, ErrLSPProtocol
	}
	if message.Error != nil && message.Error.Message == "" {
		return rpcMessage{}, ErrLSPProtocol
	}
	return message, nil
}

func rpcIDKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil || text == "" {
			return "", false
		}
		return "s:" + text, true
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil || number.String() == "" {
		return "", false
	}
	if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
		return "", false
	}
	return "n:" + number.String(), true
}

func allowedRequestMethod(method string) bool {
	switch method {
	case "initialize", "shutdown", "textDocument/prepareRename", "textDocument/rename":
		return true
	default:
		return false
	}
}

func allowedNotificationMethod(method string) bool {
	switch method {
	case "initialized", "exit", "textDocument/didOpen", "textDocument/didClose", "textDocument/didChange",
		"workspace/didChangeConfiguration", "$/cancelRequest":
		return true
	default:
		return false
	}
}

func safeRemoteMessage(value string) string {
	if len(value) > 256 {
		return "LSP server returned an error"
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "LSP server returned an error"
		}
	}
	return value
}

type RemoteError struct {
	Code    int64
	Message string
}

func (remote *RemoteError) Error() string {
	if remote == nil {
		return "LSP server error"
	}
	return fmt.Sprintf("LSP server error %d: %s", remote.Code, remote.Message)
}
