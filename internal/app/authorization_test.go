package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"eks/internal/firecall"
	"eks/internal/namespace"
)

func TestCanceledTokenAuthorizationCannotConnect(t *testing.T) {
	a, conn, fc, _ := fixture()
	production(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tokenCalls := 0
	fc.tokenFn = func(context.Context, string, string) (string, error) {
		tokenCalls++
		cancel()
		return "token", nil
	}

	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d, want one", tokenCalls)
	}
	if len(conn.requests) != 0 {
		t.Fatalf("connections = %d, want none after cancellation", len(conn.requests))
	}
}

type cancelDetailsOutput struct {
	Output
	cancel context.CancelFunc
}

func (o cancelDetailsOutput) Details(namespace.Namespace) error {
	o.cancel()
	return nil
}

func TestCanceledDetailsOutputCannotAuthorize(t *testing.T) {
	a, conn, fc, ui := fixture()
	production(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Output = cancelDetailsOutput{Output: a.Output, cancel: cancel}

	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if fc.tokenUser != "" || fc.tokenPassword != "" {
		t.Fatal("requested an OAuth token after cancellation")
	}
	if fc.listCalls != 0 || fc.created != 0 {
		t.Fatalf("list calls = %d, creations = %d; want none after cancellation", fc.listCalls, fc.created)
	}
	if ui.calls != 0 {
		t.Fatalf("prompts = %v, want none after cancellation", ui.prompts)
	}
	if len(conn.requests) != 0 {
		t.Fatalf("connections = %d, want none after cancellation", len(conn.requests))
	}
}

type cancelActiveOutput struct {
	Output
	cancel context.CancelFunc
}

func (o cancelActiveOutput) Active([]firecall.Active) error {
	o.cancel()
	return nil
}

type failingActiveOutput struct {
	Output
	err   error
	calls int
}

func (o *failingActiveOutput) Active([]firecall.Active) error {
	o.calls++
	return o.err
}

func TestFailedActiveOutputCannotConnect(t *testing.T) {
	a, conn, fc, ui := fixture()
	production(a)
	displayErr := errors.New("active access display failed")
	output := &failingActiveOutput{Output: a.Output, err: displayErr}
	a.Output = output
	ctx := context.Background()

	err := a.Run(ctx)
	if !errors.Is(err, displayErr) {
		t.Fatalf("Run error = %v, want active access display error", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("context error = %v, want live context", ctx.Err())
	}
	if output.calls != 1 {
		t.Errorf("active display calls = %d, want one", output.calls)
	}
	if fc.listCalls != 1 || fc.created != 0 {
		t.Errorf("list calls = %d, creations = %d; want one initial lookup and no creation", fc.listCalls, fc.created)
	}
	for _, prompt := range ui.prompts {
		if prompt == "confirm" || prompt == "ticket" {
			t.Errorf("unexpected %s prompt after active access display failure", prompt)
		}
	}
	if len(conn.requests) != 0 {
		t.Errorf("connections = %d, want none after active access display failure", len(conn.requests))
	}
}

type cancelCreatedOutput struct {
	Output
	cancel context.CancelFunc
}

func (o cancelCreatedOutput) Created([]firecall.Created) error {
	o.cancel()
	return nil
}

func TestCanceledCreatedOutputStopsActivation(t *testing.T) {
	a, conn, fc, ui := fixture()
	production(a)
	fc.active = nil
	ui.yes = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Output = cancelCreatedOutput{Output: a.Output, cancel: cancel}

	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if fc.created != 1 {
		t.Fatalf("creations = %d, want exactly one", fc.created)
	}
	if fc.listCalls != 1 {
		t.Fatalf("list calls = %d, want only the initial lookup and no activation lookup", fc.listCalls)
	}
	if len(conn.requests) != 0 {
		t.Fatalf("connections = %d, want none after cancellation", len(conn.requests))
	}
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
