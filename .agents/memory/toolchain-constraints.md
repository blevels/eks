---
name: Toolchain constraints
description: Why the migration target was raised and the verification principle to preserve
---

The user approved raising the Go target when the original target could not use
the current PostgreSQL driver without selecting blocked vulnerable releases.

**Why:** Prefer the supported current driver to preserving an old compiler target
through unnecessary dependency substitutions.

**How to apply:** Do not downgrade dependencies or bypass the package firewall to
restore the superseded target.

Verify the supported compiler target with checksum-verified tooling rather than
relying only on a newer host compiler.

**Why:** A successful build on a newer compiler does not establish minimum-version
compatibility, and environment download restrictions are not a reason to bypass
verification.

**How to apply:** Keep the compiler target explicit in quality checks and preserve
download verification when selecting development tools.
