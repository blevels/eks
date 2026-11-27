// Package config implements flags > environment > dotenv > defaults.
package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"eks/internal/diagnostics"
)

// Config contains resolved CLI settings. Duration fields use time.Duration;
// Duration is the requested firecall lifetime in hours. Secret fields must not
// be logged. A Config returned by a parser is valid only when its error is nil.
type Config struct {
	LogFormat, LogFile                                                       string
	LogSource                                                                bool
	Target, Mode, EnvFile                                                    string
	List, Once, JSON, Verbose, Help, Version                                 bool
	DBHost, DBPort, DBUser, DBName, DBPassword, DBRootCert                   string
	EncryptedUser, EncryptedPassword, Passphrase, Digest, ProductionPassword string
	OAuthURL, FirecallListURL, FirecallCreateURL, IDPEndpoint                string
	Region, IDPProvider, EmailDomain, Kconnect, Ticket                       string
	Interval, Timeout, CommandTimeout                                        time.Duration
	FirecallPollInterval, FirecallActivationTimeout                          time.Duration
	Duration                                                                 int
}

// Parse resolves args, lookup, and the selected dotenv file into a validated
// Config without modifying the process environment. Lookup has os.LookupEnv
// semantics, including explicitly empty values. It returns an error for invalid
// options, file failures, or invalid settings. stderr is retained for API
// compatibility; raw flag diagnostics are suppressed to protect input values.
func Parse(args []string, lookup func(string) (string, bool), stderr io.Writer) (Config, error) {
	return ParseContext(context.Background(), args, lookup, stderr)
}

// ParseContext is Parse with ctx carrying the existing diagnostic scopes.
// Parsed help/version options bypass dotenv and deployment validation. A nil
// error yields resolved settings; on error the returned Config must not be used.
// Local file reads are synchronous, not interruptible context-aware I/O.
func ParseContext(ctx context.Context, args []string, lookup func(string) (string, bool), _ io.Writer) (c Config, err error) {
	ctx, scope := diagnostics.Start(ctx, "config", "parse")
	defer func() { err = scope.End(err) }()
	o, err := parseFlags(args)
	if err != nil {
		return c, err
	}
	if o.config.Help || o.config.Version {
		// Offline requests must not open dotenv or inherit a log-file setting.
		return Config{EnvFile: o.config.EnvFile, Help: o.config.Help, Version: o.config.Version}, nil
	}
	values, err := loadEnvContext(ctx, o.config.EnvFile, o.set["env-file"])
	if err != nil {
		return c, err
	}
	if err := o.resolve(lookup, values); err != nil {
		return c, err
	}
	return o.configuration()
}

// configuration converts the resolved numeric strings and validates selection,
// common settings, and connection settings. It returns the final Config or the
// first safe validation error; production validation remains a caller decision.
func (o *options) configuration() (c Config, err error) {
	c = o.config
	if err := c.selectNamespace(o.name, o.set["id"], o.set["name"], o.args); err != nil {
		return c, err
	}
	if c.Interval, err = positiveDuration(o.interval, "interval"); err != nil {
		return c, err
	}
	if c.Timeout, err = positiveDuration(o.timeout, "timeout"); err != nil {
		return c, err
	}
	if c.CommandTimeout, err = positiveDuration(o.commandTimeout, "command-timeout"); err != nil {
		return c, err
	}
	if c.FirecallPollInterval, err = positiveDuration(o.firecallPollInterval, "firecall-poll-interval"); err != nil {
		return c, err
	}
	if c.FirecallActivationTimeout, err = positiveDuration(o.firecallActivationTimeout, "firecall-activation-timeout"); err != nil {
		return c, err
	}
	c.Duration, err = strconv.Atoi(o.hours)
	if err != nil || c.Duration < 1 {
		return c, errors.New("duration must be a positive number of hours")
	}
	if err := c.validateCommon(); err != nil {
		return c, err
	}
	if !c.List {
		return c, c.validateConnection()
	}
	return c, nil
}

