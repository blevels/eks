---
name: Migration baseline
description: User-confirmed migration scope and interpretation of the shell transcription
---

Treat the supplied shell as a transcription of a working POC, not evidence that the original POC fails. Preserve intended behavior; distinguish transcription corrections from proposed behavioral changes.

**Why:** The user explicitly corrected the interpretation that transcription errors meant the POC did not work.

**How to apply:** Explain intentional deviations separately and pause for approval between phases. The agent performs every phase; the user reviews rather than supplying analysis.

Retain encrypted username/password configuration in a local `.env`, compatible with the user's salted OpenSSL AES-128-CBC encryption format. The external authentication tool version is 0.5.24. Firecall is mandatory only for production.

**Why:** The user explicitly confirmed these requirements and requested best judgment based on the shell for OAuth/firecall details; no additional API specifications will be supplied.

**How to apply:** Document API assumptions for later manual adjustment. Do not repeatedly request further API details or silently add stricter authorization policy than the approved migration.

The user subsequently approved these production-access rules: exact valid
environment names are `dev`, `sit`, `uat`, and `prod`; only `prod` is production.
Existing active firecalls must match the AWS account and IAM role, not requester
or expiration. After creation, confirm an active account-and-full-role-ARN match
before connecting. The user approved immediate checking followed by configurable
5-second polling for up to 2 minutes, stopping on timeout, cancellation, or API
error without connecting or recreating the request.

**Why:** The user explicitly approved stricter production gating and activation
verification instead of the original POC's account-only and create-then-connect
behavior.

**How to apply:** Preserve these approved deviations from POC parity. The current
list contract cannot correlate activation to the newly created request ID; do
not claim that it does.
