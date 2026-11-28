package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"eks/internal/diagnostics/diagtest"
	"eks/internal/firecall"
)

// Exercise the real HTTP boundary and orchestration together, using only local
// TLS endpoints. Failure/retry handling must never authorize a connection.
func TestProductionHTTPRetrySafety(t *testing.T) {
	for _, mode := range []string{"initial lookup exhausted", "activation exhausted", "activation deadline", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			a, conn, _, _ := fixture()
			production(a)
			var lists, creates atomic.Int32
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth":
					fmt.Fprint(w, `{"access_token":"local-test-token"}`)
				case "/create":
					creates.Add(1)
					fmt.Fprint(w, `[{"firecall_id":"42","status":"SUCCESS"}]`)
				case "/list":
					call := lists.Add(1)
					if mode == "initial lookup exhausted" {
						w.WriteHeader(http.StatusServiceUnavailable)
					} else if call == 1 {
						fmt.Fprint(w, "[]")
					} else if mode == "recovery" && call == 3 {
						fmt.Fprint(w, `[{"account_id":"123456789012","iamRoleArn":"arn:aws:iam::123456789012:role/Role","firecall_status":"active"}]`)
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			client := firecall.New(5*time.Second, s.URL+"/oauth", s.URL+"/list", s.URL+"/create")
			client.HTTP = s.Client()
			a.Firecalls = client
			a.Config.FirecallActivationTimeout = 5 * time.Second
			if mode == "activation deadline" {
				a.Config.FirecallActivationTimeout = 20 * time.Millisecond
			}
			ctx, logs := diagtest.New(t)
			err := a.Run(ctx)
			logs.Contract()
			logs.NoSecrets("sample-user", "production-only", "local-test-token")
			token := logs.Event("oauth_token.start")
			authorization := logs.Event("authorization.start")
			if token["parent_operation_id"] != authorization["operation_id"] {
				t.Fatal("token operation lost authorization parent", logs.Text())
			}
			// The real HTTP client must receive the operation context, not its parent:
			// attempts belong to a token/list/create call even when retries fail.
			operations := make(map[any]map[string]any)
			for _, record := range logs.Records() {
				if record["event"] == "oauth_token.start" || record["event"] == "list_firecalls.start" || record["event"] == "create_firecall.start" {
					operations[record["operation_id"]] = record
				}
				if record["event"] == "http_attempt.start" {
					parent := operations[record["parent_operation_id"]]
					if parent == nil || parent["cycle_id"] != record["cycle_id"] {
						t.Fatal("HTTP attempt lost operation context", record)
					}
				}
			}
			if mode == "recovery" {
				if err != nil || len(conn.requests) != 1 || creates.Load() != 1 || lists.Load() != 3 {
					t.Fatal(err, len(conn.requests), creates.Load(), lists.Load())
				}
				return
			}
			if err == nil || len(conn.requests) != 0 {
				t.Fatal("failed HTTP authorization allowed connection", err, len(conn.requests))
			}
			wantCreates, wantLists := int32(1), int32(4)
			if mode == "initial lookup exhausted" {
				wantCreates, wantLists = 0, 3
			}
			if mode == "activation deadline" {
				wantLists = 2
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("activation deadline lost", err)
				}
			}
			if creates.Load() != wantCreates || lists.Load() != wantLists {
				t.Fatal("unexpected creation or retry count", creates.Load(), lists.Load())
			}
		})
	}
}
