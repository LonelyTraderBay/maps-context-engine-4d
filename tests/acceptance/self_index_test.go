package acceptance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/overview"
	"github.com/LonelyTraderBay/maps-context-engine-4d/tools/bench/selfindex"
)

const selfIndexRequestEnv = "MCE4D_SELF_INDEX_REQUEST"

type selfIndexWorkerRequest struct {
	Mode                   string `json:"mode"`
	TargetRoot             string `json:"target_root"`
	StateRoot              string `json:"state_root"`
	ResultPath             string `json:"result_path"`
	TargetRevision         string `json:"target_revision"`
	AnalyzerSourceRevision string `json:"analyzer_source_revision"`
	CandidateBinaryPath    string `json:"candidate_test_binary_path"`
	CandidateBinarySHA256  string `json:"candidate_test_binary_sha256"`
}

type selfIndexWorkerResult struct {
	FormatVersion          int                       `json:"format_version"`
	Mode                   string                    `json:"mode"`
	TargetRevision         string                    `json:"target_revision"`
	AnalyzerSourceRevision string                    `json:"analyzer_source_revision"`
	CandidateBinaryPath    string                    `json:"candidate_test_binary_path"`
	CandidateBinarySHA256  string                    `json:"candidate_test_binary_sha256"`
	StateRoot              string                    `json:"state_root"`
	TargetRoot             string                    `json:"target_root"`
	FallbackReason         string                    `json:"fallback_reason,omitempty"`
	EngineInvoked          bool                      `json:"engine_invoked"`
	Engine                 *selfindex.EngineResult   `json:"engine,omitempty"`
	Fallback               *selfindex.FallbackResult `json:"fallback,omitempty"`
}

func TestSelfIndexWorker(t *testing.T) {
	requestPath := os.Getenv(selfIndexRequestEnv)
	if requestPath == "" {
		t.Skip("worker runs only from the copied, pinned acceptance binary")
	}
	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read self-index worker request: %v", err)
	}
	var request selfIndexWorkerRequest
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		t.Fatalf("decode self-index worker request: %v", err)
	}
	if pathWithin(request.TargetRoot, request.StateRoot) || pathWithin(request.TargetRoot, request.ResultPath) || pathWithin(request.TargetRoot, request.CandidateBinaryPath) {
		t.Fatal("binary, request, result, and cache state must stay outside the target")
	}
	result := selfIndexWorkerResult{
		FormatVersion: 1, Mode: request.Mode, TargetRevision: request.TargetRevision,
		AnalyzerSourceRevision: request.AnalyzerSourceRevision,
		CandidateBinaryPath:    request.CandidateBinaryPath,
		CandidateBinarySHA256:  request.CandidateBinarySHA256,
		StateRoot:              request.StateRoot, TargetRoot: request.TargetRoot,
	}
	switch request.Mode {
	case "engine":
		engine, err := selfindex.AnalyzePinnedPackage(t.Context(), request.TargetRoot)
		if err != nil {
			t.Fatalf("analyze target with pinned engine APIs: %v", err)
		}
		result.EngineInvoked = true
		result.Engine = &engine
	case "forced-engine-failure":
		result.FallbackReason = "forced_engine_failure_fixture"
		fallback, err := selfindex.DirectToolFallback(t.Context(), request.TargetRoot)
		if err != nil {
			t.Fatalf("direct-tool fallback failed after forced engine error: %v", err)
		}
		result.Fallback = &fallback
	default:
		t.Fatalf("unsupported worker mode %q", request.Mode)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode self-index result: %v", err)
	}
	temporary := request.ResultPath + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		t.Fatalf("write self-index result: %v", err)
	}
	if err := os.Rename(temporary, request.ResultPath); err != nil {
		t.Fatalf("publish self-index result: %v", err)
	}
}

