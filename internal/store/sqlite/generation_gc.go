package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const generationGCModuleLeafBatch = 256

var (
	ErrGenerationGCPlanStale    = errors.New("SQLite generation GC plan is stale")
	ErrGenerationNotCollectable = errors.New("SQLite generation is not eligible for collection")
)

type GenerationGCItem struct {
	GenerationID model.UUID      `json:"generation_id"`
	ProfileID    model.Hash      `json:"profile_id"`
	State        GenerationState `json:"state"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`
}

// GenerationGCPlan is a read-only snapshot of retention and terminal rows
// eligible for removal. Collection rechecks the exact candidate set under the
// worktree writer lease before deleting anything.
type GenerationGCPlan struct {
	WorktreeID     model.UUID         `json:"worktree_id"`
	Incarnation    model.UUID         `json:"incarnation"`
	KeepPerProfile int64              `json:"keep_per_profile"`
	ObservedAt     time.Time          `json:"observed_at"`
	Candidates     []GenerationGCItem `json:"candidates"`
}

type GenerationGCResult struct {
	CollectedGenerations int64 `json:"collected_generations"`
	DeletedContentBlobs  int64 `json:"deleted_content_blobs"`
	AlreadyMissing       int64 `json:"already_missing"`
}

type GenerationGCOptions struct {
	Store          Options
	KeepPerProfile int64
	Apply          bool
}

type GenerationGCReport struct {
	Plan    GenerationGCPlan
	Applied bool
	Result  *GenerationGCResult
}

// RunGenerationGC plans against a read-only store by default. Applying a plan
// requires Apply=true and revalidates it under the writer lease before changes.
func RunGenerationGC(ctx context.Context, options GenerationGCOptions) (report GenerationGCReport, err error) {
	if ctx == nil || options.KeepPerProfile < 1 || options.KeepPerProfile > model.MaxSafeInteger {
		return report, fmt.Errorf("invalid generation GC request: %w", ErrInvalidIndex)
	}
	var store *Store
	if options.Apply {
		store, err = openIndex(ctx, options.Store)
	} else {
		store, err = OpenIndexReadOnly(ctx, options.Store)
	}
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	report.Plan, err = store.PlanGenerationGC(ctx, options.KeepPerProfile)
	if err != nil {
		return report, err
	}
	if !options.Apply {
		return report, nil
	}
	result, collectErr := store.CollectGenerationGC(ctx, report.Plan)
	if collectErr != nil {
		return report, collectErr
	}
	report.Applied = true
	report.Result = &result
	return report, nil
}

// PlanGenerationGC reports published generations outside the requested
// per-profile retention, plus terminal failed/cancelled rows. BUILDING rows are
// never selected by age or TTL and remain available for owner-aware recovery.
func (store *Store) PlanGenerationGC(ctx context.Context, keepPerProfile int64) (GenerationGCPlan, error) {
	var empty GenerationGCPlan
	if store == nil || store.readers == nil || ctx == nil || keepPerProfile < 1 || keepPerProfile > model.MaxSafeInteger {
		return empty, fmt.Errorf("invalid generation GC planning request: %w", ErrInvalidIndex)
	}
	if token, ok := ctx.Value(generationWriteContextKey{}).(*generationWriteToken); ok && token.store == store {
		if done, active := token.enter(); active {
			defer done()
			return store.planGenerationGC(ctx, keepPerProfile)
		}
	}
	store.writeGate.RLock()
	defer store.writeGate.RUnlock()
	if store.closed {
		return empty, fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
	}
	return store.planGenerationGC(ctx, keepPerProfile)
}

