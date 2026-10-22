package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
)

type Report struct {
	FormatVersion          int                    `json:"format_version"`
	Kind                   string                 `json:"kind"`
	SuiteID                string                 `json:"suite_id"`
	Synthetic              bool                   `json:"synthetic"`
	SyntheticOrReal        string                 `json:"synthetic_or_real"`
	RealRepositoryResult   string                 `json:"real_repository_result"`
	FixtureRevisions       []FixtureRevision      `json:"fixture_revisions"`
	TaskCount              int                    `json:"task_count"`
	RepoCount              int                    `json:"repo_count"`
	EngineOutputIsOracle   bool                   `json:"engine_output_is_oracle"`
	CorrectnessGate        bool                   `json:"correctness_failure_cannot_be_offset_by_context_savings"`
	RunID                  string                 `json:"run_id"`
	SuiteSHA256            string                 `json:"suite_sha256"`
	UsageSHA256            string                 `json:"usage_sha256"`
	Model                  string                 `json:"model"`
	ModelVersion           string                 `json:"model_version"`
	Reasoning              string                 `json:"reasoning"`
	PromptSHA256           string                 `json:"prompt_sha256"`
	ToolPermissionsSHA256  string                 `json:"tool_permissions_sha256"`
	BudgetSHA256           string                 `json:"budget_sha256"`
	Environment            Environment            `json:"environment"`
	HostEnvironment        Environment            `json:"host_environment"`
	ProfileAndToolVersions ProfileAndToolVersions `json:"profile_and_tool_versions"`
	Fairness               FairnessReport         `json:"fairness"`
	Coverage               CoverageReport         `json:"coverage"`
	RecallStatus           string                 `json:"recall_status"`
	TokenUsageStatus       string                 `json:"token_usage_status"`
	TokenTelemetryStatus   string                 `json:"token_telemetry_status"`
	Variants               []VariantMetrics       `json:"variants"`
	DomainMetrics          []DomainMetrics        `json:"domain_metrics,omitempty"`
	MedianAndDistribution  []CostMetric           `json:"median_and_distribution"`
	TokenSavings           *TokenSavingsReport    `json:"token_savings,omitempty"`
	Costs                  []CostMetric           `json:"costs"`
	LifetimeAmortization   []AmortizationMetric   `json:"lifetime_amortization"`
	Notes                  []string               `json:"notes"`
}

type FixtureRevision struct {
	RepositoryID string `json:"repository_id"`
	Revision     string `json:"fixture_revision"`
}

type ProfileAndToolVersions struct {
	Model                 string `json:"model"`
	ModelVersion          string `json:"model_version"`
	Reasoning             string `json:"reasoning"`
	GoVersion             string `json:"go_version,omitempty"`
	MachineProfile        string `json:"machine_profile,omitempty"`
	PromptSHA256          string `json:"prompt_sha256"`
	ToolPermissionsSHA256 string `json:"tool_permissions_sha256"`
	BudgetSHA256          string `json:"budget_sha256"`
}

type FairnessReport struct {
	Seed                       string `json:"seed"`
	RepeatCount                int    `json:"repeat_count"`
	RecommendedRepeats         int    `json:"recommended_repeats"`
	ModelAndPromptHeldConstant bool   `json:"model_prompt_tool_permission_budget_held_constant"`
	StartingRevisionPerTask    bool   `json:"starting_revision_and_dependencies_bound_to_corpus"`
	RandomizedOrderAlgorithm   string `json:"randomized_order_algorithm"`
	VerifiedShuffledTaskRuns   int    `json:"verified_shuffled_task_runs"`
	IncompleteOrderTaskRuns    int    `json:"incomplete_order_task_runs"`
	EnvironmentRecorded        bool   `json:"environment_recorded"`
	HardwareProfileComplete    bool   `json:"hardware_profile_complete"`
}

type CoverageReport struct {
	TaskCount          int `json:"task_count"`
	RepositoryCount    int `json:"repository_count"`
	ExpectedTaskRuns   int `json:"expected_task_runs"`
	ObservedTaskRuns   int `json:"observed_task_runs"`
	MissingTaskRuns    int `json:"missing_task_runs"`
	IncompleteTaskRuns int `json:"incomplete_task_runs"`
	ComparableTaskRuns int `json:"comparable_task_runs"`
	ObservedAttempts   int `json:"observed_attempts"`
	FailedAttempts     int `json:"failed_attempts"`
}

