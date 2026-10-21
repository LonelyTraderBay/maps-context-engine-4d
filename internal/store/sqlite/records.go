package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	recordsSchemaVersion      = int64(1)
	recordsReaderVersion      = int64(1)
	recordsOpenPeakGrowth     = int64(8 << 20)
	recordsMutationPeakGrowth = int64(1 << 20)
)

var (
	ErrRecordsSchemaTooNew = errors.New("records SQLite schema is newer than this reader")
)

//go:embed migrations/records/*.sql
var recordsMigrations embed.FS

type RecordsStore struct {
	owner         *storeOwnership
	worktree      model.UUID
	incarnation   model.UUID
	path          string
	busyTimeoutMS int64
	writer        *sql.DB
	readers       *sql.DB
	mu            sync.RWMutex
	closed        bool
}

type RecordPage struct {
	Items       []history.Version
	RecordEpoch int64
	HasMore     bool
	NextAfterID model.UUID
}

// OpenRecords creates a records store only when requested through the
// writable API. Its durable write admission charges volume with cache=0.
func OpenRecords(ctx context.Context, options Options) (_ *RecordsStore, retErr error) {
	if ctx == nil {
		return nil, fmt.Errorf("records store context is required: %w", ErrInvalidIndex)
	}
	indexPath, timeout, err := normalizeOptions(&options)
	if err != nil {
		return nil, err
	}
	owner, err := newStoreOwnership(ctx, options, false)
	if err != nil {
		return nil, err
	}
	keepOwner := false
	var writer *sql.DB
	defer func() {
		if !keepOwner {
			if writer != nil {
				retErr = errors.Join(retErr, writer.Close())
			}
			retErr = errors.Join(retErr, owner.Close())
		}
	}()

	writerLease, err := owner.acquireWriter(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire worktree writer lease for records: %w", err)
	}
	leaseKept := false
	defer func() {
		if !leaseKept {
			retErr = errors.Join(retErr, writerLease.Close())
		}
	}()
	reservation, err := owner.admitDurableGrowth(ctx, recordsAdmissionOperation, model.AdmissionOwnedPath(recordsStorePath(owner.worktreeID)), recordsOpenPeakGrowth)
	if err != nil {
		return nil, fmt.Errorf("admit records store initialization: %w", err)
	}

	relative := recordsStorePath(owner.worktreeID)
	// SQLite receives the caller's path spelling, matching openIndex. The
	// verified root uses a final-handle path that can have a Windows extended
	// prefix unsupported by the SQLite driver.
	path := filepath.Join(filepath.Dir(indexPath), "records.sqlite")
	info, statErr := os.Lstat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !fresh {
		return nil, errors.Join(fmt.Errorf("inspect records database: %w", statErr), reservation.Close())
	}
	if !fresh {
		if err := verifyRecordsFile(owner, relative); err != nil {
			return nil, errors.Join(err, reservation.Close())
		}
		if info.Size() == 0 {
			return nil, errors.Join(fmt.Errorf("empty records database: %w", ErrInvalidIndex), reservation.Close())
		}
		if err := preflightRecords(ctx, path, timeout, owner.worktreeID); err != nil {
			return nil, errors.Join(err, reservation.Close())
		}
	}
	if fresh {
		if err := owner.root.EnsurePrivateDirs(filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))); err != nil {
			return nil, errors.Join(fmt.Errorf("prepare records database directory: %w", err), reservation.Close())
		}
	}
	mode := "rw"
	if fresh {
		mode = "rwc"
	}
	writer, err = openPool(ctx, path, mode, timeout, false, writerPoolSize)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open records database writer: %w", err), reservation.Close())
	}
	conn, err := writer.Conn(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("acquire records migration connection: %w", err), reservation.Close())
	}
	walErr := setAndCheckWAL(ctx, conn, timeout)
	var identity recordsIdentity
	var migrationErr error
	if walErr == nil {
		identity, migrationErr = migrateRecords(ctx, conn, fresh, owner.worktreeID)
	}
	connCloseErr := conn.Close()
	if walErr != nil || migrationErr != nil || connCloseErr != nil {
		return nil, errors.Join(walErr, migrationErr, connCloseErr, reservation.Close())
	}
	if err := verifyPool(ctx, writer, writerPoolSize, timeout, false); err != nil {
		return nil, errors.Join(fmt.Errorf("verify records database writer pool: %w", err), reservation.Close())
	}
	if err := reservation.Close(); err != nil {
		return nil, fmt.Errorf("settle records initialization admission: %w", err)
	}
	if err := writerLease.Close(); err != nil {
		return nil, fmt.Errorf("release records initialization writer lease: %w", err)
	}
	leaseKept = true

	readers, err := openPool(ctx, path, "ro", timeout, true, readerPoolSize)
	if err != nil {
		return nil, fmt.Errorf("open records read-only pool: %w", err)
	}
	if err := verifyPool(ctx, readers, readerPoolSize, timeout, true); err != nil {
		_ = readers.Close()
		return nil, fmt.Errorf("verify records read-only pool: %w", err)
	}
	verifiedIdentity, err := readRecordsIdentity(ctx, readers, owner.worktreeID)
	if err != nil || verifiedIdentity.incarnation != identity.incarnation {
		_ = readers.Close()
		return nil, errors.Join(fmt.Errorf("verify migrated records identity: %w", ErrInvalidIndex), err)
	}
	store := &RecordsStore{owner: owner, worktree: verifiedIdentity.worktree, incarnation: verifiedIdentity.incarnation,
		path: path, busyTimeoutMS: timeout, writer: writer, readers: readers}
	keepOwner = true
	return store, nil
}

