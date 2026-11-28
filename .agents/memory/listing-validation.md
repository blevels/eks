---
name: Listing validation policy
description: User-approved distinction between inherited and explicit unused numeric settings for namespace listing
---

Namespace listing must ignore inherited refresh interval, command timeout,
firecall polling interval, activation timeout, and firecall-hour settings, but
reject invalid explicitly supplied flags for those settings.

**Why:** The user chose to keep unrelated environment/dotenv values from blocking
listing while still catching mistakes the caller explicitly typed.

**How to apply:** Preserve database timeout, database, logging, and connection
validation. Do not broaden the exception to explicit flags or connection commands
without agreement.
