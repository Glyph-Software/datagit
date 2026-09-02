// Typed filter expressions (§7.4, §15.4).
//
// There is deliberately no string form. With no SQL text to build, there is
// nothing to inject into, and the column is referenced by its STABLE id so a
// rename does not silently change what a filter means.

import type { Expr } from "./gen/datagit/v1/datagit_pb.js";
import { CompareOp } from "./gen/datagit/v1/datagit_pb.js";
import { toWire, type Cell } from "./values.js";

function compare(colId: number, op: CompareOp, value: Cell): Expr {
  return {
    $typeName: "datagit.v1.Expr",
    node: {
      case: "compare",
      value: { $typeName: "datagit.v1.Compare", col: colId, op, value: toWire(value) },
    },
  } as Expr;
}

/** col references a column by its stable id and builds comparisons on it. */
export function col(colId: number) {
  return {
    eq: (v: Cell) => compare(colId, CompareOp.EQ, v),
    ne: (v: Cell) => compare(colId, CompareOp.NE, v),
    lt: (v: Cell) => compare(colId, CompareOp.LT, v),
    le: (v: Cell) => compare(colId, CompareOp.LE, v),
    gt: (v: Cell) => compare(colId, CompareOp.GT, v),
    ge: (v: Cell) => compare(colId, CompareOp.GE, v),
    like: (v: string) => compare(colId, CompareOp.LIKE, v),
    isNull: (): Expr =>
      ({
        $typeName: "datagit.v1.Expr",
        node: {
          case: "isNull",
          value: { $typeName: "datagit.v1.IsNull", col: colId },
        },
      }) as Expr,
    in: (vs: Cell[]): Expr =>
      ({
        $typeName: "datagit.v1.Expr",
        node: {
          case: "in",
          value: {
            $typeName: "datagit.v1.In",
            col: colId,
            values: vs.map(toWire),
          },
        },
      }) as Expr,
  };
}

export function and(...terms: Expr[]): Expr {
  return {
    $typeName: "datagit.v1.Expr",
    node: { case: "and", value: { $typeName: "datagit.v1.Junction", terms } },
  } as Expr;
}

export function or(...terms: Expr[]): Expr {
  return {
    $typeName: "datagit.v1.Expr",
    node: { case: "or", value: { $typeName: "datagit.v1.Junction", terms } },
  } as Expr;
}

export function not(term: Expr): Expr {
  return {
    $typeName: "datagit.v1.Expr",
    node: { case: "not", value: term },
  } as Expr;
}
