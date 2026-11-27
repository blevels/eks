// Package diagnostics provides explicitly injected, secret-safe operational scopes.
package diagnostics

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

type key struct{}
type scopeKey struct{}
type cycleKey struct{}

type Diagnostics struct {
	Log *slog.Logger
}

// With explicitly installs diagnostics for this call tree, not process-global state.
func With(ctx context.Context, d *Diagnostics) context.Context {
	return context.WithValue(ctx, key{}, d)
}

// WithLogger adapts an explicitly injected legacy slog logger when no shared
// call-tree context was supplied. New application assembly uses With directly.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	if d, _ := ctx.Value(key{}).(*Diagnostics); d != nil || log == nil {
		return ctx
	}
	if _, ok := log.Handler().(*normalizedHandler); ok {
		return With(ctx, &Diagnostics{Log: log})
	}
	return With(ctx, &Diagnostics{Log: log.With("schema_version", 1, "run_id", ID(), "version", "unknown", "pid", processID())})
}

func ID() string         { return fmt.Sprintf("%x", randomID()) }
func randomID() [16]byte { var b [16]byte; _, _ = rand.Read(b[:]); return b }

func Cycle(ctx context.Context) context.Context {
	return context.WithValue(ctx, cycleKey{}, ID())
}

type Scope struct {
	ctx                              context.Context
	component, operation, id, parent string
	start                            time.Time
}

func Start(ctx context.Context, component, operation string) (context.Context, *Scope) {
	switch component {
	case "cli", "config", "database", "cache", "credentials", "terminal", "http", "workflow", "filesystem", "subprocess", "output":
	default:
		component = "cli"
	}
	switch operation {
	case "run", "dependency", "driver", "database_close", "log_sink", "cache_setup",
		"parse", "dotenv_open", "dotenv_read", "dotenv_close", "query", "query_plan", "query_scan", "query_validation", "query_iteration", "rows_close",
		"cache_lookup", "cache_wait", "cache_clear", "cache_validation",
		"decrypt_user", "decrypt_password", "selection", "password", "confirmation", "ticket", "restore",
		"oauth_token", "list_firecalls", "create_firecall", "http_attempt", "retry_wait", "response_close",
		"workflow", "refresh", "authorization", "activation", "connection", "refresh_wait", "selection_validation", "production_config", "password_validation",
		"private_directory", "directory_protection", "credential_file", "credential_encode", "cleanup", "kconnect", "subprocess",
		"output_list", "output_details", "output_active", "output_created", "output_connected", "output_help", "output_version":
	default:
		operation = "workflow"
	}
	parent, _ := ctx.Value(scopeKey{}).(string)
	s := &Scope{ctx: ctx, component: component, operation: operation, id: ID(), parent: parent, start: time.Now()}
	s.emit(slog.LevelDebug, operation+".start", nil)
	return context.WithValue(ctx, scopeKey{}, s.id), s
}

// Number is the only public payload attribute: arbitrary strings cannot enter logs.
type Number struct {
	Key   string
	Value int64
}

func N(key string, value int) Number { return Number{key, int64(value)} }

func (s *Scope) emit(level slog.Level, event string, extra []slog.Attr) {
	d, _ := s.ctx.Value(key{}).(*Diagnostics)
	if d == nil || d.Log == nil || !d.Log.Enabled(s.ctx, level) {
		return
	}
	cycle, _ := s.ctx.Value(cycleKey{}).(string)
	attrs := []slog.Attr{slog.String("event", event), slog.String("component", s.component),
		slog.String("operation", s.operation), slog.String("operation_id", s.id),
		slog.String("parent_operation_id", s.parent), slog.String("cycle_id", cycle)}
	attrs = append(attrs, extra...)
	// Skip both shared and application observation wrappers. A fixed skip alone
	// would attribute every output/decryption event to observe rather than its
	// operational call site. Frames handles compiler-inlined wrappers as well.
	var pc uintptr
	h, normalized := d.Log.Handler().(*normalizedHandler)
	_, bootstrap := d.Log.Handler().(*Bootstrap)
	if bootstrap || (normalized && h.source) {
		var pcs [16]uintptr
		n := runtime.Callers(2, pcs[:])
		frames := runtime.CallersFrames(pcs[:n])
		for {
			frame, more := frames.Next()
			fn := frame.Function
			helper := strings.HasPrefix(fn, "eks/internal/diagnostics.(*Scope).") ||
				fn == "eks/internal/diagnostics.Start" ||
				strings.HasPrefix(fn, "eks/internal/app.observe[") ||
				fn == "eks/internal/app.action" || fn == "eks/internal/app.failed"
			if !helper {
				pc = frame.PC + 1
				break
			}
			if !more {
				break
			}
		}
	}
	r := slog.NewRecord(time.Now().UTC(), level, "Application diagnostic", pc)
	r.AddAttrs(attrs...)
	_ = d.Log.Handler().Handle(s.ctx, r)
}

