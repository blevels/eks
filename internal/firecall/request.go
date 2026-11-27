package firecall

import (
	"context"
	"eks/internal/diagnostics"
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
		attemptCtx, attemptScope := diagnostics.Start(ctx, "http", "http_attempt")
		status, err := c.attempt(attemptCtx, method, endpoint, token, contentType, body, out)
		if ctx.Err() != nil {
			err = contextError(ctx.Err())
		}
		numbers := []diagnostics.Number{diagnostics.N("attempt", attempt), diagnostics.N("max_attempts", attempts), diagnostics.N("http_status", status)}
		if err == nil {
			_ = attemptScope.End(nil, numbers...)
			return nil
		}
		var failure *RequestError
		if attempt >= attempts || !errors.As(err, &failure) || !failure.retryable {
			return attemptScope.End(err, numbers...)
		}
		// A recoverable attempt is not a terminal primary error.
		attemptScope.Event("http.retryable_attempt", slog.LevelWarn, numbers...)
		delay := max(backoff(attempt, c.jitter), time.Until(failure.retryAt))
		deadline, _ := ctx.Deadline()
		if delay >= time.Until(deadline) {
			// Do not shorten a server-mandated delay just to squeeze in a retry.
			attemptScope.Event("http.retry_budget_rejected", slog.LevelDebug, numbers...)
			return attemptScope.End(fmt.Errorf("cannot retry within API deadline: %w", contextError(context.DeadlineExceeded)), numbers...)
		}
		attemptScope.Retry(err, numbers...)
		_, retry := diagnostics.Start(ctx, "http", "retry_wait")
		retry.Event("http.retry_scheduled", slog.LevelWarn, diagnostics.N("attempt", attempt+1), diagnostics.N("max_attempts", attempts), diagnostics.N("retry_delay_ms", int(delay.Milliseconds())))
		if err := wait(ctx, delay); err != nil {
			if ctx.Err() != nil {
				return retry.End(contextError(ctx.Err()))
			}
			return retry.End(transportError(ctx, err))
		}
		_ = retry.End(nil)
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
		_, closed := diagnostics.Start(ctx, "http", "response_close")
		if closeErr := resp.Body.Close(); closeErr != nil {
			// A close failure is supplemental and is never silently dropped.
			err = errors.Join(err, closed.End(&RequestError{Category: CategoryClose, StatusCode: status}))
		} else {
			_ = closed.End(nil)
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
