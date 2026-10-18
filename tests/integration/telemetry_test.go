package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/telemetry"
)

func TestTelemetrySanitizesFixedSchemaAndMetricAllowlist(t *testing.T) {
	var sink, stderr bytes.Buffer
	writer, err := telemetry.NewWriter(&sink, 16<<10, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	eventTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.FixedZone("UTC+7", 7*60*60))
	writer.Record(telemetry.Event{
		Timestamp:  eventTime,
		Level:      telemetry.LevelError,
		RequestID:  "550e8400-e29b-41d4-a716-446655440000",
		JobID:      "C:\\Users\\private.txt",
		WorktreeID: "7f9c2d17f00ba4e5efb04a4a6ec89e5ea767b3de9c1839c5a612c1d997b38d29",
		Phase:      "index",
		DurationMS: 12,
		Outcome:    "error",
		ErrorCode:  "RAW_QUERY=select-secret",
		Counts:     map[string]int64{"inventory_files": 3, "source_bytes": 99, "bytes_hashed": -1},
		Budget:     map[string]int64{"max_output_bytes": 1024, "source_path": 8},
		Metrics: []telemetry.MetricSample{
			telemetry.NumericMetric(telemetry.MetricQueryLatency, 7),
			telemetry.CategoricalMetric(telemetry.MetricFreshnessResult, "fresh"),
			telemetry.CategoricalMetric(telemetry.MetricHierarchyQueryScope, "C:\\private\\repo"),
			telemetry.CategoricalMetric(telemetry.MetricSemanticRebuildMode, "pack_token"),
		},
	})

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(sink.Bytes()), &record); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	if record["timestamp"] != "2026-01-01T20:04:05Z" {
		t.Fatalf("timestamp is not normalized to UTC: %v", record["timestamp"])
	}
	allowedFields := map[string]bool{
		"timestamp": true, "level": true, "request_id": true, "job_id": true,
		"worktree_id": true, "profile_id": true, "generation_id": true, "phase": true,
		"duration_ms": true, "outcome": true, "error_code": true, "counts": true,
		"budget": true, "provider_version": true, "metrics": true,
	}
	for field := range record {
		if !allowedFields[field] {
			t.Fatalf("unexpected event field %q: %s", field, sink.String())
		}
	}
	for _, forbidden := range []string{"private.txt", "RAW_QUERY", "select-secret", "source_bytes", "source_path", "C:\\\\private", "pack_token"} {
		if strings.Contains(sink.String(), forbidden) {
			t.Fatalf("sensitive or unapproved value %q was written: %s", forbidden, sink.String())
		}
	}
	counts, ok := record["counts"].(map[string]any)
	if !ok || len(counts) != 1 || counts["inventory_files"] != float64(3) {
		t.Fatalf("unexpected allowlisted counts: %#v", record["counts"])
	}
	metrics, ok := record["metrics"].([]any)
	if !ok || len(metrics) != 2 {
		t.Fatalf("unexpected sanitized metrics: %#v", record["metrics"])
	}
	if stderr.Len() != 0 {
		t.Fatalf("valid record degraded unexpectedly: %s", stderr.String())
	}
}

