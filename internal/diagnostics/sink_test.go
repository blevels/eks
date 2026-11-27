package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type brokenWriter struct {
	calls int
	short bool
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func (b *brokenWriter) Write(p []byte) (int, error) {
	b.calls++
	if b.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("CANARY/private/path")
}

func TestSinkRotationPermissionsAndBounds(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "events.log")
	var stderr bytes.Buffer
	s, err := NewSink(&stderr, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.limit = 300
	for i := 0; i < 20; i++ {
		if _, err := s.Write([]byte(strings.Repeat("x", 99) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".1", ".2", ".3", ".lock"} {
		st, err := os.Stat(path + suffix)
		if err != nil || st.Size() > 300 {
			t.Fatal(st, err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
			t.Fatal(st.Mode())
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatal("unbounded archives")
	}
}

func TestEmergencyAlertUsesCommonSchemaAndRunID(t *testing.T) {
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := NewSink(&stderr, filepath.Join(privateTestDir(t), "events.log"), cancel)
	if err != nil {
		t.Fatal(err)
	}
	log, emergency := NewLoggers(s, &stderr, "json", true, false, "test")
	ctx = With(ctx, &Diagnostics{Log: log})
	ctx, _ = Start(ctx, "cli", "run")
	s.SetAlert(func() error { return ReportSinkFailure(ctx, emergency) })
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	_, query := Start(ctx, "database", "query")
	_ = query.End(context.Canceled)
	if !s.Failed() || ctx.Err() == nil || s.Failure() == nil {
		t.Fatal("sink failure not propagated")
	}
	if err := s.Close(); err == nil {
		t.Fatal("closed file sync succeeded")
	}
	count, runID := 0, ""
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err, line)
		}
		if runID == "" {
			runID = r["run_id"].(string)
		}
		if r["run_id"] != runID || r["schema_version"] != float64(1) {
			t.Fatal(r)
		}
		if r["event"] == "log_sink.outcome" {
			count++
			if r["exit_code"] != float64(1) || r["error_code"] != "log_sink_failed" || r["parent_operation_id"] == "" {
				t.Fatal(r)
			}
		}
	}
	if count != 1 {
		t.Fatal("unbounded or missing emergency alert", stderr.String())
	}
}

func TestSinkFailuresCancelOnceAndRemainBounded(t *testing.T) {
	for _, short := range []bool{false, true} {
		b := &brokenWriter{short: short}
		cancels := 0
		s, err := NewSink(b, "", func() { cancels++ })
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			_, _ = s.Write([]byte("safe\n"))
		}
		if !s.Failed() || cancels != 1 || b.calls != 2 {
			t.Fatal(s.Failed(), cancels, b.calls)
		}
	}
	for _, fail := range []string{"file_write", "rotation", "sync", "oversized_record"} {
		t.Run(fail, func(t *testing.T) {
			var stderr bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := NewSink(&stderr, filepath.Join(privateTestDir(t), "events.log"), cancel)
			if err != nil {
				t.Fatal(err)
			}
			switch fail {
			case "file_write", "sync":
				_ = s.file.Close()
			case "rotation":
				s.limit, s.size = 1, 1
				if err := os.Mkdir(s.path+".3", 0700); err != nil {
					t.Fatal(err)
				}
			}
			if fail == "sync" {
				_ = s.Close()
			} else if fail == "oversized_record" {
				_, _ = s.Write(make([]byte, MaxRecordBytes+1))
			} else {
				_, _ = s.Write([]byte("safe\n"))
			}
			if ctx.Err() == nil || !s.Failed() || strings.Count(stderr.String(), "log_sink.outcome") != 1 {
				t.Fatal(stderr.String())
			}
			_ = s.Close()
		})
	}
}

func TestSinkRefusesUnsafeDestinations(t *testing.T) {
	dir := privateTestDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "missing", "events")} {
		if _, err := NewSink(&bytes.Buffer{}, path, nil); err == nil {
			t.Fatal("unsafe destination accepted", path)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err == nil {
		if _, err := NewSink(&bytes.Buffer{}, link, nil); err == nil {
			t.Fatal("symlink accepted")
		}
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "untouched" {
		t.Fatal(string(data), err)
	}
	hard := filepath.Join(dir, "hard")
	if err := os.Link(target, hard); err == nil {
		if s, err := NewSink(&bytes.Buffer{}, hard, nil); err == nil {
			_ = s.Close()
			t.Fatal("hardlink accepted")
		}
	}
}

func TestSinkRejectsConcurrentWritersAndOversizedExistingFiles(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "events.log")
	first, err := NewSink(&bytes.Buffer{}, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := NewSink(&bytes.Buffer{}, path, nil); err == nil {
		_ = second.Close()
		t.Fatal("concurrent sink accepted")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewSink(&bytes.Buffer{}, path, nil)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + MaxRecordBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := NewSink(&bytes.Buffer{}, path, nil); err == nil {
		_ = s.Close()
		t.Fatal("oversized existing log accepted")
	}
}

func TestRotatedLogsContainOnlySafeEvents(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		path := filepath.Join(privateTestDir(t), "events.log")
		s, err := NewSink(&bytes.Buffer{}, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.limit = 1500
		ctx := With(context.Background(), &Diagnostics{Log: NewLogger(s, format, true, true, "test")})
		for i := 0; i < 15; i++ {
			_, scope := Start(ctx, "database", "query")
			_ = scope.End(errors.New("CANARY\x1b/private/home/secret"))
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"", ".1", ".2", ".3"} {
			data, err := os.ReadFile(path + suffix)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("CANARY")) || bytes.Contains(data, []byte("/private/home")) {
				t.Fatal("secret in retained log")
			}
		}
	}
}
