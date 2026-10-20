package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
)

type fakeProviderProcess struct {
	response []byte
	err      error
	wait     bool
	request  ports.LanguageProviderProcessRequest
	calls    int
	deadline time.Time
}

func (fake *fakeProviderProcess) Run(ctx context.Context, request ports.LanguageProviderProcessRequest) ([]byte, error) {
	fake.calls++
	fake.request = request
	fake.deadline, _ = ctx.Deadline()
	if fake.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]byte(nil), fake.response...), fake.err
}

func testDescriptor() Descriptor {
	return Descriptor{
		ID: "typescript-v6", Version: "1.0.0", License: "Apache-2.0", Digest: model.SHA256([]byte("provider-runtime")),
		Languages:    []string{"typescript", "javascript"},
		Capabilities: []Capability{{ID: "symbols.syntax", Version: "1.0.0"}},
	}
}

func testRequest() Request {
	return Request{
		RequestID: model.UUID("123e4567-e89b-12d3-a456-426614174000"),
		Language:  "typescript",
		Profile: Profile{
			SchemaID: "typescript-profile", SchemaVersion: 1, ProfileID: model.SHA256([]byte("profile")),
			Config: json.RawMessage(`{"analysis_mode":"syntax","allow_project_plugins":false}`),
		},
		Files: []SourceFile{{Path: "src/main.ts", Source: []byte{0xff, 0x00, 'x'}}},
	}
}

func testResponse(descriptor Descriptor, requestID model.UUID) Result {
	return Result{
		SchemaVersion: SchemaVersion,
		MessageType:   "analyze_response",
		RequestID:     requestID,
		Provider:      descriptor,
		Symbols: []Symbol{{ID: "sym:main", Kind: "function", Name: "main", Path: "src/main.ts",
			Range: ByteRange{StartByte: 0, EndByte: 3}, AnalysisStatus: AnalysisSyntax}},
		Relations:   []Relation{},
		Diagnostics: []Diagnostic{},
	}
}

func frameResult(t *testing.T, result Result) []byte {
	t.Helper()
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := EncodeFrame(payload)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestProcessAdapterUsesFixedCommandAndSourceOnlyFramedRequest(t *testing.T) {
	descriptor := testDescriptor()
	request := testRequest()
	response := frameResult(t, testResponse(descriptor, request.RequestID))
	runner := &fakeProviderProcess{response: response}
	adapter, err := NewProcessAdapter(descriptor, "typescript-v6-worker", time.Second, runner)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), request.Files[0].Source...)
	result, err := adapter.Analyze(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Symbols[0].Name != "main" || runner.calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, runner.calls)
	}
	if runner.request.CommandID != "typescript-v6-worker" || runner.request.ExpectedRuntimeDigest != descriptor.Digest ||
		runner.request.MaxResponseBytes != MaxProviderResponseBytes {
		t.Fatalf("process command boundary = %+v", runner.request)
	}
	if runner.deadline.IsZero() || time.Until(runner.deadline) > time.Second {
		t.Fatalf("process did not receive a bounded deadline: %s", runner.deadline)
	}
	if !bytes.Equal(request.Files[0].Source, original) {
		t.Fatal("provider request mutated captured source bytes")
	}
	body, err := DecodeFrame(runner.request.FramedRequest)
	if err != nil {
		t.Fatal(err)
	}
	var envelope requestEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Language != "typescript" || envelope.ExecutionPolicy.TargetCode != "never_execute" ||
		envelope.ExecutionPolicy.ProjectScripts != "never_execute" || envelope.ExecutionPolicy.ProjectPlugins != "never_execute" ||
		envelope.ExecutionPolicy.NotebookCells != "never_execute" || envelope.ExecutionPolicy.Network {
		t.Fatalf("provider execution policy was not closed: %+v", envelope.ExecutionPolicy)
	}
	if len(envelope.Files) != 1 || !bytes.Equal(envelope.Files[0].ContentBase64, original) ||
		envelope.Files[0].ContentSHA256 != model.SHA256(original) {
		t.Fatalf("captured source envelope mismatch: %+v", envelope.Files)
	}
}

