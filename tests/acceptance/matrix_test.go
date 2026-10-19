package acceptance_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type acceptanceMatrix struct {
	FormatVersion int                    `json:"format_version"`
	TaskID        string                 `json:"task_id"`
	SpecVersion   string                 `json:"spec_version"`
	Scope         []string               `json:"scope"`
	Commands      map[string][]string    `json:"commands"`
	Cases         []acceptanceMatrixCase `json:"cases"`
}

type acceptanceMatrixCase struct {
	ID         string   `json:"id"`
	Command    string   `json:"command"`
	Additional []string `json:"additional_commands"`
	Fixture    string   `json:"fixture"`
	Oracle     string   `json:"oracle"`
	Evidence   []string `json:"evidence"`
	Coverage   string   `json:"coverage"`
	Limitation string   `json:"limitation"`
}

func TestAcceptanceMatrixCoversAssignedCasesAndBindsEvidence(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	matrixPath := filepath.Join(root, "artifacts", "acceptance", "T034", "matrix.json")
	data, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("read acceptance matrix %q: %v", matrixPath, err)
	}
	var matrix acceptanceMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		t.Fatalf("decode acceptance matrix: %v", err)
	}
	if matrix.FormatVersion != 1 || matrix.TaskID != "T034" || matrix.SpecVersion != "1.6.13" {
		t.Fatalf("unexpected acceptance matrix identity: version=%d task=%q spec=%q", matrix.FormatVersion, matrix.TaskID, matrix.SpecVersion)
	}
	if strings.Join(matrix.Scope, ",") != "C001-C054,C137-C151" {
		t.Fatalf("acceptance matrix scope=%v; want [C001-C054 C137-C151]", matrix.Scope)
	}

	want := make(map[string]struct{}, 69)
	for value := 1; value <= 54; value++ {
		want["C"+fmt.Sprintf("%03d", value)] = struct{}{}
	}
	for value := 137; value <= 151; value++ {
		want["C"+strconv.Itoa(value)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(matrix.Cases))
	for _, item := range matrix.Cases {
		if _, exists := want[item.ID]; !exists {
			t.Errorf("unexpected acceptance case %q", item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			t.Errorf("duplicate acceptance case %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		commandIDs := append([]string{item.Command}, item.Additional...)
		for _, commandID := range commandIDs {
			command, exists := matrix.Commands[commandID]
			if !exists || len(command) < 2 || (command[0] != "go" && command[0] != "python") {
				t.Errorf("%s has no allowlisted runnable command %q: %v", item.ID, commandID, command)
			}
		}
		if len(strings.TrimSpace(item.Oracle)) < 25 {
			t.Errorf("%s has no case-specific independent oracle", item.ID)
		}
		if item.Coverage != "complete" && item.Coverage != "limited" {
			t.Errorf("%s has invalid coverage scope %q", item.ID, item.Coverage)
		}
		if item.Coverage == "limited" && strings.TrimSpace(item.Limitation) == "" {
			t.Errorf("%s has limited coverage without stating the limit", item.ID)
		}
		if item.Coverage == "complete" && item.Limitation != "" {
			t.Errorf("%s declares complete coverage with a limitation", item.ID)
		}
		if len(item.Evidence) == 0 {
			t.Errorf("%s has no evidence references", item.ID)
		}
		refs := append([]string{item.Fixture}, item.Evidence...)
		for _, ref := range refs {
			if err := validateAcceptanceTestReference(root, ref); err != nil {
				t.Errorf("%s evidence reference %q: %v", item.ID, ref, err)
			}
		}
	}
	for id := range want {
		if _, exists := seen[id]; !exists {
			t.Errorf("assigned acceptance case %s is missing", id)
		}
	}
}

func validateAcceptanceTestReference(root, reference string) error {
	path, symbol, found := strings.Cut(reference, "#")
	if !found || path == "" || symbol == "" {
		return fmt.Errorf("expected repository path#test-name")
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	full := filepath.Join(root, clean)
	relative, err := filepath.Rel(root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("reference escapes repository")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	text := string(data)
	if !strings.Contains(text, "func "+symbol+"(") && !strings.Contains(text, "def "+symbol+"(") {
		return fmt.Errorf("test symbol is absent from source")
	}
	return nil
}
