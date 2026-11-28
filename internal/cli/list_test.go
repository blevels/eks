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
	"sync/atomic"
	"testing"
	"time"

	"eks/internal/config"
)

// blockingListConnector keeps the real database/sql path, but does not return
// from the query until its context is canceled.
type blockingListConnector struct {
	started      chan struct{}
	queryContext context.Context
	connections  atomic.Int32
	queries      atomic.Int32
	closes       atomic.Int32
}

func (c *blockingListConnector) Connect(context.Context) (driver.Conn, error) {
	c.connections.Add(1)
	return &blockingListConn{owner: c}, nil
}

func (*blockingListConnector) Driver() driver.Driver { return nil }

type blockingListConn struct {
	testConn
	owner *blockingListConnector
}

func (c *blockingListConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.owner.queries.Add(1)
	c.owner.queryContext = ctx
	close(c.owner.started)
	<-ctx.Done()
	return nil, errors.Join(ctx.Err(), errors.New("CANARY database detail /home/private"))
}

func (c *blockingListConn) Close() error {
	c.owner.closes.Add(1)
	return nil
}

func TestListDatabaseCancellationIntegration(t *testing.T) {
	for _, tt := range []struct {
		name         string
		timeout      time.Duration
		cancelParent bool
		wantCode     int
		wantErr      error
		category     string
	}{
		{"timeout", 100 * time.Millisecond, false, 1, context.DeadlineExceeded, "timeout"},
		{"parent cancellation", 30 * time.Second, true, 130, context.Canceled, "canceled"},
	} {
		for _, jsonOutput := range []bool{false, true} {
			format := "text"
			if jsonOutput {
				format = "json"
			}
			t.Run(tt.name+"/"+format, func(t *testing.T) {
				args := cliArgs(t)
				args = append(args[:len(args)-1], "--timeout="+tt.timeout.String())
				if jsonOutput {
					args = append(args, "--json")
				}
				args = append(args, "list")

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// Bound a regression that drops either the configured timeout or the parent
				// context. The elapsed-time and error assertions distinguish this safety net.
				watchdog := time.AfterFunc(5*time.Second, cancel)
				defer watchdog.Stop()
				connector := &blockingListConnector{started: make(chan struct{})}
				if tt.cancelParent {
					go func() {
						select {
						case <-connector.started:
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				deps := cliDeps(testConnector{})
				var db *sql.DB
				deps.open = func(c config.Config) (*sql.DB, error) {
					if !c.List || c.Timeout != tt.timeout {
						t.Fatalf("listing config: list=%v timeout=%v", c.List, c.Timeout)
					}
					db = sql.OpenDB(connector)
					t.Cleanup(func() { _ = db.Close() })
					return db, nil
				}
				deps.lookupPath = func(string) (string, error) {
					t.Fatal("listing checked an unused executable")
					return "", nil
				}
				var out, stderr bytes.Buffer
				start := time.Now()
				code := runWith(ctx, args, "test", os.Stdin, &out, &stderr, deps)
				if elapsed := time.Since(start); elapsed >= 3*time.Second {
					t.Fatalf("listing did not stop promptly: %v", elapsed)
				}
				if code != tt.wantCode {
					t.Fatalf("exit code=%d; want %d\n%s", code, tt.wantCode, stderr.String())
				}
				if connector.queries.Load() != 1 || connector.queryContext == nil {
					t.Fatal("listing did not reach the blocking database query exactly once")
				}
				if !errors.Is(connector.queryContext.Err(), tt.wantErr) {
					t.Fatalf("query context error=%v; want %v", connector.queryContext.Err(), tt.wantErr)
				}
				if !tt.cancelParent && ctx.Err() != nil {
					t.Fatal("query timeout canceled the parent context")
				}
				if db == nil || connector.connections.Load() != 1 || connector.closes.Load() != 1 || db.Stats().InUse != 0 {
					t.Fatal("listing did not close its database connection")
				}
				if err := db.PingContext(context.Background()); err == nil || connector.connections.Load() != 1 {
					t.Fatal("listing left its database handle open")
				}
				if out.Len() != 0 {
					t.Fatalf("canceled listing wrote result output: %q", out.String())
				}
				for _, secret := range []string{"CANARY", "/home/private", "synthetic.invalid", "SELECT"} {
					if strings.Contains(stderr.String(), secret) {
						t.Fatalf("database detail leaked into diagnostics: %q", secret)
					}
				}
				records := parseRecords(t, stderr.String())
				last := records[len(records)-1]
				if last["event"] != "run.outcome" || last["exit_code"] != float64(tt.wantCode) || last["status"] != tt.category {
					t.Fatalf("unexpected final outcome: %v", last)
				}
				queryOutcomes, closeOutcomes := 0, 0
				for _, record := range records {
					switch record["event"] {
					case "query.outcome":
						queryOutcomes++
						if record["status"] != tt.category || record["error_category"] != tt.category || record["error_code"] != "query_failed" {
							t.Fatalf("unexpected query diagnostic: %v", record)
						}
					case "database_close.outcome":
						closeOutcomes++
						if record["status"] != "success" {
							t.Fatalf("database cleanup failed: %v", record)
						}
					case "output_list.start", "output_list.outcome":
						t.Fatal("canceled listing reached success output")
					}
				}
				if queryOutcomes != 1 || closeOutcomes != 1 {
					t.Fatalf("query outcomes=%d cleanup outcomes=%d; want one each", queryOutcomes, closeOutcomes)
				}
			})
		}
	}
}

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
