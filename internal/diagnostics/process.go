package diagnostics

import "os"

func processID() int { return os.Getpid() }
