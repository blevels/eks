package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eks/internal/diagnostics/diagtest"
)

func TestArgumentBoundaries(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, path := range []string{"--help", "--version"} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, target, ticket, envFile string
		args                          []string
		help, version, bad            bool
	}{
		{name: "short help", args: []string{"-h"}, help: true},
		{name: "single dash help", args: []string{"-help"}, help: true},
		{name: "double dash help", args: []string{"--help"}, help: true},
		{name: "single dash version", args: []string{"-version"}, version: true},
		{name: "double dash version", args: []string{"--version"}, version: true},
		{name: "false help", args: []string{"--help=false", "12"}, target: "12"},
		{name: "false version", args: []string{"--version=false", "12"}, target: "12"},
		{name: "both offline flags", args: []string{"--version", "--help"}, help: true, version: true},
		{name: "version operand", args: []string{"--ticket", "--version", "12"}, target: "12", ticket: "--version"},
		{name: "help operand", args: []string{"--name", "--help"}, target: "--help"},
		{name: "env-file operand", args: []string{"--ticket", "--env-file=CANARY-missing", "list"}, target: "list", ticket: "--env-file=CANARY-missing"},
		{name: "help file path", args: []string{"--env-file", "--help", "list"}, target: "list", envFile: "--help"},
		{name: "version file path", args: []string{"--env-file", "--version", "list"}, target: "list", envFile: "--version"},
		{name: "terminator help", args: []string{"--", "--help"}, target: "--help"},
		{name: "terminator version", args: []string{"--", "--version"}, target: "--version"},
		{name: "terminator env-file", args: []string{"--", "--env-file=CANARY-missing"}, target: "--env-file=CANARY-missing"},
		{name: "repeated id", args: []string{"--id", "1", "--id", "2"}, target: "2"},
		{name: "repeated name", args: []string{"--name", "a", "--name", "b"}, target: "b"},
		{name: "trailing help", args: []string{"12", "--help"}, bad: true},
		{name: "trailing version", args: []string{"12", "--version"}, bad: true},
		{name: "trailing env-file", args: []string{"list", "--env-file", "CANARY-missing"}, bad: true},
		{name: "unknown before help", args: []string{"--CANARY", "--help"}, bad: true},
		{name: "unknown after help", args: []string{"--help", "--CANARY"}, bad: true},
		{name: "unknown with version", args: []string{"--version", "--CANARY"}, bad: true},
		{name: "missing operand with help", args: []string{"--help", "--ticket"}, bad: true},
		{name: "invalid boolean with help", args: []string{"--verbose=CANARY", "--help"}, bad: true},
		{name: "empty id with name", args: []string{"--id=", "--name", "ab"}, bad: true},
		{name: "empty name with id", args: []string{"--name=", "--id", "12"}, bad: true},
		{name: "empty name", args: []string{"--name=", "list"}, bad: true},
		{name: "last id empty", args: []string{"--id", "12", "--id="}, bad: true},
		{name: "empty explicit dotenv", args: []string{"--env-file=", "list"}, bad: true},
		{name: "control character", args: []string{"CANARY\n"}, bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			env := environment()
			c, err := Parse(tt.args, func(k string) (string, bool) {
				v, ok := env[k]
				return v, ok
			}, &output)
			if (err != nil) != tt.bad {
				t.Fatalf("error = %v; want failure %v", err, tt.bad)
			}
			if output.Len() != 0 || (err != nil && strings.Contains(err.Error(), "CANARY")) {
				t.Fatal("argument content escaped into parser diagnostics")
			}
			if tt.bad {
				return
			}
			envFile := tt.envFile
			if envFile == "" {
				envFile = ".env"
			}
			if c.Target != tt.target || c.Ticket != tt.ticket || c.EnvFile != envFile || c.Help != tt.help || c.Version != tt.version {
				t.Fatalf("unexpected selection/offline result: target=%q ticket=%q file=%q help=%v version=%v", c.Target, c.Ticket, c.EnvFile, c.Help, c.Version)
			}
		})
	}
}