func (store *Store) planGenerationGC(ctx context.Context, keepPerProfile int64) (GenerationGCPlan, error) {
	plan := GenerationGCPlan{
		WorktreeID: store.worktree, Incarnation: store.incarnation,
		KeepPerProfile: keepPerProfile, ObservedAt: time.Now().UTC(),
		Candidates: []GenerationGCItem{},
	}
	tx, err := store.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return GenerationGCPlan{}, fmt.Errorf("begin generation GC plan snapshot: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `SELECT generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
		provider_fingerprint,manifest_json,created_at,completed_at
		FROM generations WHERE state IN ('READY','READY_PARTIAL','RETIRED','FAILED','CANCELLED')`)
	if err != nil {
		return GenerationGCPlan{}, fmt.Errorf("list generation GC candidates: %w", err)
	}
	var records []GenerationRecord
	for rows.Next() {
		record, scanErr := readGeneration(ctx, rows)
		if scanErr != nil {
			_ = rows.Close()
			return GenerationGCPlan{}, fmt.Errorf("decode generation GC candidate: %w", scanErr)
		}
		records = append(records, record)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return GenerationGCPlan{}, errors.Join(fmt.Errorf("read generation GC candidates: %w", rowsErr), closeErr)
	}
	if closeErr != nil {
		return GenerationGCPlan{}, fmt.Errorf("close generation GC candidate scan: %w", closeErr)
	}
	activeRows, err := tx.QueryContext(ctx, `SELECT generation_id FROM active_generations`)
	if err != nil {
		return GenerationGCPlan{}, fmt.Errorf("list active generations for GC: %w", err)
	}
	active := make(map[model.UUID]struct{})
	for activeRows.Next() {
		var value string
		if err := activeRows.Scan(&value); err != nil {
			_ = activeRows.Close()
			return GenerationGCPlan{}, fmt.Errorf("read active generation for GC: %w", err)
		}
		generationID, err := model.ParseUUID(value)
		if err != nil || string(generationID) != value {
			_ = activeRows.Close()
			return GenerationGCPlan{}, fmt.Errorf("invalid active generation identity: %w", ErrInvalidIndex)
		}
		active[generationID] = struct{}{}
	}
	rowsErr = activeRows.Err()
	closeErr = activeRows.Close()
	if rowsErr != nil {
		return GenerationGCPlan{}, errors.Join(fmt.Errorf("finish active generation GC scan: %w", rowsErr), closeErr)
	}
	if closeErr != nil {
		return GenerationGCPlan{}, fmt.Errorf("close active generation GC scan: %w", closeErr)
	}
	if err := tx.Commit(); err != nil {
		return GenerationGCPlan{}, fmt.Errorf("finish generation GC plan snapshot: %w", err)
	}
	committed = true

	byProfile := make(map[model.Hash][]GenerationRecord)
	for _, record := range records {
		switch record.State {
		case GenerationReady, GenerationReadyPartial:
			byProfile[record.Key.ProfileID] = append(byProfile[record.Key.ProfileID], record)
		case GenerationRetired, GenerationFailed, GenerationCancelled:
			if _, isActive := active[record.Key.GenerationID]; !isActive {
				plan.Candidates = append(plan.Candidates, gcItem(record))
			}
		}
	}
	for _, profileRecords := range byProfile {
		sort.Slice(profileRecords, func(i, j int) bool {
			left, right := profileRecords[i], profileRecords[j]
			if left.CompletedAt == nil || right.CompletedAt == nil {
				return left.Key.GenerationID < right.Key.GenerationID
			}
			if left.CompletedAt.Equal(*right.CompletedAt) {
				return left.Key.GenerationID > right.Key.GenerationID
			}
			return left.CompletedAt.After(*right.CompletedAt)
		})
		for index := keepPerProfile; index < int64(len(profileRecords)); index++ {
			record := profileRecords[index]
			if _, isActive := active[record.Key.GenerationID]; !isActive {
				plan.Candidates = append(plan.Candidates, gcItem(record))
			}
		}
	}
	sort.Slice(plan.Candidates, func(i, j int) bool {
		left, right := plan.Candidates[i], plan.Candidates[j]
		if left.ProfileID != right.ProfileID {
			return left.ProfileID < right.ProfileID
		}
		if left.CompletedAt == nil || right.CompletedAt == nil {
			return left.GenerationID < right.GenerationID
		}
		if left.CompletedAt.Equal(*right.CompletedAt) {
			return left.GenerationID < right.GenerationID
		}
		return left.CompletedAt.Before(*right.CompletedAt)
	})
	return plan, nil
}

func gcItem(record GenerationRecord) GenerationGCItem {
	item := GenerationGCItem{GenerationID: record.Key.GenerationID, ProfileID: record.Key.ProfileID, State: record.State}
	if record.CompletedAt != nil {
		completedAt := *record.CompletedAt
		item.CompletedAt = &completedAt
	}
	return item
}

// CollectGenerationGC applies one transaction per generation. If any stage
// fails, that generation's retirement and child-row deletions roll back.
func (store *Store) CollectGenerationGC(ctx context.Context, plan GenerationGCPlan) (GenerationGCResult, error) {
	var result GenerationGCResult
	if store == nil || store.writer == nil || store.readers == nil || ctx == nil {
		return result, fmt.Errorf("invalid generation GC request: %w", ErrInvalidIndex)
	}
	if err := validateGenerationGCPlan(plan); err != nil {
		return result, err
	}
	err := store.withGenerationWriter(ctx, func(writerCtx context.Context) error {
		if store.closed {
			return fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
		}
		if plan.WorktreeID != store.worktree || plan.Incarnation != store.incarnation {
			return ErrGenerationGCPlanStale
		}
		current, err := store.planGenerationGC(writerCtx, plan.KeepPerProfile)
		if err != nil {
			return err
		}
		if err := ensureGCPlanCurrent(plan, current); err != nil {
			return err
		}
		result.AlreadyMissing = int64(len(plan.Candidates) - len(current.Candidates))
		for _, candidate := range current.Candidates {
			if err := writerCtx.Err(); err != nil {
				return err
			}
			blobs, found, err := store.collectGeneration(writerCtx, candidate)
			if found {
				result.CollectedGenerations++
				result.DeletedContentBlobs += blobs
			}
			if err != nil {
				return fmt.Errorf("collect generation %s: %w", candidate.GenerationID, err)
			}
			if !found {
				result.AlreadyMissing++
			}
		}
		return nil
	})
	return result, err
}

func validateGenerationGCPlan(plan GenerationGCPlan) error {
	worktreeID, worktreeErr := model.ParseUUID(string(plan.WorktreeID))
	if worktreeErr != nil || worktreeID != plan.WorktreeID {
		return fmt.Errorf("GC plan has invalid worktree identity: %w", ErrInvalidIndex)
	}
	incarnation, incarnationErr := model.ParseUUID(string(plan.Incarnation))
	if incarnationErr != nil || incarnation != plan.Incarnation {
		return fmt.Errorf("GC plan has invalid store incarnation: %w", ErrInvalidIndex)
	}
	if plan.KeepPerProfile < 1 || plan.KeepPerProfile > model.MaxSafeInteger || plan.ObservedAt.IsZero() || plan.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("GC plan has invalid retention or observation time: %w", ErrInvalidIndex)
	}
	seen := make(map[model.UUID]struct{}, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		generationID, generationErr := model.ParseUUID(string(candidate.GenerationID))
		if generationErr != nil || generationID != candidate.GenerationID {
			return fmt.Errorf("GC plan has invalid generation identity: %w", ErrInvalidIndex)
		}
		profileID, profileErr := model.ParseHash(string(candidate.ProfileID))
		if profileErr != nil || profileID != candidate.ProfileID {
			return fmt.Errorf("GC plan has invalid profile identity: %w", ErrInvalidIndex)
		}
		if candidate.State != GenerationReady && candidate.State != GenerationReadyPartial && candidate.State != GenerationRetired && candidate.State != GenerationFailed && candidate.State != GenerationCancelled {
			return fmt.Errorf("GC plan contains an ineligible generation state: %w", ErrGenerationNotCollectable)
		}
		if _, duplicate := seen[candidate.GenerationID]; duplicate {
			return fmt.Errorf("GC plan repeats a generation: %w", ErrInvalidIndex)
		}
		seen[candidate.GenerationID] = struct{}{}
	}
	return nil
}

func ensureGCPlanCurrent(requested, current GenerationGCPlan) error {
	if requested.WorktreeID != current.WorktreeID || requested.Incarnation != current.Incarnation || requested.KeepPerProfile != current.KeepPerProfile {
		return ErrGenerationGCPlanStale
	}
	requestedByID := make(map[model.UUID]GenerationGCItem, len(requested.Candidates))
	for _, candidate := range requested.Candidates {
		requestedByID[candidate.GenerationID] = candidate
	}
	for _, candidate := range current.Candidates {
		previous, ok := requestedByID[candidate.GenerationID]
		if !ok || !sameGCItem(previous, candidate) {
			return ErrGenerationGCPlanStale
		}
	}
	return nil
}

func sameGCItem(left, right GenerationGCItem) bool {
	if left.GenerationID != right.GenerationID || left.ProfileID != right.ProfileID || left.State != right.State {
		return false
	}
	if left.CompletedAt == nil || right.CompletedAt == nil {
		return left.CompletedAt == nil && right.CompletedAt == nil
	}
	return left.CompletedAt.Equal(*right.CompletedAt)
}

func (store *Store) collectGeneration(ctx context.Context, candidate GenerationGCItem) (deletedBlobs int64, found bool, retErr error) {
	measurement, err := store.measureIndexCache(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("measure SQLite growth before GC: %w", err)
	}
	peak, ok := multiplyNonnegative(measurement.IndexFileBytes, 2)
	if !ok || peak > model.MaxSafeInteger {
		return 0, false, ErrAdmissionReservationOverflow
	}
	if peak < manifestWriteOverhead {
		peak = manifestWriteOverhead
	}
	var removed bool
	var removedManifest *model.StoredCaptureManifestRef
	err = store.withWriteTx(ctx, peak, func(tx *writeTx) error {
		current, err := readGeneration(ctx, tx.QueryRowContext(ctx, `SELECT generation_id,profile_id,snapshot_digest,state,owner_job_id,
			policy_digest,policy_epoch,provider_fingerprint,manifest_json,created_at,completed_at
			FROM generations WHERE generation_id=?`, string(candidate.GenerationID)))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read generation before GC: %w", err)
		}
		if current.Key.ProfileID != candidate.ProfileID || current.State != candidate.State {
			return ErrGenerationGCPlanStale
		}
		if current.ManifestRef != nil {
			manifest := *current.ManifestRef
			removedManifest = &manifest
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM active_generations WHERE generation_id=?)`,
			string(candidate.GenerationID)).Scan(&active); err != nil {
			return fmt.Errorf("verify active generation before GC: %w", err)
		}
		if active != 0 {
			return fmt.Errorf("active generation cannot be collected: %w", ErrGenerationNotCollectable)
		}
		if current.State != GenerationRetired && current.State != GenerationFailed && current.State != GenerationCancelled {
			if current.State != GenerationReady && current.State != GenerationReadyPartial {
				return ErrGenerationNotCollectable
			}
			updated, err := tx.ExecContext(ctx, `UPDATE generations SET state='RETIRED'
				WHERE generation_id=? AND profile_id=? AND state=?`, string(candidate.GenerationID),
				string(candidate.ProfileID), string(current.State))
			if err != nil {
				return fmt.Errorf("retire SQLite generation before GC: %w", err)
			}
			rows, err := updated.RowsAffected()
			if err != nil || rows != 1 {
				return errors.Join(fmt.Errorf("retire generation affected %d rows: %w", rows, ErrGenerationGCPlanStale), err)
			}
		}
		if err := deleteGenerationAddonRows(ctx, tx, candidate.GenerationID); err != nil {
			return err
		}
		domainTables, err := generationDomainTables(ctx, tx)
		if err != nil {
			return err
		}
		if domainTables {
			if err := deleteGenerationDomainChildren(ctx, tx, candidate.GenerationID); err != nil {
				return err
			}
		}
		for _, table := range []string{"relations", "module_members", "search_documents", "diagnostics", "capability_results", "symbols"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE generation_id=?`, string(candidate.GenerationID)); err != nil {
				return fmt.Errorf("delete generation %s rows: %w", table, err)
			}
		}
		if domainTables {
			if _, err := tx.ExecContext(ctx, `DELETE FROM domain_entities WHERE generation_id=?`, string(candidate.GenerationID)); err != nil {
				return fmt.Errorf("delete generation domain_entities rows: %w", err)
			}
		}
		if err := deleteModuleLeaves(ctx, tx, candidate.GenerationID); err != nil {
			return err
		}
		for _, table := range []string{"files", "packages"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE generation_id=?`, string(candidate.GenerationID)); err != nil {
				return fmt.Errorf("delete generation %s rows: %w", table, err)
			}
		}
		deleted, err := tx.ExecContext(ctx, `DELETE FROM generations WHERE generation_id=? AND profile_id=? AND state IN ('RETIRED','FAILED','CANCELLED')`,
			string(candidate.GenerationID), string(candidate.ProfileID))
		if err != nil {
			return fmt.Errorf("delete retired SQLite generation: %w", err)
		}
		rows, err := deleted.RowsAffected()
		if err != nil || rows != 1 {
			return errors.Join(fmt.Errorf("delete generation affected %d rows: %w", rows, ErrGenerationGCPlanStale), err)
		}
		blobs, err := tx.ExecContext(ctx, `DELETE FROM content_blobs
			WHERE NOT EXISTS (SELECT 1 FROM files WHERE files.content_hash=content_blobs.content_hash)`)
		if err != nil {
			return fmt.Errorf("delete unreferenced SQLite content blobs: %w", err)
		}
		deletedBlobs, err = blobs.RowsAffected()
		if err != nil {
			return fmt.Errorf("read deleted content blob count: %w", err)
		}
		if err := verifyForeignKeys(ctx, tx); err != nil {
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		return deletedBlobs, removed, err
	}
	if removed && removedManifest != nil {
		if err := store.removeUnreferencedManifest(ctx, *removedManifest); err != nil {
			return deletedBlobs, removed, fmt.Errorf("remove unreferenced capture manifest after committed GC: %w", err)
		}
	}
	return deletedBlobs, removed, nil
}

