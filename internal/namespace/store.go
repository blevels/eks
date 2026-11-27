// Package namespace contains typed, read-only namespace discovery.
package namespace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"time"

	"eks/internal/config"
	"eks/internal/diagnostics"
	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx's database/sql driver.
)

type Namespace struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
	ID          string `json:"id"`
	Account     string `json:"account"`
	Role        string `json:"role"`
}

func (n Namespace) ARN() string { return "arn:aws:iam::" + n.Account + ":role/" + n.Role }

func (n Namespace) Validate() error {
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(n.Name) || len(n.Name) > 63 {
		return errors.New("database returned an invalid Kubernetes namespace")
	}
	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(n.Account) {
		return errors.New("database returned an invalid AWS account")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9+=,.@_/-]+$`).MatchString(n.Role) {
		return errors.New("database returned an invalid IAM role")
	}
	switch n.Environment {
	case "dev", "sit", "uat", "prod":
	default:
		return errors.New("database returned an invalid environment: expected dev, sit, uat, or prod")
	}
	return nil
}

type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type Store struct {
	DB      Queryer
	Timeout time.Duration
}

// Open always requests hostname-verified TLS. URL encoding protects special
// characters in credentials; the URL must never be logged.
func Open(c config.Config) (*sql.DB, error) {
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.DBHost, c.DBPort), Path: "/" + c.DBName, User: url.UserPassword(c.DBUser, c.DBPassword)}
	q := url.Values{"sslmode": {"verify-full"}, "connect_timeout": {fmt.Sprint(max(1, int(c.Timeout.Seconds())))}}
	if c.DBRootCert != "" {
		q.Set("sslrootcert", c.DBRootCert)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		return nil, fmt.Errorf("initialize PostgreSQL driver: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

// Query is kept separate so argument binding and field order can be tested.
func Query(mode, target string) (string, []any, error) {
	base := `SELECT n.ns, a.env, n.ns_id::text, a.ns_account::text, a.ns_role FROM eks.namespaces n INNER JOIN eks.accounts a ON n.env = a.env`
	var args []any
	switch mode {
	case "list":
	case "id":
		base += " WHERE n.ns_id = $1"
		args = []any{target}
	case "name":
		base += " WHERE n.ns = $1"
		args = []any{target}
	default:
		return "", nil, fmt.Errorf("unknown namespace lookup mode %q", mode)
	}
	return base + " ORDER BY n.ns, a.env, a.ns_account, a.ns_role, n.ns_id", args, nil
}

func (s Store) Find(ctx context.Context, mode, target string) (result []Namespace, err error) {
	ctx, scope := diagnostics.Start(ctx, "database", "query")
	defer func() { err = scope.End(err, diagnostics.N("row_count", len(result))) }()
	query, args, err := Query(mode, target)
	if err != nil {
		_, plan := diagnostics.Start(ctx, "database", "query_plan")
		return nil, plan.End(err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query namespaces: %w", err)
	}
	defer func() {
		_, cleanup := diagnostics.Start(ctx, "database", "rows_close")
		if e := rows.Close(); e != nil {
			err = errors.Join(err, cleanup.End(e))
		} else {
			_ = cleanup.End(nil)
		}
	}()
	result = []Namespace{}
	for rows.Next() {
		var n Namespace
		if err := rows.Scan(&n.Name, &n.Environment, &n.ID, &n.Account, &n.Role); err != nil {
			_, scan := diagnostics.Start(ctx, "database", "query_scan")
			return nil, scan.End(err)
		}
		if err := n.Validate(); err != nil {
			_, validation := diagnostics.Start(ctx, "database", "query_validation")
			return nil, validation.End(err)
		}
		result = append(result, n)
	}
	if err := rows.Err(); err != nil {
		_, iteration := diagnostics.Start(ctx, "database", "query_iteration")
		return nil, iteration.End(err)
	}
	return result, nil
}
