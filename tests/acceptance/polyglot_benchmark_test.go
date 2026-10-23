package acceptance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/tools/bench/compare"
)

type polyglotPlan struct {
	FormatVersion int    `json:"format_version"`
	TaskID        string `json:"task_id"`
	DeliveryID    string `json:"delivery_id"`
	TaskRevision  int    `json:"task_revision"`
	Cases         []struct {
		ID     string `json:"id"`
		Domain string `json:"domain"`
	} `json:"cases"`
	Cohort struct {
		MinimumRepositories int      `json:"minimum_repositories"`
		MinimumTasks        int      `json:"minimum_tasks"`
		Modes               []string `json:"modes"`
		Seed                int64    `json:"randomization_seed"`
		RealRepository      string   `json:"real_repository_result"`
		Repeats             int      `json:"repeats"`
		Repositories        []string `json:"repositories"`
	} `json:"cohort"`
	Oracle         string `json:"oracle"`
	SourceMode     string `json:"source_mode"`
	ReportContract struct {
		Required            []string `json:"required"`
		TokenWithoutReceipt string   `json:"token_savings_claim_without_host_receipt"`
		TokenStatus         string   `json:"token_usage_status_without_host_receipt"`
		CorrectnessGate     bool     `json:"correctness_failure_may_not_be_offset_by_context_savings"`
	} `json:"report_contract"`
}

func TestT068PolyglotCorpusBindsCasesModesAndSyntheticLimits(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	planBytes, err := os.ReadFile(filepath.Join(root, "artifacts", "preflight", "T068", "polyglot-acceptance-plan-r2.json"))
	if err != nil {
		t.Fatalf("read T068 polyglot plan: %v", err)
	}
	var plan polyglotPlan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatalf("decode T068 polyglot plan: %v", err)
	}
	if plan.FormatVersion != 1 || plan.TaskID != "T068" || plan.TaskRevision != 2 || plan.DeliveryID != "d6398163-6693-4073-88b3-bd0e54e01c6d" || len(plan.Cases) != 15 {
		t.Fatalf("unexpected T068 plan identity/case count: version=%d task=%q revision=%d delivery=%q cases=%d", plan.FormatVersion, plan.TaskID, plan.TaskRevision, plan.DeliveryID, len(plan.Cases))
	}
	for index, item := range plan.Cases {
		expectedID := fmt.Sprintf("C%03d", 55+index)
		if item.ID != expectedID {
			t.Errorf("T068 plan case %d = %q, want %q", index, item.ID, expectedID)
		}
	}
	if plan.Cohort.MinimumRepositories != 3 || plan.Cohort.MinimumTasks != 30 || plan.Cohort.Repeats != 3 || plan.Cohort.Seed != 68055 ||
		plan.Cohort.RealRepository != "NOT_RUN" || len(plan.Cohort.Repositories) != 3 ||
		len(plan.Cohort.Modes) != 3 || plan.Cohort.Modes[0] != "baseline" || plan.Cohort.Modes[1] != "full" || plan.Cohort.Modes[2] != "incremental" {
		t.Fatalf("T068 cohort does not bind the required synthetic A/B/C design: %+v", plan.Cohort)
	}
	if plan.Oracle != "independently authored expected facts and acceptance assertions; engine output is never its own oracle" ||
		plan.SourceMode != "synthetic fixtures only; no user repository was authorized for this benchmark" || plan.ReportContract.TokenWithoutReceipt != "FORBIDDEN" || plan.ReportContract.TokenStatus != "NOT_RUN" || !plan.ReportContract.CorrectnessGate {
		t.Fatalf("T068 plan weakens its oracle, source, token, or correctness contract: oracle=%q source=%q contract=%+v", plan.Oracle, plan.SourceMode, plan.ReportContract)
	}
	expectedRequired := []string{
		"fixture_revision", "synthetic_or_real", "mode", "task_count", "repo_count",
		"success_count", "failure_count", "critical_omissions", "false_edits",
		"recall_numerator", "recall_denominator", "context_proxy_bytes", "context_proxy_files",
		"context_proxy_reads", "token_usage_status", "input_tokens", "output_tokens",
		"median_and_distribution", "profile_and_tool_versions", "host_environment",
	}
	if len(plan.ReportContract.Required) != len(expectedRequired) {
		t.Fatalf("T068 report contract has %d required fields, want %d: %v", len(plan.ReportContract.Required), len(expectedRequired), plan.ReportContract.Required)
	}
	for index, field := range expectedRequired {
		if plan.ReportContract.Required[index] != field {
			t.Errorf("T068 report required field %d = %q, want %q", index, plan.ReportContract.Required[index], field)
		}
	}

	suite, err := compare.LoadSuite(filepath.Join(root, "tools", "bench", "compare", "testdata", "polyglot", "corpus.json"))
	if err != nil {
		t.Fatalf("load checked-in T068 polyglot corpus: %v", err)
	}
	if !suite.Synthetic || len(suite.Repositories) < plan.Cohort.MinimumRepositories || len(suite.Tasks) < plan.Cohort.MinimumTasks || suite.RecommendedRuns < plan.Cohort.Repeats {
		t.Fatalf("polyglot corpus does not meet the synthetic cohort floor: synthetic=%t repositories=%d tasks=%d repeats=%d", suite.Synthetic, len(suite.Repositories), len(suite.Tasks), suite.RecommendedRuns)
	}
	repositoryIDs := make(map[string]struct{}, len(suite.Repositories))
	for _, repo := range suite.Repositories {
		repositoryIDs[repo.ID] = struct{}{}
	}
	for _, repositoryID := range plan.Cohort.Repositories {
		if _, exists := repositoryIDs[repositoryID]; !exists {
			t.Errorf("planned synthetic repository %q is missing from the corpus", repositoryID)
		}
	}
	caseDomains := make(map[string]string, len(plan.Cases))
	for _, item := range plan.Cases {
		if item.ID == "" || item.Domain == "" {
			t.Errorf("T068 plan has incomplete case mapping: %+v", item)
		}
		if _, exists := caseDomains[item.ID]; exists {
			t.Errorf("T068 plan duplicates case %q", item.ID)
		}
		caseDomains[item.ID] = item.Domain
	}
	taskCounts := make(map[string]int, len(caseDomains))
	for _, task := range suite.Tasks {
		domain, exists := caseDomains[task.CaseID]
		if !exists || domain != task.Domain {
			t.Errorf("task %q case/domain %q/%q is not bound by the T068 plan", task.ID, task.CaseID, task.Domain)
		}
		taskCounts[task.CaseID]++
		if task.Oracle == "" || len(task.ExpectedFactIDs) == 0 || len(task.CriticalFactIDs) == 0 {
			t.Errorf("task %q lacks independent oracle facts", task.ID)
		}
	}
	for caseID := range caseDomains {
		if taskCounts[caseID] == 0 {
			t.Errorf("planned case %s has no corpus task", caseID)
		}
	}
}
