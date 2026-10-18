package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type GenerationState string

const (
	GenerationBuilding     GenerationState = "BUILDING"
	GenerationReady        GenerationState = "READY"
	GenerationReadyPartial GenerationState = "READY_PARTIAL"
	GenerationFailed       GenerationState = "FAILED"
	GenerationCancelled    GenerationState = "CANCELLED"
	GenerationRetired      GenerationState = "RETIRED"
)

var (
	ErrGenerationNotFound        = errors.New("SQLite generation not found")
	ErrGenerationConflict        = errors.New("SQLite generation identity conflicts with existing data")
	ErrGenerationState           = errors.New("SQLite generation is in an incompatible state")
	ErrGenerationUnavailable     = errors.New("no published SQLite generation is available")
	ErrPublicationOutcomeUnknown = errors.New("SQLite publish outcome could not be resolved")
	ErrGenerationLeaseActive     = errors.New("SQLite generation writer lease is already active")
	ErrGenerationCycle           = errors.New("SQLite generation module graph contains a cycle")
)

// GenerationKey is the compare-and-swap identity for one build. The owner job
// is mandatory so a stale worker cannot publish or abort another worker's row.
type GenerationKey struct {
	GenerationID model.UUID
	ProfileID    model.Hash
	OwnerJobID   model.UUID
}

// GenerationSpec freezes every input that must remain bound to a staged build.
type GenerationSpec struct {
	Key                 GenerationKey
	SnapshotDigest      model.Hash
	PolicyDigest        model.Hash
	PolicyEpoch         int64
	ProviderFingerprint model.Hash
	ManifestRef         model.StoredCaptureManifestRef
}

// GenerationRecord is the persisted state returned by the store. A BUILDING
// row may have no manifest only when it predates this API; CreateGeneration
// always writes a complete descriptor before returning.
type GenerationRecord struct {
	Key                 GenerationKey
	SnapshotDigest      model.Hash
	State               GenerationState
	PolicyDigest        model.Hash
	PolicyEpoch         int64
	ProviderFingerprint model.Hash
	ManifestRef         *model.StoredCaptureManifestRef
	CreatedAt           time.Time
	CompletedAt         *time.Time
}

type PublishRequest struct {
	Spec  GenerationSpec
	State GenerationState
}

type PublishResult struct {
	Generation       GenerationRecord
	Activated        bool
	AlreadyPublished bool
}

type PublicationResolution struct {
	Generation GenerationRecord
	Active     bool
}

type generationRow interface {
	Scan(dest ...any) error
}

type generationWriteContextKey struct{}

type generationWriteToken struct {
	store    *Store
	mu       sync.Mutex
	active   bool
	inFlight sync.WaitGroup
}

func (token *generationWriteToken) enter() (func(), bool) {
	token.mu.Lock()
	if !token.active {
		token.mu.Unlock()
		return nil, false
	}
	token.inFlight.Add(1)
	token.mu.Unlock()
	return token.inFlight.Done, true
}

func (token *generationWriteToken) deactivate() {
	token.mu.Lock()
	token.active = false
	token.mu.Unlock()
	token.inFlight.Wait()
}

func (token *generationWriteToken) isActive() bool {
	token.mu.Lock()
	defer token.mu.Unlock()
	return token.active
}

// WithGenerationWriter owns the worktree OS writer lease through every staging
// batch, verification callback, and final publish in operation. Writes using
// the supplied context retain short SQL transactions without reacquiring the
// same OS lock.
func (store *Store) WithGenerationWriter(ctx context.Context, operation func(context.Context) error) (retErr error) {
	if store == nil || store.writer == nil || store.ownership == nil || ctx == nil || operation == nil {
		return fmt.Errorf("generation writer requires a writable store, context, and operation: %w", ErrInvalidIndex)
	}
	if token, ok := ctx.Value(generationWriteContextKey{}).(*generationWriteToken); ok && token.store == store && token.isActive() {
		return ErrGenerationLeaseActive
	}
	store.writeGate.RLock()
	defer store.writeGate.RUnlock()
	if store.closed {
		return fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
	}
	lease, err := store.ownership.acquireWriter(ctx)
	if err != nil {
		return fmt.Errorf("acquire worktree writer lease for generation: %w", err)
	}
	token := &generationWriteToken{store: store, active: true}
	defer func() {
		token.deactivate()
		if closeErr := lease.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release generation writer lease: %w", closeErr))
		}
	}()
	return operation(context.WithValue(ctx, generationWriteContextKey{}, token))
}

