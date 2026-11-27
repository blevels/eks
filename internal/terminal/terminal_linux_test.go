//go:build linux

package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

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
	if _, err := ui.Password(context.Background()); !errors.Is(err, writeErr) {
		t.Fatalf("Password() error = %v, want prompt writer error", err)
	}

	restoredState, err := term.GetState(fd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoredState, originalState) {
		t.Fatal("terminal state was not restored after prompt write failure")
	}
}
