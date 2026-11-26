# Terminal password verification

## Checks performed

- `go test ./internal/terminal` passed on Linux; this runs the synthetic
  `readSecret` cases for hidden-input parsing, ASCII and Unicode backspace,
  Ctrl-C, Ctrl-D, and reader EOF.
- `go test -race ./internal/terminal` passed on Linux.
- `go test ./...` passed on Linux.
- The terminal package test binaries compiled for `windows/amd64` and
  `darwin/amd64`.
- The full application cross-compiled for Linux, macOS, and Windows on amd64
  and arm64.

## Platform limits

This environment is Linux, so the Windows and macOS test binaries could not be
executed here. Cross-compilation confirms the platform-specific terminal
implementations compile; it does not confirm their runtime echo or restoration
behavior. The existing synthetic parser tests do not open a real terminal and
therefore do not verify that password characters are hidden.

The code saves the terminal mode before reading and restores it after input,
reader errors, Ctrl-C input, or context cancellation. The command's context is
cancelled by `os.Interrupt` and `SIGTERM`. On Unix-like systems, raw mode
disables signal-character processing, so Ctrl-C reaches the secret reader as
byte 3 and takes the normal restoration path. Windows raw mode disables
processed input, allowing the same byte-level Ctrl-C handling. Windows reports
console close, logoff, and shutdown notifications as `SIGTERM`, but the
operating system may still terminate the process if it does not exit promptly.

Actual hidden-echo and terminal-state restoration checks still need to run with
synthetic keystrokes on native Windows and macOS terminals.
