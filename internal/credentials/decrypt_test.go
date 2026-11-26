package credentials

import "testing"

// Independent fixtures generated with openssl enc -aes-128-cbc -salt
// -S 0102030405060708 -md <digest>; Salted__ header prepended for explicit -S.
// These are synthetic test credentials, not application secrets.
func TestDecryptOpenSSLFixtures(t *testing.T) {
	for _, tt := range []struct {
		name, cipher, digest, pass, want string
		bad                              bool
	}{
		{"sha256", "U2FsdGVkX18BAgMEBQYHCE7cMDd4cyD+typtkEMxGBQ=", "sha256", "fixture-only", "sample-user", false},
		{"trailing LF", "U2FsdGVkX18BAgMEBQYHCDSADyzWbnzTyMB4dp9WE5s=", "sha256", "fixture-only", "sample-user", false},
		{"multiple trailing LF", "U2FsdGVkX18BAgMEBQYHCN/+5gzlS0kw4HLqyVYWrjU=", "sha256", "fixture-only", "sample-user", false},
		{"preserve spaces", "U2FsdGVkX18BAgMEBQYHCPk72jW+hWKph+BBNEmVQHs=", "sha256", "fixture-only", " sample-user ", false},
		{"embedded newline", "U2FsdGVkX18BAgMEBQYHCH2+FfRM66nqx64QQff2df4=", "sha256", "fixture-only", "", true},
		{"empty after LF removal", "U2FsdGVkX18BAgMEBQYHCN5U2tPvkbdehNpxaMFFxtc=", "sha256", "fixture-only", "", true},
		{"md5", "U2FsdGVkX18BAgMEBQYHCKmQ2giWh7EacgbyRrmwbXk=", "md5", "fixture-only", "sample-user", false},
		{"wrapped", "U2FsdGVkX18BAgME\nBQYHCE7cMDd4cyD+typtkEMxGBQ=", "sha256", "fixture-only", "sample-user", false},
		{"wrong pass", "U2FsdGVkX18BAgMEBQYHCE7cMDd4cyD+typtkEMxGBQ=", "sha256", "wrong", "", true},
		{"wrong digest", "U2FsdGVkX18BAgMEBQYHCE7cMDd4cyD+typtkEMxGBQ=", "md5", "fixture-only", "", true},
		{"empty", "", "sha256", "fixture-only", "", true},
		{"bad base64", "!bad", "sha256", "fixture-only", "", true},
		{"missing header", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "sha256", "fixture-only", "", true},
		{"unsupported", "ignored", "sha512", "fixture-only", "", true},
		{"empty pass", "ignored", "sha256", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decrypt(tt.cipher, tt.pass, tt.digest)
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("got %q, %v; want %q bad=%v", got, err, tt.want, tt.bad)
			}
		})
	}
}
