package cli

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"eks/internal/config"
)

// Exercise database/sql's scanning and cleanup through the real listing path.
type listRowsConnector struct {
	rows   *listFailureRows
	closes int
}

func (c *listRowsConnector) Connect(context.Context) (driver.Conn, error) {
	return &listRowsConn{owner: c}, nil
}
func (*listRowsConnector) Driver() driver.Driver { return nil }

type listRowsConn struct {
	testConn
	owner *listRowsConnector
}

func (c *listRowsConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return c.owner.rows, nil
}
func (c *listRowsConn) Close() error {
	c.owner.closes++
	return nil
}

type listFailureRows struct {
	emptyRows
	data         [][]driver.Value
	after        error
	next, closes int
}

func (r *listFailureRows) Next(out []driver.Value) error {
	r.next++
	if r.next > len(r.data) {
		if r.after != nil {
			return r.after
		}
		return io.EOF
	}
	copy(out, r.data[r.next-1])
	return nil
}
func (r *listFailureRows) Close() error {
	r.closes++
	return nil
}

func TestListRejectsPartialDatabaseResults(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, failure := range []struct {
			name, operation, category string
			column                    int
			value                     driver.Value
		}{
			{"namespace", "query_validation", "validation", 0, "CANARY invalid namespace"},
			{"environment", "query_validation", "validation", 1, "CANARY invalid environment"},
			{"account", "query_validation", "validation", 3, "CANARY invalid account"},
			{"role", "query_validation", "validation", 4, "CANARY invalid role;"},
			{"scan", "query_scan", "validation", 0, nil},
			{"iteration", "query_iteration", "database", -1, nil},
		} {
			t.Run(format+"/"+failure.name, func(t *testing.T) {
				valid := []driver.Value{"canary-valid-namespace", "dev", "CANARY-ID", "987654321098", "CANARY-ROLE"}
				rows := &listFailureRows{data: [][]driver.Value{valid}}
				if failure.column >= 0 {
					invalid := append([]driver.Value(nil), valid...)
					invalid[failure.column] = failure.value
					rows.data = append(rows.data, invalid)
				} else {
					rows.after = errors.New("CANARY raw driver row /private/database")
				}
				connector := &listRowsConnector{rows: rows}
				deps := cliDeps(testConnector{})
				deps.open = func(config.Config) (*sql.DB, error) { return sql.OpenDB(connector), nil }
				deps.lookupPath = func(string) (string, error) {
					t.Fatal("listing checked an unused executable")
					return "", nil
				}
				args := cliArgs(t)
				if format == "json" {
					args = append(args[:len(args)-1], "--json", "list")
				}
				var out, stderr bytes.Buffer
				code := runWith(context.Background(), args, "test", os.Stdin, &out, &stderr, deps)
				if code != 1 {
					t.Fatalf("exit code=%d; want runtime failure (1)", code)
				}
				if out.Len() != 0 {
					t.Fatal("failed listing emitted a successful or partial namespace result")
				}
				if rows.next != 2 || rows.closes != 1 || connector.closes != 1 {
					t.Fatalf("next=%d rows closes=%d connection closes=%d; want 2, 1, 1",
						rows.next, rows.closes, connector.closes)
				}
				for _, raw := range []string{"CANARY", "canary-valid-namespace", "987654321098", "/private/database"} {
					if strings.Contains(stderr.String(), raw) {
						t.Fatal("raw row data or driver error leaked into diagnostics")
					}
				}
				records := parseRecords(t, stderr.String())
				last := records[len(records)-1]
				if last["event"] != "run.outcome" || last["status"] != "failure" || last["exit_code"] != float64(1) {
					t.Fatal("listing did not report final runtime failure", last)
				}
				seen := make(map[string]bool)
				for _, record := range records {
					event, _ := record["event"].(string)
					switch event {
					case failure.operation + ".outcome":
						if record["level"] != "ERROR" || record["error_category"] != failure.category {
							t.Fatal("missing safe primary row failure", record)
						}
						seen[event] = true
					case "query.outcome":
						if record["status"] != "failure" || record["row_count"] != float64(0) {
							t.Fatal("query reported a successful or partial result", record)
						}
						seen[event] = true
					case "rows_close.outcome", "database_close.outcome":
						if record["status"] != "success" {
							t.Fatal("database cleanup failed", record)
						}
						seen[event] = true
					case "output_list.outcome":
						t.Fatal("failed query reached listing output")
					}
				}
				for _, event := range []string{failure.operation + ".outcome", "query.outcome", "rows_close.outcome", "database_close.outcome"} {
					if !seen[event] {
						t.Fatal("missing diagnostic event", event)
					}
				}
			})
		}
	}
}

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
