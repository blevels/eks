package terminal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"eks/internal/namespace"
)

func TestSecretEditing(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		bad               bool
	}{
		{"ascii", "abc\r", "abc", false},
		{"backspace", "ab\x7fc\n", "ac", false},
		{"unicode", "aé\x7fb\n", "ab", false},
		{"unicode followed by ascii", "éa\x7f\n", "é", false},
		{"control c", "abc\x03", "", true},
		{"control d", "abc\x04", "", true},
		{"EOF", "abc", "", true},
		{"limit", strings.Repeat("x", 4097) + "\n", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value, err := readSecret(bufio.NewReader(strings.NewReader(tt.input)))
			if (err != nil) != tt.bad || string(value) != tt.want {
				t.Fatal(string(value), err)
			}
		})
	}
}

func input(t *testing.T, s string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, s); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}
func TestPrompts(t *testing.T) {
	var out bytes.Buffer
	ui := Terminal{In: input(t, "0\n2\nmaybe\nyes\nCHG123\n"), Out: &out}
	index, err := ui.Choose(context.Background(), []namespace.Namespace{{Name: "a"}, {Name: "b"}})
	if err != nil || index != 1 {
		t.Fatal(index, err)
	}
	yes, err := ui.Confirm(context.Background())
	if err != nil || !yes {
		t.Fatal(yes, err)
	}
	ticket, err := ui.Ticket(context.Background())
	if err != nil || ticket != "CHG123" {
		t.Fatal(ticket, err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Invalid selection")) || !bytes.Contains(out.Bytes(), []byte("Invalid input")) {
		t.Fatal(out.String())
	}
}
func TestPromptFailures(t *testing.T) {
	ui := &Terminal{In: input(t, ""), Out: io.Discard}
	if _, err := ui.Confirm(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	ui = &Terminal{In: input(t, "\n"), Out: io.Discard}
	if _, err := ui.Ticket(context.Background()); err == nil {
		t.Fatal("empty ticket accepted")
	}
	if _, err := ui.Password(context.Background()); err == nil {
		t.Fatal("nonterminal password input accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ui.Confirm(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCancelledPasswordReadPreservesInputForNextPrompt(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	ui := &Terminal{In: r, Out: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan error, 1)
	go func() {
		_, err := ui.readPassword(ctx)
		readDone <- err
	}()

	// Wait until the password reader owns the terminal and is blocked awaiting
	// input, rather than cancelling before its goroutine begins.
	deadline := time.Now().Add(time.Second)
	for ui.mu.TryLock() {
		ui.mu.Unlock()
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("password reader did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-readDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled password read returned %v", err)
	}

	if _, err := io.WriteString(w, "next-prompt-answer\n"); err != nil {
		t.Fatal(err)
	}
	line, err := ui.line(context.Background(), "Next prompt: ")
	if err != nil {
		t.Fatalf("next prompt could not read input: %v", err)
	}
	if line != "next-prompt-answer" {
		t.Fatalf("next prompt received %q, want %q", line, "next-prompt-answer")
	}
}

func TestNoAndFinalLine(t *testing.T) {
	ui := Terminal{In: input(t, "no"), Out: io.Discard}
	yes, err := ui.Confirm(context.Background())
	if err != nil || yes {
		t.Fatal(yes, err)
	}
}
