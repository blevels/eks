package diagnostics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"eks/internal/diagnostics"
	"eks/internal/diagnostics/diagtest"
)

func TestSchemaOwnershipAndCancellation(t *testing.T) {
	for _, cause := range []error{nil, errors.New("CANARY\x1b\n/home/private"), context.Canceled, context.DeadlineExceeded} {
		ctx, logs := diagtest.New(t)
		ctx, run := diagnostics.Start(ctx, "cli", "run")
		ctx, parent := diagnostics.Start(ctx, "workflow", "refresh")
		_, child := diagnostics.Start(ctx, "database", "query")
		err := child.End(cause, diagnostics.N("row_count", 3), diagnostics.N("secret", 99))
		err = parent.End(err)
		_ = run.End(err, diagnostics.N("exit_code", 1))
		if cause != nil && !errors.Is(err, cause) {
			t.Fatal("lost identity")
		}
		if strings.Contains(fmt.Sprintf("%+v", err), "CANARY") {
			t.Fatal("unsafe error chain")
		}
		logs.Contract()
		logs.NoSecrets("CANARY", "/home/private", `"secret":`)
		outcome := logs.Event("query.outcome")
		if outcome["parent_operation_id"] != logs.Event("refresh.start")["operation_id"] {
			t.Fatal(outcome)
		}
		if cause == context.Canceled && outcome["level"] != "INFO" {
			t.Fatal(outcome)
		}
		if cause == context.DeadlineExceeded && outcome["status"] != "timeout" {
			t.Fatal(outcome)
		}
		if cause != nil && logs.Event("run.outcome")["failed_operation_id"] != outcome["operation_id"] {
			t.Fatal("lost owner")
		}
	}
}

func TestDurationUsesMilliseconds(t *testing.T) {
	ctx, logs := diagtest.New(t)
	_, scope := diagnostics.Start(ctx, "database", "query")
	time.Sleep(15 * time.Millisecond)
	_ = scope.End(nil)
	duration := logs.Event("query.outcome")["duration_ms"].(float64)
	if duration < 10 || duration > 10000 {
		t.Fatal("duration not milliseconds", duration)
	}
}

func TestCleanupDoesNotReclassifyPrimaryCause(t *testing.T) {
	ctx, logs := diagtest.New(t)
	ctx, parent := diagnostics.Start(ctx, "database", "query")
	_, cleanup := diagnostics.Start(ctx, "database", "rows_close")
	extra := cleanup.End(context.Canceled)
	_ = parent.End(errors.Join(errors.New("CANARY primary"), extra))
	r := logs.Event("query.outcome")
	if r["status"] != "failure" || r["error_category"] != "database" {
		t.Fatal(r)
	}
	logs.NoSecrets("CANARY")
}

func TestSourceCallerAndHostileMetadata(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		var b bytes.Buffer
		ctx := diagnostics.With(context.Background(), &diagnostics.Diagnostics{Log: diagnostics.NewLogger(&b, format, true, true, "CANARY\x1b")})
		_, scope := diagnostics.Start(ctx, "database", "query")
		_ = scope.End(errors.New("CANARY/secret\n"))
		if strings.Contains(b.String(), "CANARY") || strings.Contains(b.String(), "/home/") {
			t.Fatal(b.String())
		}
		if !strings.Contains(b.String(), "internal/diagnostics/context_test.go") {
			t.Fatal("source points to helper", b.String())
		}
		if format == "json" {
			var r map[string]any
			if err := json.Unmarshal(bytes.Split(b.Bytes(), []byte("\n"))[1], &r); err != nil {
				t.Fatal(err)
			}
			if r["source"].(map[string]any)["file"] != "internal/diagnostics/context_test.go" {
				t.Fatal(r)
			}
		}
	}
}

func TestSourceMetadataRemovesHomesAndFunctions(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, path := range []string{"/home/CANARY/private.go", `C:\Users\CANARY\repo\internal\database\store.go`} {
			var b bytes.Buffer
			log := diagnostics.NewLogger(&b, format, true, true, "test").With("source", &slog.Source{File: path, Function: "CANARY", Line: 1})
			log.Info("Application diagnostic")
			if strings.Contains(b.String(), "CANARY") || strings.Contains(b.String(), "/home/") || strings.Contains(b.String(), `C:\`) {
				t.Fatal(b.String())
			}
		}
	}
}

func TestAllowlistsAndLevelRules(t *testing.T) {
	var b bytes.Buffer
	ctx := diagnostics.With(context.Background(), &diagnostics.Diagnostics{Log: diagnostics.NewLogger(&b, "json", false, false, "test")})
	_, scope := diagnostics.Start(ctx, "CANARY", "CANARY")
	scope.Event("CANARY", slog.LevelError)
	_ = scope.End(nil)
	if b.Len() != 0 {
		t.Fatal("debug/unknown event logged", b.String())
	}
	_, run := diagnostics.Start(ctx, "cli", "run")
	_ = run.End(diagnostics.Declined())
	if strings.Contains(b.String(), "ERROR") || strings.Contains(b.String(), "CANARY") || !strings.Contains(b.String(), `"status":"canceled"`) {
		t.Fatal(b.String())
	}
}
