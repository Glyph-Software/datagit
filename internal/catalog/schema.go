// Package catalog owns DataGit's control tables and the generation of per-table
// version sidecars (DESIGN.md §5.2, §5.3).
package catalog

// ControlSchemaVersion is the version of the control-plane schema this build
// understands. Startup refuses to run against a newer one (DESIGN.md §17.2).
const ControlSchemaVersion = 1

// ControlSchema is the DESIGN.md §5.3 control schema, with the Phase 0 findings
// applied. It is written to be idempotent so it runs through the same resumable
// journalled state machine as user migrations (§17.2, dogfooded from M1).
const ControlSchema = `
CREATE TABLE IF NOT EXISTS datagit_meta (
    key   text PRIMARY KEY,
    value text NOT NULL
);

CREATE TABLE IF NOT EXISTS datagit_repo (
    id             uuid PRIMARY KEY,
    name           text NOT NULL UNIQUE,
    default_branch uuid NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS datagit_table (
    id             bigserial PRIMARY KEY,
    repo_id        uuid NOT NULL REFERENCES datagit_repo(id) ON DELETE CASCADE,
    physical_name  text NOT NULL,
    mode           text NOT NULL CHECK (mode IN ('audit','versioned')),
    pk_columns     integer[] NOT NULL,
    state          text NOT NULL CHECK (state IN ('backfilling','active','paused','untracking')),
    tracked_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, physical_name)
);

-- Stable column ids (DESIGN.md §10.5 rule 1, Phase 0 finding F1's sibling).
-- The sidecar's physical column for a live column is c_<id>, so renames are
-- metadata-only and a drop-then-re-add never collides with old history.
--
-- These must exist from the very first sidecar: retrofitting ids later is a
-- full sidecar rewrite.
CREATE TABLE IF NOT EXISTS datagit_column (
    table_id     bigint  NOT NULL REFERENCES datagit_table(id) ON DELETE CASCADE,
    id           integer NOT NULL,
    name         text    NOT NULL,
    sql_type     text    NOT NULL,
    kind         smallint NOT NULL,
    nullable     boolean NOT NULL,
    is_pk        boolean NOT NULL DEFAULT false,
    ordinal      integer NOT NULL,
    dropped_at   bigint,          -- schema epoch at which it was dropped
    PRIMARY KEY (table_id, id)
);

CREATE TABLE IF NOT EXISTS datagit_ref (
    id            uuid PRIMARY KEY,
    repo_id       uuid NOT NULL REFERENCES datagit_repo(id) ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN ('branch','tag')),
    name          text NOT NULL,
    head_commit   bytea,
    head_seq      bigint NOT NULL DEFAULT 0,
    parent_ref    uuid REFERENCES datagit_ref(id),
    fork_commit   bytea,
    fork_seq      bigint,

    -- Phase 0 finding F1. The inherited tail of this branch's resolution chain,
    -- CAPTURED AT FORK. It must not be re-derived from ancestors' live fork
    -- points: UpdateFromParent advances a fork point, and a descendant that
    -- rebuilt its chain would silently inherit rows it never asked for.
    chain         jsonb NOT NULL DEFAULT '[]'::jsonb,

    protected     boolean NOT NULL DEFAULT false,
    min_approvals smallint NOT NULL DEFAULT 0,
    created_by    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, kind, name)
);

CREATE TABLE IF NOT EXISTS datagit_commit (
    id            bytea PRIMARY KEY,
    repo_id       uuid   NOT NULL REFERENCES datagit_repo(id) ON DELETE CASCADE,
    branch_id     uuid   NOT NULL,
    seq           bigint NOT NULL,
    parent_ids    bytea[] NOT NULL,
    author        text   NOT NULL,      -- the authenticated principal, §15.2
    author_at     timestamptz NOT NULL,
    committer     text   NOT NULL,
    committed_at  timestamptz NOT NULL, -- the DATABASE clock, §7.2
    message       text   NOT NULL,
    external_ref  text   NOT NULL DEFAULT '',
    change_digest bytea  NOT NULL,
    schema_digest bytea  NOT NULL,
    schema_epoch  bigint NOT NULL DEFAULT 0,
    integrity     text   NOT NULL DEFAULT 'intact' CHECK (integrity IN ('intact','purged')),

    -- Phase 0 finding F1, again: the chain in force when this commit was made,
    -- so historical reads resolve against the world as it was, not as it became.
    chain         jsonb  NOT NULL DEFAULT '[]'::jsonb,

    UNIQUE (repo_id, branch_id, seq)
);
CREATE INDEX IF NOT EXISTS datagit_commit_branch ON datagit_commit (repo_id, branch_id, seq DESC);
CREATE INDEX IF NOT EXISTS datagit_commit_time   ON datagit_commit (repo_id, branch_id, committed_at DESC);

CREATE TABLE IF NOT EXISTS datagit_session (
    id          uuid PRIMARY KEY,
    repo_id     uuid NOT NULL REFERENCES datagit_repo(id) ON DELETE CASCADE,
    branch_id   uuid NOT NULL REFERENCES datagit_ref(id),
    principal   text NOT NULL,
    base_commit bytea NOT NULL,
    base_seq    bigint NOT NULL,
    state       text NOT NULL CHECK (state IN ('open','committing','committed','abandoned','expired')),
    lease_until timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS datagit_session_lease ON datagit_session (lease_until) WHERE state = 'open';

-- Idempotency keys, so a client retry after a lost response cannot double-apply
-- a commit (DESIGN.md §16.2).
CREATE TABLE IF NOT EXISTS datagit_idempotency (
    key          text PRIMARY KEY,
    principal    text NOT NULL,
    request_hash bytea NOT NULL,
    response     bytea NOT NULL,
    expires_at   timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS datagit_idempotency_gc ON datagit_idempotency (expires_at);

-- Proposals and conflicts (M3, §9.4). Conflicts are ROWS, not in-memory state:
-- a half-resolved merge must survive a service restart, a redeploy, and a
-- reviewer going home for the weekend.
CREATE TABLE IF NOT EXISTS datagit_proposal (
    id           bigserial PRIMARY KEY,
    repo_id      uuid NOT NULL REFERENCES datagit_repo(id) ON DELETE CASCADE,
    from_ref     uuid NOT NULL REFERENCES datagit_ref(id),
    into_ref     uuid NOT NULL REFERENCES datagit_ref(id),
    title        text NOT NULL,
    description  text NOT NULL DEFAULT '',
    state        text NOT NULL CHECK (state IN ('open','conflicted','approved','merged','closed')),
    merge_commit bytea,
    created_by   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS datagit_conflict (
    id           bigserial PRIMARY KEY,
    proposal_id  bigint NOT NULL REFERENCES datagit_proposal(id) ON DELETE CASCADE,
    table_id     bigint NOT NULL,
    pk_bytes     bytea  NOT NULL,
    column_id    integer,             -- NULL for whole-row conflicts
    kind         text   NOT NULL,     -- cell | add_add | delete_modify | unique | fk | check
    base_value   text,
    our_value    text,
    their_value  text,
    resolution   text,                -- ours | theirs | custom | NULL while unresolved
    resolved_value text,
    resolved_by  text,
    resolved_at  timestamptz
);
CREATE INDEX IF NOT EXISTS datagit_conflict_proposal ON datagit_conflict (proposal_id);

CREATE TABLE IF NOT EXISTS datagit_review (
    id          bigserial PRIMARY KEY,
    proposal_id bigint NOT NULL REFERENCES datagit_proposal(id) ON DELETE CASCADE,
    principal   text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('comment','approve','request_changes')),
    body        text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS datagit_migration_journal (
    plan_id      bigint  NOT NULL,
    ordinal      integer NOT NULL,
    kind         text    NOT NULL,
    sql_text     text    NOT NULL,
    started_at   timestamptz,
    completed_at timestamptz,
    PRIMARY KEY (plan_id, ordinal)
);

-- The purge tombstone (§13.4). It records THAT a purge happened -- by whom,
-- when, why, and how many versions -- and never the purged content.
-- Out-of-band writes observed by a capture-mode trigger (§6.3). A trigger has
-- no author, no message, and no commit boundary, so it records that a write
-- happened and leaves reconciliation to the drift verifier.
CREATE TABLE IF NOT EXISTS datagit_drift_log (
    id          bigserial PRIMARY KEY,
    table_name  text NOT NULL,
    op          text NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS datagit_drift_log_table ON datagit_drift_log (table_name, observed_at DESC);

CREATE TABLE IF NOT EXISTS datagit_purge_log (
    id               bigserial PRIMARY KEY,
    repo_id          uuid   NOT NULL REFERENCES datagit_repo(id) ON DELETE CASCADE,
    table_id         bigint NOT NULL,
    pk_bytes         bytea  NOT NULL,
    versions_removed integer NOT NULL,
    reason           text   NOT NULL,
    purged_by        text   NOT NULL,
    purged_at        timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS datagit_principal (
    id          uuid PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    key_hash    text,           -- Argon2id, §15.2
    disabled    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
`

// SidecarColumn names the physical sidecar column for a stable column id.
// DESIGN.md §10.5 rule 1.
func SidecarColumn(id uint32) string {
	return "c_" + itoa(uint64(id))
}

// SidecarTable names the sidecar for a live table.
func SidecarTable(physical string) string { return "datagit_v_" + physical }

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
