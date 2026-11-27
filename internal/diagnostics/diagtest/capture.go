// Package diagtest validates synthetic diagnostics in boundary tests.
package diagtest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"eks/internal/diagnostics"
)

type Capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
	T   *testing.T
	D   *diagnostics.Diagnostics
}

func (c *Capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *Capture) Text() string { c.mu.Lock(); defer c.mu.Unlock(); return c.buf.String() }
func New(t *testing.T) (context.Context, *Capture) {
	t.Helper()
	c := &Capture{T: t}
	c.D = &diagnostics.Diagnostics{Log: diagnostics.NewLogger(c, "json", true, true, "test")}
	return diagnostics.Cycle(diagnostics.With(context.Background(), c.D)), c
}
func (c *Capture) Records() []map[string]any {
	c.T.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.Text()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			c.T.Fatal(err, line)
		}
		records = append(records, r)
	}
	return records
}
func (c *Capture) Event(name string) map[string]any {
	c.T.Helper()
	for _, r := range c.Records() {
		if r["event"] == name {
			return r
		}
	}
	c.T.Fatalf("missing event %s in %s", name, c.Text())
	return nil
}
func (c *Capture) NoSecrets(secrets ...string) {
	c.T.Helper()
	for _, s := range secrets {
		if strings.Contains(c.Text(), s) {
			c.T.Fatalf("diagnostic leaked canary %q", s)
		}
	}
}
func (c *Capture) Contract() {
	c.T.Helper()
	records := c.Records()
	if len(records) == 0 {
		c.T.Fatal("no diagnostic records")
	}
	run := records[0]["run_id"]
	known := map[any]bool{"": true}
	for _, r := range records {
		if strings.HasSuffix(r["event"].(string), ".start") {
			known[r["operation_id"]] = true
		}
	}
	primary := map[any]bool{}
	for _, r := range records {
		for _, k := range []string{"time", "level", "msg", "event", "component", "operation", "run_id", "operation_id", "parent_operation_id", "cycle_id", "version"} {
			if _, ok := r[k].(string); !ok {
				c.T.Fatalf("missing or mistyped %s: %v", k, r)
			}
		}
		if r["schema_version"] != float64(1) || r["run_id"] != run {
			c.T.Fatal("schema/run mismatch", r)
		}
		if _, ok := r["pid"].(float64); !ok {
			c.T.Fatal("pid type", r)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, r["time"].(string)); err != nil || parsed.Location() != time.UTC {
			c.T.Fatal("non-UTC time", r)
		}
		if !known[r["parent_operation_id"]] {
			c.T.Fatal("unknown parent", r)
		}
		if strings.HasSuffix(r["event"].(string), ".outcome") {
			if n, ok := r["duration_ms"].(float64); !ok || n < 0 {
				c.T.Fatal("invalid duration", r)
			}
			switch r["status"] {
			case "success", "failure", "timeout", "canceled":
			default:
				c.T.Fatal("invalid status", r)
			}
		}
		if r["level"] == slog.LevelError.String() && r["operation"] != "run" {
			if primary[r["operation_id"]] {
				c.T.Fatal("duplicate primary failure", r)
			}
			primary[r["operation_id"]] = true
			if r["error_code"] == nil || r["error_category"] == nil {
				c.T.Fatal("missing failure classification", r)
			}
		}
		if source, ok := r["source"].(map[string]any); ok {
			file := source["file"].(string)
			if strings.HasPrefix(file, "/") || strings.Contains(file, "\\") {
				c.T.Fatal("unsafe source", r)
			}
		}
	}
}
