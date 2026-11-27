package terminal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"eks/internal/diagnostics/diagtest"
	"eks/internal/namespace"
)

type diagnosticBrokenWriter struct{}

func (diagnosticBrokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("CANARY output /private/path")
}

func TestTerminalDiagnosticOutcomes(t *testing.T) {
	for _, name := range []string{"selection", "confirmation", "ticket", "eof", "empty_ticket", "output", "selection_output", "canceled", "nonterminal"} {
		t.Run(name, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			ui := Terminal{In: input(t, "CANARY invalid\n1\nyes\nCANARY ticket\n"), Out: &bytes.Buffer{}}
			var err error
			switch name {
			case "selection":
				_, err = ui.Choose(ctx, []namespace.Namespace{{Name: "CANARY name"}})
				logs.Event("terminal.invalid_selection")
			case "confirmation":
				_, err = ui.Confirm(ctx)
				logs.Event("terminal.invalid_confirmation")
			case "ticket":
				_, err = ui.Ticket(ctx)
			case "eof":
				ui.In = input(t, "")
				_, err = ui.Confirm(ctx)
				if !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
			case "empty_ticket":
				ui.In = input(t, "\n")
				_, err = ui.Ticket(ctx)
			case "output":
				ui.Out = diagnosticBrokenWriter{}
				_, err = ui.Confirm(ctx)
			case "selection_output":
				ui.Out = diagnosticBrokenWriter{}
				_, err = ui.Choose(ctx, []namespace.Namespace{{Name: "CANARY"}})
			case "canceled":
				canceled, stop := context.WithCancel(ctx)
				stop()
				_, err = ui.Confirm(canceled)
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if logs.Event("confirmation.outcome")["level"] != "INFO" {
					t.Fatal(logs.Text())
				}
			case "nonterminal":
				_, err = ui.Password(ctx)
			}
			if name != "selection" && name != "confirmation" && name != "ticket" && err == nil {
				t.Fatal("expected failure")
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "/private/path", "Would you", "Enter option")
		})
	}
}
