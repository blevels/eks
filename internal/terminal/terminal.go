// Package terminal owns human interaction; prompts never go to stdout.
package terminal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"eks/internal/namespace"
	"golang.org/x/term"
)

type Terminal struct {
	In     *os.File
	Out    io.Writer
	reader *bufio.Reader
	mu     sync.Mutex
}

func (t *Terminal) line(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprint(t.Out, prompt); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		t.mu.Lock()
		if t.reader == nil {
			t.reader = bufio.NewReader(t.In)
		}
		s, err := t.reader.ReadString('\n')
		if errors.Is(err, io.EOF) && s != "" {
			err = nil
		}
		t.mu.Unlock()
		ch <- result{strings.TrimSpace(s), err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("read terminal input: %w", r.err)
		}
		return r.s, nil
	}
}

func (t *Terminal) Choose(ctx context.Context, items []namespace.Namespace) (int, error) {
	for i, n := range items {
		if _, err := fmt.Fprintf(t.Out, "%d) %s|%s|%s|%s\n", i+1, n.Name, n.Account, n.Role, n.Environment); err != nil {
			return 0, err
		}
	}
	for {
		s, err := t.line(ctx, fmt.Sprintf("Enter option number (1-%d): ", len(items)))
		if err != nil {
			return 0, err
		}
		i, err := strconv.Atoi(s)
		if err == nil && i >= 1 && i <= len(items) {
			return i - 1, nil
		}
		if _, err := fmt.Fprintln(t.Out, "Invalid selection. Please choose a valid number."); err != nil {
			return 0, err
		}
	}
}

func (t *Terminal) Password(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(t.In.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("production password requires a terminal or PROD_PASSWORD secret configuration")
	}
	// Set raw mode synchronously before starting the reader. Using ReadPassword
	// inside a goroutine could disable echo *after* cancellation restored it.
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("read terminal state: %w", err)
	}
	if _, err := fmt.Fprint(t.Out, "Enter production authorization (CyberArk) password: "); err != nil {
		return "", errors.Join(err, term.Restore(fd, state))
	}
	p, readErr := t.readPassword(ctx)
	password, err := string(p), readErr
	restoreErr := term.Restore(fd, state)
	_, writeErr := fmt.Fprintln(t.Out)
	if err = errors.Join(err, restoreErr, writeErr); err != nil {
		return "", fmt.Errorf("read production password: %w", err)
	}
	if password == "" {
		return "", errors.New("production password cannot be empty")
	}
	return password, nil
}

func (t *Terminal) readPassword(ctx context.Context) ([]byte, error) {
	type result struct {
		p   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		t.mu.Lock()
		if t.reader == nil {
			t.reader = bufio.NewReader(t.In)
		}
		// A cancelled read may not have started before its caller returns.
		// Check under the lock so it cannot begin stealing input after a later
		// prompt has acquired the terminal.
		if err := ctx.Err(); err != nil {
			t.mu.Unlock()
			ch <- result{err: err}
			return
		}
		recorded := &recordingByteReader{r: t.reader}
		p, err := readSecret(recorded)
		if ctx.Err() != nil && len(recorded.read) > 0 {
			// The blocking terminal read cannot be interrupted portably. Put
			// back everything it consumed so the next prompt receives it.
			t.reader = bufio.NewReader(io.MultiReader(bytes.NewReader(recorded.read), t.reader))
		}
		t.mu.Unlock()
		ch <- result{p, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.p, r.err
	}
}

// readSecret is independent of terminal state so editing/cancellation behavior
// can be tested without changing the test runner's terminal.
func readSecret(r interface{ ReadByte() (byte, error) }) ([]byte, error) {
	var result []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		switch b {
		case '\r', '\n':
			return result, nil
		case 3: // Ctrl-C in raw mode does not generate SIGINT.
			return nil, context.Canceled
		case 4:
			return nil, io.EOF
		case 8, 127: // backspace/delete; remove one UTF-8 codepoint.
			if len(result) > 0 {
				_, size := utf8.DecodeLastRune(result)
				result = result[:len(result)-size]
			}
		default:
			if b < 32 {
				continue
			}
			if len(result) >= 4096 {
				return nil, errors.New("production password exceeds 4096 bytes")
			}
			result = append(result, b)
		}
	}
}

type recordingByteReader struct {
	r    interface{ ReadByte() (byte, error) }
	read []byte
}

func (r *recordingByteReader) ReadByte() (byte, error) {
	b, err := r.r.ReadByte()
	if err == nil {
		r.read = append(r.read, b)
	}
	return b, err
}

func (t *Terminal) Confirm(ctx context.Context) (bool, error) {
	for {
		s, err := t.line(ctx, "Would you like to open a firecall request for this environment? (Y/N): ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if _, err := fmt.Fprintln(t.Out, "Invalid input. Please enter Y or N."); err != nil {
			return false, err
		}
	}
}
func (t *Terminal) Ticket(ctx context.Context) (string, error) {
	s, err := t.line(ctx, "Enter a valid production CM # or SNOW INCIDENT #: ")
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", errors.New("a change/incident ticket is required")
	}
	return s, nil
}