func TestAllConfigurationSources(t *testing.T) {
	t.Chdir(t.TempDir())
	settings := []struct{ flag, env, value string }{
		{"db-host", "PGHOST", "synthetic.invalid"}, {"db-port", "PGPORT", "5433"},
		{"db-user", "PGUSER", "synthetic"}, {"db-name", "PGDATABASE", "discovery"},
		{"db-root-cert", "PGSSLROOTCERT", "synthetic.pem"},
		{"oauth-url", "OAUTH_URL", "https://oauth.invalid"}, {"firecall-url", "FIRECALL_URL", "https://list.invalid"},
		{"firecall-create-url", "FIRECALL_URI", "https://create.invalid"},
		{"idp-endpoint", "IDP_ENDPOINT", "https://idp.invalid"}, {"region", "REGION", "us-west-2"},
		{"idp-provider", "IDP_PROVIDER", "synthetic-provider"}, {"email-domain", "EMAIL_DOMAIN", "synthetic.invalid"},
		{"kconnect", "KCONNECT_BIN", "synthetic-kconnect"}, {"ticket", "CM_TICKET", "synthetic-ticket"},
		{"decrypt-digest", "DECRYPT_DIGEST", "md5"}, {"log-format", "EKS_LOG_FORMAT", "json"},
		{"log-source", "EKS_LOG_SOURCE", "true"}, {"log-file", "EKS_LOG_FILE", "synthetic.log"},
		{"interval", "INTERVAL", "17"}, {"timeout", "TIMEOUT", "19"},
		{"command-timeout", "COMMAND_TIMEOUT", "3m"}, {"duration", "DURATION_IN_HRS", "6"},
		{"firecall-poll-interval", "FIRECALL_POLL_INTERVAL", "7s"},
		{"firecall-activation-timeout", "FIRECALL_ACTIVATION_TIMEOUT", "4m"},
		{"", "PGPASSWORD", "synthetic-db-secret"}, {"", "ENC_USERNAME", "synthetic-user-cipher"},
		{"", "ENC_PASSWORD", "synthetic-password-cipher"}, {"", "DECRYPT_PASSPHRASE", "synthetic-passphrase"},
		{"", "PROD_PASSWORD", "synthetic-production-secret"},
	}
	for _, source := range []string{"dotenv", "environment", "flags"} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.env")
			env := make(map[string]string)
			var file strings.Builder
			args := []string{"--env-file", path}
			for _, s := range settings {
				switch source {
				case "dotenv":
					file.WriteString(s.env + "=" + s.value + "\n")
				case "environment":
					file.WriteString(s.env + "=CANARY\n")
					env[s.env] = s.value
				case "flags":
					if s.flag == "" {
						env[s.env] = s.value
					} else {
						env[s.env] = "CANARY"
						args = append(args, "--"+s.flag+"="+s.value)
					}
				}
			}
			if err := os.WriteFile(path, []byte(file.String()), 0600); err != nil {
				t.Fatal(err)
			}
			args = append(args, "12")
			c, err := parse(t, args, env)
			if err != nil {
				t.Fatal(err)
			}
			want := Config{
				EnvFile: path, Target: "12", Mode: "id", LogFormat: "json", LogSource: true, LogFile: "synthetic.log",
				DBHost: "synthetic.invalid", DBPort: "5433", DBUser: "synthetic", DBName: "discovery",
				DBRootCert: "synthetic.pem", DBPassword: "synthetic-db-secret",
				EncryptedUser: "synthetic-user-cipher", EncryptedPassword: "synthetic-password-cipher",
				Passphrase: "synthetic-passphrase", ProductionPassword: "synthetic-production-secret", Digest: "md5",
				OAuthURL: "https://oauth.invalid", FirecallListURL: "https://list.invalid", FirecallCreateURL: "https://create.invalid",
				IDPEndpoint: "https://idp.invalid", Region: "us-west-2", IDPProvider: "synthetic-provider",
				EmailDomain: "synthetic.invalid", Kconnect: "synthetic-kconnect", Ticket: "synthetic-ticket",
				Interval: 17 * time.Second, Timeout: 19 * time.Second, CommandTimeout: 3 * time.Minute,
				FirecallPollInterval: 7 * time.Second, FirecallActivationTimeout: 4 * time.Minute, Duration: 6,
			}
			if c != want {
				t.Fatalf("%s did not resolve all settings correctly", source)
			}
		})
	}
}

func TestOfflineOptionsSkipConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("INVALID DOTENV"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-h", "-help", "--help", "-version", "--version"} {
		t.Run(flag, func(t *testing.T) {
			ctx, logs := diagtest.New(t)
			c, err := ParseContext(ctx, []string{"--env-file", "CANARY-missing", "--log-file", "CANARY-missing/log", flag}, func(string) (string, bool) {
				t.Fatal("offline request looked up deployment environment")
				return "", false
			}, io.Discard)
			if err != nil || (!c.Help && !c.Version) || c.LogFile != "" {
				t.Fatal("offline request loaded configuration", err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY")
			for _, r := range logs.Records() {
				if strings.HasPrefix(r["operation"].(string), "dotenv_") {
					t.Fatal("offline request accessed dotenv")
				}
			}
		})
	}
}

func TestRepeatedDotenvSelection(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "selected.env")
	if err := os.WriteFile(path, []byte("PGHOST=selected.invalid\nPGUSER=synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--env-file", "CANARY-missing", "--env-file=" + path, "list"},
		{"-env-file=CANARY-missing", "-env-file", path, "list"},
	} {
		c, err := Parse(args, func(string) (string, bool) { return "", false }, io.Discard)
		if err != nil || c.EnvFile != path || c.DBHost != "selected.invalid" {
			t.Fatal("last parsed dotenv selection did not win", err)
		}
	}
}

func TestExplicitValuesAndInvalidOverrides(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "values.env")
	if err := os.WriteFile(path, []byte("PGHOST=file.invalid\nPGPASSWORD=file-secret\nEKS_LOG_SOURCE=true\nEKS_LOG_FILE=file.log\nTIMEOUT=30s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		env   map[string]string
		args  []string
		bad   bool
		check func(Config) bool
	}{
		{name: "empty environment host", env: map[string]string{"PGHOST": ""}, bad: true},
		{name: "empty flag host", args: []string{"--db-host="}, bad: true},
		{name: "empty environment secret", env: map[string]string{"PGPASSWORD": ""}, check: func(c Config) bool { return c.DBPassword == "" }},
		{name: "empty environment file", env: map[string]string{"EKS_LOG_FILE": ""}, check: func(c Config) bool { return c.LogFile == "" }},
		{name: "empty flag file", env: map[string]string{"EKS_LOG_FILE": "env.log"}, args: []string{"--log-file="}, check: func(c Config) bool { return c.LogFile == "" }},
		{name: "false environment boolean", env: map[string]string{"EKS_LOG_SOURCE": "false"}, check: func(c Config) bool { return !c.LogSource }},
		{name: "false flag boolean", env: map[string]string{"EKS_LOG_SOURCE": "true"}, args: []string{"--log-source=false"}, check: func(c Config) bool { return !c.LogSource }},
		{name: "invalid boolean overridden", env: map[string]string{"EKS_LOG_SOURCE": "CANARY"}, args: []string{"--log-source=false"}, check: func(c Config) bool { return !c.LogSource }},
		{name: "empty boolean is invalid", env: map[string]string{"EKS_LOG_SOURCE": ""}, bad: true},
		{name: "invalid format overridden", env: map[string]string{"EKS_LOG_FORMAT": "CANARY"}, args: []string{"--log-format=json"}, check: func(c Config) bool { return c.LogFormat == "json" }},
		{name: "invalid duration overridden", env: map[string]string{"TIMEOUT": "CANARY"}, args: []string{"--timeout=7"}, check: func(c Config) bool { return c.Timeout == 7*time.Second }},
		{name: "invalid hours overridden", env: map[string]string{"DURATION_IN_HRS": "CANARY"}, args: []string{"--duration=4"}, check: func(c Config) bool { return c.Duration == 4 }},
		{name: "invalid port overridden", env: map[string]string{"PGPORT": "CANARY"}, args: []string{"--db-port=5433"}, check: func(c Config) bool { return c.DBPort == "5433" }},
		{name: "invalid activation overridden", env: map[string]string{"FIRECALL_ACTIVATION_TIMEOUT": "CANARY"}, args: []string{"--firecall-activation-timeout=1m"}, check: func(c Config) bool { return c.FirecallActivationTimeout == time.Minute }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := environment()
			maps.Copy(env, tt.env)
			before := maps.Clone(env)
			args := append([]string{"--env-file", path}, tt.args...)
			args = append(args, "list")
			c, err := parse(t, args, env)
			if (err != nil) != tt.bad || (err != nil && strings.Contains(err.Error(), "CANARY")) {
				t.Fatal("unexpected or unsafe error", err)
			}
			if err == nil && !tt.check(c) {
				t.Fatal("selected value did not respect precedence")
			}
			if !maps.Equal(env, before) {
				t.Fatal("environment source was mutated")
			}
		})
	}
}

