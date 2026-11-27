# Architecture, analysis, and migration notes

## Baseline and scope

The original POC works in its intended environment. `eks.sh` is a retyped copy;
syntax errors and omissions in this copy are transcription issues, not evidence
that the original POC failed. The original file is preserved unchanged.

The CLI targets Go 1.25 and Linux/macOS/Windows on amd64/arm64. This phase migrates
the shell workflow only. No Python code was supplied or migrated.

## Phase 1 analysis summary

1. Select list/namespace ID/namespace name from one argument.
2. Read namespace, account, role, and environment through PostgreSQL.
3. Select among multiple ID matches and print the namespace context.
4. Decrypt the configured username and password.
5. For production, prefix the identity with `s` and collect a CyberArk password.
6. Obtain an OAuth token, inspect active firecalls, and offer creation if needed.
7. Invoke `kconnect use eks` with SAML/PingNTLM and the IAM role.
8. Wait 900 seconds after completion and repeat.

Inputs include namespace identifiers, script constants, encrypted credentials,
interactive choices/password/ticket, SQL rows, API responses, and subprocess
results. Outputs include tables/details, prompts, progress, diagnostics, and
inherited tool output in the shell. Intended explicit shell exits include 0 and
1; shell/tool failures can have other statuses. There is no complete reliable
transcription-level exit-code contract.

Side effects are database reads, OAuth requests, firecall creation, and
kconnect-managed authentication/configuration updates. No temporary files are
explicitly used by the supplied shell, so no script-local temp-file race was
identified. External tools are resolved through PATH and may read their own
home-directory, trust, and working-environment configuration.

Notable transcription problems include the positional assignment, literal
container name, missing function arguments, undefined variables, missing command
continuations, malformed list expression/tests, an empty firecall condition,
and a yes/no switch on an empty string. The listing header and returned field
order also disagree.

Shell hazards beyond transcription: interpolated SQL, unquoted command arguments,
passwords in arguments, disabled OAuth TLS verification, incomplete timeouts,
regex JSON parsing, delimiter-based records, inherited global state, and
context-dependent `set -e`. Functions invoked in `if` conditions do not receive
the same errexit behavior as ordinary calls. `pipefail` exposes pipeline failure
but does not validate response meaning; pipeline loops can run in subshells.

## Native mappings

| Shell dependency | Go implementation |
|---|---|
| Docker + psql | `database/sql` and `pgx/v5`, direct RDS TLS |
| curl | bounded `net/http` requests, verified TLS, no redirects |
| jq / JSON grep / cut | typed `encoding/json` |
| cut / wc / tr | typed fields, slices, string operations |
| column | `text/tabwriter` |
| date / sleep | `time` and context-aware timers |
| read / select | terminal interaction boundary with secure password input |
| openssl | native AES-CBC/OpenSSL-compatible key derivation |
| kconnect | bounded `exec.CommandContext`, no shell |

No Bash command strings, Docker client, psql, curl, jq, or OpenSSL are needed at
runtime. Native code avoids temporary files except for protected kconnect
credential delivery.

## Layout and dependencies

```text
cmd/eks/                 thin signal-aware executable
internal/cli/            dependency assembly, help, safe errors, exit mapping
internal/config/         flags, dotenv, environment precedence, validation
internal/namespace/      read-only SQL and typed namespace rows
internal/credentials/    legacy encrypted input compatibility
internal/firecall/       OAuth and firecall HTTP contract
internal/kconnect/       protected credential file, subprocess boundary
internal/terminal/       interactive selection and password handling
internal/app/            workflow, refresh loop, text/JSON result output
```

The module is named `eks` (local CLI module, no assumed remote repository path).
See `go.mod`/`go.sum` for exact versions.

- `pgx/v5` supplies PostgreSQL wire protocol support absent from the standard
  library, through its `database/sql` adapter. **Approved target change:** Go
  1.24-compatible pgx versions checked during implementation were blocked for a
  critical vulnerability. The user approved Go 1.25 to use the current driver,
  replacing the temporary lib/pq alternative without bypassing that restriction.
- `x/term` provides portable no-echo password entry.
- `x/sys/windows` supplies Windows ACL APIs for temporary credential protection.
- Staticcheck is a pinned development tool, not a runtime dependency.

