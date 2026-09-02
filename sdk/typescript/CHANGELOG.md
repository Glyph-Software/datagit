# @glyphsoftware/datagit-sdk

## 0.1.0

### Minor Changes

- [`0b4d152`](https://github.com/Glyph-Software/datagit/commit/0b4d1521c49683bce72d4de75b4326bd4789de9a) Thanks [@pranavms13](https://github.com/pranavms13)! - First release of the DataGit SDKs.

  `@glyphsoftware/datagit-sdk` on npm and `datagit-sdk` on PyPI are the same contract
  twice, generated from `api/proto/datagit/v1/datagit.proto` plus a thin ergonomic
  layer, and released together on one version number.

  Both cover reading a branch, streaming a scan with typed filters, and buffering
  changes into a single commit. Neither lets you set a commit author, send an exact
  number as a float, or build a filter from a string.
