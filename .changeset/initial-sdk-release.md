---
"@glyphsoftware/datagit-sdk": minor
---

First release of the DataGit SDKs.

`@glyphsoftware/datagit-sdk` on npm and `datagit-sdk` on PyPI are the same contract
twice, generated from `api/proto/datagit/v1/datagit.proto` plus a thin ergonomic
layer, and released together on one version number.

Both cover reading a branch, streaming a scan with typed filters, and buffering
changes into a single commit. Neither lets you set a commit author, send an exact
number as a float, or build a filter from a string.
