package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
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
	active                                  []firecall.Active
	tokenUser, tokenPassword, email, ticket string
	created                                 int
	err                                     error
	listCalls                               int
	tokenFn                                 func(context.Context, string, string) (string, error)
	listFn                                  func(context.Context, string, string, string) ([]firecall.Active, error)
	noActivation                            bool
}

func (f *fakeFirecall) Token(ctx context.Context, u, p string) (string, error) {
	f.tokenUser, f.tokenPassword = u, p
	if f.tokenFn != nil {
		return f.tokenFn(ctx, u, p)
	}
	return "token", f.err
}
func (f *fakeFirecall) List(ctx context.Context, token, account, role string) ([]firecall.Active, error) {
	f.listCalls++
	if f.listFn != nil {
		return f.listFn(ctx, token, account, role)
	}
	return f.active, f.err
}
func (f *fakeFirecall) Create(_ context.Context, _, _, email, ticket string, _ int) ([]firecall.Created, error) {
	f.created++
	f.email = email
	f.ticket = ticket
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
	prompts  []string
}

func (f *fakeUI) Choose(context.Context, []namespace.Namespace) (int, error) {
	f.calls++
	f.prompts = append(f.prompts, "choose")
	return f.index, f.err
}
func (f *fakeUI) Password(context.Context) (string, error) {
	f.calls++
	f.prompts = append(f.prompts, "password")
	return f.password, f.err
}
func (f *fakeUI) Confirm(context.Context) (bool, error) {
	f.calls++
	f.prompts = append(f.prompts, "confirm")
	return f.yes, f.err
}
func (f *fakeUI) Ticket(context.Context) (string, error) {
	f.calls++
	f.prompts = append(f.prompts, "ticket")
	return "CHG1", f.err
}

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

func TestSuppliedProductionDetails(t *testing.T) {
	for _, tt := range []struct {
		name, password, ticket   string
		wantPassword, wantTicket string
		wantPrompts              []string
	}{
		{
			name:         "neither supplied",
			wantPassword: "production-only", wantTicket: "CHG1",
			wantPrompts: []string{"choose", "password", "confirm", "ticket"},
		},
		{
			name: "password supplied", password: "configured-production-password",
			wantPassword: "configured-production-password", wantTicket: "CHG1",
			wantPrompts: []string{"choose", "confirm", "ticket"},
		},
		{
			name: "ticket supplied", ticket: "CHG-configured",
			wantPassword: "production-only", wantTicket: "CHG-configured",
			wantPrompts: []string{"choose", "password", "confirm"},
		},
		{
			name: "both supplied", password: "configured-production-password", ticket: "CHG-configured",
			wantPassword: "configured-production-password", wantTicket: "CHG-configured",
			wantPrompts: []string{"choose", "confirm"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, decision := range []struct {
				name    string
				confirm bool
			}{
				{"confirm creation", true},
				{"decline creation", false},
			} {
				t.Run(decision.name, func(t *testing.T) {
					a, conn, fc, ui := fixture()
					production(a)
					// Keep namespace selection interactive even when details are supplied.
					store := a.Store.(fakeStore)
					store.items = append(store.items, store.items[0])
					a.Store = store
					a.Config.ProductionPassword = tt.password
					a.Config.Ticket = tt.ticket
					fc.active = nil
					ui.yes = decision.confirm

					err := a.Run(context.Background())
					if (err != nil) != !decision.confirm {
						t.Fatalf("Run error = %v, confirmed = %v", err, decision.confirm)
					}
					wantPrompts := tt.wantPrompts
					if !decision.confirm && wantPrompts[len(wantPrompts)-1] == "ticket" {
						wantPrompts = wantPrompts[:len(wantPrompts)-1]
					}
					if !slices.Equal(ui.prompts, wantPrompts) {
						t.Fatalf("prompts = %v, want %v", ui.prompts, wantPrompts)
					}
					if fc.tokenUser != "ssample-user" || fc.tokenPassword != tt.wantPassword {
						t.Fatalf("OAuth did not receive the production identity and selected password")
					}
					if !decision.confirm {
						if fc.created != 0 || len(conn.requests) != 0 || fc.ticket != "" || fc.email != "" {
							t.Fatal("declining must prevent creation and connection")
						}
						return
					}
					if fc.created != 1 || len(conn.requests) != 1 {
						t.Fatalf("creations = %d, connections = %d, want one each", fc.created, len(conn.requests))
					}
					if fc.ticket != tt.wantTicket || fc.email != "sample-user@test.com" {
						t.Fatalf("creation ticket = %q, email = %q; want %q and base-identity email", fc.ticket, fc.email, tt.wantTicket)
					}
					if r := conn.requests[0]; r.Username != "ssample-user" || r.Password != tt.wantPassword {
						t.Fatal("connection did not receive the production identity and selected password")
					}
				})
			}
		})
	}
}

