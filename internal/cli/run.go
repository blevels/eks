// Package cli assembles production dependencies and maps errors to exit codes.
package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"

	"eks/internal/app"
	"eks/internal/config"
	"eks/internal/diagnostics"
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
  --log-format text|json    Diagnostic format, independent of --json
  --log-source             Include sanitized caller file/line
  --log-file PATH          Private rotating local logs in addition to stderr
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
	return runWith(ctx, args, version, in, out, stderr, dependencies{lookupPath: exec.LookPath, open: namespace.Open})
}

type dependencies struct {
	lookupPath func(string) (string, error)
	open       func(config.Config) (*sql.DB, error)
	lookupEnv  func(string) (string, bool)
}

func runWith(ctx context.Context, args []string, version string, in *os.File, out, stderr io.Writer, deps dependencies) (code int) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bootstrap := &diagnostics.Bootstrap{}
	d := &diagnostics.Diagnostics{Log: slog.New(bootstrap)}
	ctx = diagnostics.With(ctx, d)
	ctx, run := diagnostics.Start(ctx, "cli", "run")
	run.Event("run.invocation", slog.LevelInfo)
	var primary error
	sink, _ := diagnostics.NewSink(stderr, "", cancel)
	var finalLogger *slog.Logger
	installLogger := func(format string, debug, source bool) {
		d.Log, finalLogger = diagnostics.NewLoggers(sink, stderr, format, debug, source, version)
		emergency := finalLogger
		sink.SetAlert(func() error { return diagnostics.ReportSinkFailure(ctx, emergency) })
	}
	defer func() {
		run.Event("run.shutdown", slog.LevelInfo)
		if sink != nil {
			_ = sink.Close()
			if sink.Failed() {
				code = 1
				if primary == nil || errors.Is(primary, context.Canceled) {
					primary = sink.Failure()
				}
				// A broken file must not suppress the final stderr summary.
				d.Log = finalLogger
			}
		}
		// File shutdown precedes the final stderr summary so its exit code
		// includes flush/close failures. The shutdown event is retained in file.
		_ = run.End(primary, diagnostics.N("exit_code", code))
		if sink != nil && sink.Failed() {
			code = 1
		}
	}()
	// No flag parser text, partially parsed config, or raw error enters a sink.
	lookupEnv := deps.lookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	c, err := config.ParseContext(ctx, args, lookupEnv, io.Discard)
	if err != nil {
		installLogger("text", false, false)
		bootstrap.Replay(ctx, d.Log)
		primary = err
		return 2
	}
	configuredSink, err := diagnostics.NewSink(stderr, c.LogFile, cancel)
	if err != nil {
		installLogger("text", false, false)
		bootstrap.Replay(ctx, d.Log)
		_, setup := diagnostics.Start(ctx, "cli", "log_sink")
		primary = setup.End(err)
		return 1
	}
	sink = configuredSink
	installLogger(c.LogFormat, c.Verbose, c.LogSource)
	bootstrap.Replay(ctx, d.Log)
	if c.Help {
		_, output := diagnostics.Start(ctx, "output", "output_help")
		_, err := fmt.Fprint(out, Usage)
		if primary = output.End(err); primary != nil {
			return 1
		}
		return 0
	}
	if c.Version {
		_, output := diagnostics.Start(ctx, "output", "output_version")
		_, err := fmt.Fprintln(out, version)
		if primary = output.End(err); primary != nil {
			return 1
		}
		return 0
	}
	if !c.List {
		_, dependency := diagnostics.Start(ctx, "cli", "dependency")
		if _, err := deps.lookupPath(c.Kconnect); err != nil {
			primary = dependency.End(err)
			return 1
		}
		_ = dependency.End(nil)
	}
	_, driver := diagnostics.Start(ctx, "cli", "driver")
	db, err := deps.open(c)
	err = driver.End(err)
	if err != nil {
		primary = err
		return 1
	}
	defer func() {
		_, closeScope := diagnostics.Start(ctx, "cli", "database_close")
		if err := closeScope.End(db.Close()); err != nil {
			if code == 0 {
				code = 1
				primary = err
			}
		}
	}()
	firecalls := firecall.New(c.Timeout, c.OAuthURL, c.FirecallListURL, c.FirecallCreateURL)
	namespaces, err := namespace.NewCache(namespace.Store{DB: db, Timeout: c.Timeout}, namespace.DefaultCacheEntries)
	if err != nil {
		_, setup := diagnostics.Start(ctx, "cli", "cache_setup")
		primary = setup.End(err)
		return 1
	}
	namespaces.Diagnostics = d
	application := app.App{
		Config: c, Store: namespaces,
		Connector: kconnect.Runner{Binary: c.Kconnect, Timeout: c.CommandTimeout},
		Firecalls: firecalls,
		UI:        &terminal.Terminal{In: in, Out: stderr}, Output: app.Printer{Writer: out, JSON: c.JSON},
	}
	err = application.Run(ctx)
	primary = err
	return ExitCode(err)
}
