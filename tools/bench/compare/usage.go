package compare

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

type Variant string

const (
	VariantA Variant = "A"
	VariantB Variant = "B"
	VariantC Variant = "C"
)

var variants = []Variant{VariantA, VariantB, VariantC}

type RunHeader struct {
	Kind                  string      `json:"kind"`
	FormatVersion         int         `json:"format_version"`
	RunID                 string      `json:"run_id"`
	SuiteID               string      `json:"suite_id"`
	Seed                  string      `json:"seed"`
	RepeatCount           int         `json:"repeat_count"`
	RepeatReductionReason string      `json:"repeat_reduction_reason,omitempty"`
	Model                 string      `json:"model"`
	ModelVersion          string      `json:"model_version"`
	Reasoning             string      `json:"reasoning"`
	PromptSHA256          string      `json:"prompt_sha256"`
	ToolPermissionsSHA256 string      `json:"tool_permissions_sha256"`
	BudgetSHA256          string      `json:"budget_sha256"`
	Environment           Environment `json:"environment"`
}

type Environment struct {
	OS             string `json:"os,omitempty"`
	Filesystem     string `json:"filesystem,omitempty"`
	MachineProfile string `json:"machine_profile,omitempty"`
	GoVersion      string `json:"go_version,omitempty"`
	LogicalCPUs    *int64 `json:"logical_cpus,omitempty"`
	MemoryBytes    *int64 `json:"memory_bytes,omitempty"`
}

type UsageAttempt struct {
	Kind              string   `json:"kind"`
	TaskID            string   `json:"task_id"`
	RepositoryID      string   `json:"repository_id"`
	Repeat            int      `json:"repeat"`
	Variant           Variant  `json:"variant"`
	Attempt           int      `json:"attempt"`
	OrderIndex        int      `json:"order_index"`
	StartRevision     string   `json:"start_revision"`
	DependencySHA256  string   `json:"dependency_sha256"`
	SetupSHA256       string   `json:"setup_sha256"`
	Status            string   `json:"status"`
	Accepted          bool     `json:"accepted"`
	FailureReason     string   `json:"failure_reason,omitempty"`
	InputTokens       *int64   `json:"input_tokens,omitempty"`
	OutputTokens      *int64   `json:"output_tokens,omitempty"`
	CachedInputTokens *int64   `json:"cached_input_tokens,omitempty"`
	Proxy             ProxyUse `json:"proxy"`
	ReportedFactIDs   []string `json:"reported_fact_ids"`
	CriticalOmissions int64    `json:"critical_omissions"`
	FalseEdits        int64    `json:"false_edits"`
}

type ProxyUse struct {
	PayloadBytes int64 `json:"payload_bytes"`
	Lines        int64 `json:"lines"`
	Files        int64 `json:"files"`
	ReadCalls    int64 `json:"read_calls"`
}

type CostObservation struct {
	Kind                 string  `json:"kind"`
	SampleID             string  `json:"sample_id"`
	RepositoryID         string  `json:"repository_id"`
	Variant              Variant `json:"variant"`
	StartRevision        string  `json:"start_revision"`
	DependencySHA256     string  `json:"dependency_sha256"`
	Stage                string  `json:"stage"`
	CacheState           string  `json:"cache_state"`
	DurationMS           int64   `json:"duration_ms"`
	CPUTimeMS            *int64  `json:"cpu_time_ms,omitempty"`
	FreshnessMS          *int64  `json:"freshness_ms,omitempty"`
	BytesHashed          *int64  `json:"bytes_hashed,omitempty"`
	StorageBytes         *int64  `json:"storage_bytes,omitempty"`
	PeakProcessTreeBytes *int64  `json:"peak_process_tree_bytes,omitempty"`
	OutputBytes          *int64  `json:"output_bytes,omitempty"`
	OutputFiles          *int64  `json:"output_files,omitempty"`
	OutputPending        *bool   `json:"output_pending,omitempty"`
}

type taskRunKey struct {
	task   string
	repeat int
}

type variantRun struct {
	order    int
	attempts []UsageAttempt
}

type importedUsage struct {
	header   RunHeader
	attempts []UsageAttempt
	costs    []CostObservation
}