// GenerationBatch is a short-lived parameterized write view that exists only
// inside WriteGenerationBatch's SQLite transaction.
type GenerationBatch struct {
	tx  *writeTx
	ctx context.Context
}

func (batch *GenerationBatch) Exec(query string, args ...any) (sql.Result, error) {
	if batch == nil || batch.tx == nil || batch.ctx == nil {
		return nil, fmt.Errorf("invalid generation batch write: %w", ErrInvalidIndex)
	}
	return batch.tx.ExecContext(batch.ctx, query, args...)
}

func (batch *GenerationBatch) QueryRow(query string, args ...any) (RowScanner, error) {
	if batch == nil || batch.tx == nil || batch.ctx == nil {
		return nil, fmt.Errorf("invalid generation batch query: %w", ErrInvalidIndex)
	}
	return batch.tx.QueryRowContext(batch.ctx, query, args...), nil
}

// WriteGenerationBatch applies one bounded batch only while the exact owner
// still has a BUILDING generation. The callback must not perform IO or
// unbounded work while the SQL transaction is open.
func (store *Store) WriteGenerationBatch(ctx context.Context, key GenerationKey, peakGrowthBytes int64, write func(*GenerationBatch) error) error {
	if err := validateGenerationKey(key); err != nil {
		return err
	}
	if write == nil {
		return fmt.Errorf("generation batch callback is required: %w", ErrInvalidIndex)
	}
	return store.withWriteTx(ctx, peakGrowthBytes, func(tx *writeTx) error {
		generation, err := readGeneration(ctx, tx.QueryRowContext(ctx, `SELECT
			generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
			provider_fingerprint,manifest_json,created_at,completed_at
			FROM generations WHERE generation_id=?`, string(key.GenerationID)))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGenerationNotFound
		}
		if err != nil {
			return fmt.Errorf("verify generation before batch write: %w", err)
		}
		if generation.Key != key {
			return ErrGenerationConflict
		}
		if generation.State != GenerationBuilding {
			return fmt.Errorf("cannot stage generation in state %s: %w", generation.State, ErrGenerationState)
		}
		return write(&GenerationBatch{tx: tx, ctx: ctx})
	})
}

func (spec GenerationSpec) validate() ([]byte, error) {
	if err := validateGenerationKey(spec.Key); err != nil {
		return nil, err
	}
	for _, digest := range []model.Hash{spec.SnapshotDigest, spec.PolicyDigest, spec.ProviderFingerprint} {
		if _, err := model.ParseHash(string(digest)); err != nil {
			return nil, fmt.Errorf("validate generation digest: %w", err)
		}
	}
	if spec.PolicyEpoch < 1 || spec.PolicyEpoch > model.MaxSafeInteger {
		return nil, fmt.Errorf("invalid generation policy epoch: %w", ErrInvalidIndex)
	}
	if err := spec.ManifestRef.Validate(); err != nil {
		return nil, fmt.Errorf("validate generation manifest reference: %w", err)
	}
	if spec.ManifestRef.SnapshotDigest != spec.SnapshotDigest {
		return nil, fmt.Errorf("generation snapshot and manifest digests differ: %w", ErrInvalidIndex)
	}
	raw, err := EncodeStoredCaptureManifestRef(spec.ManifestRef)
	if err != nil {
		return nil, err
	}
	if len(raw) > 4096 {
		return nil, fmt.Errorf("stored capture manifest reference exceeds 4 KiB: %w", ErrInvalidIndex)
	}
	return raw, nil
}

func validateGenerationKey(key GenerationKey) error {
	for _, value := range []model.UUID{key.GenerationID, key.OwnerJobID} {
		parsed, err := model.ParseUUID(string(value))
		if err != nil || parsed != value {
			return fmt.Errorf("generation and owner IDs must be canonical UUIDs: %w", ErrInvalidIndex)
		}
	}
	parsed, err := model.ParseHash(string(key.ProfileID))
	if err != nil || parsed != key.ProfileID {
		return fmt.Errorf("generation profile ID must be a canonical hash: %w", ErrInvalidIndex)
	}
	return nil
}

