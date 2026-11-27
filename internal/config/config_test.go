package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func environment() map[string]string {
	return map[string]string{"PGHOST": "db.example.invalid", "PGUSER": "tester", "ENC_USERNAME": "cipher", "ENC_PASSWORD": "cipher", "DECRYPT_PASSPHRASE": "test-only", "IDP_ENDPOINT": "https://idp.example.invalid"}
}
func parse(t *testing.T, args []string, env map[string]string) (Config, error) {
	t.Helper()
	return Parse(args, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, io.Discard)
}

func TestParse(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tt := range []struct {
		name         string
		args         []string
		mode, target string
		list, bad    bool
	}{
		{"id", []string{"12"}, "id", "12", false, false},
		{"name", []string{"payments"}, "name", "payments", false, false},
		{"list", []string{"list"}, "", "list", true, false},
		{"long id", []string{"--id", "123456"}, "id", "123456", false, false},
		{"short name", []string{"--name", "ab"}, "name", "ab", false, false},
		{"named list", []string{"--name", "list"}, "name", "list", false, false},
		{"missing", nil, "", "", false, true},
		{"conflict", []string{"--id", "12", "--name", "ab"}, "", "", false, true},
		{"empty", []string{"--id=", "list"}, "", "", false, true},
		{"extra", []string{"12", "13"}, "", "", false, true},
		{"trailing flag", []string{"12", "--once"}, "", "", false, true},
		{"bad duration", []string{"--interval", "0", "12"}, "", "", false, true},
		{"negative timeout", []string{"--timeout", "-1s", "12"}, "", "", false, true},
		{"bad port", []string{"--db-port", "65536", "list"}, "", "", false, true},
		{"unknown", []string{"--bogus", "12"}, "", "", false, true},
		{"help", []string{"--help"}, "", "", false, false},
		{"version", []string{"--version"}, "", "", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parse(t, tt.args, environment())
			if (err != nil) != tt.bad {
				t.Fatalf("config=%+v err=%v", c, err)
			}
			if !tt.bad && (c.Mode != tt.mode || c.Target != tt.target || c.List != tt.list) {
				t.Fatalf("unexpected config %+v", c)
			}
		})
	}
}

func TestPrecedenceAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PGHOST=file.example.invalid\nPGUSER=file\nINTERVAL=800\nENC_USERNAME=filecipher\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := environment()
	env["PGHOST"] = "env.example.invalid"
	env["INTERVAL"] = "700"
	c, err := parse(t, []string{"--env-file", path, "--db-host", "flag.example.invalid", "--interval", "10m", "list"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.DBHost != "flag.example.invalid" || c.Interval != 10*time.Minute || c.DBName != "postgres" || c.Region != "us-east-1" {
		t.Fatalf("bad precedence/defaults %+v", c)
	}
	c, err = parse(t, []string{"--env-file", path, "list"}, env)
	if err != nil || c.DBHost != "env.example.invalid" || c.Interval != 700*time.Second {
		t.Fatalf("bad env precedence: %+v %v", c, err)
	}
	delete(env, "PGHOST")
	delete(env, "INTERVAL")
	c, err = parse(t, []string{"--env-file", path, "list"}, env)
	if err != nil || c.DBHost != "file.example.invalid" || c.Interval != 800*time.Second {
		t.Fatalf("bad dotenv: %+v %v", c, err)
	}
}

func TestMissingFilesAndSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := parse(t, []string{"--env-file", filepath.Join(t.TempDir(), "missing"), "list"}, environment()); err == nil {
		t.Fatal("missing explicit file accepted")
	}
	if _, err := parse(t, []string{"list"}, map[string]string{}); err == nil {
		t.Fatal("missing database settings accepted")
	}
	if _, err := parse(t, []string{"--help"}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	env := environment()
	env["ENC_USERNAME"] = ""
	if _, err := parse(t, []string{"12"}, env); err == nil {
		t.Fatal("empty ciphertext accepted")
	}
	if _, err := parse(t, []string{"list"}, env); err != nil {
		t.Fatal("list should not need credentials", err)
	}
}

func TestReadEnv(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		bad               bool
	}{
		{"basic", " # comment\nexport A=abc==\n", "abc==", false},
		{"single", "A='abc # def'\n", "abc # def", false},
		{"double", "A=\"abc\\nxyz\"\n", "abc\nxyz", false},
		{"comment", "A=abc # comment\n", "abc", false},
		{"literal", "A=$(do-not-run)\n", "$(do-not-run)", false},
		{"duplicate", "A=a\nA=b\n", "", true},
		{"bad key", "A-B=bad\n", "", true},
		{"missing equals", "A\n", "", true},
		{"unclosed", "A='abc\n", "", true},
		{"oversize", strings.Repeat("A=x\n", 300000), "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadEnv(strings.NewReader(tt.input))
			if (err != nil) != tt.bad {
				t.Fatalf("error %v", err)
			}
			if !tt.bad && got["A"] != tt.want {
				t.Fatalf("got %q want %q", got["A"], tt.want)
			}
		})
	}
}
func TestHTTPS(t *testing.T) {
	for _, tt := range []struct {
		s   string
		bad bool
	}{
		{"https://example.invalid/a", false}, {"http://example.invalid", true}, {"", true},
		{"https://user:password@example.invalid", true}, {"https://example.invalid/#part", true},
		{"https:///missing", true},
	} {
		if (HTTPS(tt.s) != nil) != tt.bad {
			t.Errorf("unexpected validation for %q", tt.s)
		}
	}
}