type VariantMetrics struct {
	Variant                  Variant      `json:"variant"`
	Mode                     string       `json:"mode"`
	ExpectedTaskRuns         int          `json:"expected_task_runs"`
	ObservedTaskRuns         int          `json:"observed_task_runs"`
	AcceptedTaskRuns         int          `json:"accepted_task_runs"`
	SuccessCount             int          `json:"success_count"`
	FailedTaskRuns           int          `json:"failed_task_runs"`
	FailureCount             int          `json:"failure_count"`
	FailureTaskRunRate       float64      `json:"failure_task_run_rate"`
	CompletionRate           float64      `json:"completion_rate"`
	Attempts                 int          `json:"attempts"`
	FailedAttempts           int          `json:"failed_attempts"`
	CriticalOmissions        int64        `json:"critical_omissions"`
	FalseEdits               int64        `json:"false_edits"`
	RecallNumerator          int64        `json:"recall_numerator"`
	RecallDenominator        int64        `json:"recall_denominator"`
	RecallRate               *float64     `json:"recall_rate"`
	ContextProxyBytes        int64        `json:"context_proxy_bytes"`
	ContextProxyFiles        int64        `json:"context_proxy_files"`
	ContextProxyReads        int64        `json:"context_proxy_reads"`
	InputTokens              *int64       `json:"input_tokens"`
	OutputTokens             *int64       `json:"output_tokens"`
	TotalTokens              *int64       `json:"total_tokens"`
	CachedInputTokens        *int64       `json:"cached_input_tokens"`
	UncachedInputTokens      *int64       `json:"uncached_input_tokens"`
	TokensPerAcceptedTaskRun *float64     `json:"tokens_per_accepted_task_run"`
	Proxy                    ProxyMetrics `json:"context_proxy"`
}

type DomainMetrics struct {
	Domain             string       `json:"domain"`
	Variant            Variant      `json:"variant"`
	Mode               string       `json:"mode"`
	ExpectedTaskRuns   int          `json:"expected_task_runs"`
	ObservedTaskRuns   int          `json:"observed_task_runs"`
	SuccessfulTaskRuns int          `json:"successful_task_runs"`
	FailedTaskRuns     int          `json:"failed_task_runs"`
	FailureTaskRunRate float64      `json:"failure_task_run_rate"`
	FailedAttempts     int          `json:"failed_attempts"`
	CriticalOmissions  int64        `json:"critical_omissions"`
	FalseEdits         int64        `json:"false_edits"`
	RecallNumerator    int64        `json:"recall_numerator"`
	RecallDenominator  int64        `json:"recall_denominator"`
	RecallRate         *float64     `json:"recall_rate"`
	Proxy              ProxyMetrics `json:"context_proxy"`
}

type ProxyMetrics struct {
	PayloadBytes int64 `json:"payload_bytes"`
	Lines        int64 `json:"lines"`
	Files        int64 `json:"files"`
	ReadCalls    int64 `json:"read_calls"`
}

type TokenSavingsReport struct {
	ComparedTaskRuns               int                `json:"compared_task_runs"`
	ExcludedIncompleteTaskRuns     int                `json:"excluded_incomplete_task_runs"`
	ExcludedZeroBaselineTotalTasks int                `json:"excluded_zero_baseline_total_tasks"`
	ExcludedZeroBaselineInputTasks int                `json:"excluded_zero_baseline_input_tasks"`
	MedianTotalTokenSavingsRatio   *float64           `json:"median_total_token_savings_ratio"`
	MedianInputTokenSavingsRatio   *float64           `json:"median_input_token_savings_ratio"`
	MinimumTotalTokenSavingsRatio  *float64           `json:"minimum_total_token_savings_ratio"`
	MaximumTotalTokenSavingsRatio  *float64           `json:"maximum_total_token_savings_ratio"`
	PerTaskRuns                    []TaskSavingsPoint `json:"per_task_runs"`
}

type TaskSavingsPoint struct {
	TaskID               string   `json:"task_id"`
	Repeat               int      `json:"repeat"`
	BaselineInputTokens  int64    `json:"baseline_input_tokens"`
	OptimizedInputTokens int64    `json:"optimized_input_tokens"`
	BaselineTotalTokens  int64    `json:"baseline_total_tokens"`
	OptimizedTotalTokens int64    `json:"optimized_total_tokens"`
	InputSavingsRatio    *float64 `json:"input_savings_ratio"`
	TotalSavingsRatio    *float64 `json:"total_savings_ratio"`
}

type CostMetric struct {
	Variant                     Variant `json:"variant"`
	RepositoryID                string  `json:"repository_id"`
	Stage                       string  `json:"stage"`
	CacheState                  string  `json:"cache_state"`
	Samples                     int     `json:"samples"`
	DurationMedianMS            int64   `json:"duration_median_ms"`
	DurationP95MS               int64   `json:"duration_p95_ms"`
	CPUTimeTotalMS              *int64  `json:"cpu_time_total_ms"`
	CPUTimeSamples              int     `json:"cpu_time_samples"`
	BytesHashedTotal            *int64  `json:"bytes_hashed_total"`
	BytesHashedSamples          int     `json:"bytes_hashed_samples"`
	StorageBytesMaximum         *int64  `json:"storage_bytes_maximum"`
	PeakProcessTreeBytesMaximum *int64  `json:"peak_process_tree_bytes_maximum"`
	FreshnessMedianMS           *int64  `json:"freshness_median_ms"`
	FreshnessP95MS              *int64  `json:"freshness_p95_ms"`
	OutputBytesTotal            *int64  `json:"output_bytes_total"`
	OutputFilesTotal            *int64  `json:"output_files_total"`
	OutputPendingSamples        int     `json:"output_pending_samples"`
	OutputPendingTrue           int     `json:"output_pending_true"`
}

