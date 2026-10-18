package integration_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const (
	helperModeEnv       = "MCE4D_CONCURRENCY_HELPER_MODE"
	helperHomeEnv       = "MCE4D_CONCURRENCY_HELPER_HOME"
	helperDBEnv         = "MCE4D_CONCURRENCY_HELPER_DB"
	helperWorktreeEnv   = "MCE4D_CONCURRENCY_HELPER_WORKTREE"
	helperProfileEnv    = "MCE4D_CONCURRENCY_HELPER_PROFILE"
	helperGenerationEnv = "MCE4D_CONCURRENCY_HELPER_GENERATION"

	worktreeA = "11111111-1111-4111-8111-111111111111"
	worktreeB = "22222222-2222-4222-8222-222222222222"

	profileA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	profileB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	fixtureRemote = "https://example.invalid/owner/maps-context-engine-4d.git"
	fixtureHead   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixtureBase   = "maps-context-engine-4d"

	oldGenerationA      = "30000000-0000-4000-8000-000000000001"
	buildingGenerationA = "30000000-0000-4000-8000-000000000002"
	otherGenerationA    = "30000000-0000-4000-8000-000000000003"
	oldGenerationB      = "40000000-0000-4000-8000-000000000001"
	buildingGenerationB = "40000000-0000-4000-8000-000000000002"
)

const helperWait = 8 * time.Second

type indexFixture struct {
	home     string
	database string
	worktree string
	profileA string
	profileB string
	remote   string
	head     string
	base     string
}

type concurrencyHelper struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   <-chan string
	done    <-chan error
}

func TestConcurrencyHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}

	home := os.Getenv(helperHomeEnv)
	worktreeID := os.Getenv(helperWorktreeEnv)
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := mceruntime.EnsureWorktreeLockLayout(root, worktreeID); err != nil {
		t.Fatal(err)
	}
	lease, err := mceruntime.AcquireWorktreeWriter(context.Background(), root, worktreeID, helperWait)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	if mode == "publish" {
		if err := publishFixtureGeneration(
			os.Getenv(helperDBEnv), os.Getenv(helperProfileEnv), os.Getenv(helperGenerationEnv),
		); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "PUBLISHED:%s\n", worktreeID)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "LOCKED:%s\n", worktreeID)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestWorktreeWritersAndPublishedReadersAreIndependentAcrossProcesses(t *testing.T) {
	base := t.TempDir()
	fixtureA := prepareIndexFixture(t, filepath.Join(base, "same-repository-basename"), worktreeA, true)
	fixtureB := prepareIndexFixture(t, filepath.Join(base, "same-repository-basename"), worktreeB, false)

	storeA, err := sqlite.OpenIndexReadOnly(context.Background(), readOnlyOptions(fixtureA))
	if err != nil {
		t.Fatalf("open read-only index A: %v", err)
	}
	defer storeA.Close()
	if storeA.WorktreeID() != model.UUID(worktreeA) {
		t.Fatalf("index A owner=%s; want %s", storeA.WorktreeID(), worktreeA)
	}
	if _, err := sqlite.OpenIndexReadOnly(context.Background(), sqlite.Options{
		Path:     filepath.Join(fixtureA.home, "worktrees", worktreeA, "index.sqlite"),
		HomePath: fixtureA.home, WorktreeID: model.UUID(worktreeB), LockWaitMS: 500,
	}); !errors.Is(err, sqlite.ErrInvalidStorePath) {
		t.Fatalf("opening A's index with B identity returned %v; want ErrInvalidStorePath", err)
	}

	readerPinned := make(chan model.UUID, 1)
	releaseReader := make(chan struct{})
	readerResult := make(chan error, 1)
	readerContext, cancelReader := context.WithTimeout(context.Background(), 3*helperWait)
	defer cancelReader()
	go func() {
		readerResult <- storeA.WithPublishedSnapshot(readerContext, model.Hash(profileA), "", func(snapshot *sqlite.ReadSnapshot) error {
			readerPinned <- snapshot.Generation.Key.GenerationID
			select {
			case <-releaseReader:
			case <-readerContext.Done():
				return readerContext.Err()
			}
			var activeID, marker string
			row, err := snapshot.QueryRowContext(context.Background(),
				`SELECT generation_id FROM active_generations WHERE profile_id=?`, profileA)
			if err != nil {
				return err
			}
			if err := row.Scan(&activeID); err != nil {
				return err
			}
			row, err = snapshot.QueryRowContext(context.Background(),
				`SELECT path FROM files WHERE generation_id=?`, oldGenerationA)
			if err != nil {
				return err
			}
			if err := row.Scan(&marker); err != nil {
				return err
			}
			if activeID != oldGenerationA || marker != oldGenerationA || string(snapshot.Generation.Key.GenerationID) != oldGenerationA {
				return fmt.Errorf("pinned reader mixed generations: active=%s marker=%s pinned=%s", activeID, marker, snapshot.Generation.Key.GenerationID)
			}
			return nil
		})
	}()
	select {
	case generationID := <-readerPinned:
		if string(generationID) != oldGenerationA {
			close(releaseReader)
			t.Fatalf("reader pinned %s; want %s", generationID, oldGenerationA)
		}
	case <-time.After(helperWait):
		close(releaseReader)
		t.Fatal("reader did not pin the published generation")
	}

	if err := storeA.WithPublishedSnapshot(context.Background(), model.Hash(profileA), model.UUID(buildingGenerationA), func(*sqlite.ReadSnapshot) error {
		return errors.New("BUILDING generation reached a published reader")
	}); !errors.Is(err, sqlite.ErrGenerationUnavailable) {
		t.Fatalf("explicit BUILDING generation read returned %v; want ErrGenerationUnavailable", err)
	}
	if err := storeA.WithPublishedSnapshot(context.Background(), model.Hash(profileA), "", func(snapshot *sqlite.ReadSnapshot) error {
		if snapshot.Generation.Key.GenerationID != model.UUID(oldGenerationA) {
			return fmt.Errorf("active pointer changed before publication: %s", snapshot.Generation.Key.GenerationID)
		}
		return nil
	}); err != nil {
		t.Fatalf("independent reader before publication: %v", err)
	}

	helperA := startConcurrencyHelper(t, fixtureA, "publish", profileA, buildingGenerationA)
	helperA.expectLine(t, "PUBLISHED:"+worktreeA)
	assertWriterBusy(t, fixtureA)

	// A and B represent worktrees with the same remote and basename. Their OS
	// writer ownership follows the worktree UUID, so B can commit while A holds it.
	helperB := startConcurrencyHelper(t, fixtureB, "publish", profileA, buildingGenerationB)
	helperB.expectLine(t, "PUBLISHED:"+worktreeB)
	assertWriterBusy(t, fixtureB)
	if err := storeA.WithPublishedSnapshot(context.Background(), model.Hash(profileA), "", func(snapshot *sqlite.ReadSnapshot) error {
		if snapshot.Generation.Key.GenerationID != model.UUID(buildingGenerationA) {
			return fmt.Errorf("fresh reader did not resolve newly published generation: %s", snapshot.Generation.Key.GenerationID)
		}
		return nil
	}); err != nil {
		t.Fatalf("fresh reader while previous snapshot remains open: %v", err)
	}

	close(releaseReader)
	select {
	case err := <-readerResult:
		if err != nil {
			t.Fatalf("pinned reader observed an inconsistent snapshot: %v", err)
		}
	case <-time.After(helperWait):
		t.Fatal("pinned reader did not finish after release")
	}

	assertActiveGeneration(t, storeA, profileA, buildingGenerationA)
	assertActiveGeneration(t, storeA, profileB, otherGenerationA)

	storeB, err := sqlite.OpenIndexReadOnly(context.Background(), readOnlyOptions(fixtureB))
	if err != nil {
		t.Fatalf("open read-only index B: %v", err)
	}
	if storeB.WorktreeID() != model.UUID(worktreeB) {
		t.Fatalf("index B owner=%s; want %s", storeB.WorktreeID(), worktreeB)
	}
	assertActiveGeneration(t, storeB, profileA, buildingGenerationB)
	if fixtureA.remote != fixtureB.remote || fixtureA.head != fixtureB.head || fixtureA.base != fixtureB.base {
		t.Fatal("C001 fixture must keep repository remote, HEAD and folder basename identical across worktrees")
	}
	var generationA, generationB sqlite.GenerationRecord
	if err := storeA.WithPublishedSnapshot(context.Background(), model.Hash(profileA), "", func(snapshot *sqlite.ReadSnapshot) error {
		generationA = snapshot.Generation
		return nil
	}); err != nil {
		t.Fatalf("read active generation A for scope identity: %v", err)
	}
	if err := storeB.WithPublishedSnapshot(context.Background(), model.Hash(profileA), "", func(snapshot *sqlite.ReadSnapshot) error {
		generationB = snapshot.Generation
		return nil
	}); err != nil {
		t.Fatalf("read active generation B for scope identity: %v", err)
	}
	if generationA.SnapshotDigest == generationB.SnapshotDigest {
		t.Fatalf("worktrees with different dirty snapshots share digest %s", generationA.SnapshotDigest)
	}
	if digestA, digestB := scopeDigest(t, storeA, generationA), scopeDigest(t, storeB, generationB); digestA == digestB {
		t.Fatalf("distinct worktree/snapshot identities share scope digest %s", digestA)
	}
	if err := storeB.Close(); err != nil {
		t.Fatalf("close index B: %v", err)
	}

	helperA.releaseAndWait(t)
	helperB.releaseAndWait(t)
}