func TestSelfIndexPinnedBinaryAcceptance(t *testing.T) {
	root := selfIndexWorkspaceRoot(t)
	gitBinary, err := osexec.LookPath("git")
	if err != nil {
		t.Fatalf("Git is required for pinned self-index acceptance: %v", err)
	}
	gitBinary, err = filepath.Abs(gitBinary)
	if err != nil {
		t.Fatal(err)
	}
	targetRevision := gitOutput(t, gitBinary, root, "rev-parse", "HEAD")
	analyzerRevision := gitOutput(t, gitBinary, root, "log", "-1", "--format=%H", targetRevision, "--", "internal/analysis/golang")
	if len(targetRevision) != 40 || len(analyzerRevision) != 40 {
		t.Fatalf("pinned revisions are malformed: target=%q analyzer=%q", targetRevision, analyzerRevision)
	}

	stateRoot := t.TempDir()
	targetRoot := filepath.Join(stateRoot, "target-snapshot")
	if pathWithin(targetRoot, stateRoot) {
		t.Fatal("acceptance state root must not be nested in the target")
	}
	clonePinnedSnapshot(t, gitBinary, root, targetRoot, targetRevision)

	runningBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve running acceptance test binary: %v", err)
	}
	info, err := os.Stat(runningBinary)
	if err != nil {
		t.Fatal(err)
	}
	pinnedBinary := filepath.Join(stateRoot, "bin", "self-index-acceptance"+filepath.Ext(runningBinary))
	if err := os.MkdirAll(filepath.Dir(pinnedBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copySelfIndexBinary(runningBinary, pinnedBinary, info.Mode()); err != nil {
		t.Fatalf("pin candidate test binary: %v", err)
	}
	binaryDigest, err := sha256File(pinnedBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pinnedBinary, (info.Mode().Perm()|0o111)&^0o222); err != nil {
		t.Fatalf("make pinned binary read-only: %v", err)
	}
	if digestAfter, err := sha256File(pinnedBinary); err != nil || digestAfter != binaryDigest {
		t.Fatalf("pinned binary changed while preparing worker: before=%s after=%s err=%v", binaryDigest, digestAfter, err)
	}

	engineResult := runPinnedSelfIndexWorker(t, pinnedBinary, stateRoot, targetRoot, targetRevision, analyzerRevision, binaryDigest, "engine")
	if engineResult.Engine == nil || !engineResult.EngineInvoked || engineResult.Fallback != nil {
		t.Fatalf("engine worker did not report the intended API path: %+v", engineResult)
	}
	if engineResult.Engine.InventoryHead != targetRevision || engineResult.Engine.InventoryCoverage != "complete" || engineResult.Engine.InventoryFileCount < 1 {
		t.Fatalf("engine inventory is not bound to the target snapshot: %+v", engineResult.Engine)
	}
	if !reflect.DeepEqual(engineResult.Engine.EngineDeclarations, engineResult.Engine.OracleDeclarations) {
		t.Fatalf("engine declaration facts differ from independent parser oracle: engine=%d oracle=%d", len(engineResult.Engine.EngineDeclarations), len(engineResult.Engine.OracleDeclarations))
	}
	if !engineResult.Engine.ProviderChangeInvalidates || engineResult.Engine.SyntaxCacheKeyV1 == engineResult.Engine.SyntaxCacheKeyV2 {
		t.Fatal("provider revision change reused the syntax cache key")
	}
	if engineResult.Engine.OperationBoundary != "direct internal acceptance API; no CLI index command is claimed" {
		t.Fatalf("unexpected capability claim: %q", engineResult.Engine.OperationBoundary)
	}

	fallbackResult := runPinnedSelfIndexWorker(t, pinnedBinary, stateRoot, targetRoot, targetRevision, analyzerRevision, binaryDigest, "forced-engine-failure")
	if fallbackResult.Fallback == nil || fallbackResult.Engine != nil || fallbackResult.EngineInvoked || fallbackResult.FallbackReason != "forced_engine_failure_fixture" || fallbackResult.Fallback.EngineInvoked {
		t.Fatalf("forced engine failure did not use the direct-tool path: %+v", fallbackResult)
	}
	if fallbackResult.Fallback.Tool != "go list + go/parser" {
		t.Fatalf("fallback tool boundary changed: %q", fallbackResult.Fallback.Tool)
	}
	if !reflect.DeepEqual(fallbackResult.Fallback.OracleDeclarations, engineResult.Engine.OracleDeclarations) || !reflect.DeepEqual(fallbackResult.Fallback.Files, engineResult.Engine.Files) {
		t.Fatal("direct-tool fallback did not reproduce the independent file/declaration oracle")
	}
	t.Logf("SELF_INDEX target_revision=%s analyzer_source_revision=%s candidate_test_binary_sha256=%s inventory_files=%d package_files=%d source_bytes=%d declarations=%d provider_change_invalidates=%t direct_tool_fallback=%s cli_index_claimed=false state_outside_target=true",
		targetRevision, analyzerRevision, binaryDigest, engineResult.Engine.InventoryFileCount,
		len(engineResult.Engine.Files), engineResult.Engine.SourceBytes, len(engineResult.Engine.EngineDeclarations),
		engineResult.Engine.ProviderChangeInvalidates, fallbackResult.Fallback.Tool)
	if digestAfter, err := sha256File(pinnedBinary); err != nil || digestAfter != binaryDigest {
		t.Fatalf("candidate test binary changed during workers: before=%s after=%s err=%v", binaryDigest, digestAfter, err)
	}
	if got := gitOutput(t, gitBinary, targetRoot, "rev-parse", "HEAD"); got != targetRevision {
		t.Fatalf("target revision moved during analysis: got=%s want=%s", got, targetRevision)
	}
	if status := gitOutput(t, gitBinary, targetRoot, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("self-index acceptance mutated the target snapshot: %s", status)
	}
}

func TestSelfIndexSpecOnlyKeepsIntendedPlaneSeparate(t *testing.T) {
	document := []byte("# Planned components\nPaymentService calls RiskScorer.\n")
	digest := model.SHA256(document)
	input := overview.Input{
		Scope: overview.Scope{
			WorktreeID:   "11111111-1111-4111-8111-111111111111",
			ProfileID:    model.SHA256([]byte("spec-only-profile")),
			GenerationID: "22222222-2222-4222-8222-222222222222",
		},
		Files: []overview.SourceFile{{
			Path: "README.md", ContentHash: digest, ByteLength: int64(len(document)), Language: "markdown", Role: "document",
		}},
		Modules: []overview.ModuleInput{{
			Key: "planned-payment", Label: "PaymentService", Kind: "planned_component", Plane: "intended",
			Source: overview.SourceEvidence{Path: "README.md", ContentHash: digest, Range: model.ByteRange{StartByte: 0, EndByte: int64(len(document))}},
		}},
		TrustedDocuments: []overview.DocumentInput{{
			Title: "Planned components", TrustRef: "spec:README.md",
			Source: overview.SourceEvidence{Path: "README.md", ContentHash: digest, Range: model.ByteRange{StartByte: 0, EndByte: int64(len(document))}},
		}},
		KnownGaps: []overview.Gap{{Code: "implementation_not_observed", Count: 1}},
	}
	result, err := overview.Build(input)
	if err != nil {
		t.Fatalf("build spec-only overview fixture: %v", err)
	}
	if len(result.Modules) != 1 || result.Modules[0].Plane != "intended" || result.Modules[0].Key != "planned-payment" {
		t.Fatalf("planned component lost intended provenance: %+v", result.Modules)
	}
	if len(result.Packages) != 0 || len(result.ReferenceCatalog) != 0 || len(result.Dependencies) != 0 || len(result.EntryPoints) != 0 {
		t.Fatalf("spec-only document invented observed facts: packages=%d refs=%d dependencies=%d entry_points=%d", len(result.Packages), len(result.ReferenceCatalog), len(result.Dependencies), len(result.EntryPoints))
	}
	if !containsOverviewGap(result.Gaps, "implementation_not_observed") {
		t.Fatalf("spec-only implementation gap is missing: %+v", result.Gaps)
	}
}

func runPinnedSelfIndexWorker(t *testing.T, pinnedBinary, stateRoot, targetRoot, targetRevision, analyzerRevision, binaryDigest, mode string) selfIndexWorkerResult {
	t.Helper()
	requestPath := filepath.Join(stateRoot, mode+".request.json")
	resultPath := filepath.Join(stateRoot, mode+".result.json")
	request := selfIndexWorkerRequest{
		Mode: mode, TargetRoot: targetRoot, StateRoot: stateRoot, ResultPath: resultPath,
		TargetRevision: targetRevision, AnalyzerSourceRevision: analyzerRevision,
		CandidateBinaryPath: pinnedBinary, CandidateBinarySHA256: binaryDigest,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	command := osexec.CommandContext(ctx, pinnedBinary, "-test.run=^TestSelfIndexWorker$", "-test.v")
	command.Dir = targetRoot
	command.Env = selfIndexWorkerEnvironment(os.Environ(), requestPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned worker mode %s failed: %v\n%s", mode, err, output)
	}
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read pinned worker mode %s result: %v\n%s", mode, err, output)
	}
	var result selfIndexWorkerResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("decode pinned worker mode %s result: %v", mode, err)
	}
	if result.FormatVersion != 1 || result.Mode != mode || result.TargetRevision != targetRevision || result.AnalyzerSourceRevision != analyzerRevision || result.CandidateBinarySHA256 != binaryDigest || result.TargetRoot != targetRoot || result.StateRoot != stateRoot {
		t.Fatalf("worker metadata is not bound to the pinned inputs: %+v", result)
	}
	if pathWithin(targetRoot, requestPath) || pathWithin(targetRoot, resultPath) || pathWithin(targetRoot, pinnedBinary) {
		t.Fatal("self-index worker state crossed into the target")
	}
	return result
}

func selfIndexWorkspaceRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve acceptance source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func clonePinnedSnapshot(t *testing.T, gitBinary, sourceRoot, targetRoot, revision string) {
	t.Helper()
	command := osexec.Command(gitBinary, "-c", "core.longpaths=true", "clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", sourceRoot, targetRoot)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone immutable self-index target: %v: %s", err, bytes.TrimSpace(output))
	}
	command = osexec.Command(gitBinary, "-C", targetRoot, "-c", "core.longpaths=true", "checkout", "--quiet", "--detach", revision)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("checkout pinned self-index target %s: %v: %s", revision, err, bytes.TrimSpace(output))
	}
	if got := gitOutput(t, gitBinary, targetRoot, "rev-parse", "HEAD"); got != revision {
		t.Fatalf("target checkout mismatch: got=%s want=%s", got, revision)
	}
}

func gitOutput(t *testing.T, gitBinary, root string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	command := osexec.Command(gitBinary, commandArgs...)
	output, err := command.Output()
	if err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(exitErr.Stderr))
		}
		t.Fatalf("git %s failed: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func copySelfIndexBinary(source, destination string, sourceMode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	mode := sourceMode.Perm() | 0o500
	if err := os.WriteFile(destination, data, mode); err != nil {
		return err
	}
	return nil
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func selfIndexWorkerEnvironment(base []string, requestPath string) []string {
	result := make([]string, 0, len(base)+1)
	for _, item := range base {
		key, _, found := strings.Cut(item, "=")
		if found && strings.EqualFold(key, selfIndexRequestEnv) {
			continue
		}
		result = append(result, item)
	}
	return append(result, selfIndexRequestEnv+"="+requestPath)
}

func pathWithin(parent, candidate string) bool {
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(parentAbs, candidateAbs)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func containsOverviewGap(gaps []overview.Gap, code string) bool {
	for _, gap := range gaps {
		if gap.Code == code && gap.Count > 0 {
			return true
		}
	}
	return false
}