Configuration and per-run state are explicitly passed; dependencies are narrow
interfaces at their consumers. A compile-time-set version string is the only
entrypoint variable. Data is typed rather than pipe-delimited internally.

## Confirmed decisions and API assumptions

The user confirmed encrypted credentials in dotenv, the shown OpenSSL encryption
format, kconnect 0.5.24, and production-only firecall. The user requested best
judgment from the script for API details, with later manual adjustment as needed.
No further API specification is assumed.

- Retain the `eks.namespaces` / `eks.accounts` join on `env`.
  The list includes `n.ns_id`, resolving the five-column header discrepancy.
- The positional length heuristic remains; explicit selectors are additive.
- The production environment marker is exactly `prod`, not case-insensitive
  guessing. The approved environment allowlist is exactly `dev`, `sit`, `uat`,
  and `prod`; all other values fail validation. The `s` prefix is applied to the freshly decrypted base user once per
  cycle. Email uses that base user and configured domain.
- OAuth follows the script's operation order: production effective credentials
  become `client_id`/`client_secret`; grant is `client_credentials`, scope is
  `AppIdClaimsTrust`, and the token field is `access_token`.
- Firecall listing is a JSON array. The approved authorization policy requires
  exact `active` status, account, and the selected role's full `iamRoleArn`.
  This is intentionally stricter than the original account/status-only match.
  `requester` and `end_time` remain display fields, not authorization filters.
- Creation posts to `FIRECALL_URI`, distinct from list `FIRECALL_URL`.
  Request fields: `iam_role_arn` array, string `duration_in_hours`, `ticket`,
  and `email`. Response is an array containing `firecall_id`, `status`, `message`.
  Require at least one `SUCCESS` record with a nonempty/non-null ID.
- Successful creation is followed by activation verification, an approved
  deviation from the POC's immediate connection. Check the list immediately,
  then every 5 seconds for at most 2 minutes (both configurable). Require an
  active account-and-role match before connection. Timeout, cancellation, or
  an API failure after bounded eligible read-only retries stops the operation
  without retrying creation. The list does
  not expose request IDs, so this confirms authorization, not that a particular
  newly created request is active. No expiration parsing was added.
- HTTP 2xx is required, not merely the presence of a status string.
  Empty/no-match lists are not themselves authorization. An API failure never
  authorizes a connection; declining creation stops it.
- Only read-only firecall-list GETs have automatic retries: three total attempts
  for transient connection failures, timeouts, or HTTP 408/429/502/503/504.
  Exponential backoff starts at 500ms then 1s, with up to 25% additive jitter
  and a 5s backoff cap. Retry-After is a minimum delay, not capped by the
  backoff cap; a delay that cannot fit the remaining deadline stops the request.
  All attempts, body reads, and waits share one configured HTTP timeout budget,
  further limited by the caller's context and activation deadline.
  No automatic OAuth or creation retries, particularly after an uncertain result.
  Permanent DNS, TLS/certificate, authentication, invalid JSON and response-size
  failures are not retried. This is an approved reliability change from the POC.
- OpenSSL default digest varies by generation; default SHA-256 and explicit MD5
  compatibility are supported. No PBKDF2 or authenticated-encryption migration
  is imposed on the existing format.
- Decrypted credentials lose trailing LF characters only, matching Bash command
  substitution, including ciphertext prepared with ordinary `echo`. Spaces remain
  meaningful; embedded newlines and empty effective credentials are rejected.

## Intentional behavior differences

1. RDS replaces the local Docker database; TLS verification is mandatory.
2. Obvious transcription errors are corrected, including the list field order
   and malformed/dead prompt paths.
3. SQL parameters replace string interpolation; namespace/account/role rows are
   validated before use. Multiple name matches prompt rather than merge rows.
4. No hardcoded decryption passphrase; matching passphrase comes from secret
   configuration. Encrypted credentials are loaded from dotenv/environment.
5. kconnect credentials use its 0.5.24 configuration loader instead of arguments.
   Protected temporary JSON config is accepted by its JSON/YAML decoder.
   `--no-history` avoids adding connection-history entries; AWS/kubeconfig
   updates still occur. Raw child output is suppressed to protect secrets.
