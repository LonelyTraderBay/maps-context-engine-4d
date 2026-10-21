package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	_ "modernc.org/sqlite"
)

type SQLitePolicyReader struct {
	home string
}

func NewSQLitePolicyReader(home string) (*SQLitePolicyReader, error) {
	if home == "" || !filepath.IsAbs(home) {
		return nil, fmt.Errorf("records policy reader requires an absolute home path: %w", ErrInvalidRequest)
	}
	return &SQLitePolicyReader{home: filepath.Clean(home)}, nil
}

func (reader *SQLitePolicyReader) CurrentPolicy(ctx context.Context, worktreeID model.UUID) (PolicyState, error) {
	if ctx == nil || reader == nil {
		return PolicyState{}, ErrInvalidRequest
	}
	if parsed, err := model.ParseUUID(string(worktreeID)); err != nil || parsed != worktreeID {
		return PolicyState{}, ErrInvalidRequest
	}
	path := filepath.Join(reader.home, "catalog.sqlite")
	db, err := openCatalogReader(ctx, reader.home, path)
	if err != nil {
		return PolicyState{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PolicyState{}, fmt.Errorf("begin current record policy snapshot: %w", err)
	}
	defer tx.Rollback()
	var schemaVersion, minReader int64
	if err := tx.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&schemaVersion, &minReader); err != nil || schemaVersion != 1 || minReader < 1 || minReader > schemaVersion {
		return PolicyState{}, fmt.Errorf("catalog schema is unavailable to record reads")
	}
	var state PolicyState
	var rawConfig string
	err = tx.QueryRowContext(ctx, `SELECT ci.incarnation,w.binding_revision,w.status,w.canonical_root,
		h.policy_digest,h.policy_epoch,p.config_json
		FROM catalog_identity ci
		JOIN worktrees w ON w.worktree_id=?
		JOIN worktree_policy_heads h ON h.worktree_id=w.worktree_id
		JOIN policies p ON p.worktree_id=h.worktree_id AND p.policy_digest=h.policy_digest
		WHERE ci.singleton=1`, string(worktreeID)).Scan(
		&state.CatalogIncarnation, &state.BindingRevision, &state.Status, &state.CanonicalRoot,
		&state.Digest, &state.Epoch, &rawConfig)
	if err != nil {
		return PolicyState{}, fmt.Errorf("read current worktree record policy: %w", err)
	}
	state.WorktreeID = worktreeID
	if len(rawConfig) == 0 || len(rawConfig) > 64<<10 {
		return PolicyState{}, fmt.Errorf("current worktree policy exceeds its bounded encoding")
	}
	decoder := json.NewDecoder(strings.NewReader(rawConfig))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state.Config); err != nil {
		return PolicyState{}, fmt.Errorf("decode current worktree policy: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return PolicyState{}, fmt.Errorf("current worktree policy has trailing JSON")
	}
	if err := validatePolicyState(worktreeID, state); err != nil {
		return PolicyState{}, err
	}
	if err := tx.Commit(); err != nil {
		return PolicyState{}, fmt.Errorf("finish current worktree policy snapshot: %w", err)
	}
	return state, nil
}

func openCatalogReader(ctx context.Context, home, path string) (*sql.DB, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(path) || filepath.Clean(filepath.Dir(path)) != filepath.Clean(home) {
		return nil, ErrInvalidRequest
	}
	rootInfo, err := os.Lstat(home)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("catalog home is unavailable or not a regular directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("catalog policy database is unavailable or not a regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog policy database: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve catalog home: %w", err)
	}
	expected := filepath.Join(resolvedParent, filepath.Base(path))
	if !sameCatalogPath(filepath.Clean(expected), filepath.Clean(resolved)) {
		return nil, fmt.Errorf("catalog policy path resolved outside its home")
	}
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	fileURL := &url.URL{Scheme: "file", Path: uriPath}
	query := fileURL.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "query_only(1)")
	fileURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", fileURL.String())
	if err != nil {
		return nil, fmt.Errorf("open read-only catalog policy database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read-only catalog policy database is unavailable: %w", err)
	}
	return db, nil
}

func sameCatalogPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
