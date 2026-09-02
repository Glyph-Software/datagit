# Changesets

This directory records **version intent**: what changed, and whether it is a
patch, a minor, or a breaking change. It does not record what the diff did — the
commit message does that.

## Why there is a `package.json` at the root of a Go repository

Changesets discovers packages through a workspace, so it needs one. It could have
lived in `sdk/typescript` instead — and it did, until that broke a release.

The `changesets/action` commits the version bump with `git add .` from its working
directory. Pointed at `sdk/typescript`, that staged the npm bump and silently
dropped the Python one: npm went to 0.1.0 while `sdk/python` stayed at 0.0.0, in
the same commit. Running from the repository root is what makes `git add .` cover
both packages, so the two versions cannot come apart.

## The SDKs share one version

`@glyphsoftware/datagit-sdk` and the PyPI `datagit-sdk` package are versioned and
released **together**, and a changeset here bumps both.

They are the same contract twice: both are generated from
`api/proto/datagit/v1/datagit.proto` plus a thin ergonomic layer, so a breaking
proto change breaks both at once. One version number means a user can reason
"SDK 1.2 speaks the 1.2 contract" without a compatibility table, and there is no
state in which the two SDKs disagree about what the service accepts.

Changesets cannot see the Python package, so `npm run version-packages` runs
`changeset version` and then propagates the result to `sdk/python`. CI asserts
the two never drift apart.

## Recording a change

```bash
make changeset          # from the repository root
```

Pick the bump and write one or two sentences aimed at **someone upgrading**:
what they have to do differently, not what you refactored.

## Choosing the bump

| | |
|---|---|
| **patch** | a fix that changes nothing a caller has to know about |
| **minor** | new surface; existing code keeps working untouched |
| **major** | existing code breaks, or a value changes shape on the wire |

**A change to the canonical encoding or the commit hash is always major**, even
when the SDK's own API is untouched. `datagit.commit.v1` is frozen (DESIGN.md
§12.1); if the wire meaning of a value moves, every commit id a user has stored
stops matching, and no amount of source compatibility makes that a minor.
