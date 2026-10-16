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