func (store *Store) removeUnreferencedManifest(ctx context.Context, ref model.StoredCaptureManifestRef) error {
	if store == nil || store.readers == nil || store.ownership == nil || store.ownership.root == nil || ctx == nil {
		return fmt.Errorf("verified store ownership is required for manifest GC: %w", ErrInvalidIndex)
	}
	raw, err := EncodeStoredCaptureManifestRef(ref)
	if err != nil {
		return err
	}
	var referenced int
	if err := store.readers.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM generations WHERE manifest_json=?)`, string(raw)).Scan(&referenced); err != nil {
		return fmt.Errorf("check retained capture manifest references: %w", err)
	}
	if referenced != 0 {
		return nil
	}
	path := "worktrees/" + string(store.worktree) + "/" + ref.Path
	file, err := store.ownership.root.OpenRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open owned capture manifest before GC: %w", err)
	}
	info, statErr := file.Stat()
	digest := sha256.New()
	byteLength, copyErr := io.Copy(digest, io.LimitReader(file, ref.ByteLength+1))
	closeErr := file.Close()
	if statErr != nil || copyErr != nil || closeErr != nil {
		return errors.Join(fmt.Errorf("verify owned capture manifest before GC: %w", ErrInvalidIndex), statErr, copyErr, closeErr)
	}
	actualDigest := model.Hash(hex.EncodeToString(digest.Sum(nil)))
	if !info.Mode().IsRegular() || info.Size() != ref.ByteLength || byteLength != ref.ByteLength || actualDigest != ref.ArtifactSHA256 {
		return fmt.Errorf("owned capture manifest does not match its durable descriptor: %w", ErrInvalidIndex)
	}
	if err := store.ownership.root.RemoveRegularFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func deleteGenerationAddonRows(ctx context.Context, tx *writeTx, generationID model.UUID) error {
	uiCount, err := schemaTableCount(ctx, tx, "ui_build_bindings")
	if err != nil {
		return err
	}
	anchorCount, err := schemaTableCount(ctx, tx, "ui_anchor_links")
	if err != nil {
		return err
	}
	if uiCount != anchorCount {
		return fmt.Errorf("incomplete UI binding schema capability: %w", ErrInvalidIndex)
	}
	if uiCount == 1 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ui_build_bindings WHERE source_generation_id=?`, string(generationID)); err != nil {
			return fmt.Errorf("delete UI bindings for retired generation: %w", err)
		}
	}
	return nil
}

