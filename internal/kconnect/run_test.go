package kconnect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type executorFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f executorFunc) Run(ctx context.Context, b string, a []string, o, e io.Writer) error {
	return f(ctx, b, a, o, e)
}

func TestCredentialDeliveryAndCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			var configPath string
			r := Runner{Binary: "fake-kconnect", Timeout: time.Second, Exec: executorFunc(func(ctx context.Context, bin string, args []string, out, stderr io.Writer) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing deadline")
				}
				if bin != "fake-kconnect" {
					t.Error(bin)
				}
				for i, arg := range args {
					if arg == "--config" {
						configPath = args[i+1]
					}
				}
				if configPath == "" {
					t.Fatal("missing config")
				}
				if strings.Contains(strings.Join(args, " "), "secret-password") || strings.Contains(strings.Join(args, " "), "--password") {
					t.Error("password in args")
				}
				data, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				var cfg struct {
					Spec struct {
						Global map[string]string `json:"global"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatal(err)
				}
				if cfg.Spec.Global["password"] != "secret-password" || cfg.Spec.Global["username"] != "user" {
					t.Fatal("bad config")
				}
				if runtime.GOOS != "windows" {
					st, err := os.Stat(configPath)
					if err != nil {
						t.Fatal(err)
					}
					if st.Mode().Perm() != 0600 {
						t.Error(st.Mode())
					}
					st, err = os.Stat(filepath.Dir(configPath))
					if err != nil {
						t.Fatal(err)
					}
					if st.Mode().Perm() != 0700 {
						t.Error(st.Mode())
					}
				}
				fmtOut := []byte("untrusted secret-password")
				if _, err := out.Write(fmtOut); err != nil {
					t.Fatal(err)
				}
				if _, err := stderr.Write(fmtOut); err != nil {
					t.Fatal(err)
				}
				if fail {
					return errors.New("secret-password")
				}
				return nil
			})}
			err := r.Connect(context.Background(), Request{Username: "user", Password: "secret-password", Namespace: "payments", ARN: "arn", Region: "us-east-1", Endpoint: "https://idp.invalid", Provider: "PingNTLM"})
			if (err != nil) != fail {
				t.Fatal(err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-password") {
				t.Fatal("leaked password")
			}
			if _, err := os.Stat(filepath.Dir(configPath)); !os.IsNotExist(err) {
				t.Fatal("credentials not cleaned up", err)
			}
		})
	}
}

func TestCancellationAndOutputCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := Runner{Timeout: time.Second}
	if err := r.Connect(ctx, Request{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r.Exec = executorFunc(func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error { <-ctx.Done(); return ctx.Err() })
	r.Timeout = time.Millisecond
	if err := r.Connect(context.Background(), Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	var w cappedOutput
	if n, err := w.Write(make([]byte, 100000)); err != nil || n != 100000 || w.Len() != 65536 {
		t.Fatal(n, err, w.Len())
	}
}

func TestProcessFailureExitCode(t *testing.T) {
	// Launch this same test executable, not a shell, to exercise real process I/O.
	if os.Getenv("KCONNECT_HELPER_TEST") == "child" {
		os.Exit(7)
	}
	// Executor tests above cover argument construction. Process itself must not
	// inherit arbitrary secrets, so the helper is selected by test-name args.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr cappedOutput
	err = (Process{}).Run(context.Background(), exe, []string{"-test.run=TestProcessHelper"}, &out, &stderr)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatal(err)
	}
}
func TestProcessHelper(t *testing.T) {
	if len(os.Args) > 1 && os.Args[1] == "-test.run=TestProcessHelper" {
		if os.Getenv("DECRYPT_PASSPHRASE") != "" {
			os.Exit(8)
		}
		os.Exit(7)
	}
}
