package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"eks/internal/config"
	"eks/internal/firecall"
	"eks/internal/kconnect"
	"eks/internal/namespace"
)

type fakeStore struct {
	items []namespace.Namespace
	err   error
}

func (f fakeStore) Find(context.Context, string, string) ([]namespace.Namespace, error) {
	return f.items, f.err
}

type fakeConnect struct {
	requests []kconnect.Request
	err      error
}

func (f *fakeConnect) Connect(_ context.Context, r kconnect.Request) error {
	f.requests = append(f.requests, r)
	return f.err
}

type fakeFirecall struct {
	active                          []firecall.Active
	tokenUser, tokenPassword, email string
	created                         int
	err                             error
	listCalls                       int
	listFn                          func(context.Context, string, string, string) ([]firecall.Active, error)
	noActivation                    bool
}

func (f *fakeFirecall) Token(_ context.Context, u, p string) (string, error) {
	f.tokenUser, f.tokenPassword = u, p
	return "token", f.err
}
func (f *fakeFirecall) List(ctx context.Context, token, account, role string) ([]firecall.Active, error) {
	f.listCalls++
	if f.listFn != nil {
		return f.listFn(ctx, token, account, role)
	}
	return f.active, f.err
}
func (f *fakeFirecall) Create(_ context.Context, _, _, email, _ string, _ int) ([]firecall.Created, error) {
	f.created++
	f.email = email
	if !f.noActivation {
		f.active = []firecall.Active{{Status: "active"}}
	}
	return []firecall.Created{{ID: "42", Status: "SUCCESS"}}, f.err
}

type fakeUI struct {
	yes      bool
	index    int
	calls    int
	password string
	err      error
}

func (f *fakeUI) Choose(context.Context, []namespace.Namespace) (int, error) {
	f.calls++
	return f.index, f.err
}
func (f *fakeUI) Password(context.Context) (string, error) { f.calls++; return f.password, f.err }
func (f *fakeUI) Confirm(context.Context) (bool, error)    { f.calls++; return f.yes, f.err }
func (f *fakeUI) Ticket(context.Context) (string, error)   { f.calls++; return "CHG1", f.err }

func fixture() (*App, *fakeConnect, *fakeFirecall, *fakeUI) {
	conn := &fakeConnect{}
	fc := &fakeFirecall{active: []firecall.Active{{Status: "active"}}}
	ui := &fakeUI{yes: true, password: "production-only"}
	a := &App{
		Config: config.Config{Target: "12", Mode: "id", Once: true, Digest: "sha256", Passphrase: "fixture-only",
			FirecallPollInterval: 5 * time.Second, FirecallActivationTimeout: 2 * time.Minute,
			EncryptedUser:     "U2FsdGVkX18BAgMEBQYHCE7cMDd4cyD+typtkEMxGBQ=",
			EncryptedPassword: "U2FsdGVkX18BAgMEBQYHCE7cMDd4cyD+typtkEMxGBQ=",
			EmailDomain:       "test.com", OAuthURL: "https://idp.invalid/oauth", FirecallListURL: "https://fc.invalid/list", FirecallCreateURL: "https://fc.invalid", Duration: 8},
		Store:     fakeStore{items: []namespace.Namespace{{Name: "payments", Environment: "dev", ID: "12", Account: "123456789012", Role: "Role"}}},
		Connector: conn, Firecalls: fc, UI: ui, Output: Printer{Writer: io.Discard},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return a, conn, fc, ui
}
func production(a *App) {
	n := a.Store.(fakeStore)
	n.items[0].Environment = "prod"
	a.Store = n
}
func TestWorkflow(t *testing.T) {
	for _, tt := range []struct {
		name                         string
		prod, active, yes            bool
		wantConnections, wantCreated int
		bad                          bool
	}{
		{"nonprod", false, false, false, 1, 0, false},
		{"active production", true, true, false, 1, 0, false},
		{"create production", true, false, true, 1, 1, false},
		{"decline production", true, false, false, 0, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, conn, fc, ui := fixture()
			if tt.prod {
				production(a)
			}
			if !tt.active {
				fc.active = nil
			}
			ui.yes = tt.yes
			err := a.Run(context.Background())
			if (err != nil) != tt.bad || len(conn.requests) != tt.wantConnections || fc.created != tt.wantCreated {
				t.Fatal(err, len(conn.requests), fc.created)
			}
			if tt.prod && tt.wantConnections > 0 {
				r := conn.requests[0]
				if r.Username != "ssample-user" || r.Password != "production-only" || fc.tokenUser != r.Username || fc.tokenPassword != r.Password {
					t.Fatal(r, fc)
				}
			}
			if fc.created > 0 && fc.email != "sample-user@test.com" {
				t.Fatal("email must use base identity", fc.email)
			}
			if !tt.prod && fc.tokenUser != "" {
				t.Fatal("nonprod contacted OAuth")
			}
		})
	}
}

