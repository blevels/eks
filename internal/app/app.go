// Package app orchestrates the POC workflow using injected I/O boundaries.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"eks/internal/config"
	"eks/internal/credentials"
	"eks/internal/diagnostics"
	"eks/internal/firecall"
	"eks/internal/kconnect"
	"eks/internal/namespace"
)

type Finder interface {
	Find(context.Context, string, string) ([]namespace.Namespace, error)
}
type Connector interface {
	Connect(context.Context, kconnect.Request) error
}
type Firecalls interface {
	Token(context.Context, string, string) (string, error)
	List(context.Context, string, string, string) ([]firecall.Active, error)
	Create(context.Context, string, string, string, string, int) ([]firecall.Created, error)
}
type Interaction interface {
	Choose(context.Context, []namespace.Namespace) (int, error)
	Password(context.Context) (string, error)
	Confirm(context.Context) (bool, error)
	Ticket(context.Context) (string, error)
}
type Output interface {
	List([]namespace.Namespace) error
	Details(namespace.Namespace) error
	Active([]firecall.Active) error
	Created([]firecall.Created) error
	Connected(namespace.Namespace, time.Time) error
}

type App struct {
	Config    config.Config
	Store     Finder
	Connector Connector
	Firecalls Firecalls
	UI        Interaction
	Output    Output
	Log       *slog.Logger
	Wait      func(context.Context, time.Duration) error
	Now       func() time.Time
}

func Wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// observe also covers injected fake boundaries without coupling pure helpers to logging.
func observe[T any](ctx context.Context, component, operation string, f func(context.Context) (T, error)) (value T, err error) {
	ctx, scope := diagnostics.Start(ctx, component, operation)
	value, err = f(ctx)
	return value, scope.End(err)
}
func action(ctx context.Context, component, operation string, f func(context.Context) error) error {
	_, err := observe(ctx, component, operation, func(ctx context.Context) (struct{}, error) { return struct{}{}, f(ctx) })
	return err
}
func failed(ctx context.Context, component, operation string, err error) error {
	_, scope := diagnostics.Start(ctx, component, operation)
	return scope.End(err)
}

