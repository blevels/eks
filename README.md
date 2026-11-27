# EKS connection CLI

Go 1.25 implementation of the workflow in `eks.sh`: discover a namespace from
PostgreSQL, obtain production firecall access where required, and refresh an EKS
connection using **kconnect 0.5.24**. The supplied shell is retained unchanged as
the transcription of the working POC. See [MIGRATION.md](MIGRATION.md) for analysis,
assumptions, and intentional differences.

## Build and quality checks

```sh
go mod download
make build VERSION=1.0.0
make test                  # go test -race ./...
make lint                  # formatting, go vet, pinned staticcheck
make cross-compile         # Linux/macOS/Windows, amd64 and arm64
```

The development environment may provide a newer Go compiler. To reproduce the
target-version checks exactly, use:

```sh
GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.25.14 make check build
```

This enables checksum verification for the downloaded target toolchain.

All generated binaries go into ignored `generated-artifacts/`. GNU Make and a
POSIX shell are needed for the Makefile, not for running the resulting CLI.
Equivalent native build (PowerShell users can set `$env:CGO_ENABLED="0"` first):

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=1.0.0" -o eks ./cmd/eks
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o generated-artifacts/eks-linux-arm64 ./cmd/eks
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o generated-artifacts/eks-darwin-arm64 ./cmd/eks
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o generated-artifacts/eks-windows-amd64.exe ./cmd/eks
go test -race ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
```

No web server or browser is involved. Run the executable in a terminal.

## Configure

Copy `.env.example` to `.env` and fill it locally. Never commit the real `.env`.
An explicitly selected dotenv file must exist; a missing default `.env` is fine
when configuration is supplied through the environment. The parser accepts
`KEY=value`, single/double quoted values, comments, and optional `export`.
It does not execute shell expressions, expand variables, or support multiline
values. Double-quoted values use Go-style escapes. Values containing ` #` should
be quoted. Files are limited to 1 MiB; duplicate keys are errors.

**Precedence:** command-line flags > process environment > dotenv > defaults.
An explicitly empty environment value overrides dotenv, including for secrets.
No secret can be supplied by CLI flag. Keep local dotenv files owner-readable
only (`chmod 600 .env` on Unix; restrict file ACLs on Windows).

| Settings | Required/default |
|---|---|
| `PGHOST`, `PGUSER` | Required for all database commands |
| `PGPORT`, `PGDATABASE` | `5432`, `postgres` |
| `PGPASSWORD` | Database password if required by the server |
| `PGSSLROOTCERT` | Optional PEM CA bundle; system roots otherwise |
| `ENC_USERNAME`, `ENC_PASSWORD` | Required encrypted credentials for connection commands |
| `DECRYPT_PASSPHRASE` | Matching encryption passphrase; no hardcoded default |
| `DECRYPT_DIGEST` | `sha256`; set `md5` explicitly for old OpenSSL ciphertext |
| `IDP_ENDPOINT` | Required HTTPS SAML endpoint for connection commands |
| `IDP_PROVIDER`, `REGION` | `PingNTLM`, `us-east-1` |
| `EMAIL_DOMAIN` | `test.com`, matching the POC; override for your organization |
| `KCONNECT_BIN` | `kconnect`, or an explicit executable path |
| `OAUTH_URL`, `FIRECALL_URL`, `FIRECALL_URI` | Required HTTPS endpoints for `prod` only |
| `PROD_PASSWORD` | Optional secret; otherwise securely prompted each production refresh |
| `CM_TICKET` | Optional preset; prompted when opening a request if absent |
| `DURATION_IN_HRS` | `8` |
| `INTERVAL`, `TIMEOUT`, `COMMAND_TIMEOUT` | `900s`, `15s`, `2m` |
| `FIRECALL_POLL_INTERVAL`, `FIRECALL_ACTIVATION_TIMEOUT` | `5s`, `2m`; overridden by `--firecall-poll-interval` and `--firecall-activation-timeout` |