type AmortizationMetric struct {
	Variant                         Variant  `json:"variant"`
	IndexUpdateSamples              int      `json:"index_update_samples"`
	IndexUpdateDurationTotalMS      *int64   `json:"index_update_duration_total_ms"`
	IndexUpdateCPUTimeTotalMS       *int64   `json:"index_update_cpu_time_total_ms"`
	CPUTimeSamples                  int      `json:"cpu_time_samples"`
	BytesHashedTotal                *int64   `json:"bytes_hashed_total"`
	BytesHashedSamples              int      `json:"bytes_hashed_samples"`
	MaximumStorageBytes             *int64   `json:"maximum_storage_bytes"`
	MaximumPeakProcessTreeBytes     *int64   `json:"maximum_peak_process_tree_bytes"`
	AcceptedTaskRuns                int      `json:"accepted_task_runs"`
	IndexUpdateMSPerAcceptedTaskRun *float64 `json:"index_update_ms_per_accepted_task_run"`
}

type taskVariantMetrics struct {
	metrics VariantMetrics
	input   int64
	output  int64
}

func buildReport(suite Suite, imported importedUsage, usageHash [32]byte) (Report, error) {
	validated, err := validateImportedUsage(suite, imported)
	if err != nil {
		return Report{}, err
	}
	suiteHashHex := suite.SourceSHA256
	if !validSHA256(suiteHashHex) {
		planBytes, err := canonicalJSON(suite)
		if err != nil {
			return Report{}, fmt.Errorf("encode suite identity: %w", err)
		}
		suiteHash := sha256.Sum256(planBytes)
		suiteHashHex = hex.EncodeToString(suiteHash[:])
	}
	expectedRuns := len(suite.Tasks) * imported.header.RepeatCount
	tasksByID := make(map[string]Task, len(suite.Tasks))
	totalExpectedFacts := int64(0)
	structuredFacts := false
	for _, task := range suite.Tasks {
		tasksByID[task.ID] = task
		if len(task.ExpectedFactIDs) == 0 {
			continue
		}
		structuredFacts = true
		if err := addInt64(&totalExpectedFacts, int64(len(task.ExpectedFactIDs))); err != nil {
			return Report{}, err
		}
	}
	totalExpectedFacts, err = multiplyNonNegative(totalExpectedFacts, imported.header.RepeatCount)
	if err != nil {
		return Report{}, err
	}
	allTokenTelemetry := len(imported.attempts) > 0
	anyTokenTelemetry := false
	for _, attempt := range imported.attempts {
		complete := attempt.InputTokens != nil && attempt.OutputTokens != nil && attempt.CachedInputTokens != nil
		allTokenTelemetry = allTokenTelemetry && complete
		anyTokenTelemetry = anyTokenTelemetry || complete
	}
	tokenStatus := "PASS"
	if !anyTokenTelemetry {
		tokenStatus = "NOT_RUN"
	} else if !allTokenTelemetry {
		tokenStatus = "INCOMPLETE"
	} else if validated.missingGroupCount > 0 || validated.incompleteGroupCount > 0 {
		tokenStatus = "PARTIAL_RUN"
	}

	byVariant := make(map[Variant]*taskVariantMetrics, len(variants))
	for _, variant := range variants {
		byVariant[variant] = &taskVariantMetrics{metrics: VariantMetrics{
			Variant: variant, Mode: modeFor(variant), ExpectedTaskRuns: expectedRuns,
			RecallDenominator: totalExpectedFacts,
		}}
	}
	for key, group := range validated.groups {
		for variant, observed := range group {
			metrics := byVariant[variant]
			metrics.metrics.ObservedTaskRuns++
			accepted := false
			for _, attempt := range observed.attempts {
				metrics.metrics.Attempts++
				if attempt.Status == "failed" {
					metrics.metrics.FailedAttempts++
				}
				if attempt.Accepted {
					accepted = true
				}
				if err := addInt64(&metrics.metrics.CriticalOmissions, attempt.CriticalOmissions); err != nil {
					return Report{}, err
				}
				if err := addInt64(&metrics.metrics.FalseEdits, attempt.FalseEdits); err != nil {
					return Report{}, err
				}
				if err := addInt64(&metrics.metrics.Proxy.PayloadBytes, attempt.Proxy.PayloadBytes); err != nil {
					return Report{}, err
				}
				if err := addInt64(&metrics.metrics.Proxy.Lines, attempt.Proxy.Lines); err != nil {
					return Report{}, err
				}
				if err := addInt64(&metrics.metrics.Proxy.Files, attempt.Proxy.Files); err != nil {
					return Report{}, err
				}
				if err := addInt64(&metrics.metrics.Proxy.ReadCalls, attempt.Proxy.ReadCalls); err != nil {
					return Report{}, err
				}
				if allTokenTelemetry {
					if err := addInt64(&metrics.input, *attempt.InputTokens); err != nil {
						return Report{}, err
					}
					if err := addInt64(&metrics.output, *attempt.OutputTokens); err != nil {
						return Report{}, err
					}
				}
			}
			if structuredFacts && len(observed.attempts) > 0 {
				matched, _, _ := compareFactIDs(tasksByID[key.task], observed.attempts[len(observed.attempts)-1].ReportedFactIDs)
				if err := addInt64(&metrics.metrics.RecallNumerator, matched); err != nil {
					return Report{}, err
				}
			}
			if accepted {
				metrics.metrics.AcceptedTaskRuns++
			}
		}
	}

	variantReports := make([]VariantMetrics, 0, len(variants))
	acceptedByVariant := make(map[Variant]int, len(variants))
	for _, variant := range variants {
		entry := byVariant[variant]
		entry.metrics.CompletionRate = float64(entry.metrics.AcceptedTaskRuns) / float64(expectedRuns)
		entry.metrics.FailedTaskRuns = expectedRuns - entry.metrics.AcceptedTaskRuns
		entry.metrics.SuccessCount = entry.metrics.AcceptedTaskRuns
		entry.metrics.FailureCount = entry.metrics.FailedTaskRuns
		entry.metrics.FailureTaskRunRate = float64(entry.metrics.FailedTaskRuns) / float64(expectedRuns)
		entry.metrics.ContextProxyBytes = entry.metrics.Proxy.PayloadBytes
		entry.metrics.ContextProxyFiles = entry.metrics.Proxy.Files
		entry.metrics.ContextProxyReads = entry.metrics.Proxy.ReadCalls
		if entry.metrics.RecallDenominator > 0 {
			rate := float64(entry.metrics.RecallNumerator) / float64(entry.metrics.RecallDenominator)
			entry.metrics.RecallRate = &rate
		}
		acceptedByVariant[variant] = entry.metrics.AcceptedTaskRuns
		if allTokenTelemetry {
			total, err := addValues(entry.input, entry.output)
			if err != nil {
				return Report{}, err
			}
			cached := int64(0)
			for _, group := range validated.groups {
				if observed := group[variant]; observed != nil {
					for _, attempt := range observed.attempts {
						if err := addInt64(&cached, *attempt.CachedInputTokens); err != nil {
							return Report{}, err
						}
					}
				}
			}
			uncached := entry.input - cached
			entry.metrics.InputTokens = int64Pointer(entry.input)
			entry.metrics.OutputTokens = int64Pointer(entry.output)
			entry.metrics.TotalTokens = int64Pointer(total)
			entry.metrics.CachedInputTokens = int64Pointer(cached)
			entry.metrics.UncachedInputTokens = int64Pointer(uncached)
			if entry.metrics.AcceptedTaskRuns > 0 {
				perAccepted := float64(total) / float64(entry.metrics.AcceptedTaskRuns)
				entry.metrics.TokensPerAcceptedTaskRun = &perAccepted
			}
		}
		variantReports = append(variantReports, entry.metrics)
	}

	var savings *TokenSavingsReport
	if allTokenTelemetry {
		value, err := makeSavingsReport(validated.groups)
		if err != nil {
			return Report{}, err
		}
		savings = value
	}
	costs, amortization, err := aggregateCosts(validated.uniqueCostSamples, acceptedByVariant)
	if err != nil {
		return Report{}, err
	}
	verifiedShuffleCount := validated.completeGroupCount
	fairness := FairnessReport{
		Seed: imported.header.Seed, RepeatCount: imported.header.RepeatCount,
		RecommendedRepeats:         suite.RecommendedRuns,
		ModelAndPromptHeldConstant: true, StartingRevisionPerTask: true,
		RandomizedOrderAlgorithm: "MCE4D-SPLITMIX64-FY1",
		VerifiedShuffledTaskRuns: verifiedShuffleCount,
		IncompleteOrderTaskRuns:  validated.incompleteGroupCount,
		EnvironmentRecorded:      strings.TrimSpace(imported.header.Environment.OS) != "" && strings.TrimSpace(imported.header.Environment.Filesystem) != "",
		HardwareProfileComplete: strings.TrimSpace(imported.header.Environment.MachineProfile) != "" &&
			imported.header.Environment.LogicalCPUs != nil && imported.header.Environment.MemoryBytes != nil,
	}
	coverage := CoverageReport{
		TaskCount: len(suite.Tasks), RepositoryCount: len(suite.Repositories),
		ExpectedTaskRuns: expectedRuns, ObservedTaskRuns: len(validated.groups),
		MissingTaskRuns: validated.missingGroupCount, IncompleteTaskRuns: validated.incompleteGroupCount,
		ComparableTaskRuns: validated.completeGroupCount, ObservedAttempts: len(imported.attempts),
	}
	for _, attempt := range imported.attempts {
		if attempt.Status == "failed" {
			coverage.FailedAttempts++
		}
	}
	domainMetrics, err := buildDomainMetrics(suite, validated.groups, imported.header.RepeatCount)
	if err != nil {
		return Report{}, err
	}
	recallStatus := "NOT_RUN"
	if structuredFacts && len(imported.attempts) > 0 {
		recallStatus = "MEASURED"
		for _, variant := range variantReports {
			if variant.ObservedTaskRuns != expectedRuns {
				recallStatus = "PARTIAL"
				break
			}
		}
	}
	notes := []string{
		"Token totals include every recorded attempt, including failed attempts and retries; accepted-task denominator is accepted task runs.",
		"Context proxy bytes, lines, files, and read calls are reported separately and are never converted into token estimates.",
		"Index/update CPU, elapsed time, bytes, storage, and process-tree memory are kept separate from model token usage.",
		"Variants A/B/C are reported as baseline/full/incremental; correctness failures remain separate from context-savings metrics.",
	}
	if structuredFacts {
		notes = append(notes, "Recall compares host-reported fact IDs with independent suite facts; the engine output is not the oracle. Missing task runs remain in the recall denominator.")
	}
	if suite.Synthetic {
		notes = append(notes, "This corpus is synthetic; its results do not establish production real-repository performance or correctness.")
	}
	if tokenStatus == "NOT_RUN" {
		notes = append(notes, "Host token telemetry was not supplied; token totals and token savings are NOT_RUN.")
	} else if tokenStatus == "INCOMPLETE" {
		notes = append(notes, "At least one recorded attempt lacks complete host token telemetry; token totals and token savings are withheld.")
	} else if tokenStatus == "PARTIAL_RUN" {
		notes = append(notes, "Token totals cover recorded attempts only; task-run coverage is incomplete and is shown explicitly.")
	}
	return Report{
		FormatVersion: FormatVersion, Kind: "context_benchmark_usage_report", SuiteID: suite.SuiteID,
		Synthetic: suite.Synthetic, SyntheticOrReal: sourceKind(suite.Synthetic),
		FixtureRevisions: fixtureRevisions(suite),
		TaskCount:        len(suite.Tasks), RepoCount: len(suite.Repositories),
		RealRepositoryResult: realRepositoryResult(suite.Synthetic), EngineOutputIsOracle: false, CorrectnessGate: true,
		RecallStatus: recallStatus, DomainMetrics: domainMetrics, RunID: imported.header.RunID,
		SuiteSHA256: suiteHashHex, UsageSHA256: hex.EncodeToString(usageHash[:]),
		Model: imported.header.Model, ModelVersion: imported.header.ModelVersion,
		Reasoning: imported.header.Reasoning, PromptSHA256: imported.header.PromptSHA256,
		ToolPermissionsSHA256: imported.header.ToolPermissionsSHA256, BudgetSHA256: imported.header.BudgetSHA256,
		Environment: imported.header.Environment, HostEnvironment: imported.header.Environment,
		ProfileAndToolVersions: ProfileAndToolVersions{
			Model: imported.header.Model, ModelVersion: imported.header.ModelVersion, Reasoning: imported.header.Reasoning,
			GoVersion: imported.header.Environment.GoVersion, MachineProfile: imported.header.Environment.MachineProfile,
			PromptSHA256: imported.header.PromptSHA256, ToolPermissionsSHA256: imported.header.ToolPermissionsSHA256,
			BudgetSHA256: imported.header.BudgetSHA256,
		}, Fairness: fairness, Coverage: coverage,
		TokenUsageStatus: tokenStatus, TokenTelemetryStatus: tokenStatus, Variants: variantReports, TokenSavings: savings,
		MedianAndDistribution: costs, Costs: costs, LifetimeAmortization: amortization, Notes: notes,
	}, nil
}

