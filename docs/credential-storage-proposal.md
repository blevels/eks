# Credential storage proposal

**Status: design for approval only.** Nothing in this document is implemented.
No credential code, dependencies, configuration, or real secrets were changed.

## Recommendation

Add an **opt-in, versioned AES-256-GCM credential bundle**, using Argon2id to
derive its key from a separately supplied passphrase. Preserve the existing
OpenSSL format as the default compatibility mode. Do not migrate automatically.

Keep the new passphrase out of the new dotenv file. Use a hidden local terminal
prompt or a runtime environment supplied by the operator's existing secret
delivery mechanism. Never supply secrets as CLI arguments or print them.

An OS credential-store provider is a useful later option, but should not be the
first migration: the CLI must also work on headless Linux and across all six
supported OS/architecture combinations.

This proposal covers the existing encrypted **base username/password pair**.
It does not silently move `PGPASSWORD`, `PROD_PASSWORD`, tokens, or other
configuration into the bundle.

## 1. Existing protection and limitations

The current implementation accepts OpenSSL `Salted__` AES-128-CBC input using
legacy `EVP_BytesToKey`-style derivation with SHA-256 or explicitly selected MD5.
It checks padding and rejects empty credentials and embedded NUL/CR/LF; it
strips trailing LF characters to preserve shell-command-substitution behavior.

- The principal problems are unauthenticated ciphertext and fast password
  derivation, not simply AES's 128-bit key size.
- CBC padding/field validation does not authenticate ciphertext or reliably
  establish that the passphrase was correct. A modified value can occasionally
  decrypt into plausible data.
- Salt prevents precomputed attacks against many identical passphrases, but
  does not make this legacy derivation expensive to brute-force.
- Storing the passphrase next to the ciphertext defeats confidentiality against
  an attacker who obtains both. Moving it to an environment variable is a
  delivery choice, not a guarantee against a compromised process or same-user
  account.
- Authenticated encryption protects against unauthorized modification of a
  bundle, not deletion, replacement with another valid old bundle, or rollback.
  Anti-rollback requires trusted state outside the file and is not promised.
- This change cannot protect secrets already exposed, an unlocked compromised
  workstation, process memory, or plaintext required by kconnect. Buffer clearing
  is best effort; Go strings/runtime copies cannot be guaranteed zeroized.

The source `.env` remains untouched, including any old passphrase it contains.
Leaving it available is a recovery measure, not a security improvement; its
eventual removal is a separate, explicit operator decision.

## 2. Options

| Option | Benefits | Costs and operational limits |
|---|---|---|
| Versioned encrypted file with Argon2id + AES-GCM | Portable, offline, works on headless hosts, supports current dotenv workflow, authenticates data and metadata | Still needs secure passphrase delivery; adds the reviewed Go `x/crypto/argon2` package; 64 MiB KDF workspace plus runtime overhead; encrypted files remain copyable for offline attacks |
| Versioned encrypted file with PBKDF2-HMAC-SHA-256 + AES-GCM | Go 1.25 standard library has `crypto/pbkdf2`; avoids a new runtime module | CPU-hard, not memory-hard; parameters and target latency need a separate approved profile. A compliance/FIPS requirement could change this choice; do not claim compliance from algorithm names alone |
| Native OS credential store | Can avoid a separately managed application passphrase; integrates with the workstation's login/unlock policy | macOS Keychain, Windows Credential Manager, and Linux Secret Service require different adapters and native testing. Linux often needs a running unlocked D-Bus secret-service session; headless/CI availability is not assured. It is not automatically portable or immune to same-user compromise |

For an OS provider, review a maintained adapter or native bindings before choosing
a dependency. Do not shell out with secret-bearing arguments. A locked or absent
store must fail explicitly, not fall back to plaintext or another provider.

**Dependency decision:** Argon2id earns one narrowly scoped additional module
because memory hardness materially improves resistance to offline guessing.
Use a secure pinned `golang.org/x/crypto` release compatible with the approved
Go target; the latest online documentation is not proof of compiler compatibility.
Do not install anything during this review or downgrade the approved toolchain.

## 3. Concrete proposed v1 format

### Algorithms and bounded profile

- AES-256-GCM using Go's standard `crypto/aes` and `crypto/cipher` primitives.
- Argon2id version 1.3 with **64 MiB memory, 3 passes, 4 lanes**, deriving a
  **32-byte key**. This follows RFC 9106's second recommended profile, selected
  rather than its 2 GiB profile for CLI portability.
- A fresh **16-byte random salt** and **12-byte random nonce** on every write,
  generated using `crypto/rand`. Never reuse a salt/nonce intentionally.
- One approved KDF profile ID in v1. The reader accepts only that exact profile;
  there are no attacker-controlled numeric memory/iteration allocations.
  Future profiles require an explicitly supported profile/version, not silent
  weakening or arbitrary self-tuning.
