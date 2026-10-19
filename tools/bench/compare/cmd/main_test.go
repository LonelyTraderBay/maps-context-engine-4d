package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/tools/bench/compare"
)

func TestValidateCommandUsesCorpus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"validate", "-suite", filepath.Join("..", "testdata", "corpus.json")}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("validate: %v (stderr %s)", err, stderr.String())
	}
	var result struct {
		Status       string `json:"status"`
		Tasks        int    `json:"tasks"`
		Repositories int    `json:"repositories"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "PASS" || result.Tasks != 30 || result.Repositories != 3 {
		t.Fatalf("unexpected command result: %+v", result)
	}
}

func TestImportCommandCreatesReportWithoutOverwrite(t *testing.T) {
	suitePath := filepath.Join("..", "testdata", "corpus.json")
	suite, err := compare.LoadSuite(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	repo := suite.Repositories[0]
	prompt, permissions, budget := digest("prompt"), digest("permissions"), digest("budget")
	header := compare.RunHeader{
		Kind: "run_header", FormatVersion: compare.FormatVersion, RunID: "cli-test", SuiteID: suite.SuiteID,
		Seed: "cli-seed", RepeatCount: 1, RepeatReductionReason: "small command contract test",
		Model: "test-model", ModelVersion: "test-model-1", Reasoning: "low",
		PromptSHA256: prompt, ToolPermissionsSHA256: permissions, BudgetSHA256: budget,
		Environment: compare.Environment{OS: "test", Filesystem: "temporary"},
	}
	task := suite.Tasks[0]
	attempt := compare.UsageAttempt{
		Kind: "attempt", TaskID: task.ID, RepositoryID: task.RepositoryID, Repeat: 1, Variant: compare.VariantOrderFor(header.Seed, task.ID, 1)[0],
		Attempt: 1, OrderIndex: 0, StartRevision: repo.FixtureRevision, DependencySHA256: repo.DependencySHA256,
		SetupSHA256: task.Setup.SHA256, Status: "completed", Accepted: true,
		Proxy: compare.ProxyUse{PayloadBytes: 4, Lines: 1, Files: 1, ReadCalls: 1},
	}
	headerJSON, _ := json.Marshal(header)
	attemptJSON, _ := json.Marshal(attempt)
	usagePath := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(usagePath, append(append(headerJSON, '\n'), attemptJSON...), 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "report.json")
	args := []string{"import", "-suite", suitePath, "-usage", usagePath, "-out", outputPath}
	var stdout, stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("import: %v (stderr %s)", err, stderr.String())
	}
	reportBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(reportBytes, []byte(`"token_telemetry_status": "NOT_RUN"`)) {
		t.Fatalf("report does not preserve missing-token status: %s", reportBytes)
	}
	if err := run(args, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "without overwriting") {
		t.Fatalf("second import should refuse overwrite, got %v", err)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("existing report was removed: %v", err)
	}
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
