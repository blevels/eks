# Application diagnostics

## Contract and ownership

Schema 1 uses UTC `time`, `level`, fixed `msg`, `event`, `component`,
`operation`, `run_id`, `operation_id`, `parent_operation_id`, `cycle_id`,
`version` and `pid`. IDs are opaque random values, never selectors or hashes.
Starts and successful detailed outcomes are DEBUG; run and connection lifecycle
are INFO; retries are WARN; terminal failures are ERROR. Cancellation is INFO.
Outcomes include `status` and numeric `duration_ms` (milliseconds).
Failures include fixed `error_code` and `error_category`.

The lowest responsible boundary owns the detailed failure. Callers propagate
its safe operation reference without repeating the failure. The run summary
references `failed_operation_id`. Supplemental cleanup failures have their own
operations and do not replace the primary cause. No raw error strings belong
in diagnostics, even if a redactor could mask some known secrets.

## Event catalog and automated evidence

The implementation and automated evidence matrix below are maintained together.
Each named operation emits `<operation>.start` and `<operation>.outcome`;
cache decisions and retry decisions also have fixed named events.

| Component | Operations / decisions | Outcomes |
|---|---|---|
| cli | run, dependency, driver, database_close, cache_setup, log_sink | setup, exit, cancellation, cleanup |
| config | parse, dotenv_open, dotenv_read, dotenv_close | validation, optional absence, I/O |
| database | query, query_plan, query_scan, query_validation, query_iteration, rows_close | query, scan, validation, iteration, row close, timeout |
| cache | cache_lookup, cache_wait, cache_validation, cache_clear; cache.hit/miss/wait/fill/empty/eviction/clear/suppressed | wait cancellation, invalid fill, stale fill |
| credentials | decrypt_user, decrypt_password | invalid encrypted input, success |
| terminal | selection, password, confirmation, ticket, restore | EOF, invalid input, output, cancellation |
| http | oauth_token, list_firecalls, create_firecall, http_attempt, retry_wait, response_close | semantic response, retry budget, status, close |
| workflow | workflow, selection, selection_validation, production_config, password_validation, refresh, authorization, activation, connection, refresh_wait | allowed/blocked, timeout, stop |
| filesystem | private_directory, directory_protection, credential_encode, credential_file, cleanup | creation, permissions, serialization, write, removal |
| subprocess | kconnect, subprocess | start, child exit, timeout, cancellation |
| output | output_list, output_details, output_active, output_created, output_connected, output_help, output_version | serialization/write |

Decision events: `run.invocation`, `run.shutdown`, `refresh.wait`,
`authorization.allowed_nonproduction`, `authorization.allowed_active`,
`authorization.allowed_activated`, `authorization.blocked_noninteractive`,
`authorization.blocked_declined`, `authorization.stopped`, `activation.wait`,
`activation.poll`, `connection.permitted`, `connection.blocked`,
`terminal.invalid_selection`, `terminal.invalid_confirmation`,
`http.retryable_attempt`, `http.retry_scheduled`, `http.retry_budget_rejected`.
`authorization.stopped` means the workflow could not complete (including an
output failure); it does not assert that the service denied authorization.
`connection.permitted` is a decision to attempt the child, not proof it connected.
`driver.outcome` success means driver initialization, **not** verified connectivity.

All error codes are `<owning_operation>_failed`. Categories are fixed:
`configuration`, `database`, `validation`, `credentials`, `terminal`,
`filesystem`, `subprocess`, `output`, `cache`, `workflow`, `setup`,
`response_validation`, `eof`, `canceled`, `timeout`, `declined`, and the HTTP
categories `tls_validation`, `network`, `http_status`, `invalid_json`,
`response_limit`, `response_read`, `response_close`, `invalid_request`.
Unknown original errors are never formatted. `errors.Is` identity and safe
HTTP/child classifications remain available without exposing raw driver,
transport, filesystem, or child errors through `Unwrap`/`As`.

