# SDKs

Both SDKs are **generated stubs plus a thin ergonomic layer**. The generated
part is complete and unpleasant; the layer makes the common paths short without
hiding what the service does.

| | Path | Package | Regenerate | Test |
|---|---|---|---|---|
| Python | `sdk/python` | `datagit-sdk` on PyPI | `make sdk-py` | `make test-sdk-py` |
| TypeScript | `sdk/typescript` | `@glyphsoftware/datagit-sdk` on npm | `make sdk-ts` | `make test-sdk-ts` |

Both are generated from `api/proto/datagit/v1/datagit.proto`. `make sdk-py` needs
`grpcio-tools`; point `PYTHON` at a virtualenv if your system Python is
externally managed:

```bash
make sdk-py PYTHON=.venv/bin/python
```

## Three things neither SDK does

**Neither lets you set a commit author.** The author comes from the credential
(DESIGN.md §15.2). An audit trail whose author is client-supplied is decoration,
so there is no field to pass — not a validated one, not one at all.

**Neither carries exact numbers as a float.** A value is hashed into history, so
a rounding difference on the way through the SDK would change the commit id for
data nobody edited. Python uses `Decimal`; TypeScript uses `bigint` and a
`Decimal` wrapper, because JavaScript's `number` is a float64 and cannot hold
either an exact decimal or every int64.

**Neither builds filters from strings.** Filters are typed expression trees with
no SQL text form, so there is nothing to inject into (§15.4). A hostile string
passed as a filter value stays a value.

## Rows are keyed by column id

Both SDKs key rows by **stable column id**, not by name (§10.5 rule 1). A rename
is metadata-only in DataGit, and keying by name would make a rename silently
change what a row means on the wire.

## Batching is not just ergonomics

A commit takes the branch's ref lock, so throughput is commits per second
regardless of how many rows each one carries (§11.3). One commit of a thousand
rows costs roughly what one commit of one row costs. Both SDKs expose a
transaction that buffers changes into a single commit; a loop of single-row
commits is the slowest way to write.

## Python

```python
from decimal import Decimal
from datagit import Client, col

with Client("datagit.internal:443", api_key=KEY) as c:
    items = c.repo("catalog").table("products")

    # Reading a BRANCH. Reads on main need no DataGit at all — query the table.
    for row in items.read(branch="q4-pricing", where=col(3) == "outdoor"):
        print(row)

    # One commit, however many rows.
    with items.transaction(branch="q4-pricing", message="Q4 pricing") as tx:
        tx.update(pk, {1: "TENT-4P", 4: Decimal("268.92")})
```

## TypeScript

```ts
import { DataGitClient, col, dec } from "@glyphsoftware/datagit-sdk";

const c = new DataGitClient({ baseUrl: "https://datagit.internal", apiKey: KEY });

for await (const row of c.scan({
  repo: "catalog", table: "products", branch: "q4-pricing",
  filter: col(3).eq("outdoor"),
})) {
  console.log(row);
}

await c.transaction(
  { repo: "catalog", table: "products", branch: "q4-pricing", message: "Q4 pricing" },
  (tx) => tx.update(pk, { 1: "TENT-4P", 4: dec("268.92") }),
);
```

---

# Releasing

## The two SDKs share one version

They are the same contract twice — both generated from the same proto plus a thin
layer — so a breaking proto change breaks both at once. One version number means
a user can reason "SDK 1.2 speaks the 1.2 contract" without a compatibility
table, and there is no state in which the two SDKs disagree about what the
service accepts.

