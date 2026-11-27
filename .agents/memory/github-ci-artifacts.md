---
name: GitHub CI artifacts
description: Downloading native test reports through the authenticated GitHub proxy
---

When `gh run download` fails with authentication errors on a public GitHub API
artifact URL, use `gh api --hostname github.replit.internal` with the relative
repository actions artifact ZIP endpoint instead.

**Why:** The run-download command can follow absolute public API links outside
the authenticated proxy even when repository access and CI execution work.
The relative API request keeps authentication on the configured proxy.

**How to apply:** List the run's artifacts through the proxy, then download the
selected artifact using its returned ID and the relative ZIP endpoint. Create
the destination directory first and keep downloaded reports in the ignored
generated-artifacts directory. Do not copy tokens or signed URLs between hosts.