func (s *Scope) Event(event string, level slog.Level, numbers ...Number) {
	switch event {
	case "cache.hit", "cache.miss", "cache.wait", "cache.fill", "cache.empty", "cache.eviction", "cache.clear", "cache.suppressed",
		"http.retryable_attempt", "http.retry_scheduled", "http.retry_budget_rejected",
		"terminal.invalid_selection", "terminal.invalid_confirmation",
		"authorization.allowed_active", "authorization.blocked_noninteractive", "authorization.blocked_declined",
		"authorization.allowed_activated", "authorization.stopped", "connection.permitted", "connection.blocked",
		"authorization.allowed_nonproduction", "refresh.wait", "activation.wait", "activation.poll", "run.invocation", "run.shutdown":
	default:
		return
	}
	s.emit(level, event, numeric(numbers))
}
func numeric(numbers []Number) []slog.Attr {
	var attrs []slog.Attr
	for _, n := range numbers {
		switch n.Key {
		case "attempt", "max_attempts", "http_status", "retry_delay_ms", "child_exit_code", "row_count", "exit_code":
			attrs = append(attrs, slog.Int64(n.Key, n.Value))
		}
	}
	return attrs
}

// Failure exposes only a fixed code and context sentinels through its error chain.
// Is delegates identity checks without exposing unsafe original Error strings.
type Failure struct {
	Code, Category, OperationID string
	original                    error
}

func (e *Failure) Error() string        { return e.Code }
func (e *Failure) Is(target error) bool { return errors.Is(e.original, target) }

// Only error types explicitly certifying a payload-free view support As.
type SafeError interface{ DiagnosticError() error }

func (e *Failure) As(target any) bool {
	var safe SafeError
	if errors.As(e.original, &safe) {
		return errors.As(safe.DiagnosticError(), target)
	}
	return false
}

func primaryOwned(err error) *Failure {
	if err == nil {
		return nil
	}
	if f, ok := err.(*Failure); ok {
		return f
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		items := joined.Unwrap()
		if len(items) > 0 {
			return primaryOwned(items[0])
		}
		return nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return primaryOwned(wrapped.Unwrap())
	}
	return nil
}

func Category(err error) string {
	if owned := primaryOwned(err); owned != nil {
		return owned.Category
	}
	// Joined supplemental cleanup errors must not reclassify a primary cause.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		if items := joined.Unwrap(); len(items) > 0 {
			return Category(items[0])
		}
	}
	if _, categorized := err.(interface{ DiagnosticCategory() string }); !categorized {
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			return Category(wrapped.Unwrap())
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF):
		return "eof"
	default:
		var categorized interface{ DiagnosticCategory() string }
		if errors.As(err, &categorized) {
			switch c := categorized.DiagnosticCategory(); c {
			case "tls_validation", "network", "http_status", "invalid_json", "response_limit", "response_read", "response_close", "invalid_request", "declined":
				return c
			}
		}
		return "operational"
	}
}

func boundaryCategory(component, operation string) string {
	switch operation {
	case "query_plan", "query_scan", "query_validation", "cache_validation", "selection_validation", "password_validation":
		return "validation"
	case "production_config":
		return "configuration"
	}
	switch component {
	case "config":
		return "configuration"
	case "database":
		return "database"
	case "credentials":
		return "credentials"
	case "terminal":
		return "terminal"
	case "filesystem":
		return "filesystem"
	case "subprocess":
		return "subprocess"
	case "output":
		return "output"
	case "cache":
		return "cache"
	case "workflow":
		return "workflow"
	case "http":
		return "response_validation"
	default:
		return "setup"
	}
}

// Retry is a completed failed attempt that will be retried, not an owned
// terminal error. It records the safe failure category and duration at WARN.
func (s *Scope) Retry(err error, numbers ...Number) {
	category := Category(err)
	attrs := numeric(numbers)
	attrs = append(attrs, slog.String("status", "failure"), slog.String("error_code", s.operation+"_failed"),
		slog.String("error_category", category), slog.Float64("duration_ms", float64(time.Since(s.start))/float64(time.Millisecond)))
	s.emit(slog.LevelWarn, s.operation+".outcome", attrs)
}

