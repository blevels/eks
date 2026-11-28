package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListUnusedNumericSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	settings := []struct {
		flag, env string
		value     func(Config) int64
		want      int64
	}{
		{"interval", "INTERVAL", func(c Config) int64 { return int64(c.Interval) }, int64(7 * time.Second)},
		{"command-timeout", "COMMAND_TIMEOUT", func(c Config) int64 { return int64(c.CommandTimeout) }, int64(7 * time.Second)},
		{"firecall-poll-interval", "FIRECALL_POLL_INTERVAL", func(c Config) int64 { return int64(c.FirecallPollInterval) }, int64(7 * time.Second)},
		{"firecall-activation-timeout", "FIRECALL_ACTIVATION_TIMEOUT", func(c Config) int64 { return int64(c.FirecallActivationTimeout) }, int64(7 * time.Second)},
		{"duration", "DURATION_IN_HRS", func(c Config) int64 { return int64(c.Duration) }, 7},
	}
	for _, s := range settings {
		t.Run(s.flag, func(t *testing.T) {
			for _, source := range []string{"environment", "dotenv", "flag"} {
				for _, value := range []string{"", "CANARY", "0", "-1", "999999999999999999999999"} {
					for _, selector := range [][]string{{"list"}, {"12"}, {"--id=list"}, {"--name=list"}} {
						t.Run(source+"/"+value+"/"+strings.Join(selector, " "), func(t *testing.T) {
							env := environment()
							args := []string{}
							switch source {
							case "environment":
								env[s.env] = value
							case "dotenv":
								path := filepath.Join(t.TempDir(), "settings.env")
								if err := os.WriteFile(path, []byte(s.env+"="+value+"\n"), 0600); err != nil {
									t.Fatal(err)
								}
								args = append(args, "--env-file", path)
							case "flag":
								args = append(args, "--"+s.flag+"="+value)
							}
							args = append(args, selector...)
							c, err := parse(t, args, env)
							wantErr := source == "flag" || selector[0] != "list"
							if (err != nil) != wantErr {
								t.Fatalf("error=%v; want failure=%v", err, wantErr)
							}
							if err != nil && strings.Contains(err.Error(), "CANARY") {
								t.Fatal("numeric input escaped into error")
							}
							if err == nil && (!c.List || s.value(c) != 0) {
								t.Fatal("inherited unused setting was not ignored")
							}
						})
					}
				}
			}
			env := environment()
			env[s.env] = "CANARY"
			for _, flags := range [][]string{
				{"--" + s.flag + "=7"},
				{"--" + s.flag + "=CANARY", "--" + s.flag + "=7"},
			} {
				c, err := parse(t, append(flags, "list"), env)
				if err != nil || s.value(c) != s.want {
					t.Fatal("valid explicit value did not win", err)
				}
			}
			if _, err := parse(t, []string{"--" + s.flag + "=7", "--" + s.flag + "=CANARY", "list"}, env); err == nil {
				t.Fatal("invalid final explicit value accepted")
			}
		})
	}
}

func TestListRetainsCommonValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, s := range []struct{ flag, env, value string }{
		{"timeout", "TIMEOUT", ""},
		{"timeout", "TIMEOUT", "CANARY"},
		{"timeout", "TIMEOUT", "0"},
		{"timeout", "TIMEOUT", "-1s"},
		{"timeout", "TIMEOUT", "999999999999999999999999"},
		{"db-host", "PGHOST", ""},
		{"db-user", "PGUSER", ""},
		{"db-name", "PGDATABASE", ""},
		{"db-port", "PGPORT", "CANARY"},
		{"db-port", "PGPORT", "0"},
		{"db-port", "PGPORT", "65536"},
		{"log-format", "EKS_LOG_FORMAT", "CANARY"},
		{"log-source", "EKS_LOG_SOURCE", "CANARY"},
	} {
		for _, source := range []string{"environment", "dotenv", "flag"} {
			t.Run(s.env+"/"+s.value+"/"+source, func(t *testing.T) {
				env := environment()
				env["INTERVAL"] = "CANARY"
				args := []string{}
				switch source {
				case "environment":
					env[s.env] = s.value
				case "dotenv":
					path := filepath.Join(t.TempDir(), "settings.env")
					delete(env, s.env)
					if err := os.WriteFile(path, []byte(s.env+"="+s.value+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--env-file", path)
				case "flag":
					args = append(args, "--"+s.flag+"="+s.value)
				}
				_, err := parse(t, append(args, "list"), env)
				if err == nil || strings.Contains(err.Error(), "CANARY") {
					t.Fatal("missing or unsafe common validation error", err)
				}
			})
		}
	}
	c, err := parse(t, []string{"list"}, environment())
	if err != nil || c.Timeout != 15*time.Second || c.Interval != 0 || c.CommandTimeout != 0 ||
		c.FirecallPollInterval != 0 || c.FirecallActivationTimeout != 0 || c.Duration != 0 {
		t.Fatal("unexpected listing defaults", err)
	}
}
