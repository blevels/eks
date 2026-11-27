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

func (a *App) Run(ctx context.Context) error {
	c := a.Config
	mode := c.Mode
	if c.List {
		mode = "list"
	}
	items, err := a.Store.Find(ctx, mode, c.Target)
	if err != nil {
		return err
	}
	if c.List {
		return a.Output.List(items)
	}
	if len(items) == 0 {
		return fmt.Errorf("no data found matching identifier %q", c.Target)
	}
	n := items[0]
	if len(items) > 1 {
		if c.JSON {
			return errors.New("multiple namespaces matched; JSON mode cannot prompt; use a unique selector")
		}
		index, err := a.UI.Choose(ctx, items)
		if err != nil {
			return err
		}
		if index < 0 || index >= len(items) {
			return errors.New("invalid namespace selection")
		}
		n = items[index]
	}
	if err := n.Validate(); err != nil {
		return err
	}
	if n.Environment == "prod" {
		if err := c.ValidateProduction(); err != nil {
			return err
		}
		if c.JSON && c.ProductionPassword == "" {
			return errors.New("JSON mode requires PROD_PASSWORD for production")
		}
	}
	if err := a.Output.Details(n); err != nil {
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
		a.Log.Debug("refreshing EKS connection", "namespace", n.Name)
		username, err := credentials.Decrypt(c.EncryptedUser, c.Passphrase, c.Digest)
		if err != nil {
			return fmt.Errorf("decrypt username: %w", err)
		}
		password, err := credentials.Decrypt(c.EncryptedPassword, c.Passphrase, c.Digest)
		if err != nil {
			return fmt.Errorf("decrypt password: %w", err)
		}
		email := username + "@" + c.EmailDomain
		if n.Environment == "prod" {
			a.Log.Warn("PRODUCTION environment targeted; using production SA identity")
			username = "s" + username
			password = c.ProductionPassword
			if password == "" {
				password, err = a.UI.Password(ctx)
				if err != nil {
					return err
				}
			}
			if password == "" {
				return errors.New("production password cannot be empty")
			}
			if err := a.authorize(ctx, n, username, password, email); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.Connector.Connect(ctx, kconnect.Request{Username: username, Password: password, Namespace: n.Name, ARN: n.ARN(), Region: c.Region, Endpoint: c.IDPEndpoint, Provider: c.IDPProvider}); err != nil {
			return err
		}
		if err := a.Output.Connected(n, now()); err != nil {
			return err
		}
		if c.Once {
			return nil
		}
		a.Log.Info("waiting for next refresh", "interval", c.Interval.String())
		if err := wait(ctx, c.Interval); err != nil {
			return err
		}
	}
}

func (a *App) authorize(ctx context.Context, n namespace.Namespace, username, password, email string) error {
	token, err := a.Firecalls.Token(ctx, username, password)
	if err != nil {
		return err
	}
	active, err := a.Firecalls.List(ctx, token, n.Account, n.ARN())
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return a.Output.Active(active)
	}
	if a.Config.JSON {
		return errors.New("no active firecall; JSON mode will not create one without interactive confirmation")
	}
	yes, err := a.UI.Confirm(ctx)
	if err != nil {
		return err
	}
	if !yes {
		return errors.New("no active firecall; connection cancelled")
	}
	ticket := a.Config.Ticket
	if ticket == "" {
		ticket, err = a.UI.Ticket(ctx)
		if err != nil {
			return err
		}
	}
	created, err := a.Firecalls.Create(ctx, token, n.ARN(), email, ticket, a.Config.Duration)
	if err != nil {
		return err
	}
	if err := a.Output.Created(created); err != nil {
		return err
	}
	return a.waitForActivation(ctx, n, token)
}

// waitForActivation confirms account-and-role authorization, not a specific
// request ID: the current list contract does not expose request IDs.
func (a *App) waitForActivation(ctx context.Context, n namespace.Namespace, token string) error {
	ctx, cancel := context.WithTimeout(ctx, a.Config.FirecallActivationTimeout)
	defer cancel()
	wait := a.Wait
	if wait == nil {
		wait = Wait
	}
	a.Log.Info("waiting for firecall activation", "timeout", a.Config.FirecallActivationTimeout.String())
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for firecall activation: %w", err)
		}
		active, err := a.Firecalls.List(ctx, token, n.Account, n.ARN())
		if err != nil {
			return fmt.Errorf("confirm firecall activation: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for firecall activation: %w", err)
		}
		if len(active) > 0 {
			return a.Output.Active(active)
		}
		if err := wait(ctx, a.Config.FirecallPollInterval); err != nil {
			return fmt.Errorf("waiting for firecall activation: %w", err)
		}
	}
}
