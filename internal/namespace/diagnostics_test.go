package namespace

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"eks/internal/diagnostics/diagtest"
)

func TestDatabaseDiagnosticOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name, event string
		c           connector
		mode        string
		category    string
	}{
		{"rows", "query.outcome", connector{rows: [][]driver.Value{{"payments", "dev", "12", "123456789012", "Role"}}}, "id", ""},
		{"empty", "query.outcome", connector{}, "id", ""},
		{"driver", "query.outcome", connector{queryErr: errors.New("CANARY postgres /private/file")}, "id", "database"},
		{"scan", "query_scan.outcome", connector{rows: [][]driver.Value{{nil, "dev", "12", "123456789012", "Role"}}}, "id", "validation"},
		{"validation", "query_validation.outcome", connector{rows: [][]driver.Value{{"CANARY!", "dev", "12", "123456789012", "Role"}}}, "id", "validation"},
		{"iteration", "query_iteration.outcome", connector{errorAfter: errors.New("CANARY driver rows")}, "id", "database"},
		{"timeout", "query.outcome", connector{queryErr: context.DeadlineExceeded}, "id", "timeout"},
		{"cancel", "query.outcome", connector{queryErr: context.Canceled}, "id", "canceled"},
		{"plan", "query_plan.outcome", connector{}, "CANARY", "validation"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			db := sql.OpenDB(tt.c)
			defer db.Close()
			_, err := (Store{DB: db, Timeout: time.Second}).Find(ctx, tt.mode, "CANARY selector")
			if (err != nil) != (tt.category != "") {
				t.Fatal(err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "postgres /private", "SELECT", "123456789012", "payments")
			if tt.category != "" && logs.Event(tt.event)["error_category"] != tt.category {
				t.Fatal(logs.Text())
			}
			if tt.name == "empty" && logs.Event(tt.event)["row_count"] != float64(0) {
				t.Fatal(logs.Text())
			}
		})
	}
}

func TestCacheDiagnosticDecisionsAndConcurrentClear(t *testing.T) {
	ctx, logs := diagtest.New(t)
	started, release := make(chan struct{}), make(chan struct{})
	c := makeCache(t, findFunc(func(_ context.Context, _, target string) ([]Namespace, error) {
		if target == "CANARY slow" {
			close(started)
			<-release
		}
		return []Namespace{}, nil
	}), 1)
	c.Diagnostics = logs.D
	for _, target := range []string{"CANARY warm", "CANARY warm", "CANARY evict"} {
		if _, err := c.Find(ctx, "name", target); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.Find(ctx, "name", "CANARY slow"); err != nil {
			t.Error(err)
		}
	}()
	<-started
	waitCtx, stop := context.WithTimeout(ctx, 5*time.Millisecond)
	defer stop()
	if _, err := c.Find(waitCtx, "name", "CANARY queued"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	c.Clear()
	close(release)
	wg.Wait()
	for _, event := range []string{"cache.hit", "cache.miss", "cache.wait", "cache.fill", "cache.empty", "cache.eviction", "cache.clear", "cache.suppressed"} {
		logs.Event(event)
	}
	if logs.Event("cache_wait.outcome") == nil {
		t.Fatal("missing wait outcome")
	}
	timedOut := false
	for _, r := range logs.Records() {
		if r["event"] == "cache_wait.outcome" && r["status"] == "timeout" {
			timedOut = true
		}
	}
	if !timedOut {
		t.Fatal("missing queued timeout", logs.Text())
	}
	logs.Contract()
	logs.NoSecrets("CANARY")
}

func TestCacheDiagnosticFailureOutcomes(t *testing.T) {
	for _, scenario := range []string{"initial_cancel", "source_cancel", "source_timeout", "source_error", "invalid_mode", "invalid_row"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			cause := errors.New("CANARY source error")
			if scenario == "source_cancel" || scenario == "initial_cancel" {
				cause = context.Canceled
			}
			if scenario == "source_timeout" {
				cause = context.DeadlineExceeded
			}
			c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
				if scenario == "invalid_row" {
					return []Namespace{{Name: "CANARY invalid"}}, nil
				}
				return nil, cause
			}), 1)
			mode := "id"
			if scenario == "invalid_mode" {
				mode = "CANARY"
			}
			if scenario == "initial_cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := c.Find(ctx, mode, "CANARY selector")
			if err == nil {
				t.Fatal("missing failure")
			}
			if scenario != "invalid_mode" && scenario != "invalid_row" && !errors.Is(err, cause) {
				t.Fatal("lost error identity")
			}
			if scenario == "source_cancel" || scenario == "initial_cancel" {
				if logs.Event("cache_lookup.outcome")["status"] != "canceled" {
					t.Fatal(logs.Text())
				}
			}
			logs.Contract()
			logs.NoSecrets("CANARY")
		})
	}
}

func TestDatabaseRowCloseDiagnostics(t *testing.T) {
	for _, early := range []bool{false, true} {
		ctx, logs := diagtest.New(t)
		c := connector{closeErr: errors.New("CANARY close rows")}
		if early {
			c.rows = [][]driver.Value{{"CANARY invalid", "dev", "12", "123456789012", "Role"}}
		}
		db := sql.OpenDB(c)
		_, err := (Store{DB: db, Timeout: time.Second}).Find(ctx, "list", "CANARY")
		_ = db.Close()
		if err == nil {
			t.Fatal("row close failure ignored")
		}
		if early {
			if logs.Event("query_validation.outcome")["level"] != "ERROR" || logs.Event("rows_close.outcome")["level"] != "ERROR" {
				t.Fatal(logs.Text())
			}
		} else {
			// database/sql auto-closes at EOF and exposes this via Rows.Err.
			if logs.Event("query_iteration.outcome")["level"] != "ERROR" {
				t.Fatal(logs.Text())
			}
		}
		logs.Contract()
		logs.NoSecrets("CANARY")
	}
}
