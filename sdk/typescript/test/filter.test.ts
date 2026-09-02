// Filters are typed trees with no string form, so there is nothing to inject
// into (§15.4). These tests pin that there is no path from user text to SQL.

import assert from "node:assert/strict";
import test from "node:test";

import { and, col, not, or } from "../src/filter.js";
import { dec } from "../src/values.js";

test("a comparison carries a typed value, never text", () => {
  const e = col(3).eq("'; DROP TABLE products; --");
  assert.equal(e.node.case, "compare");
  const c = e.node.value as { value?: { kind?: { case?: string; value?: unknown } } };
  // The hostile string is a VALUE, not syntax. It never becomes part of a
  // statement, which is why there is no escaping step anywhere in this SDK.
  assert.equal(c.value?.kind?.case, "textValue");
  assert.equal(c.value?.kind?.value, "'; DROP TABLE products; --");
});

test("filters compose", () => {
  const e = and(col(1).ge(dec("10.00")), or(col(2).eq("a"), not(col(2).isNull())));
  assert.equal(e.node.case, "and");
  const terms = (e.node.value as { terms: unknown[] }).terms;
  assert.equal(terms.length, 2);
});

test("in-lists carry each value typed", () => {
  const e = col(4).in(["a", "b"]);
  assert.equal(e.node.case, "in");
  assert.equal((e.node.value as { values: unknown[] }).values.length, 2);
});
