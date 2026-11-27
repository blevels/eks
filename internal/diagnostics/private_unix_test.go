//go:build !windows

package diagnostics

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func checkLogProtection(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("%s: permissions = %v, want 0600", path, st.Mode())
	}
}

func TestLogFileRepairsExistingPermissions(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "events.log")
	for _, suffix := range []string{"", ".1", ".2", ".3", ".lock"} {
		if err := os.WriteFile(path+suffix, nil, 0600); err != nil {
			t.Fatal(err)
		}
		// chmod deliberately bypasses the runner's umask.
		if err := os.Chmod(path+suffix, 0666); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewSink(&bytes.Buffer{}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, suffix := range []string{"", ".1", ".2", ".3", ".lock"} {
		st, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("permissions for %q: got %v, want 0600", suffix, st.Mode())
		}
	}
}