RDS connections always use **TLS verify-full**; configure a trusted RDS CA bundle
where system trust is insufficient. HTTP uses system certificate trust and
rejects redirects. There is no insecure TLS fallback or hidden Docker database.

### Existing encrypted credentials

The CLI reads the salted Base64 OpenSSL format already used by the POC:
AES-128-CBC, legacy `EVP_BytesToKey` derivation (not PBKDF2), PKCS#7 padding.
Store the encryption command's output as `ENC_USERNAME` / `ENC_PASSWORD`.
For new encryption without putting the passphrase in command arguments, supply
it through your local secret environment and use OpenSSL's
`-pass env:DECRYPT_PASSPHRASE`; feed the value being encrypted through stdin.
This is an operator-side preparation step; the Go executable does not run OpenSSL.

Existing ciphertext remains compatible when the original passphrase and digest
are configured. SHA-256 matches modern OpenSSL defaults; old installations may
have used MD5. No silent fallback is attempted. After decryption, trailing LF
characters are removed just as Bash command substitution removes them; spaces
are preserved. Embedded LF, CR, NUL, and empty effective credentials are rejected.
AES-CBC provides no integrity
protection. Keeping the passphrase next to ciphertext is obfuscation rather than
strong protection; prefer supplying the passphrase through a secret environment.
No passphrase, password, ciphertext, or token should be pasted into chat.

## Usage

```sh
./generated-artifacts/eks --help
./generated-artifacts/eks --version
./generated-artifacts/eks list
./generated-artifacts/eks --once payments
./generated-artifacts/eks --id 12345
./generated-artifacts/eks --name ab
./generated-artifacts/eks --interval 15m --timeout 20s payments
./generated-artifacts/eks --json list
./generated-artifacts/eks --json --once payments
./generated-artifacts/eks --env-file /secure/path/operator.env --once payments
```

**Options must precede a positional selector.** Positionals longer than three
characters are names; shorter values are IDs, preserving the POC rule. Explicit
`--id`/`--name` bypass that heuristic. Multiple rows prompt for selection; JSON
mode refuses ambiguity. `list` never decrypts credentials or invokes kconnect.

Without `--once`, successful connection cycles repeat with a delay of 900 seconds
after each connection completes. Errors stop the loop. Ctrl-C / SIGTERM cancels
network calls, the active direct subprocess, terminal waits, and refresh waits.
For production the base username gets an `s` prefix, as in the POC.

Only the exact environment names `dev`, `sit`, `uat`, and `prod` are accepted;
unknown names are rejected, and only `prod` requires production authorization.
Production checks an OAuth-authenticated firecall list for exact `active` status,
the chosen `account_id`, and `iamRoleArn` matching the selected role's full ARN.
If none exists, the user must confirm creation and provide a ticket. After a
successful creation, the CLI checks activation immediately, then every 5 seconds
for up to 2 minutes, configurable using the settings above. Creation is submitted
only once. Timeout, cancellation, or an API error after eligible lookup retries
stops without connecting.
An active record for that account and role authorizes connection; requester and
expiration are not checked, and the list contract cannot correlate activation
to a specific created request ID. See MIGRATION.md for service assumptions.

### HTTP diagnostics and retries

The client reuses one HTTP client during a run. Each logical HTTP operation has
one `--timeout` / `TIMEOUT` budget (15 seconds by default), including all attempts,
response reads, and retry delays. A shorter caller deadline always wins; activation
checks also stay within `--firecall-activation-timeout`. The timeout does not reset
on each retry.

Only the read-only firecall-list GET is automatically retried, at most **three
total attempts**:

- Retry transient connection failures and timeouts, plus HTTP **408, 429, 502,
  503, and 504**. Permanent DNS failures, TLS validation errors, authentication
  failures, invalid JSON, and oversized responses are not retried.
- Exponential backoff starts at **500 ms**, then **1 second**, plus random
  additive jitter up to 25%. The backoff, including jitter, is capped at
  **5 seconds**; the three-attempt limit normally uses only the first two delays.
