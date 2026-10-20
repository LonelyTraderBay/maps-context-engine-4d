package providers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func pythonProfileForTest(descriptor Descriptor) Profile {
	config := pythonProfileConfig{
		SchemaVersion: 1, Language: "python", PythonImplementation: "CPython", PythonVersion: "3.12.10",
		PythonExecutableDigest: model.SHA256([]byte("python-executable")), ProviderDigest: descriptor.Digest,
		ModuleRoots: []string{"src"}, AnalysisMode: "syntax", AllowProjectPlugins: false,
	}
	raw, _ := json.Marshal(config)
	return Profile{
		SchemaID: "python-profile", SchemaVersion: 1, ProfileID: model.SHA256([]byte("profile")), Config: raw,
	}
}

func pythonDescriptorForTest() Descriptor {
	return Descriptor{
		ID: pythonProviderID, Version: "1.0.0", License: "Apache-2.0", Digest: model.SHA256([]byte("python-provider")),
		Languages: []string{"python"}, Capabilities: []Capability{{ID: "symbols.syntax", Version: "1.0.0"}},
	}
}

func TestPythonProfileIsClosedAndBoundToCapturedRoots(t *testing.T) {
	descriptor := pythonDescriptorForTest()
	profile := pythonProfileForTest(descriptor)
	sources := map[string][]byte{"src/app.py": []byte("value = 1\n")}
	if err := validatePythonProfile(profile, descriptor, sources); err != nil {
		t.Fatalf("valid Python profile rejected: %v", err)
	}
	if _, _, err := normalizeRequest(Request{
		RequestID: model.UUID("123e4567-e89b-12d3-a456-426614174000"), Language: "python", Profile: profile,
		Files: []SourceFile{{Path: "src/app.py", Source: sources["src/app.py"]}},
	}, descriptor); err != nil {
		t.Fatalf("host boundary rejected valid Python request: %v", err)
	}
}

func TestPythonProfileRejectsUnknownMissingAndUncapturedFields(t *testing.T) {
	descriptor := pythonDescriptorForTest()
	sources := map[string][]byte{"src/app.py": []byte("value = 1\n")}
	tests := []struct {
		name   string
		config string
		files  map[string][]byte
	}{
		{
			name:   "unknown field",
			config: `{"schema_version":1,"language":"python","python_implementation":"CPython","python_version":"3.12.10","python_executable_digest":"` + string(model.SHA256([]byte("python-executable"))) + `","provider_digest":"` + string(descriptor.Digest) + `","module_roots":["src"],"analysis_mode":"syntax","allow_project_plugins":false,"plugins":[]}`,
			files:  sources,
		},
		{
			name:   "missing required field",
			config: `{"schema_version":1,"language":"python","python_implementation":"CPython","python_version":"3.12.10","python_executable_digest":"` + string(model.SHA256([]byte("python-executable"))) + `","provider_digest":"` + string(descriptor.Digest) + `","module_roots":["src"],"analysis_mode":"syntax"}`,
			files:  sources,
		},
		{
			name:   "source outside root",
			config: string(pythonProfileForTest(descriptor).Config),
			files:  map[string][]byte{"src/app.py": []byte("value = 1\n"), "other/app.py": []byte("value = 2\n")},
		},
		{
			name:   "duplicate config key",
			config: `{"schema_version":1,"schema_version":1,"language":"python","python_implementation":"CPython","python_version":"3.12.10","python_executable_digest":"` + string(model.SHA256([]byte("python-executable"))) + `","provider_digest":"` + string(descriptor.Digest) + `","module_roots":["src"],"analysis_mode":"syntax","allow_project_plugins":false}`,
			files:  sources,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := Profile{
				SchemaID: "python-profile", SchemaVersion: 1, ProfileID: model.SHA256([]byte("profile")),
				Config: json.RawMessage(test.config),
			}
			if err := validatePythonProfile(profile, descriptor, test.files); err == nil {
				t.Fatal("invalid Python profile was accepted")
			}
		})
	}
}

func TestPythonProviderContractFixtures(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate Python provider contract tests")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "python", "-B", "-m", "unittest", "discover", "-s", "packages/python-provider/test", "-v")
	command.Dir = repositoryRoot
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(repositoryRoot, "packages", "python-provider", "src"),
		"PYTHONDONTWRITEBYTECODE=1",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Python provider contract fixtures failed: %v\n%s", err, output)
	}
}