// OpenRecordsReadOnly never creates the database or lock layout.
func OpenRecordsReadOnly(ctx context.Context, options Options) (_ *RecordsStore, retErr error) {
	if ctx == nil || options.Create {
		return nil, fmt.Errorf("read-only records open cannot create state: %w", ErrInvalidIndex)
	}
	indexPath, timeout, err := normalizeOptions(&options)
	if err != nil {
		return nil, err
	}
	owner, err := newStoreOwnership(ctx, options, true)
	if err != nil {
		return nil, err
	}
	keepOwner := false
	var readers *sql.DB
	defer func() {
		if !keepOwner {
			if readers != nil {
				retErr = errors.Join(retErr, readers.Close())
			}
			retErr = errors.Join(retErr, owner.Close())
		}
	}()

	relative := recordsStorePath(owner.worktreeID)
	path := filepath.Join(filepath.Dir(indexPath), "records.sqlite")
	if err := verifyRecordsFile(owner, relative); err != nil {
		return nil, err
	}
	if err := preflightRecords(ctx, path, timeout, owner.worktreeID); err != nil {
		return nil, err
	}
	readers, err = openPool(ctx, path, "ro", timeout, true, readerPoolSize)
	if err != nil {
		return nil, fmt.Errorf("open read-only records pool: %w", err)
	}
	if err := verifyPool(ctx, readers, readerPoolSize, timeout, true); err != nil {
		return nil, fmt.Errorf("verify records reader pool: %w", err)
	}
	identity, err := readRecordsIdentity(ctx, readers, owner.worktreeID)
	if err != nil {
		return nil, err
	}
	store := &RecordsStore{owner: owner, worktree: identity.worktree, incarnation: identity.incarnation,
		path: path, busyTimeoutMS: timeout, readers: readers}
	keepOwner = true
	return store, nil
}

func verifyRecordsFile(owner *storeOwnership, relative string) error {
	if owner == nil || owner.root == nil || relative != recordsStorePath(owner.worktreeID) {
		return fmt.Errorf("records path is outside its owner: %w", ErrAdmissionOwnership)
	}
	file, err := owner.root.OpenRegularFile(relative)
	if err != nil {
		return fmt.Errorf("open verified records database: %w", errors.Join(ErrInvalidStorePath, err))
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return errors.Join(fmt.Errorf("records database is not a regular file: %w", ErrInvalidStorePath), statErr, closeErr)
	}
	return nil
}

func preflightRecords(ctx context.Context, path string, timeout int64, expectedWorktree model.UUID) error {
	db, err := openPool(ctx, path, "ro", timeout, true, 1)
	if err != nil {
		return fmt.Errorf("open records database for preflight: %w", err)
	}
	validateErr := validateRecordsDatabase(ctx, db, expectedWorktree)
	closeErr := db.Close()
	if validateErr != nil || closeErr != nil {
		return errors.Join(validateErr, closeErr)
	}
	return nil
}

type recordsIdentity struct {
	worktree    model.UUID
	incarnation model.UUID
	epoch       int64
}

