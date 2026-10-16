package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	_ "modernc.org/sqlite"
)

const (
	indexSchemaVersion = int64(2)
	indexReaderVersion = int64(2)
	defaultBusyTimeout = int64(5000)
	readerPoolSize     = 4
	writerPoolSize     = 1
)

var (
	ErrSchemaTooNew        = errors.New("SQLite index schema is newer than this reader")
	ErrInvalidIndex        = errors.New("SQLite file is not a supported index store")
	ErrWorktreeMismatch    = errors.New("SQLite index belongs to a different worktree")
	ErrInvalidStorePath    = errors.New("SQLite index path is not a regular owned file")
	ErrManifestUnavailable = errors.New("SQLite generation has no capture manifest reference")
)

//go:embed migrations/index/*.sql
var indexMigrations embed.FS

// Options describes one private index store. Create must be explicit because
// opening a typo must not silently create a second index beside the intended one.
type Options struct {
	Path                    string
	HomePath                string
	WorktreeID              model.UUID
	BusyTimeoutMS           int64
	LockWaitMS              int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
	Create                  bool
}

// Store owns a single-connection writer and a four-connection read-only pool.
// Home and worktree lifecycle leases remain held until every pool is closed.
type Store struct {
	path          string
	worktree      model.UUID
	incarnation   model.UUID
	busyTimeoutMS int64
	writer        *sql.DB
	readers       *sql.DB
	ownership     *storeOwnership
	writeGate     sync.RWMutex
	closed        bool
}

// openIndex opens an index, applying only shipped index migrations in order.
// A future schema is checked through a read-only connection before any setting
// or migration can modify the file. The writer lease and admission reservation
// cover every SQLite mutation performed while opening the index.
func openIndex(ctx context.Context, options Options) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidIndex)
	}
	path, timeout, err := normalizeOptions(&options)
	if err != nil {
		return nil, err
	}
	ownership, err := newStoreOwnership(ctx, options, false)
	if err != nil {
		return nil, err
	}
	keepOwnership := false
	defer func() {
		if !keepOwnership {
			_ = ownership.Close()
		}
	}()
	writerLease, err := ownership.acquireWriter(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire worktree writer lease before opening SQLite: %w", err)
	}
	keepWriterLease := false
	defer func() {
		if !keepWriterLease {
			_ = writerLease.Close()
		}
	}()
	reservation, err := ownership.admitIndexGrowth(ctx, indexOpenPeakGrowth)
	if err != nil {
		return nil, fmt.Errorf("admit SQLite open and migration growth: %w", err)
	}
	defer func() { _ = reservation.Close() }()
	info, err := os.Lstat(path)
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return nil, fmt.Errorf("inspect SQLite index path: %w", err)
	}
	if created && !options.Create {
		return nil, fmt.Errorf("SQLite index does not exist: %w", os.ErrNotExist)
	}
	fresh := created
	if !created {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", ErrInvalidStorePath, path)
		}
		fresh = info.Size() == 0
		if fresh && !options.Create {
			return nil, fmt.Errorf("empty SQLite index requires explicit creation: %w", ErrInvalidIndex)
		}
		if !fresh {
			if err := preflightIndex(ctx, path, timeout, options.WorktreeID); err != nil {
				return nil, err
			}
		}
	}
	if fresh {
		if options.WorktreeID == "" {
			return nil, fmt.Errorf("new SQLite index requires a worktree UUID: %w", ErrInvalidIndex)
		}
		worktreeID, err := model.ParseUUID(string(options.WorktreeID))
		if err != nil || worktreeID != options.WorktreeID {
			return nil, fmt.Errorf("new SQLite index requires a valid worktree UUID: %w", ErrInvalidIndex)
		}
	}

	mode := "rw"
	if fresh {
		mode = "rwc"
	}
	writer, err := openPool(ctx, path, mode, timeout, false, writerPoolSize)
	if err != nil {
		return nil, err
	}
	closeWriter := true
	defer func() {
		if closeWriter {
			_ = writer.Close()
		}
	}()

	conn, err := writer.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire SQLite writer connection: %w", err)
	}
	if err := setAndCheckWAL(ctx, conn, timeout); err != nil {
		_ = conn.Close()
		return nil, err
	}
	identity, err := migrateAndReadIdentity(ctx, conn, fresh, options.WorktreeID)
	if closeErr := conn.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("release SQLite migration connection: %w", closeErr)
	}
	if err != nil {
		return nil, err
	}
	if err := verifyPool(ctx, writer, 1, timeout, false); err != nil {
		return nil, fmt.Errorf("verify SQLite writer pool: %w", err)
	}

	readers, err := openPool(ctx, path, "ro", timeout, true, readerPoolSize)
	if err != nil {
		return nil, err
	}
	if err := verifyPool(ctx, readers, readerPoolSize, timeout, true); err != nil {
		_ = readers.Close()
		return nil, fmt.Errorf("verify SQLite reader pool: %w", err)
	}
	if err := reservation.Close(); err != nil {
		_ = readers.Close()
		return nil, fmt.Errorf("settle SQLite open reservation: %w", err)
	}
	if err := writerLease.Close(); err != nil {
		_ = readers.Close()
		return nil, fmt.Errorf("release worktree writer lease after open: %w", err)
	}
	closeWriter = false
	keepOwnership = true
	keepWriterLease = true
	return &Store{path: path, worktree: identity.worktree, incarnation: identity.incarnation, busyTimeoutMS: timeout, writer: writer, readers: readers, ownership: ownership}, nil
}

