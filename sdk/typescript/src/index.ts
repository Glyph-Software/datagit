// DataGit's TypeScript SDK.
//
// The generated code is complete and unpleasant. This layer makes the common
// paths short without hiding what the service does.
//
// Three things it deliberately does NOT do:
//
// It does not let you set a commit author. The author comes from the credential
// (DESIGN.md §15.2); an audit trail whose author is client-supplied is
// decoration. There is no field to pass.
//
// It does not carry exact numbers as `number`. JavaScript's number is a float64,
// and a value is hashed into history, so a rounding difference would change a
// commit id for data nobody edited. Exact values use bigint and Decimal.
//
// It does not build filters from strings. Filters are typed expressions with no
// SQL text form, so there is nothing to inject into (§15.4).

export { Decimal, dec, toWire, fromWire } from "./values.js";
export type { Cell } from "./values.js";
export { DataGitClient, ConflictError, NeedsMigrationError } from "./client.js";
export type { ClientOptions, RowData } from "./client.js";
export { col, and, or, not } from "./filter.js";