func ImportUsage(suite Suite, suiteRoot string, usageBytes []byte) (Report, error) {
	if err := ValidateSuite(suite, suiteRoot); err != nil {
		return Report{}, err
	}
	imported, err := parseUsageJSONL(usageBytes)
	if err != nil {
		return Report{}, err
	}
	return buildReport(suite, imported, sha256.Sum256(usageBytes))
}

func parseUsageJSONL(data []byte) (importedUsage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), MaxUsageLineBytes)
	var imported importedUsage
	lineNumber := 0
	headerSeen := false
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := rejectDuplicateJSONKeys(line); err != nil {
			return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(line, &envelope); err != nil {
			return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
		}
		var kind string
		if err := json.Unmarshal(envelope["kind"], &kind); err != nil {
			return importedUsage{}, fmt.Errorf("line %d: missing kind: %w", lineNumber, ErrInvalidUsage)
		}
		switch kind {
		case "run_header":
			if headerSeen || len(imported.attempts) > 0 || len(imported.costs) > 0 {
				return importedUsage{}, fmt.Errorf("line %d: run_header must be the first record: %w", lineNumber, ErrInvalidUsage)
			}
			if err := requireJSONFields(envelope, "kind", "format_version", "run_id", "suite_id", "seed", "repeat_count", "model", "model_version", "reasoning", "prompt_sha256", "tool_permissions_sha256", "budget_sha256"); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			if err := decodeStrictJSON(line, &imported.header); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			headerSeen = true
		case "attempt":
			if !headerSeen {
				return importedUsage{}, fmt.Errorf("line %d: run_header is missing: %w", lineNumber, ErrInvalidUsage)
			}
			if err := requireJSONFields(envelope, "kind", "task_id", "repository_id", "repeat", "variant", "attempt", "order_index", "start_revision", "dependency_sha256", "setup_sha256", "status", "accepted", "proxy", "critical_omissions", "false_edits"); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			var proxy map[string]json.RawMessage
			if err := json.Unmarshal(envelope["proxy"], &proxy); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: proxy must be an object: %w", lineNumber, ErrInvalidUsage)
			}
			if err := requireJSONFields(proxy, "payload_bytes", "lines", "files", "read_calls"); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: proxy: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			var attempt UsageAttempt
			if err := decodeStrictJSON(line, &attempt); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			imported.attempts = append(imported.attempts, attempt)
		case "cost":
			if !headerSeen {
				return importedUsage{}, fmt.Errorf("line %d: run_header is missing: %w", lineNumber, ErrInvalidUsage)
			}
			if err := requireJSONFields(envelope, "kind", "sample_id", "repository_id", "variant", "start_revision", "dependency_sha256", "stage", "cache_state", "duration_ms"); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			var cost CostObservation
			if err := decodeStrictJSON(line, &cost); err != nil {
				return importedUsage{}, fmt.Errorf("line %d: %w: %v", lineNumber, ErrInvalidUsage, err)
			}
			imported.costs = append(imported.costs, cost)
		default:
			return importedUsage{}, fmt.Errorf("line %d: unsupported record kind %q: %w", lineNumber, kind, ErrInvalidUsage)
		}
	}
	if err := scanner.Err(); err != nil {
		return importedUsage{}, fmt.Errorf("usage JSONL too large or unreadable: %w", err)
	}
	if !headerSeen || (len(imported.attempts) == 0 && len(imported.costs) == 0) {
		return importedUsage{}, fmt.Errorf("run header and at least one measurement are required: %w", ErrInvalidUsage)
	}
	return imported, nil
}

func requireJSONFields(record map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		value, ok := record[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required JSON field %q is missing or null", field)
		}
	}
	return nil
}

