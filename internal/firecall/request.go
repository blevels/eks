package firecall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type operation string

const (
	opToken          operation = "oauth_token"
	opList           operation = "list_firecalls"
	opCreate         operation = "create_firecall"
	maxResponseBytes           = 1024 * 1024
)

func (c *Client) request(ctx context.Context, op operation, method, endpoint, token, contentType string, body io.Reader, out any) error {
	if c.HTTP == nil || c.timeout <= 0 {
		return &RequestError{Category: CategoryRequest}
	}
	// One budget covers all attempts, body reads and sleeps. In particular,
	// replacing HTTP in a test cannot accidentally disable this deadline.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	attempts := 1
	if op == opList && method == http.MethodGet {
		attempts = maxAttempts
	}
	wait := c.wait
	if wait == nil {
		wait = waitRetry
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return contextError(err)
		}
		start := time.Now()
		status, err := c.attempt(ctx, method, endpoint, token, contentType, body, out)
		if ctx.Err() != nil {
			err = contextError(ctx.Err())
		}
		c.logAttempt(ctx, op, attempt, status, time.Since(start), err)
		if err == nil {
			return nil
		}
		var failure *RequestError
		if attempt >= attempts || !errors.As(err, &failure) || !failure.retryable {
			return err
		}
		delay := max(backoff(attempt, c.jitter), time.Until(failure.retryAt))
		deadline, _ := ctx.Deadline()
		if delay >= time.Until(deadline) {
			// Do not shorten a server-mandated delay just to squeeze in a retry.
			return fmt.Errorf("cannot retry within API deadline: %w", contextError(context.DeadlineExceeded))
		}
		if c.Log != nil {
			c.Log.DebugContext(ctx, "HTTP retry scheduled", "operation", string(op), "attempt", attempt+1, "delay", delay)
		}
		if err := wait(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return contextError(ctx.Err())
			}
			return transportError(ctx, err)
		}
	}
}

// attempt closes the response before returning, so neither retry waits nor new
// attempts retain a previous response body. No raw I/O error escapes.
func (c *Client) attempt(ctx context.Context, method, endpoint, token, contentType string, body io.Reader, out any) (status int, err error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, &RequestError{Category: CategoryRequest}
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, transportError(ctx, err)
	}
	status = resp.StatusCode
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, &RequestError{Category: CategoryClose, StatusCode: status})
		}
	}()
	if status < 200 || status >= 300 {
		return status, &RequestError{
			Category: CategoryHTTP, StatusCode: status, retryable: retryableStatus(status),
			retryAt: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		failure := transportError(ctx, err)
		if failure.Category == CategoryNetwork && !failure.retryable {
			failure.Category = CategoryRead
		}
		failure.StatusCode = status
		return status, failure
	}
	if len(data) > maxResponseBytes {
		return status, &RequestError{Category: CategoryResponseLimit, StatusCode: status}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return status, &RequestError{Category: CategoryInvalidJSON, StatusCode: status}
	}
	return status, nil
}

func (c *Client) logAttempt(ctx context.Context, op operation, attempt, status int, duration time.Duration, err error) {
	if c.Log == nil {
		return
	}
	category := "success"
	level := slog.LevelDebug
	if err != nil {
		category, level = "request_failed", slog.LevelWarn
		var failure *RequestError
		if errors.As(err, &failure) {
			category = string(failure.Category)
		}
	}
	attrs := []any{"operation", string(op), "attempt", attempt, "duration", duration, "category", category}
	if status != 0 {
		attrs = append(attrs, "status_code", status)
	}
	c.Log.Log(ctx, level, "HTTP attempt", attrs...)
}