func migrateRecords(ctx context.Context, conn *sql.Conn, fresh bool, expectedWorktree model.UUID) (recordsIdentity, error) {
	if ctx == nil || conn == nil || expectedWorktree == "" {
		return recordsIdentity{}, fmt.Errorf("invalid records migration request: %w", ErrInvalidIndex)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return recordsIdentity{}, fmt.Errorf("begin records migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	version := int64(0)
	if !fresh {
		if err := tx.QueryRowContext(ctx, `SELECT schema_version FROM schema_meta WHERE singleton=1`).Scan(&version); err != nil {
			return recordsIdentity{}, fmt.Errorf("read records schema version: %w", err)
		}
	}
	if version > recordsSchemaVersion {
		return recordsIdentity{}, fmt.Errorf("records schema=%d supported=%d: %w", version, recordsSchemaVersion, ErrRecordsSchemaTooNew)
	}
	if fresh {
		if err := applyRecordsMigration(ctx, tx, "migrations/records/001.sql"); err != nil {
			return recordsIdentity{}, err
		}
		incarnation, err := newUUID()
		if err != nil {
			return recordsIdentity{}, fmt.Errorf("create records incarnation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records_meta(singleton,worktree_id,incarnation,record_epoch) VALUES(1,?,?,0)`, expectedWorktree, incarnation); err != nil {
			return recordsIdentity{}, fmt.Errorf("initialize records store identity: %w", err)
		}
	}
	if err := validateRecordsSchema(ctx, tx, versionOrOne(version, fresh)); err != nil {
		return recordsIdentity{}, err
	}
	var identity recordsIdentity
	if err := tx.QueryRowContext(ctx, `SELECT worktree_id,incarnation,record_epoch FROM records_meta WHERE singleton=1`).Scan(&identity.worktree, &identity.incarnation, &identity.epoch); err != nil {
		return recordsIdentity{}, fmt.Errorf("read records store identity: %w", err)
	}
	if identity.worktree != expectedWorktree || identity.epoch < 0 || identity.epoch > model.MaxSafeInteger {
		return recordsIdentity{}, fmt.Errorf("records store identity or epoch mismatch: %w", ErrInvalidIndex)
	}
	if _, err := model.ParseUUID(string(identity.incarnation)); err != nil {
		return recordsIdentity{}, fmt.Errorf("records store incarnation is invalid: %w", ErrInvalidIndex)
	}
	if err := tx.Commit(); err != nil {
		return recordsIdentity{}, fmt.Errorf("commit records migrations: %w", err)
	}
	committed = true
	return identity, nil
}

func versionOrOne(version int64, fresh bool) int64 {
	if fresh {
		return 1
	}
	return version
}

func applyRecordsMigration(ctx context.Context, tx *sql.Tx, relative string) error {
	if ctx == nil || tx == nil {
		return fmt.Errorf("invalid records migration transaction: %w", ErrInvalidIndex)
	}
	content, err := recordsMigrations.ReadFile(relative)
	if err != nil {
		return fmt.Errorf("read embedded records migration %s: %w", relative, err)
	}
	if _, err := tx.ExecContext(ctx, string(content)); err != nil {
		return fmt.Errorf("apply embedded records migration %s: %w", relative, err)
	}
	return nil
}

func validateRecordsDatabase(ctx context.Context, db *sql.DB, expectedWorktree model.UUID) error {
	if db == nil {
		return fmt.Errorf("records database is unavailable: %w", ErrInvalidIndex)
	}
	var version int64
	if err := db.QueryRowContext(ctx, `SELECT schema_version FROM schema_meta WHERE singleton=1`).Scan(&version); err != nil {
		return fmt.Errorf("read records schema metadata: %w", ErrInvalidIndex)
	}
	if version > recordsSchemaVersion {
		return fmt.Errorf("records schema=%d supported=%d: %w", version, recordsSchemaVersion, ErrRecordsSchemaTooNew)
	}
	if err := validateRecordsSchema(ctx, db, version); err != nil {
		return err
	}
	_, err := readRecordsIdentity(ctx, db, expectedWorktree)
	return err
}

type recordsSchemaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateRecordsSchema(ctx context.Context, query recordsSchemaQuerier, version int64) error {
	if version != recordsSchemaVersion {
		return fmt.Errorf("records schema version %d is unsupported: %w", version, ErrInvalidIndex)
	}
	var minReader int64
	if err := query.QueryRowContext(ctx, `SELECT min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&minReader); err != nil {
		return fmt.Errorf("read records minimum reader version: %w", ErrInvalidIndex)
	}
	if minReader < 1 || minReader > recordsReaderVersion || minReader > version {
		return fmt.Errorf("records minimum reader version %d is unsupported: %w", minReader, ErrRecordsSchemaTooNew)
	}
	for _, name := range []string{"schema_meta", "records_meta", "record_versions", "record_heads", "mutation_receipts"} {
		var count int
		if err := query.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name=? AND type='table'`, name).Scan(&count); err != nil || count != 1 {
			return fmt.Errorf("required records schema object %s is missing: %w", name, ErrInvalidIndex)
		}
	}
	var immutableTrigger int
	if err := query.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name='record_versions_immutable' AND type='trigger'`).Scan(&immutableTrigger); err != nil || immutableTrigger != 1 {
		return fmt.Errorf("immutable record version guard is missing: %w", ErrInvalidIndex)
	}
	rows, err := query.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check records foreign keys: %w", err)
	}
	hasViolation := rows.Next()
	readErr := rows.Err()
	closeErr := rows.Close()
	if hasViolation {
		return errors.Join(fmt.Errorf("records database has a foreign key violation: %w", ErrInvalidIndex), readErr, closeErr)
	}
	if readErr != nil || closeErr != nil {
		return errors.Join(fmt.Errorf("read records foreign key check: %w", ErrInvalidIndex), readErr, closeErr)
	}
	return nil
}

func readRecordsIdentity(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, expectedWorktree model.UUID) (recordsIdentity, error) {
	var identity recordsIdentity
	if err := query.QueryRowContext(ctx, `SELECT worktree_id,incarnation,record_epoch FROM records_meta WHERE singleton=1`).Scan(&identity.worktree, &identity.incarnation, &identity.epoch); err != nil {
		return recordsIdentity{}, fmt.Errorf("read records identity: %w", err)
	}
	if identity.worktree != expectedWorktree || identity.epoch < 0 || identity.epoch > model.MaxSafeInteger {
		return recordsIdentity{}, fmt.Errorf("records identity or epoch mismatch: %w", ErrWorktreeMismatch)
	}
	if parsed, err := model.ParseUUID(string(identity.worktree)); err != nil || parsed != identity.worktree {
		return recordsIdentity{}, fmt.Errorf("stored records worktree ID is invalid: %w", ErrInvalidIndex)
	}
	if parsed, err := model.ParseUUID(string(identity.incarnation)); err != nil || parsed != identity.incarnation {
		return recordsIdentity{}, fmt.Errorf("stored records incarnation is invalid: %w", ErrInvalidIndex)
	}
	return identity, nil
}

func (store *RecordsStore) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	var writerErr, readerErr, ownerErr error
	if store.writer != nil {
		writerErr = store.writer.Close()
	}
	if store.readers != nil {
		readerErr = store.readers.Close()
	}
	if store.owner != nil {
		ownerErr = store.owner.Close()
	}
	return errors.Join(writerErr, readerErr, ownerErr)
}

func (store *RecordsStore) WorktreeID() model.UUID {
	if store == nil {
		return ""
	}
	return store.worktree
}

func (store *RecordsStore) Incarnation() model.UUID {
	if store == nil {
		return ""
	}
	return store.incarnation
}

func (store *RecordsStore) List(ctx context.Context, filter history.ListFilter) (RecordPage, error) {
	if ctx == nil || store == nil || store.readers == nil {
		return RecordPage{}, fmt.Errorf("records reader and context are required: %w", ErrInvalidIndex)
	}
	if filter.Kind != "" && !filter.Kind.Valid() {
		return RecordPage{}, history.ErrInvalidRecord
	}
	if filter.Status != "" && !filter.Status.ValidDeclared() && filter.Status != history.StatusNeedsReview {
		return RecordPage{}, history.ErrInvalidRecord
	}
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 1 || filter.Limit > 100 || filter.Limit >= model.MaxSafeInteger {
		return RecordPage{}, history.ErrInvalidRecord
	}
	if filter.AfterRecordID != "" {
		if parsed, err := model.ParseUUID(string(filter.AfterRecordID)); err != nil || parsed != filter.AfterRecordID {
			return RecordPage{}, history.ErrInvalidRecord
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return RecordPage{}, fmt.Errorf("records store is closed: %w", ErrInvalidIndex)
	}
	tx, err := store.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RecordPage{}, fmt.Errorf("begin records list snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT record_epoch FROM records_meta WHERE singleton=1`).Scan(&epoch); err != nil {
		return RecordPage{}, fmt.Errorf("read record epoch: %w", err)
	}
	status := string(filter.Status)
	query := `SELECT v.record_id,v.version,v.record_kind,v.declared_status,v.payload_json,v.source_refs_json,
		v.actor_id,v.reason,v.payload_hash,v.created_at
		FROM record_heads h JOIN record_versions v ON v.record_id=h.record_id AND v.version=h.version
		WHERE (?='' OR v.record_kind=?) AND (?='' OR (?='needs_review' AND v.declared_status='approved') OR v.declared_status=?)
		AND (?='' OR v.record_id>?) ORDER BY v.record_id LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, string(filter.Kind), string(filter.Kind), status, status, status,
		string(filter.AfterRecordID), string(filter.AfterRecordID), filter.Limit+1)
	if err != nil {
		return RecordPage{}, fmt.Errorf("query records list: %w", err)
	}
	page := RecordPage{Items: make([]history.Version, 0, filter.Limit), RecordEpoch: epoch}
	for rows.Next() {
		version, err := scanRecordVersion(rows, store.worktree)
		if err != nil {
			_ = rows.Close()
			return RecordPage{}, err
		}
		if int64(len(page.Items)) == filter.Limit {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, version)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return RecordPage{}, errors.Join(fmt.Errorf("read records page: %w", rowsErr), closeErr)
	}
	if len(page.Items) > 0 && page.HasMore {
		page.NextAfterID = page.Items[len(page.Items)-1].RecordID
	}
	if epoch < 0 || epoch > model.MaxSafeInteger {
		return RecordPage{}, fmt.Errorf("record epoch is outside the safe integer range: %w", history.ErrRecordStoreCorrupted)
	}
	if err := tx.Commit(); err != nil {
		return RecordPage{}, fmt.Errorf("finish records list snapshot: %w", err)
	}
	return page, nil
}

func (store *RecordsStore) Read(ctx context.Context, recordID model.UUID, version *int64) (history.Version, int64, error) {
	if ctx == nil || store == nil || store.readers == nil {
		return history.Version{}, 0, fmt.Errorf("records reader and context are required: %w", ErrInvalidIndex)
	}
	if parsed, err := model.ParseUUID(string(recordID)); err != nil || parsed != recordID {
		return history.Version{}, 0, history.ErrInvalidRecord
	}
	if version != nil && (*version < 1 || *version > model.MaxSafeInteger) {
		return history.Version{}, 0, history.ErrInvalidRecord
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return history.Version{}, 0, fmt.Errorf("records store is closed: %w", ErrInvalidIndex)
	}
	tx, err := store.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return history.Version{}, 0, fmt.Errorf("begin records read snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT record_epoch FROM records_meta WHERE singleton=1`).Scan(&epoch); err != nil {
		return history.Version{}, 0, fmt.Errorf("read record epoch: %w", err)
	}
	query := `SELECT v.record_id,v.version,v.record_kind,v.declared_status,v.payload_json,v.source_refs_json,
		v.actor_id,v.reason,v.payload_hash,v.created_at FROM record_versions v
		WHERE v.record_id=? AND v.version=COALESCE(?,(SELECT version FROM record_heads WHERE record_id=?))`
	var requested any
	if version != nil {
		requested = *version
	}
	record, err := scanRecordVersion(tx.QueryRowContext(ctx, query, string(recordID), requested, string(recordID)), store.worktree)
	if errors.Is(err, sql.ErrNoRows) {
		return history.Version{}, epoch, history.ErrRecordNotFound
	}
	if err != nil {
		return history.Version{}, epoch, err
	}
	if err := tx.Commit(); err != nil {
		return history.Version{}, 0, fmt.Errorf("finish record read snapshot: %w", err)
	}
	return record, epoch, nil
}

