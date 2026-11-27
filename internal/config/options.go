package config

import (
	"errors"
	"flag"
	"io"
	"strconv"
)

// options retains explicit flag presence and unconverted numeric values until
// dotenv and environment precedence can be resolved.
type options struct {
	config Config
	set    map[string]bool
	args   []string
	name   string

	interval, timeout, commandTimeout               string
	firecallPollInterval, firecallActivationTimeout string
	hours                                           string
}

// parseFlags parses args using standard flag semantics without reading files or
// environment variables. It returns options with literal defaults, or a fixed
// error that cannot expose argument values through flag's diagnostic messages.
func parseFlags(args []string) (*options, error) {
	o := &options{set: make(map[string]bool)}
	c := &o.config
	fs := flag.NewFlagSet("eks", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.EnvFile, "env-file", ".env", "dotenv path")
	fs.StringVar(&c.Target, "id", "", "explicit namespace ID")
	fs.StringVar(&o.name, "name", "", "explicit namespace name")
	fs.BoolVar(&c.Help, "help", false, "show help")
	fs.BoolVar(&c.Help, "h", false, "show help")
	fs.BoolVar(&c.Version, "version", false, "show version")
	fs.BoolVar(&c.Once, "once", false, "connect once")
	fs.BoolVar(&c.JSON, "json", false, "emit JSON records; never prompt")
	fs.BoolVar(&c.Verbose, "verbose", false, "debug diagnostics on stderr")
	fs.StringVar(&c.LogFormat, "log-format", "text", "text or json diagnostics")
	fs.StringVar(&c.LogFile, "log-file", "", "optional private rotating log")
	fs.BoolVar(&c.LogSource, "log-source", false, "include sanitized source location")
	fs.StringVar(&c.DBHost, "db-host", "", "database host")
	fs.StringVar(&c.DBPort, "db-port", "5432", "database port")
	fs.StringVar(&c.DBUser, "db-user", "", "database user")
	fs.StringVar(&c.DBName, "db-name", "postgres", "database name")
	fs.StringVar(&c.DBRootCert, "db-root-cert", "", "database TLS root certificate")
	fs.StringVar(&c.OAuthURL, "oauth-url", "", "OAuth endpoint")
	fs.StringVar(&c.FirecallListURL, "firecall-url", "", "firecall list endpoint")
	fs.StringVar(&c.FirecallCreateURL, "firecall-create-url", "", "firecall creation endpoint")
	fs.StringVar(&c.IDPEndpoint, "idp-endpoint", "", "identity provider endpoint")
	fs.StringVar(&c.Region, "region", "us-east-1", "AWS region")
	fs.StringVar(&c.IDPProvider, "idp-provider", "PingNTLM", "identity provider")
	fs.StringVar(&c.EmailDomain, "email-domain", "test.com", "email domain")
	fs.StringVar(&c.Kconnect, "kconnect", "kconnect", "kconnect executable")
	fs.StringVar(&c.Ticket, "ticket", "", "change or incident ticket")
	fs.StringVar(&c.Digest, "decrypt-digest", "sha256", "sha256 or legacy md5")
	fs.StringVar(&o.interval, "interval", "900s", "refresh delay (Go duration or seconds)")
	fs.StringVar(&o.timeout, "timeout", "15s", "database/HTTP timeout")
	fs.StringVar(&o.commandTimeout, "command-timeout", "2m", "kconnect timeout")
	fs.StringVar(&o.hours, "duration", "8", "firecall duration in hours")
	fs.StringVar(&o.firecallPollInterval, "firecall-poll-interval", "5s", "activation check delay")
	fs.StringVar(&o.firecallActivationTimeout, "firecall-activation-timeout", "2m", "total activation wait")
	if err := fs.Parse(args); err != nil {
		return nil, errors.New("invalid command-line options; options must precede the namespace selector")
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	o.args = fs.Args()
	return o, nil
}

// resolve applies lookup > dotenv > existing default to options not explicitly
// supplied on the command line. Empty values override lower-priority sources.
// Secrets come only from lookup/dotenv; an invalid selected log-source boolean
// returns a safe error. Neither source is modified.
func (o *options) resolve(lookup func(string) (string, bool), dotenv map[string]string) error {
	get := func(name, env, current string) string {
		if o.set[name] {
			return current
		}
		if v, ok := lookup(env); ok {
			return v
		}
		if v, ok := dotenv[env]; ok {
			return v
		}
		return current
	}
	c := &o.config
	c.DBHost = get("db-host", "PGHOST", c.DBHost)
	c.DBPort = get("db-port", "PGPORT", c.DBPort)
	c.DBUser = get("db-user", "PGUSER", c.DBUser)
	c.DBName = get("db-name", "PGDATABASE", c.DBName)
	c.DBRootCert = get("db-root-cert", "PGSSLROOTCERT", c.DBRootCert)
	c.OAuthURL = get("oauth-url", "OAUTH_URL", c.OAuthURL)
	c.FirecallListURL = get("firecall-url", "FIRECALL_URL", c.FirecallListURL)
	c.FirecallCreateURL = get("firecall-create-url", "FIRECALL_URI", c.FirecallCreateURL)
	c.IDPEndpoint = get("idp-endpoint", "IDP_ENDPOINT", c.IDPEndpoint)
	c.Region = get("region", "REGION", c.Region)
	c.IDPProvider = get("idp-provider", "IDP_PROVIDER", c.IDPProvider)
	c.EmailDomain = get("email-domain", "EMAIL_DOMAIN", c.EmailDomain)
	c.Kconnect = get("kconnect", "KCONNECT_BIN", c.Kconnect)
	c.Ticket = get("ticket", "CM_TICKET", c.Ticket)
	c.Digest = get("decrypt-digest", "DECRYPT_DIGEST", c.Digest)
	c.LogFormat = get("log-format", "EKS_LOG_FORMAT", c.LogFormat)
	c.LogFile = get("log-file", "EKS_LOG_FILE", c.LogFile)
	o.interval = get("interval", "INTERVAL", o.interval)
	o.timeout = get("timeout", "TIMEOUT", o.timeout)
	o.commandTimeout = get("command-timeout", "COMMAND_TIMEOUT", o.commandTimeout)
	o.firecallPollInterval = get("firecall-poll-interval", "FIRECALL_POLL_INTERVAL", o.firecallPollInterval)
	o.firecallActivationTimeout = get("firecall-activation-timeout", "FIRECALL_ACTIVATION_TIMEOUT", o.firecallActivationTimeout)
	o.hours = get("duration", "DURATION_IN_HRS", o.hours)
	c.DBPassword = get("", "PGPASSWORD", "")
	c.EncryptedUser = get("", "ENC_USERNAME", "")
	c.EncryptedPassword = get("", "ENC_PASSWORD", "")
	c.Passphrase = get("", "DECRYPT_PASSPHRASE", "")
	c.ProductionPassword = get("", "PROD_PASSWORD", "")
	if !o.set["log-source"] {
		source, err := strconv.ParseBool(get("", "EKS_LOG_SOURCE", "false"))
		if err != nil {
			return errors.New("invalid logging configuration")
		}
		c.LogSource = source
	}
	return nil
}