// OpenIndexReadOnly opens an existing store without running migrations or
// changing journal mode. Its pool is explicitly SQLite read-only and query-only.
func OpenIndexReadOnly(ctx context.Context, options Options) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidIndex)
	}
	if options.Create {
		return nil, fmt.Errorf("read-only SQLite open cannot create a store: %w", ErrInvalidIndex)
	}
	path, timeout, err := normalizeOptions(&options)
	if err != nil {
		return nil, err
	}
	ownership, err := newStoreOwnership(ctx, options, true)
	if err != nil {
		return nil, err
	}
	keepOwnership := false
	defer func() {
		if !keepOwnership {
			_ = ownership.Close()
		}
	}()
	if err := preflightIndex(ctx, path, timeout, options.WorktreeID); err != nil {
		return nil, err
	}
	readers, err := openPool(ctx, path, "ro", timeout, true, readerPoolSize)
	if err != nil {
		return nil, err
	}
	if err := verifyPool(ctx, readers, readerPoolSize, timeout, true); err != nil {
		_ = readers.Close()
		return nil, fmt.Errorf("verify SQLite reader pool: %w", err)
	}
	identity, err := readIdentity(ctx, readers)
	if err != nil {
		_ = readers.Close()
		return nil, err
	}
	keepOwnership = true
	return &Store{path: path, worktree: identity.worktree, incarnation: identity.incarnation, busyTimeoutMS: timeout, readers: readers, ownership: ownership}, nil
}

// ReadDB returns the bounded read-only connection pool for internal store users.
func (store *Store) ReadDB() *sql.DB {
	if store == nil {
		return nil
	}
	return store.readers
}

// WorktreeID identifies the index owner recorded in store_identity.
func (store *Store) WorktreeID() model.UUID {
	if store == nil {
		return ""
	}
	return store.worktree
}

// Incarnation identifies this physical index lifetime and changes only when a
// new index file is initialized.
func (store *Store) Incarnation() model.UUID {
	if store == nil {
		return ""
	}
	return store.incarnation
}

// Close closes both pools and reports both failures when necessary.
func (store *Store) Close() error {
	if store == nil {
		return nil
	}
	store.writeGate.Lock()
	defer store.writeGate.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	var writerErr, readerErr error
	if store.writer != nil {
		writerErr = store.writer.Close()
	}
	if store.readers != nil {
		readerErr = store.readers.Close()
	}
	var ownershipErr error
	if store.ownership != nil {
		ownershipErr = store.ownership.Close()
	}
	return errors.Join(writerErr, readerErr, ownershipErr)
}