func TestActiveProductionSuppliedDetails(t *testing.T) {
	for _, tt := range []struct {
		name, password, ticket string
		wantPassword           string
		wantPrompts            []string
	}{
		{
			name:         "neither supplied",
			wantPassword: "production-only",
			wantPrompts:  []string{"password"},
		},
		{
			name: "password supplied", password: "configured-production-password",
			wantPassword: "configured-production-password",
		},
		{
			name: "ticket supplied", ticket: "CHG-configured",
			wantPassword: "production-only",
			wantPrompts:  []string{"password"},
		},
		{
			name: "both supplied", password: "configured-production-password", ticket: "CHG-configured",
			wantPassword: "configured-production-password",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, conn, fc, ui := fixture()
			production(a)
			a.Config.ProductionPassword = tt.password
			a.Config.Ticket = tt.ticket
			ui.yes = false

			if err := a.Run(context.Background()); err != nil {
				t.Fatalf("Run error = %v", err)
			}
			if !slices.Equal(ui.prompts, tt.wantPrompts) {
				t.Fatalf("prompts = %v, want %v", ui.prompts, tt.wantPrompts)
			}
			if fc.tokenUser != "ssample-user" || fc.tokenPassword != tt.wantPassword {
				t.Fatal("OAuth did not receive the production identity and selected password")
			}
			if fc.created != 0 {
				t.Fatalf("creations = %d, want none for active access", fc.created)
			}
			if len(conn.requests) != 1 {
				t.Fatalf("connections = %d, want one", len(conn.requests))
			}
			if r := conn.requests[0]; r.Username != "ssample-user" || r.Password != tt.wantPassword {
				t.Fatal("connection did not receive the production identity and selected password")
			}
		})
	}
}

func TestActiveProductionRefreshPasswords(t *testing.T) {
	for _, tt := range []struct {
		name, password string
	}{
		{name: "prompted password"},
		{name: "supplied password", password: "configured-production-password"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, conn, fc, ui := fixture()
			production(a)
			a.Config.Once = false
			a.Config.ProductionPassword = tt.password
			ui.yes = false
			waits := 0
			a.Wait = func(context.Context, time.Duration) error {
				waits++
				wantPassword := tt.password
				var wantPrompts []string
				if tt.password == "" {
					wantPassword = ui.password
					wantPrompts = []string{"password"}
					if waits == 2 {
						wantPrompts = append(wantPrompts, "password")
					}
				}
				if !slices.Equal(ui.prompts, wantPrompts) || ui.calls != len(wantPrompts) {
					t.Fatalf("cycle %d: prompts = %v, calls = %d; want %v and %d calls",
						waits, ui.prompts, ui.calls, wantPrompts, len(wantPrompts))
				}
				if fc.tokenUser != "ssample-user" || fc.tokenPassword != wantPassword {
					t.Fatalf("cycle %d: OAuth did not receive the production identity and selected password", waits)
				}
				if fc.listCalls != waits || fc.created != 0 {
					t.Fatalf("cycle %d: access lookups = %d, creations = %d; want %d and 0",
						waits, fc.listCalls, fc.created, waits)
				}
				if len(conn.requests) != waits {
					t.Fatalf("cycle %d: connections = %d, want %d", waits, len(conn.requests), waits)
				}
				if r := conn.requests[waits-1]; r.Username != "ssample-user" || r.Password != wantPassword {
					t.Fatalf("cycle %d: connection did not receive the production identity and selected password", waits)
				}
				if waits >= 2 {
					return context.Canceled
				}
				// A fresh prompt must supply the next cycle's credentials.
				ui.password = "second-cycle-production-password"
				return nil
			}

			if err := a.Run(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v, want cancellation after two successful cycles", err)
			}
			if waits != 2 || len(conn.requests) != 2 {
				t.Fatalf("waits = %d, connections = %d; want two each", waits, len(conn.requests))
			}
		})
	}
}

