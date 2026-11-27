package diagnostics

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const MaxFileBytes int64 = 10 * 1024 * 1024
const MaxRecordBytes = 16 * 1024

// Sink serializes complete records, bounds retention, and cancels the run once
// on delivery failure. Sink I/O is never performed under a business/cache lock.
type Sink struct {
	mu          sync.Mutex
	stderr      io.Writer
	file        *os.File
	lease       *os.File
	path        string
	size, limit int64
	failed      bool
	cancel      context.CancelFunc
	alert       func() error
	failure     error
}

func NewSink(stderr io.Writer, path string, cancel context.CancelFunc) (*Sink, error) {
	s := &Sink{stderr: stderr, path: path, cancel: cancel, limit: MaxFileBytes}
	s.alert = func() error {
		return ReportSinkFailure(context.Background(), NewLogger(stderr, "text", false, false, "unknown"))
	}
	if path == "" {
		return s, nil
	}
	// Parent symlinks are refused as well, so a configured destination is never
	// redirected through an existing symbolic link.
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("log_sink_initialization_failed")
	}
	for p := filepath.Dir(abs); ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("log_sink_initialization_failed")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	s.path = abs
	s.lease, err = lockPrivate(s.path + ".lock")
	if err != nil {
		return nil, errors.New("log_sink_initialization_failed")
	}
	initialized := false
	defer func() {
		if !initialized {
			_ = s.lease.Close()
		}
	}()
	if err := s.open(); err != nil {
		return nil, errors.New("log_sink_initialization_failed")
	}
	if s.size > s.limit+MaxRecordBytes {
		_ = s.file.Close()
		return nil, errors.New("log_sink_initialization_failed")
	}
	for _, suffix := range []string{".1", ".2", ".3"} {
		st, e := os.Lstat(s.path + suffix)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !st.Mode().IsRegular() || st.Size() > s.limit+MaxRecordBytes {
			_ = s.file.Close()
			return nil, errors.New("log_sink_initialization_failed")
		}
		archive, e := openPrivate(s.path + suffix)
		if e == nil {
			e = archive.Close()
		}
		if e != nil {
			_ = s.file.Close()
			return nil, errors.New("log_sink_initialization_failed")
		}
	}
	initialized = true
	return s, nil
}

func (s *Sink) open() error {
	f, err := openPrivate(s.path)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	s.file, s.size = f, st.Size()
	return nil
}

func (s *Sink) fail() {
	if s.failed {
		return
	}
	s.failed = true
	// Direct stderr only: never recursively report a failed emergency write.
	s.failure = s.alert()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Sink) SetAlert(alert func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if alert != nil {
		s.alert = alert
	}
}

func (s *Sink) Failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return 0, errors.New("log_sink_failed")
	}
	if len(p) > MaxRecordBytes {
		s.fail()
		return 0, errors.New("log_record_limit")
	}
	n, err := s.stderr.Write(p)
	if err != nil || n != len(p) {
		s.fail()
		return n, errors.New("log_sink_failed")
	}
	if s.file == nil {
		return len(p), nil
	}
	if s.size+int64(len(p)) > s.limit && s.size > 0 {
		if err := s.rotate(); err != nil {
			s.fail()
			return 0, errors.New("log_rotation_failed")
		}
	}
	n, err = s.file.Write(p)
	s.size += int64(n)
	if err != nil || n != len(p) {
		s.fail()
		return n, errors.New("log_sink_failed")
	}
	return len(p), nil
}

func (s *Sink) rotate() error {
	if err := s.file.Close(); err != nil {
		s.file = nil
		return err
	}
	s.file = nil
	// Refuse symlinks/nonregular archive destinations before mutation.
	for _, suffix := range []string{"", ".1", ".2", ".3"} {
		st, err := os.Lstat(s.path + suffix)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && !st.Mode().IsRegular() {
			return errors.New("unsafe_log_destination")
		}
	}
	if err := os.Remove(s.path + ".3"); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, pair := range [][2]string{{".2", ".3"}, {".1", ".2"}, {"", ".1"}} {
		if err := os.Rename(s.path+pair[0], s.path+pair[1]); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return s.open()
}

func (s *Sink) Failed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.failed }
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.file != nil {
		err = errors.Join(s.file.Sync(), s.file.Close())
		s.file = nil
	}
	if s.lease != nil {
		err = errors.Join(err, s.lease.Close())
		s.lease = nil
	}
	if err != nil {
		s.fail()
		return errors.New("log_sink_close_failed")
	}
	return nil
}
