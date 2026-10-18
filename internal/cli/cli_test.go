package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

func invoke(args []string, stdin string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(nil, args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeCandidate(t *testing.T, dir, name, raw string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeConfigData(t *testing.T, raw string) runtimeconfig.RuntimeConfigData {
	t.Helper()
	var data runtimeconfig.RuntimeConfigData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("decode JSON result %q: %v", raw, err)
	}
	return data
}

func TestConfigValidateIsWholeCandidateAndDoesNotWriteHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "missing-home")
	t.Setenv("MCE4D_HOME", home)
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(home, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	old := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"max_indexed_files":100}}`)
	if err := store.CompareAndSwapCanonical(nilContext(), nil, old); err != nil {
		t.Fatal(err)
	}
	candidatePath := writeCandidate(t, root, "candidate.json", `{"schema_version":1,"core":{"query_timeout_ms":2200}}`)
	before, _, err := store.ReadCanonical(nilContext())
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invoke([]string{"config", "validate", "--json", "--input-json", candidatePath}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("validate: code=%d stderr=%q", code, stderr)
	}
	data := decodeConfigData(t, stdout)
	if data.SourceKind != "canonical_home" || len(data.EffectiveSources) != 2 || data.EffectiveSources[1] != "candidate_input" {
		t.Fatalf("candidate provenance = %#v", data)
	}
	if data.Config.Core.MaxIndexedFiles == 100 || data.Config.Core.QueryTimeoutMS != 2200 {
		t.Fatalf("validate merged the old home config or lost the candidate: %#v", data.Config.Core)
	}
	after, present, err := store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(before, after) {
		t.Fatalf("validate changed canonical home bytes: present=%v err=%v", present, err)
	}
}

func TestConfigShowAlternateAndOverridesKeepCanonicalSource(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("MCE4D_HOME", home)
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(home, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	homeCandidate := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"max_indexed_files":300}}`)
	if err := store.CompareAndSwapCanonical(nilContext(), nil, homeCandidate); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.ReadCanonical(nilContext())
	if err != nil {
		t.Fatal(err)
	}
	alternate := writeCandidate(t, root, "alternate.json", `{"schema_version":1,"core":{"query_timeout_ms":1600}}`)
	code, stdout, stderr := invoke([]string{"config", "show", "--json", "--alternate-json", alternate, "--override", "query_timeout_ms=1800"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("show: code=%d stderr=%q", code, stderr)
	}
	data := decodeConfigData(t, stdout)
	homeSource, err := pipeline.ParseSource(homeCandidate, true)
	if err != nil {
		t.Fatal(err)
	}
	if data.SourceKind != "canonical_home" || data.SourceDigest == nil || *data.SourceDigest != *homeSource.Digest() {
		t.Fatalf("canonical source provenance = %#v", data)
	}
	if data.Config.Core.MaxIndexedFiles != 300 || data.Config.Core.QueryTimeoutMS != 1800 {
		t.Fatalf("effective layer values = %#v", data.Config.Core)
	}
	want := []string{"built_in", "canonical_home", "approved_alternate", "cli_process"}
	if strings.Join(data.EffectiveSources, ",") != strings.Join(want, ",") {
		t.Fatalf("effective sources = %v, want %v", data.EffectiveSources, want)
	}
	after, present, err := store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(before, after) {
		t.Fatalf("show changed canonical home bytes: present=%v err=%v", present, err)
	}
}

