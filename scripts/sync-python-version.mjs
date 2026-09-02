#!/usr/bin/env node
// Propagate the SDK version from the TypeScript package to the Python one.
//
// Changesets computes versions from queued changesets, and it only understands
// npm packages. The two SDKs release together on one version number (see
// sdk/typescript/.changeset/README.md), so something has to carry the computed
// version across, and this is it.
//
//   node scripts/sync-python-version.mjs           write the version to Python
//   node scripts/sync-python-version.mjs --check   fail if they already disagree
//
// The --check mode runs in CI. Without it, a hand-edited version in one package
// would sit undetected until a release published two SDKs claiming to be the
// same version while being different code.

import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const tsPackage = join(root, "sdk/typescript/package.json");
const pyProject = join(root, "sdk/python/pyproject.toml");
const pyInit = join(root, "sdk/python/datagit/__init__.py");

const check = process.argv.includes("--check");

const version = JSON.parse(readFileSync(tsPackage, "utf8")).version;
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
  fail(`sdk/typescript/package.json has version ${JSON.stringify(version)}, which is not a version`);
}

// Both targets are matched with an anchored, single-capture pattern rather than
// a loose search: `version` appears in more than one place in a pyproject, and
// rewriting the wrong one would be silent.
const targets = [
  {
    path: pyProject,
    label: "pyproject.toml",
    pattern: /^(version\s*=\s*")([^"]*)(")$/m,
  },
  {
    path: pyInit,
    label: "datagit/__init__.py",
    pattern: /^(__version__\s*=\s*")([^"]*)(")$/m,
  },
];

let changed = false;
const mismatches = [];

for (const t of targets) {
  const text = readFileSync(t.path, "utf8");
  const m = text.match(t.pattern);
  if (!m) {
    fail(`${t.label} has no version line matching ${t.pattern}`);
  }
  if (m[2] === version) continue;

  if (check) {
    mismatches.push(`  ${t.label}: ${m[2]}  (TypeScript says ${version})`);
    continue;
  }
  writeFileSync(t.path, text.replace(t.pattern, `$1${version}$3`));
  console.log(`  ${t.label}: ${m[2]} -> ${version}`);
  changed = true;
}

if (check) {
  if (mismatches.length > 0) {
    fail(
      "the SDK versions have drifted apart:\n" +
        mismatches.join("\n") +
        "\n\nThe two SDKs are one contract twice and release on one version.\n" +
        "Record the change with `make changeset` rather than editing a version by hand;\n" +
        "to repair this, run `node scripts/sync-python-version.mjs`.",
    );
  }
  console.log(`SDK versions agree: ${version}`);
} else if (!changed) {
  console.log(`SDK versions already agree: ${version}`);
}

function fail(message) {
  console.error(`sync-python-version: ${message}`);
  process.exit(1);
}