func validateGenerationState(state GenerationState) bool {
	switch state {
	case GenerationBuilding, GenerationReady, GenerationReadyPartial, GenerationFailed, GenerationCancelled, GenerationRetired:
		return true
	default:
		return false
	}
}

func sameGenerationSpec(record GenerationRecord, spec GenerationSpec) bool {
	return record.Key == spec.Key && record.SnapshotDigest == spec.SnapshotDigest &&
		record.PolicyDigest == spec.PolicyDigest && record.PolicyEpoch == spec.PolicyEpoch &&
		record.ProviderFingerprint == spec.ProviderFingerprint && record.ManifestRef != nil &&
		*record.ManifestRef == spec.ManifestRef
}

// CreateGeneration creates a BUILDING row with its frozen capture descriptor.
// Repeating the same generation/job request is idempotent; reusing an ID with
// different input or owner fails closed.
func (store *Store) CreateGeneration(ctx context.Context, spec GenerationSpec) (GenerationRecord, error) {
	var empty GenerationRecord
	rawManifest, err := spec.validate()
	if err != nil {
		return empty, err
	}
	peak, ok := addNonnegative(int64(len(rawManifest)), manifestWriteOverhead)
	if !ok || peak > model.MaxSafeInteger {
		return empty, ErrAdmissionReservationOverflow
	}
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	var result GenerationRecord
	err = store.withWriteTx(ctx, peak, func(tx *writeTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO generations(
			generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
			provider_fingerprint,manifest_json,created_at,completed_at
		) VALUES(?,?,?,'BUILDING',?,?,?,?,?,?,NULL)
		ON CONFLICT(generation_id) DO NOTHING`,
			string(spec.Key.GenerationID), string(spec.Key.ProfileID), string(spec.SnapshotDigest),
			string(spec.Key.OwnerJobID), string(spec.PolicyDigest), spec.PolicyEpoch,
			string(spec.ProviderFingerprint), string(rawManifest), createdAt)
		if err != nil {
			return fmt.Errorf("create BUILDING SQLite generation: %w", err)
		}
		result, err = readGeneration(ctx, tx.QueryRowContext(ctx, `SELECT
			generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
			provider_fingerprint,manifest_json,created_at,completed_at
			FROM generations WHERE generation_id=?`, string(spec.Key.GenerationID)))
		if err != nil {
			return fmt.Errorf("read created SQLite generation: %w", err)
		}
		if !sameGenerationSpec(result, spec) {
			return ErrGenerationConflict
		}
		return nil
	})
	if err != nil {
		return empty, err
	}
	return result, nil
}

// PublishGeneration verifies the exact owner/profile/snapshot/manifest tuple,
// marks it ready, and switches the profile pointer in one SQLite transaction.
// The generation ID is also the durable receipt key for resolving an uncertain
// COMMIT; a retry never republishes an older generation over a newer pointer.
func (store *Store) PublishGeneration(ctx context.Context, request PublishRequest) (PublishResult, error) {
	var result PublishResult
	rawManifest, err := request.Spec.validate()
	if err != nil {
		return result, err
	}
	if store == nil || store.writer == nil || ctx == nil {
		return result, fmt.Errorf("generation publish requires a writable store and context: %w", ErrInvalidIndex)
	}
	if request.State != GenerationReady && request.State != GenerationReadyPartial {
		return result, fmt.Errorf("publish target must be READY or READY_PARTIAL: %w", ErrInvalidIndex)
	}
	publish := func(writerCtx context.Context) error {
		if err := store.validateGenerationForPublish(writerCtx, request.Spec.Key.GenerationID); err != nil {
			return err
		}
		return store.withWriteTx(writerCtx, manifestWriteOverhead, func(tx *writeTx) error {
			current, err := readGeneration(ctx, tx.QueryRowContext(ctx, `SELECT
			generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
			provider_fingerprint,manifest_json,created_at,completed_at
			FROM generations WHERE generation_id=?`, string(request.Spec.Key.GenerationID)))
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("publish generation: %w", ErrGenerationNotFound)
			}
			if err != nil {
				return fmt.Errorf("read generation before publish: %w", err)
			}
			if !sameGenerationSpec(current, request.Spec) {
				return ErrGenerationConflict
			}
			if current.State == request.State {
				result.Generation = current
				result.AlreadyPublished = true
				active, err := activeGenerationMatchesWrite(ctx, tx, request.Spec.Key.ProfileID, request.Spec.Key.GenerationID)
				if err != nil {
					return err
				}
				result.Activated = active
				return nil
			}
			if current.State != GenerationBuilding {
				return fmt.Errorf("generation cannot transition from %s to %s: %w", current.State, request.State, ErrGenerationState)
			}
			if current.ManifestRef == nil || current.ManifestRef.SnapshotDigest != request.Spec.SnapshotDigest {
				return fmt.Errorf("generation has no matching capture manifest: %w", ErrInvalidIndex)
			}

			completedAt := time.Now().UTC().Format(time.RFC3339Nano)
			updated, err := tx.ExecContext(ctx, `UPDATE generations SET state=?,completed_at=?
			WHERE generation_id=? AND profile_id=? AND owner_job_id=? AND snapshot_digest=?
			  AND state='BUILDING' AND manifest_json=?`,
				string(request.State), completedAt, string(request.Spec.Key.GenerationID),
				string(request.Spec.Key.ProfileID), string(request.Spec.Key.OwnerJobID),
				string(request.Spec.SnapshotDigest), string(rawManifest))
			if err != nil {
				return fmt.Errorf("mark SQLite generation published: %w", err)
			}
			rows, err := updated.RowsAffected()
			if err != nil {
				return fmt.Errorf("read generation publish update count: %w", err)
			}
			if rows != 1 {
				return ErrGenerationConflict
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO active_generations(profile_id,generation_id)
			VALUES(?,?) ON CONFLICT(profile_id) DO UPDATE SET generation_id=excluded.generation_id`,
				string(request.Spec.Key.ProfileID), string(request.Spec.Key.GenerationID)); err != nil {
				return fmt.Errorf("switch active SQLite generation: %w", err)
			}
			current.State = request.State
			parsedCompletedAt, err := time.Parse(time.RFC3339Nano, completedAt)
			if err != nil {
				return fmt.Errorf("parse generated completion time: %w", err)
			}
			current.CompletedAt = &parsedCompletedAt
			result.Generation = current
			result.Activated = true
			return nil
		})
	}
	err = store.withGenerationWriter(ctx, publish)
	if err == nil {
		return result, nil
	}

	resolutionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(store.busyTimeoutMS)*time.Millisecond)
	defer cancel()
	resolution, resolveErr := store.ResolvePublication(resolutionCtx, request.Spec.Key)
	if resolveErr == nil && resolution.Generation.State == request.State && sameGenerationSpec(resolution.Generation, request.Spec) {
		return PublishResult{Generation: resolution.Generation, Activated: resolution.Active, AlreadyPublished: true}, nil
	}
	if resolveErr != nil {
		if errors.Is(resolveErr, ErrGenerationNotFound) || errors.Is(resolveErr, ErrGenerationConflict) {
			return result, err
		}
		return result, errors.Join(fmt.Errorf("%w: original publish error: %v", ErrPublicationOutcomeUnknown, err), resolveErr)
	}
	return result, err
}