func TestValidationApplicability(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		key, value, selector string
		bad                  bool
	}{
		{"PGPORT", "0", "list", true},
		{"PGPORT", "65536", "list", true},
		{"PGPORT", "65535", "list", false},
		{"PGDATABASE", "", "list", true},
		{"DURATION_IN_HRS", "0", "list", false},
		{"DURATION_IN_HRS", "9999999999999999999999", "list", false},
		{"INTERVAL", "0", "list", false},
		{"COMMAND_TIMEOUT", "-1s", "list", false},
		{"FIRECALL_POLL_INTERVAL", "0", "list", false},
		{"FIRECALL_ACTIVATION_TIMEOUT", "0", "list", false},
		{"ENC_PASSWORD", "", "list", false},
		{"ENC_PASSWORD", "", "12", true},
		{"DECRYPT_DIGEST", "invalid", "list", false},
		{"DECRYPT_DIGEST", "invalid", "12", true},
		{"DECRYPT_DIGEST", "md5", "12", false},
		{"IDP_ENDPOINT", "http://insecure.invalid", "list", false},
		{"IDP_ENDPOINT", "http://insecure.invalid", "12", true},
		{"KCONNECT_BIN", "-invalid", "12", true},
		{"EMAIL_DOMAIN", "bad@domain", "12", true},
		{"OAUTH_URL", "http://deferred.invalid", "12", false},
	} {
		t.Run(tt.key+"/"+tt.value+"/"+tt.selector, func(t *testing.T) {
			env := environment()
			env[tt.key] = tt.value
			_, err := parse(t, []string{tt.selector}, env)
			if (err != nil) != tt.bad {
				t.Fatal("validation applicability changed", err)
			}
		})
	}
}

func TestPositiveDuration(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"7", 7 * time.Second}, {"1ns", time.Nanosecond}, {"1.5m", 90 * time.Second},
		{"0", 0}, {"-1s", 0}, {"1.5", 0}, {"CANARY", 0},
		{"999999999999999999999999", 0},
	} {
		got, err := positiveDuration(tt.value, "timeout")
		if got != tt.want || (err != nil) != (tt.want == 0) {
			t.Fatalf("value=%q: got %v, %v", tt.value, got, err)
		}
		if err != nil && strings.Contains(err.Error(), "CANARY") {
			t.Fatal("duration input escaped into error")
		}
	}
}

func TestDotenvErrorIdentity(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		_, err := loadEnvWith(context.Background(), "synthetic.env", true, func(string) (io.ReadCloser, error) {
			return nil, cause
		})
		if !errors.Is(err, cause) {
			t.Fatal("dotenv error lost its identity", err)
		}
	}
}
