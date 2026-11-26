// Package config implements flags > environment > dotenv > defaults.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Target, Mode, EnvFile                                                    string
	List, Once, JSON, Verbose, Help, Version                                 bool
	DBHost, DBPort, DBUser, DBName, DBPassword, DBRootCert                   string
	EncryptedUser, EncryptedPassword, Passphrase, Digest, ProductionPassword string
	OAuthURL, FirecallListURL, FirecallCreateURL, IDPEndpoint                string
	Region, IDPProvider, EmailDomain, Kconnect, Ticket                       string
	Interval, Timeout, CommandTimeout                                        time.Duration
	Duration                                                                 int
}

// Parse does not mutate the process environment. Lookup is injectable for tests.
func Parse(args []string, lookup func(string) (string, bool), stderr io.Writer) (Config, error) {
	c := Config{EnvFile: ".env"}
	// Locate only the dotenv selector before parsing defaults from that file.
	explicit := false
	for i, a := range args {
		if a == "--env-file" || a == "-env-file" {
			if i+1 == len(args) {
				return c, errors.New("--env-file requires a path")
			}
			c.EnvFile, explicit = args[i+1], true
		} else if strings.HasPrefix(a, "--env-file=") || strings.HasPrefix(a, "-env-file=") {
			_, c.EnvFile, _ = strings.Cut(a, "=")
			explicit = true
		}
	}
	// Help and version do not require a readable deployment configuration.
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "-help" {
			c.Help = true
			return c, nil
		}
		if a == "--version" || a == "-version" {
			c.Version = true
			return c, nil
		}
	}
	values, err := loadEnv(c.EnvFile, explicit)
	if err != nil {
		return c, err
	}
	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		if v, ok := values[key]; ok {
			return v
		}
		return fallback
	}
	fs := flag.NewFlagSet("eks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.EnvFile, "env-file", c.EnvFile, "dotenv path")
	fs.StringVar(&c.Target, "id", "", "explicit namespace ID")
	var name string
	fs.StringVar(&name, "name", "", "explicit namespace name")
	fs.BoolVar(&c.Once, "once", false, "connect once")
	fs.BoolVar(&c.JSON, "json", false, "emit JSON records; never prompt")
	fs.BoolVar(&c.Verbose, "verbose", false, "debug diagnostics on stderr")
	for _, x := range []struct {
		p                   *string
		flag, env, fallback string
	}{
		{&c.DBHost, "db-host", "PGHOST", ""}, {&c.DBPort, "db-port", "PGPORT", "5432"},
		{&c.DBUser, "db-user", "PGUSER", ""}, {&c.DBName, "db-name", "PGDATABASE", "postgres"},
		{&c.DBRootCert, "db-root-cert", "PGSSLROOTCERT", ""},
		{&c.OAuthURL, "oauth-url", "OAUTH_URL", ""}, {&c.FirecallListURL, "firecall-url", "FIRECALL_URL", ""},
		{&c.FirecallCreateURL, "firecall-create-url", "FIRECALL_URI", ""},
		{&c.IDPEndpoint, "idp-endpoint", "IDP_ENDPOINT", ""}, {&c.Region, "region", "REGION", "us-east-1"},
		{&c.IDPProvider, "idp-provider", "IDP_PROVIDER", "PingNTLM"}, {&c.EmailDomain, "email-domain", "EMAIL_DOMAIN", "test.com"},
		{&c.Kconnect, "kconnect", "KCONNECT_BIN", "kconnect"}, {&c.Ticket, "ticket", "CM_TICKET", ""},
		{&c.Digest, "decrypt-digest", "DECRYPT_DIGEST", "sha256"},
	} {
		fs.StringVar(x.p, x.flag, get(x.env, x.fallback), x.env)
	}
	var interval, timeout, commandTimeout, duration string
	fs.StringVar(&interval, "interval", get("INTERVAL", "900s"), "refresh delay (Go duration or seconds)")
	fs.StringVar(&timeout, "timeout", get("TIMEOUT", "15s"), "database/HTTP timeout")
	fs.StringVar(&commandTimeout, "command-timeout", get("COMMAND_TIMEOUT", "2m"), "kconnect timeout")
	fs.StringVar(&duration, "duration", get("DURATION_IN_HRS", "8"), "firecall duration in hours")
	// flag accepts options before positionals; reject trailing arguments explicitly.
	if err := fs.Parse(args); err != nil {
		return c, errors.New("invalid command-line options; options must precede the namespace selector")
	}
	if c.Target != "" && name != "" {
		return c, errors.New("--id and --name are mutually exclusive")
	}
	explicitSelectors := 0
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "id" || f.Name == "name" {
			explicitSelectors++
		}
	})
	if explicitSelectors > 0 && c.Target == "" && name == "" {
		return c, errors.New("explicit namespace selector cannot be empty")
	}
	if c.Target != "" {
		c.Mode = "id"
	}
	if name != "" {
		c.Target, c.Mode = name, "name"
	}
	if fs.NArg() > 1 || (c.Target != "" && fs.NArg() != 0) {
		return c, errors.New("provide exactly one namespace selector or list")
	}
	if fs.NArg() == 1 {
		c.Target = fs.Arg(0)
		if c.Target == "list" {
			c.List = true
		} else if len(c.Target) > 3 {
			c.Mode = "name"
		} else {
			c.Mode = "id"
		}
	}
	if c.Target == "" {
		return c, errors.New("missing namespace identifier; use --help")
	}
	if strings.ContainsAny(c.Target, "\x00\r\n") {
		return c, errors.New("invalid namespace selector")
	}
	for _, x := range []struct {
		s    string
		p    *time.Duration
		name string
	}{
		{interval, &c.Interval, "interval"}, {timeout, &c.Timeout, "timeout"}, {commandTimeout, &c.CommandTimeout, "command-timeout"},
	} {
		v := x.s
		if _, err := strconv.Atoi(v); err == nil {
			v += "s"
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("%s must be a positive duration", x.name)
		}
		*x.p = d
	}
	c.Duration, err = strconv.Atoi(duration)
	if err != nil || c.Duration < 1 {
		return c, errors.New("duration must be a positive number of hours")
	}
	port, err := strconv.Atoi(c.DBPort)
	if err != nil || port < 1 || port > 65535 {
		return c, errors.New("invalid database port")
	}
	if c.DBHost == "" || c.DBUser == "" || c.DBName == "" {
		return c, errors.New("PGHOST, PGUSER and PGDATABASE are required")
	}
	c.DBPassword = get("PGPASSWORD", "")
	c.EncryptedUser, c.EncryptedPassword = get("ENC_USERNAME", ""), get("ENC_PASSWORD", "")
	c.Passphrase = get("DECRYPT_PASSPHRASE", "")
	c.ProductionPassword = get("PROD_PASSWORD", "")
	if !c.List {
		if c.EncryptedUser == "" || c.EncryptedPassword == "" || c.Passphrase == "" {
			return c, errors.New("ENC_USERNAME, ENC_PASSWORD and DECRYPT_PASSPHRASE are required")
		}
		if c.Digest != "sha256" && c.Digest != "md5" {
			return c, errors.New("DECRYPT_DIGEST must be sha256 or md5")
		}
		if err := HTTPS(c.IDPEndpoint); err != nil {
			return c, fmt.Errorf("IDP_ENDPOINT: %w", err)
		}
		if c.Kconnect == "" || strings.HasPrefix(c.Kconnect, "-") {
			return c, errors.New("invalid kconnect executable")
		}
		if c.EmailDomain == "" || strings.ContainsAny(c.EmailDomain, "@ \r\n") {
			return c, errors.New("invalid email domain")
		}
	}
	return c, nil
}

func HTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("an absolute HTTPS URL without embedded credentials or fragment is required")
	}
	return nil
}

func (c Config) ValidateProduction() error {
	for _, x := range []struct{ name, value string }{{"OAUTH_URL", c.OAuthURL}, {"FIRECALL_URL", c.FirecallListURL}, {"FIRECALL_URI", c.FirecallCreateURL}} {
		if err := HTTPS(x.value); err != nil {
			return fmt.Errorf("%s: %w", x.name, err)
		}
	}
	return nil
}
