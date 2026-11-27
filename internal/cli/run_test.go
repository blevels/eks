package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"eks/internal/config"
	"eks/internal/diagnostics"
)

func TestExitCodes(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want int
	}{{nil, 0}, {errors.New("runtime"), 1}, {context.DeadlineExceeded, 1}, {context.Canceled, 130}, {diagnostics.Declined(), 1}} {
		if got := ExitCode(tt.err); got != tt.want {
			t.Fatal(got, tt.want)
		}
	}
}
func TestOfflineCLI(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		code     int
		contains string
	}{
		{"help", []string{"--help"}, 0, "Usage:"},
		{"version", []string{"--version"}, 0, "test-version"},
		{"invalid", []string{"--unknown-password=do-not-echo"}, 2, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := Run(context.Background(), tt.args, "test-version", os.Stdin, &out, &stderr)
			if code != tt.code || !strings.Contains(out.String(), tt.contains) {
				t.Fatal(code, out.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "do-not-echo") {
				t.Fatal("echoed unknown input")
			}
		})
	}
}
func TestRedaction(t *testing.T) {
	c := config.Config{DBPassword: "db-secret", Passphrase: "fixture-only", ProductionPassword: "prod-secret",
		EncryptedPassword: "U2FsdGVkX18BAgMEBQYHCE7cMDd4cyD+typtkEMxGBQ=", Digest: "sha256"}
	got := safeError(errors.New("db-secret prod-secret sample-user fixture-only\x1b"), c)
	for _, s := range []string{"db-secret", "prod-secret", "sample-user", "fixture-only", "\x1b"} {
		if strings.Contains(got, s) {
			t.Fatal("leak", got)
		}
	}
}
