package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func TestSinkConcurrentWritesDuringRotation(t *testing.T) {
	const (
		writers        = 8
		perWriter      = 40
		recordsPerFile = 5
		retainedFiles  = 4
		recordBytes    = 256
	)
	type record struct {
		Writer   int    `json:"writer"`
		Sequence int    `json:"sequence"`
		Payload  string `json:"payload"`
	}

	// Fixed-width records make the retained suffix and byte bounds exact,
	// regardless of the order in which writers acquire the sink's lock.
	var records [writers][perWriter][]byte
	expected := make(map[record][]byte, writers*perWriter)
	for writer := range writers {
		for sequence := range perWriter {
			r := record{writer, sequence, strings.Repeat(fmt.Sprintf("%02d:%02d|", writer, sequence), 24)}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			// JSON numbers vary in width; pad with legal trailing whitespace.
			if len(data) >= recordBytes {
				t.Fatalf("synthetic record exceeds %d-byte fixture size", recordBytes)
			}
			data = append(data, bytes.Repeat([]byte(" "), recordBytes-len(data)-1)...)
			data = append(data, '\n')
			records[writer][sequence] = data
			expected[r] = data
		}
	}

	path := filepath.Join(privateTestDir(t), "events.log")
	var stderr bytes.Buffer
	s, err := NewSink(&stderr, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	s.limit = int64(recordsPerFile * recordBytes)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(writers)
	for writer := range writers {
		go func() {
			defer wg.Done()
			<-start
			for sequence := range perWriter {
				data := records[writer][sequence]
				if n, err := s.Write(data); err != nil || n != len(data) {
					t.Errorf("writer %d record %d: Write = (%d, %v), want (%d, nil)", writer, sequence, n, err, len(data))
					return
				}
				runtime.Gosched()
			}
		}()
	}
	close(start)
	wg.Wait()
	// Close only after every writer exits; cleanup also checks repeated Close.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Failed() || s.Failure() != nil {
		t.Fatal("concurrent writes or close marked the sink failed")
	}
	if t.Failed() {
		return
	}

	checkRecords := func(name string, data []byte, want int) {
		t.Helper()
		if !bytes.HasSuffix(data, []byte("\n")) {
			t.Fatalf("%s: missing final record delimiter", name)
		}
		lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
		if len(lines) != want {
			t.Fatalf("%s: got %d records, want %d", name, len(lines), want)
		}
		seen := make(map[record]bool, want)
		for _, line := range lines {
			var r record
			if err := json.Unmarshal(line, &r); err != nil {
				t.Fatalf("%s: incomplete JSON record: %v", name, err)
			}
			original, ok := expected[r]
			if !ok || !bytes.Equal(line, bytes.TrimSuffix(original, []byte("\n"))) {
				t.Fatalf("%s: unknown or corrupted record: %+v", name, r)
			}
			if seen[r] {
				t.Fatalf("%s: duplicate record from writer %d sequence %d", name, r.Writer, r.Sequence)
			}
			seen[r] = true
		}
	}
	checkRecords("stderr", stderr.Bytes(), writers*perWriter)

	var retained []byte
	// Archives are numbered newest first; read them in chronological order.
	for _, suffix := range []string{".3", ".2", ".1", ""} {
		name := path + suffix
		st, err := os.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Mode().IsRegular() || st.Size() != s.limit {
			t.Fatalf("%s: got mode %v and size %d, want regular file of %d bytes", suffix, st.Mode(), st.Size(), s.limit)
		}
		checkLogProtection(t, name)
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checkRecords(name, data, recordsPerFile)
		retained = append(retained, data...)
	}
	checkRecords("all retained files", retained, retainedFiles*recordsPerFile)
	if !bytes.Equal(retained, stderr.Bytes()[stderr.Len()-len(retained):]) {
		t.Fatal("retained files do not contain the exact final records in write order")
	}
	checkLogProtection(t, path+".lock")
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != retainedFiles+1 {
		t.Fatalf("got %d directory entries, want active log, three archives, and lock", len(entries))
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
	for _, suffix := range []string{"", ".lock", ".1", ".2", ".3"} {
		for _, kind := range []string{"directory", "symlink", "hardlink"} {
			t.Run(kind+"/events.log"+suffix, func(t *testing.T) {
				dir := privateTestDir(t)
				path := filepath.Join(dir, "events.log")
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "directory":
					err = os.Mkdir(path+suffix, 0700)
				case "symlink":
					err = os.Symlink(target, path+suffix)
					checkLogSymlinkSupport(t, err)
				case "hardlink":
					err = os.Link(target, path+suffix)
				}
				if err != nil {
					t.Fatal(err)
				}
				requireLogDestinationRejected(t, path)
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "untouched" {
					t.Fatal("unsafe target modified", string(data), err)
				}
			})
		}
	}
	t.Run("missing_parent", func(t *testing.T) {
		requireLogDestinationRejected(t, filepath.Join(privateTestDir(t), "missing", "events.log"))
	})
	t.Run("symlink_parent", func(t *testing.T) {
		dir := privateTestDir(t)
		parent := filepath.Join(dir, "parent")
		checkLogSymlinkSupport(t, os.Symlink(dir, parent))
		requireLogDestinationRejected(t, filepath.Join(parent, "events.log"))
		if _, err := os.Stat(filepath.Join(dir, "events.log")); !os.IsNotExist(err) {
			t.Fatal("created log through symlink parent", err)
		}
	})
}

func checkLogSymlinkSupport(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) && os.Getenv("EKS_REQUIRE_LOG_SYMLINK_TEST") != "1" {
		t.Skipf("Windows symlink privilege unavailable: %v", err)
	}
	t.Fatal(err)
}

func requireLogDestinationRejected(t *testing.T, path string) {
	t.Helper()
	if s, err := NewSink(&bytes.Buffer{}, path, nil); err == nil {
		_ = s.Close()
		t.Fatal("unsafe destination accepted")
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
