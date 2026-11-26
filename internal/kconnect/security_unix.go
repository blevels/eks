//go:build !windows

package kconnect

import "os"

func secureDirectory(path string) error { return os.Chmod(path, 0700) }