func TestActiveProductionRefreshStopsOnConnectionFailure(t *testing.T) {
	for _, tt := range []struct {
		name, password string
	}{
		{name: "prompted password"},
		{name: "supplied password", password: "configured-production-password"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, conn, fc, ui := fixture()
			production(a)
			a.Config.Once = false
			a.Config.ProductionPassword = tt.password
			connectErr := errors.New("second connection failed")
			tokenCalls := 0
			fc.tokenFn = func(context.Context, string, string) (string, error) {
				tokenCalls++
				return "token", nil
			}
			var wantPrompts []string
			if tt.password == "" {
				wantPrompts = []string{"password"}
			}
			waits := 0
			a.Wait = func(context.Context, time.Duration) error {
				waits++
				if waits > 1 {
					// Bound the test if a failed refresh incorrectly continues.
					return context.Canceled
				}
				if len(conn.requests) != 1 || conn.err != nil {
					t.Fatalf("first cycle: connections = %d, error = %v; want one successful connection",
						len(conn.requests), conn.err)
				}
				if tokenCalls != 1 || fc.listCalls != 1 || fc.created != 0 {
					t.Fatalf("first cycle: tokens = %d, access checks = %d, creations = %d; want 1, 1, 0",
						tokenCalls, fc.listCalls, fc.created)
				}
				if !slices.Equal(ui.prompts, wantPrompts) || ui.calls != len(wantPrompts) {
					t.Fatalf("first cycle: prompts = %v, calls = %d; want %v and %d calls",
						ui.prompts, ui.calls, wantPrompts, len(wantPrompts))
				}
				conn.err = connectErr
				ui.password = "second-cycle-production-password"
				return nil
			}

			if err := a.Run(context.Background()); !errors.Is(err, connectErr) {
				t.Fatalf("Run error = %v, want connector error %v", err, connectErr)
			}
			if waits != 1 {
				t.Errorf("refresh waits = %d, want one", waits)
			}
			if len(conn.requests) != 2 {
				t.Fatalf("connections = %d, want two", len(conn.requests))
			}
			for i, r := range conn.requests {
				wantPassword := tt.password
				if wantPassword == "" {
					wantPassword = "production-only"
					if i == 1 {
						wantPassword = "second-cycle-production-password"
					}
				}
				if r.Username != "ssample-user" || r.Password != wantPassword {
					t.Errorf("cycle %d: connection did not receive the production identity and selected password", i+1)
				}
			}
			if tt.password == "" {
				wantPrompts = append(wantPrompts, "password")
			}
			if !slices.Equal(ui.prompts, wantPrompts) || ui.calls != len(wantPrompts) {
				t.Errorf("prompts = %v, calls = %d; want %v and %d calls",
					ui.prompts, ui.calls, wantPrompts, len(wantPrompts))
			}
			if tokenCalls != 2 || fc.listCalls != 2 || fc.created != 0 {
				t.Errorf("tokens = %d, access checks = %d, creations = %d; want 2, 2, 0",
					tokenCalls, fc.listCalls, fc.created)
			}
		})
	}
}

func TestProductionSelectionConnectsToChosenTarget(t *testing.T) {
	a, conn, fc, ui := fixture()
	a.Store = fakeStore{items: []namespace.Namespace{
		{Name: "payments", Environment: "prod", ID: "12", Account: "123456789012", Role: "PaymentsRole"},
		{Name: "orders", Environment: "prod", ID: "34", Account: "210987654321", Role: "OrdersRole"},
	}}
	ui.index = 1
	a.Config.ProductionPassword = "configured-production-password"
	a.Config.Ticket = "CHG-configured"

	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if want := []string{"choose"}; !slices.Equal(ui.prompts, want) {
		t.Fatalf("prompts = %v, want %v", ui.prompts, want)
	}
	if fc.tokenUser != "ssample-user" || fc.tokenPassword != a.Config.ProductionPassword {
		t.Fatal("OAuth did not receive the production identity and supplied password")
	}
	if fc.listCalls != 1 {
		t.Errorf("access lookups = %d, want one", fc.listCalls)
	}
	if fc.created != 0 {
		t.Errorf("creations = %d, want none for active access", fc.created)
	}
	if len(conn.requests) != 1 {
		t.Fatalf("connections = %d, want one", len(conn.requests))
	}
	r := conn.requests[0]
	if r.Namespace != "orders" || r.ARN != "arn:aws:iam::210987654321:role/OrdersRole" {
		t.Errorf("connection target = %q, %q; want selected orders namespace and ARN", r.Namespace, r.ARN)
	}
	if r.Username != "ssample-user" || r.Password != a.Config.ProductionPassword {
		t.Error("connection did not receive the production identity and supplied password")
	}
}

func TestJSONSuppliedProductionDetails(t *testing.T) {
	for _, tt := range []struct {
		name            string
		active          bool
		wantConnections int
	}{
		{name: "no active access"},
		{name: "active access", active: true, wantConnections: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, conn, fc, ui := fixture()
			production(a)
			a.Config.JSON = true
			a.Config.ProductionPassword = "configured-production-password"
			a.Config.Ticket = "CHG-configured"
			a.Output = Printer{Writer: io.Discard, JSON: true}
			if !tt.active {
				fc.active = nil
			}

			err := a.Run(context.Background())
			if tt.active {
				if err != nil {
					t.Fatalf("Run with active access: %v", err)
				}
			} else if err == nil || err.Error() != "authorization_failed" {
				t.Fatalf("Run error = %v, want JSON-mode creation refusal", err)
			}
			if ui.calls != 0 || len(ui.prompts) != 0 {
				t.Errorf("JSON mode prompted: calls = %d, prompts = %v", ui.calls, ui.prompts)
			}
			if fc.tokenUser != "ssample-user" || fc.tokenPassword != a.Config.ProductionPassword {
				t.Error("OAuth did not receive the production identity and supplied password")
			}
			if fc.listCalls != 1 {
				t.Errorf("access lookups = %d, want 1", fc.listCalls)
			}
			if fc.created != 0 {
				t.Errorf("firecall creations = %d, want 0", fc.created)
			}
			if len(conn.requests) != tt.wantConnections {
				t.Fatalf("connections = %d, want %d", len(conn.requests), tt.wantConnections)
			}
			if tt.active {
				if r := conn.requests[0]; r.Username != "ssample-user" || r.Password != a.Config.ProductionPassword {
					t.Error("connection did not receive the production identity and supplied password")
				}
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
