package cli

import "eks/internal/config"

func safeError(err error, c config.Config) string {
	// Unknown errors cannot be made safe by replacing only known secrets.
	// Never decrypt credentials merely for logging.
	return "operation_failed"
}
