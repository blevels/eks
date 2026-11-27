package diagnostics

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Runs only on native Windows; cross-compiling this test is not runtime evidence.
func TestLogFileOwnerOnlyACL(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(privateTestDir(t), "events.log")
			if existing {
				// os.WriteFile creates files with the parent directory's inherited ACL.
				// Opening the sink must replace it for every retained file and sidecar.
				for _, suffix := range []string{"", ".1", ".2", ".3", ".lock"} {
					if err := os.WriteFile(path+suffix, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			s, err := NewSink(&bytes.Buffer{}, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Check repaired archives before rotation could replace them.
			if existing {
				for _, suffix := range []string{".1", ".2", ".3"} {
					checkLogACL(t, path+suffix)
				}
			}
			s.limit = 30
			for i := 0; i < 20; i++ {
				if _, err := s.Write([]byte("synthetic\n")); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", ".1", ".2", ".3", ".lock"} {
				checkLogACL(t, path+suffix)
			}
		})
	}
}

func checkLogProtection(t *testing.T, path string) {
	t.Helper()
	checkLogACL(t, path)
}

func checkLogACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("unprotected ACL", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatal("ACL must contain only owner", err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	owner, err := windows.StringToSid("S-1-3-4")
	if err != nil || !sid.Equals(owner) || ace.Mask != windows.ACCESS_MASK(0x001F01FF) {
		t.Fatal("incorrect owner permissions", err)
	}
}
