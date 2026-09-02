// The ergonomic client, over Connect's gRPC transport.

import { createClient, type Client } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node";

import {
  Repository,
  Data,
  Version,
  Branching,
  Proposals,
  Admin,
  Op,
  type Change,
  type ConflictInfo,
  type Expr,
} from "./gen/datagit/v1/datagit_pb.js";
import { fromWire, toWire, type Cell } from "./values.js";

/** RowData is a row keyed by STABLE column id, never by name, so a rename does
 *  not change what a row means (§10.5 rule 1). */
export type RowData = Record<number, Cell>;

export interface ClientOptions {
  /** Base URL of the DataGit service. */
  baseUrl: string;
  /** API key. It identifies the principal, and the principal is what commits
   *  are attributed to. There is no way to commit as someone else. */
  apiKey: string;
}

/** ConflictError is a merge that did not apply because cells disagree.
 *
 *  Not a failure in the usual sense: DataGit surfaces conflicts rather than
 *  guessing, and NOTHING was applied. */
export class ConflictError extends Error {
  constructor(readonly conflicts: ConflictInfo[]) {
    super(
      `${conflicts.length} conflict(s); nothing was applied. ` +
        `Resolve them and merge again`,
    );
    this.name = "ConflictError";
  }
}

/** NeedsMigrationError is a merge whose DATA applied and whose SHAPE change is
 *  waiting.
 *
 *  Not a failure. Applications read the live table directly, so a column that
 *  appears or vanishes mid-query has no rollout window; the schema change is a
 *  plan to be applied deliberately (§10.4). */
export class NeedsMigrationError extends Error {
  constructor(readonly planId: bigint) {
    super(
      `data merged; migration plan ${planId} is pending. ` +
        `Apply it when readers can tolerate the change`,
    );
    this.name = "NeedsMigrationError";
  }
}

export class DataGitClient {
  readonly repository: Client<typeof Repository>;
  readonly data: Client<typeof Data>;
  readonly version: Client<typeof Version>;
  readonly branching: Client<typeof Branching>;
  readonly proposals: Client<typeof Proposals>;
  readonly admin: Client<typeof Admin>;

  constructor(opts: ClientOptions) {
    const transport = createGrpcTransport({
      baseUrl: opts.baseUrl,
      interceptors: [
        (next) => (req) => {
          req.header.set("authorization", `Bearer ${opts.apiKey}`);
          return next(req);
        },
      ],
    });
    this.repository = createClient(Repository, transport);
    this.data = createClient(Data, transport);
    this.version = createClient(Version, transport);
    this.branching = createClient(Branching, transport);
    this.proposals = createClient(Proposals, transport);
    this.admin = createClient(Admin, transport);
  }

  /** scan streams rows from a branch.
   *
   *  Reads on `main` do not need DataGit at all — query the table directly.
   *  This is for reading a BRANCH, or a point in history. */
  async *scan(args: {
    repo: string;
    table: string;
    branch?: string;
    filter?: Expr;
    limit?: number;
  }): AsyncIterable<RowData> {
    const stream = this.data.scan({
      repo: args.repo,
      table: args.table,
      branch: args.branch ?? "main",
      filter: args.filter,
      limit: args.limit ?? 0,
    });
    for await (const row of stream) {
      const out: RowData = {};
      for (const [k, v] of Object.entries(row.cells)) {
        out[Number(k)] = fromWire(v);
      }
      yield out;
    }
  }

  /** transaction buffers changes and writes them as ONE commit.
   *
   *  Buffering is not only ergonomics. A commit takes the branch's ref lock, so
   *  throughput is commits per second regardless of how many rows each carries
   *  (§11.3): one commit of a thousand rows costs what one commit of one row
   *  costs. A loop of single-row commits is the slowest way to write.
   *
   *  A throw inside the callback abandons the buffer without committing. */
  async transaction(
    args: { repo: string; table: string; branch?: string; message: string },
    fn: (tx: Tx) => void | Promise<void>,
  ): Promise<Uint8Array> {
    const tx = new Tx();
    await fn(tx);
    if (tx.changes.length === 0) return new Uint8Array();
    // No author field, and there never will be one: the author comes from the
    // credential this client authenticated with (§15.2).
    const res = await this.version.commit({
      repo: args.repo,
      table: args.table,
      branch: args.branch ?? "main",
      message: args.message,
      changes: tx.changes,
    });
    return res.id;
  }
}

/** Tx buffers changes for one commit. */
export class Tx {
  readonly changes: Change[] = [];

  insert(pk: Uint8Array, row: RowData): void {
    this.changes.push(this.change(pk, Op.INSERT, row));
  }
  update(pk: Uint8Array, row: RowData): void {
    this.changes.push(this.change(pk, Op.UPDATE, row));
  }
  delete(pk: Uint8Array): void {
    this.changes.push({ $typeName: "datagit.v1.Change", pk, op: Op.DELETE } as Change);
  }

  private change(pk: Uint8Array, op: Op, row: RowData): Change {
    const cells: Record<number, ReturnType<typeof toWire>> = {};
    for (const [k, v] of Object.entries(row)) cells[Number(k)] = toWire(v);
    return {
      $typeName: "datagit.v1.Change",
      pk,
      op,
      row: { $typeName: "datagit.v1.Row", cells },
    } as Change;
  }
}
