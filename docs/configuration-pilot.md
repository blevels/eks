# Configuration pilot

## Behavior contract

| Area | Contract |
| --- | --- |
| Sources | Explicit flags override process environment, then dotenv, then defaults. Empty strings and false booleans are values, not missing settings. |
| Overrides | Convert only the selected value; invalid lower-priority values cannot defeat a valid explicit override. |
| Arguments | Standard `flag` syntax, including single/double dashes, repeated options (last value wins), and `--`. Options precede the positional selector. |
| Offline options | Parsed help/version flags bypass dotenv and deployment configuration. A spelling used as a string operand or after `--` is not an option. |
| Dotenv | Default `.env` may be absent; an explicitly selected file must be readable. Preserve the existing bounded, literal dotenv grammar and do not modify process environment. |
| Selection | One positional ID/name/`list`, or one explicit `--id`/`--name`. Preserve the positional length heuristic and reject control characters. |
| Validation | Preserve database and logging validation for all commands. Listing ignores inherited connection-only numeric settings but validates explicit flags; connection commands retain full validation. Production endpoints are still validated after discovery establishes the environment. |
| Safety | No secrets in flags; no raw flag-parser diagnostics, argument values, or configuration dumps in errors/output. Preserve structured diagnostic events and CLI exits. |

### Approved corrections

1. Parse options once instead of scanning for special spellings. For example,
   `--ticket --version` supplies a ticket string; it does not request version output.
2. Parsed help/version do not hide malformed options before the positional
   boundary. Unknown options and missing string operands remain usage errors.
3. Only an actual parsed `--env-file` selects a dotenv file. Repeated parsed
   selections use the last path; spellings used as operands/positionals do not.
4. Explicit `--id` and `--name` are mutually exclusive by presence, even if one is
   empty. An explicitly empty selector is invalid.

### Listing validation policy

For positional `list`, ignore inherited `INTERVAL`, `COMMAND_TIMEOUT`,
`FIRECALL_POLL_INTERVAL`, `FIRECALL_ACTIVATION_TIMEOUT`, and `DURATION_IN_HRS`
from environment and dotenv, including empty, malformed, nonpositive, or
overflowing values. Listing does not use these settings; their Config fields
remain zero when not explicitly flagged (including when defaults would apply).

Explicit `--interval`, `--command-timeout`, `--firecall-poll-interval`,
`--firecall-activation-timeout`, and `--duration` still require valid positive
values, catching caller mistakes. Valid explicit values override invalid
inherited ones, with repeated flags retaining standard last-value-wins behavior.

`TIMEOUT` / `--timeout` remains mandatory and positive because listing uses it
for database operations. Database and logging requirements, dotenv loading and
syntax, selector validation, and flag syntax are unchanged. Connection commands,
including `--id=list` and `--name=list`, retain all existing validation.

### Other decisions

Help/version flag values follow standard boolean syntax. When both are true,
the existing CLI output dispatcher gives help precedence.

## Design

`ParseContext` coordinates argument parsing, offline handling, dotenv loading,
value resolution, and conversion/validation. One private options type holds
the existing Config plus the few values that are not yet converted and explicit
flag presence. It is not a second configuration model.

Flag declarations are ordinary `FlagSet` registrations. A local resolver
expresses precedence once; straightforward assignments show which environment
variable supplies each field. Conversion and validation use ordinary error
returns. No registry, reflection, new interface, or dependency is required.

### Before and after

Previously, `ParseContext` scanned argument spellings twice, loaded dotenv,
registered flags with environment-derived defaults, parsed them, and combined
selector handling, conversion, and validation in the same function. The scans
could mistake an option's operand for a separate flag.

Now:

- `config.go`: public configuration, coordinating parser, conversion, selection,
  and common/connection/production validation.
- `options.go`: one standard flag parse, explicit flag presence, and source
  resolution. The private options type holds Config and six numeric strings;
  it does not duplicate all configuration fields.
- `dotenv.go`: unchanged grammar/I/O behavior, with function contracts documented.

For example, `--timeout=7` is recorded as an explicit string option. `resolve`
therefore ignores any TIMEOUT environment/dotenv value, even an invalid one.
`configuration` converts the chosen `7` through `positiveDuration` into seven
seconds. Without a flag, the same assignment chooses TIMEOUT from environment,
dotenv, or the literal `15s` default. Conversion happens only after that choice.

The extra private type is needed to distinguish explicit empty/false flags
from defaults and to delay numeric conversion until precedence is resolved.
The shared duration helper implements the existing seconds-or-Go-duration rule
once. The validation helpers name real policy boundaries; there is no generic
conversion table, plugin mechanism, or interface solely for unit tests.
All production functions in the configuration package document their contracts.

## Verification baseline

Before implementation, the existing tree passed, with checksum-verified
Go 1.25.14:

- `make test lint`: race tests, gofmt, vet, and pinned staticcheck.
- `make build cross-compile`: native build and six Linux/macOS/Windows builds.

Cross-builds are compilation evidence, not native macOS/Windows runtime tests.

## Completed verification

After implementation, `GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.25.14 make check build`
passed: all race tests, formatting, vet, pinned staticcheck, six cross-builds,
and the native CLI build. `git diff --check` also passed.

Configuration regressions cover every environment-backed field through dotenv,
environment, and supported flags, plus empty/false overrides, invalid overridden
values, argument boundaries, repeated dotenv selection, validation applicability,
duration bounds, and safe error identity. CLI integration verifies usage exits,
offline dependency avoidance, and machine-parseable JSON results.

The listing-policy follow-up also passed
`GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.25.14 make test lint build cross-compile`
and `git diff --check`. Focused regressions cover invalid unused settings from
environment, dotenv, and explicit flags; valid overrides and repeated flags;
unchanged connection validation (including explicit selectors named `list`);
and retained database, database-timeout, and logging validation. CLI tests verify
successful JSON listing without executable lookup, safe usage failures before
database access, and successful exit diagnostics.

No production service was contacted. No macOS/Windows runtime behavior is newly
claimed. Production changes are confined to configuration; the CLI change is
test coverage only.
