# Changesets

This directory records **version intent**: what changed, and whether it is a
patch, a minor, or a breaking change. It does not record what the diff did — the
commit message does that.

## Why it lives here and not at the repository root

Changesets is a JavaScript tool. Putting a `package.json` at the root of a Go
repository so that a JS tool could find it would be a worse trade than asking you
to `cd sdk/typescript`, and `make changeset` from the root does that for you.

## The SDKs share one version

`@glyphsoftware/datagit-sdk` and the Python `datagit` package are versioned and
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
