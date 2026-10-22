package compare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSuiteValidatesSyntheticCorpus(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Tasks) != 30 || len(suite.Repositories) != 3 {
		t.Fatalf("corpus size = %d tasks, %d repositories; want 30 and 3", len(suite.Tasks), len(suite.Repositories))
	}
	if !validSHA256(suite.SourceSHA256) {
		t.Fatalf("source SHA is invalid: %q", suite.SourceSHA256)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(normalizeLineEndings(raw))
	if suite.SourceSHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("source SHA does not match normalized corpus bytes: %s", suite.SourceSHA256)
	}
	if suite.RecommendedRuns < RecommendedRepeats {
		t.Fatalf("recommended repeats = %d; want at least %d", suite.RecommendedRuns, RecommendedRepeats)
	}
}

func TestNormalizeLineEndingsKeepsBinaryBytes(t *testing.T) {
	if got := string(normalizeLineEndings([]byte("module test\r\n"))); got != "module test\n" {
		t.Fatalf("normalized text = %q", got)
	}
	binary := []byte{0, '\r', '\n', 1}
	if got := normalizeLineEndings(binary); !bytes.Equal(got, binary) {
		t.Fatalf("binary content changed: %v", got)
	}
}

func TestImportUsageAggregatesRetriesAndSeparateCosts(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	usage := completeUsage(t, suite, true, true)
	report, err := ImportUsage(suite, filepath.Join("testdata"), usage)
	if err != nil {
		t.Fatal(err)
	}
	if report.SuiteSHA256 != suite.SourceSHA256 {
		t.Fatalf("suite SHA = %s, want raw source SHA %s", report.SuiteSHA256, suite.SourceSHA256)
	}
	if report.Coverage.ExpectedTaskRuns != 90 || report.Coverage.ComparableTaskRuns != 90 || report.Coverage.FailedAttempts != 1 {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
	if report.TokenTelemetryStatus != "PASS" || report.TokenSavings == nil || report.TokenSavings.ComparedTaskRuns != 90 {
		t.Fatalf("token report status=%s savings=%+v", report.TokenTelemetryStatus, report.TokenSavings)
	}
	if len(report.Variants) != 3 || report.Variants[0].Attempts != 91 || report.Variants[0].FailedAttempts != 1 {
		t.Fatalf("A metrics should include failed attempt and retry: %+v", report.Variants)
	}
	if got, want := *report.Variants[0].InputTokens, int64(9010); got != want {
		t.Fatalf("A input tokens = %d, want %d (including failed attempt)", got, want)
	}
	for _, expected := range []struct {
		variant Variant
		cached  int64
	}{
		{variant: VariantA, cached: 1801},
		{variant: VariantB, cached: 1620},
		{variant: VariantC, cached: 1260},
	} {
		var metric *VariantMetrics
		for index := range report.Variants {
			if report.Variants[index].Variant == expected.variant {
				metric = &report.Variants[index]
				break
			}
		}
		if metric == nil || metric.CachedInputTokens == nil || *metric.CachedInputTokens != expected.cached {
			var got any
			if metric != nil {
				got = metric.CachedInputTokens
			}
			t.Fatalf("%s cached input tokens = %v, want %d", expected.variant, got, expected.cached)
		}
	}
	if report.Variants[0].Proxy.PayloadBytes != 9100 {
		t.Fatalf("A proxy bytes = %d, want 9100", report.Variants[0].Proxy.PayloadBytes)
	}
	if len(report.Costs) != 6 {
		t.Fatalf("cost groups = %d, want 6", len(report.Costs))
	}
	var indexCost, queryCost *CostMetric
	for index := range report.Costs {
		metric := &report.Costs[index]
		if metric.Variant == VariantA && metric.RepositoryID == "alpha" && metric.Stage == "index_full" {
			indexCost = metric
		}
		if metric.Variant == VariantA && metric.RepositoryID == "alpha" && metric.Stage == "query" && metric.CacheState == "warm" {
			queryCost = metric
		}
	}
	if indexCost == nil || indexCost.DurationMedianMS != 150 || indexCost.DurationP95MS != 200 || *indexCost.CPUTimeTotalMS != 50 || *indexCost.BytesHashedTotal != 300 {
		t.Fatalf("index cost aggregation = %+v", indexCost)
	}
	if queryCost == nil || *queryCost.FreshnessMedianMS != 7 || queryCost.OutputPendingTrue != 1 || *queryCost.OutputBytesTotal != 64 {
		t.Fatalf("query cost aggregation = %+v", queryCost)
	}
	if len(report.LifetimeAmortization) != 3 || report.LifetimeAmortization[0].IndexUpdateSamples != 2 ||
		*report.LifetimeAmortization[0].IndexUpdateDurationTotalMS != 300 ||
		report.LifetimeAmortization[0].AcceptedTaskRuns != 90 {
		t.Fatalf("lifetime amortization = %+v", report.LifetimeAmortization)
	}
}

func TestImportWithoutTokenTelemetryKeepsProxySeparate(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	usage := completeUsage(t, suite, false, false)
	// One group is enough to prove unavailable token data does not erase proxy measurements.
	firstLine := bytes.IndexByte(usage, '\n')
	secondLine := bytes.IndexByte(usage[firstLine+1:], '\n') + firstLine + 1
	usage = append(append([]byte(nil), usage[:secondLine+1]...), '\n')
	report, err := ImportUsage(suite, filepath.Join("testdata"), usage)
	if err != nil {
		t.Fatal(err)
	}
	if report.TokenTelemetryStatus != "NOT_RUN" || report.TokenSavings != nil {
		t.Fatalf("token status=%s savings=%+v, want NOT_RUN and no savings", report.TokenTelemetryStatus, report.TokenSavings)
	}
	if report.Variants[0].InputTokens != nil || report.Variants[0].Proxy.PayloadBytes != 100 {
		t.Fatalf("token/proxy metrics = %+v; want nil tokens and proxy bytes retained", report.Variants[0])
	}
}

func TestImportRejectsMalformedAndMismatchedRecords(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	usage := completeUsage(t, suite, true, false)
	duplicateKey := bytes.Replace(usage, []byte(`"kind":"run_header"`), []byte(`"kind":"run_header","kind":"run_header"`), 1)
	if _, err := ImportUsage(suite, filepath.Join("testdata"), duplicateKey); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate JSON key error = %v", err)
	}
	withoutProxy := bytes.Split(bytes.TrimSpace(usage), []byte{'\n'})
	var attemptFields map[string]json.RawMessage
	if err := json.Unmarshal(withoutProxy[1], &attemptFields); err != nil {
		t.Fatal(err)
	}
	delete(attemptFields, "proxy")
	withoutProxy[1], _ = json.Marshal(attemptFields)
	if _, err := ImportUsage(suite, filepath.Join("testdata"), bytes.Join(withoutProxy, []byte{'\n'})); err == nil || !strings.Contains(err.Error(), `required JSON field "proxy"`) {
		t.Fatalf("missing proxy error = %v", err)
	}
	var header RunHeader
	if err := json.Unmarshal(bytes.SplitN(usage, []byte{'\n'}, 2)[0], &header); err != nil {
		t.Fatal(err)
	}
	var attempt UsageAttempt
	lines := bytes.Split(bytes.TrimSpace(usage), []byte{'\n'})
	if err := json.Unmarshal(lines[1], &attempt); err != nil {
		t.Fatal(err)
	}
	attempt.DependencySHA256 = strings.Repeat("0", 64)
	lines[1], _ = json.Marshal(attempt)
	if _, err := ImportUsage(suite, filepath.Join("testdata"), bytes.Join(lines, []byte{'\n'})); err == nil || !strings.Contains(err.Error(), "starting revision/dependency/setup") {
		t.Fatalf("dependency mismatch error = %v", err)
	}

	brokenSuite := suite
	brokenSuite.Repositories = append([]Repository(nil), suite.Repositories...)
	brokenSuite.Repositories[0].FixtureSHA256 = strings.Repeat("0", 64)
	if err := ValidateSuite(brokenSuite, filepath.Join("testdata")); err == nil {
		t.Fatal("ValidateSuite accepted a changed fixture fingerprint")
	}
}