func fixtureRevisions(suite Suite) []FixtureRevision {
	repositories := append([]Repository(nil), suite.Repositories...)
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].ID < repositories[j].ID })
	revisions := make([]FixtureRevision, 0, len(repositories))
	for _, repository := range repositories {
		revisions = append(revisions, FixtureRevision{RepositoryID: repository.ID, Revision: repository.FixtureRevision})
	}
	return revisions
}

func modeFor(variant Variant) string {
	switch variant {
	case VariantA:
		return "baseline"
	case VariantB:
		return "full"
	case VariantC:
		return "incremental"
	default:
		return "unknown"
	}
}

func sourceKind(synthetic bool) string {
	if synthetic {
		return "synthetic"
	}
	return "real"
}

func realRepositoryResult(synthetic bool) string {
	if synthetic {
		return "NOT_RUN"
	}
	return "MEASURED"
}

func multiplyNonNegative(value int64, factor int) (int64, error) {
	if value < 0 || factor < 0 || (factor > 0 && value > math.MaxInt64/int64(factor)) {
		return 0, fmt.Errorf("metric multiplication overflow or negative value: %w", ErrInvalidUsage)
	}
	return value * int64(factor), nil
}

func buildDomainMetrics(suite Suite, groups map[taskRunKey]map[Variant]*variantRun, repeats int) ([]DomainMetrics, error) {
	tasksByID := make(map[string]Task, len(suite.Tasks))
	taskCounts := make(map[string]int)
	factCounts := make(map[string]int64)
	for _, task := range suite.Tasks {
		tasksByID[task.ID] = task
		if task.Domain == "" {
			continue
		}
		taskCounts[task.Domain]++
		factCount := factCounts[task.Domain]
		if err := addInt64(&factCount, int64(len(task.ExpectedFactIDs))); err != nil {
			return nil, err
		}
		factCounts[task.Domain] = factCount
	}
	if len(taskCounts) == 0 {
		return nil, nil
	}
	domains := make([]string, 0, len(taskCounts))
	for domain := range taskCounts {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	byDomain := make(map[string]map[Variant]*DomainMetrics, len(domains))
	for _, domain := range domains {
		byDomain[domain] = make(map[Variant]*DomainMetrics, len(variants))
		expectedTaskRuns := taskCounts[domain] * repeats
		expectedFacts, err := multiplyNonNegative(factCounts[domain], repeats)
		if err != nil {
			return nil, err
		}
		for _, variant := range variants {
			byDomain[domain][variant] = &DomainMetrics{
				Domain: domain, Variant: variant, Mode: modeFor(variant),
				ExpectedTaskRuns: expectedTaskRuns, RecallDenominator: expectedFacts,
			}
		}
	}
	for key, group := range groups {
		task := tasksByID[key.task]
		if task.Domain == "" {
			continue
		}
		for variant, observed := range group {
			metric := byDomain[task.Domain][variant]
			metric.ObservedTaskRuns++
			accepted := false
			for _, attempt := range observed.attempts {
				if attempt.Accepted {
					accepted = true
				}
				if attempt.Status == "failed" {
					metric.FailedAttempts++
				}
				if err := addInt64(&metric.CriticalOmissions, attempt.CriticalOmissions); err != nil {
					return nil, err
				}
				if err := addInt64(&metric.FalseEdits, attempt.FalseEdits); err != nil {
					return nil, err
				}
				if err := addInt64(&metric.Proxy.PayloadBytes, attempt.Proxy.PayloadBytes); err != nil {
					return nil, err
				}
				if err := addInt64(&metric.Proxy.Lines, attempt.Proxy.Lines); err != nil {
					return nil, err
				}
				if err := addInt64(&metric.Proxy.Files, attempt.Proxy.Files); err != nil {
					return nil, err
				}
				if err := addInt64(&metric.Proxy.ReadCalls, attempt.Proxy.ReadCalls); err != nil {
					return nil, err
				}
			}
			if accepted {
				metric.SuccessfulTaskRuns++
			}
			if len(observed.attempts) > 0 {
				matched, _, _ := compareFactIDs(task, observed.attempts[len(observed.attempts)-1].ReportedFactIDs)
				if err := addInt64(&metric.RecallNumerator, matched); err != nil {
					return nil, err
				}
			}
		}
	}
	result := make([]DomainMetrics, 0, len(domains)*len(variants))
	for _, domain := range domains {
		for _, variant := range variants {
			metric := byDomain[domain][variant]
			metric.FailedTaskRuns = metric.ExpectedTaskRuns - metric.SuccessfulTaskRuns
			metric.FailureTaskRunRate = float64(metric.FailedTaskRuns) / float64(metric.ExpectedTaskRuns)
			if metric.RecallDenominator > 0 {
				rate := float64(metric.RecallNumerator) / float64(metric.RecallDenominator)
				metric.RecallRate = &rate
			}
			result = append(result, *metric)
		}
	}
	return result, nil
}

func makeSavingsReport(groups map[taskRunKey]map[Variant]*variantRun) (*TokenSavingsReport, error) {
	report := &TokenSavingsReport{PerTaskRuns: []TaskSavingsPoint{}}
	keys := make([]taskRunKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].task != keys[j].task {
			return keys[i].task < keys[j].task
		}
		return keys[i].repeat < keys[j].repeat
	})
	var totalRatios, inputRatios []float64
	for _, key := range keys {
		group := groups[key]
		if len(group) != len(variants) {
			report.ExcludedIncompleteTaskRuns++
			continue
		}
		inputTotals := make(map[Variant]int64, len(variants))
		outputTotals := make(map[Variant]int64, len(variants))
		for _, variant := range variants {
			inputTotal, outputTotal := int64(0), int64(0)
			for _, attempt := range group[variant].attempts {
				if err := addInt64(&inputTotal, *attempt.InputTokens); err != nil {
					return nil, err
				}
				if err := addInt64(&outputTotal, *attempt.OutputTokens); err != nil {
					return nil, err
				}
			}
			inputTotals[variant], outputTotals[variant] = inputTotal, outputTotal
		}
		aTotal, err := addValues(inputTotals[VariantA], outputTotals[VariantA])
		if err != nil {
			return nil, err
		}
		cTotal, err := addValues(inputTotals[VariantC], outputTotals[VariantC])
		if err != nil {
			return nil, err
		}
		point := TaskSavingsPoint{
			TaskID: key.task, Repeat: key.repeat,
			BaselineInputTokens: inputTotals[VariantA], OptimizedInputTokens: inputTotals[VariantC],
			BaselineTotalTokens: aTotal, OptimizedTotalTokens: cTotal,
		}
		if aTotal == 0 {
			report.ExcludedZeroBaselineTotalTasks++
		} else {
			ratio := 1 - float64(cTotal)/float64(aTotal)
			point.TotalSavingsRatio = &ratio
			totalRatios = append(totalRatios, ratio)
		}
		if inputTotals[VariantA] == 0 {
			report.ExcludedZeroBaselineInputTasks++
		} else {
			ratio := 1 - float64(inputTotals[VariantC])/float64(inputTotals[VariantA])
			point.InputSavingsRatio = &ratio
			inputRatios = append(inputRatios, ratio)
		}
		if point.TotalSavingsRatio != nil || point.InputSavingsRatio != nil {
			report.PerTaskRuns = append(report.PerTaskRuns, point)
		}
	}
	report.ComparedTaskRuns = len(report.PerTaskRuns)
	report.MedianTotalTokenSavingsRatio = medianFloat(totalRatios)
	report.MedianInputTokenSavingsRatio = medianFloat(inputRatios)
	if len(totalRatios) > 0 {
		minimum, maximum := totalRatios[0], totalRatios[0]
		for _, value := range totalRatios[1:] {
			if value < minimum {
				minimum = value
			}
			if value > maximum {
				maximum = value
			}
		}
		report.MinimumTotalTokenSavingsRatio = &minimum
		report.MaximumTotalTokenSavingsRatio = &maximum
	}
	return report, nil
}

