package firecall

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestExponentialBackoff(t *testing.T) {
	for i, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		got := backoff(i+1, func(time.Duration) time.Duration { return 0 })
		if got != want {
			t.Fatalf("retry %d: got %s want %s", i+1, got, want)
		}
		upper := min(want+want/4, maxBackoff)
		for j := 0; j < 100; j++ {
			random := backoff(i+1, nil)
			if random < want || random > upper {
				t.Fatalf("retry %d jitter %s not in [%s,%s]", i+1, random, want, upper)
			}
		}
		if got := backoff(i+1, func(d time.Duration) time.Duration { return d }); got != upper {
			t.Fatalf("maximum jitter: %s want %s", got, upper)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		header string
		want   time.Time
	}{
		{"", time.Time{}},
		{"0", now},
		{" 12 ", now.Add(12 * time.Second)},
		{now.Add(time.Minute).Format(http.TimeFormat), now.Add(time.Minute)},
		{now.Add(-time.Minute).Format(http.TimeFormat), now.Add(-time.Minute)},
		{"-2", time.Time{}},
		{"1.5", time.Time{}},
		{"invalid-secret-header", time.Time{}},
		{"+2", time.Time{}},
	} {
		if got := retryAfter(tt.header, now); !got.Equal(tt.want) {
			t.Fatalf("header %q: got %v want %v", tt.header, got, tt.want)
		}
	}
	for _, header := range []string{"9223372036854775807", "999999999999999999999999999999999999"} {
		if got := retryAfter(header, now); !got.After(now.Add(24 * time.Hour)) {
			t.Fatal("overflow ignored", got)
		}
	}
}

func TestWaitRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := waitRetry(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}
