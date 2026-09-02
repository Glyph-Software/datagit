// Value conversion is where an SDK silently corrupts data, so it is tested
// first and hardest. JavaScript makes this worse than most languages: `number`
// is a float64, so the obvious mapping is the wrong one for exactly the values
// that matter.

import assert from "node:assert/strict";
import test from "node:test";

import { dec, Decimal, fromWire, toWire } from "../src/values.js";

test("exact decimals round-trip without becoming floats", () => {
  for (const s of ["0.10", "1234567890.123456789", "-0.00", "0"]) {
    const wire = toWire(dec(s));
    assert.equal(wire.kind?.case, "numericValue", `${s} did not travel as a numeric`);
    const back = fromWire(wire);
    assert.ok(back instanceof Decimal, `${s} came back as ${typeof back}`);
    assert.equal((back as Decimal).toString(), s, `${s} round-tripped to ${back}`);
  }
});

test("0.1 survives, which it would not as a float", () => {
  // 0.1 has no exact float64 representation. The whole reason Decimal exists.
  assert.equal(String(fromWire(toWire(dec("0.1")))), "0.1");
});

test("big integers stay exact", () => {
  // 2^53 + 1 is the first integer a JavaScript number cannot represent. Commit
  // sequence numbers reach into that range.
  const n = 9007199254740993n;
  const back = fromWire(toWire(n));
  assert.equal(back, n, "an int64 past 2^53 lost precision");
  assert.equal(typeof back, "bigint", "an integer came back as a number");
});

test("null and empty string stay distinct", () => {
  assert.equal(fromWire(toWire(null)), null);
  assert.equal(fromWire(toWire("")), "");
  assert.equal(toWire(null).kind?.case, "isNull");
});

test("NaN and Infinity are refused, not encoded", () => {
  // NaN is not equal to itself, so it has no canonical encoding: a hash needs
  // equality to mean something.
  for (const v of [NaN, Infinity, -Infinity]) {
    assert.throws(() => toWire(v), /canonical encoding|NaN/,
      `${v} was accepted`);
  }
});

test("a malformed decimal is refused at construction", () => {
  assert.throws(() => dec("twelve"), /not a decimal/);
  assert.throws(() => dec("1.2.3"), /not a decimal/);
});

test("bytes and text stay distinct", () => {
  const b = new Uint8Array([0, 255]);
  assert.deepEqual(fromWire(toWire(b)), b);
  assert.equal(fromWire(toWire("hello")), "hello");
});

test("dates round-trip", () => {
  const d = new Date("2026-09-02T12:30:45.000Z");
  assert.equal((fromWire(toWire(d)) as Date).toISOString(), d.toISOString());
});

test("an unsupported type is refused with advice", () => {
  assert.throws(
    () => toWire({ not: "a value" } as never),
    /bigint for exact integers/,
    "the refusal should say what to use instead",
  );
});