func normalizeOptions(options *Options) (string, int64, error) {
	if strings.TrimSpace(options.Path) == "" {
		return "", 0, fmt.Errorf("%w: empty index path", ErrInvalidIndex)
	}
	if options.BusyTimeoutMS == 0 {
		options.BusyTimeoutMS = defaultBusyTimeout
	}
	if options.BusyTimeoutMS < 1 || options.BusyTimeoutMS > math.MaxInt32 {
		return "", 0, fmt.Errorf("%w: SQLite busy timeout must be within 1..%d ms", ErrInvalidIndex, int64(math.MaxInt32))
	}
	abs, err := filepath.Abs(options.Path)
	if err != nil {
		return "", 0, fmt.Errorf("resolve SQLite index path: %w", err)
	}
	return filepath.Clean(abs), options.BusyTimeoutMS, nil
}

type indexIdentity struct {
	worktree    model.UUID
	incarnation model.UUID
}

func preflightIndex(ctx context.Context, path string, timeout int64, expectedWorktree model.UUID) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect SQLite index: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrInvalidStorePath, path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("empty SQLite index requires explicit creation: %w", ErrInvalidIndex)
	}
	db, err := openPool(ctx, path, "ro", timeout, true, 1)
	if err != nil {
		return fmt.Errorf("open SQLite index read-only for compatibility check: %w", err)
	}
	defer db.Close()
	var metaCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_meta'`).Scan(&metaCount); err != nil {
		return fmt.Errorf("inspect SQLite schema metadata: %w", err)
	}
	if metaCount != 1 {
		var objectCount int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type IN ('table','view','trigger') AND name NOT LIKE 'sqlite_%'`).Scan(&objectCount); err != nil {
			return fmt.Errorf("inspect unmanaged SQLite schema: %w", err)
		}
		if objectCount == 0 {
			return fmt.Errorf("empty SQLite database requires explicit creation: %w", ErrInvalidIndex)
		}
		return fmt.Errorf("SQLite database has no schema_meta: %w", ErrInvalidIndex)
	}
	var version, minReader int64
	if err := db.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&version, &minReader); err != nil {
		return fmt.Errorf("read SQLite schema compatibility: %w", err)
	}
	if version < 1 || minReader < 1 || minReader > version {
		return fmt.Errorf("invalid SQLite schema metadata version=%d min_reader=%d: %w", version, minReader, ErrInvalidIndex)
	}
	if version > indexSchemaVersion || minReader > indexReaderVersion {
		return fmt.Errorf("found schema=%d min_reader=%d; supported schema=%d reader=%d: %w", version, minReader, indexSchemaVersion, indexReaderVersion, ErrSchemaTooNew)
	}
	if expectedWorktree != "" {
		want, err := model.ParseUUID(string(expectedWorktree))
		if err != nil || want != expectedWorktree {
			return fmt.Errorf("invalid expected worktree UUID: %w", ErrInvalidIndex)
		}
	}
	identity, err := readIdentity(ctx, db)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var generationCount int
		if qerr := db.QueryRowContext(ctx, `SELECT count(*) FROM generations`).Scan(&generationCount); qerr != nil {
			return fmt.Errorf("inspect identity-less SQLite index: %w", qerr)
		}
		if generationCount != 0 || expectedWorktree == "" {
			return fmt.Errorf("index identity is missing or ambiguous: %w", ErrInvalidIndex)
		}
		return nil
	}
	if expectedWorktree != "" && identity.worktree != expectedWorktree {
		return fmt.Errorf("index worktree=%s expected=%s: %w", identity.worktree, expectedWorktree, ErrWorktreeMismatch)
	}
	return validateRequiredSchema(ctx, db, version)
}

func openPool(ctx context.Context, path, mode string, timeout int64, readOnly bool, maxConnections int) (*sql.DB, error) {
	dsn, err := makeDSN(path, mode, timeout, readOnly)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite pool: %w", err)
	}
	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(maxConnections)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping SQLite pool: %w", err)
	}
	return db, nil
}

func makeDSN(path, mode string, timeout int64, readOnly bool) (string, error) {
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	fileURL := &url.URL{Scheme: "file", Path: uriPath}
	query := fileURL.Query()
	query.Set("mode", mode)
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(timeout, 10)+")")
	query.Add("_pragma", "synchronous(2)")
	if readOnly {
		query.Add("_pragma", "query_only(1)")
	}
	fileURL.RawQuery = query.Encode()
	return fileURL.String(), nil
}

func setAndCheckWAL(ctx context.Context, conn *sql.Conn, timeout int64) error {
	var mode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("SQLite journal mode is %q, want WAL: %w", mode, ErrInvalidIndex)
	}
	return verifyConnection(ctx, conn, timeout, false, true)
}

func verifyPool(ctx context.Context, db *sql.DB, size int, timeout int64, readOnly bool) error {
	connections := make([]*sql.Conn, 0, size)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for index := 0; index < size; index++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire pooled connection %d: %w", index+1, err)
		}
		connections = append(connections, conn)
		if err := verifyConnection(ctx, conn, timeout, readOnly, false); err != nil {
			return fmt.Errorf("pooled connection %d: %w", index+1, err)
		}
	}
	return nil
}

func verifyConnection(ctx context.Context, conn *sql.Conn, timeout int64, readOnly, requireWAL bool) error {
	var foreignKeys, busyTimeout, synchronous, queryOnly int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return fmt.Errorf("read busy_timeout: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
		return fmt.Errorf("read query_only: %w", err)
	}
	if foreignKeys != 1 || int64(busyTimeout) != timeout || synchronous != 2 || queryOnly != boolInt(readOnly) {
		return fmt.Errorf("SQLite connection settings foreign_keys=%d busy_timeout=%d synchronous=%d query_only=%d; want 1/%d/2/%d", foreignKeys, busyTimeout, synchronous, queryOnly, timeout, boolInt(readOnly))
	}
	if requireWAL || !readOnly {
		var mode string
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			return fmt.Errorf("read journal_mode: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("SQLite journal_mode=%q; want WAL", mode)
		}
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func migrateAndReadIdentity(ctx context.Context, conn *sql.Conn, fresh bool, expectedWorktree model.UUID) (indexIdentity, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return indexIdentity{}, fmt.Errorf("begin SQLite migration transaction: %w", err)
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
			return indexIdentity{}, fmt.Errorf("read migration version: %w", err)
		}
	}
	if version > indexSchemaVersion {
		return indexIdentity{}, fmt.Errorf("schema=%d supported=%d: %w", version, indexSchemaVersion, ErrSchemaTooNew)
	}
	if fresh {
		if err := applyMigration(ctx, tx, "migrations/index/001.sql"); err != nil {
			return indexIdentity{}, err
		}
		version = 1
	}
	if version < 2 {
		if err := applyMigration(ctx, tx, "migrations/index/002.sql"); err != nil {
			return indexIdentity{}, err
		}
		version = 2
	}
	var minReader int64
	if err := tx.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&version, &minReader); err != nil {
		return indexIdentity{}, fmt.Errorf("verify migrated schema metadata: %w", err)
	}
	if version != indexSchemaVersion || minReader > indexReaderVersion || minReader < 1 || minReader > version {
		return indexIdentity{}, fmt.Errorf("post-migration schema=%d min_reader=%d is unsupported: %w", version, minReader, ErrSchemaTooNew)
	}
	if err := validateRequiredSchema(ctx, tx, version); err != nil {
		return indexIdentity{}, err
	}
	var identity indexIdentity
	err = tx.QueryRowContext(ctx, `SELECT worktree_id,incarnation FROM store_identity WHERE singleton=1`).Scan(&identity.worktree, &identity.incarnation)
	if errors.Is(err, sql.ErrNoRows) {
		var generations int
		if qerr := tx.QueryRowContext(ctx, `SELECT count(*) FROM generations`).Scan(&generations); qerr != nil {
			return indexIdentity{}, fmt.Errorf("inspect unbound index generations: %w", qerr)
		}
		if generations != 0 || expectedWorktree == "" {
			return indexIdentity{}, fmt.Errorf("index has no owner identity: %w", ErrInvalidIndex)
		}
		parsed, parseErr := model.ParseUUID(string(expectedWorktree))
		if parseErr != nil || parsed != expectedWorktree {
			return indexIdentity{}, fmt.Errorf("invalid expected worktree UUID: %w", ErrInvalidIndex)
		}
		incarnation, makeErr := newUUID()
		if makeErr != nil {
			return indexIdentity{}, fmt.Errorf("create index incarnation: %w", makeErr)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO store_identity(singleton,worktree_id,incarnation) VALUES(1,?,?)`, expectedWorktree, incarnation); err != nil {
			return indexIdentity{}, fmt.Errorf("bind new SQLite index identity: %w", err)
		}
		identity = indexIdentity{worktree: expectedWorktree, incarnation: incarnation}
	} else if err != nil {
		return indexIdentity{}, fmt.Errorf("read SQLite index identity: %w", err)
	}
	if _, err := model.ParseUUID(string(identity.worktree)); err != nil {
		return indexIdentity{}, fmt.Errorf("invalid stored worktree identity: %w", ErrInvalidIndex)
	}
	if _, err := model.ParseUUID(string(identity.incarnation)); err != nil {
		return indexIdentity{}, fmt.Errorf("invalid stored index incarnation: %w", ErrInvalidIndex)
	}
	if expectedWorktree != "" && identity.worktree != expectedWorktree {
		return indexIdentity{}, fmt.Errorf("index worktree=%s expected=%s: %w", identity.worktree, expectedWorktree, ErrWorktreeMismatch)
	}
	if err := tx.Commit(); err != nil {
		return indexIdentity{}, fmt.Errorf("commit SQLite migrations: %w", err)
	}
	committed = true
	return identity, nil
}

