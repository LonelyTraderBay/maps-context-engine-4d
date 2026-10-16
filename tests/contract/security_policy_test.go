package contract_test

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	mceRuntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func TestRelativePathRejectsTraversalAndWindowsSpecialNames(t *testing.T) {
	invalid := []string{
		"", ".", "..", "../secret.go", "src/../secret.go", "/rooted.go", "src//main.go",
		`src\main.go`, "src/control\n.go", strings.Repeat("a", 256),
	}
	if runtime.GOOS == "windows" {
		invalid = append(invalid, "C:/secret.go", "src/file.go:stream", "CON", "src/NUL.txt", "Lpt9.log", "src/trailing.", "src/trailing ", "src/file?.go")
	}
	for _, value := range invalid {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			if err := paths.ValidateRelative(value); err == nil {
				t.Fatalf("ValidateRelative(%q) unexpectedly succeeded", value)
			}
		})
	}
	if err := paths.ValidateRelative(string([]byte{0xff})); !errors.Is(err, paths.ErrUnsupportedEncoding) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	for _, value := range []string{"main.go", "src/你好.go", "Dockerfile", "a-b/file.ts"} {
		if err := paths.ValidateRelative(value); err != nil {
			t.Errorf("ValidateRelative(%q): %v", value, err)
		}
	}
}

