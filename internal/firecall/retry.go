package firecall

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxAttempts    = 3
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// backoff uses additive jitter of up to 25%, capped at five seconds including
// jitter. Its first two base delays are 500ms and 1s. No delay follows the final
// attempt. The jitter source is injectable for deterministic tests.
func backoff(retry int, jitter func(time.Duration) time.Duration) time.Duration {
	base := initialBackoff
	for i := 1; i < retry && base < maxBackoff; i++ {
		base = min(base*2, maxBackoff)
	}
	room := min(base/4, maxBackoff-base)
	if room == 0 {
		return base
	}
	if jitter == nil {
		return base + time.Duration(rand.Int64N(int64(room)+1))
	}
	return base + max(0, min(jitter(room), room))
}

// retryAfter returns an absolute lower bound. Numeric overflow is clamped to a
// far-future time rather than ignored: an enormous server delay must not cause
// an early retry. Negative/fractional values are not valid Retry-After syntax.
func retryAfter(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	digits := true
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			digits = false
			break
		}
	}
	if digits {
		const maxDelay = time.Duration(1<<63 - 1)
		seconds, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || seconds > uint64(maxDelay/time.Second) {
			return now.Add(maxDelay)
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	date, err := http.ParseTime(raw)
	if err != nil {
		return time.Time{}
	}
	return date
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