func (store *Store) withGenerationWriter(ctx context.Context, operation func(context.Context) error) error {
	if token, ok := ctx.Value(generationWriteContextKey{}).(*generationWriteToken); ok && token.store == store && token.isActive() {
		return operation(ctx)
	}
	return store.WithGenerationWriter(ctx, operation)
}

// validateGenerationForPublish performs whole-store FK and module-graph checks
// while the worktree writer lease is held, before the short publish transaction.
func (store *Store) validateGenerationForPublish(ctx context.Context, generationID model.UUID) error {
	if store == nil || store.readers == nil || ctx == nil {
		return fmt.Errorf("invalid generation publication validation: %w", ErrInvalidIndex)
	}
	var hasCycle int
	if err := store.readers.QueryRowContext(ctx, `WITH RECURSIVE module_ancestry(origin_id,node_id) AS (
		SELECT module_id,parent_module_id FROM modules WHERE generation_id=? AND parent_module_id IS NOT NULL
		UNION
		SELECT ancestry.origin_id,module.parent_module_id
		FROM module_ancestry ancestry JOIN modules module
		  ON module.generation_id=? AND module.module_id=ancestry.node_id
		WHERE module.parent_module_id IS NOT NULL
	)
	SELECT EXISTS(SELECT 1 FROM module_ancestry WHERE origin_id=node_id)`,
		string(generationID), string(generationID)).Scan(&hasCycle); err != nil {
		return fmt.Errorf("validate SQLite module graph before publish: %w", err)
	}
	if hasCycle != 0 {
		return ErrGenerationCycle
	}
	var violations int
	if err := store.readers.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("validate SQLite foreign keys before publish: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("index has %d foreign-key violations before publish: %w", violations, ErrInvalidIndex)
	}
	return nil
}

