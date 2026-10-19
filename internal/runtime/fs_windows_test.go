//go:build windows

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"golang.org/x/sys/windows"
)

func TestAvailableBytesRejectsPathNotBoundToRootHandle(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	finalPath, err := root.FinalPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := availableBytesForHandle(root.handle, finalPath+`\different`); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("headroom lookup for a path unrelated to the open root returned %v; want ErrPathEscape", err)
	}
}

func TestWindowsPrivateDirectoriesAndFilesUseProtectedOwnerACLs(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := root.EnsurePrivateDirs("private"); err != nil {
		t.Fatalf("create private directory: %v", err)
	}
	assertWindowsPrivateACL(t, filepath.Join(rootPath, "private"), true)

	lock, err := root.OpenLockFile("private/operation.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateACL(t, filepath.Join(rootPath, "private", "operation.lock"), false)

	if err := root.WriteAtomicFile("private/config.json", []byte(`{"fixture":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateACL(t, filepath.Join(rootPath, "private", "config.json"), false)
}

func TestWindowsPrivateDirectoriesReplaceInheritedACLForCurrentOwner(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// os.Mkdir inherits the ambient parent DACL. A current-user-owned directory
	// inside the verified root must be secured before it is treated as private.
	if err := os.Mkdir(filepath.Join(rootPath, "inherited"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.EnsurePrivateDirs("inherited"); err != nil {
		t.Fatalf("secure current-user-owned directory with inherited ACL: %v", err)
	}
	assertWindowsPrivateACL(t, filepath.Join(rootPath, "inherited"), true)
	if err := root.EnsurePrivateDirs("inherited"); err != nil {
		t.Fatalf("revalidate secured directory: %v", err)
	}
}

func TestWindowsRelativePathsRejectStreamsDevicesAndAmbiguousNames(t *testing.T) {
	for _, relative := range []string{
		"file.txt:secret",
		"CON",
		"NUL.txt",
		"nested/COM1.log",
		"trailing.",
		"trailing ",
		`//server/share/file.txt`,
	} {
		if err := paths.ValidateRelative(relative); !errors.Is(err, paths.ErrInvalidRelativePath) {
			t.Errorf("ValidateRelative(%q)=%v; want ErrInvalidRelativePath", relative, err)
		}
	}
}

func assertWindowsPrivateACL(t *testing.T, path string, directory bool) {
	t.Helper()
	security, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || security == nil {
		t.Fatalf("read Windows security descriptor: err=%v descriptor=%v", err, security != nil)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("read current Windows user SID: %v", err)
	}
	owner, _, err := security.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatalf("private object owner does not match current user: err=%v", err)
	}
	dacl, _, err := security.DACL()
	count := uint16(0)
	if dacl != nil {
		count = dacl.AceCount
	}
	if err != nil || dacl == nil || count != 3 {
		t.Fatalf("private object DACL has %d ACEs (err=%v); want exactly user, SYSTEM, and Administrators", count, err)
	}
	sddl := security.String()
	if !strings.Contains(sddl, "D:P") {
		t.Fatal("private object DACL is not protected from ambient inheritance")
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	for _, sid := range []string{"SY", "BA", user.User.Sid.String()} {
		ace := fmt.Sprintf("(A;%s;FA;;;%s)", inheritance, sid)
		if strings.Count(sddl, ace) != 1 {
			t.Fatal("private object ACL lacks exactly one full-control ACE for an allowed principal")
		}
	}
}