func TestWorkflowErrors(t *testing.T) {
	for _, name := range []string{"missing", "query", "ambiguous json", "invalid choice", "prompt EOF", "decrypt", "connect", "oauth", "production config", "json password", "json create", "empty password"} {
		t.Run(name, func(t *testing.T) {
			a, conn, fc, ui := fixture()
			switch name {
			case "missing":
				a.Store = fakeStore{}
			case "query":
				a.Store = fakeStore{err: errors.New("query failed")}
			case "ambiguous json", "invalid choice", "prompt EOF":
				s := a.Store.(fakeStore)
				s.items = append(s.items, s.items[0])
				a.Store = s
				if name == "ambiguous json" {
					a.Config.JSON = true
				} else if name == "invalid choice" {
					ui.index = 99
				} else {
					ui.err = io.EOF
				}
			case "decrypt":
				a.Config.EncryptedPassword = "invalid"
			case "connect":
				conn.err = errors.New("connection failed")
			case "oauth":
				production(a)
				fc.err = errors.New("OAuth failed")
			case "production config":
				production(a)
				a.Config.OAuthURL = ""
			case "json password":
				production(a)
				a.Config.JSON = true
			case "json create":
				production(a)
				a.Config.JSON = true
				a.Config.ProductionPassword = "prod-test"
				fc.active = nil
			case "empty password":
				production(a)
				ui.password = ""
			}
			if err := a.Run(context.Background()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRefreshAndCancellation(t *testing.T) {
	a, conn, _, _ := fixture()
	a.Config.Once = false
	waits := 0
	a.Wait = func(context.Context, time.Duration) error {
		waits++
		if waits == 2 {
			return context.Canceled
		}
		return nil
	}
	if err := a.Run(context.Background()); !errors.Is(err, context.Canceled) || len(conn.requests) != 2 {
		t.Fatal(err, len(conn.requests))
	}
	a, _, fc, _ := fixture()
	production(a)
	a.Config.Once = false
	fc.active = nil
	a.Wait = func(context.Context, time.Duration) error {
		fc.active = []firecall.Active{{Status: "active"}}
		if len(fc.tokenUser) > 0 && fc.created > 1 {
			t.Fatal("recreated firecall")
		}
		return context.Canceled
	}
	if err := a.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if fc.created != 1 {
		t.Fatal(fc.created)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := Wait(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}

func TestSelectionAndList(t *testing.T) {
	a, conn, _, ui := fixture()
	s := a.Store.(fakeStore)
	second := s.items[0]
	second.Name = "other"
	s.items = append(s.items, second)
	a.Store = s
	ui.index = 1
	if err := a.Run(context.Background()); err != nil || conn.requests[0].Namespace != "other" {
		t.Fatal(err)
	}
	a, conn, _, _ = fixture()
	a.Config.List = true
	if err := a.Run(context.Background()); err != nil || len(conn.requests) != 0 {
		t.Fatal(err)
	}
}

func TestOutputGolden(t *testing.T) {
	var out bytes.Buffer
	p := Printer{Writer: &out}
	n := namespace.Namespace{Name: "payments", Environment: "prod", ID: "12", Account: "123456789012", Role: "Role"}
	if err := p.Details(n); err != nil {
		t.Fatal(err)
	}
	want := "\nNAMESPACE DETAILS:\n-----------------------------------\nNAME:      payments\nACCOUNT:   123456789012\nROLE:      Role\nENV:       prod\n-----------------------------------\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	out.Reset()
	p.JSON = true
	if err := p.List([]namespace.Namespace{n}); err != nil {
		t.Fatal(err)
	}
	want = `{"event":"namespaces","data":[{"name":"payments","environment":"prod","id":"12","account":"123456789012","role":"Role"}]}` + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	if clean("bad\x1b[31m\n") != "bad [31m " {
		t.Fatal("terminal control characters retained")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestOutputErrors(t *testing.T) {
	n := namespace.Namespace{Name: "x"}
	p := Printer{Writer: brokenWriter{}}
	for _, err := range []error{p.List([]namespace.Namespace{n}), p.Details(n), p.Active([]firecall.Active{{}}), p.Created([]firecall.Created{{}}), p.Connected(n, time.Now())} {
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	p = Printer{Writer: &out}
	if err := p.List([]namespace.Namespace{{Name: "x", Environment: "dev", ID: "1", Account: "123", Role: "r"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "NAME") {
		t.Fatal(out.String())
	}
}