func prepareIndexFixture(t *testing.T, home, worktreeID string, seedOtherProfile bool) indexFixture {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create fixture home: %v", err)
	}
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatalf("open fixture home root: %v", err)
	}
	if err := mceruntime.EnsureWorktreeLockLayout(root, worktreeID); err != nil {
		_ = root.Close()
		t.Fatalf("prepare fixture lock layout: %v", err)
	}
	homeLease, err := mceruntime.AcquireHomeLifecycle(context.Background(), root, mceruntime.LockShared, time.Second)
	if err != nil {
		_ = root.Close()
		t.Fatalf("initialize fixture home lifecycle lock: %v", err)
	}
	worktreeLease, err := mceruntime.AcquireWorktreeLifecycle(context.Background(), root, worktreeID, mceruntime.LockShared, time.Second)
	if err != nil {
		_ = homeLease.Close()
		_ = root.Close()
		t.Fatalf("initialize fixture worktree lifecycle lock: %v", err)
	}
	if err := worktreeLease.Close(); err != nil {
		_ = homeLease.Close()
		_ = root.Close()
		t.Fatalf("close fixture worktree lifecycle lock: %v", err)
	}
	if err := homeLease.Close(); err != nil {
		_ = root.Close()
		t.Fatalf("close fixture home lifecycle lock: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close fixture root: %v", err)
	}
	databasePath := filepath.Join(home, "worktrees", worktreeID, "index.sqlite")
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatalf("create private index directory: %v", err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode); err != nil || !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("enable WAL mode: mode=%q err=%v", journalMode, err)
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve integration test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	for _, migration := range []string{"001.sql", "002.sql"} {
		source, err := os.ReadFile(filepath.Join(repoRoot, "internal", "store", "sqlite", "migrations", "index", migration))
		if err != nil {
			t.Fatalf("read shipped index migration %s: %v", migration, err)
		}
		if _, err := db.Exec(string(source)); err != nil {
			t.Fatalf("apply shipped index migration %s: %v", migration, err)
		}
	}
	incarnation := "99999999-0000-4000-8000-000000000001"
	if worktreeID == worktreeB {
		incarnation = "99999999-0000-4000-8000-000000000002"
	}
	if _, err := db.Exec(`INSERT INTO store_identity(singleton,worktree_id,incarnation) VALUES(1,?,?)`, worktreeID, incarnation); err != nil {
		t.Fatalf("bind fixture index to its worktree identity: %v", err)
	}
	primaryOld, primaryBuilding := oldGenerationA, buildingGenerationA
	if !seedOtherProfile {
		primaryOld, primaryBuilding = oldGenerationB, buildingGenerationB
	}
	oldDigest, oldArtifact := "1", "a"
	buildingDigest, buildingArtifact := "2", "b"
	if !seedOtherProfile {
		oldDigest, oldArtifact = "4", "d"
		buildingDigest, buildingArtifact = "5", "e"
	}
	insertFixtureGeneration(t, db, primaryOld, profileA, oldDigest, oldArtifact, true)
	insertFixtureGeneration(t, db, primaryBuilding, profileA, buildingDigest, buildingArtifact, false)
	if seedOtherProfile {
		insertFixtureGeneration(t, db, otherGenerationA, profileB, "3", "c", true)
		if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id) VALUES(?,?)`, profileB, otherGenerationA); err != nil {
			t.Fatalf("seed independent profile pointer: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id) VALUES(?,?)`, profileA, primaryOld); err != nil {
		t.Fatalf("seed worktree primary profile pointer: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded fixture database: %v", err)
	}
	return indexFixture{home: home, database: databasePath, worktree: worktreeID, profileA: profileA, profileB: profileB,
		remote: fixtureRemote, head: fixtureHead, base: fixtureBase}
}