Allowed numeric payloads: `attempt`, `max_attempts`, `http_status` (0 means
no HTTP response), `retry_delay_ms`, `child_exit_code`, `row_count`, `exit_code`.
`duration_ms` is a nonnegative number, not a Go duration string or nanoseconds.
Failures that will be retried have WARN outcomes with `status=failure`, not
misleading successful attempt outcomes. Higher scopes emit DEBUG references to
the owning failure (connection lifecycle references remain INFO).
An operator declining creation remains exit 1, but is INFO with
`status=canceled` and `error_category=declined`, not an error incident.

The first cycle covers discovery and the first refresh. Later cycles use new
IDs while keeping the selected database snapshot, as before. Explicit cache
invalidation outside a request has the run ID but no invented cycle or parent.

### Boundary/outcome test matrix

Tests below use only synthetic inputs, injected failures, or local HTTP servers.
`diagtest.Contract` parses JSON and verifies required fields/types, UTC,
operation parents, numeric durations, statuses, and failure classification.

| Boundary / path | Automated evidence |
|---|---|
| Schema, ownership, canceled/timeout levels, source caller, hostile metadata, numeric units | `diagnostics.TestSchemaOwnershipAndCancellation`, `TestSourceCallerAndHostileMetadata`, `TestSourceMetadataRemovesHomesAndFunctions`, `TestAllowlistsAndLevelRules`, `TestDurationUsesMilliseconds`; `app.TestSourceIdentifiesOperationalCallerNotObservationWrapper` |
| Supplemental error does not reclassify primary | `diagnostics.TestCleanupDoesNotReclassifyPrimaryCause`; CLI and kconnect dual-failure cases below |
| Invocation, config failure, unknown/invalid logging options, missing/malformed dotenv, safe text fallback | `cli.TestBootstrapErrorsUseSafeTextFallback`, `TestOfflineCLI`; `config.TestLoggingConfigurationPrecedence` |
| Dotenv open/read/validation/close, optional absence, simultaneous read/close failure | `config.TestDotenvDiagnosticBoundaries` |
| Dependency validation; offline help/version and their output errors | `cli.TestCLIHelpVersionOutputAndDependencyFailures`, `TestOfflineCLI` |
| Driver setup failure, database close failure, query plus close failure, run exit/owner | `cli.TestCLIOutcomesAndCleanupOwnership` |
| SQL rows/empty, query error, scan error, invalid row, iteration error, invalid plan, cancellation/deadline | `namespace.TestDatabaseDiagnosticOutcomes`, `TestFind` |
| Row close error surfaced by database/sql | `namespace.TestDatabaseRowCloseDiagnostics` |
| Cache hit/miss/fill/empty/LRU eviction/clear/suppressed stale fill; queued deadline | `namespace.TestCacheDiagnosticDecisionsAndConcurrentClear` |
| Cache concurrency, canceled hits/fills/waiters, no errors cached, invalid results, independent stores | `namespace.TestCacheDiagnosticFailureOutcomes`, `TestConcurrentIdenticalLookupsQueryOnce`, `TestWaitingMissCanCancelWhileHitsRemainAvailable`, `TestCacheDoesNotKeepErrorsOrInvalidResults`, `TestCacheCanceledCallsDoNotPopulate`, `TestCacheHitHonorsCancellation`, `TestCacheClearAndStoreIsolation` (race suite) |
| Credential decryption success and both failure boundaries | `app.TestWorkflowDiagnosticCoverage`; `credentials.TestDecryptOpenSSLFixtures` |
| Selection/confirmation/ticket, invalid retry input, EOF, empty ticket, output error, canceled prompt, nonterminal password | `terminal.TestTerminalDiagnosticOutcomes`, `TestPrompts`, `TestPromptFailures` |
| Native password success/EOF/Ctrl-C, final output failure, restore failure, primary plus restore failure | Linux PTY `terminal.TestPasswordDiagnosticNativeOutcomes`, `TestPasswordRestoresTerminalWhenPromptWriteFails` |
| HTTP attempts, transient/terminal responses, semantic token/create failure, retry budget rejection, canceled wait, response close and primary-plus-close, deadline, network failure | `firecall.TestHTTPCommonSchemaOutcomes`, `TestTransportFailureCategories`, `TestResponseFailurePolicyAndCleanup`, `TestHTTPDiagnosticsExcludeSensitiveDetails` |
| Authorization active/created/nonproduction/declined/noninteractive, activation timeout/cancellation, refresh cancellation, selection/config validation | `app.TestWorkflowDiagnosticCoverage` and existing authorization/cache tests (live firecall checks preserved) |
| Output in each text/JSON path fails safely; serializing an unsupported value is handled at output boundary | `app.TestAllOutputBoundariesOwnWriterFailures`, `TestSerializationFailureIsOwnedByOutputBoundary`, `TestJSONResultsIndependentFromDiagnosticFormat` |
| Private-directory creation/protection, credential serialization/file write, child success/failure/timeout/cancel, cleanup alone or with primary | `kconnect.TestFilesystemAndSubprocessDiagnostics`, `TestCredentialDeliveryAndCleanup`, `TestCancellationRemovesTemporaryCredentials`, `TestProcessFailureExitCode`, `TestChildExitDiagnosticCode` |
| File rotation, bounds, permissions, symlinks/hardlinks, exclusive writer, oversized preexisting file | `diagnostics.TestSinkRotationPermissionsAndBounds`, `TestSinkRefusesUnsafeDestinations`, `TestSinkRejectsConcurrentWritersAndOversizedExistingFiles` |
| Sink short writes/write/rotation/sync/close/record limit, bounded correlated alert/cancellation | `diagnostics.TestSinkFailuresCancelOnceAndRemainBounded`, `TestEmergencyAlertUsesCommonSchemaAndRunID`, `cli.TestCLILogSinkFailureForcesOrderlyNonzeroExit` |
| Canary secrets/control characters in text/JSON/errors/source/archives | `diagnostics.TestSourceCallerAndHostileMetadata`, `TestRotatedLogsContainOnlySafeEvents`; all boundary diagnostic tests |
| Windows log ACL | `diagnostics.TestLogFileOwnerOnlyACL` (requires native Windows; not claimed by cross-builds) |