func scanRecordVersion(row interface{ Scan(...any) error }, worktreeID model.UUID) (history.Version, error) {
	var record history.Version
	var id, kind, status, payload, refs, actor, payloadHash, createdAt string
	if err := row.Scan(&id, &record.Version, &kind, &status, &payload, &refs, &actor, &record.Reason, &payloadHash, &createdAt); err != nil {
		return history.Version{}, err
	}
	record.RecordID = model.UUID(id)
	record.Kind = history.Kind(kind)
	record.DeclaredStatus = history.Status(status)
	record.ActorID = model.Identifier(actor)
	record.Payload = json.RawMessage(payload)
	record.PayloadHash = model.Hash(payloadHash)
	record.CreatedAt = createdAt
	if _, err := model.ParseUUID(id); err != nil || !record.Kind.Valid() || !record.DeclaredStatus.ValidDeclared() ||
		record.Version < 1 || record.Version > model.MaxSafeInteger || history.ValidatePrincipal(record.ActorID) != nil || history.ValidateReason(record.Reason) != nil {
		return history.Version{}, fmt.Errorf("persisted record metadata is invalid: %w", history.ErrRecordStoreCorrupted)
	}
	canonicalPayload, payloadDigest, err := history.CanonicalPayload(record.Payload)
	if err != nil || string(canonicalPayload) != payload || payloadDigest != record.PayloadHash {
		return history.Version{}, fmt.Errorf("persisted record payload hash or encoding is invalid: %w", history.ErrRecordStoreCorrupted)
	}
	decoder := json.NewDecoder(strings.NewReader(refs))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record.SourceRefs); err != nil || record.SourceRefs == nil || len(record.SourceRefs) > history.MaxSourceRefs {
		return history.Version{}, fmt.Errorf("persisted source references are invalid: %w", history.ErrRecordStoreCorrupted)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return history.Version{}, fmt.Errorf("persisted source references contain trailing JSON: %w", history.ErrRecordStoreCorrupted)
	}
	if err := history.ValidateSourceRefs(worktreeID, record.SourceRefs); err != nil {
		return history.Version{}, fmt.Errorf("persisted source reference is invalid: %w", history.ErrRecordStoreCorrupted)
	}
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return history.Version{}, fmt.Errorf("persisted record timestamp is invalid: %w", history.ErrRecordStoreCorrupted)
	}
	return record, nil
}