- Limit passphrase input to 1,024 bytes; reject empty input. Recommend a long,
  independently managed passphrase. Preserve its bytes exactly: no trimming or
  Unicode normalization.
- Serialize KDF work within a process. Its working memory is bounded by the
  profile, but this is not a hard RSS or wall-clock guarantee. Check cancellation
  before and after derivation; `argon2.IDKey` is not itself cancellable. Do not
  start abandoned goroutines to pretend that it is.

### Encoding and authenticated data

Proposed dotenv setting:

```text
CREDENTIALS_FORMAT=v1
ENC_CREDENTIALS=ekscred:v1:<unpadded-base64url-envelope>
```

The placeholder above is notation, not a credential or a working example.

Decoded envelope layout, in order:

1. One-byte KDF ID (`0x01` = Argon2id).
2. One-byte KDF profile ID (`0x01` = the exact profile above).
3. One-byte AEAD ID (`0x01` = AES-256-GCM).
4. The 16-byte salt.
5. The 12-byte nonce.
6. Ciphertext followed by the standard 16-byte GCM authentication tag.

Authenticate the literal prefix `ekscred:v1:` plus the complete fixed header
(IDs, salt, nonce) as GCM additional authenticated data. Verify authentication
before parsing or using plaintext.

The encrypted payload contains two length-prefixed fields:
`uint16-big-endian username length`, username bytes,
`uint16-big-endian password length`, password bytes.
Each field must be nonempty and no longer than 4,096 bytes. Require exact
payload consumption and the existing no-NUL/CR/LF effective-credential checks.
Keep bytes unchanged; do not add JSON conversion, trimming, or normalization.
This preserves the legacy decoder's effective output, including meaningful
spaces, without separately swappable username/password ciphertext fields.

Reject input larger than 16 KiB encoded, malformed/noncanonical base64url,
truncated headers/tags, unknown versions/IDs/profiles, oversized fields, and
trailing data before expensive work wherever possible. Authenticate all
recognized headers; bounded structural checks are not a substitute for the tag.

Wrong passphrase and failed authentication produce the same safe error:
**“Could not unlock credentials: incorrect passphrase or modified data.”**
Structural/unsupported-version errors may be distinguished without printing
input values. No fallback to CBC after a v1 failure. No partial plaintext escapes.

An authenticated bundle can still be replayed or replaced wholesale by another
valid bundle under the same passphrase. This proposal does not bind credentials
to an AWS role/account because the existing base identity is reused across them.

## 4. Compatibility and provider selection

- Default `CREDENTIALS_FORMAT=legacy` retains current installations, required
  legacy fields, digest selection, and effective-plaintext behavior.
- Explicit `CREDENTIALS_FORMAT=v1` requires `ENC_CREDENTIALS` and a separately
  supplied passphrase. A missing/corrupt v1 bundle fails closed.
- Unknown formats and conflicting explicitly configured legacy/v1 credential
  fields are errors; do not infer a provider from a failed decryption attempt.
  Parser defaults such as the legacy SHA-256 digest are not explicit conflicts.
- Preserve flags > environment > dotenv > defaults for non-secret configuration.
  Make conflicting process-environment overrides actionable by naming keys,
  never printing their values.
- In v1 mode, accept the passphrase from a hidden terminal prompt or the runtime
  environment, not the credential dotenv file. Noninteractive/JSON use requires
  runtime secret delivery; it never opens a prompt.
- Existing `list` and help/version paths remain independent of decryption.
- Retain legacy reading indefinitely until a separate deprecation decision.
  New v1 writes are opt-in only. Old binaries cannot read the new bundle.
- Do not introduce credential caching as part of this migration.

## 5. Proposed migration and verification UX

The following commands are **proposed, not currently implemented**:

```text
eks credentials migrate --env-file .env --output-file .env.v1
eks credentials verify --env-file .env.v1
```

1. Inspect a bounded, regular source file using the existing dotenv grammar.
   Reject ambiguous/mixed inputs. Credential ciphertext/digest come from the
   explicitly chosen source file, not silent process-environment replacements;
   document this migration-specific rule. Existing source passphrase delivery
   may be used to unlock it locally. Never expose its value.
2. Decrypt with the current legacy decoder, preserving its effective output.
   This cannot retroactively authenticate the legacy input. Ask the operator
   to confirm successful normal authentication before retiring recovery copies.
3. Obtain the new passphrase twice through hidden local terminal prompts.
   For a separately selected noninteractive path, accept a dedicated migration
   secret environment variable; never a flag, ordinary stdin pipeline, log,
   clipboard output, or value written to the destination.
4. Create the bundle, decrypt it back in memory, and verify byte-for-byte equality
   with the effective source credentials before publication. A readback check
   of the written encrypted data must also pass.