The fixed cache constructor cannot fail with the CLI's nonnil source and positive
constant capacity; its defensive error path is instrumented and constructor
rejections are tested by `namespace.TestCacheRejectsInvalidSetupAndMode`.
Credential JSON and ordinary result records use only statically serializable
types. Their encoding failure boundaries are instrumented; unsupported-value
failure injection tests the output boundary without changing production types.

## Security and retention policy

stderr structured text is the default; JSON diagnostic output is separate from
JSON result records. Verbose enables DEBUG. No environment, raw arguments,
answers, prompt contents, paths, SQL, selectors, credentials, lengths, hashes,
remote payloads, child output, or identity/account/role values may be recorded.
Only fixed enums and allowlisted numeric attributes are accepted.

Opt-in local retention is limited to 10 MiB per file, three archives, and a
bounded single-record margin. An explicitly requested sink must initialize or
startup fails. Runtime delivery failure requests orderly cancellation and cleanup,
then exit 1, with one best-effort stderr alert. No recursive logging is used.
No sink can guarantee delivery after SIGKILL, power loss, corruption, or broken
stderr and file output. No network collector or telemetry is enabled.

## Configuration and collection

| Flag | Environment / dotenv | Default |
|---|---|---|
| `--log-format text\|json` | `EKS_LOG_FORMAT` | `text` |
| `--log-source[=true\|false]` | `EKS_LOG_SOURCE` | `false` |
| `--log-file PATH` | `EKS_LOG_FILE` | disabled |
| `--verbose` | no environment alias | disabled |