// End returns an owned safe failure. Propagating an already-owned failure emits
// no duplicate terminal failure; run alone emits a summary referencing its owner.
func (s *Scope) End(err error, numbers ...Number) error {
	attrs := numeric(numbers)
	status, level := "success", slog.LevelDebug
	if s.operation == "run" || s.operation == "connection" {
		level = slog.LevelInfo
	}
	var owned *Failure
	if err != nil {
		category := Category(err)
		if category == "operational" {
			category = boundaryCategory(s.component, s.operation)
		}
		status, level = "failure", slog.LevelError
		if category == "canceled" {
			status, level = "canceled", slog.LevelInfo
		}
		if category == "timeout" {
			status = "timeout"
		}
		if category == "declined" {
			status, level = "canceled", slog.LevelInfo
		}
		owned = primaryOwned(err)
		if owned != nil && s.operation != "run" {
			// A completion reference is not a second primary failure.
			attrs = append(attrs, slog.String("status", status), slog.String("failed_operation_id", owned.OperationID),
				slog.String("error_code", owned.Code), slog.String("error_category", owned.Category),
				slog.Float64("duration_ms", float64(time.Since(s.start))/float64(time.Millisecond)))
			level := slog.LevelDebug
			if s.operation == "connection" {
				level = slog.LevelInfo
			}
			s.emit(level, s.operation+".outcome", attrs)
			return err
		}
		if owned == nil {
			owned = &Failure{Code: s.operation + "_failed", Category: category, OperationID: s.id, original: err}
		}
		attrs = append(attrs, slog.String("error_code", owned.Code), slog.String("error_category", owned.Category))
		if s.operation == "run" {
			attrs = append(attrs, slog.String("failed_operation_id", owned.OperationID))
		}
	}
	attrs = append(attrs, slog.String("status", status), slog.Float64("duration_ms", float64(time.Since(s.start))/float64(time.Millisecond)))
	s.emit(level, s.operation+".outcome", attrs)
	if err != nil {
		return owned
	}
	return nil
}

type declined struct{}

func (declined) Error() string              { return "authorization_declined" }
func (declined) DiagnosticCategory() string { return "declined" }

// Declined preserves operational exit 1 while treating a user's refusal as
// normal cancellation rather than an error-level incident.
func Declined() error { return declined{} }

// NewLogger always normalizes metadata and source paths at the handler boundary.
func NewLogger(w io.Writer, format string, debug, source bool, version string) *slog.Logger {
	return newLogger(w, format, debug, source, version, ID())
}

// NewLoggers shares immutable run metadata with a nonrecursive emergency logger.
func NewLoggers(w, emergency io.Writer, format string, debug, source bool, version string) (*slog.Logger, *slog.Logger) {
	id := ID()
	return newLogger(w, format, debug, source, version, id), newLogger(emergency, format, false, false, version, id)
}

func ReportSinkFailure(ctx context.Context, log *slog.Logger) error {
	ctx = With(ctx, &Diagnostics{Log: log})
	parent, _ := ctx.Value(scopeKey{}).(string)
	scope := &Scope{ctx: ctx, component: "cli", operation: "log_sink", id: ID(), parent: parent, start: time.Now()}
	return scope.End(errors.New("log_sink_failed"), N("exit_code", 1))
}

func newLogger(w io.Writer, format string, debug, source bool, version, runID string) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: source, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Time(a.Key, a.Value.Time().UTC())
		}
		if a.Key == slog.SourceKey {
			if s, ok := a.Value.Any().(*slog.Source); ok {
				file := strings.ReplaceAll(s.File, "\\", "/")
				if i := strings.LastIndex(file, "/internal/"); i >= 0 {
					file = file[i+1:]
				} else if i := strings.LastIndex(file, "/cmd/"); i >= 0 {
					file = file[i+1:]
				} else {
					file = "external"
				}
				return slog.Group("source", slog.String("file", file), slog.Int("line", s.Line))
			}
		}
		return a
	}}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	if format == "json" {
		h = slog.NewJSONHandler(w, opts)
	}
	// Version comes from the build, but still constrain it to a safe bounded token.
	if len(version) > 80 || strings.ContainsFunc(version, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r))
	}) {
		version = "unknown"
	}
	return slog.New(&normalizedHandler{Handler: h, source: source}).With("schema_version", 1, "run_id", runID, "version", version, "pid", processID())
}

// Preserve the source setting across slog's immutable With operations. This
// avoids stack walking entirely when source attribution was not requested.
type normalizedHandler struct {
	slog.Handler
	source bool
}

func (h *normalizedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &normalizedHandler{Handler: h.Handler.WithAttrs(attrs), source: h.source}
}
func (h *normalizedHandler) WithGroup(name string) slog.Handler {
	return &normalizedHandler{Handler: h.Handler.WithGroup(name), source: h.source}
}
