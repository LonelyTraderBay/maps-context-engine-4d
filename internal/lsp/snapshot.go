package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const maxSnapshotFiles = 1_000_000

var ErrCapturedWorkspace = errors.New("captured workspace is incomplete or changed")

type SnapshotFile struct {
	Path        string
	ContentHash model.Hash
}

type WorkspaceSnapshot struct {
	Root     string
	Complete bool
	Scope    model.ScopeVector
	Files    []SnapshotFile
}

type validatedWorkspace struct {
	root        *snapshotRoot
	rootPath    string
	moduleRoots []string
	files       map[string]SnapshotFile
	totalSize   int64
}

type snapshotRoot struct {
	mu     sync.RWMutex
	path   string
	closed bool
}

func openSnapshotRoot(path string) (*snapshotRoot, error) {
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return nil, ErrCapturedWorkspace
	}
	return &snapshotRoot{path: canonical}, nil
}

func (root *snapshotRoot) OpenRegularFile(relative string) (*os.File, error) {
	if root == nil || model.ValidateRelativePath(relative) != nil {
		return nil, ErrCapturedWorkspace
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return nil, ErrCapturedWorkspace
	}
	path := filepath.Join(root.path, filepath.FromSlash(relative))
	if err := verifySnapshotPath(root.path, relative, true); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrCapturedWorkspace
	}
	before, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !before.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || isReparsePoint(path) || !os.SameFile(before, current) {
		_ = file.Close()
		return nil, ErrCapturedWorkspace
	}
	if err := verifySnapshotPath(root.path, relative, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (root *snapshotRoot) Close() error {
	if root == nil {
		return nil
	}
	root.mu.Lock()
	root.closed = true
	root.mu.Unlock()
	return nil
}

func verifySnapshotPath(root, relative string, finalRegularFile bool) error {
	if model.ValidateRelativePath(relative) != nil {
		return ErrCapturedWorkspace
	}
	current := root
	segments := strings.Split(filepath.FromSlash(relative), string(os.PathSeparator))
	for index, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || isReparsePoint(current) {
			return ErrCapturedWorkspace
		}
		last := index == len(segments)-1
		if last && finalRegularFile {
			if !info.Mode().IsRegular() {
				return ErrCapturedWorkspace
			}
		} else if !info.IsDir() {
			return ErrCapturedWorkspace
		}
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil || !samePath(filepath.Clean(current), filepath.Clean(resolved)) || !pathWithin(root, current) {
		return ErrCapturedWorkspace
	}
	return nil
}

func validateWorkspaceSnapshot(ctx context.Context, snapshot WorkspaceSnapshot, profile GoProfile) (*validatedWorkspace, error) {
	if ctx == nil {
		return nil, ErrCapturedWorkspace
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !snapshot.Complete || snapshot.Scope.Validate() != nil || snapshot.Scope.Kind != "single" ||
		len(snapshot.Scope.Members) != 1 || len(snapshot.Scope.UnavailableMembers) != 0 ||
		snapshot.Scope.Members[0].ProfileID != profile.ID || len(snapshot.Files) == 0 || len(snapshot.Files) > maxSnapshotFiles {
		return nil, ErrCapturedWorkspace
	}
	rootPath, err := canonicalDirectory(snapshot.Root)
	if err != nil {
		return nil, ErrCapturedWorkspace
	}
	root, err := openSnapshotRoot(rootPath)
	if err != nil {
		return nil, ErrCapturedWorkspace
	}
	workspace := &validatedWorkspace{root: root, rootPath: rootPath, moduleRoots: append([]string(nil), profile.ModuleRoots...)}
	if err := workspace.verify(ctx, snapshot.Files, profile.MaxFileBytes, profile.MaxSourceBytes); err != nil {
		_ = root.Close()
		return nil, err
	}
	return workspace, nil
}

func (workspace *validatedWorkspace) verify(ctx context.Context, declared []SnapshotFile, maxFileBytes, maxSourceBytes int64) error {
	if workspace == nil || workspace.root == nil || ctx == nil || maxFileBytes < 1 || maxSourceBytes < maxFileBytes {
		return ErrCapturedWorkspace
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	declaredFiles := make(map[string]SnapshotFile, len(declared))
	expectedDirectories := map[string]struct{}{".": {}}
	var total int64
	for _, file := range declared {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := model.ValidateRelativePath(file.Path); err != nil {
			return ErrCapturedWorkspace
		}
		if _, err := model.ParseHash(string(file.ContentHash)); err != nil {
			return ErrCapturedWorkspace
		}
		if _, duplicate := declaredFiles[file.Path]; duplicate {
			return ErrCapturedWorkspace
		}
		captured, size, err := workspace.read(file.Path, maxFileBytes)
		if err != nil || model.SHA256(captured) != file.ContentHash {
			clear(captured)
			return fmt.Errorf("verify captured source %s: %w", filepath.Base(filepath.FromSlash(file.Path)), ErrCapturedWorkspace)
		}
		clear(captured)
		if size < 0 || total > maxSourceBytes-size {
			return ErrCapturedWorkspace
		}
		total += size
		declaredFiles[file.Path] = file
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(file.Path))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			expectedDirectories[parent] = struct{}{}
		}
	}
	if workspace.files != nil {
		if len(declaredFiles) != len(workspace.files) {
			return ErrCapturedWorkspace
		}
		for path, previous := range workspace.files {
			if current, ok := declaredFiles[path]; !ok || current.ContentHash != previous.ContentHash {
				return ErrCapturedWorkspace
			}
		}
	}
	if err := workspace.verifyTree(ctx, declaredFiles, expectedDirectories); err != nil {
		return err
	}
	workspace.files = declaredFiles
	workspace.totalSize = total
	return nil
}

func (workspace *validatedWorkspace) verifyTree(ctx context.Context, files map[string]SnapshotFile, directories map[string]struct{}) error {
	seenFiles := make(map[string]struct{}, len(files))
	seenDirectories := map[string]struct{}{".": {}}
	err := filepath.WalkDir(workspace.rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil || entry == nil || isReparsePoint(path) {
			return ErrCapturedWorkspace
		}
		if path == workspace.rootPath {
			return nil
		}
		relative, err := filepath.Rel(workspace.rootPath, path)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return ErrCapturedWorkspace
		}
		relative = filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrCapturedWorkspace
		}
		if info.IsDir() {
			if _, allowed := directories[relative]; !allowed {
				return ErrCapturedWorkspace
			}
			seenDirectories[relative] = struct{}{}
			return nil
		}
		if !info.Mode().IsRegular() {
			return ErrCapturedWorkspace
		}
		if _, allowed := files[relative]; !allowed {
			return ErrCapturedWorkspace
		}
		seenFiles[relative] = struct{}{}
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(seenFiles) != len(files) || len(seenDirectories) != len(directories) {
		return ErrCapturedWorkspace
	}
	return nil
}