func (store *RecordsStore) Propose(ctx context.Context, principal model.Identifier, input history.ProposeInput) (history.MutationResult, error) {
	if store == nil || history.ValidatePropose(store.worktree, principal, input) != nil {
		return history.MutationResult{}, history.ErrInvalidRecord
	}
	payload, payloadHash, err := history.CanonicalPayload(input.Payload)
	if err != nil {
		return history.MutationResult{}, err
	}
	input.Payload = payload
	refs := append([]model.SourceRef{}, input.SourceRefs...)
	requestJSON, err := json.Marshal(struct {
		Operation string               `json:"operation"`
		Input     history.ProposeInput `json:"input"`
	}{"propose", input})
	if err != nil {
		return history.MutationResult{}, fmt.Errorf("encode record proposal identity: %w", err)
	}
	requestHash := model.SHA256(requestJSON)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var result history.MutationResult
	err = store.withWriteTx(ctx, recordsMutationPeakGrowth, func(tx *writeTx) error {
		prior, exists, err := lookupMutationReceipt(ctx, tx, input.ClientMutationID)
		if err != nil {
			return err
		}
		if exists {
			if prior.principal != principal || prior.requestHash != requestHash {
				return history.ErrMutationConflict
			}
			record, err := readVersionByKey(ctx, tx, prior.recordID, prior.resultVersion, store.worktree)
			if err != nil {
				return err
			}
			result = history.MutationResult{Version: record, Replayed: true}
			return nil
		}
		recordID, err := newUUID()
		if err != nil {
			return fmt.Errorf("create record identity: %w", err)
		}
		version := history.Version{RecordID: recordID, Version: 1, Kind: input.Kind, DeclaredStatus: history.StatusProposed,
			Payload: payload, SourceRefs: refs, ActorID: principal, Reason: input.Reason, PayloadHash: payloadHash, CreatedAt: now}
		if err := insertRecordVersion(ctx, tx, version); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO record_heads(record_id,version) VALUES(?,1)`, string(recordID)); err != nil {
			return fmt.Errorf("create record head: %w", err)
		}
		if err := advanceRecordEpoch(ctx, tx); err != nil {
			return err
		}
		if err := insertMutationReceipt(ctx, tx, input.ClientMutationID, principal, requestHash, recordID, 1, now); err != nil {
			return err
		}
		result.Version = version
		return nil
	})
	if err != nil {
		return history.MutationResult{}, err
	}
	return result, nil
}

func (store *RecordsStore) Review(ctx context.Context, principal model.Identifier, input history.ReviewInput) (history.MutationResult, error) {
	if store == nil || history.ValidateReview(principal, input) != nil {
		return history.MutationResult{}, history.ErrInvalidRecord
	}
	requestJSON, err := json.Marshal(struct {
		Operation string              `json:"operation"`
		Input     history.ReviewInput `json:"input"`
	}{"review", input})
	if err != nil {
		return history.MutationResult{}, fmt.Errorf("encode record review identity: %w", err)
	}
	requestHash := model.SHA256(requestJSON)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var result history.MutationResult
	err = store.withWriteTx(ctx, recordsMutationPeakGrowth, func(tx *writeTx) error {
		prior, exists, err := lookupMutationReceipt(ctx, tx, input.ClientMutationID)
		if err != nil {
			return err
		}
		if exists {
			if prior.principal != principal || prior.requestHash != requestHash {
				return history.ErrMutationConflict
			}
			record, err := readVersionByKey(ctx, tx, prior.recordID, prior.resultVersion, store.worktree)
			if err != nil {
				return err
			}
			result = history.MutationResult{Version: record, Replayed: true}
			return nil
		}
		current, err := readCurrentVersion(ctx, tx, input.RecordID, store.worktree)
		if errors.Is(err, sql.ErrNoRows) {
			return history.ErrRecordNotFound
		}
		if err != nil {
			return err
		}
		if current.Version != input.ExpectedVersion {
			return history.ErrRecordConflict
		}
		if current.Version >= model.MaxSafeInteger {
			return history.ErrRecordConflict
		}
		status := history.StatusApproved
		switch input.Action {
		case history.ReviewRevoke:
			status = history.StatusRevoked
		case history.ReviewSupersede:
			status = history.StatusSuperseded
		}
		current.Version++
		current.DeclaredStatus = status
		current.ActorID = principal
		current.Reason = input.Reason
		current.CreatedAt = now
		if err := insertRecordVersion(ctx, tx, current); err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE record_heads SET version=? WHERE record_id=? AND version=?`, current.Version, string(current.RecordID), input.ExpectedVersion)
		if err != nil {
			return fmt.Errorf("compare and swap record head: %w", err)
		}
		rows, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return history.ErrRecordConflict
		}
		if err := advanceRecordEpoch(ctx, tx); err != nil {
			return err
		}
		if err := insertMutationReceipt(ctx, tx, input.ClientMutationID, principal, requestHash, current.RecordID, current.Version, now); err != nil {
			return err
		}
		result.Version = current
		return nil
	})
	if err != nil {
		return history.MutationResult{}, err
	}
	return result, nil
}

