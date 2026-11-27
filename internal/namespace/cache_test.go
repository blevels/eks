package namespace

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
	"time"
)

type findFunc func(context.Context, string, string) ([]Namespace, error)

func (f findFunc) Find(ctx context.Context, mode, target string) ([]Namespace, error) {
	return f(ctx, mode, target)
}

func cacheRow() Namespace {
	return Namespace{Name: "payments", Environment: "dev", ID: "12", Account: "123456789012", Role: "Role"}
}

func makeCache(t *testing.T, source finder, limit int) *Cache {
	t.Helper()
	c, err := NewCache(source, limit)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCacheCompleteResultsAndExactKeys(t *testing.T) {
	calls := make(map[cacheKey]int)
	rows := []Namespace{cacheRow(), cacheRow()}
	rows[1].Name = "other"
	c := makeCache(t, findFunc(func(_ context.Context, mode, target string) ([]Namespace, error) {
		calls[cacheKey{mode, target}]++
		return rows, nil
	}), 10)
	for _, key := range []cacheKey{{"id", "12"}, {"name", "12"}, {"id", "012"}, {"list", ""}, {"list", "list"}} {
		for i := 0; i < 3; i++ {
			found, err := c.Find(context.Background(), key.mode, key.target)
			if err != nil || !reflect.DeepEqual(found, rows) {
				t.Fatal(found, err)
			}
		}
		if calls[key] != 1 {
			t.Fatal("repeated lookup queried source", key, calls[key])
		}
	}
	if len(calls) != 5 {
		t.Fatal("different selectors shared an entry", calls)
	}
}

func TestCacheCopiesAtBothBoundaries(t *testing.T) {
	rows := []Namespace{cacheRow()}
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) { return rows, nil }), 2)
	first, err := c.Find(context.Background(), "id", "12")
	if err != nil {
		t.Fatal(err)
	}
	first[0].Environment = "prod"
	rows[0].Name = "source-mutated"
	second, err := c.Find(context.Background(), "id", "12")
	if err != nil || second[0] != cacheRow() {
		t.Fatal(second, err)
	}
	second[0].Name = "hit-mutated"
	third, err := c.Find(context.Background(), "id", "12")
	if err != nil || third[0] != cacheRow() {
		t.Fatal(third, err)
	}
}

func TestCacheEmptyResults(t *testing.T) {
	for _, rows := range [][]Namespace{nil, {}} {
		calls := 0
		c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) { calls++; return rows, nil }), 1)
		for i := 0; i < 2; i++ {
			found, err := c.Find(context.Background(), "list", "")
			if err != nil || !reflect.DeepEqual(found, rows) {
				t.Fatal(found, err)
			}
		}
		if calls != 1 {
			t.Fatal("empty result not cached", calls)
		}
	}
}

func TestCacheDoesNotKeepErrorsOrInvalidResults(t *testing.T) {
	for _, mode := range []string{"query error", "partial", "invalid environment", "invalid account", "canceled source", "deadline source"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			sentinel := errors.New("database failed")
			c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
				calls++
				if calls > 1 {
					return []Namespace{cacheRow()}, nil
				}
				switch mode {
				case "query error":
					return nil, sentinel
				case "partial":
					return []Namespace{cacheRow()}, sentinel
				case "canceled source":
					return []Namespace{cacheRow()}, context.Canceled
				case "deadline source":
					return []Namespace{cacheRow()}, context.DeadlineExceeded
				}
				n := cacheRow()
				if mode == "invalid environment" {
					n.Environment = "production"
				} else {
					n.Account = "invalid"
				}
				return []Namespace{cacheRow(), n}, nil
			}), 2)
			found, err := c.Find(context.Background(), "id", "12")
			if err == nil || found != nil {
				t.Fatal("invalid/partial result escaped", found, err)
			}
			if (mode == "query error" || mode == "partial") && !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				found, err = c.Find(context.Background(), "id", "12")
				if err != nil || len(found) != 1 {
					t.Fatal(found, err)
				}
			}
			if calls != 2 {
				t.Fatal("cached failure or did not cache recovery", calls)
			}
		})
	}
}

