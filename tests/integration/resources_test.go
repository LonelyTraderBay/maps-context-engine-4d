package integration_test

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const fixtureTeardownAuthority = "A046/TEARDOWN-FIXTURE-01"

type fixtureTeardownRequest struct {
	runRoot         string
	fixtureRoot     string
	runID           string
	fixtureID       string
	trustedRoot     string
	authority       string
	baselineAbsent  bool
	reconstructible bool
	writersSettled  bool
	crashLeft       bool
	oldOrShared     bool
	staged          bool
	evidencePath    string
	protectedPaths  []string
	requestedPaths  []string
	createdIdentity map[string]fixtureIdentity
}

type fixtureIdentity struct {
	info   os.FileInfo
	digest [32]byte
}

func TestFreshSyntheticGitAndSQLiteFixtureTeardownEligibility(t *testing.T) {
	request := makeFreshFixtureTeardownRequest(t, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if !fixtureTeardownEligible(request) {
		t.Fatal("fresh reconstructible fixture with closed writers and retained evidence was refused")
	}
	request.requestedPaths = append(request.requestedPaths, request.evidencePath)
	if fixtureTeardownEligible(request) {
		t.Fatal("fixture teardown included retained failure evidence")
	}
}

func TestFixtureTeardownRefusesProtectedAndUnprovenCandidates(t *testing.T) {
	mutations := []struct {
		name   string
		change func(*fixtureTeardownRequest)
	}{
		{name: "missing authority", change: func(request *fixtureTeardownRequest) { request.authority = "" }},
		{name: "baseline existed", change: func(request *fixtureTeardownRequest) { request.baselineAbsent = false }},
		{name: "not reconstructible", change: func(request *fixtureTeardownRequest) { request.reconstructible = false }},
		{name: "writer still active", change: func(request *fixtureTeardownRequest) { request.writersSettled = false }},
		{name: "crash leftover", change: func(request *fixtureTeardownRequest) { request.crashLeft = true }},
		{name: "old or shared data", change: func(request *fixtureTeardownRequest) { request.oldOrShared = true }},
		{name: "staged data", change: func(request *fixtureTeardownRequest) { request.staged = true }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			request := makeFreshFixtureTeardownRequest(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			mutation.change(&request)
			if fixtureTeardownEligible(request) {
				t.Fatal("unproven fixture candidate was admitted for teardown")
			}
		})
	}
}

func TestFixtureTeardownRefusesLinkedGitAndUnexpectedReplacement(t *testing.T) {
	t.Run("linked common directory", func(t *testing.T) {
		request := makeFreshFixtureTeardownRequest(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
		common := t.TempDir()
		if err := os.WriteFile(filepath.Join(request.fixtureRoot, ".git", "commondir"), []byte(common), 0o600); err != nil {
			t.Fatal(err)
		}
		request.createdIdentity = fixtureInventory(t, request.fixtureRoot)
		if fixtureTeardownEligible(request) {
			t.Fatal("fixture with an external Git common directory was admitted")
		}
	})
	t.Run("unexpected file", func(t *testing.T) {
		request := makeFreshFixtureTeardownRequest(t, "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
		if err := os.WriteFile(filepath.Join(request.fixtureRoot, "unexpected.bin"), []byte("unmanifested"), 0o600); err != nil {
			t.Fatal(err)
		}
		if fixtureTeardownEligible(request) {
			t.Fatal("fixture with an unmanifested file was admitted")
		}
	})
	t.Run("identity replaced", func(t *testing.T) {
		request := makeFreshFixtureTeardownRequest(t, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
		databasePath := filepath.Join(request.fixtureRoot, "home", "records.sqlite")
		if err := os.Remove(databasePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(databasePath, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		if fixtureTeardownEligible(request) {
			t.Fatal("candidate path replacement was admitted")
		}
	})
}

func makeFreshFixtureTeardownRequest(t *testing.T, fixtureID string) fixtureTeardownRequest {
	t.Helper()
	base := t.TempDir()
	runID := "11111111-1111-4111-8111-111111111111"
	runRoot := filepath.Join(base, "artifacts", "runs", runID)
	fixtureRoot := filepath.Join(runRoot, "fixtures", fixtureID)
	if _, err := os.Lstat(fixtureRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture baseline was not absent: %v", err)
	}
	for _, directory := range []string{filepath.Join(fixtureRoot, ".git", "objects"), filepath.Join(fixtureRoot, ".git", "refs"), filepath.Join(fixtureRoot, "home")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for relative, value := range map[string]string{
		".git/HEAD":                          "ref: refs/heads/main\n",
		".git/config":                        "[core]\n\trepositoryformatversion = 0\n\tbare = false\n",
		"main.go":                            "package fixture\n\nfunc Value() int { return 1 }\n",
		"home/catalog.sqlite.fixture-source": "synthetic fixture recipe\n",
	} {
		path := filepath.Join(fixtureRoot, filepath.FromSlash(relative))
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	databasePath := filepath.Join(fixtureRoot, "home", "records.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE fixture_records (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(base, "retained-failure-evidence.json")
	if err := os.WriteFile(evidencePath, []byte("fixture evidence retained outside teardown root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalHome := filepath.Join(base, "canonical-mce4d-home")
	if err := os.Mkdir(canonicalHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return fixtureTeardownRequest{
		runRoot: runRoot, fixtureRoot: fixtureRoot, runID: runID, fixtureID: fixtureID,
		trustedRoot: base,
		authority:   fixtureTeardownAuthority, baselineAbsent: true, reconstructible: true, writersSettled: true,
		evidencePath: evidencePath, protectedPaths: []string{canonicalHome, evidencePath},
		requestedPaths: []string{fixtureRoot}, createdIdentity: fixtureInventory(t, fixtureRoot),
	}
}

func fixtureInventory(t *testing.T, root string) map[string]fixtureIdentity {
	t.Helper()
	result := make(map[string]fixtureIdentity)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		identity := fixtureIdentity{info: info}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			identity.digest = sha256.Sum256(data)
		}
		result[relative] = identity
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func fixtureTeardownEligible(request fixtureTeardownRequest) bool {
	if request.authority != fixtureTeardownAuthority || !request.baselineAbsent || !request.reconstructible ||
		!request.writersSettled || request.crashLeft || request.oldOrShared || request.staged ||
		len(request.createdIdentity) == 0 || len(request.requestedPaths) != 1 || request.requestedPaths[0] != request.fixtureRoot {
		return false
	}
	if request.runID != "11111111-1111-4111-8111-111111111111" || !validFixtureUUID(request.fixtureID) ||
		filepath.Clean(request.fixtureRoot) != filepath.Join(request.runRoot, "fixtures", request.fixtureID) {
		return false
	}
	if !fixturePathWithin(request.trustedRoot, request.runRoot) || !fixturePathWithin(request.runRoot, request.fixtureRoot) ||
		fixturePathWithin(request.fixtureRoot, request.evidencePath) || !fixtureAncestorsAreDirectories(request.trustedRoot, request.fixtureRoot) {
		return false
	}
	for _, protected := range request.protectedPaths {
		if fixturePathWithin(request.fixtureRoot, protected) || fixturePathWithin(protected, request.fixtureRoot) {
			return false
		}
	}
	rootInfo, err := os.Lstat(request.fixtureRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return false
	}
	if err := validateSyntheticGitMetadata(request.fixtureRoot); err != nil {
		return false
	}
	observed := make(map[string]bool, len(request.createdIdentity))
	err = filepath.WalkDir(request.fixtureRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("fixture contains an unreadable path or link")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&(os.ModeSymlink|os.ModeIrregular|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 {
			return errors.New("fixture contains an unsupported filesystem object")
		}
		relative, err := filepath.Rel(request.fixtureRoot, path)
		if err != nil {
			return err
		}
		baseline, exists := request.createdIdentity[relative]
		if !exists || !os.SameFile(baseline.info, info) || baseline.info.Size() != info.Size() || !baseline.info.ModTime().Equal(info.ModTime()) {
			return errors.New("fixture contains an unexpected or replaced candidate")
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(data) != baseline.digest {
				return errors.New("fixture contents changed after ownership was recorded")
			}
		}
		observed[relative] = true
		return nil
	})
	if err != nil || len(observed) != len(request.createdIdentity) {
		return false
	}
	evidenceInfo, err := os.Lstat(request.evidencePath)
	if err != nil || !evidenceInfo.Mode().IsRegular() {
		return false
	}
	return true
}

func validateSyntheticGitMetadata(root string) error {
	gitPath := filepath.Join(root, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("synthetic Git metadata is not a private directory")
	}
	for _, relative := range []string{"commondir", filepath.Join("objects", "info", "alternates"), "worktrees"} {
		if _, err := os.Lstat(filepath.Join(gitPath, relative)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return errors.New("linked or shared Git metadata is protected")
		}
	}
	return nil
}

func fixturePathWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && relative != "." && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func fixtureAncestorsAreDirectories(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	current := rootAbs
	relative, err := filepath.Rel(rootAbs, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	if info, err := os.Lstat(current); err != nil || !info.IsDir() || info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return false
	}
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "." || segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return false
		}
	}
	return true
}

func validFixtureUUID(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, character := range strings.ReplaceAll(value, "-", "") {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