func (workspace *validatedWorkspace) read(relative string, maxBytes int64) ([]byte, int64, error) {
	if workspace == nil || workspace.root == nil || model.ValidateRelativePath(relative) != nil || maxBytes < 1 {
		return nil, 0, ErrCapturedWorkspace
	}
	file, err := workspace.root.OpenRegularFile(relative)
	if err != nil {
		return nil, 0, ErrCapturedWorkspace
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxBytes {
		return nil, 0, ErrCapturedWorkspace
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(content)) > maxBytes {
		clear(content)
		return nil, 0, ErrCapturedWorkspace
	}
	after, err := file.Stat()
	if err != nil || before.Size() != int64(len(content)) || after.Size() != before.Size() || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		clear(content)
		return nil, 0, ErrCapturedWorkspace
	}
	return content, int64(len(content)), nil
}

func (workspace *validatedWorkspace) close() error {
	if workspace == nil || workspace.root == nil {
		return nil
	}
	return workspace.root.Close()
}

func (workspace *validatedWorkspace) readSource(ctx context.Context, path string, expected model.Hash, maxFileBytes int64) ([]byte, error) {
	if workspace == nil || ctx == nil {
		return nil, ErrCapturedWorkspace
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, ok := workspace.files[path]
	if !ok || file.ContentHash != expected {
		return nil, ErrCapturedWorkspace
	}
	content, _, err := workspace.read(path, maxFileBytes)
	if err != nil || model.SHA256(content) != expected {
		clear(content)
		return nil, ErrCapturedWorkspace
	}
	if err := ctx.Err(); err != nil {
		clear(content)
		return nil, err
	}
	return content, nil
}