5. Generate a new destination dotenv file preserving unrelated source settings
   and removing legacy credential keys and the old passphrase entry. Add only
   the explicit v1 selector and encrypted bundle. Do not copy the new passphrase.
   Other existing secrets, such as a database password, may still be present;
   the destination must remain protected and this change does not secure them.
6. Refuse an existing destination, a symlink, the source path, or an alias of
   the source. Use a trusted owner-controlled destination directory, protected
   temporary file on the same filesystem, and exclusive atomic no-replace
   publication appropriate to each OS. A check followed by ordinary replacing
   rename is not sufficient. If no-replace publication or secure permissions
   cannot be guaranteed, fail rather than silently weaken the write policy.
7. Restrict Unix file permissions to 0600; on Windows establish an owner-only
   DACL before writing any sensitive bytes. Flush and close before publication,
   sync directory metadata where supported, and report platform durability
   limits. Clean temporary files on ordinary failure/cancellation.
8. Print only a safe completion summary, not credentials, ciphertext, file
   contents, or the passphrase. Verification is offline, authenticates and
   validates the bundle, and emits only success/failure.
9. The operator supplies the new runtime passphrase and explicitly selects
   `.env.v1` for a normal connection. Do not contact services during migration
   or offline verification.

**Recovery and rollback:** no in-place replacement or automatic source deletion.
If verification or normal authentication fails, continue using the untouched
legacy file and its matching passphrase. A forgotten new passphrase cannot be
recovered cryptographically; remigrate from the retained source or rotate the
underlying credentials. Remove redundant secret files only after a separate
explicit decision. Deleting a file is not a guaranteed secure erase.

## 6. Required implementation verification

- Deterministic format and KDF/AEAD vectors; v1 round-trip and legacy regression
  fixtures for both approved digests and shell-effective LF behavior.
- Flip every authenticated header field, nonce, ciphertext and tag; verify no
  plaintext output and no fallback. Test wrong passwords, valid-bundle swaps,
  unknown versions, truncation, oversized input, and trailing bytes.
- Fuzz structural decoding under strict bounds. Prove unknown KDF profiles are
  rejected before derivation; test fixed resource limits and serialized KDF work.
- Test exact field bytes, meaningful spaces, Unicode/byte handling, empty and
  control-character rejection, and parser bounds without silently changing data.
- Check provider conflicts, precedence, noninteractive secret delivery, no
  prompt in JSON mode, and help/list independence from decryption.
- Ensure redaction supports both formats **without deriving the KDF again for
  each log message** or retaining a new long-lived plaintext cache. Prefer safe
  boundary errors and operation-scoped redaction.
- Check arguments, stdout/stderr, error chains, and failed verification for
  secret leakage; verify migration does not contact DB, OAuth, firecall or AWS.
- Exercise no-overwrite, symlink/hardlink alias handling, permissions, atomic
  publication, full disk, flush/close errors, cancellation, and crash recovery.
- Run formatting, race tests, vet/staticcheck, and six cross-builds. Run native
  Windows/macOS/Linux filesystem/ACL tests and KDF latency/memory measurements;
  cross-compilation alone cannot establish those results.
- Pin/review the new module for the approved compiler before adoption. No package
  installation, crypto implementation, native benchmark, or migration occurred
  during this design review.

## 7. Approval and unresolved decisions

Approve or revise the following before implementation:

1. Use the portable Argon2id/AES-GCM bundle first rather than an OS-store provider.
2. Accept the single 64 MiB / 3-pass / 4-lane profile and one additional reviewed
   Go module, subject to native performance and target-version verification.
3. Keep the new passphrase separate from dotenv, including in automation.
4. Use explicit provider selection, conflict rejection, and new-file-only
   migration with the original retained for recovery.

Confirm whether any organizational FIPS/compliance requirement or constrained
target hardware would invalidate these choices. No such requirement is assumed.
Actual module version, native no-replace publication implementation, and measured
KDF latency remain implementation validation gates, not claims established here.

**Approval request:** approve this design, request revisions, or keep the current
format. Approval of the review is not permission to migrate real credentials;
implementation and any later migration remain separate explicit actions.

## References checked

- [RFC 9106, section 4](https://www.rfc-editor.org/rfc/rfc9106.html#section-4):
  Argon2id parameters and the 64 MiB second recommended profile.
- [Go Argon2 package](https://pkg.go.dev/golang.org/x/crypto/argon2):
  `IDKey`, memory units, and recommended profiles.
- [Go cipher package](https://pkg.go.dev/crypto/cipher#NewGCM):
  standard authenticated-encryption primitive. The pinned Go 1.25.14 local API
  documentation was also checked.
- [Go PBKDF2 package](https://pkg.go.dev/crypto/pbkdf2):
  standard-library alternative; its Go 1.25.14 API was checked locally.
