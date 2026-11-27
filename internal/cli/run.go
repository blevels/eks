// Package cli assembles production dependencies and maps errors to exit codes.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"

	"eks/internal/app"
	"eks/internal/config"
	"eks/internal/firecall"
	"eks/internal/kconnect"
	"eks/internal/namespace"
	"eks/internal/terminal"
)

const Usage = `Usage:
  eks [options] list
  eks [options] <namespace-id-or-name>
  eks [options] --id <id>
  eks [options] --name <name>

Options must precede the positional selector.
  --help / --version       Show help or version without connecting
  --env-file PATH          Dotenv file (default .env, optional)
  --once                   Connect once rather than refresh every 900s
  --interval DURATION      Refresh delay; accepts 900s, 15m, or seconds
  --timeout DURATION       Database/HTTP deadline (default 15s)
  --command-timeout VALUE  kconnect deadline (default 2m)
  --json                   JSON Lines results; no interactive prompts
  --verbose                Debug diagnostics on stderr
  --db-host/--db-port/--db-user/--db-name/--db-root-cert VALUE
  --oauth-url/--firecall-url/--firecall-create-url VALUE
  --idp-endpoint/--idp-provider/--region/--email-domain VALUE
  --kconnect PATH          kconnect 0.5.24 executable
  --decrypt-digest VALUE   sha256 (default) or legacy md5
  --ticket VALUE           Change/incident ticket (creation still prompts)
  --duration HOURS         Firecall request duration (default 8)
  --firecall-poll-interval DURATION       Activation check delay (default 5s)
  --firecall-activation-timeout DURATION  Total activation wait (default 2m)

Secrets are read only from environment/dotenv, never flags:
  ENC_USERNAME, ENC_PASSWORD, DECRYPT_PASSPHRASE, PGPASSWORD,
  PROD_PASSWORD (optional noninteractive production password).
Precedence: flags > environment > dotenv > defaults.
Exit codes: 0 success, 1 operational failure, 2 usage/config, 130 interrupt.
See README.md and MIGRATION.md for configuration and service assumptions.
`

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}

func Run(ctx context.Context, args []string, version string, in *os.File, out, stderr io.Writer) (code int) {
	// flag diagnostics can include supplied values; suppress raw flag errors.
	c, err := config.Parse(args, os.LookupEnv, io.Discard)
	if err != nil {
		// Parser errors never include secret values, except unknown flag input
		// supplied by the caller. Do not echo unknown arguments.
		if _, writeErr := fmt.Fprintln(stderr, "Configuration error:", safeError(err, c)); writeErr != nil {
			return 1
		}
		return 2
	}
	if c.Help {
		if _, err := fmt.Fprint(out, Usage); err != nil {
			return 1
		}
		return 0
	}
	if c.Version {
		if _, err := fmt.Fprintln(out, version); err != nil {
			return 1
		}
		return 0
	}
	level := slog.LevelInfo
	if c.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	if !c.List {
		if _, err := exec.LookPath(c.Kconnect); err != nil {
			log.Error("kconnect executable not found")
			return 1
		}
	}
	db, err := namespace.Open(c)
	if err != nil {
		log.Error("could not initialize PostgreSQL connection")
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("close PostgreSQL connection failed")
			if code == 0 {
				code = 1
			}
		}
	}()
	firecalls := firecall.New(c.Timeout, c.OAuthURL, c.FirecallListURL, c.FirecallCreateURL)
	firecalls.Log = log
	namespaces, err := namespace.NewCache(namespace.Store{DB: db, Timeout: c.Timeout}, namespace.DefaultCacheEntries)
	if err != nil {
		log.Error("could not initialize namespace cache")
		return 1
	}
	application := app.App{
		Config: c, Store: namespaces,
		Connector: kconnect.Runner{Binary: c.Kconnect, Timeout: c.CommandTimeout},
		Firecalls: firecalls,
		UI:        &terminal.Terminal{In: in, Out: stderr}, Output: app.Printer{Writer: out, JSON: c.JSON}, Log: log,
	}
	err = application.Run(ctx)
	if err != nil {
		// Operational error chains can include database server text or external
		// service data. Log only a safe top-level classification by boundary.
		if errors.Is(err, context.Canceled) {
			log.Info("interrupted")
			return 130
		}
		var child *kconnect.CommandError
		if errors.As(err, &child) {
			log.Error("kconnect failed", "exit_code", child.Code)
		} else {
			log.Error("operation failed", "error", safeError(err, c))
		}
	}
	return ExitCode(err)
}
