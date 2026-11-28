package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"eks/internal/diagnostics"
	"eks/internal/diagnostics/diagtest"
	"eks/internal/kconnect"
	"eks/internal/namespace"
)

type scopedFinder struct {
	Finder
	cause error
}

func (f scopedFinder) Find(ctx context.Context, mode, target string) ([]namespace.Namespace, error) {
	_, query := diagnostics.Start(ctx, "database", "query")
	if f.cause != nil {
		return nil, query.End(f.cause)
	}
	items, err := f.Finder.Find(ctx, mode, target)
	return items, query.End(err)
}

type scopedConnector struct{ cause error }

func (c scopedConnector) Connect(ctx context.Context, _ kconnect.Request) error {
	_, child := diagnostics.Start(ctx, "subprocess", "subprocess")
	return child.End(c.cause)
}

func TestDirectCallsPreserveNestedFailureOwnership(t *testing.T) {
	for _, boundary := range []string{"selection", "connection"} {
		t.Run(boundary, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			a, _, _, _ := fixture()
			cause := errors.New("CANARY nested failure")
			owner, parent := "query", "selection"
			if boundary == "selection" {
				a.Store = scopedFinder{Finder: a.Store, cause: cause}
			} else {
				a.Connector = scopedConnector{cause: cause}
				owner, parent = "subprocess", "connection"
			}
			err := a.Run(ctx)
			if !errors.Is(err, cause) {
				t.Fatal("lost original cause", err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY")
			child := logs.Event(owner + ".outcome")
			call := logs.Event(parent + ".outcome")
			if child["parent_operation_id"] != call["operation_id"] ||
				child["cycle_id"] != call["cycle_id"] {
				t.Fatal("nested boundary lost its operation context", logs.Text())
			}
			for _, event := range []string{parent + ".outcome", "workflow.outcome"} {
				record := logs.Event(event)
				if record["failed_operation_id"] != child["operation_id"] ||
					record["error_code"] != owner+"_failed" || record["level"] == "ERROR" {
					t.Fatal("parent took ownership of nested failure", record)
				}
			}
		})
	}
}

func TestProductionRefreshRechecksAuthorizationInNewCycle(t *testing.T) {
	ctx, logs := diagtest.New(t)
	a, conn, fc, ui := fixture()
	production(a)
	a.Config.Once = false
	waits := 0
	a.Wait = func(context.Context, time.Duration) error {
		waits++
		// Access was revoked after the first successful connection. The next
		// cycle must ask the service again rather than reuse authorization.
		fc.active = nil
		ui.yes = false
		return nil
	}
	err := a.Run(ctx)
	if err == nil || diagnostics.Category(err) != "declined" ||
		waits != 1 || len(conn.requests) != 1 || fc.listCalls != 2 || fc.created != 0 {
		t.Fatal("refresh reused authorization", err, waits, len(conn.requests), fc.listCalls, fc.created)
	}
	logs.Contract()
	logs.NoSecrets("sample-user", "production-only")
	var refreshes, tokens, lists []map[string]any
	for _, record := range logs.Records() {
		switch record["event"] {
		case "refresh.start":
			refreshes = append(refreshes, record)
		case "oauth_token.start":
			tokens = append(tokens, record)
		case "list_firecalls.start":
			lists = append(lists, record)
		}
	}
	if len(refreshes) != 2 || len(tokens) != 2 || len(lists) != 2 {
		t.Fatal("missing repeated refresh steps", logs.Text())
	}
	if refreshes[0]["cycle_id"] == refreshes[1]["cycle_id"] {
		t.Fatal("refresh reused cycle ID")
	}
	for i, refresh := range refreshes {
		if tokens[i]["cycle_id"] != refresh["cycle_id"] || lists[i]["cycle_id"] != refresh["cycle_id"] {
			t.Fatal("authorization detached from refresh cycle", logs.Text())
		}
	}
}
