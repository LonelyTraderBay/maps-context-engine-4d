package runtime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
)

var (
	ErrRootClosed                = errors.New("verified root is closed")
	ErrNotDirectory              = errors.New("root is not a directory")
	ErrPathDenied                = errors.New("path denied by policy")
	ErrOpenFailed                = errors.New("secure file open failed")
	ErrReparsePoint              = errors.New("reparse point or symbolic link denied")
	ErrNotRegularFile            = errors.New("regular file required")
	ErrPathEscape                = errors.New("opened file escaped verified root")
	ErrUnsupportedOS             = errors.New("secure file open unsupported on this operating system")
	ErrAlreadyPublished          = errors.New("atomic destination already exists")
	ErrDirectoryFlushUnsupported = errors.New("atomic destination published but directory flush is unsupported or unconfirmed")
)

type Root struct {
	mu        sync.RWMutex
	handle    uintptr
	finalPath string
	closed    bool
}

type secureFile struct {
	file    *os.File
	release func() error
	once    sync.Once
	err     error
}

// SecureFile is an opened regular file whose final handle was verified beneath
// the root. Closing it also releases any platform-specific parent handles.
type SecureFile = secureFile

type CapturedFile struct {
	Bytes  []byte
	SHA256 [32]byte
}

type AtomicPublishResult struct {
	Published        bool
	FileFlushed      bool
	DirectoryFlushed bool
}

