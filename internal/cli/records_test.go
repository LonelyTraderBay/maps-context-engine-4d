package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

func TestRecordsListIsReadOnlyAndReturnsContractEnvelope(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "missing-home")
	t.Setenv("MCE4D_HOME", home)
	request := `{"limit":5}`
	code, stdout, stderr := invoke([]string{"records", "list", "--worktree-id", "11111111-1111-4111-8111-111111111111", "--input-json", "-", "--json"}, request)
	if code != 0 || stderr != "" {
		t.Fatalf("records list: code=%d stderr=%q", code, stderr)
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Operation string            `json:"operation"`
			Records   []json.RawMessage `json:"records"`
		} `json:"data"`
		Pagination struct {
			EnumerationComplete bool `json:"enumeration_complete"`
			ReturnedCount       int  `json:"returned_count"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("decode records list response: %v", err)
	}
	if response.Status != "ok" || response.Data.Operation != "records_list" || response.Data.Records == nil || len(response.Data.Records) != 0 || !response.Pagination.EnumerationComplete || response.Pagination.ReturnedCount != 0 {
		t.Fatalf("empty records response=%+v", response)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateResponse("CLI records list", []byte(strings.TrimSpace(stdout))); err != nil {
		t.Fatalf("records list response violates contract: %v", err)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("read-only records list created home directory: stat err=%v", err)
	}
}

func TestRecordsListAndReadUseDurableRecordsStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	worktreeID := model.UUID("11111111-1111-4111-8111-111111111111")
	indexPath := filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.OpenRecords(ctx, sqlite.Options{Path: indexPath, HomePath: home, WorktreeID: worktreeID,
		AdmissionConfig: model.AdmissionConfig{SchemaVersion: 1, MaxWorktreeCacheBytes: 64 << 20,
			VolumeReserveBytes: 1 << 20, VolumeWarningBytes: 2 << 20}, LockWaitMS: 500})
	if err != nil {
		t.Fatalf("open durable records store: %v", err)
	}
	created, err := store.Propose(ctx, "server:operator", history.ProposeInput{
		ClientMutationID: model.UUID("33333333-3333-4333-8333-333333333333"), Kind: history.KindDecision,
		Payload: json.RawMessage(`{"decision":"read from records.sqlite"}`), Reason: "CLI durable read test",
	})
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("write fixture record: propose=%v close=%v", err, closeErr)
	}
	t.Setenv("MCE4D_HOME", home)
	code, stdout, stderr := invoke([]string{"records", "list", "--worktree-id", string(worktreeID), "--input-json", "-", "--json"}, `{"limit":10}`)
	if code != 0 || stderr != "" {
		t.Fatalf("records list: code=%d stderr=%q", code, stderr)
	}
	var listed struct {
		Data struct {
			Records []history.Summary `json:"records"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatalf("decode records list: %v", err)
	}
	if len(listed.Data.Records) != 1 || listed.Data.Records[0].RecordID != created.Version.RecordID {
		t.Fatalf("durable records list=%+v, want record %s", listed.Data.Records, created.Version.RecordID)
	}

	request, err := json.Marshal(recordReadRequest{RecordID: created.Version.RecordID})
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = invoke([]string{"records", "read", "--worktree-id", string(worktreeID), "--input-json", "-", "--json"}, string(request))
	if code != 0 || stderr != "" {
		t.Fatalf("records read: code=%d stderr=%q", code, stderr)
	}
	var read struct {
		Data struct {
			Record history.Summary `json:"record"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &read); err != nil {
		t.Fatalf("decode records read: %v", err)
	}
	if read.Data.Record.RecordID != created.Version.RecordID || read.Data.Record.Version != 1 {
		t.Fatalf("durable record read=%+v", read.Data.Record)
	}
}

func TestRecordsReadMissingIsTypedNotFoundAndMutationsAreUnavailable(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "missing-home")
	t.Setenv("MCE4D_HOME", home)
	code, _, stderr := invoke([]string{"records", "read", "--worktree-id", "11111111-1111-4111-8111-111111111111", "--input-json", "-", "--json"}, `{"record_id":"22222222-2222-4222-8222-222222222222"}`)
	if code != 3 || !strings.Contains(stderr, history.ErrRecordNotFound.Error()) {
		t.Fatalf("missing record read: code=%d stderr=%q", code, stderr)
	}
	code, _, stderr = invoke([]string{"records", "approve", "--worktree-id", "11111111-1111-4111-8111-111111111111", "--input-json", "-", "--json"}, `{}`)
	if code != 2 || !strings.Contains(stderr, "unavailable") {
		t.Fatalf("records mutation route: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("read-only records routes created home directory: stat err=%v", err)
	}
}

func TestRecordsRequestRejectsActorAliasAndHelpMatchesDocs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCE4D_HOME", filepath.Join(root, "missing-home"))
	code, _, stderr := invoke([]string{"records", "list", "--worktree-id", "11111111-1111-4111-8111-111111111111", "--input-json", "-", "--json"}, `{"actor_id":"client:forged"}`)
	if code != 2 || !strings.Contains(stderr, "closed contract") {
		t.Fatalf("client actor field: code=%d stderr=%q", code, stderr)
	}
	help, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLI.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"mce4d records list --worktree-id <uuid> --input-json <file-or-dash> --json",
		"mce4d records read --worktree-id <uuid> --input-json <file-or-dash> --json",
	} {
		if !strings.Contains(Help(), command) || !strings.Contains(string(help), command) {
			t.Fatalf("records command is missing from Help or docs: %s", command)
		}
	}
}
