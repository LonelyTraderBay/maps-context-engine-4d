// Package telemetry writes bounded, local JSONL events with a fixed schema.
// Events intentionally have no free-form message, path, source, or query field.
package telemetry

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	maxRecordBytes = 64 << 10
	maxMapEntries  = 64
)

var (
	ErrInvalidWriter = errors.New("invalid telemetry writer")

	safeValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	safeCodePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	safeKeyPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	secretPattern    = regexp.MustCompile(`(?i)(gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|bearer[ \t]+|pack[_-]?token|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|private[_-]?key)[=:])`)
	sensitiveKey     = regexp.MustCompile(`(?i)(source|query|path|secret|token|password|payload|diagnostic|credential|auth|private|(?:^|_)(?:api_?key|key|keys)(?:_|$))`)
)

type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Metric is restricted to the names defined by S16.
type Metric string

const (
	MetricInventoryFiles            Metric = "inventory_files"
	MetricFilesExcludedByReason     Metric = "files_excluded_by_reason"
	MetricBytesHashed               Metric = "bytes_hashed"
	MetricCacheHit                  Metric = "cache_hit"
	MetricNoop                      Metric = "noop"
	MetricFilesReparsed             Metric = "files_reparsed"
	MetricSemanticRebuildMode       Metric = "semantic_rebuild_mode"
	MetricNodes                     Metric = "nodes"
	MetricEdges                     Metric = "edges"
	MetricUnresolved                Metric = "unresolved"
	MetricFreshnessDuration         Metric = "freshness_duration"
	MetricFreshnessResult           Metric = "freshness_result"
	MetricQueryLatency              Metric = "query_latency"
	MetricOutputBytes               Metric = "output_bytes"
	MetricRequiredPending           Metric = "required_pending"
	MetricCursorErrors              Metric = "cursor_errors"
	MetricLockWaitMS                Metric = "lock_wait_ms"
	MetricBusyCount                 Metric = "busy_count"
	MetricIndexDuration             Metric = "index_duration"
	MetricCancellationLatency       Metric = "cancellation_latency"
	MetricChildProcessCount         Metric = "child_process_count"
	MetricDiskBytes                 Metric = "disk_bytes"
	MetricRecoveryCount             Metric = "recovery_count"
	MetricHierarchyQueryScope       Metric = "hierarchy_query_scope"
	MetricPartitionCacheHit         Metric = "partition_cache_hit"
	MetricInvalidationClosureSize   Metric = "invalidation_closure_size"
	MetricSnapshotAnalysisCount     Metric = "snapshot_analysis_count"
	MetricLiveValidationCount       Metric = "live_validation_count"
	MetricSelfIndexAnalyzerRevision Metric = "self_index_analyzer_revision"
)

var numericMetrics = map[Metric]struct{}{
	MetricInventoryFiles: {}, MetricFilesExcludedByReason: {}, MetricBytesHashed: {},
	MetricCacheHit: {}, MetricNoop: {}, MetricFilesReparsed: {}, MetricNodes: {},
	MetricEdges: {}, MetricUnresolved: {}, MetricFreshnessDuration: {},
	MetricQueryLatency: {}, MetricOutputBytes: {}, MetricRequiredPending: {},
	MetricCursorErrors: {}, MetricLockWaitMS: {}, MetricBusyCount: {},
	MetricIndexDuration: {}, MetricCancellationLatency: {}, MetricChildProcessCount: {},
	MetricDiskBytes: {}, MetricRecoveryCount: {}, MetricPartitionCacheHit: {},
	MetricInvalidationClosureSize: {}, MetricSnapshotAnalysisCount: {},
	MetricLiveValidationCount: {},
}

var categoricalMetrics = map[Metric]struct{}{
	MetricSemanticRebuildMode: {}, MetricFreshnessResult: {},
	MetricHierarchyQueryScope: {}, MetricSelfIndexAnalyzerRevision: {},
}

// MetricSample records either a numeric value or a bounded categorical value.
// Category is accepted only for the categorical metrics listed by S16.
type MetricSample struct {
	Name     Metric `json:"name"`
	Value    *int64 `json:"value,omitempty"`
	Category string `json:"category,omitempty"`
}

func NumericMetric(name Metric, value int64) MetricSample {
	return MetricSample{Name: name, Value: &value}
}

func CategoricalMetric(name Metric, category string) MetricSample {
	return MetricSample{Name: name, Category: category}
}

