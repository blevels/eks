// Package credentials provides compatibility with OpenSSL enc's legacy salted format.
package credentials

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G501 -- explicit legacy OpenSSL compatibility only.
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// Decrypt accepts base64 Salted__ AES-128-CBC ciphertext made by openssl enc
// without -pbkdf2. The digest must match the generating OpenSSL installation.
// CBC does not authenticate ciphertext; this is compatibility, not a new format.
func Decrypt(encoded, passphrase, digest string) (string, error) {
	var newHash func() hash.Hash
	switch digest {
	case "sha256":
		newHash = sha256.New
	case "md5":
		newHash = md5.New
	default:
		return "", fmt.Errorf("unsupported decryption digest %q", digest)
	}
	if passphrase == "" {
		return "", errors.New("decryption passphrase is required")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil || len(raw) < 32 || string(raw[:8]) != "Salted__" || (len(raw)-16)%aes.BlockSize != 0 {
		return "", errors.New("invalid OpenSSL salted ciphertext")
	}
	var material, previous []byte
	for len(material) < 32 {
		h := newHash()
		// hash.Hash.Write is documented to never return an error.
		_, _ = h.Write(previous)
		_, _ = h.Write([]byte(passphrase))
		_, _ = h.Write(raw[8:16])
		previous = h.Sum(nil)
		material = append(material, previous...)
	}
	block, err := aes.NewCipher(material[:16])
	if err != nil {
		return "", fmt.Errorf("create credential cipher: %w", err)
	}
	plain := make([]byte, len(raw)-16)
	cipher.NewCBCDecrypter(block, material[16:32]).CryptBlocks(plain, raw[16:])
	padding := int(plain[len(plain)-1])
	if padding < 1 || padding > aes.BlockSize || !bytes.Equal(plain[len(plain)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return "", errors.New("credential decryption failed: check passphrase and digest")
	}
	// Bash command substitution strips all trailing LF characters from openssl
	// output. Preserve that behavior, but never trim spaces or other characters
	// that may be meaningful in the password.
	result := strings.TrimRight(string(plain[:len(plain)-padding]), "\n")
	if result == "" || strings.ContainsAny(result, "\x00\r\n") {
		return "", errors.New("decrypted credential is empty or contains control characters")
	}
	return result, nil
}