func TestTelemetryDebugIsOptInAndStillSanitized(t *testing.T) {
	var sink bytes.Buffer
	writer, err := telemetry.NewWriter(&sink, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	event := telemetry.Event{
		Level:     telemetry.LevelDebug,
		RequestID: "550e8400-e29b-41d4-a716-446655440000",
		JobID:     "ghp_abcdefghijklmnopqrstuvwxyz1234567890",
	}
	writer.Record(event)
	if sink.Len() != 0 {
		t.Fatal("debug event was written without opt-in")
	}
	writer.EnableDebug(true)
	writer.Record(event)
	if strings.Contains(sink.String(), "ghp_") {
		t.Fatalf("debug opt-in bypassed redaction: %s", sink.String())
	}
	if !strings.Contains(sink.String(), event.RequestID) {
		t.Fatalf("valid UUID identifier was dropped: %s", sink.String())
	}
	if got := countLines(sink.Bytes()); got != 1 {
		t.Fatalf("expected one opted-in debug record, got %d", got)
	}
}

func TestTelemetryByteCapDegradesOnceWithoutFailingCaller(t *testing.T) {
	event := telemetry.Event{Timestamp: time.Unix(1, 0).UTC(), Level: telemetry.LevelInfo, Outcome: "success"}
	var expected bytes.Buffer
	encoder := json.NewEncoder(&expected)
	if err := encoder.Encode(event); err != nil {
		t.Fatal(err)
	}
	var sink, stderr bytes.Buffer
	writer, err := telemetry.NewWriter(&sink, int64(expected.Len()), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(event)
	writer.Record(event)
	writer.Record(event)
	if sink.Len() != expected.Len() {
		t.Fatalf("byte cap exceeded or first record dropped: got %d bytes, want %d", sink.Len(), expected.Len())
	}
	if got := stderr.String(); got != "telemetry degraded: byte limit reached\n" {
		t.Fatalf("unexpected degradation notice: %q", got)
	}
}

func TestTelemetryPartialWriteDisablesSinkAndSanitizesFailure(t *testing.T) {
	sink := &partialErrorWriter{}
	var stderr bytes.Buffer
	writer, err := telemetry.NewWriter(sink, 4096, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(telemetry.Event{Level: telemetry.LevelError, RequestID: "550e8400-e29b-41d4-a716-446655440000"})
	writer.Record(telemetry.Event{Level: telemetry.LevelInfo, RequestID: "550e8400-e29b-41d4-a716-446655440001"})
	if sink.calls != 1 {
		t.Fatalf("failed sink should be disabled after one write, got %d calls", sink.calls)
	}
	if got := stderr.String(); got != "telemetry degraded: sink write failed\n" {
		t.Fatalf("sink error leaked or degradation was not reported: %q", got)
	}
	if strings.Contains(stderr.String(), "C:\\secret") {
		t.Fatal("sink path leaked to stderr")
	}
}

func TestTelemetryConcurrentRecordsRemainWholeJSONLines(t *testing.T) {
	const goroutines = 32
	const recordsPerGoroutine = 40
	var sink bytes.Buffer
	writer, err := telemetry.NewWriter(&sink, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < goroutines; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < recordsPerGoroutine; index++ {
				writer.Record(telemetry.Event{
					Level:     telemetry.LevelInfo,
					RequestID: fmt.Sprintf("%064x", worker*recordsPerGoroutine+index+1),
					Counts:    map[string]int64{"inventory_files": int64(index)},
					Outcome:   "success",
				})
			}
		}(worker)
	}
	wait.Wait()
	lines := bytes.Split(bytes.TrimSuffix(sink.Bytes(), []byte{'\n'}), []byte{'\n'})
	want := goroutines * recordsPerGoroutine
	if len(lines) != want {
		t.Fatalf("got %d JSONL records, want %d", len(lines), want)
	}
	for index, line := range lines {
		var event telemetry.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("record %d is not a complete JSON line: %v", index, err)
		}
	}
}

func TestTelemetryOpenFileCountsExistingBytesAndDoesNotTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	initial := []byte("{\"level\":\"info\"}\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	writer, err := telemetry.OpenFile(path, int64(len(initial)), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(telemetry.Event{Level: telemetry.LevelInfo})
	writer.Close()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, initial) {
		t.Fatalf("existing log was truncated or extended: %q", contents)
	}
	if stderr.String() != "telemetry degraded: byte limit reached\n" {
		t.Fatalf("expected one bounded-sink notice, got %q", stderr.String())
	}
}

type partialErrorWriter struct {
	calls int
}

func (writer *partialErrorWriter) Write(data []byte) (int, error) {
	writer.calls++
	return len(data) / 2, errors.New("C:\\secret\\events.jsonl: disk full")
}

var _ io.Writer = (*partialErrorWriter)(nil)

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return bytes.Count(data, []byte{'\n'})
}
