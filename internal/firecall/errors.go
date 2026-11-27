package firecall

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

// ErrorCategory is a fixed, secret-free description of a failed HTTP boundary.
type ErrorCategory string

const (
	CategoryCanceled      ErrorCategory = "canceled"
	CategoryTimeout       ErrorCategory = "timeout"
	CategoryTLS           ErrorCategory = "tls_validation"
	CategoryNetwork       ErrorCategory = "network"
	CategoryHTTP          ErrorCategory = "http_status"
	CategoryInvalidJSON   ErrorCategory = "invalid_json"
	CategoryResponseLimit ErrorCategory = "response_limit"
	CategoryRead          ErrorCategory = "response_read"
	CategoryClose         ErrorCategory = "response_close"
	CategoryRequest       ErrorCategory = "invalid_request"
)

// RequestError intentionally retains no raw transport error, URL, header, or
// body. Unwrap exposes only safe context sentinels, never a credential-bearing
// *url.Error or remote error text. Inspect Category/StatusCode for diagnostics.
type RequestError struct {
	Category   ErrorCategory
	StatusCode int

	retryable bool
	retryAt   time.Time
	cause     error
}

func (e *RequestError) Error() string {
	switch e.Category {
	case CategoryHTTP:
		return fmt.Sprintf("API returned HTTP %d", e.StatusCode)
	case CategoryCanceled:
		return "API request canceled"
	case CategoryTimeout:
		return "API request timed out or retry delay exceeds remaining deadline"
	case CategoryTLS:
		return "API TLS validation failed"
	case CategoryNetwork:
		return "API network request failed"
	case CategoryInvalidJSON:
		return "API returned invalid or unexpected JSON"
	case CategoryResponseLimit:
		return "API response exceeds 1 MiB"
	case CategoryRead:
		return "could not read API response"
	case CategoryClose:
		return "could not close API response"
	default:
		return "invalid API request configuration or endpoint"
	}
}

func (e *RequestError) Unwrap() error              { return e.cause }
func (e *RequestError) DiagnosticError() error     { return e }
func (e *RequestError) DiagnosticCategory() string { return string(e.Category) }
func (e *RequestError) Timeout() bool              { return e.Category == CategoryTimeout }

// Temporary implements net.Error without exposing the underlying network error.
func (e *RequestError) Temporary() bool { return e.retryable }

func contextError(err error) *RequestError {
	if errors.Is(err, context.Canceled) {
		return &RequestError{Category: CategoryCanceled, cause: context.Canceled}
	}
	return &RequestError{Category: CategoryTimeout, cause: context.DeadlineExceeded}
}

func transportError(ctx context.Context, err error) *RequestError {
	if ctx.Err() != nil {
		return contextError(ctx.Err())
	}
	if errors.Is(err, context.Canceled) {
		return contextError(context.Canceled)
	}
	var verification *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &verification) || errors.As(err, &unknown) ||
		errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &record) || errors.As(err, &alert) {
		return &RequestError{Category: CategoryTLS}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return &RequestError{Category: CategoryTimeout, cause: context.DeadlineExceeded, retryable: true}
	}
	// Do not retry permanent DNS failures such as NXDOMAIN.
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return &RequestError{Category: CategoryNetwork, retryable: !dns.IsNotFound && dns.IsTemporary}
	}
	transient := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	for _, code := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		transient = transient || errors.Is(err, code)
	}
	// net.Error.Temporary is deprecated and too broad for a fail-closed policy.
	// Retry only the explicit connection failures and DNS/timeout cases above.
	return &RequestError{Category: CategoryNetwork, retryable: transient}
}