func medianFloat(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	median := sorted[middle]
	if len(sorted)%2 == 0 {
		median = sorted[middle-1] + (sorted[middle]-sorted[middle-1])/2
	}
	return &median
}

func int64Pointer(value int64) *int64 { return &value }

func addInt64(total *int64, value int64) error {
	if value < 0 || *total > math.MaxInt64-value {
		return fmt.Errorf("metric sum overflow or negative value: %w", ErrInvalidUsage)
	}
	*total += value
	return nil
}

func addValues(left, right int64) (int64, error) {
	total := left
	if err := addInt64(&total, right); err != nil {
		return 0, err
	}
	return total, nil
}

type costGroupKey struct {
	variant Variant
	repo    string
	stage   string
	cache   string
}

func aggregateCosts(samples map[string]CostObservation, accepted map[Variant]int) ([]CostMetric, []AmortizationMetric, error) {
	groups := make(map[costGroupKey][]CostObservation)
	for _, sample := range samples {
		key := costGroupKey{variant: sample.Variant, repo: sample.RepositoryID, stage: sample.Stage, cache: sample.CacheState}
		groups[key] = append(groups[key], sample)
	}
	keys := make([]costGroupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.variant != b.variant {
			return a.variant < b.variant
		}
		if a.repo != b.repo {
			return a.repo < b.repo
		}
		if a.stage != b.stage {
			return a.stage < b.stage
		}
		return a.cache < b.cache
	})

	metrics := make([]CostMetric, 0, len(keys))
	amortized := make(map[Variant]*AmortizationMetric, len(variants))
	for _, variant := range variants {
		amortized[variant] = &AmortizationMetric{Variant: variant, AcceptedTaskRuns: accepted[variant]}
	}
	for _, key := range keys {
		group := groups[key]
		durations := make([]int64, 0, len(group))
		freshness := make([]int64, 0, len(group))
		metric := CostMetric{Variant: key.variant, RepositoryID: key.repo, Stage: key.stage, CacheState: key.cache, Samples: len(group)}
		var cpuTotal, bytesTotal, outputBytesTotal, outputFilesTotal int64
		var cpuPresent, bytesPresent, outputBytesPresent, outputFilesPresent bool
		var storageMax, peakMax int64
		var storagePresent, peakPresent bool
		for _, sample := range group {
			durations = append(durations, sample.DurationMS)
			if sample.CPUTimeMS != nil {
				if err := addInt64(&cpuTotal, *sample.CPUTimeMS); err != nil {
					return nil, nil, err
				}
				cpuPresent = true
			}
			if sample.BytesHashed != nil {
				if err := addInt64(&bytesTotal, *sample.BytesHashed); err != nil {
					return nil, nil, err
				}
				bytesPresent = true
			}
			if sample.StorageBytes != nil && (!storagePresent || *sample.StorageBytes > storageMax) {
				storageMax, storagePresent = *sample.StorageBytes, true
			}
			if sample.PeakProcessTreeBytes != nil && (!peakPresent || *sample.PeakProcessTreeBytes > peakMax) {
				peakMax, peakPresent = *sample.PeakProcessTreeBytes, true
			}
			if sample.FreshnessMS != nil {
				freshness = append(freshness, *sample.FreshnessMS)
			}
			if sample.OutputBytes != nil {
				if err := addInt64(&outputBytesTotal, *sample.OutputBytes); err != nil {
					return nil, nil, err
				}
				outputBytesPresent = true
			}
			if sample.OutputFiles != nil {
				if err := addInt64(&outputFilesTotal, *sample.OutputFiles); err != nil {
					return nil, nil, err
				}
				outputFilesPresent = true
			}
			if sample.OutputPending != nil {
				metric.OutputPendingSamples++
				if *sample.OutputPending {
					metric.OutputPendingTrue++
				}
			}
			if strings.HasPrefix(sample.Stage, "index_") {
				amortization := amortized[sample.Variant]
				amortization.IndexUpdateSamples++
				if amortization.IndexUpdateDurationTotalMS == nil {
					amortization.IndexUpdateDurationTotalMS = int64Pointer(0)
				}
				if err := addInt64(amortization.IndexUpdateDurationTotalMS, sample.DurationMS); err != nil {
					return nil, nil, err
				}
				if sample.CPUTimeMS != nil {
					if amortization.IndexUpdateCPUTimeTotalMS == nil {
						amortization.IndexUpdateCPUTimeTotalMS = int64Pointer(0)
					}
					if err := addInt64(amortization.IndexUpdateCPUTimeTotalMS, *sample.CPUTimeMS); err != nil {
						return nil, nil, err
					}
					amortization.CPUTimeSamples++
				}
				if sample.BytesHashed != nil {
					if amortization.BytesHashedTotal == nil {
						amortization.BytesHashedTotal = int64Pointer(0)
					}
					if err := addInt64(amortization.BytesHashedTotal, *sample.BytesHashed); err != nil {
						return nil, nil, err
					}
					amortization.BytesHashedSamples++
				}
				if sample.StorageBytes != nil && (amortization.MaximumStorageBytes == nil || *sample.StorageBytes > *amortization.MaximumStorageBytes) {
					amortization.MaximumStorageBytes = int64Pointer(*sample.StorageBytes)
				}
				if sample.PeakProcessTreeBytes != nil && (amortization.MaximumPeakProcessTreeBytes == nil || *sample.PeakProcessTreeBytes > *amortization.MaximumPeakProcessTreeBytes) {
					amortization.MaximumPeakProcessTreeBytes = int64Pointer(*sample.PeakProcessTreeBytes)
				}
			}
		}
		metric.DurationMedianMS = medianInt64(durations)
		metric.DurationP95MS = percentileInt64(durations, 95)
		if cpuPresent {
			metric.CPUTimeTotalMS, metric.CPUTimeSamples = int64Pointer(cpuTotal), countCPU(group)
		}
		if bytesPresent {
			metric.BytesHashedTotal, metric.BytesHashedSamples = int64Pointer(bytesTotal), countBytesHashed(group)
		}
		if storagePresent {
			metric.StorageBytesMaximum = int64Pointer(storageMax)
		}
		if peakPresent {
			metric.PeakProcessTreeBytesMaximum = int64Pointer(peakMax)
		}
		if len(freshness) > 0 {
			metric.FreshnessMedianMS = int64Pointer(medianInt64(freshness))
			metric.FreshnessP95MS = int64Pointer(percentileInt64(freshness, 95))
		}
		if outputBytesPresent {
			metric.OutputBytesTotal = int64Pointer(outputBytesTotal)
		}
		if outputFilesPresent {
			metric.OutputFilesTotal = int64Pointer(outputFilesTotal)
		}
		metrics = append(metrics, metric)
	}

	resultAmortization := make([]AmortizationMetric, 0, len(variants))
	for _, variant := range variants {
		metric := amortized[variant]
		if metric.IndexUpdateSamples > 0 && metric.AcceptedTaskRuns > 0 {
			perTask := float64(*metric.IndexUpdateDurationTotalMS) / float64(metric.AcceptedTaskRuns)
			metric.IndexUpdateMSPerAcceptedTaskRun = &perTask
		}
		resultAmortization = append(resultAmortization, *metric)
	}
	return metrics, resultAmortization, nil
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	middle := len(sorted) / 2
	if len(sorted)%2 != 0 {
		return sorted[middle]
	}
	return sorted[middle-1] + (sorted[middle]-sorted[middle-1])/2
}

func percentileInt64(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (percentile*len(sorted)+99)/100 - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func countCPU(samples []CostObservation) int {
	count := 0
	for _, sample := range samples {
		if sample.CPUTimeMS != nil {
			count++
		}
	}
	return count
}

func countBytesHashed(samples []CostObservation) int {
	count := 0
	for _, sample := range samples {
		if sample.BytesHashed != nil {
			count++
		}
	}
	return count
}
