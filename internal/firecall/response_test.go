package firecall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

type responseBody struct {
	reader            io.Reader
	readErr, closeErr error
	closed            *int
}

func (b *responseBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.reader.Read(p)
}
func (b *responseBody) Close() error {
	*b.closed++
	return b.closeErr
}

func TestResponseFailurePolicyAndCleanup(t *testing.T) {
	for _, tt := range []struct {
		name, body        string
		readErr, closeErr error
		category          ErrorCategory
		attempts          int
	}{
		{name: "invalid JSON", body: "private-invalid-json", category: CategoryInvalidJSON, attempts: 1},
		{name: "unexpected JSON type", body: `{"private":"object-not-array"}`, category: CategoryInvalidJSON, attempts: 1},
		{name: "oversize", body: strings.Repeat("x", maxResponseBytes+1), category: CategoryResponseLimit, attempts: 1},
		{name: "read reset", readErr: syscall.ECONNRESET, category: CategoryNetwork, attempts: 3},
		{name: "read short", readErr: io.ErrUnexpectedEOF, category: CategoryNetwork, attempts: 3},
		{name: "read unknown", readErr: errors.New("private-read-error"), category: CategoryRead, attempts: 1},
		{name: "close", body: "[]", closeErr: errors.New("private-close-error"), category: CategoryClose, attempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := New(5*time.Second, "", "https://example.invalid", "")
			calls, closed := 0, 0
			noDelay(c)
			c.wait = func(context.Context, time.Duration) error {
				if closed != calls {
					t.Fatal("retry waited before closing response")
				}
				return nil
			}
			c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				if closed != calls {
					t.Fatal("retry started before closing previous response")
				}
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{},
					Body: &responseBody{reader: strings.NewReader(tt.body), readErr: tt.readErr, closeErr: tt.closeErr, closed: &closed}}, nil
			})
			_, err := c.List(context.Background(), "token", "a", "r")
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Category != tt.category || calls != tt.attempts || closed != calls {
				t.Fatal(err, calls, closed)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), "private") {
				t.Fatal("remote text escaped", err)
			}
		})
	}
}

func TestTokenResponseFailuresAndCleanup(t *testing.T) {
	for _, tt := range []struct {
		name              string
		readErr, closeErr error
		category          ErrorCategory
	}{
		{name: "read reset", readErr: syscall.ECONNRESET, category: CategoryNetwork},
		{name: "read short", readErr: io.ErrUnexpectedEOF, category: CategoryNetwork},
		{name: "read unknown", readErr: errors.New("private-read-error"), category: CategoryRead},
		{name: "close after valid token", closeErr: errors.New("private-close-error"), category: CategoryClose},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := New(time.Second, "https://example.invalid/oauth", "", "")
			delays := noDelay(c)
			calls, closed := 0, 0
			c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body: &responseBody{
						reader:   strings.NewReader(`{"access_token":"private-usable-token"}`),
						readErr:  tt.readErr,
						closeErr: tt.closeErr,
						closed:   &closed,
					},
				}, nil
			})

			token, err := c.Token(context.Background(), "private-user", "private-password")
			if err == nil {
				t.Fatal("failed OAuth response accepted")
			}
			if token != "" {
				t.Error("failed OAuth response returned a nonempty token")
			}
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Category != tt.category {
				t.Errorf("error = %v, want category %s", err, tt.category)
			}
			if calls != 1 || closed != 1 || len(*delays) != 0 {
				t.Errorf("calls = %d, closes = %d, retry delays = %v; want 1, 1, none", calls, closed, *delays)
			}
		})
	}
}

func TestPOSTTransportFailuresAndDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	c := New(time.Second, "https://example.invalid/private-oauth", "", "https://example.invalid/private-create")
	c.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	delays := noDelay(c)
	calls := 0
	c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("private-network-detail: %w", syscall.ECONNRESET)
	})
	_, tokenErr := c.Token(context.Background(), "private-user", "private-password")
	_, createErr := c.Create(context.Background(), "private-token", "private-arn", "private-email", "private-ticket", 8)
	if tokenErr == nil || createErr == nil || calls != 2 || len(*delays) != 0 {
		t.Fatal(tokenErr, createErr, calls, *delays)
	}
	for _, text := range []string{logs.String(), tokenErr.Error(), createErr.Error()} {
		if strings.Contains(text, "private") || strings.Contains(text, "https://") {
			t.Fatal("secret in diagnostics", text)
		}
	}
	if !strings.Contains(logs.String(), "operation=oauth_token") || !strings.Contains(logs.String(), "operation=create_firecall") {
		t.Fatal("missing operation diagnostics", logs.String())
	}
}

func TestCancellationDuringRequestIsSafe(t *testing.T) {
	c := New(time.Second, "", "https://example.invalid/private", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, fmt.Errorf("private-transport-error: %w", r.Context().Err())
	})
	_, err := c.List(ctx, "private-token", "a", "r")
	if !errors.Is(err, context.Canceled) || calls != 1 || strings.Contains(err.Error(), "private") {
		t.Fatal(err, calls)
	}
}