func TestProcessAdapterRejectsUnregisteredInputAndResponseFacts(t *testing.T) {
	descriptor := testDescriptor()
	runner := &fakeProviderProcess{response: frameResult(t, testResponse(descriptor, testRequest().RequestID))}
	adapter, err := NewProcessAdapter(descriptor, "typescript-v6-worker", time.Second, runner)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.Files[0].Path = "../target.ts"
	if _, err := adapter.Analyze(context.Background(), request); !errors.Is(err, ErrInvalidProviderInput) {
		t.Fatalf("path traversal error = %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("untrusted request reached process port: %d calls", runner.calls)
	}

	bad := testResponse(descriptor, testRequest().RequestID)
	bad.Provider.License = "MIT"
	runner.response = frameResult(t, bad)
	if _, err := adapter.Analyze(context.Background(), testRequest()); !errors.Is(err, ErrInvalidProviderOutput) {
		t.Fatalf("provider identity mismatch error = %v", err)
	}
}

func TestProcessAdapterAppliesTimeoutAndRejectsUnsupportedLanguage(t *testing.T) {
	descriptor := testDescriptor()
	runner := &fakeProviderProcess{wait: true}
	adapter, err := NewProcessAdapter(descriptor, "typescript-v6-worker", 10*time.Millisecond, runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Analyze(context.Background(), testRequest()); !errors.Is(err, ErrProviderTimeout) {
		t.Fatalf("deadline error = %v", err)
	}

	request := testRequest()
	request.Language = "python"
	if _, err := adapter.Analyze(context.Background(), request); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("unsupported language error = %v", err)
	}
}

func TestDecodeFrameRejectsNoncanonicalOrTrailingData(t *testing.T) {
	requestBody, err := json.Marshal(requestEnvelope{
		SchemaVersion: SchemaVersion, MessageType: "analyze_request", RequestID: testRequest().RequestID,
		Language: "typescript", Profile: testRequest().Profile,
		Files:           []requestFile{{Path: "src/main.ts", ContentBase64: []byte("a"), ContentSHA256: model.SHA256([]byte("a"))}},
		ExecutionPolicy: executionPolicy{TargetCode: "never_execute", ProjectScripts: "never_execute", ProjectPlugins: "never_execute", NotebookCells: "never_execute"},
	})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := EncodeFrame(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFrame(valid); err != nil {
		t.Fatalf("valid frame rejected: %v", err)
	}
	separator := []byte("\r\n\r\n")
	leadingZero := append([]byte("Content-Length: 0"), []byte(strconv.Itoa(len(requestBody)))...)
	leadingZero = append(leadingZero, separator...)
	leadingZero = append(leadingZero, requestBody...)
	for name, candidate := range map[string][]byte{
		"leading zero":   leadingZero,
		"extra header":   append(append([]byte("Content-Length: 1\r\nX-Plugin: target.js"), separator...), requestBody...),
		"trailing bytes": append(append([]byte(nil), valid...), 'x'),
		"truncated body": valid[:len(valid)-1],
		"multiple frame": append(append([]byte(nil), valid...), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeFrame(candidate); err == nil {
				t.Fatal("invalid frame accepted")
			}
		})
	}
	if _, err := EncodeFrame([]byte(`{"message_type":"target_script"}`)); err == nil {
		t.Fatal("non-provider payload accepted")
	}
}

func TestValidateDescriptorRequiresVersionLicenseDigestAndCapabilities(t *testing.T) {
	valid := testDescriptor()
	if _, err := ValidateDescriptor(valid); err != nil {
		t.Fatalf("valid descriptor rejected: %v", err)
	}
	valid.License = ""
	if _, err := ValidateDescriptor(valid); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("missing license error = %v", err)
	}
}
