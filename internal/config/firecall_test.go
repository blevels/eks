package config

import (
	"testing"
	"time"
)

func TestFirecallPollingConfiguration(t *testing.T) {
	c, err := parse(t, []string{"12"}, environment())
	if err != nil || c.FirecallPollInterval != 5*time.Second || c.FirecallActivationTimeout != 2*time.Minute {
		t.Fatal(c, err)
	}
	env := environment()
	env["FIRECALL_POLL_INTERVAL"] = "7"
	env["FIRECALL_ACTIVATION_TIMEOUT"] = "3m"
	c, err = parse(t, []string{"12"}, env)
	if err != nil || c.FirecallPollInterval != 7*time.Second || c.FirecallActivationTimeout != 3*time.Minute {
		t.Fatal(c, err)
	}
	c, err = parse(t, []string{"--firecall-poll-interval", "1s", "--firecall-activation-timeout", "30s", "12"}, env)
	if err != nil || c.FirecallPollInterval != time.Second || c.FirecallActivationTimeout != 30*time.Second {
		t.Fatal(c, err)
	}
	for _, flag := range []string{"--firecall-poll-interval", "--firecall-activation-timeout"} {
		for _, value := range []string{"0", "-1s", "invalid"} {
			if _, err := parse(t, []string{flag, value, "12"}, environment()); err == nil {
				t.Fatalf("accepted %s=%s", flag, value)
			}
		}
	}
}
