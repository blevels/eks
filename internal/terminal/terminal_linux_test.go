//go:build linux

package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"eks/internal/diagnostics/diagtest"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type passwordScriptWriter struct {
	send   func() error
	writes int
	failAt int
}

func (w *passwordScriptWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errors.New("CANARY prompt/write")
	}
	if w.writes == 1 && w.send != nil {
		if err := w.send(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func TestPasswordDiagnosticNativeOutcomes(t *testing.T) {
	for _, scenario := range []string{"success", "eof", "canceled", "restore", "final_output", "primary_and_restore"} {
		t.Run(scenario, func(t *testing.T) {
			master, slave := openPTY(t)
			ctx, logs := diagtest.New(t)
			w := &passwordScriptWriter{send: func() error {
				text := "CANARY-password\n"
				if scenario == "eof" {
					text = "\x04"
				}
				if scenario == "canceled" {
					text = "\x03"
				}
				_, err := master.Write([]byte(text))
				return err
			}}
			if scenario == "final_output" {
				w.failAt = 2
			}
			if scenario == "primary_and_restore" {
				w.failAt = 1
			}
			ui := Terminal{In: slave, Out: w}
			if scenario == "restore" || scenario == "primary_and_restore" {
				ui.restore = func(fd int, state *term.State) error {
					if err := term.Restore(fd, state); err != nil {
						return err
					}
					return errors.New("CANARY restore")
				}
			}
			_, err := ui.Password(ctx)
			if (err == nil) != (scenario == "success") {
				t.Fatal(err)
			}
			logs.Contract()
			logs.NoSecrets("CANARY", "CyberArk", "Enter production")
			if scenario == "restore" || scenario == "primary_and_restore" {
				if logs.Event("restore.outcome")["level"] != "ERROR" {
					t.Fatal(logs.Text())
				}
			}
			if scenario == "canceled" {
				if !errors.Is(err, context.Canceled) || logs.Event("password.outcome")["level"] != "INFO" {
					t.Fatal(err, logs.Text())
				}
			}
		})
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(masterFD), "/dev/ptmx")
	t.Cleanup(func() {
		if err := master.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := slave.Close(); err != nil {
			t.Error(err)
		}
	})
	return master, slave
}

func TestPasswordRestoresTerminalWhenPromptWriteFails(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())
	originalState, err := term.GetState(fd)
	if err != nil {
		t.Fatal(err)
	}

	writeErr := errors.New("prompt output failed")
	ui := Terminal{In: slave, Out: failingWriter{err: writeErr}}
	ctx, logs := diagtest.New(t)
	if _, err := ui.Password(ctx); !errors.Is(err, writeErr) {
		t.Fatalf("Password() error = %v, want prompt writer error", err)
	}
	logs.Contract()
	logs.Event("restore.outcome")

	restoredState, err := term.GetState(fd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoredState, originalState) {
		t.Fatal("terminal state was not restored after prompt write failure")
	}
}