func applyMigration(ctx context.Context, tx *sql.Tx, relativePath string) error {
	sqlBytes, err := indexMigrations.ReadFile(relativePath)
	if err != nil {
		return fmt.Errorf("read embedded migration %s: %w", relativePath, err)
	}
	if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("apply embedded migration %s: %w", relativePath, err)
	}
	return nil
}

type schemaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateRequiredSchema(ctx context.Context, query schemaQuerier, version int64) error {
	required := []string{"schema_meta", "store_identity", "generations", "active_generations", "content_blobs", "packages", "files", "symbols", "relations", "diagnostics", "capability_results", "search_documents", "search_fts"}
	if version >= 2 {
		required = append(required, "modules", "module_members")
	}
	for _, name := range required {
		var count int
		if err := query.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name=? AND type IN ('table','view')`, name).Scan(&count); err != nil {
			return fmt.Errorf("check SQLite schema object %s: %w", name, err)
		}
		if count != 1 {
			return fmt.Errorf("required SQLite schema object %s is missing: %w", name, ErrInvalidIndex)
		}
	}
	var foreignKeyViolations int
	if err := query.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil {
		return fmt.Errorf("check SQLite foreign keys: %w", err)
	}
	if foreignKeyViolations != 0 {
		return fmt.Errorf("SQLite index has %d foreign-key violations: %w", foreignKeyViolations, ErrInvalidIndex)
	}
	return nil
}

func readIdentity(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (indexIdentity, error) {
	var identity indexIdentity
	err := query.QueryRowContext(ctx, `SELECT worktree_id,incarnation FROM store_identity WHERE singleton=1`).Scan(&identity.worktree, &identity.incarnation)
	if err != nil {
		return indexIdentity{}, err
	}
	if _, err := model.ParseUUID(string(identity.worktree)); err != nil {
		return indexIdentity{}, fmt.Errorf("invalid stored worktree identity: %w", ErrInvalidIndex)
	}
	if _, err := model.ParseUUID(string(identity.incarnation)); err != nil {
		return indexIdentity{}, fmt.Errorf("invalid stored index incarnation: %w", ErrInvalidIndex)
	}
	return identity, nil
}

func newUUID() (model.UUID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	text := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
	return model.ParseUUID(text)
}
