package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	modernsqlite "modernc.org/sqlite"
)

const sqliteBackupPageBatch = int32(128)

// backupSQLiteFile uses SQLite's online backup API and publishes only into a
// new, home-owned destination. The caller holds the appropriate lifecycle
// lease; this helper does not coordinate stores on its own.
func backupSQLiteFile(ctx context.Context, root *mceruntime.Root, sourceRelative, destinationRelative string, busyTimeoutMS int64) (model.Hash, int64, error) {
	var empty model.Hash
	if ctx == nil || root == nil || busyTimeoutMS < 1 || busyTimeoutMS > math.MaxInt32 ||
		model.AdmissionOwnedPath(sourceRelative).Validate() != nil || model.AdmissionOwnedPath(destinationRelative).Validate() != nil {
		return empty, 0, fmt.Errorf("invalid SQLite backup request: %w", ErrInvalidIndex)
	}
	if err := ctx.Err(); err != nil {
		return empty, 0, err
	}
	source, err := root.OpenRegularFile(sourceRelative)
	if err != nil {
		return empty, 0, fmt.Errorf("open verified SQLite backup source: %w", err)
	}
	sourceInfo, statErr := source.Stat()
	closeErr := source.Close()
	if statErr != nil || closeErr != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() < 1 {
		return empty, 0, errors.Join(fmt.Errorf("SQLite backup source is not a nonempty regular file: %w", ErrInvalidStorePath), statErr, closeErr)
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(destinationRelative)))
	if parent != "." {
		if err := root.EnsurePrivateDirs(parent); err != nil {
			return empty, 0, fmt.Errorf("prepare private SQLite backup destination: %w", err)
		}
	}
	if _, err := root.PublishAtomicFile(destinationRelative, nil, 0o600); err != nil {
		return empty, 0, fmt.Errorf("create new SQLite backup destination without overwrite: %w", err)
	}
	rootPath, err := root.FinalPath()
	if err != nil {
		return empty, 0, err
	}
	sourcePath := comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(sourceRelative)))
	destinationPath := comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(destinationRelative)))
	destinationURI, err := makeDSN(destinationPath, "rw", busyTimeoutMS, false)
	if err != nil {
		return empty, 0, fmt.Errorf("build SQLite backup destination URI: %w", err)
	}
	sourceDB, err := openPool(ctx, sourcePath, "ro", busyTimeoutMS, true, 1)
	if err != nil {
		return empty, 0, fmt.Errorf("open SQLite backup source read-only: %w", err)
	}
	backupErr := runOnlineSQLiteBackup(ctx, sourceDB, destinationURI)
	closeDBErr := sourceDB.Close()
	if backupErr != nil || closeDBErr != nil {
		var copyErr error
		if backupErr != nil {
			copyErr = fmt.Errorf("copy SQLite backup pages: %w", backupErr)
		}
		return empty, 0, errors.Join(copyErr, closeDBErr)
	}
	return hashOwnedRegularFile(root, destinationRelative)
}

func runOnlineSQLiteBackup(ctx context.Context, source *sql.DB, destinationURI string) (retErr error) {
	if ctx == nil || source == nil || destinationURI == "" {
		return fmt.Errorf("invalid online SQLite backup connection: %w", ErrInvalidIndex)
	}
	connection, err := source.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire SQLite backup source connection: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, connection.Close()) }()
	retErr = connection.Raw(func(driverConnection any) (rawErr error) {
		backuper, ok := driverConnection.(interface {
			NewBackup(string) (*modernsqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver does not expose online backup API: %w", ErrInvalidIndex)
		}
		backup, err := backuper.NewBackup(destinationURI)
		if err != nil {
			return fmt.Errorf("initialize SQLite online backup: %w", err)
		}
		finished := false
		defer func() {
			if !finished {
				rawErr = errors.Join(rawErr, backup.Finish())
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(sqliteBackupPageBatch)
			if err != nil {
				return fmt.Errorf("copy bounded SQLite backup page batch: %w", err)
			}
			if !more {
				break
			}
		}
		finished = true
		return backup.Finish()
	})
	if retErr != nil {
		return fmt.Errorf("run SQLite online backup: %w", retErr)
	}
	return nil
}

func hashOwnedRegularFile(root *mceruntime.Root, relative string) (model.Hash, int64, error) {
	var empty model.Hash
	file, err := root.OpenRegularFile(relative)
	if err != nil {
		return empty, 0, fmt.Errorf("open owned SQLite backup output: %w", err)
	}
	info, statErr := file.Stat()
	hasher := sha256.New()
	byteLength, copyErr := io.Copy(hasher, io.LimitReader(file, model.MaxSafeInteger+1))
	closeErr := file.Close()
	if statErr != nil || copyErr != nil || closeErr != nil {
		return empty, 0, errors.Join(fmt.Errorf("hash owned SQLite backup output: %w", ErrInvalidStorePath), statErr, copyErr, closeErr)
	}
	if !info.Mode().IsRegular() || byteLength < 1 || byteLength != info.Size() || byteLength > model.MaxSafeInteger {
		return empty, 0, fmt.Errorf("owned SQLite backup output has invalid length or type: %w", ErrInvalidStorePath)
	}
	return model.Hash(hex.EncodeToString(hasher.Sum(nil))), byteLength, nil
}