// AbortGeneration moves only the exact BUILDING owner to FAILED or CANCELLED.
// It never changes the profile's active pointer.
func (store *Store) AbortGeneration(ctx context.Context, key GenerationKey, terminal GenerationState) error {
	if err := validateGenerationKey(key); err != nil {
		return err
	}
	if terminal != GenerationFailed && terminal != GenerationCancelled {
		return fmt.Errorf("generation abort target must be FAILED or CANCELLED: %w", ErrInvalidIndex)
	}
	return store.withWriteTx(ctx, manifestWriteOverhead, func(tx *writeTx) error {
		current, err := readGeneration(ctx, tx.QueryRowContext(ctx, `SELECT
			generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
			provider_fingerprint,manifest_json,created_at,completed_at
			FROM generations WHERE generation_id=?`, string(key.GenerationID)))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGenerationNotFound
		}
		if err != nil {
			return err
		}
		if current.Key != key {
			return ErrGenerationConflict
		}
		if current.State == terminal {
			return nil
		}
		if current.State != GenerationBuilding {
			return fmt.Errorf("cannot abort generation in state %s: %w", current.State, ErrGenerationState)
		}
		completedAt := time.Now().UTC().Format(time.RFC3339Nano)
		updated, err := tx.ExecContext(ctx, `UPDATE generations SET state=?,completed_at=?
			WHERE generation_id=? AND profile_id=? AND owner_job_id=? AND state='BUILDING'`,
			string(terminal), completedAt, string(key.GenerationID), string(key.ProfileID), string(key.OwnerJobID))
		if err != nil {
			return fmt.Errorf("abort SQLite generation: %w", err)
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("read generation abort update count: %w", err)
		}
		if rows != 1 {
			return ErrGenerationConflict
		}
		return nil
	})
}

// ResolvePublication checks the durable generation row and profile pointer in
// one read transaction. READY means the generation was committed even if a
// later successful publication has since advanced the active pointer.
func (store *Store) ResolvePublication(ctx context.Context, key GenerationKey) (PublicationResolution, error) {
	var result PublicationResolution
	if err := validateGenerationKey(key); err != nil {
		return result, err
	}
	if store == nil || store.readers == nil || ctx == nil {
		return result, fmt.Errorf("invalid generation resolution request: %w", ErrInvalidIndex)
	}
	if token, ok := ctx.Value(generationWriteContextKey{}).(*generationWriteToken); ok && token.store == store {
		if done, active := token.enter(); active {
			defer done()
			return store.resolvePublication(ctx, key)
		}
	}
	store.writeGate.RLock()
	defer store.writeGate.RUnlock()
	if store.closed {
		return result, fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
	}
	return store.resolvePublication(ctx, key)
}

func (store *Store) resolvePublication(ctx context.Context, key GenerationKey) (result PublicationResolution, retErr error) {
	tx, err := store.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, fmt.Errorf("begin SQLite publication resolution: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, fmt.Errorf("rollback SQLite publication resolution: %w", rollbackErr))
			}
		}
	}()
	result.Generation, err = readGeneration(ctx, tx.QueryRowContext(ctx, `SELECT
		generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
		provider_fingerprint,manifest_json,created_at,completed_at
		FROM generations WHERE generation_id=?`, string(key.GenerationID)))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationResolution{}, ErrGenerationNotFound
	}
	if err != nil {
		return PublicationResolution{}, fmt.Errorf("read SQLite publication receipt: %w", err)
	}
	if result.Generation.Key != key {
		return PublicationResolution{}, ErrGenerationConflict
	}
	result.Active, err = activeGenerationMatchesSQL(ctx, tx, key.ProfileID, key.GenerationID)
	if err != nil {
		return PublicationResolution{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublicationResolution{}, fmt.Errorf("finish SQLite publication resolution: %w", err)
	}
	committed = true
	return result, nil
}