[Changesets](https://github.com/changesets/changesets) is the source of version
intent. It only understands npm, so `npm run version-packages` runs
`changeset version` and then carries the computed version to `sdk/python`. CI
asserts the two never drift apart.

## Recording a change

Any pull request that changes the SDKs or the proto should carry a changeset:

```bash
make changeset
```

Pick the bump and write a sentence or two aimed at **someone upgrading**. A
change to the canonical encoding is always a **major**, even when the SDK's own
API is untouched: `datagit.commit.v1` is frozen (DESIGN.md §12.1), and if the
wire meaning of a value moves, every commit id a user has stored stops matching.

## What happens on merge

1. Merging a change with a changeset makes `.github/workflows/sdk-release.yml`
   open (or update) a **"Release: version the SDK packages"** pull request. That
   PR consumes the queued changesets, bumps both packages, and writes the
   changelog.
2. Merging *that* PR publishes to npm and PyPI.

The version bump is a **reviewed commit**, not a side effect of merging. Someone
sees the computed version and the changelog before anything reaches a registry,
which matters because neither registry lets you re-upload a version — not even
after an unpublish.

## If the release half-fails

npm and PyPI are published in that order and the two uploads are **not atomic**;
no arrangement of CI would make them so. If the PyPI job fails after npm has
published, npm is ahead.

**Re-run the PyPI job.** The version on `main` is already correct and PyPI has
nothing at that version yet. Do not cut a new version to route around it — that
would leave a released npm version with no Python counterpart forever.

## One-time repository setup

Neither is needed until the first release.

| | |
|---|---|
| **npm** | The `@glyphsoftware` scope must exist on npm, and an `NPM_TOKEN` repository secret must have publish rights on it. **The token type matters** — see below. The workflow publishes with [provenance](https://docs.npmjs.com/generating-provenance-statements), so it also needs `id-token: write` — already set. |
| **PyPI** | A [trusted publisher](https://docs.pypi.org/trusted-publishers/) for `datagit`: owner `Glyph-Software`, repository `datagit`, workflow `sdk-release.yml`, environment `pypi`. No token, so there is no long-lived credential to leak. Confirm `datagit-sdk` is free before the first release, not after: the two registries publish in sequence and are not atomic, so a taken name fails the PyPI half once npm has already gone out.|

The `pypi` environment should be created in repository settings, with required
reviewers if you want a human gate in front of the upload itself.

### The npm token has to be one that bypasses two-factor auth

If the npm account requires 2FA for write actions — the default for a while now —
most token types still demand a one-time password, and CI has no authenticator:

```
npm error code EOTP
npm error This operation requires a one-time password from your authenticator.
```

| Token | Works in CI? |
|---|---|
| **Granular access token** | **yes** — this is the one to use |
| Classic → Automation | yes, but npm no longer offers this on newer accounts |
| Classic → Publish | no — still prompts for an OTP |
| Classic → Read-only | no |

The failure is late and looks like a permissions problem: the token authenticates,
the provenance statement is signed and logged, and only then does the registry
reject the upload. Nothing is published when this happens.

**The account's 2FA mode has to allow token writes.** npm accounts set to
*Authorization and writes* demand a one-time password for every publish, and CI
has no authenticator — every write fails with `EOTP` while reads keep working, so
the token looks valid right up to the upload. Set
[Two-Factor Authentication](https://www.npmjs.com/settings/~/profile) to
**Authorization only**: login keeps its second factor, tokens can publish.

The honest trade: with `Authorization only`, anyone holding the token can publish.
That is inherent to unattended publishing, and the way back out is
[trusted publishing](https://docs.npmjs.com/trusted-publishers) — once the package
exists, attach a trusted publisher, delete `NPM_TOKEN`, and set 2FA back to
*Authorization and writes*. OIDC is not a token, so the strict setting stops
mattering.

**Creating the granular token**, at
[npmjs.com/settings/~/tokens/new](https://www.npmjs.com/settings/~/tokens/new):

- **Packages and scopes** → Permissions: **Read and write**.
- **Select packages**: choose **All packages**, not "Only select packages". The
  package does not exist yet, so it cannot appear in the picker — a token scoped
  to selected packages therefore cannot create it, and the publish fails with 403.
  Narrow the token to the package after the first release if you want.
- **Organizations** → **Read and write**, if `@glyphsoftware` is an npm
  organization. Creating a *new* package inside an org scope needs it.
- Note the **expiry**. A granular token always has one, so the release will start
  failing on a date rather than gradually.

`@glyphsoftware` also has to exist as an npm organization before anything can be
published into it — a scope is not created implicitly by a publish the way a
user scope is.

### The first publish needs a token even if you want OIDC

npm trusted publishing (OIDC) is the better steady state — no stored credential —
but it is configured **per package on npmjs.com**, and a package that has never
been published cannot have a trusted publisher attached to it. `changeset publish`
detects the OIDC token, finds no npm package to match it against, and falls back
to needing auth:

```
No NPM_TOKEN found, but OIDC is available - using npm trusted publishing
error: ENEEDAUTH This command requires you to be logged in to https://registry.npmjs.org
```

So the first release goes out with an `NPM_TOKEN`. After that the package exists,
a trusted publisher can be configured for it, and the secret can be deleted.

PyPI has no such bootstrap problem: a trusted publisher there can be created for
a project name that does not exist yet.

### If the release cannot open its pull request

`changesets/action` pushes the version commit and then opens a pull request for
it. That second step fails with

> GitHub Actions is not permitted to create or approve pull requests

when the organization forbids it. The repository-level setting is locked in that
case — changing it returns `409 Conflict` — so it has to be fixed in one of two
places:

- **Organization settings** → Actions → General → Workflow permissions → *Allow
  GitHub Actions to create and approve pull requests*. Needs an org owner.
- **Or add a `CHANGESETS_TOKEN` secret**: a fine-grained PAT scoped to this
  repository with `contents: write` and `pull-requests: write`. The restriction
  applies to the Actions token, not a user token, so this works with no org
  change. The workflow already prefers it when present.

Either way the version commit itself is safe: it is pushed to
`changeset-release/main` before the failure, so nothing is lost — a pull request
opened by hand from that branch releases exactly the same thing.