func insertFixtureGeneration(t *testing.T, db *sql.DB, generationID, profileID, digestByte, artifactByte string, ready bool) {
	t.Helper()
	snapshotDigest := strings.Repeat(digestByte, 64)
	artifactDigest := strings.Repeat(artifactByte, 64)
	manifest, err := sqlite.EncodeStoredCaptureManifestRef(model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + artifactDigest + ".json",
		ByteLength: 128, ArtifactSHA256: model.Hash(artifactDigest), SnapshotDigest: model.Hash(snapshotDigest),
	})
	if err != nil {
		t.Fatalf("encode fixture manifest reference: %v", err)
	}
	ownerID := "50000000-0000-4000-8000-000000000001"
	if generationID == buildingGenerationA || generationID == buildingGenerationB {
		ownerID = "50000000-0000-4000-8000-000000000002"
	}
	if _, err := db.Exec(`INSERT INTO generations(
		generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
		provider_fingerprint,manifest_json,created_at,completed_at
	) VALUES(?,?,?,'BUILDING',?,?,1,?,?,?,NULL)`,
		generationID, profileID, snapshotDigest, ownerID, strings.Repeat("d", 64), strings.Repeat("e", 64),
		string(manifest), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed BUILDING generation %s: %v", generationID, err)
	}
	if _, err := db.Exec(`INSERT INTO files(generation_id,path,content_hash,kind,status,byte_length,package_key,generated,reason_code)
		VALUES(?,?,NULL,'test_marker','missing',NULL,NULL,0,'test_missing')`, generationID, generationID); err != nil {
		t.Fatalf("seed generation marker %s: %v", generationID, err)
	}
	if ready {
		if _, err := db.Exec(`UPDATE generations SET state='READY',completed_at=? WHERE generation_id=?`, time.Now().UTC().Format(time.RFC3339Nano), generationID); err != nil {
			t.Fatalf("mark fixture generation READY %s: %v", generationID, err)
		}
	}
}