type snapshotTx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type RowScanner interface {
	Scan(dest ...any) error
}

// ReadSnapshot pins one READY generation and all subsequent queries in the
// callback to the same SQLite read transaction.
type ReadSnapshot struct {
	Generation GenerationRecord
	tx         snapshotTx
}

func (snapshot *ReadSnapshot) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if snapshot == nil || snapshot.tx == nil || ctx == nil {
		return nil, fmt.Errorf("invalid generation snapshot query: %w", ErrInvalidIndex)
	}
	return snapshot.tx.QueryContext(ctx, query, args...)
}

func (snapshot *ReadSnapshot) QueryRowContext(ctx context.Context, query string, args ...any) (RowScanner, error) {
	if snapshot == nil || snapshot.tx == nil || ctx == nil {
		return nil, fmt.Errorf("invalid generation snapshot query: %w", ErrInvalidIndex)
	}
	return snapshot.tx.QueryRowContext(ctx, query, args...), nil
}

// WithPublishedSnapshot resolves the active pointer (or a requested published
// generation) and keeps every callback query on one read snapshot. The
// callback must finish before paging, network, LSP, or other long-running work.
func (store *Store) WithPublishedSnapshot(ctx context.Context, profileID model.Hash, generationID model.UUID, read func(*ReadSnapshot) error) (retErr error) {
	if ctx == nil || read == nil {
		return fmt.Errorf("invalid published generation read request: %w", ErrInvalidIndex)
	}
	if _, err := model.ParseHash(string(profileID)); err != nil {
		return fmt.Errorf("invalid profile ID: %w", err)
	}
	if generationID != "" {
		if parsed, err := model.ParseUUID(string(generationID)); err != nil || parsed != generationID {
			return fmt.Errorf("invalid explicit generation ID: %w", ErrInvalidIndex)
		}
	}
	if store == nil || store.readers == nil {
		return fmt.Errorf("invalid SQLite generation reader: %w", ErrInvalidIndex)
	}
	leased := false
	if token, ok := ctx.Value(generationWriteContextKey{}).(*generationWriteToken); ok && token.store == store {
		if done, active := token.enter(); active {
			defer done()
			leased = true
		}
	}
	if !leased {
		store.writeGate.RLock()
		defer store.writeGate.RUnlock()
	}
	if store.closed {
		return fmt.Errorf("SQLite store is closed: %w", ErrInvalidIndex)
	}
	tx, err := store.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin consistent SQLite generation read: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, fmt.Errorf("rollback SQLite generation read: %w", rollbackErr))
			}
		}
	}()
	query := `SELECT generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,
		provider_fingerprint,manifest_json,created_at,completed_at FROM generations
		WHERE profile_id=? AND generation_id=? AND state IN ('READY','READY_PARTIAL')`
	args := []any{string(profileID), string(generationID)}
	if generationID == "" {
		query = `SELECT g.generation_id,g.profile_id,g.snapshot_digest,g.state,g.owner_job_id,g.policy_digest,g.policy_epoch,
			g.provider_fingerprint,g.manifest_json,g.created_at,g.completed_at
			FROM active_generations a JOIN generations g
			ON g.profile_id=a.profile_id AND g.generation_id=a.generation_id
			WHERE a.profile_id=? AND g.state IN ('READY','READY_PARTIAL')`
		args = []any{string(profileID)}
	}
	generation, err := readGeneration(ctx, tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrGenerationUnavailable
	}
	if err != nil {
		return fmt.Errorf("resolve published SQLite generation: %w", err)
	}
	if generation.ManifestRef == nil || generation.SnapshotDigest == "" || generation.CompletedAt == nil {
		return fmt.Errorf("published generation metadata is incomplete: %w", ErrInvalidIndex)
	}
	if err := read(&ReadSnapshot{Generation: generation, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("finish consistent SQLite generation read: %w", err)
	}
	committed = true
	return nil
}