func validateHeader(header RunHeader, suite Suite) error {
	if header.Kind != "run_header" || header.FormatVersion != FormatVersion ||
		strings.TrimSpace(header.RunID) == "" || header.SuiteID != suite.SuiteID ||
		strings.TrimSpace(header.Seed) == "" || header.RepeatCount < 1 || header.RepeatCount > MaxRepeatCount ||
		strings.TrimSpace(header.Model) == "" || strings.TrimSpace(header.ModelVersion) == "" ||
		strings.TrimSpace(header.Reasoning) == "" || !validSHA256(header.PromptSHA256) ||
		!validSHA256(header.ToolPermissionsSHA256) || !validSHA256(header.BudgetSHA256) {
		return fmt.Errorf("run header missing or invalid required fairness identity: %w", ErrInvalidUsage)
	}
	if header.RepeatCount < RecommendedRepeats && strings.TrimSpace(header.RepeatReductionReason) == "" {
		return fmt.Errorf("repeat_count below %d requires repeat_reduction_reason: %w", RecommendedRepeats, ErrInvalidUsage)
	}
	if header.Environment.LogicalCPUs != nil && *header.Environment.LogicalCPUs < 0 {
		return fmt.Errorf("logical_cpus must be non-negative: %w", ErrInvalidUsage)
	}
	if header.Environment.MemoryBytes != nil && *header.Environment.MemoryBytes < 0 {
		return fmt.Errorf("memory_bytes must be non-negative: %w", ErrInvalidUsage)
	}
	return nil
}

func validateAttempt(attempt UsageAttempt, header RunHeader, tasks map[string]Task, repositories map[string]Repository) error {
	task, ok := tasks[attempt.TaskID]
	if !ok {
		return fmt.Errorf("unknown task %q: %w", attempt.TaskID, ErrInvalidUsage)
	}
	repo, ok := repositories[attempt.RepositoryID]
	if !ok || attempt.RepositoryID != task.RepositoryID || attempt.StartRevision != repo.FixtureRevision ||
		attempt.DependencySHA256 != repo.DependencySHA256 || attempt.SetupSHA256 != task.Setup.SHA256 {
		return fmt.Errorf("task %q starting revision/dependency/setup does not match corpus: %w", attempt.TaskID, ErrInvalidUsage)
	}
	if attempt.Repeat < 1 || attempt.Repeat > header.RepeatCount || attempt.Attempt < 1 ||
		attempt.OrderIndex < 0 || attempt.OrderIndex >= len(variants) || !containsVariant(variants, attempt.Variant) {
		return fmt.Errorf("task %q repeat/attempt/variant/order is invalid: %w", attempt.TaskID, ErrInvalidUsage)
	}
	if attempt.Kind != "attempt" || (attempt.Status != "completed" && attempt.Status != "failed") ||
		attempt.CriticalOmissions < 0 || attempt.FalseEdits < 0 ||
		!nonNegative(attempt.Proxy.PayloadBytes, attempt.Proxy.Lines, attempt.Proxy.Files, attempt.Proxy.ReadCalls) {
		return fmt.Errorf("task %q attempt status/metrics invalid: %w", attempt.TaskID, ErrInvalidUsage)
	}
	if attempt.Status == "failed" && strings.TrimSpace(attempt.FailureReason) == "" {
		return fmt.Errorf("failed attempt needs failure_reason: %w", ErrInvalidUsage)
	}
	if len(task.ExpectedFactIDs) > 0 {
		if attempt.ReportedFactIDs == nil || !validFactIDSet(attempt.ReportedFactIDs) {
			return fmt.Errorf("task %q needs a unique reported_fact_ids array: %w", attempt.TaskID, ErrInvalidUsage)
		}
		matched, missingCritical, unexpected := compareFactIDs(task, attempt.ReportedFactIDs)
		if attempt.CriticalOmissions != missingCritical || attempt.FalseEdits != unexpected {
			return fmt.Errorf("task %q oracle fact counts mismatch (matched=%d omitted=%d false=%d): %w",
				attempt.TaskID, matched, missingCritical, unexpected, ErrInvalidUsage)
		}
	} else if len(attempt.ReportedFactIDs) > 0 {
		return fmt.Errorf("task %q reports fact IDs without an independent suite oracle: %w", attempt.TaskID, ErrInvalidUsage)
	}
	if attempt.Accepted && (attempt.Status != "completed" || attempt.CriticalOmissions != 0 || attempt.FalseEdits != 0) {
		return fmt.Errorf("accepted attempt must complete with zero reported omissions and false edits: %w", ErrInvalidUsage)
	}
	if attempt.InputTokens == nil || attempt.OutputTokens == nil || attempt.CachedInputTokens == nil {
		if attempt.InputTokens != nil || attempt.OutputTokens != nil || attempt.CachedInputTokens != nil {
			return fmt.Errorf("token telemetry fields must be all present or all absent: %w", ErrInvalidUsage)
		}
	} else if !nonNegative(*attempt.InputTokens, *attempt.OutputTokens, *attempt.CachedInputTokens) ||
		*attempt.CachedInputTokens > *attempt.InputTokens {
		return fmt.Errorf("token usage invalid or cached input exceeds input total: %w", ErrInvalidUsage)
	}
	return nil
}

