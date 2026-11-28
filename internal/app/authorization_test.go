package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"eks/internal/firecall"
)

type cancelActiveOutput struct {
	Output
	cancel context.CancelFunc
}

func (o cancelActiveOutput) Active([]firecall.Active) error {
	o.cancel()
	return nil
}

func TestCanceledActiveOutputCannotConnect(t *testing.T) {
	a, conn, fc, _ := fixture()
	production(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Output = cancelActiveOutput{Output: a.Output, cancel: cancel}

	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if fc.listCalls != 1 || fc.created != 0 {
		t.Fatalf("list calls = %d, creations = %d; want one initial lookup and no creation", fc.listCalls, fc.created)
	}
	if len(conn.requests) != 0 {
		t.Fatalf("connections = %d, want none after cancellation", len(conn.requests))
	}
}

func TestCanceledActiveAuthorizationCannotConnect(t *testing.T) {
	a, conn, fc, _ := fixture()
	production(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.listFn = func(context.Context, string, string, string) ([]firecall.Active, error) {
		cancel()
		return []firecall.Active{{Status: "active"}}, nil
	}

	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if fc.listCalls != 1 || fc.created != 0 {
		t.Fatalf("list calls = %d, creations = %d; want one initial lookup and no creation", fc.listCalls, fc.created)
	}
	if len(conn.requests) != 0 {
		t.Fatalf("connections = %d, want none after cancellation", len(conn.requests))
	}
}

func TestActivationBeforeConnection(t *testing.T) {
	for _, mode := range []string{"immediate", "delayed", "timeout", "cancel", "api error", "inflight timeout", "create error"} {
		t.Run(mode, func(t *testing.T) {
			a, conn, fc, _ := fixture()
			production(a)
			fc.active, fc.noActivation = nil, true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waits := 0
			a.Wait = func(ctx context.Context, delay time.Duration) error {
				waits++
				if delay != 5*time.Second {
					t.Fatalf("unexpected polling delay %s", delay)
				}
				switch mode {
				case "cancel":
					cancel()
					return ctx.Err()
				case "timeout":
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			if mode == "timeout" || mode == "inflight timeout" {
				a.Config.FirecallActivationTimeout = 10 * time.Millisecond
			}
			fc.listFn = func(ctx context.Context, token, account, role string) ([]firecall.Active, error) {
				if token != "token" || account != "123456789012" || role != "arn:aws:iam::123456789012:role/Role" {
					t.Fatal("incorrect authorization selector", account, role)
				}
				if len(conn.requests) != 0 {
					t.Fatal("connected before activation")
				}
				if fc.listCalls == 1 {
					if mode == "create error" {
						fc.err = errors.New("creation failed")
					}
					return nil, nil
				}
				if fc.created != 1 {
					t.Fatal("activation checked without exactly one creation")
				}
				switch mode {
				case "immediate":
					return []firecall.Active{{Status: "active"}}, nil
				case "delayed":
					if waits == 2 {
						return []firecall.Active{{Status: "active"}}, nil
					}
				case "api error":
					return nil, errors.New("lookup failed")
				case "inflight timeout":
					<-ctx.Done()
					// Even a late successful response must not authorize a connection.
					return []firecall.Active{{Status: "active"}}, nil
				}
				return nil, nil
			}
			err := a.Run(ctx)
			success := mode == "immediate" || mode == "delayed"
			if (err == nil) != success || fc.created != 1 {
				t.Fatalf("err=%v creations=%d", err, fc.created)
			}
			want := 0
			if success {
				want = 1
			}
			if len(conn.requests) != want {
				t.Fatalf("connections=%d want=%d", len(conn.requests), want)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if (mode == "timeout" || mode == "inflight timeout") && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "immediate" && waits != 0 {
				t.Fatal("first activation check was delayed")
			}
			if mode == "delayed" && (waits != 2 || fc.listCalls != 4) {
				t.Fatalf("waits=%d list calls=%d", waits, fc.listCalls)
			}
			if mode == "api error" && fc.listCalls != 2 {
				t.Fatal("retried failed activation check")
			}
			if mode == "create error" && fc.listCalls != 1 {
				t.Fatal("polled after unsuccessful creation")
			}
		})
	}
}

func TestEnvironmentGating(t *testing.T) {
	for _, env := range []string{"dev", "sit", "uat", "prod", "production", "PROD", " prod", "prod ", "qa", ""} {
		t.Run(env, func(t *testing.T) {
			a, conn, fc, _ := fixture()
			s := a.Store.(fakeStore)
			s.items[0].Environment = env
			a.Store = s
			err := a.Run(context.Background())
			valid := env == "dev" || env == "sit" || env == "uat" || env == "prod"
			if (err == nil) != valid {
				t.Fatal(err)
			}
			if !valid && len(conn.requests) != 0 {
				t.Fatal("unknown environment connected")
			}
			if (fc.tokenUser != "") != (env == "prod") {
				t.Fatal("incorrect production gating")
			}
			if valid && len(conn.requests) != 1 {
				t.Fatal("valid environment did not connect")
			}
		})
	}
}
