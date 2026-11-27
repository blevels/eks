package namespace

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"eks/internal/config"
)

func TestQueryAndValidation(t *testing.T) {
	for _, mode := range []string{"id", "name", "list"} {
		query, args, err := Query(mode, `x'; DROP TABLE eks.namespaces; --`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(query, "DROP") {
			t.Fatal("interpolated SQL")
		}
		if mode == "list" && len(args) != 0 {
			t.Fatal(args)
		}
		if mode != "list" && (len(args) != 1 || !strings.Contains(query, "$1")) {
			t.Fatal(query, args)
		}
	}
	if _, _, err := Query("bad", ""); err == nil {
		t.Fatal("bad mode")
	}
	valid := Namespace{Name: "payments", Environment: "prod", ID: "12", Account: "123456789012", Role: "path/Role"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if valid.ARN() != "arn:aws:iam::123456789012:role/path/Role" {
		t.Fatal(valid.ARN())
	}
	for _, mutate := range []func(*Namespace){
		func(n *Namespace) { n.Name = "Bad Name" }, func(n *Namespace) { n.Account = "123" },
		func(n *Namespace) { n.Role = "bad;role" }, func(n *Namespace) { n.Environment = "" },
	} {
		n := valid
		mutate(&n)
		if n.Validate() == nil {
			t.Fatal("invalid row accepted", n)
		}
	}
}

// Use database/sql's real scanning/rows machinery with a minimal test driver.
type connector struct {
	rows                 [][]driver.Value
	queryErr, errorAfter error
	closeErr             error
	seen                 *bool
}

func (c connector) Connect(context.Context) (driver.Conn, error) { return &conn{c: c}, nil }
func (c connector) Driver() driver.Driver                        { return testDriver{} }

type testDriver struct{}

func (testDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type conn struct{ c connector }

func (c *conn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *conn) Close() error                        { return nil }
func (c *conn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (c *conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing query deadline")
	}
	if c.c.seen != nil {
		*c.c.seen = true
	}
	if c.c.queryErr != nil {
		return nil, c.c.queryErr
	}
	return &testRows{data: c.c.rows, after: c.c.errorAfter, closeErr: c.c.closeErr}, nil
}

type testRows struct {
	data     [][]driver.Value
	at       int
	after    error
	closeErr error
}

func (r *testRows) Columns() []string { return []string{"name", "env", "id", "account", "role"} }
func (r *testRows) Close() error      { return r.closeErr }
func (r *testRows) Next(out []driver.Value) error {
	if r.at == len(r.data) {
		if r.after != nil {
			return r.after
		}
		return io.EOF
	}
	copy(out, r.data[r.at])
	r.at++
	return nil
}

func TestFind(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    connector
		want int
		bad  bool
	}{
		{"rows", connector{rows: [][]driver.Value{{"payments", "dev", "12", "123456789012", "Role"}}}, 1, false},
		{"empty", connector{}, 0, false},
		{"query error", connector{queryErr: errors.New("offline")}, 0, true},
		{"row error", connector{errorAfter: errors.New("row read error")}, 0, true},
		{"null", connector{rows: [][]driver.Value{{nil, "dev", "12", "123456789012", "Role"}}}, 0, true},
		{"invalid", connector{rows: [][]driver.Value{{"payments", "dev", "12", "bad", "Role"}}}, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := sql.OpenDB(tt.c)
			defer db.Close()
			found, err := (Store{DB: db, Timeout: time.Second}).Find(context.Background(), "id", "12")
			if (err != nil) != tt.bad || len(found) != tt.want {
				t.Fatalf("%v %v", found, err)
			}
		})
	}
}
func TestOpen(t *testing.T) {
	db, err := Open(config.Config{DBHost: "db.example.invalid", DBPort: "5432", DBUser: "user", DBPassword: "a&b=?'", DBName: "postgres", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
