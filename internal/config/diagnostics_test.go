package config

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"eks/internal/diagnostics/diagtest"
)

type envReadCloser struct {
	io.Reader
	closeErr error
}

func (r envReadCloser) Close() error { return r.closeErr }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("CANARY read /home/private") }

func TestDotenvDiagnosticBoundaries(t *testing.T) {
	for _, scenario := range []string{"success", "optional_absent", "open", "read", "validation", "close", "read_and_close"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			open := func(string) (io.ReadCloser, error) {
				switch scenario {
				case "optional_absent":
					return nil, os.ErrNotExist
				case "open":
					return nil, errors.New("CANARY open /home/private")
				case "read":
					return envReadCloser{Reader: errorReader{}}, nil
				case "read_and_close":
					return envReadCloser{Reader: errorReader{}, closeErr: errors.New("CANARY close")}, nil
				case "validation":
					return envReadCloser{Reader: strings.NewReader("CANARY=1\nCANARY=2\n")}, nil
				case "close":
					return envReadCloser{Reader: strings.NewReader("CANARY=value\n"), closeErr: errors.New("CANARY close")}, nil
				}
				return envReadCloser{Reader: strings.NewReader("CANARY=value\n")}, nil
			}
			_, err := loadEnvWith(ctx, "CANARY/path", scenario != "optional_absent", open)
			if (err == nil) != (scenario == "success" || scenario == "optional_absent") {
				t.Fatal(err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "/home/private")
			if scenario == "read_and_close" {
				count := 0
				for _, r := range logs.Records() {
					if r["level"] == "ERROR" {
						count++
					}
				}
				if count != 2 {
					t.Fatal(logs.Text())
				}
			}
		})
	}
}

func TestLoggingConfigurationPrecedence(t *testing.T) {
	path := t.TempDir() + "/synthetic.env"
	if err := os.WriteFile(path, []byte("PGHOST=synthetic.invalid\nPGUSER=synthetic\nEKS_LOG_FORMAT=json\nEKS_LOG_SOURCE=true\nEKS_LOG_FILE=dotenv.log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--env-file", path}
	lookup := func(k string) (string, bool) {
		v, ok := map[string]string{"EKS_LOG_FORMAT": "text", "EKS_LOG_SOURCE": "false", "EKS_LOG_FILE": "environment.log"}[k]
		return v, ok
	}
	for _, tt := range []struct {
		args         []string
		lookup       func(string) (string, bool)
		format, file string
		source       bool
	}{
		{append(append([]string{}, base...), "list"), func(string) (string, bool) { return "", false }, "json", "dotenv.log", true},
		{append(append([]string{}, base...), "list"), lookup, "text", "environment.log", false},
		{append(append([]string{}, base...), "--log-format", "json", "--log-file", "flag.log", "--log-source", "list"), lookup, "json", "flag.log", true},
	} {
		c, err := Parse(tt.args, tt.lookup, io.Discard)
		if err != nil || c.LogFormat != tt.format || c.LogFile != tt.file || c.LogSource != tt.source {
			t.Fatal(c.LogFormat, c.LogFile, c.LogSource, err)
		}
	}
	_, err := ParseContext(context.Background(), append(base, "--log-format", "CANARY", "list"), lookup, io.Discard)
	if err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatal(err)
	}
	invalidLookup := func(k string) (string, bool) {
		if k == "EKS_LOG_SOURCE" || k == "EKS_LOG_FORMAT" {
			return "CANARY", true
		}
		return "", false
	}
	if _, err := Parse(append(append([]string{}, base...), "list"), invalidLookup, io.Discard); err == nil {
		t.Fatal("invalid logging environment accepted")
	}
	overridden := append(append([]string{}, base...), "--log-format=json", "--log-source=false", "list")
	c, err := Parse(overridden, invalidLookup, io.Discard)
	if err != nil || c.LogFormat != "json" || c.LogSource {
		t.Fatal("flags did not override invalid environment", err)
	}
}
