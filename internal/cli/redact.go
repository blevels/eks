package cli

import (
	"strings"
	"unicode"

	"eks/internal/config"
	"eks/internal/credentials"
)

func safeError(err error, c config.Config) string {
	secrets := []string{c.DBPassword, c.Passphrase, c.EncryptedUser, c.EncryptedPassword, c.ProductionPassword}
	// Decrypt locally only to redact errors from remote/database boundaries.
	for _, encrypted := range []string{c.EncryptedUser, c.EncryptedPassword} {
		if value, e := credentials.Decrypt(encrypted, c.Passphrase, c.Digest); e == nil {
			secrets = append(secrets, value)
		}
	}
	message := err.Error()
	for _, value := range secrets {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
}