func generationDomainTables(ctx context.Context, tx *writeTx) (bool, error) {
	tables := []string{"entity_links", "state_transitions", "contract_fields", "domain_entities"}
	counts := make([]int, len(tables))
	for index, table := range tables {
		count, err := schemaTableCount(ctx, tx, table)
		if err != nil {
			return false, err
		}
		counts[index] = count
	}
	for _, count := range counts {
		if count != counts[0] {
			return false, fmt.Errorf("incomplete domain schema capability: %w", ErrInvalidIndex)
		}
	}
	return counts[0] == 1, nil
}

func deleteGenerationDomainChildren(ctx context.Context, tx *writeTx, generationID model.UUID) error {
	for _, table := range []string{"entity_links", "state_transitions", "contract_fields"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE generation_id=?`, string(generationID)); err != nil {
			return fmt.Errorf("delete generation %s rows: %w", table, err)
		}
	}
	return nil
}

func schemaTableCount(ctx context.Context, tx *writeTx, table string) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		return 0, fmt.Errorf("inspect SQLite schema capability %s: %w", table, err)
	}
	if count > 1 {
		return 0, fmt.Errorf("duplicate SQLite schema capability %s: %w", table, ErrInvalidIndex)
	}
	return count, nil
}

func deleteModuleLeaves(ctx context.Context, tx *writeTx, generationID model.UUID) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := tx.conn.QueryContext(ctx, `SELECT module.module_id FROM modules module
			WHERE module.generation_id=? AND NOT EXISTS (
				SELECT 1 FROM modules child WHERE child.generation_id=module.generation_id
				AND child.parent_module_id=module.module_id
			)
			ORDER BY module.module_id COLLATE BINARY LIMIT ?`, string(generationID), generationGCModuleLeafBatch)
		if err != nil {
			return fmt.Errorf("select module leaves for GC: %w", err)
		}
		leaves := make([]string, 0, generationGCModuleLeafBatch)
		for rows.Next() {
			var moduleID string
			if err := rows.Scan(&moduleID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("read module leaf for GC: %w", err)
			}
			leaves = append(leaves, moduleID)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return errors.Join(fmt.Errorf("finish module leaf scan: %w", rowsErr), closeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close module leaf scan: %w", closeErr)
		}
		if len(leaves) == 0 {
			var remaining int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM modules WHERE generation_id=?`, string(generationID)).Scan(&remaining); err != nil {
				return fmt.Errorf("count remaining modules during GC: %w", err)
			}
			if remaining != 0 {
				return fmt.Errorf("module cycle or corrupt parent graph blocks GC: %w", ErrGenerationCycle)
			}
			return nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(leaves)+1), ",")
		args := make([]any, 0, len(leaves)+1)
		args = append(args, string(generationID))
		for _, moduleID := range leaves {
			args = append(args, moduleID)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM modules WHERE generation_id=? AND module_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return fmt.Errorf("delete module leaves during GC: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil || deleted != int64(len(leaves)) {
			return errors.Join(fmt.Errorf("deleted %d of %d selected module leaves: %w", deleted, len(leaves), ErrInvalidIndex), err)
		}
	}
}

func verifyForeignKeys(ctx context.Context, tx *writeTx) error {
	rows, err := tx.conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check SQLite foreign keys after GC: %w", err)
	}
	if rows.Next() {
		var table string
		var rowID, parent, foreignKeyID any
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			return errors.Join(fmt.Errorf("read SQLite foreign-key violation after GC: %w", err), rows.Close())
		}
		return errors.Join(fmt.Errorf("SQLite foreign-key violation after GC in %s: %w", table, ErrInvalidIndex), rows.Close())
	}
	if err := rows.Err(); err != nil {
		return errors.Join(fmt.Errorf("finish SQLite foreign-key check after GC: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close SQLite foreign-key check after GC: %w", err)
	}
	return nil
}
