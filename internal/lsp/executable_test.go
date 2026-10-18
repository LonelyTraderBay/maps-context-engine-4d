package lsp

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestVerifyExecutableVersionRejectsWrongPinnedVersion(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go executable is unavailable")
	}
	goPath, err = filepath.Abs(goPath)
	if err != nil {
		t.Fatal(err)
	}
	pin := ExecutablePin{Path: goPath, Version: "wrong-version", SHA256: fileHash(t, goPath)}
	environment := map[string]string{
		"PATH": filepath.Dir(goPath), "GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off",
	}
	if err := verifyExecutableVersion(context.Background(), pin, t.TempDir(), environment); !errors.Is(err, ErrExecutableUnavailable) {
		t.Fatalf("wrong pinned version error=%v", err)
	}
}

func TestExecutionPolicyRejectsRuntimeExecutionAndDownloads(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go executable is unavailable")
	}
	goPath, err = filepath.Abs(goPath)
	if err != nil {
		t.Fatal(err)
	}
	policy := ExecutionPolicy{
		AllowedRoots: []string{t.TempDir()}, AllowedExecutables: []AllowedExecutable{{Path: goPath, SHA256: fileHash(t, goPath)}},
		MaxFileBytes: 2 * 1024 * 1024, MaxSourceBytes: 32 * 1024 * 1024 * 1024,
	}
	if validateExecutionPolicy(policy) != nil {
		t.Fatal("valid bounded execution policy was rejected")
	}
	policy.AllowTargetExecution = true
	profile := testGoProfile()
	profile.Go = ExecutablePin{Path: goPath, Version: "go-test", SHA256: fileHash(t, goPath)}
	profile.GoVersion = profile.Go.Version
	profile.GoBinarySHA256 = profile.Go.SHA256
	if _, _, err := validateGoProfile(profile, policy, policy.AllowedRoots[0]); err == nil {
		t.Fatal("policy allowing target execution was accepted")
	}
}

func testGoProfile() GoProfile {
	profile := GoProfile{
		GOOS: "windows", GOARCH: "amd64", TestsEnabled: true, ModuleRoots: []string{"."}, DependencyRoots: []string{},
		ToolchainDigest: model.SHA256([]byte("test toolchain")), ProviderDigest: model.SHA256([]byte("test provider")),
		ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoVersion: "go-test",
		GoBinarySHA256: model.SHA256([]byte("test go binary")),
		MaxFileBytes:   2 * 1024 * 1024, MaxSourceBytes: 32 * 1024 * 1024 * 1024,
	}
	profile.Go = ExecutablePin{Version: profile.GoVersion, SHA256: profile.GoBinarySHA256}
	profile.Gopls = ExecutablePin{Version: pinnedGoplsVersion, SHA256: model.SHA256([]byte("test gopls binary"))}
	profile.ID, _ = effectiveGoProfileID(profile)
	return profile
}

func TestGoProfileIDBindsEffectiveFields(t *testing.T) {
	base := testGoProfile()
	changed := []func(*GoProfile){
		func(profile *GoProfile) { profile.GOOS = "linux" },
		func(profile *GoProfile) { profile.BuildTags = []string{"integration"} },
		func(profile *GoProfile) { profile.TestsEnabled = false },
		func(profile *GoProfile) { profile.ModuleRoots = []string{"nested"} },
		func(profile *GoProfile) { profile.ToolchainDigest = model.SHA256([]byte("other toolchain")) },
		func(profile *GoProfile) { profile.ProviderDigest = model.SHA256([]byte("other provider")) },
		func(profile *GoProfile) { profile.ModuleMode = "vendor_explicit" },
		func(profile *GoProfile) { profile.AnalysisMode = "syntax" },
		func(profile *GoProfile) { profile.DependencyRoots = []string{"C:/deps"} },
	}
	for index, mutate := range changed {
		profile := base
		profile.BuildTags = append([]string(nil), base.BuildTags...)
		profile.ModuleRoots = append([]string(nil), base.ModuleRoots...)
		profile.DependencyRoots = append([]string(nil), base.DependencyRoots...)
		mutate(&profile)
		profileID, err := effectiveGoProfileID(profile)
		if err != nil || profileID == base.ID {
			t.Errorf("effective profile change %d did not change its identity: id=%s err=%v", index, profileID, err)
		}
	}
}

func TestGoProfileRejectsAnalysisModesWithoutTypedRenameEvidence(t *testing.T) {
	for _, mode := range []string{"syntax", "vendor_explicit"} {
		profile := testGoProfile()
		if mode == "syntax" {
			profile.AnalysisMode = mode
		} else {
			profile.ModuleMode = mode
		}
		profile.ID, _ = effectiveGoProfileID(profile)
		if _, _, err := validateGoProfile(profile, ExecutionPolicy{}, t.TempDir()); !errors.Is(err, ErrLSPNotReady) {
			t.Errorf("unsupported typed rename profile mode %q error=%v", mode, err)
		}
	}
}

func TestGoProfileRequiresExactGoplsVersion(t *testing.T) {
	profile := testGoProfile()
	profile.Gopls.Version = "v0.25.0"
	if _, _, err := validateGoProfile(profile, ExecutionPolicy{}, t.TempDir()); !errors.Is(err, ErrLSPNotReady) {
		t.Fatalf("wrong gopls version error=%v", err)
	}
}