- A valid `Retry-After` (seconds or HTTP date) is a minimum delay, even when longer
  than the backoff cap. If the required delay cannot fit in the remaining budget,
  stop immediately without retrying early or extending the deadline.
- OAuth token POSTs and firecall-creation POSTs are **never automatically retried**.
  A failed or uncertain creation result must be checked before manually retrying.
  No tokens or firecall authorization results are cached.

Failure retries are separate from activation polling: a successful lookup with
no active match is not an HTTP failure. It returns to the existing activation
polling loop, which waits the configured polling interval.

HTTP attempt failures are logged on stderr with operation name, attempt number,
duration, safe category, and HTTP status when available. `--verbose` also shows
successful attempts and scheduled retry delays. Raw URLs, transport error text,
headers, credentials, tokens, and request/response bodies are excluded.
Returned errors preserve safe cancellation/timeout checks without exposing the
underlying URL-bearing transport error.

### Output and logging

- Human-readable result tables/details go to stdout.
- Prompts, progress, and `log/slog` diagnostics go to stderr.
- `--verbose` enables debug diagnostics without printing credentials.
- `--json` emits **JSON Lines** events: `namespaces`, `namespace`,
  `active_firecalls`, `created_firecalls`, and `connected`.
- JSON mode never prompts or creates a firecall. For production it requires
  `PROD_PASSWORD` and an already active firecall. Namespace ambiguity fails.
- Raw kconnect output is captured separately and suppressed because it can
  contain secrets or corrupt JSON. Its exit code is retained in diagnostics.

Run `--help` for the complete flag list. Exit codes:

| Code | Meaning |
|---|---|
| 0 | Success, help, version, or successful one-shot connection |
| 1 | Runtime failure, no namespace, denied/missing firecall, or child failure |
| 2 | Invalid command-line or initial configuration |
| 130 | Context cancelled by Ctrl-C or SIGTERM |

Missing production-only configuration is detected once the namespace environment
is known, before authentication, and currently returns runtime failure (1).
A child timeout returns 1; external kconnect codes are diagnostic fields rather
than unconditionally becoming this tool's exit code.

## Runtime dependencies and platform limits

Install `kconnect` 0.5.24 and any helpers required by that distribution
(upstream EKS documentation names `aws-iam-authenticator`). RDS, the identity
provider, AWS discovery APIs, and production firecall endpoints must be reachable.
Namespace alone may not disambiguate multiple discoverable EKS clusters; the
existing kconnect discovery/no-input behavior remains in control.

Passwords are passed to kconnect using a temporary configuration, not command
arguments. The directory is 0700 and file 0600 on Unix. Windows uses an explicit
protected owner-only inheritable DACL before writing credentials. The file
contains plaintext for the duration of the child invocation and is removed on
normal success/failure/cancellation. Forced process termination, a crash, or
power loss can leave it behind; removal is not a secure disk wipe.
The child's environment is allowlisted to avoid inheriting unrelated secrets.
Connection history is disabled; kconnect still updates AWS/kubeconfig state.

The direct child is cancelled on shutdown; arbitrary detached descendants are
not guaranteed to be terminated. Windows signal behavior differs from Unix.
Windows-only tests inspect the protected owner-rights directory ACL and the
inherited ACL on a synthetic config file. Cross-compilation alone does not run
those checks or verify native password-terminal restoration. The Linux AMD64
kconnect 0.5.24 binary was run with this temporary JSON configuration under an
isolated home directory and accepted it, proceeding to a SAML request pointed
only at loopback; the request was intentionally refused, so no AWS or identity
provider service was contacted. The child command arguments contained no
credentials. Unit tests verify temporary-file removal after success, child
failure, and cancellation. This confirms config and invocation compatibility,
not a real provider login, AWS discovery, or native Windows/macOS behavior.
Unit tests use synthetic credentials and database/HTTP/command fakes; live
access is not needed for them.
