package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"strings"
	"testing"
	"time"

	"eks/internal/diagnostics"
	"eks/internal/diagnostics/diagtest"
	"eks/internal/namespace"
)

func TestProductionSignInFailureDoesNotExposePasswords(t *testing.T) {
	const suppliedPassword = "CANARY-supplied-production-password-9371"
	const promptedPassword = "CANARY-prompted-production-password-2846"
	oauthErr := errors.New("OAuth sign-in failed")
	for _, supplied := range []bool{true, false} {
		name, wantPassword, wantPrompts := "prompted", promptedPassword, 1
		if supplied {
			name, wantPassword, wantPrompts = "supplied", suppliedPassword, 0
		}
		t.Run(name, func(t *testing.T) {
			a, _, fc, ui := fixture()
			production(a)
			ui.password = promptedPassword
			if supplied {
				a.Config.ProductionPassword = suppliedPassword
			}
			var logs, output bytes.Buffer
			a.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			a.Output = Printer{Writer: &output}
			fc.tokenFn = func(_ context.Context, _, password string) (string, error) {
				if password != wantPassword {
					t.Error("OAuth did not receive the selected production password")
				}
				return "", oauthErr
			}

			if err := a.Run(context.Background()); !errors.Is(err, oauthErr) {
				t.Fatalf("Run error = %v, want original OAuth error", err)
			}
			if ui.calls != wantPrompts || (wantPrompts == 1 && ui.prompts[0] != "password") {
				t.Errorf("prompts = %v, calls = %d; want %d password prompts", ui.prompts, ui.calls, wantPrompts)
			}
			if !strings.Contains(logs.String(), "oauth_token.outcome") {
				t.Error("OAuth outcome was not captured in diagnostic logs")
			}
			for _, password := range []string{suppliedPassword, promptedPassword} {
				if strings.Contains(logs.String(), password) {
					t.Error("production password appeared in diagnostic logs")
				}
				if strings.Contains(output.String(), password) {
					t.Error("production password appeared in application output")
				}
			}
		})
	}
}

func TestWorkflowDiagnosticCoverage(t *testing.T) {
	for _, scenario := range []string{"nonprod", "list", "active", "created", "declined", "noninteractive", "activation_timeout", "activation_cancel", "refresh_cancel", "decrypt_user", "decrypt_password", "query", "connection", "output", "ambiguous", "invalid_choice", "password", "confirmation", "ticket", "empty", "invalid_row", "production_config"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			a, conn, fc, ui := fixture()
			switch scenario {
			case "list":
				a.Config.List = true
			case "active":
				production(a)
			case "created":
				production(a)
				fc.active = nil
			case "declined":
				production(a)
				fc.active = nil
				ui.yes = false
			case "noninteractive":
				production(a)
				fc.active = nil
				a.Config.JSON = true
				a.Config.ProductionPassword = "CANARY"
			case "activation_timeout", "activation_cancel":
				production(a)
				fc.active = nil
				fc.noActivation = true
				a.Wait = func(context.Context, time.Duration) error {
					if scenario == "activation_cancel" {
						return context.Canceled
					}
					return context.DeadlineExceeded
				}
			case "refresh_cancel":
				a.Config.Once = false
				a.Wait = func(context.Context, time.Duration) error { return context.Canceled }
			case "decrypt_user":
				a.Config.EncryptedUser = "CANARY invalid"
			case "decrypt_password":
				a.Config.EncryptedPassword = "CANARY invalid"
			case "query":
				a.Store = fakeStore{err: errors.New("CANARY driver")}
			case "connection":
				conn.err = errors.New("CANARY child")
			case "output":
				a.Output = Printer{Writer: brokenWriter{}}
			case "ambiguous", "invalid_choice":
				s := a.Store.(fakeStore)
				s.items = append(s.items, s.items[0])
				a.Store = s
				if scenario == "ambiguous" {
					a.Config.JSON = true
				} else {
					ui.index = 99
				}
			case "password":
				production(a)
				ui.err = errors.New("CANARY password")
			case "confirmation":
				production(a)
				a.Config.ProductionPassword = "CANARY"
				fc.active = nil
				ui.err = errors.New("CANARY confirm")
			case "ticket":
				production(a)
				a.Config.ProductionPassword = "CANARY"
				fc.active = nil
				a.UI = ticketFailureUI{fakeUI: ui}
			case "empty":
				a.Store = fakeStore{}
			case "invalid_row":
				a.Store = fakeStore{items: []namespace.Namespace{{Name: "CANARY invalid"}}}
			case "production_config":
				production(a)
				a.Config.OAuthURL = "CANARY"
			}
			err := a.Run(ctx)
			if scenario == "nonprod" || scenario == "list" || scenario == "active" || scenario == "created" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "sample-user", "fixture-only", "123456789012", "payments", "test.com")
			if scenario == "declined" {
				for _, r := range logs.Records() {
					if r["level"] == "ERROR" {
						t.Fatal("decline mislabeled", logs.Text())
					}
				}
			}
			if scenario == "active" {
				logs.Event("authorization.allowed_active")
			}
			if scenario == "created" {
				logs.Event("authorization.allowed_activated")
			}
			if scenario == "activation_timeout" && logs.Event("activation.outcome")["status"] != "timeout" {
				t.Fatal(logs.Text())
			}
			if scenario == "nonprod" {
				logs.Event("connection.permitted")
				logs.Event("connection.outcome")
			}
		})
	}
}