// publishFixtureGeneration uses test-only SQL because product code intentionally
// exposes no raw writer here. It exercises the same READY-plus-active-pointer
// transaction boundary against the shipped guards without adding a production API.
func publishFixtureGeneration(databasePath, profileID, generationID string) error {
	if databasePath == "" || profileID == "" || generationID == "" {
		return errors.New("publish helper is missing its fixture identity")
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.Exec(`UPDATE generations SET state='READY',completed_at=? WHERE generation_id=? AND profile_id=? AND state='BUILDING'`,
		time.Now().UTC().Format(time.RFC3339Nano), generationID, profileID)
	if err != nil {
		return fmt.Errorf("mark staged generation READY: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return fmt.Errorf("publish expected one BUILDING generation: rows=%d err=%v", rows, err)
	}
	if _, err := tx.Exec(`INSERT INTO active_generations(profile_id,generation_id) VALUES(?,?)
		ON CONFLICT(profile_id) DO UPDATE SET generation_id=excluded.generation_id`, profileID, generationID); err != nil {
		return fmt.Errorf("switch active profile pointer: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit generation and pointer together: %w", err)
	}
	committed = true
	return nil
}

func readOnlyOptions(fixture indexFixture) sqlite.Options {
	return sqlite.Options{Path: fixture.database, HomePath: fixture.home, WorktreeID: model.UUID(fixture.worktree),
		BusyTimeoutMS: 2_000, LockWaitMS: 2_000}
}

func assertActiveGeneration(t *testing.T, store *sqlite.Store, profileID, expected string) {
	t.Helper()
	err := store.WithPublishedSnapshot(context.Background(), model.Hash(profileID), "", func(snapshot *sqlite.ReadSnapshot) error {
		if got := string(snapshot.Generation.Key.GenerationID); got != expected {
			return fmt.Errorf("profile %s active generation=%s; want %s", profileID, got, expected)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read active generation for profile %s: %v", profileID, err)
	}
}

func scopeDigest(t *testing.T, store *sqlite.Store, generation sqlite.GenerationRecord) model.Hash {
	t.Helper()
	vector := model.ScopeVector{
		Kind:               "single",
		UnavailableMembers: []model.UnavailableMember{},
		CatalogIncarnation: model.UUID("88888888-0000-4000-8000-000000000001"),
		Members: []model.ScopeMember{{
			WorktreeID: store.WorktreeID(), ProfileID: generation.Key.ProfileID,
			GenerationID: generation.Key.GenerationID, SnapshotDigest: generation.SnapshotDigest,
			PolicyDigest: generation.PolicyDigest, PolicyEpoch: generation.PolicyEpoch,
			BindingRevision: 1, RecordEpoch: 1, IndexIncarnation: store.Incarnation(),
			RecordsIncarnation: model.UUID("77777777-0000-4000-8000-000000000001"),
		}},
	}
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatalf("compute worktree scope digest: %v", err)
	}
	return digest
}

func assertWriterBusy(t *testing.T, fixture indexFixture) {
	t.Helper()
	root, err := mceruntime.OpenRoot(fixture.home)
	if err != nil {
		t.Fatalf("open lock probe root: %v", err)
	}
	defer root.Close()
	if _, err := mceruntime.AcquireWorktreeWriter(context.Background(), root, fixture.worktree, 0); !errors.Is(err, mceruntime.ErrLockBusy) {
		t.Fatalf("parent acquired active child writer for worktree %s: %v", fixture.worktree, err)
	}
}

func startConcurrencyHelper(t *testing.T, fixture indexFixture, mode, profileID, generationID string) *concurrencyHelper {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestConcurrencyHelperProcess$")
	command.Env = append(os.Environ(), helperModeEnv+"="+mode, helperHomeEnv+"="+fixture.home,
		helperDBEnv+"="+fixture.database, helperWorktreeEnv+"="+fixture.worktree,
		helperProfileEnv+"="+profileID, helperGenerationEnv+"="+generationID)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open helper stdout: %v", err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("open helper stdin: %v", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lines <- strings.TrimSpace(line)
	}()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	helper := &concurrencyHelper{command: command, stdin: stdin, lines: lines, done: done}
	t.Cleanup(func() { helper.stop() })
	return helper
}

func (helper *concurrencyHelper) expectLine(t *testing.T, expected string) {
	t.Helper()
	select {
	case line := <-helper.lines:
		if line != expected {
			helper.stop()
			t.Fatalf("helper handshake=%q; want %q", line, expected)
		}
	case err := <-helper.done:
		t.Fatalf("helper exited before handshake %q: %v", expected, err)
	case <-time.After(helperWait):
		helper.stop()
		t.Fatalf("helper did not report handshake %q within %s", expected, helperWait)
	}
}

func (helper *concurrencyHelper) releaseAndWait(t *testing.T) {
	t.Helper()
	_, _ = io.WriteString(helper.stdin, "release\n")
	_ = helper.stdin.Close()
	select {
	case err := <-helper.done:
		if err != nil {
			t.Fatalf("helper process failed after release: %v", err)
		}
	case <-time.After(helperWait):
		helper.stop()
		t.Fatalf("helper did not exit after release within %s", helperWait)
	}
}

func (helper *concurrencyHelper) stop() {
	if helper == nil || helper.command == nil || helper.command.ProcessState != nil {
		return
	}
	_ = helper.command.Process.Kill()
	select {
	case <-helper.done:
	case <-time.After(helperWait):
	}
}
