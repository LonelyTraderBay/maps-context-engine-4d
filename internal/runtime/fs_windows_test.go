//go:build windows

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

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
	ownerSID, err := currentWindowsOwnerSID()
	if err != nil {
		t.Fatalf("read current Windows token owner SID: %v", err)
	}
	owner, _, err := security.Owner()
	if err != nil || owner == nil || !owner.Equals(ownerSID) {
		t.Fatalf("private object owner does not match current token owner: err=%v", err)
	}
	dacl, _, err := security.DACL()
	count := uint16(0)
	if dacl != nil {
		count = dacl.AceCount
	}
	if err != nil || dacl == nil || count != 3 {
		t.Fatalf("private object DACL has %d ACEs (err=%v); want exactly user, SYSTEM, and Administrators", count, err)
	}
	control, _, err := security.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("private object DACL is not protected from ambient inheritance")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		t.Fatalf("read current Windows user SID: %v", err)
	}
	inheritanceFlags := uint8(0)
	if directory {
		inheritanceFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	allowed := map[string]bool{
		"S-1-5-18":             false,
		"S-1-5-32-544":         false,
		user.User.Sid.String(): false,
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil {
			t.Fatalf("read native private ACL ACE %d: %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != inheritanceFlags || ace.Mask != 0x001f01ff {
			t.Fatalf("private ACL ACE %d has unexpected type, inheritance, or permissions", index)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			t.Fatalf("private ACL ACE %d has an invalid SID", index)
		}
		principal := sid.String()
		if _, ok := allowed[principal]; !ok || allowed[principal] {
			t.Fatalf("private ACL contains an unexpected or duplicate principal: %s", principal)
		}
		allowed[principal] = true
	}
	for principal, present := range allowed {
		if !present {
			t.Fatalf("private ACL is missing allowed principal %s", principal)
		}
	}
}
