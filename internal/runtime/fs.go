package runtime

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
)

var (
	ErrRootClosed     = errors.New("verified root is closed")
	ErrNotDirectory   = errors.New("root is not a directory")
	ErrPathDenied     = errors.New("path denied by policy")
	ErrOpenFailed     = errors.New("secure file open failed")
	ErrReparsePoint   = errors.New("reparse point or symbolic link denied")
	ErrNotRegularFile = errors.New("regular file required")
	ErrPathEscape     = errors.New("opened file escaped verified root")
	ErrUnsupportedOS  = errors.New("secure file open unsupported on this operating system")
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

type CapturedFile struct {
	Bytes  []byte
	SHA256 [32]byte
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
