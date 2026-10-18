package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestReadLSPFrameCountsUTF8BytesAndRejectsInvalidHeaders(t *testing.T) {
	body := []byte(`{"text":"ສາຍ😀"}`)
	frame := append([]byte("Content-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"), body...)
	got, err := readLSPFrame(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("readLSPFrame=%q, %v; want %q", got, err, body)
	}
	for _, header := range []string{
		"Content-Length: 1\r\nContent-Length: 1\r\n\r\nx",
		"Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\nx",
		"Content-Length: 33554433\r\n\r\n",
	} {
		if _, err := readLSPFrame(bufio.NewReader(bytes.NewBufferString(header))); !errors.Is(err, ErrLSPProtocol) {
			t.Errorf("invalid frame %q error=%v", header, err)
		}
	}
}

func TestRPCClientCorrelatesConcurrentResponsesByRequestID(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := newRPCClient(context.Background(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		requests := make([]rpcMessage, 2)
		for index := range requests {
			body, readErr := readLSPFrame(reader)
			if readErr != nil {
				serverDone <- readErr
				return
			}
			requests[index], readErr = decodeRPCMessage(body)
			if readErr != nil {
				serverDone <- readErr
				return
			}
		}
		for index := len(requests) - 1; index >= 0; index-- {
			result, _ := json.Marshal(map[string]string{"method": requests[index].Method})
			if writeErr := writeTestRPC(serverConn, rpcMessage{JSONRPC: "2.0", ID: requests[index].ID, Result: result}); writeErr != nil {
				serverDone <- writeErr
				return
			}
		}
		serverDone <- nil
	}()

	methods := []string{"textDocument/prepareRename", "textDocument/rename"}
	var wait sync.WaitGroup
	errorsByMethod := make(chan error, len(methods))
	for _, method := range methods {
		method := method
		wait.Add(1)
		go func() {
			defer wait.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, callErr := client.Call(ctx, method, map[string]string{"method": method})
			if callErr != nil {
				errorsByMethod <- callErr
				return
			}
			var value map[string]string
			if err := json.Unmarshal(result, &value); err != nil || value["method"] != method {
				errorsByMethod <- ErrLSPProtocol
			}
		}()
	}
	wait.Wait()
	close(errorsByMethod)
	for callErr := range errorsByMethod {
		t.Errorf("request correlation failed: %v", callErr)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestRPCClientRejectsServerEditAndCommandRequests(t *testing.T) {
	for _, method := range []string{"workspace/applyEdit", "workspace/executeCommand"} {
		t.Run(method, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			client, err := newRPCClient(context.Background(), clientConn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			request := rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: method, Params: json.RawMessage(`{}`)}
			if err := writeTestRPC(serverConn, request); err != nil {
				t.Fatal(err)
			}
			body, err := readLSPFrame(bufio.NewReader(serverConn))
			if err != nil {
				t.Fatal(err)
			}
			response, err := decodeRPCMessage(body)
			if err != nil {
				t.Fatal(err)
			}
			if string(response.ID) != "7" {
				t.Fatalf("response id=%s", response.ID)
			}
			if method == "workspace/applyEdit" {
				var result struct {
					Applied bool `json:"applied"`
				}
				if err := json.Unmarshal(response.Result, &result); err != nil || result.Applied {
					t.Fatalf("workspace/applyEdit result=%s err=%v", response.Result, err)
				}
			} else if response.Error == nil {
				t.Fatalf("workspace/executeCommand was not rejected: %+v", response)
			}
			_ = serverConn.Close()
		})
	}
}

func TestRPCClientCancelsRequestAndRejectsUnlistedOutboundMethods(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := newRPCClient(context.Background(), clientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Call(context.Background(), "workspace/executeCommand", map[string]any{}); !errors.Is(err, ErrUnsupportedMethod) {
		t.Fatalf("unlisted method error=%v", err)
	}
	serverMessages := make(chan rpcMessage, 2)
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverConn)
		for index := 0; index < 2; index++ {
			body, readErr := readLSPFrame(reader)
			if readErr != nil {
				serverDone <- readErr
				return
			}
			message, decodeErr := decodeRPCMessage(body)
			if decodeErr != nil {
				serverDone <- decodeErr
				return
			}
			if index == 1 && message.Method != "$/cancelRequest" {
				serverDone <- ErrLSPProtocol
				return
			}
			serverMessages <- message
		}
		serverDone <- nil
	}()
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, callErr := client.Call(ctx, "textDocument/rename", map[string]any{})
		callDone <- callErr
	}()
	request := <-serverMessages
	if request.Method != "textDocument/rename" {
		t.Fatalf("unexpected request method %q", request.Method)
	}
	cancel()
	if callErr := <-callDone; !errors.Is(callErr, context.Canceled) {
		t.Fatalf("cancelled call error=%v", callErr)
	}
	cancellationDelivered := false
	select {
	case message := <-serverMessages:
		if message.Method != "$/cancelRequest" {
			t.Fatalf("expected cancellation notification, got %q", message.Method)
		}
		cancellationDelivered = true
	case <-client.done:
		if !errors.Is(client.closedError(), context.Canceled) {
			t.Fatalf("client closed before cancellation notification: %v", client.closedError())
		}
	case <-time.After(defaultLSPShutdownTimeout + time.Second):
		t.Fatal("cancellation notification neither arrived nor closed the client")
	}
	if cancellationDelivered {
		if err := <-serverDone; err != nil {
			t.Fatal(err)
		}
	}
	_ = serverConn.Close()
}

func TestRPCClientNotifyHonorsDeadlineWhenTransportWriteBlocks(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	transport := &observedWriteTransport{Conn: clientConn, writeStarted: make(chan struct{}, 1)}
	client, err := newRPCClient(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- client.Notify(ctx, "textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": "file:///fixture.go"}})
	}()
	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("notification did not reach the blocked transport write")
	}

	select {
	case err := <-writeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked notification error=%v; want context deadline", err)
		}
	case <-time.After(2 * time.Second):
		_ = serverConn.Close()
		_ = client.Close()
		select {
		case <-writeDone:
		case <-time.After(time.Second):
		}
		t.Fatal("notification write ignored its context deadline")
	}
}

func TestRPCClientCancellationBoundsCancellationWrite(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	transport := &observedWriteTransport{Conn: clientConn, writeStarted: make(chan struct{}, 2)}
	client, err := newRPCClient(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer serverConn.Close()

	writeDone := make(chan error, 1)
	go func() { writeDone <- client.notifyCancellation("1") }()
	select {
	case <-transport.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("cancel notification did not reach the blocked transport write")
	}

	select {
	case err := <-writeDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancel notification error=%v; want its bounded deadline", err)
		}
	case <-time.After(defaultLSPShutdownTimeout + time.Second):
		_ = serverConn.Close()
		_ = client.Close()
		select {
		case <-writeDone:
		case <-time.After(time.Second):
		}
		t.Fatal("cancel notification write exceeded its bounded deadline")
	}
}

type observedWriteTransport struct {
	net.Conn
	writeStarted chan struct{}
}

func (transport *observedWriteTransport) Write(value []byte) (int, error) {
	select {
	case transport.writeStarted <- struct{}{}:
	default:
	}
	return transport.Conn.Write(value)
}

func writeTestRPC(writer io.Writer, message rpcMessage) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	frame := append([]byte("Content-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"), body...)
	for len(frame) > 0 {
		written, writeErr := writer.Write(frame)
		if writeErr != nil {
			return writeErr
		}
		frame = frame[written:]
	}
	return nil
}
