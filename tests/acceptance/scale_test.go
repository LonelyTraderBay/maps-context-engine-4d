package acceptance_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	resourceexec "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/exec"
	"github.com/LonelyTraderBay/maps-context-engine-4d/tools/bench/scale"
	_ "modernc.org/sqlite"
)

func TestScaleAcceptanceWorkload(t *testing.T) {
	const workerRequestName = "scale-worker-request.json"
	if _, err := os.Stat(workerRequestName); err == nil {
		runScaleAcceptanceFixture(t, workerRequestName)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect scale worker request: %v", err)
	}
	runScaleAcceptanceControlled(t)
}

type scaleWorkerRequest struct {
	Cohort          scale.Cohort `json:"cohort"`
	Mode            scale.Mode   `json:"mode"`
	Repo            string       `json:"repo"`
	Secondary       string       `json:"secondary_worktree"`
	SecondaryParent string       `json:"secondary_parent"`
	OutputRoot      string       `json:"output_root"`
	Template        string       `json:"git_template"`
	Metrics         string       `json:"metrics"`
}

func runScaleAcceptanceControlled(t *testing.T) {
	t.Helper()
	cohort := scale.Cohort(strings.TrimSpace(os.Getenv("MCE4D_SCALE_COHORT")))
	if cohort == "" {
		cohort = scale.CohortS
	}
	mode := scale.Mode(strings.TrimSpace(os.Getenv("MCE4D_SCALE_MODE")))
	if mode == "" {
		mode = scale.ModeCombined
	}
	plan, err := scale.NewPlan(cohort, mode)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	repo := filepath.Join(workDir, "repo")
	secondaryParent := filepath.Join(workDir, "worktrees")
	secondary := filepath.Join(secondaryParent, "retained")
	outputRoot := filepath.Join(workDir, "cache")
	template := filepath.Join(workDir, "empty-git-template")
	metricsPath := filepath.Join(workDir, "scale-metrics.json")
	for _, path := range []string{repo, secondaryParent, outputRoot, template} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("prepare isolated scale destination %q: %v", path, err)
		}
	}
	request := scaleWorkerRequest{
		Cohort: cohort, Mode: mode, Repo: repo, Secondary: secondary, SecondaryParent: secondaryParent,
		OutputRoot: outputRoot, Template: template, Metrics: metricsPath,
	}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(workDir, "scale-worker-request.json")
	if err := os.WriteFile(requestPath, requestBytes, 0o600); err != nil {
		t.Fatalf("write controlled scale worker request: %v", err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate acceptance test executable: %v", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatalf("resolve acceptance test executable: %v", err)
	}
	timeout := scaleCohortTimeout(cohort)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sqliteBudget := plan.FileCount * 8192
	const gitMetadataReserve = 64 * scale.MiB
	storagePlan := &resourceexec.StoragePlan{
		Policy: resourceexec.ResourceAdmissionPolicy{
			BlockThresholdBytes: 0, WarningThresholdBytes: plan.Limits.VolumeWarning,
			SafetyReserveBytes: plan.Limits.VolumeReserve, MaxObservationAge: time.Minute,
		},
		Destinations: []resourceexec.StorageDestination{
			{Name: "synthetic-source", Path: repo, PeakRemainingBytes: plan.SourceBytes + gitMetadataReserve},
			{Name: "retained-worktree", Path: secondaryParent, PeakRemainingBytes: scale.MiB + gitMetadataReserve},
			{Name: "synthetic-cache", Path: outputRoot, PeakRemainingBytes: plan.ManifestUpperBound + sqliteBudget},
		},
	}
	controlledStarted := time.Now()
	result, runErr := resourceexec.Run(ctx, resourceexec.Spec{
		Path: executable, Args: []string{"-test.timeout=" + scaleTestBinaryTimeout(cohort).String(), "-test.run=^TestScaleAcceptanceWorkload$", "-test.v"}, Dir: workDir,
		Env: controlledTestEnvironment(), MaxStdoutBytes: 4 << 20, MaxStderrBytes: 4 << 20,
		RSSBudgetBytes: plan.Limits.RSSBudgetBytes, MonitorInterval: 250 * time.Millisecond, StoragePlan: storagePlan,
	})
	if errors.Is(runErr, resourceexec.ErrStorageBudget) {
		t.Skipf("unsupported %s fixture envelope: controlled storage preflight blocked before materialization: %+v", cohort, result.StorageAdmission)
	}
	if runErr != nil {
		t.Fatalf("controlled %s scale worker failed (reason=%s, RSS=%d, storage=%s): %v\nstdout:\n%s\nstderr:\n%s",
			cohort, result.TerminationReason, result.PeakRSSBytes, result.StoragePreflight, runErr, result.Stdout, result.Stderr)
	}
	metricsBytes, err := os.ReadFile(metricsPath)
	if err != nil {
		t.Fatalf("read measured scale metrics: %v\nworker stdout:\n%s", err, result.Stdout)
	}
	var metrics map[string]any
	if err := json.Unmarshal(metricsBytes, &metrics); err != nil {
		t.Fatalf("decode measured scale metrics: %v", err)
	}
	metrics["process_tree_rss_peak_bytes"] = result.PeakRSSBytes
	metrics["storage_preflight"] = result.StoragePreflight
	metrics["storage_admission"] = result.StorageAdmission
	metrics["storage_warnings"] = result.Warnings
	metrics["controlled_process_tree_elapsed_ms"] = time.Since(controlledStarted).Milliseconds()
	metrics["worker_timeout_seconds"] = int64(timeout / time.Second)
	metrics["test_binary_timeout_seconds"] = int64(scaleTestBinaryTimeout(cohort) / time.Second)
	metrics["sealed_index_deadline_seconds"] = plan.Limits.IndexDeadlineSecs
	metrics["sealed_limits"] = plan.Limits
	metrics["benchmark_scope"] = "synthetic inventory/capture and SQLite facts fixture; not product index support evidence"
	if cohort == scale.CohortXL {
		elapsed := time.Since(controlledStarted)
		withinSealedDeadline := elapsed <= time.Duration(plan.Limits.IndexDeadlineSecs)*time.Second
		metrics["xl_fixture_within_sealed_deadline"] = withinSealedDeadline
		if withinSealedDeadline {
			metrics["xl_fixture_deadline_state"] = "WITHIN_SEALED_XL_V1_DEADLINE"
		} else {
			metrics["xl_fixture_deadline_state"] = "EXCEEDS_SEALED_XL_V1_DEADLINE"
		}
	}
	encodedMetrics, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SCALE_METRIC %s", encodedMetrics)
}