func TestCacheClearAndStoreIsolation(t *testing.T) {
	calls := 0
	name := "before"
	source := findFunc(func(context.Context, string, string) ([]Namespace, error) {
		calls++
		n := cacheRow()
		n.Name = name
		return []Namespace{n}, nil
	})
	first, second := makeCache(t, source, 2), makeCache(t, source, 2)
	found, err := first.Find(context.Background(), "id", "12")
	if err != nil || found[0].Name != "before" {
		t.Fatal(found, err)
	}
	name = "after"
	found, err = first.Find(context.Background(), "id", "12")
	if err != nil || found[0].Name != "before" {
		t.Fatal("snapshot was refreshed", found, err)
	}
	found, err = second.Find(context.Background(), "id", "12")
	if err != nil || found[0].Name != "after" || calls != 2 {
		t.Fatal("cache shared across instances", found, err, calls)
	}
	first.Clear()
	found, err = first.Find(context.Background(), "id", "12")
	if err != nil || found[0].Name != "after" || calls != 3 {
		t.Fatal("invalidation failed", found, err, calls)
	}
	other := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
		n := cacheRow()
		n.Account = "999999999999"
		return []Namespace{n}, nil
	}), 2)
	found, err = other.Find(context.Background(), "id", "12")
	if err != nil || found[0].Account != "999999999999" {
		t.Fatal("cross-database result leak", found, err)
	}
}

func TestCacheEvictsLeastRecentlyUsedEntry(t *testing.T) {
	calls := make(map[string]int)
	c := makeCache(t, findFunc(func(_ context.Context, _, target string) ([]Namespace, error) {
		calls[target]++
		return []Namespace{cacheRow()}, nil
	}), 2)
	for _, key := range []string{"a", "b", "a", "c", "a", "b"} {
		if _, err := c.Find(context.Background(), "name", key); err != nil {
			t.Fatal(err)
		}
		if len(c.entries) > 2 || c.recent.Len() > 2 {
			t.Fatal("unbounded cache")
		}
	}
	if calls["a"] != 1 || calls["b"] != 2 || calls["c"] != 1 {
		t.Fatal(calls)
	}
	c.Clear()
	if len(c.entries) != 0 || c.recent.Len() != 0 {
		t.Fatal("clear retained entries")
	}
}

func TestCacheCanceledCallsDoNotPopulate(t *testing.T) {
	calls := 0
	var cancel context.CancelFunc
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
		calls++
		cancel()
		return []Namespace{cacheRow()}, nil
	}), 2)
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	found, err := c.Find(ctx, "id", "12")
	if !errors.Is(err, context.Canceled) || found != nil || len(c.entries) != 0 {
		t.Fatal(found, err)
	}
	ctx, stop = context.WithCancel(context.Background())
	stop()
	if _, err := c.Find(ctx, "id", "12"); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestCacheHitHonorsCancellation(t *testing.T) {
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) { return []Namespace{cacheRow()}, nil }), 1)
	if _, err := c.Find(context.Background(), "id", "12"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if found, err := c.Find(ctx, "id", "12"); !errors.Is(err, context.Canceled) || found != nil {
		t.Fatal(found, err)
	}
}

func TestCacheRejectsInvalidSetupAndMode(t *testing.T) {
	for _, limit := range []int{0, -1} {
		if _, err := NewCache(findFunc(nil), limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, err := NewCache(nil, 1); err == nil {
		t.Fatal("nil source accepted")
	}
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
		t.Fatal("invalid mode reached source")
		return nil, nil
	}), 1)
	if _, err := c.Find(context.Background(), "bad", "12"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

type countingQueryer struct {
	db    *sql.DB
	calls int
}

func (q *countingQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.calls++
	return q.db.QueryContext(ctx, query, args...)
}

func TestCacheAvoidsRepeatedSQLQueries(t *testing.T) {
	db := sql.OpenDB(connector{rows: [][]driver.Value{{"payments", "dev", "12", "123456789012", "Role"}}})
	defer db.Close()
	q := &countingQueryer{db: db}
	c := makeCache(t, Store{DB: q, Timeout: time.Second}, 2)
	for i := 0; i < 3; i++ {
		found, err := c.Find(context.Background(), "id", "12")
		if err != nil || len(found) != 1 {
			t.Fatal(found, err)
		}
	}
	if q.calls != 1 {
		t.Fatal("repeated database query", q.calls)
	}
}