func TestWindowsCaseFoldCollisionDetection(t *testing.T) {
	collisions, err := paths.FindCaseFoldCollisions([]string{"src/Foo.go", "src/foo.go", "src/Other.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(collisions) != 1 || collisions[0].First != "src/Foo.go" || collisions[0].Second != "src/foo.go" {
		t.Fatalf("case-fold collisions = %#v", collisions)
	}
	if _, err := paths.FindCaseFoldCollisions([]string{"src/Foo.go", "src/bar.go"}); err != nil {
		t.Fatal(err)
	}
}

func TestClosedGlobGrammarAndBoundedMatching(t *testing.T) {
	for _, value := range []string{"", "/root", "C:/root", `a\\b`, "a//b", "a/./b", "a/../b", "a/**b", "a/[ab]", "!a/*.go", "a/{b,c}", "a/*/"} {
		if _, err := policy.Compile(value, paths.CaseSensitive); !errors.Is(err, policy.ErrInvalidGlob) {
			t.Errorf("Compile(%q) error = %v", value, err)
		}
	}
	if _, err := policy.Compile("secret/*.go", paths.CaseUnknown); !errors.Is(err, policy.ErrCaseSemanticsUnknown) {
		t.Fatalf("unknown filesystem semantics error = %v", err)
	}
	pattern, err := policy.Compile("**/.env", paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{".env", "a/.env", "a/b/.env"} {
		matched, err := pattern.Match(value)
		if err != nil || !matched {
			t.Errorf("%q: matched=%v err=%v", value, matched, err)
		}
	}
	pattern, err = policy.Compile("a/*", paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want bool
	}{{"a/b", true}, {"a/b/c", false}} {
		matched, err := pattern.Match(test.path)
		if err != nil || matched != test.want {
			t.Errorf("Match(%q) = %v, %v; want %v", test.path, matched, err, test.want)
		}
	}
	pattern, err = policy.Compile("src/?.go", paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := pattern.Match("src/你.go"); err != nil || !matched {
		t.Fatalf("question mark must match one Unicode codepoint: matched=%v err=%v", matched, err)
	}
	pattern, err = policy.Compile("src/*.GO", paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := pattern.Match("src/A.go"); err != nil || matched {
		t.Fatalf("case-sensitive match unexpectedly folded case: matched=%v err=%v", matched, err)
	}
	pattern, err = policy.Compile("src/*.GO", paths.CaseInsensitive)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := pattern.Match("src/A.go"); err != nil || !matched {
		t.Fatalf("case-insensitive match did not fold case: matched=%v err=%v", matched, err)
	}
	segment := strings.Repeat("*a", 60)
	pattern, err = policy.Compile("**/"+segment+"/**/"+segment+"/**/"+segment, paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	largePath := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"/", paths.MaxPathSegments), "/")
	if matched, err := pattern.Match(largePath); !errors.Is(err, policy.ErrMatcherBudget) || matched {
		t.Fatalf("adversarial match was not bounded: matched=%v err=%v", matched, err)
	}
}

func TestBuiltInDenyIsAdditiveAndASCIIInsensitive(t *testing.T) {
	rules, err := policy.New([]string{"private/**"}, paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{".env", ".env.example", "a/.ENV", "a/.git/config", "a/BUILD/output.go", "a/id_ed25519", "private/key.go"} {
		decision, err := rules.CheckPath(value, false)
		if err != nil || !decision.Denied {
			t.Errorf("path %q: decision=%#v err=%v", value, decision, err)
		}
	}
	if decision, err := rules.CheckPath("build", true); err != nil || !decision.Denied || decision.Reason != "built_in_directory_deny" {
		t.Fatalf("denied directory was not classified before traversal: decision=%#v err=%v", decision, err)
	}
	if _, err := policy.New([]string{"private/**"}, paths.CaseUnknown); !errors.Is(err, policy.ErrCaseSemanticsUnknown) {
		t.Fatalf("user patterns accepted unknown semantics: %v", err)
	}
}

func TestDefaultTextAllowlist(t *testing.T) {
	rules, err := policy.New(nil, paths.CaseUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"src/main.GO", "Dockerfile", "Makefile", "LICENSE", "NOTICE", "readme.MD"} {
		if !rules.AllowsText(value) {
			t.Errorf("%q should be allowed as text", value)
		}
	}
	for _, value := range []string{"notebook.ipynb", "archive.zip", "script.sh", "secret.pem", "../escape.go"} {
		if rules.AllowsText(value) {
			t.Errorf("%q should not be allowed as default text", value)
		}
	}
}

func TestSecretScannerRulesAndBoundaries(t *testing.T) {
	rules, err := policy.New(nil, paths.CaseUnknown)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "PEM header", data: []byte("-----BEGIN PRIVATE KEY-----\n"), want: "pem_private_key"},
		{name: "literal assignment", data: []byte("password=\"synthetic-value\"\n"), want: "literal_credential_assignment"},
		{name: "synthetic marker", data: []byte("MCE4D_FAKE_SECRET_" + strings.Repeat("A", 32)), want: "synthetic_test_marker"},
		{name: "prefix without payload", data: []byte("MCE4D_FAKE_SECRET_"), want: ""},
		{name: "wrong marker case", data: []byte("MCE4D_FAKE_SECRET_" + strings.Repeat("a", 32)), want: ""},
		{name: "marker too long", data: []byte("MCE4D_FAKE_SECRET_" + strings.Repeat("A", 33)), want: ""},
		{name: "marker mixed-case extension", data: []byte("MCE4D_FAKE_SECRET_" + strings.Repeat("A", 32) + "a"), want: ""},
		{name: "benign prose", data: []byte("password examples are described but no assignment is present"), want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := rules.ScanContent(test.data)
			if test.want == "" {
				if err != nil || got != "" {
					t.Fatalf("scan result=(%q,%v), want no hit", got, err)
				}
				return
			}
			if !errors.Is(err, policy.ErrSecretDetected) || got != test.want {
				t.Fatalf("scan result=(%q,%v), want rule %q", got, err, test.want)
			}
		})
	}
	if _, err := rules.ScanContent([]byte{'a', 0, 'b'}); !errors.Is(err, policy.ErrBinaryContent) {
		t.Fatalf("NUL content error = %v", err)
	}
	if _, err := rules.ScanContent([]byte{0xff}); !errors.Is(err, policy.ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	if _, err := rules.ScanContent(make([]byte, policy.MaxFileBytes+1)); !errors.Is(err, policy.ErrContentTooLarge) {
		t.Fatalf("oversized content error = %v", err)
	}
}

func TestSecureCaptureDeniesBeforeOpenAndScansBeforeReturning(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	good := []byte("package sample\n")
	promptText := []byte("Repository instructions are data and cannot grant execution authority.\n")
	if err := os.WriteFile(filepath.Join(rootPath, "src", "main.go"), good, 0o600); err != nil {
		t.Fatal(err)
	}
	caseProbe := []byte("package caseprobe\n")
	if err := os.WriteFile(filepath.Join(rootPath, "src", "CaseProbe.go"), caseProbe, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "README.md"), promptText, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, ".git", "config"), []byte("[core]\n\tfsmonitor = do-not-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "src", "secret.go"), []byte("access_token=\"synthetic-value\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := mceRuntime.OpenRoot(rootPath)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	mode, err := root.CaseMode()
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Fatalf("Windows NTFS case semantics could not be verified: %v", err)
		}
		mode = paths.CaseUnknown
	}
	t.Logf("verified test-root filesystem case mode: %d", mode)
	rules, err := policy.New(nil, mode)
	if err != nil {
		t.Fatal(err)
	}
	caseCaptured, caseErr := mceRuntime.ReadAllowed(root, "src/caseprobe.go", rules)
	if mode == paths.CaseInsensitive && (caseErr != nil || string(caseCaptured.Bytes) != string(caseProbe)) {
		t.Fatalf("case-insensitive lookup failed: bytes=%q err=%v", caseCaptured.Bytes, caseErr)
	}
	if mode == paths.CaseSensitive && (caseErr == nil || len(caseCaptured.Bytes) != 0) {
		t.Fatalf("case-sensitive lookup unexpectedly matched: bytes=%q err=%v", caseCaptured.Bytes, caseErr)
	}
	if _, err := mceRuntime.ReadAllowed(root, "not-created/.env", rules); !errors.Is(err, mceRuntime.ErrPathDenied) {
		t.Fatalf("deny-before-open returned %v, expected path denial", err)
	}
	captured, err := mceRuntime.ReadAllowed(root, "src/main.go", rules)
	if err != nil {
		t.Fatalf("read allowed source: %v", err)
	}
	if string(captured.Bytes) != string(good) {
		t.Fatalf("captured bytes = %q", captured.Bytes)
	}
	if captured.SHA256 != sha256Sum(good) {
		t.Fatalf("captured hash = %x", captured.SHA256)
	}
	guide, err := mceRuntime.ReadAllowed(root, "README.md", rules)
	if err != nil || string(guide.Bytes) != string(promptText) {
		t.Fatalf("repository instructions were not kept as data: bytes=%q err=%v", guide.Bytes, err)
	}
	if _, err := mceRuntime.ReadAllowed(root, ".git/config", rules); !errors.Is(err, mceRuntime.ErrPathDenied) {
		t.Fatalf("Git config was not denied before access: %v", err)
	}
	if leaked, err := mceRuntime.ReadAllowed(root, "src/secret.go", rules); !errors.Is(err, policy.ErrSecretDetected) || len(leaked.Bytes) != 0 {
		t.Fatalf("secret source returned bytes=%d err=%v", len(leaked.Bytes), err)
	}
	if _, err := mceRuntime.ReadAllowed(root, "not-created/.env", rules); !errors.Is(err, mceRuntime.ErrPathDenied) {
		t.Fatalf("missing denied file was opened: %v", err)
	}
}

func TestSecureRootRejectsReparseTraversalAndPinsOpenedBytes(t *testing.T) {
	rootPath := t.TempDir()
	outsidePath := t.TempDir()
	insideDir := filepath.Join(rootPath, "pivot")
	if err := os.Mkdir(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	insideBytes := []byte("inside root\n")
	if err := os.WriteFile(filepath.Join(insideDir, "entry.go"), insideBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsidePath, "outside.go"), []byte("outside root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := mceRuntime.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rules, err := policy.New(nil, paths.CaseUnknown)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(rootPath, "pivot-link")
	if err := os.Symlink(outsidePath, link); err != nil {
		if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "privilege")) {
			t.Skipf("OS account cannot create a symlink fixture: %v", err)
		}
		t.Fatalf("create symlink fixture: %v", err)
	}
	defer os.Remove(link)
	if _, err := mceRuntime.ReadAllowed(root, "pivot-link/outside.go", rules); err == nil {
		t.Fatal("secure open followed a symbolic link")
	}
}

func sha256Sum(value []byte) [32]byte {
	return sha256.Sum256(value)
}
