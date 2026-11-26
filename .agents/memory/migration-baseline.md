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
