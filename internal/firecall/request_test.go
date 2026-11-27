package firecall

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func noDelay(c *Client) *[]time.Duration {
	delays := new([]time.Duration)
	c.jitter = func(time.Duration) time.Duration { return 0 }
	c.wait = func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}
	return delays
}

func TestListRetriesOnlyTransientHTTPStatuses(t *testing.T) {
	for _, status := range []int{408, 429, 502, 503, 504, 400, 401, 403, 404, 500, 501, 302} {
		for _, recover := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/recover=%v", status, recover), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if recover && calls == 2 {
						fmt.Fprint(w, `[{"account_id":"123","iamRoleArn":"arn","firecall_status":"active"}]`)
						return
					}
					w.WriteHeader(status)
					fmt.Fprint(w, "private-response")
				}))
				defer s.Close()
				c := New(5*time.Second, "", s.URL, "")
				delays := noDelay(c)
				found, err := c.List(context.Background(), "private-token", "123", "arn")
				transient := retryableStatus(status)
				wantCalls := 1
				if transient {
					wantCalls = 3
					if recover {
						wantCalls = 2
					}
				}
				if calls != wantCalls || len(*delays) != wantCalls-1 {
					t.Fatalf("calls=%d delays=%v", calls, *delays)
				}
				if (err == nil) != (transient && recover) {
					t.Fatal(found, err)
				}
				if err == nil && len(found) != 1 {
					t.Fatal(found)
				}
				if wantCalls > 1 && (*delays)[0] != 500*time.Millisecond {
					t.Fatal(*delays)
				}
				if wantCalls == 3 && (*delays)[1] != time.Second {
					t.Fatal(*delays)
				}
			})
		}
	}
}

func TestPOSTRequestsNeverRetried(t *testing.T) {
	for _, status := range []int{408, 429, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
			}))
			defer s.Close()
			c := New(5*time.Second, s.URL, "", s.URL)
			delays := noDelay(c)
			if _, err := c.Token(context.Background(), "user", "password"); err == nil {
				t.Fatal("OAuth failure accepted")
			}
			if calls != 1 || len(*delays) != 0 {
				t.Fatal(calls, *delays)
			}
			if _, err := c.Create(context.Background(), "token", "arn", "email", "ticket", 8); err == nil {
				t.Fatal("creation failure accepted")
			}
			if calls != 2 || len(*delays) != 0 {
				t.Fatal(calls, *delays)
			}
		})
	}
}

func TestRetryAfterScheduling(t *testing.T) {
	for _, header := range []string{"2", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat), "0", "invalid"} {
		t.Run(header, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", header)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				fmt.Fprint(w, "[]")
			}))
			defer s.Close()
			c := New(2*time.Minute, "", s.URL, "")
			delays := noDelay(c)
			var scheduled time.Time
			c.wait = func(_ context.Context, delay time.Duration) error {
				*delays = append(*delays, delay)
				scheduled = time.Now().Add(delay)
				return nil
			}
			started := time.Now()
			if _, err := c.List(context.Background(), "t", "a", "r"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || len(*delays) != 1 {
				t.Fatal(calls, *delays)
			}
			d := (*delays)[0]
			if header == "2" && (scheduled.Before(started.Add(2*time.Second)) || d > 2*time.Second) {
				t.Fatal("numeric Retry-After ignored", d)
			}
			if strings.Contains(header, "GMT") {
				minimum, err := http.ParseTime(header)
				if err != nil || scheduled.Before(minimum) {
					t.Fatal("date Retry-After ignored", d)
				}
			}
			if (header == "0" || header == "invalid") && d != initialBackoff {
				t.Fatal(d)
			}
		})
	}
}

func TestLogicalDeadlineAndCancellation(t *testing.T) {
	for _, mode := range []string{"retry-after-too-long", "backoff-too-long", "cancel-wait", "deadline-wait", "deadline-request", "already-canceled"} {
		t.Run(mode, func(t *testing.T) {
			c := New(5*time.Second, "", "https://example.invalid", "")
			calls := 0
			var deadlines []time.Time
			c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Fatal("unbounded HTTP request")
				}
				deadlines = append(deadlines, deadline)
				if mode == "deadline-request" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				header := http.Header{}
				if mode == "retry-after-too-long" {
					header.Set("Retry-After", "99999999999999999999999")
				}
				return &http.Response{StatusCode: 503, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := error(context.DeadlineExceeded)
			switch mode {
			case "backoff-too-long", "deadline-request":
				c.timeout = 20 * time.Millisecond
			case "cancel-wait":
				c.wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
				expected = context.Canceled
			case "deadline-wait":
				c.jitter = func(time.Duration) time.Duration { return 0 }
				c.wait = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 550*time.Millisecond)
				defer stop()
			case "already-canceled":
				cancel()
				expected = context.Canceled
			}
			_, err := c.List(ctx, "secret", "a", "r")
			if !errors.Is(err, expected) {
				t.Fatalf("got %v want %v", err, expected)
			}
			wantCalls := 1
			if mode == "already-canceled" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatal("unexpected attempts", calls)
			}
			if len(deadlines) > 0 && time.Until(deadlines[0]) > c.timeout {
				t.Fatal("extended configured deadline")
			}
			if parent, ok := ctx.Deadline(); ok && len(deadlines) > 0 && deadlines[0].After(parent) {
				t.Fatal("extended caller deadline")
			}
		})
	}
}