func (root *Root) Close() error {
	if root == nil {
		return nil
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed {
		return nil
	}
	root.closed = true
	err := closeRootHandle(root.handle)
	root.handle = 0
	return err
}

func (root *Root) CaseMode() (paths.CaseMode, error) {
	if root == nil {
		return paths.CaseUnknown, ErrRootClosed
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return paths.CaseUnknown, ErrRootClosed
	}
	return caseModeForHandle(root.handle)
}

// FinalPath returns the canonical path bound to the open root handle.
func (root *Root) FinalPath() (string, error) {
	if root == nil {
		return "", ErrRootClosed
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return "", ErrRootClosed
	}
	return root.finalPath, nil
}

// Identity returns stable opaque identities for the filesystem volume and the
// opened home directory. It never derives either value from a path or PID.
func (root *Root) Identity() (volumeID, homeIdentity string, err error) {
	if root == nil {
		return "", "", ErrRootClosed
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return "", "", ErrRootClosed
	}
	return identityForHandle(root.handle)
}

// AvailableBytes reports filesystem headroom using the verified root handle.
func (root *Root) AvailableBytes() (int64, error) {
	if root == nil {
		return 0, ErrRootClosed
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return 0, ErrRootClosed
	}
	return availableBytesForHandle(root.handle, root.finalPath)
}

// EnsurePrivateDirs creates or validates a bounded set of home-relative
// private directories without following links or crossing filesystem roots.
func (root *Root) EnsurePrivateDirs(relatives ...string) error {
	if root == nil {
		return ErrRootClosed
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return ErrRootClosed
	}
	for _, relative := range relatives {
		if err := paths.ValidateRelative(relative); err != nil {
			return fmt.Errorf("validate private directory path: %w", ErrPathDenied)
		}
		if err := makeRelativeDirs(root.handle, root.finalPath, relative); err != nil {
			return err
		}
	}
	return nil
}

// OpenLockFile securely creates or opens a permanent lockfile beneath root.
// Callers must never unlink or replace the returned lockfile path.
func (root *Root) OpenLockFile(relative string) (*os.File, error) {
	return root.openLockFile(relative, true)
}

// OpenExistingLockFile securely opens a permanent lockfile without creating it.
// Read-only maintenance planning uses this so acquiring coordination locks
// cannot create directory entries as a side effect.
func (root *Root) OpenExistingLockFile(relative string) (*os.File, error) {
	return root.openLockFile(relative, false)
}

func (root *Root) openLockFile(relative string, create bool) (*os.File, error) {
	if root == nil {
		return nil, ErrRootClosed
	}
	if err := paths.ValidateRelative(relative); err != nil {
		return nil, fmt.Errorf("validate lockfile path: %w", ErrPathDenied)
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return nil, ErrRootClosed
	}
	duplicate, err := duplicateRootHandle(root.handle)
	if err != nil {
		return nil, ErrOpenFailed
	}
	file, err := openRelativeLock(duplicate, root.finalPath, relative, create)
	rootCloseErr := closeRootHandle(duplicate)
	if err != nil {
		return nil, err
	}
	if rootCloseErr != nil {
		_ = file.Close()
		return nil, ErrOpenFailed
	}
	return file, nil
}

// WriteAtomicFile replaces one regular home-relative file with a flushed
// temporary created in the same verified directory. The path is never
// resolved through a symbolic link or reparse point.
func (root *Root) WriteAtomicFile(relative string, value []byte, mode os.FileMode) error {
	if root == nil {
		return ErrRootClosed
	}
	if err := paths.ValidateRelative(relative); err != nil || mode.Perm()&0o077 != 0 {
		return fmt.Errorf("validate private atomic file: %w", ErrPathDenied)
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return ErrRootClosed
	}
	duplicate, err := duplicateRootHandle(root.handle)
	if err != nil {
		return ErrOpenFailed
	}
	writeErr := writeAtomicRelative(duplicate, root.finalPath, relative, func(output io.Writer) error {
		for offset := 0; offset < len(value); {
			written, err := output.Write(value[offset:])
			if err != nil {
				return err
			}
			if written == 0 {
				return io.ErrShortWrite
			}
			offset += written
		}
		return nil
	}, mode.Perm(), true, nil)
	closeErr := closeRootHandle(duplicate)
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return nil
}

// PublishAtomicFile creates one new file beneath the verified root and refuses
// to replace an existing destination. If the directory flush fails after the
// rename, result.Published remains true; callers must read back the target
// before retrying an operation whose outcome is otherwise unknown.
func (root *Root) PublishAtomicFile(relative string, value []byte, mode os.FileMode) (AtomicPublishResult, error) {
	if root == nil {
		return AtomicPublishResult{}, ErrRootClosed
	}
	if err := paths.ValidateRelative(relative); err != nil || mode.Perm()&0o077 != 0 {
		return AtomicPublishResult{}, fmt.Errorf("validate private atomic publication: %w", ErrPathDenied)
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return AtomicPublishResult{}, ErrRootClosed
	}
	duplicate, err := duplicateRootHandle(root.handle)
	if err != nil {
		return AtomicPublishResult{}, ErrOpenFailed
	}
	var result AtomicPublishResult
	publishErr := writeAtomicRelative(duplicate, root.finalPath, relative, func(output io.Writer) error {
		for offset := 0; offset < len(value); {
			written, err := output.Write(value[offset:])
			if err != nil {
				return err
			}
			if written == 0 {
				return io.ErrShortWrite
			}
			offset += written
		}
		return nil
	}, mode.Perm(), false, &result)
	closeErr := closeRootHandle(duplicate)
	return result, errors.Join(publishErr, closeErr)
}

// CopyAtomicFile copies one verified regular file to a same-directory temporary
// and atomically replaces the destination beneath this root. The copy streams
// through a bounded buffer and never follows a source or destination link.
func (root *Root) CopyAtomicFile(sourceRelative, destinationRelative string, mode os.FileMode) (AtomicPublishResult, error) {
	if root == nil {
		return AtomicPublishResult{}, ErrRootClosed
	}
	if paths.ValidateRelative(sourceRelative) != nil || paths.ValidateRelative(destinationRelative) != nil || mode.Perm()&0o077 != 0 {
		return AtomicPublishResult{}, fmt.Errorf("validate atomic file copy: %w", ErrPathDenied)
	}
	source, err := root.OpenRegularFile(sourceRelative)
	if err != nil {
		return AtomicPublishResult{}, fmt.Errorf("open atomic copy source: %w", err)
	}
	info, statErr := source.Stat()
	if statErr != nil || info == nil || !info.Mode().IsRegular() || info.Size() < 1 {
		return AtomicPublishResult{}, errors.Join(fmt.Errorf("atomic copy source is not a nonempty regular file: %w", ErrNotRegularFile), statErr, source.Close())
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return AtomicPublishResult{}, errors.Join(source.Close(), ErrRootClosed)
	}
	duplicate, err := duplicateRootHandle(root.handle)
	if err != nil {
		return AtomicPublishResult{}, errors.Join(source.Close(), ErrOpenFailed)
	}
	var result AtomicPublishResult
	copyErr := writeAtomicRelative(duplicate, root.finalPath, destinationRelative, func(output io.Writer) error {
		written, err := io.CopyN(output, source, info.Size())
		if err != nil || written != info.Size() {
			return errors.Join(io.ErrUnexpectedEOF, err)
		}
		return nil
	}, mode.Perm(), true, &result)
	rootCloseErr := closeRootHandle(duplicate)
	sourceCloseErr := source.Close()
	return result, errors.Join(copyErr, rootCloseErr, sourceCloseErr)
}

func (root *Root) openRegularFile(relative string) (*secureFile, error) {
	if root == nil {
		return nil, ErrRootClosed
	}
	if err := paths.ValidateRelative(relative); err != nil {
		return nil, err
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return nil, ErrRootClosed
	}
	duplicate, err := duplicateRootHandle(root.handle)
	if err != nil {
		return nil, ErrOpenFailed
	}
	file, release, err := openRelative(duplicate, root.finalPath, relative)
	if err != nil {
		_ = closeRootHandle(duplicate)
		return nil, err
	}
	return &secureFile{
		file: file,
		release: func() error {
			firstErr := release()
			if err := closeRootHandle(duplicate); firstErr == nil {
				firstErr = err
			}
			return firstErr
		},
	}, nil
}

func (root *Root) OpenRegularFile(relative string) (*SecureFile, error) {
	return root.openRegularFile(relative)
}

// RemoveRegularFile removes one existing regular, single-link file beneath
// the verified root. It never follows a link or removes a directory.
func (root *Root) RemoveRegularFile(relative string) error {
	if root == nil {
		return ErrRootClosed
	}
	if err := paths.ValidateRelative(relative); err != nil {
		return fmt.Errorf("validate file removal path: %w", ErrPathDenied)
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed {
		return ErrRootClosed
	}
	duplicate, err := duplicateRootHandle(root.handle)
	if err != nil {
		return ErrOpenFailed
	}
	removeErr := removeRegularRelative(duplicate, root.finalPath, relative)
	closeErr := closeRootHandle(duplicate)
	return errors.Join(removeErr, closeErr)
}

func (file *secureFile) Read(value []byte) (int, error) {
	if file == nil || file.file == nil {
		return 0, os.ErrClosed
	}
	return file.file.Read(value)
}

func (file *secureFile) Stat() (os.FileInfo, error) {
	if file == nil || file.file == nil {
		return nil, os.ErrClosed
	}
	return file.file.Stat()
}

func (file *secureFile) Close() error {
	if file == nil {
		return nil
	}
	file.once.Do(func() {
		if file.file != nil {
			file.err = file.file.Close()
		}
		if file.release != nil {
			if err := file.release(); file.err == nil {
				file.err = err
			}
		}
	})
	return file.err
}

// ReadAllowed enforces path/type/size/content checks before returning bytes to a caller.
func ReadAllowed(root *Root, relative string, rules *policy.Policy) (captured CapturedFile, resultErr error) {
	if rules == nil {
		return CapturedFile{}, errors.New("policy is required")
	}
	decision, err := rules.CheckPath(relative, false)
	if err != nil {
		return CapturedFile{}, err
	}
	if decision.Denied {
		return CapturedFile{}, ErrPathDenied
	}
	if !rules.AllowsText(relative) {
		return CapturedFile{}, policy.ErrUnsupportedTextType
	}
	file, err := root.openRegularFile(relative)
	if err != nil {
		return CapturedFile{}, err
	}
	defer func() {
		if closeErr := file.Close(); resultErr == nil && closeErr != nil {
			clear(captured.Bytes)
			captured = CapturedFile{}
			resultErr = ErrOpenFailed
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return CapturedFile{}, ErrOpenFailed
	}
	if !info.Mode().IsRegular() {
		return CapturedFile{}, ErrNotRegularFile
	}
	if info.Size() < 0 || info.Size() > policy.MaxFileBytes {
		return CapturedFile{}, policy.ErrContentTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, policy.MaxFileBytes+1))
	if err != nil {
		clear(data)
		return CapturedFile{}, ErrOpenFailed
	}
	if int64(len(data)) > policy.MaxFileBytes {
		clear(data)
		return CapturedFile{}, policy.ErrContentTooLarge
	}
	if _, err := rules.ScanContent(data); err != nil {
		clear(data)
		return CapturedFile{}, err
	}
	return CapturedFile{Bytes: data, SHA256: sha256.Sum256(data)}, nil
}