func runScaleAcceptanceFixture(t *testing.T, requestPath string) {
	t.Helper()
	requestBytes, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read scale worker request: %v", err)
	}
	var request scaleWorkerRequest
	if err := json.Unmarshal(requestBytes, &request); err != nil {
		t.Fatalf("decode scale worker request: %v", err)
	}
	plan, err := scale.NewPlan(request.Cohort, request.Mode)
	if err != nil {
		t.Fatalf("invalid controlled scale plan: %v", err)
	}
	repo := request.Repo
	outputRoot := request.OutputRoot
	gitBinary, err := osexec.LookPath("git")
	if err != nil {
		t.Skip("Git executable is unavailable for the isolated scale fixture")
	}
	init := osexec.Command(gitBinary, "init", "--quiet", "--template="+request.Template, repo)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initialize synthetic scale worktree: %v: %s", err, output)
	}
	baseline := osexec.Command(gitBinary, "-C", repo, "-c", "user.name=Scale fixture", "-c", "user.email=scale-fixture@example.invalid",
		"commit", "--allow-empty", "--quiet", "-m", "scale-fixture-baseline")
	if output, err := baseline.CombinedOutput(); err != nil {
		t.Fatalf("create isolated baseline commit for retained worktree: %v: %s", err, output)
	}
	addWorktree := osexec.Command(gitBinary, "-C", repo, "worktree", "add", "--detach", "--quiet", request.Secondary, "HEAD")
	if output, err := addWorktree.CombinedOutput(); err != nil {
		t.Fatalf("add isolated retained Git worktree: %v: %s", err, output)
	}

	root, err := mceruntime.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	caseMode, err := root.CaseMode()
	if err != nil || caseMode == paths.CaseUnknown {
		t.Fatalf("scale worktree case mode is not measurable: mode=%d err=%v", caseMode, err)
	}
	volumeID, _, err := root.Identity()
	if err != nil || volumeID == "" {
		t.Fatalf("scale worktree volume identity is not measurable: id=%q err=%v", volumeID, err)
	}
	availableBefore, err := root.AvailableBytes()
	if err != nil {
		t.Fatal(err)
	}
	outputVolume, _, err := func() (string, string, error) {
		opened, openErr := mceruntime.OpenRoot(outputRoot)
		if openErr != nil {
			return "", "", openErr
		}
		defer opened.Close()
		return opened.Identity()
	}()
	if err != nil || outputVolume != volumeID {
		t.Fatalf("synthetic worktree and cache fixture must share the measured volume: %q/%q err=%v", volumeID, outputVolume, err)
	}
	secondaryRoot, err := mceruntime.OpenRoot(request.Secondary)
	if err != nil {
		t.Fatalf("open retained Git worktree: %v", err)
	}
	defer secondaryRoot.Close()
	secondaryVolume, _, err := secondaryRoot.Identity()
	if err != nil || secondaryVolume != volumeID {
		t.Fatalf("retained worktree and primary worktree must share the measured volume: %q/%q err=%v", volumeID, secondaryVolume, err)
	}
	policyRules, err := policy.New(nil, caseMode)
	if err != nil {
		t.Fatal(err)
	}
	secondaryMode, err := secondaryRoot.CaseMode()
	if err != nil || secondaryMode != caseMode {
		t.Fatalf("retained worktree case mode differs: primary=%d secondary=%d err=%v", caseMode, secondaryMode, err)
	}

	started := time.Now()
	materializationStarted := time.Now()
	fileWriter := newScaleFileWriter()
	if err := fileWriter.WriteFile(filepath.Join(request.Secondary, "retained-source.txt"), plan.FileCount+1, scale.MiB); err != nil {
		t.Fatalf("write retained-worktree scale sentinel: %v", err)
	}
	wantSourceBytes := int64(0)
	for index := int64(0); index < plan.FileCount; index++ {
		fileBytes, err := plan.FileBytes(index)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(repo, "src", fmt.Sprintf("dir-%03d", index/plan.FanoutFilesPerDir), fmt.Sprintf("file-%06d.txt", index))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create fixture fanout directory: %v", err)
		}
		if err := fileWriter.WriteFile(path, index, fileBytes); err != nil {
			t.Fatalf("write synthetic source file %d: %v", index, err)
		}
		wantSourceBytes += fileBytes
	}
	if err := plan.VerifyOracle(plan.FileCount, wantSourceBytes); err != nil {
		t.Fatalf("source generator count/byte oracle failed: %v", err)
	}
	materializationMillis := time.Since(materializationStarted).Milliseconds()

	initialScanStarted := time.Now()
	initialSnapshot, err := inventory.Scan(context.Background(), inventory.Options{
		Root: repo, GitExecutable: gitBinary, Policy: policyRules, CaseMode: caseMode,
	})
	if err != nil {
		t.Fatalf("initial inventory scan of scale fixture: %v", err)
	}
	if int64(len(initialSnapshot.Files)) != plan.FileCount || initialSnapshot.Coverage.State != inventory.CoverageComplete {
		t.Fatalf("initial inventory count/coverage=%d/%s; want %d/complete", len(initialSnapshot.Files), initialSnapshot.Coverage.State, plan.FileCount)
	}
	secondarySnapshot, err := inventory.Scan(context.Background(), inventory.Options{
		Root: request.Secondary, GitExecutable: gitBinary, Policy: policyRules, CaseMode: secondaryMode,
	})
	if err != nil {
		t.Fatalf("inventory scan of retained Git worktree: %v", err)
	}
	if len(secondarySnapshot.Files) != 1 || secondarySnapshot.Files[0].Path != "retained-source.txt" || secondarySnapshot.Files[0].SizeBytes != scale.MiB {
		t.Fatalf("retained worktree inventory=%+v; want one 1 MiB independent source", secondarySnapshot.Files)
	}
	initialScanMillis := time.Since(initialScanStarted).Milliseconds()
	churnStarted := time.Now()
	for index := int64(0); index < plan.ChurnFiles; index++ {
		item := initialSnapshot.Files[index]
		original := filepath.Join(repo, filepath.FromSlash(item.Path))
		info, err := os.Stat(original)
		if err != nil {
			t.Fatalf("stat churn source %q: %v", item.Path, err)
		}
		if err := os.Remove(original); err != nil {
			t.Fatalf("remove churn source %q: %v", item.Path, err)
		}
		replacement := filepath.Join(repo, "src", "churn", fmt.Sprintf("added-%06d.txt", index))
		if err := os.MkdirAll(filepath.Dir(replacement), 0o700); err != nil {
			t.Fatalf("create churn replacement directory: %v", err)
		}
		if err := fileWriter.WriteFile(replacement, plan.FileCount+index, info.Size()); err != nil {
			t.Fatalf("write churn replacement %d: %v", index, err)
		}
	}
	churnMillis := time.Since(churnStarted).Milliseconds()
	postChurnScanStarted := time.Now()
	snapshot, err := inventory.Scan(context.Background(), inventory.Options{
		Root: repo, GitExecutable: gitBinary, Policy: policyRules, CaseMode: caseMode,
	})
	if err != nil {
		t.Fatalf("post-churn inventory scan of scale fixture: %v", err)
	}
	if int64(len(snapshot.Files)) != plan.FileCount || snapshot.Coverage.State != inventory.CoverageComplete {
		t.Fatalf("post-churn inventory count/coverage=%d/%s; want %d/complete", len(snapshot.Files), snapshot.Coverage.State, plan.FileCount)
	}
	churn := inventory.CompareMembership(initialSnapshot, snapshot)
	if int64(len(churn.Added)) != plan.ChurnFiles || int64(len(churn.Removed)) != plan.ChurnFiles || len(churn.Changed) != 0 {
		t.Fatalf("membership churn added/removed/changed=%d/%d/%d; want %d/%d/0", len(churn.Added), len(churn.Removed), len(churn.Changed), plan.ChurnFiles, plan.ChurnFiles)
	}
	postChurnScanMillis := time.Since(postChurnScanStarted).Milliseconds()

	hashStarted := time.Now()
	filesystemCount, filesystemBytes, manifestFiles := independentTreeOracleAndManifest(t, repo, snapshot.Files)
	if err := plan.VerifyOracle(filesystemCount, filesystemBytes); err != nil {
		t.Fatalf("independent filesystem count/byte oracle failed: %v", err)
	}
	if filesystemCount != int64(len(snapshot.Files)) {
		t.Fatalf("inventory and independent filesystem counts differ: %d/%d", len(snapshot.Files), filesystemCount)
	}

	hashMillis := time.Since(hashStarted).Milliseconds()
	manifestStarted := time.Now()
	manifest := model.CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.SHA256([]byte("scale-profile-S")), PolicyDigest: model.SHA256([]byte("scale-policy-S")),
		ProviderDigest: model.SHA256([]byte("scale-provider-S")), Files: manifestFiles,
		DependencyInputDigests: []model.Hash{},
	}
	manifest.SnapshotDigest, err = manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatalf("compute capture manifest oracle: %v", err)
	}
	manifestPath := filepath.Join(outputRoot, "capture-manifest.json")
	manifestFile, err := os.Create(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestSummary, writeErr := model.WriteCaptureManifestJSON1Stream(manifestFile, model.CaptureManifestSliceSources(manifest))
	closeManifestErr := manifestFile.Close()
	if writeErr != nil || closeManifestErr != nil {
		t.Fatalf("write canonical capture manifest: write=%v close=%v", writeErr, closeManifestErr)
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil || manifestInfo.Size() != manifestSummary.ByteLength || manifestSummary.FileCount != plan.FileCount {
		t.Fatalf("manifest byte/count totals diverged: summary=%+v stat=%v err=%v", manifestSummary, manifestInfo, err)
	}
	if manifestSummary.ByteLength > plan.Limits.MaxManifestBytes {
		t.Fatalf("manifest exceeded sealed cap: %d > %d", manifestSummary.ByteLength, plan.Limits.MaxManifestBytes)
	}
	manifestMillis := time.Since(manifestStarted).Milliseconds()

	sqliteStarted := time.Now()
	dbPath := filepath.Join(outputRoot, "scale-index.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable SQLite WAL for scale fixture: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatalf("disable fixture auto-checkpoint while measuring WAL: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE captured_files(path TEXT PRIMARY KEY, size_bytes INTEGER NOT NULL, digest TEXT NOT NULL)`); err != nil {
		t.Fatalf("create bounded scale facts fixture: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare(`INSERT INTO captured_files(path,size_bytes,digest) VALUES(?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range manifestFiles {
		if _, err := statement.Exec(item.Path, *item.ByteLength, string(*item.ContentHash)); err != nil {
			t.Fatalf("insert measured scale fact %q: %v", item.Path, err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var storedCount, storedBytes int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes),0) FROM captured_files`).Scan(&storedCount, &storedBytes); err != nil {
		t.Fatal(err)
	}
	if storedCount != filesystemCount || storedBytes != filesystemBytes {
		t.Fatalf("SQLite independent oracle=%d/%d; want %d/%d", storedCount, storedBytes, filesystemCount, filesystemBytes)
	}
	walInfo, err := os.Stat(dbPath + "-wal")
	if err != nil {
		t.Fatalf("measure SQLite WAL before checkpoint: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint scale fixture WAL: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	availableAfter, err := root.AvailableBytes()
	if err != nil {
		t.Fatal(err)
	}
	databaseInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("measure synthetic SQLite database size: %v", err)
	}
	cacheBytes := manifestInfo.Size() + databaseInfo.Size() + walInfo.Size()
	if cacheBytes > plan.Limits.CacheBytes {
		t.Fatalf("measured fixture cache exceeded sealed cap: %d > %d", cacheBytes, plan.Limits.CacheBytes)
	}
	sqliteMillis := time.Since(sqliteStarted).Milliseconds()
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planDigest := sha256.Sum256(planBytes)
	metrics := map[string]any{
		"cohort": string(plan.Cohort), "mode": string(plan.Mode), "result": "PASS", "source_files": filesystemCount,
		"source_bytes": filesystemBytes, "manifest_bytes": manifestSummary.ByteLength,
		"manifest_sha256": manifestSummary.ArtifactSHA256, "sqlite_wal_peak_bytes": walInfo.Size(),
		"sqlite_database_file":   "synthetic scale facts fixture; not production index schema",
		"available_bytes_before": availableBefore, "available_bytes_after": availableAfter,
		"volume_id": volumeID, "cache_bytes_measured": cacheBytes,
		"primary_scale_worktrees": plan.Worktrees, "physical_git_worktrees": 2,
		"retained_worktree_files": len(secondarySnapshot.Files),
		"retained_worktree_bytes": secondarySnapshot.Files[0].SizeBytes,
		"churn_added":             len(churn.Added), "churn_removed": len(churn.Removed), "churn_changed": len(churn.Changed),
		"elapsed_ms": time.Since(started).Milliseconds(), "plan_sha256": hex.EncodeToString(planDigest[:]),
		"runtime_os": runtime.GOOS, "runtime_arch": runtime.GOARCH, "logical_processors": runtime.NumCPU(),
		"phase_ms": map[string]int64{
			"materialize": materializationMillis, "initial_inventory": initialScanMillis,
			"churn": churnMillis, "post_churn_inventory": postChurnScanMillis,
			"independent_oracle_and_content_hash": hashMillis, "capture_manifest": manifestMillis,
			"sqlite_fixture": sqliteMillis,
		},
	}
	encodedMetrics, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SCALE_METRIC %s", encodedMetrics)
	if strings.Contains(string(encodedMetrics), "NOT_RUN") {
		t.Fatal("measured sample was mislabeled as not run")
	}
	if err := os.WriteFile(request.Metrics, encodedMetrics, 0o600); err != nil {
		t.Fatalf("save measured scale metrics: %v", err)
	}
}

func scaleCohortTimeout(cohort scale.Cohort) time.Duration {
	switch cohort {
	case scale.CohortS:
		return 15 * time.Minute
	case scale.CohortM:
		return 30 * time.Minute
	case scale.CohortL:
		return 90 * time.Minute
	case scale.CohortXL:
		// The bounded L sample projects above XL-V1's two-hour sealed target.
		// Keep that target unchanged and use this finite ceiling only to finish
		// the synthetic benchmark and report the comparison explicitly.
		return 4 * time.Hour
	default:
		return time.Duration(scale.DefaultIndexDeadlineSecs) * time.Second
	}
}

func scaleTestBinaryTimeout(cohort scale.Cohort) time.Duration {
	return scaleCohortTimeout(cohort) + 5*time.Minute
}

func TestScaleCohortDeadlinesAreFinite(t *testing.T) {
	tests := []struct {
		cohort scale.Cohort
		want   time.Duration
	}{
		{scale.CohortS, 15 * time.Minute},
		{scale.CohortM, 30 * time.Minute},
		{scale.CohortL, 90 * time.Minute},
		{scale.CohortXL, 4 * time.Hour},
	}
	for _, test := range tests {
		t.Run(string(test.cohort), func(t *testing.T) {
			if got := scaleCohortTimeout(test.cohort); got != test.want {
				t.Fatalf("worker timeout=%s; want finite %s", got, test.want)
			}
			if got := scaleTestBinaryTimeout(test.cohort); got != test.want+5*time.Minute {
				t.Fatalf("worker test-binary timeout=%s; want worker deadline plus five minutes (%s)", got, test.want+5*time.Minute)
			}
		})
	}
}

func controlledTestEnvironment() map[string]string {
	environment := make(map[string]string)
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			environment[key] = value
		}
	}
	return environment
}

type scaleFileWriter struct {
	chunk []byte
}

func newScaleFileWriter() *scaleFileWriter {
	chunk := make([]byte, 64*1024)
	for index := range chunk {
		chunk[index] = 0x5a
	}
	return &scaleFileWriter{chunk: chunk}
}

func (writer *scaleFileWriter) WriteFile(path string, seed int64, size int64) error {
	if writer == nil || len(writer.chunk) == 0 || size < 0 {
		return errors.New("invalid scale file writer input")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer.chunk[0] = byte(seed%251 + 1)
	for remaining := size; remaining > 0; {
		length := int64(len(writer.chunk))
		if remaining < length {
			length = remaining
		}
		written, err := file.Write(writer.chunk[:int(length)])
		if err != nil {
			return errors.Join(err, file.Close())
		}
		if written != int(length) {
			return errors.Join(io.ErrShortWrite, file.Close())
		}
		remaining -= int64(written)
	}
	return file.Close()
}

func TestScaleRetentionAdmissionCountsSharedVolumeReservationsOnce(t *testing.T) {
	now := time.Now()
	const volumeID = "synthetic-volume-retention"
	sharedReservation := resourceexec.StorageReservation{ID: "home-durable-job-1", RemainingBytes: 100}
	destinations := []resourceexec.StorageDestination{
		{Name: "worktree-a-profile-a", VolumeID: volumeID, PeakRemainingBytes: 200, OtherJobReservations: []resourceexec.StorageReservation{sharedReservation}},
		{Name: "worktree-a-profile-b", VolumeID: volumeID, PeakRemainingBytes: 150, OtherJobReservations: []resourceexec.StorageReservation{sharedReservation}},
		{Name: "worktree-a-profile-c-building", VolumeID: volumeID, PeakRemainingBytes: 250, OtherJobReservations: []resourceexec.StorageReservation{sharedReservation}},
		{Name: "worktree-b-profile-a", VolumeID: volumeID, PeakRemainingBytes: 200, OtherJobReservations: []resourceexec.StorageReservation{sharedReservation}},
	}
	policy := resourceexec.ResourceAdmissionPolicy{
		BlockThresholdBytes: 0, WarningThresholdBytes: 1500,
		SafetyReserveBytes: 100, MaxObservationAge: time.Minute,
	}
	observe := func(available int64) []resourceexec.ResourceVolumeObservation {
		return []resourceexec.ResourceVolumeObservation{{VolumeID: volumeID, AvailableBytes: available, ObservedAt: now, Scope: "three-profiles-two-worktrees"}}
	}
	result, err := resourceexec.EvaluateResourceAdmission(destinations, observe(1000), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].RequiredBytes != 1000 || !result[0].Admitted {
		t.Fatalf("multi-profile shared-volume admission=%+v; want one 1000-byte admitted volume", result)
	}
	result, err = resourceexec.EvaluateResourceAdmission(destinations, observe(999), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].RequiredBytes != 1000 || result[0].Admitted {
		t.Fatalf("one-byte-short multi-profile admission=%+v; want reject", result)
	}
	if _, err := resourceexec.EvaluateResourceAdmission(destinations, nil, policy); !errors.Is(err, resourceexec.ErrResourceAdmission) {
		t.Fatalf("missing shared-volume measurement error=%v, want ErrResourceAdmission", err)
	}
}

func independentTreeOracleAndManifest(t *testing.T, root string, expected []inventory.File) (int64, int64, []model.ManifestFile) {
	t.Helper()
	var files, sourceBytes int64
	manifestFiles := make([]model.ManifestFile, 0, len(expected))
	var buffer [64 * 1024]byte
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture contains a non-regular source: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sourcePath := filepath.ToSlash(relative)
		if files >= int64(len(expected)) || expected[files].Path != sourcePath {
			return fmt.Errorf("independent filesystem membership differs at %q (inventory index %d)", sourcePath, files)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open independent source %q: %w", sourcePath, err)
		}
		hash := sha256.New()
		var readBytes int64
		for {
			read, readErr := file.Read(buffer[:])
			if read > 0 {
				_, _ = hash.Write(buffer[:read])
				readBytes += int64(read)
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					err = readErr
				}
				break
			}
			if read == 0 {
				err = io.ErrNoProgress
				break
			}
		}
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("read independent source %q: read=%v close=%v", sourcePath, err, closeErr)
		}
		if readBytes != info.Size() {
			return fmt.Errorf("independent source size changed: path=%q read=%d stat=%d", sourcePath, readBytes, info.Size())
		}
		contentHash := model.Hash(hex.EncodeToString(hash.Sum(nil)))
		byteLength := readBytes
		manifestFiles = append(manifestFiles, model.ManifestFile{
			Path: sourcePath, Status: "captured", ContentHash: &contentHash, ByteLength: &byteLength,
		})
		files++
		sourceBytes += readBytes
		return nil
	})
	if err != nil {
		t.Fatalf("independent source-tree/hash oracle: %v", err)
	}
	if files != int64(len(expected)) {
		t.Fatalf("independent filesystem count %d differs from inventory count %d", files, len(expected))
	}
	return files, sourceBytes, manifestFiles
}