func TestConfigSetUsesExpectedDigestAndPreservesBytesOnConflict(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("MCE4D_HOME", home)
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(home, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	initial := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":1300}}`)
	if err := store.CompareAndSwapCanonical(nilContext(), nil, initial); err != nil {
		t.Fatal(err)
	}
	candidate := writeCandidate(t, root, "candidate.json", `{"schema_version":1,"core":{"query_timeout_ms":2400}}`)
	before, _, err := store.ReadCanonical(nilContext())
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invoke([]string{"config", "set", "--json", "--input-json", candidate, "--expected-digest", "absent"}, "")
	if code != 3 || stdout != "" || stderr == "" {
		t.Fatalf("stale set: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	after, present, err := store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(before, after) {
		t.Fatalf("stale set changed file bytes: present=%v err=%v", present, err)
	}
	current, err := pipeline.ParseSource(before, true)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = invoke([]string{"config", "set", "--json", "--input-json", candidate, "--expected-digest", string(*current.Digest())}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("set with current digest: code=%d stderr=%q", code, stderr)
	}
	data := decodeConfigData(t, stdout)
	if data.Config.Core.QueryTimeoutMS != 2400 || data.SourceKind != "canonical_home" || data.SourceDigest == nil {
		t.Fatalf("set result = %#v", data)
	}
	after, present, err = store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(after, canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":2400}}`)) {
		t.Fatalf("successful set did not replace the complete canonical source: present=%v err=%v", present, err)
	}
}

func TestConfigValidateFromStdinAndInputLimit(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent-home")
	t.Setenv("MCE4D_HOME", home)
	code, stdout, stderr := invoke([]string{"config", "validate", "--json", "--input-json", "-"}, `{"schema_version":1,"core":{"query_timeout_ms":2000}}`)
	if code != 0 || stderr != "" || decodeConfigData(t, stdout).Config.Core.QueryTimeoutMS != 2000 {
		t.Fatalf("stdin validate: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("validate created the MCE4D home: stat err=%v", err)
	}
	tooLarge := strings.Repeat(" ", maxRuntimeConfigBytes+1)
	code, stdout, stderr = invoke([]string{"config", "validate", "--json", "--input-json", "-"}, tooLarge)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "1 MiB") {
		t.Fatalf("oversized stdin: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestHelpVersionAndUnknownArguments(t *testing.T) {
	code, stdout, stderr := invoke([]string{"version", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("version: code=%d stderr=%q", code, stderr)
	}
	var versionResult map[string]any
	if err := json.Unmarshal([]byte(stdout), &versionResult); err != nil || versionResult["version"] != version {
		t.Fatalf("version JSON = %q (parse error %v)", stdout, err)
	}
	code, stdout, stderr = invoke([]string{"config", "set", "--json", "--input-json", "-", "--bogus"}, `{}`)
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf("unknown flag: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = invoke([]string{"--help"}, "")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "config validate") {
		t.Fatalf("help: code=%d stderr=%q", code, stderr)
	}
}

func TestDoctorReportsSQLiteAndDoesNotCreateHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("MCE4D_HOME", home)
	code, stdout, stderr := invoke([]string{"doctor", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor: code=%d stderr=%q", code, stderr)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor JSON %q: %v", stdout, err)
	}
	if report.Operation != "doctor" || report.Platform.OS == "" || report.SQLiteFTS5.Available == false && report.SQLiteFTS5.Issue == "" {
		t.Fatalf("doctor omitted platform or FTS status: %#v", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("doctor created the MCE4D home: stat err=%v", err)
	}
}

func TestHelpCommandsArePresentInCLIReference(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLI.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"mce4d version --json",
		"mce4d doctor --json",
		"mce4d config show [--json] [--alternate-json <file>] [--override <key=value>...]",
		"mce4d config validate --input-json <file-or-dash>",
		"mce4d config set --input-json <file-or-dash> --expected-digest <sha256-or-absent>",
	} {
		if !strings.Contains(Help(), command) || !strings.Contains(string(docs), command) {
			t.Errorf("help and docs must both include %q", command)
		}
	}
}

func TestParseExpectedDigestRejectsMalformedValue(t *testing.T) {
	if digest, err := parseExpectedDigest("not-a-digest"); err == nil || digest != nil {
		t.Fatalf("malformed expected digest accepted: %#v, %v", digest, err)
	}
	if digest, err := parseExpectedDigest("absent"); err != nil || digest != nil {
		t.Fatalf("absent source digest = %#v, %v", digest, err)
	}
	valid, err := model.ParseHash(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := parseExpectedDigest(string(valid)); err != nil || digest == nil || *digest != valid {
		t.Fatalf("valid expected digest = %#v, %v", digest, err)
	}
}

func nilContext() context.Context { return context.Background() }
