package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"eks/internal/config"
)

func TestListUnusedSettingsIntegration(t *testing.T) {
	settings := []struct{ flag, env string }{
		{"interval", "INTERVAL"},
		{"command-timeout", "COMMAND_TIMEOUT"},
		{"firecall-poll-interval", "FIRECALL_POLL_INTERVAL"},
		{"firecall-activation-timeout", "FIRECALL_ACTIVATION_TIMEOUT"},
		{"duration", "DURATION_IN_HRS"},
	}
	for _, source := range []string{"environment", "dotenv", "flag", "override"} {
		for _, s := range settings {
			t.Run(source+"/"+s.flag, func(t *testing.T) {
				args := cliArgs(t)
				args = args[:len(args)-1]
				deps := cliDeps(testConnector{})
				switch source {
				case "environment", "override":
					deps.lookupEnv = func(key string) (string, bool) {
						return "CANARY", key == s.env
					}
					if source == "override" {
						args = append(args, "--"+s.flag+"=7")
					}
				case "dotenv":
					data := "PGHOST=synthetic.invalid\nPGUSER=synthetic\n" + s.env + "=CANARY\n"
					if err := os.WriteFile(args[1], []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				case "flag":
					args = append(args, "--"+s.flag+"=CANARY")
				}
				args = append(args, "--json", "list")
				opened := false
				open := deps.open
				deps.open = func(c config.Config) (*sql.DB, error) {
					opened = true
					if !c.List || c.Timeout <= 0 {
						t.Fatal("listing lost its database timeout")
					}
					return open(c)
				}
				deps.lookupPath = func(string) (string, error) {
					t.Fatal("listing checked an unused executable")
					return "", nil
				}
				var out, stderr bytes.Buffer
				code := runWith(context.Background(), args, "test", os.Stdin, &out, &stderr, deps)
				wantCode := 0
				if source == "flag" {
					wantCode = 2
				}
				if code != wantCode || opened != (wantCode == 0) {
					t.Fatalf("code=%d opened=%v; want code=%d", code, opened, wantCode)
				}
				if strings.Contains(stderr.String(), "CANARY") || strings.Contains(out.String(), "CANARY") {
					t.Fatal("unused setting leaked into output")
				}
				if wantCode != 0 {
					if out.Len() != 0 {
						t.Fatal("usage error wrote result output")
					}
					return
				}
				var result struct {
					Event string `json:"event"`
				}
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Event != "namespaces" {
					t.Fatal("listing did not emit a JSON namespace result", err)
				}
				records := parseRecords(t, stderr.String())
				last := records[len(records)-1]
				if last["event"] != "run.outcome" || last["exit_code"] != float64(0) {
					t.Fatal("listing did not report successful completion")
				}
			})
		}
	}
}

func TestListStillRejectsInvalidCommonSettings(t *testing.T) {
	for _, setting := range []struct{ env, value string }{
		{"TIMEOUT", "0"},
		{"PGHOST", ""},
		{"PGUSER", ""},
		{"PGDATABASE", ""},
		{"PGPORT", "65536"},
		{"EKS_LOG_FORMAT", "CANARY"},
		{"EKS_LOG_SOURCE", "CANARY"},
	} {
		t.Run(setting.env, func(t *testing.T) {
			args := cliArgs(t)
			// Do not let cliArgs' explicit log format mask the inherited value.
			args = []string{"--env-file", args[1], "--json", "list"}
			deps := cliDeps(testConnector{})
			deps.lookupEnv = func(key string) (string, bool) {
				if key == "INTERVAL" {
					return "CANARY", true
				}
				return setting.value, key == setting.env
			}
			deps.open = func(config.Config) (*sql.DB, error) {
				t.Fatal("invalid common settings reached the database")
				return nil, nil
			}
			deps.lookupPath = func(string) (string, error) {
				t.Fatal("invalid common settings reached an executable")
				return "", nil
			}
			var out, stderr bytes.Buffer
			code := runWith(context.Background(), args, "test", os.Stdin, &out, &stderr, deps)
			if code != 2 || out.Len() != 0 || strings.Contains(stderr.String(), "CANARY") {
				t.Fatal("common validation or safe usage output changed", code)
			}
		})
	}
}
