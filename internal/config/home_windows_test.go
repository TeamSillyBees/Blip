//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCanonicalHomeDoesNotNeedDirectoryReadPermission(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "restricted")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	original, err := windows.GetNamedSecurityInfo(home, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	oldDACL, _, err := original.DACL()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Permit metadata and traversal only on this disposable directory. Keep
	// WRITE_DAC for cleanup; never apply this test ACL to a real harness home.
	sid := user.User.Sid.String()
	limited, err := windows.SecurityDescriptorFromString("D:P(A;;0x001400A0;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := limited.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(home, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.SetNamedSecurityInfo(home, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, oldDACL, nil); err != nil {
			t.Errorf("restore temporary ACL: %v", err)
		}
	})
	if _, err := os.ReadDir(home); err == nil {
		t.Fatal("test directory still permits content reads")
	}
	canonical, err := CanonicalHome(home)
	if err != nil || canonical != strings.ToLower(home) {
		t.Fatalf("metadata-only identity failed: %q %v", canonical, err)
	}
	if _, err := os.ReadDir(home); err == nil {
		t.Fatal("identity check changed read access")
	}
}
