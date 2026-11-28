# Connection workflow

## Bounded design

Keep `Run`, `refresh`, `authorize`, and `waitForActivation` as the workflow's
existing stages. Calls that select, decrypt, prompt, authorize, and connect are
ordinary Go calls with explicit diagnostic start/end scopes. There are no new
production types, interfaces, dependencies, or orchestration layers.

Two wrappers remain deliberately:

- `action` starts a scope, runs a single output callback, and ends the scope.
  Output methods have no context parameter; spelling this out at every print
  site would obscure the decisions without clarifying context propagation.
- `failed` gives immediate validation errors their existing diagnostic owner.
  These branches have no I/O call whose inputs or context need exposing.

The generic `observe` wrapper is removed. `action` no longer calls through it.
Logging internals, file retention, configuration, and HTTP retries are unchanged.

## Before and after walkthrough

Before, a namespace lookup was passed to `observe` as a callback. Reading the
callback revealed the actual `Store.Find` call, but the wrapper separately
determined which context reached it and how its error was returned. Production
token/list/create requests and connection repeated this pattern. `action`
introduced another wrapper through a synthetic empty result.

After, lookup reads directly as:

```go
selectionCtx, selection := diagnostics.Start(ctx, "workflow", "selection")
items, err := a.Store.Find(selectionCtx, mode, c.Target)
if err = selection.End(err); err != nil {
    return err
}
```

The same pattern makes the remaining steps visible:

1. `Run` finds namespaces. List prints and returns; connection mode selects and
   validates a row, checks production configuration, then prints details.
2. Each `refresh` decrypts both stored credentials. Production uses the prefixed
   username and supplied/prompted production password; email still uses the base
   identity. Nonproduction skips firecall authorization.
3. `authorize` obtains a token and checks the selected account and role. Active
   access prints and returns. Otherwise JSON refuses creation, a declined prompt
   stops the workflow, or confirmation/ticket collection leads to creation.
4. Creation output precedes immediate activation polling. Polling stays within
   its deadline, checks cancellation even after a successful response, and never
   retries an unsuccessful lookup itself. HTTP retries remain the client's job.
5. Only after authorization and the final cancellation check does the connector
   run, visibly receiving `connectionCtx`. A successful child marks the connection
   complete before success output; a later writer failure is not misreported as
   a blocked connection.
6. Once returns. Otherwise the cancellable refresh wait completes and a new cycle
   begins, decrypting credentials and checking authorization again.

Operation contexts are local variables, not replacements for their parent.
Sibling scopes therefore retain the same parents as before. Synchronous
decryption has no context parameter; its scope still owns any decryption error.
Every direct boundary ends its scope before checking/returning the error, so a
deeper diagnostic owner and `errors.Is` identity survive propagation. Existing
operation names, events, safe error messages, result output, and ordering remain
unchanged. No intentional behavior differences are introduced.

This is intentionally somewhat repetitive: the start/call/end sequence shows
context and error ownership at the call site. Further helpers would reintroduce
the indirection being removed. Function contracts document inputs, effects, and
error behavior without replacing readable control flow.

## Verification

Baseline: pinned Go 1.25.14 race tests for `internal/app` passed before editing.

Regression coverage supplements the existing tests:

- Nested query/connector failures keep their original owner, parent operation,
  cycle, and error identity, without duplicate primary error reports.
- The real HTTP retry tests also assert that HTTP attempts stay under their
  token/list/create operation contexts and retain their cycle correlation.
- A repeated production refresh reauthenticates in a new cycle. Revoked access
  and a declined creation prompt cannot cause a second connection.

Existing tests continue to cover production gating, activation/deadline safety,
output failures and formatting, prompt/decryption failures, source attribution,
retry exhaustion/recovery, cancellation, and diagnostic secret canaries.

Completed verification with checksum-verified Go 1.25.14:

```text
GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.25.14 make test lint build cross-compile
git diff --check
```

All passed: repository-wide `go test -race ./...` (including the changed app
package), gofmt checks, vet, pinned staticcheck, the native static CLI build, and
Linux/macOS/Windows cross-builds for amd64 and arm64.

All service tests use fakes or local TLS servers; no production service is
contacted. Cross-compilation is not native macOS/Windows runtime evidence.
