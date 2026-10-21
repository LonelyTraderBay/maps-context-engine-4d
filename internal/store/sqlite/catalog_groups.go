package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	_ "modernc.org/sqlite"
)

const (
	groupCatalogBusyTimeoutMS  = int64(5_000)
	groupCatalogMaxPage        = int64(100)
	groupCursorVersion         = int64(1)
	maxGroupNamespaceJSONBytes = 32 << 10
)

type GroupCatalogStore struct {
	root  *mceruntime.Root
	lease *mceruntime.Lease
	db    *sql.DB
	mu    sync.RWMutex
	close bool
}

type groupListCursor struct {
	FormatVersion      int64      `json:"format_version"`
	CatalogIncarnation model.UUID `json:"catalog_incarnation"`
	CatalogWatermark   string     `json:"catalog_watermark"`
	AfterGroupID       model.UUID `json:"after_group_id"`
}

// OpenGroupCatalog opens only an existing, verified catalog database and
// retains a shared home-lifecycle lease for the full lifetime of the handle.
func OpenGroupCatalog(ctx context.Context, home string) (_ *GroupCatalogStore, retErr error) {
	if ctx == nil || home == "" || !filepath.IsAbs(home) {
		return nil, fmt.Errorf("group catalog requires an absolute home and context")
	}
	root, err := mceruntime.OpenRoot(filepath.Clean(home))
	if err != nil {
		return nil, fmt.Errorf("open verified group catalog home: %w", err)
	}
	keepRoot := false
	defer func() {
		if !keepRoot {
			retErr = errors.Join(retErr, root.Close())
		}
	}()
	lease, err := mceruntime.AcquireHomeLifecycle(ctx, root, mceruntime.LockShared, time.Duration(groupCatalogBusyTimeoutMS)*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire shared catalog lifecycle lease: %w", err)
	}
	keepLease := false
	defer func() {
		if !keepLease {
			retErr = errors.Join(retErr, lease.Close())
		}
	}()
	info, err := os.Lstat(filepath.Join(filepath.Clean(home), "catalog.sqlite"))
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 {
		return nil, fmt.Errorf("existing regular catalog.sqlite is required")
	}
	file, err := root.OpenRegularFile("catalog.sqlite")
	if err != nil {
		return nil, fmt.Errorf("verify catalog database beneath home: %w", err)
	}
	openedInfo, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || openedInfo == nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() {
		return nil, errors.Join(fmt.Errorf("catalog database changed during verification"), statErr, closeErr)
	}
	dsn, err := groupCatalogDSN(filepath.Join(filepath.Clean(home), "catalog.sqlite"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open group catalog database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	closeDB := true
	defer func() {
		if closeDB {
			retErr = errors.Join(retErr, db.Close())
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("catalog database is unavailable: %w", err)
	}
	if _, err := readGroupCatalogIdentity(ctx, db); err != nil {
		return nil, fmt.Errorf("catalog identity or schema is unavailable: %w", err)
	}
	store := &GroupCatalogStore{root: root, lease: lease, db: db}
	closeDB, keepLease, keepRoot = false, true, true
	return store, nil
}

func (store *GroupCatalogStore) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.close {
		return nil
	}
	store.close = true
	return errors.Join(store.db.Close(), store.lease.Close(), store.root.Close())
}

func (store *GroupCatalogStore) GetGroup(ctx context.Context, groupID model.UUID) (catalog.GroupSnapshot, error) {
	if ctx == nil || !validGroupID(groupID) {
		return catalog.GroupSnapshot{}, fmt.Errorf("invalid group read: %w", model.ErrInvalidValue)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(); err != nil {
		return catalog.GroupSnapshot{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("begin group snapshot: %w", err)
	}
	defer tx.Rollback()
	snapshot, err := getGroupSnapshotTx(ctx, tx, groupID)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("finish group snapshot: %w", err)
	}
	return snapshot, nil
}

func (store *GroupCatalogStore) ListGroups(ctx context.Context, limit int64, cursor string) (catalog.GroupListPage, error) {
	if ctx == nil || limit < 1 || limit > groupCatalogMaxPage || len(cursor) > 32<<10 {
		return catalog.GroupListPage{}, fmt.Errorf("invalid group page request: %w", model.ErrInvalidValue)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(); err != nil {
		return catalog.GroupListPage{}, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return catalog.GroupListPage{}, fmt.Errorf("begin group list snapshot: %w", err)
	}
	defer tx.Rollback()
	incarnation, err := readGroupCatalogIdentity(ctx, tx)
	if err != nil {
		return catalog.GroupListPage{}, err
	}
	watermark, err := readGroupWatermark(ctx, tx)
	if err != nil {
		return catalog.GroupListPage{}, err
	}
	var afterID model.UUID
	if cursor != "" {
		decoded, err := decodeGroupCursor(cursor)
		if err != nil || decoded.CatalogIncarnation != incarnation || decoded.CatalogWatermark != watermark {
			return catalog.GroupListPage{}, ports.ErrGroupStalePage
		}
		afterID = decoded.AfterGroupID
	}
	groups, hasMore, err := readGroupPageTx(ctx, tx, afterID, watermark, limit)
	if err != nil {
		return catalog.GroupListPage{}, err
	}
	page := catalog.GroupListPage{Groups: groups, CatalogIncarnation: incarnation, CatalogWatermark: watermark, HasMore: hasMore}
	if hasMore && len(groups) > 0 {
		page.NextCursor, err = encodeGroupCursor(groupListCursor{
			FormatVersion: groupCursorVersion, CatalogIncarnation: incarnation, CatalogWatermark: watermark,
			AfterGroupID: groups[len(groups)-1].Config.GroupID,
		})
		if err != nil {
			return catalog.GroupListPage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return catalog.GroupListPage{}, fmt.Errorf("finish group list snapshot: %w", err)
	}
	return page, nil
}

func (store *GroupCatalogStore) PutGroup(ctx context.Context, config catalog.GroupConfig, expectedRevision int64) (catalog.GroupSnapshot, error) {
	if ctx == nil || expectedRevision < 0 || expectedRevision > model.MaxSafeInteger {
		return catalog.GroupSnapshot{}, fmt.Errorf("invalid group put request: %w", model.ErrInvalidValue)
	}
	if err := catalog.ValidateGroupConfig(config); err != nil {
		return catalog.GroupSnapshot{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(); err != nil {
		return catalog.GroupSnapshot{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("begin group put: %w", err)
	}
	defer tx.Rollback()
	incarnation, err := lockAndReadGroupCatalog(ctx, tx)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	watermark, err := readGroupWatermark(ctx, tx)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	now, err := nextGroupTimestamp(watermark)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	var existingRevision int64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT revision,status FROM repo_groups WHERE group_id=?`, string(config.GroupID)).Scan(&existingRevision, &status)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return catalog.GroupSnapshot{}, fmt.Errorf("read current group revision: %w", err)
	}
	var revision int64
	if created {
		if expectedRevision != 0 {
			return catalog.GroupSnapshot{}, ports.ErrGroupConflict
		}
		revision = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO repo_groups(group_id,name,status,revision,created_at,updated_at) VALUES(?,?,'active',?,?,?)`,
			string(config.GroupID), config.Name, revision, now, now)
	} else {
		if existingRevision != expectedRevision || existingRevision >= model.MaxSafeInteger {
			return catalog.GroupSnapshot{}, ports.ErrGroupConflict
		}
		revision = existingRevision + 1
		_, err = tx.ExecContext(ctx, `UPDATE repo_groups SET name=?,status='active',revision=?,updated_at=? WHERE group_id=? AND revision=?`,
			config.Name, revision, now, string(config.GroupID), expectedRevision)
	}
	if err != nil {
		return catalog.GroupSnapshot{}, groupWriteError("write group metadata", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=?`, string(config.GroupID)); err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("replace group members: %w", err)
	}
	for _, member := range config.Members {
		namespace, err := json.Marshal(member.Namespace)
		if err != nil {
			return catalog.GroupSnapshot{}, fmt.Errorf("encode group namespace: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,service_id,worktree_id,profile_id,namespace_json) VALUES(?,?,?,?,?)`,
			string(config.GroupID), string(member.ServiceID), string(member.WorktreeID), string(member.ProfileID), string(namespace)); err != nil {
			return catalog.GroupSnapshot{}, groupWriteError("write declared group member", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return catalog.GroupSnapshot{}, groupWriteError("commit group put", err)
	}
	return catalog.GroupSnapshot{Group: catalog.GroupSummary{Config: cloneGroupConfig(config), Revision: revision, Status: "active"}, CatalogIncarnation: incarnation}, nil
}

func (store *GroupCatalogStore) RemoveGroup(ctx context.Context, groupID model.UUID, expectedRevision int64) (catalog.GroupSnapshot, error) {
	if ctx == nil || !validGroupID(groupID) || expectedRevision < 0 || expectedRevision > model.MaxSafeInteger {
		return catalog.GroupSnapshot{}, fmt.Errorf("invalid group remove request: %w", model.ErrInvalidValue)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := store.checkOpen(); err != nil {
		return catalog.GroupSnapshot{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("begin group remove: %w", err)
	}
	defer tx.Rollback()
	incarnation, err := lockAndReadGroupCatalog(ctx, tx)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	watermark, err := readGroupWatermark(ctx, tx)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	var revision int64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT revision,status FROM repo_groups WHERE group_id=?`, string(groupID)).Scan(&revision, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.GroupSnapshot{}, ports.ErrGroupNotFound
	}
	if err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("read current group revision: %w", err)
	}
	if revision != expectedRevision || status != "active" || revision >= model.MaxSafeInteger {
		return catalog.GroupSnapshot{}, ports.ErrGroupConflict
	}
	now, err := nextGroupTimestamp(watermark)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	nextRevision := revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE repo_groups SET status='removed',revision=?,updated_at=? WHERE group_id=? AND revision=? AND status='active'`,
		nextRevision, now, string(groupID), expectedRevision); err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("tombstone group: %w", err)
	}
	snapshot, err := getGroupSnapshotTx(ctx, tx, groupID)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("commit group tombstone: %w", err)
	}
	if snapshot.Group.Revision != nextRevision || snapshot.Group.Status != "removed" || snapshot.CatalogIncarnation != incarnation {
		return catalog.GroupSnapshot{}, fmt.Errorf("group tombstone readback mismatch")
	}
	return snapshot, nil
}

func (store *GroupCatalogStore) checkOpen() error {
	if store == nil || store.close || store.db == nil || store.root == nil || store.lease == nil {
		return sql.ErrConnDone
	}
	return nil
}

func getGroupSnapshotTx(ctx context.Context, tx *sql.Tx, groupID model.UUID) (catalog.GroupSnapshot, error) {
	incarnation, err := readGroupCatalogIdentity(ctx, tx)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	var config catalog.GroupConfig
	var status string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT group_id,name,status,revision FROM repo_groups WHERE group_id=?`, string(groupID)).Scan(
		&config.GroupID, &config.Name, &status, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.GroupSnapshot{}, ports.ErrGroupNotFound
	}
	if err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("read group metadata: %w", err)
	}
	if (status != "active" && status != "removed") || revision < 1 || revision > model.MaxSafeInteger {
		return catalog.GroupSnapshot{}, fmt.Errorf("stored group metadata is invalid")
	}
	config.Members, err = readGroupMembersTx(ctx, tx, groupID)
	if err != nil {
		return catalog.GroupSnapshot{}, err
	}
	if err := catalog.ValidateGroupConfig(config); err != nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("stored group violates its contract: %w", err)
	}
	return catalog.GroupSnapshot{Group: catalog.GroupSummary{Config: config, Revision: revision, Status: status}, CatalogIncarnation: incarnation}, nil
}

func readGroupMembersTx(ctx context.Context, tx *sql.Tx, groupID model.UUID) ([]catalog.GroupMemberConfig, error) {
	rows, err := tx.QueryContext(ctx, `SELECT service_id,worktree_id,profile_id,namespace_json FROM group_members WHERE group_id=? ORDER BY service_id`, string(groupID))
	if err != nil {
		return nil, fmt.Errorf("read declared group members: %w", err)
	}
	defer rows.Close()
	members := make([]catalog.GroupMemberConfig, 0, 8)
	for rows.Next() {
		var member catalog.GroupMemberConfig
		var namespaceJSON string
		if err := rows.Scan(&member.ServiceID, &member.WorktreeID, &member.ProfileID, &namespaceJSON); err != nil {
			return nil, fmt.Errorf("scan declared group member: %w", err)
		}
		if err := decodeGroupNamespace(namespaceJSON, &member.Namespace); err != nil {
			return nil, err
		}
		members = append(members, member)
		if len(members) > 32 {
			return nil, fmt.Errorf("stored group exceeds its member cap")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finish group member read: %w", err)
	}
	return members, nil
}

func readGroupPageTx(ctx context.Context, tx *sql.Tx, afterID model.UUID, watermark string, limit int64) ([]catalog.GroupSummary, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT group_id,name,status,revision FROM repo_groups WHERE group_id>? AND updated_at<=? ORDER BY group_id LIMIT ?`,
		string(afterID), watermark, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("query group page: %w", err)
	}
	groups := make([]catalog.GroupSummary, 0, limit+1)
	for rows.Next() {
		var summary catalog.GroupSummary
		if err := rows.Scan(&summary.Config.GroupID, &summary.Config.Name, &summary.Status, &summary.Revision); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan group page: %w", err)
		}
		if (summary.Status != "active" && summary.Status != "removed") || summary.Revision < 1 || summary.Revision > model.MaxSafeInteger {
			_ = rows.Close()
			return nil, false, fmt.Errorf("stored group page metadata is invalid")
		}
		groups = append(groups, summary)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, fmt.Errorf("finish group page query: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, false, fmt.Errorf("close group page query: %w", err)
	}
	hasMore := int64(len(groups)) > limit
	if hasMore {
		groups = groups[:limit]
	}
	if len(groups) == 0 {
		return []catalog.GroupSummary{}, false, nil
	}
	placeholders := make([]string, len(groups))
	args := make([]any, len(groups))
	indices := make(map[model.UUID]int, len(groups))
	for index := range groups {
		placeholders[index] = "?"
		args[index] = string(groups[index].Config.GroupID)
		indices[groups[index].Config.GroupID] = index
		groups[index].Config.Members = make([]catalog.GroupMemberConfig, 0, 8)
	}
	memberRows, err := tx.QueryContext(ctx, `SELECT group_id,service_id,worktree_id,profile_id,namespace_json FROM group_members WHERE group_id IN (`+strings.Join(placeholders, ",")+") ORDER BY group_id,service_id", args...)
	if err != nil {
		return nil, false, fmt.Errorf("query page group members: %w", err)
	}
	for memberRows.Next() {
		var groupID model.UUID
		var member catalog.GroupMemberConfig
		var namespaceJSON string
		if err := memberRows.Scan(&groupID, &member.ServiceID, &member.WorktreeID, &member.ProfileID, &namespaceJSON); err != nil {
			_ = memberRows.Close()
			return nil, false, fmt.Errorf("scan page group member: %w", err)
		}
		index, ok := indices[groupID]
		if !ok || len(groups[index].Config.Members) >= 32 {
			_ = memberRows.Close()
			return nil, false, fmt.Errorf("stored group page member set is invalid")
		}
		if err := decodeGroupNamespace(namespaceJSON, &member.Namespace); err != nil {
			_ = memberRows.Close()
			return nil, false, err
		}
		groups[index].Config.Members = append(groups[index].Config.Members, member)
	}
	if err := memberRows.Err(); err != nil {
		_ = memberRows.Close()
		return nil, false, fmt.Errorf("finish page group member query: %w", err)
	}
	if err := memberRows.Close(); err != nil {
		return nil, false, fmt.Errorf("close page group member query: %w", err)
	}
	for _, summary := range groups {
		if err := catalog.ValidateGroupConfig(summary.Config); err != nil {
			return nil, false, fmt.Errorf("stored group page violates its contract: %w", err)
		}
	}
	return groups, hasMore, nil
}

func lockAndReadGroupCatalog(ctx context.Context, tx *sql.Tx) (model.UUID, error) {
	// The first write in every mutation serializes cross-process CAS before the
	// revision read. It does not change the incarnation or any catalog row.
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_identity SET incarnation=incarnation WHERE singleton=1`); err != nil {
		return "", fmt.Errorf("acquire catalog write slot: %w", err)
	}
	return readGroupCatalogIdentity(ctx, tx)
}

func readGroupCatalogIdentity(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (model.UUID, error) {
	var schemaVersion, minReader int64
	if err := query.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&schemaVersion, &minReader); err != nil || schemaVersion != 1 || minReader < 1 || minReader > schemaVersion {
		return "", fmt.Errorf("catalog schema version is unavailable")
	}
	var raw string
	if err := query.QueryRowContext(ctx, `SELECT incarnation FROM catalog_identity WHERE singleton=1`).Scan(&raw); err != nil {
		return "", fmt.Errorf("catalog incarnation is unavailable: %w", err)
	}
	incarnation, err := model.ParseUUID(raw)
	if err != nil || string(incarnation) != raw {
		return "", fmt.Errorf("catalog incarnation is invalid")
	}
	return incarnation, nil
}

func readGroupWatermark(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	// S44 has no catalog-wide group revision. T046 advances updated_at strictly
	// under the SQLite write slot and uses its lexical maximum as the list epoch.
	var watermark string
	if err := query.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),'') FROM repo_groups`).Scan(&watermark); err != nil {
		return "", fmt.Errorf("read catalog group watermark: %w", err)
	}
	if watermark != "" {
		if _, err := time.Parse(time.RFC3339Nano, watermark); err != nil {
			return "", fmt.Errorf("stored group watermark is invalid: %w", err)
		}
	}
	return watermark, nil
}

func nextGroupTimestamp(watermark string) (string, error) {
	now := time.Now().UTC()
	if watermark != "" {
		previous, err := time.Parse(time.RFC3339Nano, watermark)
		if err != nil {
			return "", fmt.Errorf("stored group watermark is invalid: %w", err)
		}
		if !now.After(previous) {
			now = previous.Add(time.Nanosecond)
		}
	}
	return now.Format("2006-01-02T15:04:05.000000000Z07:00"), nil
}

func encodeGroupCursor(cursor groupListCursor) (string, error) {
	if cursor.FormatVersion != groupCursorVersion || !validGroupID(cursor.CatalogIncarnation) || !validGroupID(cursor.AfterGroupID) {
		return "", fmt.Errorf("invalid group list cursor fields")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CatalogWatermark); err != nil {
		return "", fmt.Errorf("invalid group list cursor watermark: %w", err)
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode group list cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeGroupCursor(token string) (groupListCursor, error) {
	var cursor groupListCursor
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) > 24<<10 {
		return cursor, fmt.Errorf("decode group list cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return cursor, fmt.Errorf("decode group list cursor: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return cursor, fmt.Errorf("group list cursor contains trailing JSON")
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(canonical, raw) || cursor.FormatVersion != groupCursorVersion ||
		!validGroupID(cursor.CatalogIncarnation) || !validGroupID(cursor.AfterGroupID) {
		return cursor, fmt.Errorf("group list cursor is not canonical")
	}
	if _, err := time.Parse(time.RFC3339Nano, cursor.CatalogWatermark); err != nil {
		return cursor, fmt.Errorf("group list cursor watermark is invalid: %w", err)
	}
	return cursor, nil
}

func decodeGroupNamespace(raw string, namespace *catalog.ServiceNamespace) error {
	if len(raw) == 0 || len(raw) > maxGroupNamespaceJSONBytes || namespace == nil {
		return fmt.Errorf("stored group namespace exceeds its bound")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(namespace); err != nil {
		return fmt.Errorf("decode stored group namespace: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("stored group namespace contains trailing JSON")
	}
	return namespace.Validate()
}

func groupCatalogDSN(path string) (string, error) {
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	fileURL := &url.URL{Scheme: "file", Path: uriPath}
	query := fileURL.Query()
	query.Set("mode", "rw")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "synchronous(2)")
	fileURL.RawQuery = query.Encode()
	return fileURL.String(), nil
}

func validGroupID(groupID model.UUID) bool {
	parsed, err := model.ParseUUID(string(groupID))
	return err == nil && parsed == groupID
}

func cloneGroupConfig(config catalog.GroupConfig) catalog.GroupConfig {
	clone := config
	clone.Members = append([]catalog.GroupMemberConfig(nil), config.Members...)
	for index := range clone.Members {
		namespace := &clone.Members[index].Namespace
		if namespace.BaseOrigin != nil {
			value := *namespace.BaseOrigin
			namespace.BaseOrigin = &value
		}
		if namespace.RoutePrefix != nil {
			value := *namespace.RoutePrefix
			namespace.RoutePrefix = &value
		}
		if namespace.ContractVersion != nil {
			value := *namespace.ContractVersion
			namespace.ContractVersion = &value
		}
	}
	return clone
}

func groupWriteError(operation string, err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint failed: repo_groups.name") || strings.Contains(message, "unique constraint failed: repo_groups.group_id") {
		return fmt.Errorf("%s: %w", operation, ports.ErrGroupConflict)
	}
	if strings.Contains(message, "foreign key constraint failed") {
		return fmt.Errorf("%s: declared worktree/profile is unavailable: %w", operation, model.ErrInvalidValue)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
