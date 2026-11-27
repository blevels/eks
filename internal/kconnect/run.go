// Package kconnect isolates the only external command used by the CLI.
package kconnect

import (
	"bytes"
	"context"
	"eks/internal/diagnostics"
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

func (e *CommandError) Error() string          { return fmt.Sprintf("kconnect failed (exit %d)", e.Code) }
func (e *CommandError) Unwrap() error          { return e.Cause }
func (e *CommandError) DiagnosticError() error { return &CommandError{Code: e.Code} }

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
	Files   FileSystem
}

// FileSystem permits deterministic failure injection without changing normal
// temporary-file lifetime or credential delivery.
type FileSystem interface {
	MkdirTemp(string, string) (string, error)
	Protect(string) error
	WriteFile(string, []byte, os.FileMode) error
	RemoveAll(string) error
}
type localFiles struct{}

func (localFiles) MkdirTemp(dir, pattern string) (string, error) { return os.MkdirTemp(dir, pattern) }
func (localFiles) Protect(path string) error                     { return secureDirectory(path) }
func (localFiles) WriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}
func (localFiles) RemoveAll(path string) error { return os.RemoveAll(path) }

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
	ctx, connection := diagnostics.Start(ctx, "subprocess", "kconnect")
	defer func() { err = connection.End(err) }()
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	files := r.Files
	if files == nil {
		files = localFiles{}
	}
	_, directory := diagnostics.Start(ctx, "filesystem", "private_directory")
	dir, err := files.MkdirTemp("", "eks-kconnect-")
	err = directory.End(err)
	if err != nil {
		return fmt.Errorf("create private kconnect directory: %w", err)
	}
	defer func() {
		_, cleanup := diagnostics.Start(ctx, "filesystem", "cleanup")
		if e := files.RemoveAll(dir); e != nil {
			err = errors.Join(err, cleanup.End(e))
		} else {
			_ = cleanup.End(nil)
		}
	}()
	_, protection := diagnostics.Start(ctx, "filesystem", "directory_protection")
	if err := protection.End(files.Protect(dir)); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")
	// JSON is accepted by kconnect's Kubernetes JSON/YAML configuration decoder.
	_, encode := diagnostics.Start(ctx, "filesystem", "credential_encode")
	data, err := json.Marshal(map[string]any{
		"apiVersion": "kconnect.fidelity.github.com/v1alpha1", "kind": "Configuration",
		"spec": map[string]any{"global": map[string]string{"username": input.Username, "password": input.Password}},
	})
	err = encode.End(err)
	if err != nil {
		return fmt.Errorf("encode kconnect credentials: %w", err)
	}
	_, file := diagnostics.Start(ctx, "filesystem", "credential_file")
	if err := file.End(files.WriteFile(path, data, 0600)); err != nil {
		return err
	}
	args := []string{"use", "eks", "--config", path, "--no-version-check", "--no-input", "--no-history",
		"--idp-protocol", "saml", "--namespace", input.Namespace, "--role-arn", input.ARN,
		"--idp-endpoint", input.Endpoint, "--idp-provider", input.Provider, "--region", input.Region}
	var stdout, stderr cappedOutput
	executor := r.Exec
	if executor == nil {
		executor = Process{}
	}
	childCtx, child := diagnostics.Start(ctx, "subprocess", "subprocess")
	if err := executor.Run(childCtx, r.Binary, args, &stdout, &stderr); err != nil {
		if ctx.Err() != nil {
			return child.End(ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return child.End(&CommandError{Code: exit.ExitCode(), Cause: err}, diagnostics.N("child_exit_code", exit.ExitCode()))
		}
		return child.End(err)
	}
	_ = child.End(nil)
	return nil
}
