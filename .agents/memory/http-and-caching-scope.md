---
name: HTTP and caching scope
description: User requirements for retries and database caching; token caching deferred
---

Any retry policy must include exponential backoff.

**Why:** The user explicitly required exponential backoff when requesting HTTP improvements.

**How to apply:** Preserve bounded attempts and cancellation/deadline handling alongside backoff; do not retry non-idempotent creation without an approved safety guarantee.

The user requests caching all data retrieved from the database to prevent unnecessary requerying. Token caching is deferred to a later revision.

**Why:** The user explicitly specified database caching and excluded token caching from this revision.

**How to apply:** Keep caching scoped to database results for this work; do not extend it to OAuth tokens or firecall authorization. The accepted cache task uses session snapshots with explicit invalidation or process restart, not an automatic TTL. Changes to that freshness policy require separate review.
