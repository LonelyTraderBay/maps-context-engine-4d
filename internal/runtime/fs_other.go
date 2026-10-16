//go:build !windows && !linux

package runtime

import (
	"errors"
	"os"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
)

func OpenRoot(string) (*Root, error)                    { return nil, ErrUnsupportedOS }
func duplicateRootHandle(uintptr) (uintptr, error)      { return 0, ErrUnsupportedOS }
func closeRootHandle(uintptr) error                     { return nil }
func caseModeForHandle(uintptr) (paths.CaseMode, error) { return paths.CaseUnknown, ErrUnsupportedOS }
func openRelative(uintptr, string, string) (*os.File, func() error, error) {
	return nil, nil, errors.New("secure file open unsupported on this operating system")
}
func identityForHandle(uintptr) (string, string, error)                      { return "", "", ErrUnsupportedOS }
func availableBytesForHandle(uintptr, string) (int64, error)                 { return 0, ErrUnsupportedOS }
func makeRelativeDirs(uintptr, string, string) error                         { return ErrUnsupportedOS }
func openRelativeLock(uintptr, string, string) (*os.File, error)             { return nil, ErrUnsupportedOS }
func writeAtomicRelative(uintptr, string, string, []byte, os.FileMode) error { return ErrUnsupportedOS }