func TestRetriesShareOneDeadline(t *testing.T) {
	c := New(5*time.Second, "", "https://example.invalid", "")
	noDelay(c)
	var first time.Time
	calls := 0
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		d, _ := r.Context().Deadline()
		if first.IsZero() {
			first = d
		}
		if !d.Equal(first) {
			t.Fatal("deadline reset between attempts")
		}
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNRESET}
	})
	_, err := c.List(context.Background(), "token", "a", "r")
	if err == nil || calls != 3 {
		t.Fatal(err, calls)
	}
}

func TestTransportFailureCategories(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		category ErrorCategory
		attempts int
		timeout  bool
	}{
		{"reset", syscall.ECONNRESET, CategoryNetwork, 3, false},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, CategoryNetwork, 3, false},
		{"short response", io.ErrUnexpectedEOF, CategoryNetwork, 3, false},
		{"timeout", &net.DNSError{Err: "secret-host", IsTimeout: true}, CategoryTimeout, 3, true},
		{"temporary DNS", &net.DNSError{Err: "private", IsTemporary: true}, CategoryNetwork, 3, false},
		{"NXDOMAIN", &net.DNSError{Err: "private", IsNotFound: true}, CategoryNetwork, 1, false},
		{"certificate", x509.UnknownAuthorityError{}, CategoryTLS, 1, false},
		{"hostname", x509.HostnameError{Host: "private-host"}, CategoryTLS, 1, false},
		{"certificate invalid", x509.CertificateInvalidError{}, CategoryTLS, 1, false},
		{"wrapped TLS", &tls.CertificateVerificationError{Err: errors.New("private TLS detail")}, CategoryTLS, 1, false},
		{"TLS protocol", tls.RecordHeaderError{Msg: "private protocol detail"}, CategoryTLS, 1, false},
		{"TLS alert", tls.AlertError(40), CategoryTLS, 1, false},
		{"other", errors.New("private unknown detail"), CategoryNetwork, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := New(5*time.Second, "", "https://secret-host/private-path?secret=value", "")
			noDelay(c)
			calls := 0
			c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, tt.err })
			_, err := c.List(context.Background(), "private-token", "a", "r")
			var failure *RequestError
			if !errors.As(err, &failure) || failure.Category != tt.category || calls != tt.attempts {
				t.Fatal(err, calls)
			}
			var network net.Error
			if !errors.As(err, &network) || network.Timeout() != tt.timeout {
				t.Fatal("timeout classification lost", err)
			}
			if tt.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("missing safe timeout cause")
			}
			var raw *url.Error
			if errors.As(err, &raw) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatal("raw transport detail escaped", err)
			}
		})
	}
}

func TestHTTPDiagnosticsExcludeSensitiveDetails(t *testing.T) {
	var log bytes.Buffer
	c := New(5*time.Second, "", "https://secret-host/private-path?private-query=value", "")
	c.Log = slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	noDelay(c)
	calls := 0
	c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("private-transport: %w", syscall.ECONNRESET)
		}
		if calls == 2 {
			return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"private-header"}}, Body: io.NopCloser(strings.NewReader("private-response"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})
	if _, err := c.List(context.Background(), "private-token", "private-account", "private-role"); err != nil {
		t.Fatal(err)
	}
	text := log.String()
	for _, secret := range []string{"secret-host", "private", "https://", "Authorization", "Bearer"} {
		if strings.Contains(text, secret) {
			t.Fatalf("sensitive diagnostic %s", text)
		}
	}
	for _, want := range []string{`"operation":"list_firecalls"`, `"attempt":1`, `"attempt":2`, `"attempt":3`, `"duration":`, `"status_code":503`, `"status_code":200`, `"category":"network"`, `"category":"http_status"`, `"category":"success"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
}