func (a *App) Run(ctx context.Context) (err error) {
	ctx = diagnostics.WithLogger(ctx, a.Log)
	ctx = diagnostics.Cycle(ctx)
	ctx, workflow := diagnostics.Start(ctx, "workflow", "workflow")
	defer func() { err = workflow.End(err) }()
	c := a.Config
	mode := c.Mode
	if c.List {
		mode = "list"
	}
	items, err := observe(ctx, "workflow", "selection", func(ctx context.Context) ([]namespace.Namespace, error) { return a.Store.Find(ctx, mode, c.Target) })
	if err != nil {
		return err
	}
	if c.List {
		return action(ctx, "output", "output_list", func(context.Context) error { return a.Output.List(items) })
	}
	if len(items) == 0 {
		return failed(ctx, "workflow", "selection_validation", errors.New("no matching data"))
	}
	n := items[0]
	if len(items) > 1 {
		if c.JSON {
			return failed(ctx, "workflow", "selection_validation", errors.New("multiple namespaces matched; JSON mode cannot prompt; use a unique selector"))
		}
		index, err := observe(ctx, "terminal", "selection", func(ctx context.Context) (int, error) { return a.UI.Choose(ctx, items) })
		if err != nil {
			return err
		}
		if index < 0 || index >= len(items) {
			return failed(ctx, "terminal", "selection_validation", errors.New("invalid namespace selection"))
		}
		n = items[index]
	}
	if err := n.Validate(); err != nil {
		return failed(ctx, "workflow", "selection_validation", err)
	}
	if n.Environment == "prod" {
		if err := c.ValidateProduction(); err != nil {
			return failed(ctx, "workflow", "production_config", err)
		}
		if c.JSON && c.ProductionPassword == "" {
			return failed(ctx, "workflow", "password_validation", errors.New("JSON mode requires PROD_PASSWORD for production"))
		}
	}
	if err := action(ctx, "output", "output_details", func(context.Context) error { return a.Output.Details(n) }); err != nil {
		return err
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	wait := a.Wait
	if wait == nil {
		wait = Wait
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.refresh(ctx, n, c, now); err != nil {
			return err
		}
		if c.Once {
			return nil
		}
		waitCtx, waiting := diagnostics.Start(ctx, "workflow", "refresh_wait")
		waiting.Event("refresh.wait", slog.LevelInfo)
		if err := waiting.End(wait(waitCtx, c.Interval)); err != nil {
			return err
		}
		ctx = diagnostics.Cycle(ctx)
	}
}

func (a *App) refresh(ctx context.Context, n namespace.Namespace, c config.Config, now func() time.Time) (err error) {
	ctx, refresh := diagnostics.Start(ctx, "workflow", "refresh")
	connected := false
	defer func() {
		if err != nil && !connected {
			refresh.Event("connection.blocked", slog.LevelInfo)
		}
		err = refresh.End(err)
	}()
	username, err := observe(ctx, "credentials", "decrypt_user", func(context.Context) (string, error) {
		return credentials.Decrypt(c.EncryptedUser, c.Passphrase, c.Digest)
	})
	if err != nil {
		return fmt.Errorf("decrypt username: %w", err)
	}
	password, err := observe(ctx, "credentials", "decrypt_password", func(context.Context) (string, error) {
		return credentials.Decrypt(c.EncryptedPassword, c.Passphrase, c.Digest)
	})
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}
	email := username + "@" + c.EmailDomain
	if n.Environment == "prod" {
		username = "s" + username
		password = c.ProductionPassword
		if password == "" {
			password, err = observe(ctx, "terminal", "password", a.UI.Password)
			if err != nil {
				return err
			}
		}
		if password == "" {
			return failed(ctx, "workflow", "password_validation", errors.New("production password cannot be empty"))
		}
		if err := a.authorize(ctx, n, username, password, email); err != nil {
			return err
		}
	} else {
		refresh.Event("authorization.allowed_nonproduction", slog.LevelInfo)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	refresh.Event("connection.permitted", slog.LevelInfo)
	if err := action(ctx, "workflow", "connection", func(ctx context.Context) error {
		return a.Connector.Connect(ctx, kconnect.Request{Username: username, Password: password, Namespace: n.Name, ARN: n.ARN(), Region: c.Region, Endpoint: c.IDPEndpoint, Provider: c.IDPProvider})
	}); err != nil {
		return err
	}
	connected = true
	if err := action(ctx, "output", "output_connected", func(context.Context) error { return a.Output.Connected(n, now()) }); err != nil {
		return err
	}
	return nil
}

func (a *App) authorize(ctx context.Context, n namespace.Namespace, username, password, email string) (err error) {
	ctx, authorization := diagnostics.Start(ctx, "workflow", "authorization")
	defer func() {
		if err != nil {
			authorization.Event("authorization.stopped", slog.LevelInfo)
		}
		err = authorization.End(err)
	}()
	token, err := observe(ctx, "http", "oauth_token", func(ctx context.Context) (string, error) { return a.Firecalls.Token(ctx, username, password) })
	if err != nil {
		return err
	}
	active, err := observe(ctx, "http", "list_firecalls", func(ctx context.Context) ([]firecall.Active, error) {
		return a.Firecalls.List(ctx, token, n.Account, n.ARN())
	})
	if err != nil {
		return err
	}
	if len(active) > 0 {
		authorization.Event("authorization.allowed_active", slog.LevelInfo)
		return action(ctx, "output", "output_active", func(context.Context) error { return a.Output.Active(active) })
	}
	if a.Config.JSON {
		authorization.Event("authorization.blocked_noninteractive", slog.LevelInfo)
		return errors.New("no active firecall; JSON mode will not create one without interactive confirmation")
	}
	yes, err := observe(ctx, "terminal", "confirmation", a.UI.Confirm)
	if err != nil {
		return err
	}
	if !yes {
		authorization.Event("authorization.blocked_declined", slog.LevelInfo)
		return diagnostics.Declined()
	}
	ticket := a.Config.Ticket
	if ticket == "" {
		ticket, err = observe(ctx, "terminal", "ticket", a.UI.Ticket)
		if err != nil {
			return err
		}
	}
	created, err := observe(ctx, "http", "create_firecall", func(ctx context.Context) ([]firecall.Created, error) {
		return a.Firecalls.Create(ctx, token, n.ARN(), email, ticket, a.Config.Duration)
	})
	if err != nil {
		return err
	}
	if err := action(ctx, "output", "output_created", func(context.Context) error { return a.Output.Created(created) }); err != nil {
		return err
	}
	return a.waitForActivation(ctx, n, token)
}

// waitForActivation confirms account-and-role authorization, not a specific
// request ID: the current list contract does not expose request IDs.
func (a *App) waitForActivation(ctx context.Context, n namespace.Namespace, token string) (err error) {
	ctx, activation := diagnostics.Start(ctx, "workflow", "activation")
	defer func() { err = activation.End(err) }()
	ctx, cancel := context.WithTimeout(ctx, a.Config.FirecallActivationTimeout)
	defer cancel()
	wait := a.Wait
	if wait == nil {
		wait = Wait
	}
	activation.Event("activation.wait", slog.LevelInfo)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for firecall activation: %w", err)
		}
		activation.Event("activation.poll", slog.LevelDebug)
		active, err := observe(ctx, "http", "list_firecalls", func(ctx context.Context) ([]firecall.Active, error) {
			return a.Firecalls.List(ctx, token, n.Account, n.ARN())
		})
		if err != nil {
			return fmt.Errorf("confirm firecall activation: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for firecall activation: %w", err)
		}
		if len(active) > 0 {
			activation.Event("authorization.allowed_activated", slog.LevelInfo)
			return action(ctx, "output", "output_active", func(context.Context) error { return a.Output.Active(active) })
		}
		if err := wait(ctx, a.Config.FirecallPollInterval); err != nil {
			return fmt.Errorf("waiting for firecall activation: %w", err)
		}
	}
}