func activeGenerationMatchesSQL(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, profileID model.Hash, generationID model.UUID) (bool, error) {
	var active bool
	err := queryer.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM active_generations WHERE profile_id=? AND generation_id=?
	)`, string(profileID), string(generationID)).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("read active SQLite generation pointer: %w", err)
	}
	return active, nil
}

func activeGenerationMatchesWrite(ctx context.Context, tx *writeTx, profileID model.Hash, generationID model.UUID) (bool, error) {
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM active_generations WHERE profile_id=? AND generation_id=?
	)`, string(profileID), string(generationID)).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("read active SQLite generation pointer: %w", err)
	}
	return active, nil
}

func readGeneration(ctx context.Context, row generationRow) (GenerationRecord, error) {
	var record GenerationRecord
	var generationID, profileID, state, ownerJobID string
	var snapshotDigest, manifestJSON sql.NullString
	var policyDigest, providerFingerprint string
	var createdAt string
	var policyEpoch int64
	var completedAt sql.NullString
	if err := row.Scan(&generationID, &profileID, &snapshotDigest, &state, &ownerJobID,
		&policyDigest, &policyEpoch, &providerFingerprint, &manifestJSON, &createdAt, &completedAt); err != nil {
		return record, err
	}
	parsedGenerationID, err := model.ParseUUID(generationID)
	if err != nil {
		return record, fmt.Errorf("invalid persisted generation ID: %w", err)
	}
	parsedProfileID, err := model.ParseHash(profileID)
	if err != nil {
		return record, fmt.Errorf("invalid persisted profile ID: %w", err)
	}
	parsedOwnerID, err := model.ParseUUID(ownerJobID)
	if err != nil {
		return record, fmt.Errorf("invalid persisted owner job ID: %w", err)
	}
	parsedPolicyDigest, err := model.ParseHash(policyDigest)
	if err != nil {
		return record, fmt.Errorf("invalid persisted policy digest: %w", err)
	}
	parsedProviderFingerprint, err := model.ParseHash(providerFingerprint)
	if err != nil {
		return record, fmt.Errorf("invalid persisted provider fingerprint: %w", err)
	}
	if policyEpoch < 1 || policyEpoch > model.MaxSafeInteger || !validateGenerationState(GenerationState(state)) {
		return record, fmt.Errorf("invalid persisted generation state or policy epoch: %w", ErrInvalidIndex)
	}
	created, err := parseGenerationTime(createdAt)
	if err != nil {
		return record, fmt.Errorf("invalid generation creation time: %w", err)
	}
	record = GenerationRecord{
		Key:   GenerationKey{GenerationID: parsedGenerationID, ProfileID: parsedProfileID, OwnerJobID: parsedOwnerID},
		State: GenerationState(state), PolicyDigest: parsedPolicyDigest, PolicyEpoch: policyEpoch,
		ProviderFingerprint: parsedProviderFingerprint, CreatedAt: created,
	}
	if snapshotDigest.Valid {
		record.SnapshotDigest, err = model.ParseHash(snapshotDigest.String)
		if err != nil {
			return GenerationRecord{}, fmt.Errorf("invalid persisted snapshot digest: %w", err)
		}
	}
	if manifestJSON.Valid {
		manifest, err := DecodeStoredCaptureManifestRef([]byte(manifestJSON.String))
		if err != nil {
			return GenerationRecord{}, fmt.Errorf("invalid persisted generation manifest: %w", err)
		}
		if record.SnapshotDigest == "" || manifest.SnapshotDigest != record.SnapshotDigest {
			return GenerationRecord{}, fmt.Errorf("persisted generation manifest digest mismatch: %w", ErrInvalidIndex)
		}
		record.ManifestRef = &manifest
	}
	if completedAt.Valid {
		completed, err := parseGenerationTime(completedAt.String)
		if err != nil {
			return GenerationRecord{}, fmt.Errorf("invalid generation completion time: %w", err)
		}
		record.CompletedAt = &completed
	}
	if (record.State == GenerationReady || record.State == GenerationReadyPartial || record.State == GenerationRetired) &&
		(record.SnapshotDigest == "" || record.ManifestRef == nil || record.CompletedAt == nil) {
		return GenerationRecord{}, fmt.Errorf("published generation metadata is incomplete: %w", ErrInvalidIndex)
	}
	return record, nil
}

func parseGenerationTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	_, offset := parsed.Zone()
	if offset != 0 || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, fmt.Errorf("timestamp is not canonical UTC RFC3339Nano")
	}
	return parsed.UTC(), nil
}
