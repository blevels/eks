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

// Wait pauses for d or until ctx is canceled, returning the context error on
// cancellation and nil when the timer expires.
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

// action runs f with an operation context and returns its diagnostic-owned
// error, or nil on success. It keeps one-call output boundaries concise.
func action(ctx context.Context, component, operation string, f func(context.Context) error) error {
	ctx, scope := diagnostics.Start(ctx, component, operation)
	return scope.End(f(ctx))
}

// failed records an immediate validation error under component and operation,
// returning the diagnostic-owned error without adding another workflow step.
func failed(ctx context.Context, component, operation string, err error) error {
	_, scope := diagnostics.Start(ctx, component, operation)
	return scope.End(err)
}

// Run selects a namespace using Config and runs connection refreshes until Once,
// cancellation, or failure. List only prints matches. Production connections
// require authorization; errors preserve their diagnostic owner and cause.
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
	selectionCtx, selection := diagnostics.Start(ctx, "workflow", "selection")
	items, err := a.Store.Find(selectionCtx, mode, c.Target)
	if err = selection.End(err); err != nil {
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
		choiceCtx, choice := diagnostics.Start(ctx, "terminal", "selection")
		index, err := a.UI.Choose(choiceCtx, items)
		if err = choice.End(err); err != nil {
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

// refresh decrypts c's credentials, authorizes production access to n, connects,
// and prints success using now. It returns the first boundary error and reports
// a blocked connection only if the connector has not succeeded.
func (a *App) refresh(ctx context.Context, n namespace.Namespace, c config.Config, now func() time.Time) (err error) {
	ctx, refresh := diagnostics.Start(ctx, "workflow", "refresh")
	connected := false
	defer func() {
		if err != nil && !connected {
			refresh.Event("connection.blocked", slog.LevelInfo)
		}
		err = refresh.End(err)
	}()
	_, decryptUser := diagnostics.Start(ctx, "credentials", "decrypt_user")
	username, err := credentials.Decrypt(c.EncryptedUser, c.Passphrase, c.Digest)
	if err = decryptUser.End(err); err != nil {
		return fmt.Errorf("decrypt username: %w", err)
	}
	_, decryptPassword := diagnostics.Start(ctx, "credentials", "decrypt_password")
	password, err := credentials.Decrypt(c.EncryptedPassword, c.Passphrase, c.Digest)
	if err = decryptPassword.End(err); err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}
	email := username + "@" + c.EmailDomain
	if n.Environment == "prod" {
		username = "s" + username
		password = c.ProductionPassword
		if password == "" {
			passwordCtx, prompt := diagnostics.Start(ctx, "terminal", "password")
			password, err = a.UI.Password(passwordCtx)
			if err = prompt.End(err); err != nil {
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
	connectionCtx, connection := diagnostics.Start(ctx, "workflow", "connection")
	err = a.Connector.Connect(connectionCtx, kconnect.Request{
		Username: username, Password: password, Namespace: n.Name, ARN: n.ARN(),
		Region: c.Region, Endpoint: c.IDPEndpoint, Provider: c.IDPProvider,
	})
	if err = connection.End(err); err != nil {
		return err
	}
	connected = true
	if err := action(ctx, "output", "output_connected", func(context.Context) error { return a.Output.Connected(n, now()) }); err != nil {
		return err
	}
	return nil
}

// authorize uses username and password to verify access to n's account and role.
// If needed, it confirms creation interactively using the base identity's email
// and waits for activation. It returns errors or a decline without connecting.
func (a *App) authorize(ctx context.Context, n namespace.Namespace, username, password, email string) (err error) {
	ctx, authorization := diagnostics.Start(ctx, "workflow", "authorization")
	defer func() {
		if err != nil {
			authorization.Event("authorization.stopped", slog.LevelInfo)
		}
		err = authorization.End(err)
	}()
	tokenCtx, oauth := diagnostics.Start(ctx, "http", "oauth_token")
	token, err := a.Firecalls.Token(tokenCtx, username, password)
	if err = oauth.End(err); err != nil {
		return err
	}
	listCtx, listing := diagnostics.Start(ctx, "http", "list_firecalls")
	active, err := a.Firecalls.List(listCtx, token, n.Account, n.ARN())
	if err = listing.End(err); err != nil {
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
	confirmationCtx, confirmation := diagnostics.Start(ctx, "terminal", "confirmation")
	yes, err := a.UI.Confirm(confirmationCtx)
	if err = confirmation.End(err); err != nil {
		return err
	}
	if !yes {
		authorization.Event("authorization.blocked_declined", slog.LevelInfo)
		return diagnostics.Declined()
	}
	ticket := a.Config.Ticket
	if ticket == "" {
		ticketCtx, prompt := diagnostics.Start(ctx, "terminal", "ticket")
		ticket, err = a.UI.Ticket(ticketCtx)
		if err = prompt.End(err); err != nil {
			return err
		}
	}
	createCtx, creation := diagnostics.Start(ctx, "http", "create_firecall")
	created, err := a.Firecalls.Create(createCtx, token, n.ARN(), email, ticket, a.Config.Duration)
	if err = creation.End(err); err != nil {
		return err
	}
	if err := action(ctx, "output", "output_created", func(context.Context) error { return a.Output.Created(created) }); err != nil {
		return err
	}
	return a.waitForActivation(ctx, n, token)
}

// waitForActivation polls for n's account-and-role authorization using token,
// printing active access when found. The list contract exposes no request IDs.
// It returns lookup/output errors or cancellation/timeout errors within Config's
// activation deadline; it never retries a failed lookup itself.
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
		listCtx, listing := diagnostics.Start(ctx, "http", "list_firecalls")
		active, err := a.Firecalls.List(listCtx, token, n.Account, n.ARN())
		if err = listing.End(err); err != nil {
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