// selectNamespace sets Target, Mode, and List from a single explicit selector or
// positional args. idSet/nameSet distinguish absent from explicitly empty flags.
// Invalid, conflicting, or missing selectors return an error without echoing input.
func (c *Config) selectNamespace(name string, idSet, nameSet bool, args []string) error {
	if idSet && nameSet {
		return errors.New("--id and --name are mutually exclusive")
	}
	if (idSet && c.Target == "") || (nameSet && name == "") {
		return errors.New("explicit namespace selector cannot be empty")
	}
	if len(args) > 1 || ((idSet || nameSet) && len(args) != 0) {
		return errors.New("provide exactly one namespace selector or list")
	}
	if idSet {
		c.Mode = "id"
	}
	if nameSet {
		c.Target, c.Mode = name, "name"
	}
	if len(args) == 1 {
		c.Target = args[0]
		if c.Target == "list" {
			c.List = true
		} else if len(c.Target) > 3 {
			c.Mode = "name"
		} else {
			c.Mode = "id"
		}
	}
	if c.Target == "" {
		return errors.New("missing namespace identifier; use --help")
	}
	if strings.ContainsAny(c.Target, "\x00\r\n") {
		return errors.New("invalid namespace selector")
	}
	return nil
}

// positiveDuration accepts a positive Go duration or an integer number of
// seconds in value. name is an internal option label used in errors; malformed,
// nonpositive, or overflowing input returns zero and an error without its value.
func positiveDuration(value, name string) (time.Duration, error) {
	if _, err := strconv.Atoi(value); err == nil {
		value += "s"
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return d, nil
}

// validateCommon checks logging format and database settings required by both
// list and connection commands. It returns nil when valid and a value-free
// diagnostic error otherwise; it does not connect to the database.
func (c Config) validateCommon() error {
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return errors.New("invalid logging configuration")
	}
	port, err := strconv.Atoi(c.DBPort)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid database port")
	}
	if c.DBHost == "" || c.DBUser == "" || c.DBName == "" {
		return errors.New("PGHOST, PGUSER and PGDATABASE are required")
	}
	return nil
}

// validateConnection checks encrypted-credential presence, digest selection,
// identity-provider URL, executable, and email domain. It returns a safe error
// on invalid settings without decrypting credentials or contacting services.
func (c Config) validateConnection() error {
	if c.EncryptedUser == "" || c.EncryptedPassword == "" || c.Passphrase == "" {
		return errors.New("ENC_USERNAME, ENC_PASSWORD and DECRYPT_PASSPHRASE are required")
	}
	if c.Digest != "sha256" && c.Digest != "md5" {
		return errors.New("DECRYPT_DIGEST must be sha256 or md5")
	}
	if err := HTTPS(c.IDPEndpoint); err != nil {
		return fmt.Errorf("IDP_ENDPOINT: %w", err)
	}
	if c.Kconnect == "" || strings.HasPrefix(c.Kconnect, "-") {
		return errors.New("invalid kconnect executable")
	}
	if c.EmailDomain == "" || strings.ContainsAny(c.EmailDomain, "@ \r\n") {
		return errors.New("invalid email domain")
	}
	return nil
}

// HTTPS returns nil if raw is an absolute HTTPS URL without user information or
// a fragment. Invalid URLs return a fixed error, never the potentially sensitive
// input. This validates syntax only; it makes no network request.
func HTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("an absolute HTTPS URL without embedded credentials or fragment is required")
	}
	return nil
}

// ValidateProduction returns an error if activation timing or OAuth/firecall
// endpoint URLs are invalid. Call it after discovery identifies production and
// before authentication; nonproduction connections do not require these URLs.
func (c Config) ValidateProduction() error {
	if c.FirecallPollInterval <= 0 || c.FirecallActivationTimeout <= 0 {
		return errors.New("firecall polling interval and activation timeout must be positive")
	}
	for _, x := range []struct{ name, value string }{{"OAUTH_URL", c.OAuthURL}, {"FIRECALL_URL", c.FirecallListURL}, {"FIRECALL_URI", c.FirecallCreateURL}} {
		if err := HTTPS(x.value); err != nil {
			return fmt.Errorf("%s: %w", x.name, err)
		}
	}
	return nil
}