func TestSourceIdentifiesOperationalCallerNotObservationWrapper(t *testing.T) {
	ctx, logs := diagtest.New(t)
	a, _, _, _ := fixture()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "app.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"decrypt_user.start", "decrypt_password.outcome", "output_details.outcome", "connection.outcome"} {
		source := logs.Event(event)["source"].(map[string]any)
		if source["file"] != "internal/app/app.go" {
			t.Fatal(event, source)
		}
		line := int(source["line"].(float64))
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || (fn.Name.Name != "observe" && fn.Name.Name != "action" && fn.Name.Name != "failed") {
				continue
			}
			if line >= fset.Position(fn.Pos()).Line && line <= fset.Position(fn.End()).Line {
				t.Fatal("source identifies helper", event, source)
			}
		}
	}
}

type ticketFailureUI struct{ *fakeUI }

func (ticketFailureUI) Ticket(context.Context) (string, error) {
	return "", errors.New("CANARY ticket error")
}

func TestJSONResultsIndependentFromDiagnosticFormat(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		a, _, _, _ := fixture()
		a.Config.JSON = true
		var out, stderr bytes.Buffer
		a.Output = Printer{Writer: &out, JSON: true}
		ctx, capture := diagtest.New(t)
		// The app receives a context logger independently from Printer.JSON.
		if format == "text" {
			capture.D.Log = diagnostics.NewLogger(&stderr, format, true, false, "test")
		}
		if err := a.Run(ctx); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err, out.String())
			}
			if record["schema_version"] != nil {
				t.Fatal("diagnostic entered stdout")
			}
		}
		if strings.Contains(out.String(), "Application diagnostic") {
			t.Fatal(out.String())
		}
	}
}

type failNthOutput struct{ calls, failAt int }

func (w *failNthOutput) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, errors.New("CANARY output writer")
	}
	return len(p), nil
}

func TestAllOutputBoundariesOwnWriterFailures(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, stage := range []string{"list", "details", "active", "created", "connected"} {
			t.Run(fmt.Sprintf("%t/%s", jsonMode, stage), func(t *testing.T) {
				ctx, logs := diagtest.New(t)
				a, _, fc, _ := fixture()
				failAt := 2
				switch stage {
				case "list":
					a.Config.List = true
					failAt = 1
				case "details":
					failAt = 1
				case "active":
					production(a)
				case "created":
					production(a)
					fc.active = nil
				}
				a.Output = Printer{Writer: &failNthOutput{failAt: failAt}, JSON: jsonMode}
				err := a.Run(ctx)
				if err == nil || err.Error() != "output_"+stage+"_failed" {
					t.Fatal(err, logs.Text())
				}
				logs.Event("output_" + stage + ".outcome")
				logs.Contract()
				logs.NoSecrets("CANARY")
				if stage == "connected" {
					for _, r := range logs.Records() {
						if r["event"] == "connection.blocked" {
							t.Fatal("reported connection blocked after successful child")
						}
					}
				}
			})
		}
	}
}

func TestSerializationFailureIsOwnedByOutputBoundary(t *testing.T) {
	ctx, logs := diagtest.New(t)
	var out bytes.Buffer
	p := Printer{Writer: &out, JSON: true}
	err := action(ctx, "output", "output_list", func(context.Context) error {
		return p.event("synthetic", make(chan string))
	})
	if err == nil || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	if logs.Event("output_list.outcome")["error_code"] != "output_list_failed" {
		t.Fatal(logs.Text())
	}
	logs.Contract()
}