Flags override environment, then dotenv, then defaults. Formats are
case-sensitive. Source accepts Go boolean flag syntax; invalid environment
booleans fail unless explicitly overridden. An empty `--log-file=` disables
an inherited file sink. Invalid logging/config options use deterministic
non-verbose **text stderr**, not partially parsed format/source/file options.
Config errors exit 2; explicitly requested sink initialization failures exit 1.
Bootstrap setup records are bounded and replayed only after options validate.
Source attribution skips shared and application observation wrappers; after
bootstrap, source stacks are not walked unless attribution was requested. Emergency
sink alerts (and the final summary following sink failure) omit source attribution.
Help/version intentionally bypass dotenv and all logging options, keep stdout
unchanged, and require no database/service access; their diagnostics use defaults.

Examples (replace the sample selectors and local destination yourself):

```sh
eks --log-format json --verbose --once --id 12 >results.txt 2>diagnostics.jsonl
eks --json --log-format json list >results.jsonl 2>diagnostics.jsonl
eks --log-format json --log-source --log-file ./private-logs/eks.log --once --id 12
```

Create the parent directory yourself with owner-only permissions. The program
does not create parent directories or print paths in failure records. Existing
symlink parents/destinations, nonregular files, hardlinks, and files owned by
another user are refused. Existing Unix files are restricted to 0600 before
writing. Windows files receive a protected owner-only DACL before writes.
The sink holds an exclusive, empty owner-only `.lock` sidecar for its lifetime
so separate runs cannot concurrently rotate the same destination. The sidecar
is retained (empty) after shutdown to avoid lock-inode races. Do not remove or
replace it while a process is running. Log directories must be trusted; this
does not defend against another process with your own account modifying files.

Rotation moves active → `.1` → `.2` → `.3` and discards the oldest archive,
before exceeding 10 MiB on a normal write. Records are capped at 16 KiB.
Oversized existing active/archive files fail startup rather than being
silently truncated or rotated beyond the retention bound. File shutdown syncs
and closes explicitly, then releases the lock. `run.shutdown` is written to both
sinks; the **final `run.outcome` is stderr-only**, after file shutdown, so its
exit code can include sync/close failures. If a sink fails at runtime, one
nonrecursive structured `log_sink.outcome` alert is attempted directly on stderr.
Cleanup still runs; exit 1 takes precedence over interruption for sink failure.
If the final stderr write itself fails, the exit is still 1, but a record
describing that failure cannot be guaranteed.

For collection, ingest JSON stderr, or tail JSON local files and include the
archives when troubleshooting. Retain stderr when you need the final run
summary. The application makes no remote connections for diagnostic collection.
Results may contain operational data by design: diagnostic secrecy is **not**
a promise that stdout result records or interactive displays contain no identities.

## Troubleshooting

1. Find `run.outcome` on stderr and note `exit_code`, `run_id`,
   `failed_operation_id`, `error_code`, and `error_category`.
2. Filter the same run by `operation_id=failed_operation_id`; use its parent
   and `cycle_id` to inspect authorization, attempts, retry waits, and cleanup.
   Enable `--verbose` next time if detailed successes/starts are needed.
3. A database `driver` success only says the driver was initialized. Use the
   query outcome for connectivity/read evidence. Never add a DSN or raw SQL to logs.
4. For `http_status`, inspect numeric `http_status`. For `timeout`, check attempt
   count, `http.retry_budget_rejected`, and the enclosing operation. POST token
   and creation requests are not retried; verify remote request state before
   manually repeating a creation. An activation timeout never permits connection.
5. For `*_validation_failed` or `response_validation`, inspect the input/service
   contract privately, not by logging payloads. For terminal EOF or user refusal,
   retry interactively when appropriate. Cancellation is not an error incident.
6. For `log_sink_failed`, check parent permissions, disk space, destination type,
   exclusive use, and archive size without pasting file paths/contents into logs.
   Do not assume any failed record was persisted. Stderr may have a copy that a
   file sink failed to save.

