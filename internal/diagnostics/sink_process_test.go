package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const sinkHelperModeEnv = "EKS_DIAGNOSTICS_SINK_HELPER"
const sinkHelperPathEnv = "EKS_DIAGNOSTICS_SINK_PATH"

// TestSinkProcessHelper runs only in a copy of this test binary. In particular,
// the holder must not Close its sink: the parent kills it to exercise OS cleanup.
func TestSinkProcessHelper(t *testing.T) {
	mode := os.Getenv(sinkHelperModeEnv)
	if mode == "" {
		return
	}
	s, err := NewSink(io.Discard, os.Getenv(sinkHelperPathEnv), nil)
	if mode == "refuse" {
		if err == nil {
			if closeErr := s.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			t.Fatal("another process acquired the diagnostic log lock")
		}
		if err.Error() != "log_sink_initialization_failed" {
			t.Fatalf("unexpected initialization error: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "hold":
		if _, err := s.Write([]byte("held-0\n")); err != nil {
			t.Fatal(err)
		}
		if err := s.file.Sync(); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
			t.Fatal(err)
		}
		// The parent keeps stdin open until it kills us. Keep the sink reachable
		// so finalizers cannot release its file handles while we wait.
		_, err := io.Copy(io.Discard, os.Stdin)
		runtime.KeepAlive(s)
		t.Fatalf("holder stdin ended before forced termination: %v", err)
	case "rotate":
		// One complete record per file makes the expected archive chain exact.
		s.limit = int64(len("held-0\n"))
		for i := 1; i <= 3; i++ {
			record := []byte(fmt.Sprintf("next-%d\n", i))
			if n, err := s.Write(record); err != nil || n != len(record) {
				t.Fatalf("write record %d: n=%d, err=%v", i, n, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown sink helper mode %q", mode)
	}
}

func sinkHelperCommand(t *testing.T, ctx context.Context, mode, path string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSinkProcessHelper$", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), sinkHelperModeEnv+"="+mode, sinkHelperPathEnv+"="+path)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func runSinkHelper(t *testing.T, mode, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	output, err := sinkHelperCommand(t, ctx, mode, path).CombinedOutput()
	if err != nil {
		t.Fatalf("%s helper: %v\n%s", mode, err, output)
	}
}

func TestSinkLockRecoversAfterProcessCrash(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "events.log")
	// Record the sidecar identity before any process opens it. Recovery must
	// reuse this file, not delete/recreate it and split the lock identity.
	if err := os.WriteFile(path+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	holder := sinkHelperCommand(t, ctx, "hold", path)
	var stderr bytes.Buffer
	holder.Stderr = &stderr
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			// Wait reaps the child before TempDir removes its files, including
			// on Windows where an open exclusive handle prevents removal.
			_ = holder.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "locked\n" {
			err = fmt.Errorf("unexpected holder readiness message %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("holder did not acquire lock: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for holder to acquire lock")
	}

	// A different process must fail promptly while the holder is alive.
	runSinkHelper(t, "refuse", path)
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = holder.Wait()
	waited = true
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.Success() {
		t.Fatalf("holder was not forcibly terminated: %v\n%s", err, stderr.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "held-0\n" {
		t.Fatalf("contender changed holder record: %q, %v", data, err)
	}
	requireSameSinkSidecar(t, path, lockBefore)

	// No unlink, retry, or orderly Close from the holder is allowed here.
	runSinkHelper(t, "rotate", path)
	for suffix, want := range map[string]string{
		"": "next-3\n", ".1": "next-2\n", ".2": "next-1\n", ".3": "held-0\n",
	} {
		data, err := os.ReadFile(path + suffix)
		if err != nil || string(data) != want {
			t.Errorf("log%s after recovery: got %q, err=%v; want %q", suffix, data, err, want)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("unexpected extra archive: %v", err)
	}
	requireSameSinkSidecar(t, path, lockBefore)
}

func requireSameSinkSidecar(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal("lock sidecar was not retained:", err)
	}
	if !after.Mode().IsRegular() || after.Size() != 0 || !os.SameFile(before, after) {
		t.Fatal("lock sidecar was modified or replaced")
	}
}