func compareFactIDs(task Task, reported []string) (matched, missingCritical, unexpected int64) {
	expected := make(map[string]struct{}, len(task.ExpectedFactIDs))
	for _, id := range task.ExpectedFactIDs {
		expected[id] = struct{}{}
	}
	critical := make(map[string]struct{}, len(task.CriticalFactIDs))
	for _, id := range task.CriticalFactIDs {
		critical[id] = struct{}{}
	}
	reportedSet := make(map[string]struct{}, len(reported))
	for _, id := range reported {
		reportedSet[id] = struct{}{}
		if _, ok := expected[id]; ok {
			matched++
		} else {
			unexpected++
		}
	}
	for id := range critical {
		if _, ok := reportedSet[id]; !ok {
			missingCritical++
		}
	}
	return matched, missingCritical, unexpected
}

func validateCost(cost CostObservation, header RunHeader, repositories map[string]Repository) error {
	repo, ok := repositories[cost.RepositoryID]
	if !ok || strings.TrimSpace(cost.SampleID) == "" || cost.Kind != "cost" ||
		!containsVariant(variants, cost.Variant) || cost.StartRevision != repo.FixtureRevision ||
		cost.DependencySHA256 != repo.DependencySHA256 || cost.DurationMS < 0 {
		return fmt.Errorf("cost observation identity/metrics invalid: %w", ErrInvalidUsage)
	}
	if cost.CacheState != "cold" && cost.CacheState != "warm" {
		return fmt.Errorf("cost cache_state must be cold or warm: %w", ErrInvalidUsage)
	}
	switch cost.Stage {
	case "index_full", "index_noop_update", "index_change_update":
		if cost.Stage == "index_full" && cost.CacheState != "cold" {
			return fmt.Errorf("index_full must be measured cold: %w", ErrInvalidUsage)
		}
	case "query":
		if cost.FreshnessMS == nil || *cost.FreshnessMS < 0 || cost.OutputPending == nil ||
			(cost.OutputBytes != nil && *cost.OutputBytes < 0) ||
			(cost.OutputFiles != nil && *cost.OutputFiles < 0) {
			return fmt.Errorf("query cost requires freshness and output_pending: %w", ErrInvalidUsage)
		}
	default:
		return fmt.Errorf("unknown cost stage %q: %w", cost.Stage, ErrInvalidUsage)
	}
	for _, value := range []*int64{cost.CPUTimeMS, cost.FreshnessMS, cost.BytesHashed,
		cost.StorageBytes, cost.PeakProcessTreeBytes, cost.OutputBytes, cost.OutputFiles} {
		if value != nil && *value < 0 {
			return fmt.Errorf("cost observation has negative metric: %w", ErrInvalidUsage)
		}
	}
	if cost.Stage != "query" && (cost.FreshnessMS != nil || cost.OutputPending != nil || cost.OutputBytes != nil || cost.OutputFiles != nil) {
		return fmt.Errorf("index cost must not carry query output metrics: %w", ErrInvalidUsage)
	}
	if header.RunID == "" { // Keep the header parameter part of the validated identity boundary.
		return fmt.Errorf("run id is empty: %w", ErrInvalidUsage)
	}
	return nil
}

func containsVariant(list []Variant, value Variant) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}

func nonNegative(values ...int64) bool {
	for _, value := range values {
		if value < 0 {
			return false
		}
	}
	return true
}

func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}