func (store *RecordsStore) withWriteTx(ctx context.Context, peak int64, write func(*writeTx) error) (retErr error) {
	if ctx == nil || store == nil || store.owner == nil || store.writer == nil || write == nil || peak < 1 || peak > model.MaxSafeInteger {
		return fmt.Errorf("invalid records write request: %w", ErrInvalidIndex)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return fmt.Errorf("records store is closed: %w", ErrInvalidIndex)
	}
	writerLease, err := store.owner.acquireWriter(ctx)
	if err != nil {
		return fmt.Errorf("acquire worktree writer lease for record mutation: %w", err)
	}
	defer func() {
		if err := writerLease.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release records writer lease: %w", err))
		}
	}()
	reservation, err := store.owner.admitDurableGrowth(ctx, recordsAdmissionOperation, model.AdmissionOwnedPath(recordsStorePath(store.worktree)), peak)
	if err != nil {
		return fmt.Errorf("admit durable record growth: %w", err)
	}
	defer func() {
		if err := reservation.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("settle durable record admission: %w", err))
		}
	}()
	conn, err := store.writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire records write connection: %w", err)
	}
	changedTimeout := false
	inTransaction := false
	discard := false
	defer func() {
		if inTransaction {
			if _, rollbackErr := conn.ExecContext(context.Background(), `ROLLBACK`); rollbackErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rollback records write transaction: %w", rollbackErr))
				discard = true
			}
		}
		if changedTimeout && !discard {
			if _, restoreErr := conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout=%d`, store.busyTimeoutMS)); restoreErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restore records SQLite busy timeout: %w", restoreErr))
				discard = true
			}
		}
		if discard {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		if closeErr := conn.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("release records write connection: %w", closeErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		discard = true
		return fmt.Errorf("set cancellable records SQLite busy wait: %w", err)
	}
	changedTimeout = true
	if err := retryBusy(ctx, time.Duration(store.busyTimeoutMS)*time.Millisecond, func() error {
		_, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`)
		return err
	}); err != nil {
		return fmt.Errorf("begin records write transaction: %w", err)
	}
	inTransaction = true
	transaction := &writeTx{conn: conn, busyTimeout: time.Duration(store.busyTimeoutMS) * time.Millisecond}
	if err := write(transaction); err != nil {
		return err
	}
	if err := retryBusy(ctx, transaction.busyTimeout, func() error {
		_, err := conn.ExecContext(ctx, `COMMIT`)
		return err
	}); err != nil {
		return fmt.Errorf("commit records write transaction: %w", err)
	}
	inTransaction = false
	return nil
}