// Event contains only bounded, structured fields. It deliberately excludes
// arbitrary messages and payloads, so source text, queries, and paths cannot
// be added accidentally by a caller. Identifier fields accept canonical UUIDs
// or lowercase SHA-256 digests only.
type Event struct {
	Timestamp       time.Time        `json:"timestamp"`
	Level           Level            `json:"level"`
	RequestID       string           `json:"request_id,omitempty"`
	JobID           string           `json:"job_id,omitempty"`
	WorktreeID      string           `json:"worktree_id,omitempty"`
	ProfileID       string           `json:"profile_id,omitempty"`
	GenerationID    string           `json:"generation_id,omitempty"`
	Phase           string           `json:"phase,omitempty"`
	DurationMS      int64            `json:"duration_ms,omitempty"`
	Outcome         string           `json:"outcome,omitempty"`
	ErrorCode       string           `json:"error_code,omitempty"`
	Counts          map[string]int64 `json:"counts,omitempty"`
	Budget          map[string]int64 `json:"budget,omitempty"`
	ProviderVersion string           `json:"provider_version,omitempty"`
	Metrics         []MetricSample   `json:"metrics,omitempty"`
}

// Writer serializes each JSONL record under a mutex and enforces a byte cap.
// Record never returns sink errors to the instrumented operation.
type Writer struct {
	mu          sync.Mutex
	sink        io.Writer
	stderr      io.Writer
	file        *os.File
	maxBytes    int64
	usedBytes   int64
	debug       bool
	disabled    bool
	reported    bool
	degradation string
}

// NewWriter creates a writer whose byte budget starts at zero. Use OpenFile
// when appending to an existing JSONL file so its existing size is counted.
func NewWriter(sink io.Writer, maxBytes int64, stderr io.Writer) (*Writer, error) {
	if sink == nil || maxBytes <= 0 {
		return nil, ErrInvalidWriter
	}
	return &Writer{sink: sink, stderr: stderr, maxBytes: maxBytes}, nil
}

// OpenFile opens a local append-only JSONL sink with owner-only permissions
// where supported. Existing bytes count toward maxBytes; the file is never
// truncated to make room.
func OpenFile(path string, maxBytes int64, stderr io.Writer) (*Writer, error) {
	if path == "" || maxBytes <= 0 {
		return nil, ErrInvalidWriter
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errors.New("open telemetry sink failed")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, errors.New("inspect telemetry sink failed")
	}
	writer := &Writer{sink: file, file: file, stderr: stderr, maxBytes: maxBytes, usedBytes: info.Size()}
	if info.Size() >= maxBytes {
		writer.disabled = true
		writer.degradation = "telemetry degraded: byte limit reached\n"
	}
	return writer, nil
}

