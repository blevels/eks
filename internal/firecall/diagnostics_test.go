package firecall

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"eks/internal/diagnostics/diagtest"
)

func TestHTTPCommonSchemaOutcomes(t *testing.T) {
	for _, scenario := range []string{"success", "retries", "terminal_status", "semantic_token", "semantic_create", "budget", "wait_cancel", "close", "close_and_status", "invalid_json", "transport", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			c := New(5*time.Second, "https://CANARY.invalid", "https://CANARY.invalid", "https://CANARY.invalid")
			noDelay(c)
			calls := 0
			c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				status, body := 200, "[]"
				header := http.Header{}
				var closeErr error
				switch scenario {
				case "retries":
					if calls < 3 {
						return nil, fmt.Errorf("CANARY/private: %w", syscall.ECONNRESET)
					}
				case "terminal_status":
					status = 403
				case "semantic_token":
					body = `{"access_token":""}`
				case "semantic_create":
					body = `[{"status":"CANARY","message":"CANARY"}]`
				case "budget":
					status = 503
					header.Set("Retry-After", "10000")
				case "wait_cancel":
					status = 503
				case "close":
					closeErr = errors.New("CANARY close")
				case "close_and_status":
					status = 403
					closeErr = errors.New("CANARY close")
				case "invalid_json":
					body = "CANARY invalid json\x1b"
				case "transport":
					return nil, errors.New("CANARY transport")
				case "deadline":
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: status, Header: header, Body: &responseBody{reader: strings.NewReader(body), closeErr: closeErr, closed: new(int)}}, nil
			})
			if scenario == "wait_cancel" {
				c.wait = func(context.Context, time.Duration) error { return context.Canceled }
			}
			var err error
			switch scenario {
			case "semantic_token":
				_, err = c.Token(ctx, "CANARY user", "CANARY pass")
			case "semantic_create":
				_, err = c.Create(ctx, "CANARY token", "CANARY arn", "CANARY email", "CANARY ticket", 8)
			default:
				_, err = c.List(ctx, "CANARY token", "CANARY account", "CANARY role")
			}
			if (err == nil) != (scenario == "success" || scenario == "retries") {
				t.Fatal(err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "https://", "Bearer", "Authorization")
			if scenario == "budget" {
				logs.Event("http.retry_budget_rejected")
				if calls != 1 {
					t.Fatal(calls)
				}
			}
			if scenario == "wait_cancel" {
				if !errors.Is(err, context.Canceled) || logs.Event("retry_wait.outcome")["status"] != "canceled" {
					t.Fatal(err, logs.Text())
				}
			}
			if scenario == "retries" {
				warns := 0
				for _, r := range logs.Records() {
					if r["event"] == "http_attempt.outcome" && r["level"] == "WARN" {
						warns++
					}
				}
				if warns != 2 {
					t.Fatal(logs.Text())
				}
			}
			if scenario == "close_and_status" {
				failures := 0
				for _, r := range logs.Records() {
					if r["level"] == "ERROR" {
						failures++
					}
				}
				if failures != 2 {
					t.Fatal("primary and supplemental failure not independently visible", logs.Text())
				}
			}
			if err != nil {
				if strings.Contains(fmt.Sprintf("%+v", err), "CANARY") {
					t.Fatal(err)
				}
			}
		})
	}
}
