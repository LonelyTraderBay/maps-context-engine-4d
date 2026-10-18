//go:build windows

package acceptance_test

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func TestDirectoryJunctionCannotExposeSourceOutsideRegisteredRoot(t *testing.T) {
	powershell, err := osexec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable for the junction boundary probe")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideBytes := []byte("package outside\nconst Value = 99\n")
	writeFixtureFile(t, filepath.Join(outside, "outside.go"), outsideBytes)
	junction := filepath.Join(root, "pivot")
	command := osexec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:MCE4D_JUNCTION -Target $env:MCE4D_TARGET | Out-Null")
	command.Env = append(os.Environ(), "MCE4D_JUNCTION="+junction, "MCE4D_TARGET="+outside)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("host cannot create the isolated junction fixture: %v: %s", err, output)
	}
	opened, err := mceruntime.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	caseMode, err := opened.CaseMode()
	if err != nil || caseMode == paths.CaseUnknown {
		t.Fatalf("junction test case mode is unknown: mode=%d err=%v", caseMode, err)
	}
	rules, err := policy.New(nil, caseMode)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := mceruntime.ReadAllowed(opened, "pivot/outside.go", rules)
	if err == nil || len(captured.Bytes) != 0 {
		t.Fatalf("directory junction returned bytes=%q err=%v; want fail-closed", captured.Bytes, err)
	}
}