// EnableDebug opts in to debug-level records. Debug records pass through the
// same schema and redaction rules as every other level.
func (writer *Writer) EnableDebug(enabled bool) {
	if writer == nil {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.debug = enabled
}

// Record appends one sanitized JSONL record. Sink saturation and write errors
// disable further records and emit one fixed, sanitized stderr notice.
func (writer *Writer) Record(event Event) {
	if writer == nil {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if event.Level == LevelDebug && !writer.debug {
		return
	}
	if writer.disabled {
		writer.reportDegradationLocked()
		return
	}

	data, err := json.Marshal(sanitizeEvent(event))
	if err != nil || len(data)+1 > maxRecordBytes {
		writer.degradeLocked("telemetry degraded: record rejected\n")
		return
	}
	line := append(data, '\n')
	var start int64 = -1
	if writer.file != nil {
		start, err = writer.file.Seek(0, io.SeekEnd)
		if err != nil {
			writer.degradeLocked("telemetry degraded: sink write failed\n")
			return
		}
		if start > writer.usedBytes {
			writer.usedBytes = start
		}
	}
	if int64(len(line)) > writer.maxBytes-writer.usedBytes {
		writer.degradeLocked("telemetry degraded: byte limit reached\n")
		return
	}

	n, writeErr := writer.sink.Write(line)
	if writeErr == nil && n != len(line) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		if writer.file != nil && start >= 0 {
			if truncateErr := writer.file.Truncate(start); truncateErr == nil {
				_, _ = writer.file.Seek(0, io.SeekEnd)
				writer.usedBytes = start
			} else if n > 0 {
				writer.usedBytes += int64(n)
			}
		} else if n > 0 {
			writer.usedBytes += int64(n)
		}
		writer.degradeLocked("telemetry degraded: sink write failed\n")
		return
	}
	writer.usedBytes += int64(n)
}

// Close closes only a file opened by OpenFile. Close failures are reported in
// sanitized form and never propagated into the instrumented operation.
func (writer *Writer) Close() {
	if writer == nil {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.file == nil {
		return
	}
	if err := writer.file.Close(); err != nil {
		writer.degradeLocked("telemetry degraded: sink close failed\n")
	}
	writer.file = nil
	writer.disabled = true
}

func (writer *Writer) degradeLocked(message string) {
	writer.disabled = true
	writer.degradation = message
	writer.reportDegradationLocked()
}

func (writer *Writer) reportDegradationLocked() {
	if writer.reported || writer.stderr == nil || writer.degradation == "" {
		return
	}
	writer.reported = true
	_, _ = io.WriteString(writer.stderr, writer.degradation)
}

func sanitizeEvent(event Event) Event {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if !validLevel(event.Level) {
		event.Level = LevelInfo
	}
	event.RequestID = safeID(event.RequestID)
	event.JobID = safeID(event.JobID)
	event.WorktreeID = safeID(event.WorktreeID)
	event.ProfileID = safeID(event.ProfileID)
	event.GenerationID = safeID(event.GenerationID)
	event.Phase = safeValue(event.Phase)
	event.Outcome = safeOutcome(event.Outcome)
	event.ErrorCode = safeCode(event.ErrorCode)
	event.ProviderVersion = safeValue(event.ProviderVersion)
	if event.DurationMS < 0 {
		event.DurationMS = 0
	}
	event.Counts = sanitizeCounts(event.Counts)
	event.Budget = sanitizeBudget(event.Budget)
	event.Metrics = sanitizeMetrics(event.Metrics)
	return event
}

func sanitizeCounts(input map[string]int64) map[string]int64 {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]int64, len(numericMetrics))
	for name := range numericMetrics {
		if value, ok := input[string(name)]; ok && value >= 0 {
			output[string(name)] = value
		}
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func sanitizeBudget(input map[string]int64) map[string]int64 {
	if len(input) == 0 || len(input) > maxMapEntries {
		return nil
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		if safeKeyPattern.MatchString(key) && !sensitiveKey.MatchString(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxMapEntries {
		keys = keys[:maxMapEntries]
	}
	output := make(map[string]int64, len(keys))
	for _, key := range keys {
		if value := input[key]; value >= 0 {
			output[key] = value
		}
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func sanitizeMetrics(input []MetricSample) []MetricSample {
	if len(input) == 0 {
		return nil
	}
	output := make([]MetricSample, 0, min(len(input), maxMapEntries))
	for index, sample := range input {
		if index == maxMapEntries {
			break
		}
		if _, ok := numericMetrics[sample.Name]; ok {
			if sample.Value == nil || *sample.Value < 0 || sample.Category != "" {
				continue
			}
			value := *sample.Value
			output = append(output, MetricSample{Name: sample.Name, Value: &value})
			continue
		}
		if _, ok := categoricalMetrics[sample.Name]; !ok || sample.Value != nil || sample.Category == "" {
			continue
		}
		category := safeValue(sample.Category)
		if category == "" {
			continue
		}
		output = append(output, MetricSample{Name: sample.Name, Category: category})
	}
	sort.Slice(output, func(i, j int) bool {
		if output[i].Name != output[j].Name {
			return output[i].Name < output[j].Name
		}
		return metricSortValue(output[i]) < metricSortValue(output[j])
	})
	if len(output) == 0 {
		return nil
	}
	return output
}

func metricSortValue(sample MetricSample) string {
	if sample.Value != nil {
		return "0:" + strconv.FormatInt(*sample.Value, 10)
	}
	return "1:" + sample.Category
}

func validLevel(level Level) bool {
	return level == LevelDebug || level == LevelInfo || level == LevelWarn || level == LevelError
}

func safeOutcome(value string) string {
	switch value {
	case "success", "error", "cancelled", "noop", "degraded", "unknown":
		return value
	default:
		return ""
	}
}

func safeCode(value string) string {
	if !safeCodePattern.MatchString(value) || secretPattern.MatchString(value) {
		return ""
	}
	return value
}

func safeValue(value string) string {
	if len(value) == 0 || len(value) > 128 || !safeValuePattern.MatchString(value) || secretPattern.MatchString(value) {
		return ""
	}
	return value
}

func safeID(value string) string {
	if len(value) != 36 && len(value) != 64 {
		return ""
	}
	if uuidPattern.MatchString(value) || hashPattern.MatchString(value) {
		return value
	}
	return ""
}