6. HTTP/database/subprocess deadlines, cancellation, bounded HTTP/output sizes,
   and explicit errors replace shell implicit failure behavior.
7. Prompts/logs are on stderr; tables/data remain on stdout. Date formatting is
   RFC3339. Corrected connection text omits transcription misspellings.
8. A successful create prints the actual ID/status/message rather than undefined
   list-loop variables.
9. Empty production passwords and required tickets are errors; no silent
   fallback to the nonproduction password.
10. Defined CLI exits are 0/1/2/130. Child exits remain available as error metadata.
11. Help/version, one-shot, JSON Lines, explicit selectors, configurable timeouts,
    endpoint flags, and verbosity are additive. Flags precede positionals.
12. Placeholder service endpoints are not used as live defaults. Production-only
    endpoint validation happens after discovery, before production authentication.
13. Zero namespace results from `list` produce an empty table/JSON array and
    succeed; zero results for a connection fail.
14. Refresh state is local to each cycle, preventing an old creation choice from
    automatically opening repeated requests.
15. HTTP failures are typed, safe categories with status codes where available;
    raw transport errors are not retained in returned chains because they may
    expose URLs or secrets through error inspection. Cancellation and timeout
    remain detectable through safe context sentinels and timeout classification.
    HTTP attempt diagnostics use the common event contract: recoverable attempts
    and retry scheduling at warning level, final failures at error, cancellation
    at info, and successes at debug. Read-only retries
    follow the bounded exponential-backoff policy above; activation polling
    remains separate and does not reset any request deadline.
16. Diagnostics now share a versioned `slog` schema with run/cycle/operation
    correlation and safe fixed failure codes. `--log-format`, `--log-source`,
    and opt-in bounded `--log-file` retention are additive; stdout result formats
    and ordinary exits are unchanged. A requested sink initialization/runtime
    failure returns 1, after cleanup where initialized. User refusal is logged
    as normal cancellation but retains exit 1. No authorization, retries,
    cache freshness, prompt behavior, credential format, or child arguments were
    changed for instrumentation. Unknown error text is not emitted, and redaction
    no longer performs extra decryption. See [diagnostics](docs/diagnostics.md)
    for catalog, failure ownership, tests, examples, and delivery limits.

## Verification and remaining risks

Unit tests cover synthetic OpenSSL fixtures, config/precedence and malformed
dotenv, SQL binding/scan failures through a driver fake, HTTP contracts and
failures, terminal input/EOF, subprocess arguments/temp cleanup, secret-safe
errors, production/nonproduction paths, selections, output, and refresh waits.
Golden expectations capture the intended namespace text/JSON contract rather
than executing the retyped shell against live resources.

The Makefile provides formatting, race tests, vet, staticcheck, and six static
cross-builds. Windows-only regression tests now inspect the protected
owner-rights ACL and inherited file ACL using synthetic credentials, but the
Linux development environment cannot execute them; native Windows ACL behavior
and secure password-terminal restoration still need native platform validation.
Linux PTY diagnostic tests exercise password success, EOF, cancellation, output
failure and surfaced restoration failure; they do not establish native macOS or
Windows terminal behavior. New native Windows log-file ACL tests are supplied,
but are not claimed to have run in this Linux workspace.
No kconnect binary is installed here, so actual 0.5.24 invocation has not been
exercised. Real RDS, CyberArk, OAuth/firecall, and EKS access have not been
exercised with user credentials. The direct subprocess is cancelled; arbitrary
detached descendants are outside the portable process boundary.

Legacy CBC ciphertext is unauthenticated. Temporary plaintext credentials can
survive forced termination; normal cleanup is not a secure disk erase. Go
strings are not guaranteed to be zeroized. kconnect itself and its credential
stores remain part of the trust boundary.

The account/role/status firecall policy does not validate requester or expiry.
Activation checks cannot correlate a list record to the newly created request ID.
Service-specific response types (including string account IDs)
follow the supplied jq expressions and can be adjusted in
the isolated firecall package if live responses differ. Request IDs accept both
JSON strings and numbers, retaining large numeric IDs without float rounding.
