package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"eks/internal/cli"
)

// version is set by the Makefile's -ldflags; it is immutable at runtime.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], version, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
