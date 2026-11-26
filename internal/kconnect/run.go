// Package kconnect isolates the only external command used by the CLI.
package kconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Request struct {
	Username, Password, Namespace, ARN, Region, Endpoint, Provider string
}

type CommandError struct {
	Code  int
	Cause error
}

func (e *CommandError) Error() string { return fmt.Sprintf("kconnect failed (exit %d)", e.Code) }
func (e *CommandError) Unwrap() error { return e.Cause }

type Executor interface {
	Run(context.Context, string, []string, io.Writer, io.Writer) error
}
type Process struct{}

func (Process) Run(ctx context.Context, bin string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{}
	// Do not inherit unrelated dotenv/process secrets into the child.
	for _, key := range []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "WINDIR", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "AWS_CA_BUNDLE", "KUBECONFIG", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	return cmd.Run()
}

type Runner struct {
	Binary  string
	Timeout time.Duration
	Exec    Executor
}

// cappedOutput captures stdout/stderr separately without unbounded buffering.
// The raw output is deliberately never surfaced: external tools may echo secrets.
type cappedOutput struct{ bytes.Buffer }

func (w *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if w.Len() < 64*1024 {
		_, _ = w.Buffer.Write(p[:min(len(p), 64*1024-w.Len())])
	}
	return n, nil
}

func (r Runner) Connect(ctx context.Context, input Request) (err error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "eks-kconnect-")
	if err != nil {
		return fmt.Errorf("create private kconnect directory: %w", err)
	}
	defer func() {
		if e := os.RemoveAll(dir); e != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary credentials: %w", e))
		}
	}()
	if err := secureDirectory(dir); err != nil {
		return fmt.Errorf("restrict temporary credential directory: %w", err)
	}
	path := filepath.Join(dir, "config.json")
	// JSON is accepted by kconnect's Kubernetes JSON/YAML configuration decoder.
	data, err := json.Marshal(map[string]any{
		"apiVersion": "kconnect.fidelity.github.com/v1alpha1", "kind": "Configuration",
		"spec": map[string]any{"global": map[string]string{"username": input.Username, "password": input.Password}},
	})
	if err != nil {
		return fmt.Errorf("encode kconnect credentials: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write temporary kconnect configuration: %w", err)
	}
	args := []string{"use", "eks", "--config", path, "--no-version-check", "--no-input", "--no-history",
		"--idp-protocol", "saml", "--namespace", input.Namespace, "--role-arn", input.ARN,
		"--idp-endpoint", input.Endpoint, "--idp-provider", input.Provider, "--region", input.Region}
	var stdout, stderr cappedOutput
	executor := r.Exec
	if executor == nil {
		executor = Process{}
	}
	if err := executor.Run(ctx, r.Binary, args, &stdout, &stderr); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("kconnect interrupted or timed out: %w", ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &CommandError{Code: exit.ExitCode(), Cause: err}
		}
		return errors.New("could not execute kconnect; check executable path and permissions")
	}
	return nil
}
