package kconnect

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"eks/internal/diagnostics/diagtest"
)

type failingFiles struct{ fail, cleanup bool }

func (f failingFiles) MkdirTemp(string, string) (string, error) {
	if f.fail {
		return "", errors.New("CANARY/private/path")
	}
	return "/synthetic/path", nil
}

func TestChildExitDiagnosticCode(t *testing.T) {
	ctx, logs := diagtest.New(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandErr := (Process{}).Run(ctx, exe, []string{"-test.run=TestProcessHelper"}, io.Discard, io.Discard)
	var exit *exec.ExitError
	if !errors.As(commandErr, &exit) {
		t.Fatal(commandErr)
	}
	r := Runner{Timeout: time.Second, Files: failingFiles{}, Exec: executorFunc(func(context.Context, string, []string, io.Writer, io.Writer) error { return commandErr })}
	err = r.Connect(ctx, Request{})
	var child *CommandError
	if !errors.As(err, &child) || child.Code != 7 {
		t.Fatal(err)
	}
	if !errors.Is(err, commandErr) {
		t.Fatal("lost child error identity")
	}
	if errors.As(err, &exit) {
		t.Fatal("raw child failure escaped")
	}
	if logs.Event("subprocess.outcome")["child_exit_code"] != float64(7) {
		t.Fatal(logs.Text())
	}
	logs.Contract()
	logs.NoSecrets("CANARY", exe)
}
func (f failingFiles) Protect(string) error                        { return nil }
func (f failingFiles) WriteFile(string, []byte, os.FileMode) error { return nil }
func (f failingFiles) RemoveAll(string) error {
	if f.cleanup {
		return errors.New("CANARY/private/cleanup")
	}
	return nil
}

type failStageFiles struct {
	failingFiles
	stage string
}

func (f failStageFiles) Protect(string) error {
	if f.stage == "directory_protection" {
		return errors.New("CANARY/private/protect")
	}
	return nil
}
func (f failStageFiles) WriteFile(string, []byte, os.FileMode) error {
	if f.stage == "credential_file" {
		return errors.New("CANARY/private/write")
	}
	return nil
}

func TestFilesystemAndSubprocessDiagnostics(t *testing.T) {
	for _, stage := range []string{"success", "private_directory", "directory_protection", "credential_file", "subprocess", "cleanup", "primary_and_cleanup", "timeout", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			files := failStageFiles{failingFiles: failingFiles{fail: stage == "private_directory", cleanup: stage == "cleanup" || stage == "primary_and_cleanup"}, stage: stage}
			r := Runner{Timeout: time.Second, Files: files, Exec: executorFunc(func(ctx context.Context, _ string, _ []string, out, stderr io.Writer) error {
				_, _ = out.Write([]byte("CANARY child payload"))
				_, _ = stderr.Write([]byte("CANARY child payload"))
				switch stage {
				case "subprocess", "primary_and_cleanup":
					return errors.New("CANARY child error")
				case "timeout":
					return context.DeadlineExceeded
				case "canceled":
					return context.Canceled
				}
				return nil
			})}
			err := r.Connect(ctx, Request{Username: "CANARY", Password: "CANARY", Namespace: "CANARY", ARN: "CANARY"})
			if (err == nil) != (stage == "success") {
				t.Fatal(err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "/synthetic/path", "/private/")
			if stage == "primary_and_cleanup" {
				if logs.Event("subprocess.outcome")["level"] != "ERROR" || logs.Event("cleanup.outcome")["level"] != "ERROR" {
					t.Fatal(logs.Text())
				}
				if err.Error() != "subprocess_failed\ncleanup_failed" {
					t.Fatal("primary overwritten", err)
				}
			}
			if stage == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if stage == "canceled" && logs.Event("subprocess.outcome")["level"] != "INFO" {
				t.Fatal(logs.Text())
			}
		})
	}
}