### Representative synthetic JSON Lines

IDs, version and process values below are synthetic; timings are milliseconds.
DEBUG records are omitted here except where shown.

```json
{"time":"2026-11-26T12:00:00Z","level":"WARN","msg":"Application diagnostic","schema_version":1,"run_id":"run-example","version":"dev","pid":100,"event":"http_attempt.outcome","component":"http","operation":"http_attempt","operation_id":"attempt-example","parent_operation_id":"list-example","cycle_id":"cycle-example","attempt":1,"max_attempts":3,"http_status":503,"status":"failure","error_code":"http_attempt_failed","error_category":"http_status","duration_ms":12.5}
{"time":"2026-11-26T12:00:02Z","level":"ERROR","msg":"Application diagnostic","schema_version":1,"run_id":"run-example","version":"dev","pid":100,"event":"activation.outcome","component":"workflow","operation":"activation","operation_id":"activation-example","parent_operation_id":"auth-example","cycle_id":"cycle-example","status":"timeout","error_code":"activation_failed","error_category":"timeout","duration_ms":2000}
{"time":"2026-11-26T12:00:02Z","level":"DEBUG","msg":"Application diagnostic","schema_version":1,"run_id":"run-example","version":"dev","pid":100,"event":"cleanup.outcome","component":"filesystem","operation":"cleanup","operation_id":"cleanup-example","parent_operation_id":"child-example","cycle_id":"cycle-example","status":"success","duration_ms":0.5}
{"time":"2026-11-26T12:00:02Z","level":"ERROR","msg":"Application diagnostic","schema_version":1,"run_id":"run-example","version":"dev","pid":100,"event":"run.outcome","component":"cli","operation":"run","operation_id":"run-op-example","parent_operation_id":"","cycle_id":"","status":"timeout","error_code":"activation_failed","error_category":"timeout","failed_operation_id":"activation-example","exit_code":1,"duration_ms":2020}
```

## Verification limits

### Native diagnostic-log CI

`.github/workflows/diagnostics-native.yml` runs on pushes, pull requests and
manual dispatches, independently on `windows-latest` and `macos-latest`.
Each runner uses checksum-verified Go 1.25.14 and executes
`go test -race -count=1 -timeout=5m -json ./internal/diagnostics/...`.
The uncached suite includes archive rotation and retention bounds, exclusive
writer locking and release, unsafe active/archive/lock destinations, Unix
0600 permissions, and (on Windows only) `TestLogFileOwnerOnlyACL` for the active
file, all three archives and the lock sidecar. The Windows job enables
unprivileged symlinks; both jobs require link-refusal tests to run rather than
silently skipping them. A failure in one OS does not cancel the other.

Each job preserves its JSON test report for 14 days, including on test failure.
Inspect the OS-specific job and its report for passing tests and any skips;
adding this workflow alone is not evidence of a native pass. The reports contain
synthetic test data, not retained application logs. Run locally on the target OS
with `make test-diagnostics`, or the equivalent Go command above (no Make
installation required on Windows).

These jobs are deliberately limited to diagnostic log files. They do not
validate temporary-credential cleanup or native terminal restoration, and do not
claim every architecture is covered: the report's `go env GOOS GOARCH` identifies
the hosted runner architecture.

Native Linux tests cover file modes, rotation, process execution and PTYs.
macOS/Windows amd64/arm64 binary cross-builds establish compilation only.
Windows ACL behavior and macOS runtime behavior require passing results from
their native jobs; neither is claimed from Linux or from cross-builds.
No real production database, OAuth/firecall service, real credentials, external
terminal session, power-loss recovery, or hostile same-account process was used
for verification. Arbitrary OS/kernel corruption and unavailable output sinks
are not delivery guarantees. `database/sql` reports some automatic row-close
errors through `Rows.Err`; those belong to `query_iteration`, while errors
surfaced by explicit `Rows.Close` belong to `rows_close`.