func TestImportWithPartialTokenTelemetryWithholdsTotals(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(completeUsage(t, suite, true, false)), []byte{'\n'})
	var attempt UsageAttempt
	if err := json.Unmarshal(lines[1], &attempt); err != nil {
		t.Fatal(err)
	}
	attempt.InputTokens, attempt.OutputTokens, attempt.CachedInputTokens = nil, nil, nil
	lines[1], err = json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ImportUsage(suite, filepath.Join("testdata"), bytes.Join(lines, []byte{'\n'}))
	if err != nil {
		t.Fatal(err)
	}
	if report.TokenTelemetryStatus != "INCOMPLETE" || report.TokenSavings != nil {
		t.Fatalf("partial telemetry status=%s savings=%+v", report.TokenTelemetryStatus, report.TokenSavings)
	}
	for _, variant := range report.Variants {
		if variant.InputTokens != nil || variant.TotalTokens != nil {
			t.Fatalf("partial telemetry leaked totals: %+v", variant)
		}
	}
}

func TestImportRejectsAcceptedRetryAndSeedOrderMismatch(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	complete := bytes.Split(bytes.TrimSpace(completeUsage(t, suite, true, false)), []byte{'\n'})
	var accepted UsageAttempt
	acceptedLine := -1
	for index := 1; index < len(complete); index++ {
		var candidate UsageAttempt
		if err := json.Unmarshal(complete[index], &candidate); err != nil {
			t.Fatal(err)
		}
		if candidate.TaskID == suite.Tasks[1].ID && candidate.Repeat == 1 && candidate.Attempt == 1 && candidate.Accepted {
			accepted, acceptedLine = candidate, index
			break
		}
	}
	if acceptedLine < 0 {
		t.Fatal("did not find accepted attempt for retry validation")
	}
	acceptedRetry := accepted
	acceptedRetry.Attempt = 2
	acceptedRetry.Status = "failed"
	acceptedRetry.Accepted = false
	acceptedRetry.FailureReason = "late retry"
	complete = append(complete, mustJSON(t, acceptedRetry))
	if _, err := ImportUsage(suite, filepath.Join("testdata"), bytes.Join(complete, []byte{'\n'})); err == nil || !strings.Contains(err.Error(), "accepted result has a later retry") {
		t.Fatalf("accepted retry error = %v", err)
	}

	wrongOrder := bytes.Split(bytes.TrimSpace(completeUsage(t, suite, true, false)), []byte{'\n'})
	var orderRecords []UsageAttempt
	for index := 1; index < len(wrongOrder); index++ {
		var attempt UsageAttempt
		if err := json.Unmarshal(wrongOrder[index], &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.TaskID == suite.Tasks[0].ID && attempt.Repeat == 1 && attempt.Attempt == 1 {
			orderRecords = append(orderRecords, attempt)
		}
	}
	if len(orderRecords) != 3 {
		t.Fatalf("first task/repeat has %d variants", len(orderRecords))
	}
	firstVariant, secondVariant := orderRecords[0].Variant, orderRecords[1].Variant
	firstOrder, secondOrder := orderRecords[0].OrderIndex, orderRecords[1].OrderIndex
	for index := 1; index < len(wrongOrder); index++ {
		var attempt UsageAttempt
		if err := json.Unmarshal(wrongOrder[index], &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.TaskID != suite.Tasks[0].ID || attempt.Repeat != 1 {
			continue
		}
		if attempt.Variant == firstVariant {
			attempt.OrderIndex = secondOrder
		} else if attempt.Variant == secondVariant {
			attempt.OrderIndex = firstOrder
		} else {
			continue
		}
		wrongOrder[index] = mustJSON(t, attempt)
	}
	if _, err := ImportUsage(suite, filepath.Join("testdata"), bytes.Join(wrongOrder, []byte{'\n'})); err == nil || !strings.Contains(err.Error(), "seeded shuffle") {
		t.Fatalf("seed order mismatch error = %v", err)
	}
}

func TestLoadPolyglotSuiteCoversEveryAssignedDomain(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !suite.Synthetic || suite.SuiteID != "mce4d-polyglot-benchmark-synthetic-v1" || len(suite.Repositories) != 3 || len(suite.Tasks) != 30 {
		t.Fatalf("polyglot suite identity/size = %s synthetic=%v repos=%d tasks=%d", suite.SuiteID, suite.Synthetic, len(suite.Repositories), len(suite.Tasks))
	}
	want := map[string]string{
		"C055": "typescript", "C056": "frontend-backend", "C057": "python", "C058": "python-notebook", "C059": "sql",
		"C060": "http-schema-proto", "C061": "events", "C062": "state-machine", "C063": "federation", "C064": "temporal-diff",
		"C065": "trading", "C066": "commerce", "C067": "ai-lineage", "C068": "architecture", "C069": "benchmark",
	}
	seen := make(map[string]map[string]struct{})
	languages := make(map[string]struct{})
	for _, repo := range suite.Repositories {
		for _, language := range repo.Languages {
			languages[language] = struct{}{}
		}
	}
	for _, task := range suite.Tasks {
		if want[task.CaseID] != task.Domain || len(task.ExpectedFactIDs) != 2 || len(task.CriticalFactIDs) != 1 {
			t.Fatalf("task %s has incomplete case/oracle identity: %+v", task.ID, task)
		}
		if seen[task.CaseID] == nil {
			seen[task.CaseID] = make(map[string]struct{})
		}
		seen[task.CaseID][task.ID] = struct{}{}
	}
	if len(seen) != len(want) || len(languages) < 6 {
		t.Fatalf("case/language coverage = %d/%d; want %d cases and at least 6 languages", len(seen), len(languages), len(want))
	}
	for caseID := range want {
		if len(seen[caseID]) != 2 {
			t.Fatalf("case %s has %d cohort tasks; want 2", caseID, len(seen[caseID]))
		}
	}
}

func TestPolyglotReportSeparatesModesFailuresRecallAndSyntheticResults(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := ImportUsage(suite, filepath.Join("testdata", "polyglot"), polyglotUsage(t, suite, "", "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if report.SyntheticOrReal != "synthetic" || report.RealRepositoryResult != "NOT_RUN" || report.EngineOutputIsOracle || !report.CorrectnessGate {
		t.Fatalf("source/oracle gate = synthetic=%s real=%s engineOracle=%v correctnessGate=%v", report.SyntheticOrReal, report.RealRepositoryResult, report.EngineOutputIsOracle, report.CorrectnessGate)
	}
	if report.TaskCount != 30 || report.RepoCount != 3 || len(report.FixtureRevisions) != 3 || report.FixtureRevisions[0].RepositoryID != "alpha" ||
		report.TokenUsageStatus != "NOT_RUN" || report.HostEnvironment.OS != "test" || report.ProfileAndToolVersions.ModelVersion != "not-run" ||
		len(report.MedianAndDistribution) != len(report.Costs) {
		t.Fatalf("T068 report contract metadata is incomplete: tasks=%d repos=%d fixtures=%+v tokens=%s host=%+v profile=%+v", report.TaskCount, report.RepoCount, report.FixtureRevisions, report.TokenUsageStatus, report.HostEnvironment, report.ProfileAndToolVersions)
	}
	if report.RecallStatus != "MEASURED" || report.Coverage.ExpectedTaskRuns != 90 || report.Coverage.ObservedTaskRuns != 90 || report.Coverage.FailedAttempts != 1 {
		t.Fatalf("coverage/recall status = %+v, %s", report.Coverage, report.RecallStatus)
	}
	if report.TokenTelemetryStatus != "NOT_RUN" || report.TokenSavings != nil {
		t.Fatalf("fixture without host token receipts claimed token results: status=%s savings=%+v", report.TokenTelemetryStatus, report.TokenSavings)
	}
	wantModes := []string{"baseline", "full", "incremental"}
	wantRecall := []int64{180, 178, 179}
	wantAccepted := []int{90, 89, 88}
	for index, variant := range report.Variants {
		if variant.Mode != wantModes[index] || variant.RecallNumerator != wantRecall[index] || variant.RecallDenominator != 180 || variant.AcceptedTaskRuns != wantAccepted[index] ||
			variant.SuccessCount != variant.AcceptedTaskRuns || variant.FailureCount != variant.FailedTaskRuns ||
			variant.ContextProxyBytes != 9000 || variant.ContextProxyFiles != 180 || variant.ContextProxyReads != 270 {
			t.Fatalf("variant %s metrics = %+v", variant.Variant, variant)
		}
		if variant.FailedTaskRuns != 90-wantAccepted[index] || variant.FailureTaskRunRate != float64(variant.FailedTaskRuns)/90 {
			t.Fatalf("variant %s failure denominator = %+v", variant.Variant, variant)
		}
	}
	if report.Variants[2].CriticalOmissions != 1 || report.Variants[2].FalseEdits != 1 {
		t.Fatalf("incremental errors = omissions %d, false edits %d", report.Variants[2].CriticalOmissions, report.Variants[2].FalseEdits)
	}
	if len(report.DomainMetrics) != 45 {
		t.Fatalf("domain metric rows = %d; want 15 domains × 3 modes", len(report.DomainMetrics))
	}
	for _, domain := range report.DomainMetrics {
		if domain.ExpectedTaskRuns != 6 || domain.RecallDenominator != 12 {
			t.Fatalf("domain denominator = %+v", domain)
		}
	}
	var distribution *CostMetric
	for index := range report.Costs {
		metric := &report.Costs[index]
		if metric.RepositoryID == "alpha" && metric.Variant == VariantB && metric.Stage == "index_full" {
			distribution = metric
			break
		}
	}
	if distribution == nil || distribution.Samples != 3 || distribution.DurationMedianMS != 200 || distribution.DurationP95MS != 300 {
		t.Fatalf("median/p95 report = %+v", distribution)
	}
}

func TestPolyglotMissingTaskRunRemainsInRecallDenominator(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := ImportUsage(suite, filepath.Join("testdata", "polyglot"), polyglotUsage(t, suite, "c055-a", VariantC, 1))
	if err != nil {
		t.Fatal(err)
	}
	incremental := report.Variants[2]
	if report.RecallStatus != "PARTIAL" || incremental.RecallDenominator != 180 || incremental.RecallNumerator != 177 || incremental.ObservedTaskRuns != 89 || incremental.FailedTaskRuns != 3 {
		t.Fatalf("missing run reduced its oracle denominator or disappeared from failures: status=%s metrics=%+v", report.RecallStatus, incremental)
	}
}

func TestPolyglotRecallRemainsNotRunWithoutTaskAttempts(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(polyglotUsage(t, suite, "", "", 0)), []byte{'\n'})
	records := [][]byte{lines[0]}
	for _, line := range lines[1:] {
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			t.Fatal(err)
		}
		if header.Kind == "cost" {
			records = append(records, line)
			break
		}
	}
	if len(records) != 2 {
		t.Fatal("benchmark usage fixture has no cost observation for the empty-attempt case")
	}
	report, err := ImportUsage(suite, filepath.Join("testdata", "polyglot"), bytes.Join(records, []byte{'\n'}))
	if err != nil {
		t.Fatal(err)
	}
	if report.RecallStatus != "NOT_RUN" || report.TokenTelemetryStatus != "NOT_RUN" || report.Coverage.MissingTaskRuns != 90 {
		t.Fatalf("empty benchmark import inferred measurements: recall=%s tokens=%s missing=%d", report.RecallStatus, report.TokenTelemetryStatus, report.Coverage.MissingTaskRuns)
	}
	for _, variant := range report.Variants {
		if variant.RecallNumerator != 0 || variant.RecallDenominator != 180 || variant.ObservedTaskRuns != 0 {
			t.Fatalf("empty benchmark import changed recall denominator: %+v", variant)
		}
	}
}

func TestPolyglotImportRejectsCountersThatDisagreeWithOracleFacts(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(polyglotUsage(t, suite, "", "", 0)), []byte{'\n'})
	for index := 1; index < len(lines); index++ {
		var attempt UsageAttempt
		if err := json.Unmarshal(lines[index], &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.TaskID == "c055-a" && attempt.Repeat == 1 {
			attempt.ReportedFactIDs = []string{"unexpected-fact"}
			lines[index] = mustJSON(t, attempt)
			break
		}
	}
	if _, err := ImportUsage(suite, filepath.Join("testdata", "polyglot"), bytes.Join(lines, []byte{'\n'})); err == nil || !strings.Contains(err.Error(), "oracle fact counts mismatch") {
		t.Fatalf("unverified oracle counters were accepted: %v", err)
	}
}

func TestPolyglotImportRejectsMissingOrDuplicateReportedFacts(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []struct {
		name string
		edit func(*UsageAttempt)
	}{
		{name: "missing", edit: func(attempt *UsageAttempt) { attempt.ReportedFactIDs = nil }},
		{name: "duplicate", edit: func(attempt *UsageAttempt) {
			attempt.ReportedFactIDs = []string{attempt.ReportedFactIDs[0], attempt.ReportedFactIDs[0]}
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			lines := bytes.Split(bytes.TrimSpace(polyglotUsage(t, suite, "", "", 0)), []byte{'\n'})
			changed := false
			for index := 1; index < len(lines); index++ {
				var attempt UsageAttempt
				if err := json.Unmarshal(lines[index], &attempt); err != nil {
					t.Fatal(err)
				}
				if attempt.TaskID == "c055-a" && attempt.Repeat == 1 && attempt.Variant == VariantA {
					mutate.edit(&attempt)
					lines[index] = mustJSON(t, attempt)
					changed = true
					break
				}
			}
			if !changed {
				t.Fatal("did not find target structured attempt")
			}
			if _, err := ImportUsage(suite, filepath.Join("testdata", "polyglot"), bytes.Join(lines, []byte{'\n'})); err == nil {
				t.Fatal("invalid reported_fact_ids were accepted")
			}
		})
	}
}

func polyglotUsage(t *testing.T, suite Suite, skipTask string, skipVariant Variant, skipRepeat int) []byte {
	t.Helper()
	sha := func(value string) string {
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	header := RunHeader{
		Kind: "run_header", FormatVersion: FormatVersion, RunID: "synthetic-fixture-only", SuiteID: suite.SuiteID,
		Seed: "68055", RepeatCount: 3, Model: "fixture-only", ModelVersion: "not-run", Reasoning: "not-applicable",
		PromptSHA256: sha("fixture-prompt"), ToolPermissionsSHA256: sha("fixture-permissions"), BudgetSHA256: sha("fixture-budget"),
		Environment: Environment{OS: "test", Filesystem: "temporary-fixture", MachineProfile: "deterministic-test", LogicalCPUs: int64Pointer(1), MemoryBytes: int64Pointer(1024)},
	}
	records := [][]byte{mustJSON(t, header)}
	repositories := make(map[string]Repository, len(suite.Repositories))
	for _, repo := range suite.Repositories {
		repositories[repo.ID] = repo
	}
	for repeat := 1; repeat <= header.RepeatCount; repeat++ {
		for _, task := range suite.Tasks {
			repo := repositories[task.RepositoryID]
			order := VariantOrderFor(header.Seed, task.ID, repeat)
			for orderIndex, variant := range order {
				if task.ID == skipTask && variant == skipVariant && repeat == skipRepeat {
					continue
				}
				reported := append([]string{}, task.ExpectedFactIDs...)
				status, accepted, failure := "completed", true, ""
				omissions, falseEdits := int64(0), int64(0)
				switch {
				case task.ID == "c055-a" && repeat == 1 && variant == VariantB:
					reported = make([]string, 0)
					status, accepted, failure, omissions = "failed", false, "synthetic controlled failure", 1
				case task.ID == "c055-b" && repeat == 1 && variant == VariantC:
					reported = []string{task.ExpectedFactIDs[1]}
					accepted, omissions = false, 1
				case task.ID == "c056-a" && repeat == 2 && variant == VariantC:
					reported = append(reported, "unexpected-fact")
					accepted, falseEdits = false, 1
				}
				records = append(records, mustJSON(t, UsageAttempt{
					Kind: "attempt", TaskID: task.ID, RepositoryID: task.RepositoryID, Repeat: repeat,
					Variant: variant, Attempt: 1, OrderIndex: orderIndex,
					StartRevision: repo.FixtureRevision, DependencySHA256: repo.DependencySHA256, SetupSHA256: task.Setup.SHA256,
					Status: status, Accepted: accepted, FailureReason: failure,
					Proxy: ProxyUse{PayloadBytes: 100, Lines: 5, Files: 2, ReadCalls: 3}, ReportedFactIDs: reported,
					CriticalOmissions: omissions, FalseEdits: falseEdits,
				}))
			}
		}
	}
	for _, repo := range suite.Repositories {
		for _, variant := range variants {
			for index, duration := range []int64{100, 200, 300} {
				cpu, hashed, storage, peak := int64(10+index), int64(1000+index), int64(2000+index), int64(3000+index)
				records = append(records, mustJSON(t, CostObservation{
					Kind: "cost", SampleID: repo.ID + "-" + string(variant) + "-index-" + string(rune('1'+index)),
					RepositoryID: repo.ID, Variant: variant, StartRevision: repo.FixtureRevision,
					DependencySHA256: repo.DependencySHA256, Stage: "index_full", CacheState: "cold", DurationMS: duration,
					CPUTimeMS: &cpu, BytesHashed: &hashed, StorageBytes: &storage, PeakProcessTreeBytes: &peak,
				}))
				freshness, outputBytes, outputFiles, pending := int64(4+index), int64(40+index), int64(1), false
				records = append(records, mustJSON(t, CostObservation{
					Kind: "cost", SampleID: repo.ID + "-" + string(variant) + "-query-" + string(rune('1'+index)),
					RepositoryID: repo.ID, Variant: variant, StartRevision: repo.FixtureRevision,
					DependencySHA256: repo.DependencySHA256, Stage: "query", CacheState: "warm", DurationMS: int64(12 + 4*index),
					FreshnessMS: &freshness, OutputBytes: &outputBytes, OutputFiles: &outputFiles, OutputPending: &pending,
				}))
			}
		}
	}
	return bytes.Join(records, []byte{'\n'})
}

func completeUsage(t *testing.T, suite Suite, tokens, costs bool) []byte {
	t.Helper()
	prompt := sha256.Sum256([]byte("fixed prompt"))
	permissions := sha256.Sum256([]byte("fixed tool permissions"))
	budget := sha256.Sum256([]byte("fixed budget"))
	header := RunHeader{
		Kind: "run_header", FormatVersion: FormatVersion, RunID: "test-run", SuiteID: suite.SuiteID,
		Seed: "seed-123", RepeatCount: 3, Model: "host-model", ModelVersion: "host-model-version", Reasoning: "medium",
		PromptSHA256: hex.EncodeToString(prompt[:]), ToolPermissionsSHA256: hex.EncodeToString(permissions[:]),
		BudgetSHA256: hex.EncodeToString(budget[:]), Environment: Environment{OS: "test", Filesystem: "temporary"},
	}
	records := [][]byte{mustJSON(t, header)}
	repositories := make(map[string]Repository, len(suite.Repositories))
	for _, repo := range suite.Repositories {
		repositories[repo.ID] = repo
	}
	for repeat := 1; repeat <= header.RepeatCount; repeat++ {
		for _, task := range suite.Tasks {
			repo := repositories[task.RepositoryID]
			order := VariantOrderFor(header.Seed, task.ID, repeat)
			for orderIndex, variant := range order {
				attemptCount := 1
				if task.ID == suite.Tasks[0].ID && repeat == 1 && variant == VariantA {
					attemptCount = 2
				}
				for attemptNumber := 1; attemptNumber <= attemptCount; attemptNumber++ {
					input := int64(100)
					if variant == VariantB {
						input = 90
					} else if variant == VariantC {
						input = 70
					}
					if attemptNumber == 1 && attemptCount == 2 {
						input = 10
					}
					output, cached := int64(10), int64(input/5)
					status, accepted, failure := "completed", true, ""
					if attemptNumber == 1 && attemptCount == 2 {
						status, accepted, failure, output, cached = "failed", false, "controlled retry fixture", 2, 1
					}
					observation := UsageAttempt{
						Kind: "attempt", TaskID: task.ID, RepositoryID: task.RepositoryID, Repeat: repeat,
						Variant: variant, Attempt: attemptNumber, OrderIndex: orderIndex, StartRevision: repo.FixtureRevision,
						DependencySHA256: repo.DependencySHA256, SetupSHA256: task.Setup.SHA256,
						Status: status, Accepted: accepted, FailureReason: failure,
						Proxy: ProxyUse{PayloadBytes: 100, Lines: 2, Files: 1, ReadCalls: 1},
					}
					if tokens {
						observation.InputTokens, observation.OutputTokens, observation.CachedInputTokens = &input, &output, &cached
					}
					records = append(records, mustJSON(t, observation))
				}
			}
		}
	}
	if costs {
		repo := repositories["alpha"]
		for _, variant := range variants {
			for index, duration := range []int64{100, 200} {
				cpu, hashed, storage, peak := int64(20+index*10), int64(100+index*100), int64(1000+index*100), int64(2000+index*100)
				records = append(records, mustJSON(t, CostObservation{
					Kind: "cost", SampleID: string(variant) + "-index-" + string(rune('1'+index)), RepositoryID: "alpha",
					Variant: variant, StartRevision: repo.FixtureRevision, DependencySHA256: repo.DependencySHA256,
					Stage: "index_full", CacheState: "cold", DurationMS: duration,
					CPUTimeMS: &cpu, BytesHashed: &hashed, StorageBytes: &storage, PeakProcessTreeBytes: &peak,
				}))
			}
			freshness, outputBytes, outputFiles, pending := int64(7), int64(64), int64(2), true
			records = append(records, mustJSON(t, CostObservation{
				Kind: "cost", SampleID: string(variant) + "-query", RepositoryID: "alpha", Variant: variant,
				StartRevision: repo.FixtureRevision, DependencySHA256: repo.DependencySHA256,
				Stage: "query", CacheState: "warm", DurationMS: 15, FreshnessMS: &freshness,
				OutputBytes: &outputBytes, OutputFiles: &outputFiles, OutputPending: &pending,
			}))
		}
	}
	return bytes.Join(records, []byte{'\n'})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUsageFixtureIsDeterministic(t *testing.T) {
	suite, err := LoadSuite(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := completeUsage(t, suite, true, false)
	second := completeUsage(t, suite, true, false)
	if !bytes.Equal(first, second) {
		t.Fatal("generated usage fixture is not deterministic")
	}
	if _, err := os.Stat(filepath.Join("testdata", "corpus.json")); err != nil {
		t.Fatal(err)
	}
}