type mutationReceipt struct {
	principal     model.Identifier
	requestHash   model.Hash
	recordID      model.UUID
	resultVersion int64
}

func lookupMutationReceipt(ctx context.Context, tx *writeTx, mutationID model.UUID) (mutationReceipt, bool, error) {
	var result mutationReceipt
	err := tx.QueryRowContext(ctx, `SELECT principal_id,request_hash,record_id,result_version FROM mutation_receipts WHERE client_mutation_id=?`, string(mutationID)).Scan(
		&result.principal, &result.requestHash, &result.recordID, &result.resultVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return mutationReceipt{}, false, nil
	}
	if err != nil {
		return mutationReceipt{}, false, fmt.Errorf("read record mutation receipt: %w", err)
	}
	if parsed, err := model.ParseUUID(string(result.recordID)); err != nil || parsed != result.recordID ||
		result.resultVersion < 1 || result.resultVersion > model.MaxSafeInteger ||
		history.ValidatePrincipal(result.principal) != nil {
		return mutationReceipt{}, false, fmt.Errorf("persisted mutation receipt is invalid: %w", history.ErrRecordStoreCorrupted)
	}
	if parsed, err := model.ParseHash(string(result.requestHash)); err != nil || parsed != result.requestHash {
		return mutationReceipt{}, false, fmt.Errorf("persisted mutation request hash is invalid: %w", history.ErrRecordStoreCorrupted)
	}
	return result, true, nil
}

