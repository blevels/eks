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
	"path/filepath"
	"strings"
	"testing"

	"eks/internal/config"
)

type testConnector struct{ queryErr, closeErr error }

func (c testConnector) Connect(context.Context) (driver.Conn, error) { return testConn{c}, nil }
func (testConnector) Driver() driver.Driver                          { return nil }

type testConn struct{ c testConnector }

func (c testConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c testConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (c testConn) Close() error                        { return c.c.closeErr }
func (c testConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.c.queryErr != nil {
		return nil, c.c.queryErr
	}
	return emptyRows{}, nil
}

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"name", "env", "id", "account", "role"} }
func (emptyRows) Next([]driver.Value) error { return io.EOF }
func (emptyRows) Close() error              { return nil }

type badWriter struct{}

func (badWriter) Write([]byte) (int, error) { return 0, errors.New("CANARY writer") }

func cliArgs(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte("PGHOST=synthetic.invalid\nPGUSER=synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--env-file", path, "--log-format", "json", "--verbose", "list"}
}
func cliDeps(c testConnector) dependencies {
	return dependencies{
		open:       func(config.Config) (*sql.DB, error) { return sql.OpenDB(c), nil },
		lookupPath: func(string) (string, error) { return "synthetic", nil },
		lookupEnv:  func(string) (string, bool) { return "", false },
	}
}
func parseRecords(t *testing.T, text string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err, line)
		}
		records = append(records, r)
	}
	return records
}

func TestCLIOutcomesAndCleanupOwnership(t *testing.T) {
	for _, failure := range []string{"success", "query", "close", "both", "driver", "output", "sink_startup"} {
		t.Run(failure, func(t *testing.T) {
			args := cliArgs(t)
			c := testConnector{}
			if failure == "query" || failure == "both" {
				c.queryErr = errors.New("CANARY remote /home/private")
			}
			if failure == "close" || failure == "both" {
				c.closeErr = errors.New("CANARY database close")
			}
			deps := cliDeps(c)
			if failure == "driver" {
				deps.open = func(config.Config) (*sql.DB, error) { return nil, errors.New("CANARY driver") }
			}
			if failure == "sink_startup" {
				args = append([]string{"--log-file", filepath.Join(t.TempDir(), "absent", "events")}, args...)
			}
			var out, stderr bytes.Buffer
			var writer io.Writer = &out
			if failure == "output" {
				writer = badWriter{}
			}
			code := runWith(context.Background(), args, "test", os.Stdin, writer, &stderr, deps)
			if (code == 0) != (failure == "success") {
				t.Fatal(code, stderr.String())
			}
			if strings.Contains(stderr.String(), "CANARY") || strings.Contains(stderr.String(), "/home/private") {
				t.Fatal("leak", stderr.String())
			}
			if failure == "sink_startup" {
				if !strings.Contains(stderr.String(), "log_sink_failed") || code != 1 {
					t.Fatal(code, stderr.String())
				}
				return
			}
			records := parseRecords(t, stderr.String())
			last := records[len(records)-1]
			if last["event"] != "run.outcome" || last["exit_code"] != float64(code) {
				t.Fatal(last)
			}
			owners := 0
			for _, r := range records {
				if r["level"] == "ERROR" && r["operation"] != "run" {
					owners++
				}
			}
			want := 1
			if failure == "success" {
				want = 0
			}
			if failure == "both" {
				want = 2
			}
			if owners != want {
				t.Fatal("duplicate or missing primary", owners, stderr.String())
			}
		})
	}
}

func TestBootstrapErrorsUseSafeTextFallback(t *testing.T) {
	invalid := filepath.Join(t.TempDir(), "invalid.env")
	if err := os.WriteFile(invalid, []byte("CANARY=1\nCANARY=2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--CANARY-unknown=CANARY"},
		{"--env-file", filepath.Join(t.TempDir(), "CANARY-missing")},
		{"--env-file", invalid},
		{"--log-format", "CANARY", "list"},
	} {
		var out, stderr bytes.Buffer
		code := runWith(context.Background(), args, "test", os.Stdin, &out, &stderr, cliDeps(testConnector{}))
		if code != 2 || strings.Contains(stderr.String(), "CANARY") || !strings.Contains(stderr.String(), "schema_version=1") {
			t.Fatal(code, stderr.String())
		}
		if out.Len() != 0 {
			t.Fatal("diagnostic entered stdout")
		}
	}
	if got := runWith(context.Background(), []string{"--CANARY"}, "test", os.Stdin, io.Discard, badWriter{}, cliDeps(testConnector{})); got != 1 {
		t.Fatal(got)
	}
}

func TestCLILogSinkFailureForcesOrderlyNonzeroExit(t *testing.T) {
	code := runWith(context.Background(), cliArgs(t), "test", os.Stdin, io.Discard, badWriter{}, cliDeps(testConnector{}))
	if code != 1 {
		t.Fatal(code)
	}
}

func TestCLIHelpVersionOutputAndDependencyFailures(t *testing.T) {
	for _, arg := range []string{"--help", "--version"} {
		deps := cliDeps(testConnector{})
		deps.open = func(config.Config) (*sql.DB, error) { t.Fatal("offline option accessed database"); return nil, nil }
		if code := runWith(context.Background(), []string{arg}, "test", os.Stdin, badWriter{}, io.Discard, deps); code != 1 {
			t.Fatal(code)
		}
	}
	args := cliArgs(t)
	envPath := args[1]
	if err := os.WriteFile(envPath, []byte("PGHOST=synthetic.invalid\nPGUSER=synthetic\nENC_USERNAME=CANARY\nENC_PASSWORD=CANARY\nDECRYPT_PASSPHRASE=CANARY\nIDP_ENDPOINT=https://synthetic.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = "12"
	deps := cliDeps(testConnector{})
	deps.lookupPath = func(string) (string, error) { return "", errors.New("CANARY path") }
	var stderr bytes.Buffer
	if code := runWith(context.Background(), args, "test", os.Stdin, io.Discard, &stderr, deps); code != 1 {
		t.Fatal(code)
	}
	if strings.Contains(stderr.String(), "CANARY") || !strings.Contains(stderr.String(), `"error_code":"dependency_failed"`) {
		t.Fatal(stderr.String())
	}
	stderr.Reset()
	deps.lookupPath = func(string) (string, error) { return "synthetic", nil }
	if code := runWith(context.Background(), args, "test", os.Stdin, io.Discard, &stderr, deps); code != 1 {
		t.Fatal(code)
	}
	found := false
	for _, r := range parseRecords(t, stderr.String()) {
		if r["event"] == "dependency.outcome" && r["status"] == "success" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing executable validation success", stderr.String())
	}
}
