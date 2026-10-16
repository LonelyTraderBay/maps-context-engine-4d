package catalog

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	policyengine "github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
)

func TestDefaultPolicyContainsVerifiedRootGitAndClosedDenyBaseline(t *testing.T) {
	digest, _ := model.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	policy, err := DefaultPolicy("C:\\work\\repo", "C:\\Program Files\\Git\\bin\\git.exe", digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.AllowedRoots) != 1 || policy.AllowedRoots[0] != "C:\\work\\repo" || len(policy.AllowedExecutables) != 1 || policy.AllowedExecutables[0].SHA256 != digest {
		t.Fatalf("default policy escaped verified root/Git identity: %#v", policy)
	}
	if policy.AllowTargetExecution || policy.AllowRuntimeDownload || len(policy.DependencyRoots) != 0 || len(policy.TrustedRulePaths) != 0 {
		t.Fatalf("default policy widened authority: %#v", policy)
	}
	if len(policy.DeniedGlobs) != 29 || policy.SecretRulesVersion != SecretRulesVersion || !slices.Equal(policy.DeniedGlobs, policyengine.BuiltinDenyGlobs()) {
		t.Fatalf("default deny baseline is incomplete: count=%d version=%s", len(policy.DeniedGlobs), policy.SecretRulesVersion)
	}
	copyOf := policyengine.BuiltinDenyGlobs()
	copyOf[0] = "**"
	if policyengine.BuiltinDenyGlobs()[0] == "**" {
		t.Fatal("mutable copy changed the built-in deny baseline")
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyDigestSortsSetFieldsButKeepsTrustedRuleOrder(t *testing.T) {
	digest, _ := model.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	left, err := DefaultPolicy("C:\\work\\repo", "C:\\git.exe", digest)
	if err != nil {
		t.Fatal(err)
	}
	right := left
	right.DeniedGlobs = append([]string(nil), left.DeniedGlobs...)
	for i, j := 0, len(right.DeniedGlobs)-1; i < j; i, j = i+1, j-1 {
		right.DeniedGlobs[i], right.DeniedGlobs[j] = right.DeniedGlobs[j], right.DeniedGlobs[i]
	}
	leftDigest, err := left.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := right.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatal("set-valued policy arrays changed digest when reordered")
	}
	left.TrustedRulePaths = []string{"rules/a.md", "rules/b.md"}
	right.TrustedRulePaths = []string{"rules/b.md", "rules/a.md"}
	leftDigest, _ = left.Digest()
	rightDigest, _ = right.Digest()
	if leftDigest == rightDigest {
		t.Fatal("trusted rule precedence order was not included in digest")
	}
}

func TestPolicyRejectsDuplicateAndMutableAuthority(t *testing.T) {
	digest, _ := model.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	policy, err := DefaultPolicy("C:\\work\\repo", "C:\\git.exe", digest)
	if err != nil {
		t.Fatal(err)
	}
	policy.AllowedRoots = []string{"C:\\work\\repo", "C:\\work\\repo"}
	if err := policy.Validate(); err == nil {
		t.Fatal("duplicate allowed root accepted")
	}
	policy, _ = DefaultPolicy("C:\\work\\repo", "C:\\git.exe", digest)
	policy.AllowTargetExecution = true
	if err := policy.Validate(); err == nil {
		t.Fatal("target execution enabled by policy")
	}
}

func TestDefaultPolicyMatchesClosedWireSchema(t *testing.T) {
	gitDigest, _ := model.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	config, err := DefaultPolicy("C:\\work\\repo", "C:\\git.exe", gitDigest)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]any{
		"worktree_id":    "22222222-2222-4222-8222-222222222222",
		"expected_epoch": 0, "policy": config,
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateRequest("CLI policy set", request); err != nil {
		t.Fatalf("default policy differs from the closed public schema: %v", err)
	}
}