func readVersionByKey(ctx context.Context, tx *writeTx, recordID model.UUID, version int64, worktreeID model.UUID) (history.Version, error) {
	query := `SELECT record_id,version,record_kind,declared_status,payload_json,source_refs_json,actor_id,reason,payload_hash,created_at
		FROM record_versions WHERE record_id=? AND version=?`
	return scanRecordVersion(tx.QueryRowContext(ctx, query, string(recordID), version), worktreeID)
}

func readCurrentVersion(ctx context.Context, tx *writeTx, recordID model.UUID, worktreeID model.UUID) (history.Version, error) {
	query := `SELECT v.record_id,v.version,v.record_kind,v.declared_status,v.payload_json,v.source_refs_json,v.actor_id,v.reason,v.payload_hash,v.created_at
		FROM record_heads h JOIN record_versions v ON v.record_id=h.record_id AND v.version=h.version WHERE h.record_id=?`
	return scanRecordVersion(tx.QueryRowContext(ctx, query, string(recordID)), worktreeID)
}

func insertRecordVersion(ctx context.Context, tx *writeTx, record history.Version) error {
	refs := append([]model.SourceRef{}, record.SourceRefs...)
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("encode record source references: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO record_versions(
		record_id,version,record_kind,declared_status,payload_json,source_refs_json,actor_id,reason,payload_hash,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?)`, string(record.RecordID), record.Version, string(record.Kind), string(record.DeclaredStatus), string(record.Payload), string(refsJSON),
		string(record.ActorID), record.Reason, string(record.PayloadHash), record.CreatedAt); err != nil {
		return fmt.Errorf("append immutable record version: %w", err)
	}
	return nil
}

func advanceRecordEpoch(ctx context.Context, tx *writeTx) error {
	result, err := tx.ExecContext(ctx, `UPDATE records_meta SET record_epoch=record_epoch+1 WHERE singleton=1 AND record_epoch<?`, model.MaxSafeInteger)
	if err != nil {
		return fmt.Errorf("advance record epoch: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("record epoch is exhausted or metadata is missing: %w", history.ErrRecordConflict)
	}
	return nil
}

func insertMutationReceipt(ctx context.Context, tx *writeTx, mutationID model.UUID, principal model.Identifier, requestHash model.Hash, recordID model.UUID, version int64, createdAt string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO mutation_receipts(client_mutation_id,principal_id,request_hash,record_id,result_version,created_at) VALUES(?,?,?,?,?,?)`,
		string(mutationID), string(principal), string(requestHash), string(recordID), version, createdAt); err != nil {
		return fmt.Errorf("persist record mutation receipt: %w", err)
	}
	return nil
}

func (store *RecordsStore) RecordEpoch(ctx context.Context) (int64, error) {
	if ctx == nil || store == nil || store.readers == nil {
		return 0, fmt.Errorf("records reader and context are required: %w", ErrInvalidIndex)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return 0, fmt.Errorf("records store is closed: %w", ErrInvalidIndex)
	}
	var epoch int64
	if err := store.readers.QueryRowContext(ctx, `SELECT record_epoch FROM records_meta WHERE singleton=1`).Scan(&epoch); err != nil {
		return 0, fmt.Errorf("read record epoch: %w", err)
	}
	if epoch < 0 || epoch > model.MaxSafeInteger {
		return 0, history.ErrRecordStoreCorrupted
	}
	return epoch, nil
}
