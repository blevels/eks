//go:build windows

package kconnect

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestSecureDirectoryProtectsInheritedFiles(t *testing.T) {
	dir, err := os.MkdirTemp("", "kconnect-acl-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	if err := secureDirectory(dir); err != nil {
		t.Fatal(err)
	}
	checkOwnerRightsACL(t, dir, windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE, true, true)

	config := filepath.Join(dir, "synthetic-config.json")
	if err := os.WriteFile(config, []byte(`{"password":"synthetic-only"}`), 0600); err != nil {
		t.Fatal(err)
	}
	checkOwnerRightsACL(t, config, windows.INHERITED_ACE, false, false)
}

func checkOwnerRightsACL(t *testing.T, path string, expectedFlags uint8, exactFlags, directory bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if directory && control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("directory DACL is not protected: %v", sd)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("expected exactly one owner-rights ACE, got descriptor %v", sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Fatalf("unexpected ACE type %d in %v", ace.Header.AceType, sd)
	}
	if exactFlags && ace.Header.AceFlags != expectedFlags {
		t.Fatalf("ACE flags = %#x, want exactly %#x (%v)", ace.Header.AceFlags, expectedFlags, sd)
	}
	if !exactFlags && ace.Header.AceFlags&expectedFlags != expectedFlags {
		t.Fatalf("ACE flags = %#x, must include %#x (%v)", ace.Header.AceFlags, expectedFlags, sd)
	}
	if ace.Mask != windows.ACCESS_MASK(0x001F01FF) {
		t.Fatalf("ACE mask = %#x, want full file access %#x (%v)", ace.Mask, uint32(0x001F01FF), sd)
	}
	ownerRights, err := windows.StringToSid("S-1-3-4")
	if err != nil {
		t.Fatal(err)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(ownerRights) {
		t.Fatalf("ACE is not restricted to Owner Rights: %v", sd)
	}
}
