// Conversion between JavaScript values and DataGit's canonical wire values.
//
// This file exists because of one hazard, and everything in it follows from
// that hazard: JavaScript's `number` is a float64. It cannot hold every int64,
// and it cannot hold an exact decimal at all. A value sent through DataGit is
// HASHED INTO HISTORY, so a value that changes shape on the way through changes
// the commit id for data the user never edited.
//
// So the mapping is explicit and refuses the ambiguous cases:
//
//   bigint   -> int          exact 64-bit integers
//   Decimal  -> numeric      exact decimals, carried as a string
//   number   -> float        ONLY where a float is what the column holds
//
// A `number` for a decimal column is refused rather than silently rounded.

import type { Value } from "./gen/datagit/v1/datagit_pb.js";
import { timestampFromDate, timestampDate } from "@bufbuild/protobuf/wkt";

/** An exact decimal. A string, because that is the only JavaScript type that
 *  holds one without loss. */
export class Decimal {
  constructor(readonly text: string) {
    if (!/^-?\d+(\.\d+)?([eE][-+]?\d+)?$/.test(text)) {
      throw new TypeError(`not a decimal: ${JSON.stringify(text)}`);
    }
  }
  toString(): string {
    return this.text;
  }
}

/** dec is shorthand for building a Decimal. */
export function dec(text: string | number): Decimal {
  return new Decimal(typeof text === "number" ? String(text) : text);
}

export type Cell = null | boolean | bigint | number | Decimal | string | Uint8Array | Date;

/** toWire converts a JavaScript value to a wire Value. */
export function toWire(v: Cell): Value {
  if (v === null || v === undefined) {
    return { $typeName: "datagit.v1.Value", kind: { case: "isNull", value: true } } as Value;
  }
  if (typeof v === "boolean") {
    return { $typeName: "datagit.v1.Value", kind: { case: "boolValue", value: v } } as Value;
  }
  if (typeof v === "bigint") {
    return { $typeName: "datagit.v1.Value", kind: { case: "intValue", value: v } } as Value;
  }
  if (v instanceof Decimal) {
    return {
      $typeName: "datagit.v1.Value",
      kind: { case: "numericValue", value: v.text },
    } as Value;
  }
  if (typeof v === "number") {
    if (!Number.isFinite(v)) {
      throw new TypeError(
        `cannot send ${v}: NaN and Infinity have no canonical encoding, because ` +
          `NaN is not equal to itself and a hash needs equality to mean something`,
      );
    }
    return { $typeName: "datagit.v1.Value", kind: { case: "floatValue", value: v } } as Value;
  }
  if (typeof v === "string") {
    return { $typeName: "datagit.v1.Value", kind: { case: "textValue", value: v } } as Value;
  }
  if (v instanceof Uint8Array) {
    return { $typeName: "datagit.v1.Value", kind: { case: "bytesValue", value: v } } as Value;
  }
  if (v instanceof Date) {
    return {
      $typeName: "datagit.v1.Value",
      kind: { case: "timeValue", value: timestampFromDate(v) },
    } as Value;
  }
  throw new TypeError(
    `cannot send ${typeof v} to DataGit. Use bigint for exact integers, ` +
      `dec("1.25") for exact decimals, Date for timestamps, or Uint8Array for binary`,
  );
}

/** fromWire converts a wire Value to a JavaScript value. */
export function fromWire(v: Value): Cell {
  switch (v.kind?.case) {
    case "isNull":
    case undefined:
      return null;
    case "boolValue":
      return v.kind.value;
    case "intValue":
      // A bigint, not a number: an int64 past 2^53 loses precision as a number,
      // and DataGit's sequence numbers reach into that range.
      return v.kind.value;
    case "floatValue":
      return v.kind.value;
    case "numericValue":
      return new Decimal(v.kind.value);
    case "textValue":
      return v.kind.value;
    case "bytesValue":
      return v.kind.value;
    case "timeValue":
      return timestampDate(v.kind.value);
  }
  throw new Error(`unknown value kind ${(v.kind as { case?: string })?.case}`);
}
