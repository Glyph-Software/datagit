package catalog

import "github.com/Glyph-Software/datagit/internal/adapter"

// ControlSchemaFor returns the control schema for an engine (§4.3, §5.3).
//
// Two texts rather than one generated from a common description. A generator
// would have to model CHECK constraints, partial indexes, foreign-key syntax,
// and per-engine key-length limits, and that model would be a second schema
// language to get wrong. Two readable texts, with a test asserting they declare
// the same tables and columns, is the smaller thing to keep right.
func ControlSchemaFor(d adapter.Dialect) string {
	if d == adapter.MySQL {
		return ControlSchemaMySQL
	}
	return ControlSchema
}

// ControlSchemaMySQL is the §5.3 control schema for MySQL 8.4.
//
// The differences from the PostgreSQL text are all forced, and each one is
// marked where it appears:
//
//   - uuid -> binary(16), bytea -> varbinary(n), timestamptz -> datetime(6).
//   - Indexed text columns need a bounded length; MySQL cannot index an
//     unbounded TEXT without a prefix. 255 characters is the declared limit for
//     names, which is longer than any identifier either engine accepts.
//   - Indexes are declared INSIDE the table rather than as separate statements,
//     because MySQL has no CREATE INDEX IF NOT EXISTS and the whole script must
//     stay re-runnable (§17.2).
//   - The session lease index is plain rather than partial (Caps.PartialIndexes).
//     It selects more rows than PostgreSQL's; the sweeper filters on state
//     anyway, so the difference is a scan cost, not a behaviour change.
const ControlSchemaMySQL = `
CREATE TABLE IF NOT EXISTS datagit_meta (
    meta_key   varchar(255) NOT NULL PRIMARY KEY,
    meta_value text NOT NULL
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_repo (
    id             binary(16) NOT NULL PRIMARY KEY,
    name           varchar(255) NOT NULL UNIQUE,
    default_branch binary(16) NOT NULL,
    created_at     datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_table (
    id             bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    repo_id        binary(16) NOT NULL,
    physical_name  varchar(255) NOT NULL,
    mode           varchar(16) NOT NULL CHECK (mode IN ('audit','versioned')),
    pk_columns     text NOT NULL,
    state          varchar(16) NOT NULL CHECK (state IN ('backfilling','active','paused','untracking')),
    tracked_at     datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY datagit_table_uq (repo_id, physical_name),
    CONSTRAINT datagit_table_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_column (
    table_id     bigint  NOT NULL,
    id           int     NOT NULL,
    name         varchar(255) NOT NULL,
    sql_type     varchar(255) NOT NULL,
    kind         smallint NOT NULL,
    nullable     boolean NOT NULL,
    is_pk        boolean NOT NULL DEFAULT false,
    ordinal      int     NOT NULL,
    dropped_at   bigint,
    PRIMARY KEY (table_id, id),
    CONSTRAINT datagit_column_table FOREIGN KEY (table_id)
        REFERENCES datagit_table(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_ref (
    id            binary(16) NOT NULL PRIMARY KEY,
    repo_id       binary(16) NOT NULL,
    kind          varchar(16) NOT NULL CHECK (kind IN ('branch','tag')),
    name          varchar(255) NOT NULL,
    head_commit   varbinary(32),
    head_seq      bigint NOT NULL DEFAULT 0,
    parent_ref    binary(16),
    fork_commit   varbinary(32),
    fork_seq      bigint,
    chain         text NOT NULL,
    protected     boolean NOT NULL DEFAULT false,
    min_approvals smallint NOT NULL DEFAULT 0,
    merge_in_progress boolean NOT NULL DEFAULT false,
    created_by    varchar(255) NOT NULL,
    created_at    datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    UNIQUE KEY datagit_ref_uq (repo_id, kind, name),
    KEY datagit_ref_parent (parent_ref),
    CONSTRAINT datagit_ref_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE,
    CONSTRAINT datagit_ref_parent_fk FOREIGN KEY (parent_ref) REFERENCES datagit_ref(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_commit (
    id            varbinary(32) NOT NULL PRIMARY KEY,
    repo_id       binary(16) NOT NULL,
    branch_id     binary(16) NOT NULL,
    seq           bigint NOT NULL,
    parent_ids    text NOT NULL,
    author        varchar(255) NOT NULL,
    author_at     datetime(6) NOT NULL,
    committer     varchar(255) NOT NULL,
    committed_at  datetime(6) NOT NULL,
    message       text   NOT NULL,
    external_ref  varchar(255) NOT NULL DEFAULT '',
    change_digest varbinary(32) NOT NULL,
    schema_digest varbinary(32) NOT NULL,
    schema_epoch  bigint NOT NULL DEFAULT 0,
    integrity     varchar(16) NOT NULL DEFAULT 'intact' CHECK (integrity IN ('intact','purged')),
    chain         text NOT NULL,
    UNIQUE KEY datagit_commit_uq (repo_id, branch_id, seq),
    KEY datagit_commit_branch (repo_id, branch_id, seq DESC),
    KEY datagit_commit_time (repo_id, branch_id, committed_at DESC),
    CONSTRAINT datagit_commit_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_session (
    id          binary(16) NOT NULL PRIMARY KEY,
    repo_id     binary(16) NOT NULL,
    branch_id   binary(16) NOT NULL,
    principal   varchar(255) NOT NULL,
    base_commit varbinary(32) NOT NULL,
    base_seq    bigint NOT NULL,
    state       varchar(16) NOT NULL CHECK (state IN ('open','committing','committed','abandoned','expired')),
    lease_until datetime(6) NOT NULL,
    created_at  datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at  datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY datagit_session_lease (state, lease_until),
    KEY datagit_session_branch (branch_id),
    CONSTRAINT datagit_session_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE,
    CONSTRAINT datagit_session_ref FOREIGN KEY (branch_id) REFERENCES datagit_ref(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_idempotency (
    idem_key     varchar(255) NOT NULL PRIMARY KEY,
    principal    varchar(255) NOT NULL,
    request_hash varbinary(32) NOT NULL,
    response     blob NOT NULL,
    expires_at   datetime(6) NOT NULL,
    KEY datagit_idempotency_gc (expires_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_proposal (
    id           bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    repo_id      binary(16) NOT NULL,
    from_ref     binary(16) NOT NULL,
    into_ref     binary(16) NOT NULL,
    title        varchar(512) NOT NULL,
    description  text NOT NULL,
    state        varchar(16) NOT NULL CHECK (state IN ('open','conflicted','approved','merged','closed')),
    merge_commit varbinary(32),
    created_by   varchar(255) NOT NULL,
    created_at   datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY datagit_proposal_from (from_ref),
    KEY datagit_proposal_into (into_ref),
    CONSTRAINT datagit_proposal_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE,
    CONSTRAINT datagit_proposal_from_fk FOREIGN KEY (from_ref) REFERENCES datagit_ref(id),
    CONSTRAINT datagit_proposal_into_fk FOREIGN KEY (into_ref) REFERENCES datagit_ref(id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_conflict (
    id           bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    proposal_id  bigint NOT NULL,
    table_id     bigint NOT NULL,
    pk_bytes     varbinary(1024) NOT NULL,
    column_id    int,
    kind         varchar(32) NOT NULL,
    base_value   text,
    our_value    text,
    their_value  text,
    resolution   varchar(16),
    resolved_value text,
    resolved_by  varchar(255),
    resolved_at  datetime(6),
    KEY datagit_conflict_proposal (proposal_id),
    CONSTRAINT datagit_conflict_proposal_fk FOREIGN KEY (proposal_id)
        REFERENCES datagit_proposal(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_review (
    id          bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    proposal_id bigint NOT NULL,
    principal   varchar(255) NOT NULL,
    kind        varchar(32) NOT NULL CHECK (kind IN ('comment','approve','request_changes')),
    body        text NOT NULL,
    created_at  datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY datagit_review_proposal (proposal_id),
    CONSTRAINT datagit_review_proposal_fk FOREIGN KEY (proposal_id)
        REFERENCES datagit_proposal(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_migration_journal (
    plan_id      bigint  NOT NULL,
    ordinal      int     NOT NULL,
    kind         varchar(32) NOT NULL,
    sql_text     text    NOT NULL,
    started_at   datetime(6),
    completed_at datetime(6),
    PRIMARY KEY (plan_id, ordinal)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_drift_log (
    id          bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    table_name  varchar(255) NOT NULL,
    op          varchar(16) NOT NULL,
    observed_at datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY datagit_drift_log_table (table_name, observed_at DESC)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_purge_log (
    id               bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    repo_id          binary(16) NOT NULL,
    table_id         bigint NOT NULL,
    pk_bytes         varbinary(1024) NOT NULL,
    versions_removed int NOT NULL,
    reason           text NOT NULL,
    purged_by        varchar(255) NOT NULL,
    purged_at        datetime(6) NOT NULL,
    CONSTRAINT datagit_purge_log_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_schema_version (
    table_id    bigint NOT NULL,
    branch_id   binary(16) NOT NULL,
    epoch       bigint NOT NULL,
    columns     text   NOT NULL,
    dropped     text   NOT NULL,
    digest      varbinary(32) NOT NULL,
    mask_width  int    NOT NULL,
    created_by  varchar(255) NOT NULL,
    created_at  datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (table_id, branch_id, epoch)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_migration_plan (
    id           bigint NOT NULL AUTO_INCREMENT PRIMARY KEY,
    repo_id      binary(16) NOT NULL,
    table_id     bigint NOT NULL,
    proposal_id  bigint,
    ops          text   NOT NULL,
    target_epoch bigint NOT NULL,
    state        varchar(16) NOT NULL CHECK (state IN ('pending','applying','applied','failed','abandoned')),
    created_by   varchar(255) NOT NULL,
    created_at   datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    applied_by   varchar(255),
    applied_at   datetime(6),
    CONSTRAINT datagit_migration_plan_repo FOREIGN KEY (repo_id)
        REFERENCES datagit_repo(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS datagit_principal (
    id          binary(16) NOT NULL PRIMARY KEY,
    name        varchar(255) NOT NULL UNIQUE,
    key_hash    varchar(255),
    disabled    boolean NOT NULL DEFAULT false,
    created_at  datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;
`
