-- S1: synthetic version sidecar for branch-resolution measurement.
-- THROWAWAY spike code (PLAN.md Phase 0). Schema mirrors DESIGN.md §5.2.
--
-- Shape:
--   main   10,000,000 live keys x 5 versions (4 closed + 1 open) = 50,000,000
--   b1..b7 200,000 overlay rows each                             =  1,400,000
--
-- Branch chain is main <- b1 <- ... <- b7, giving resolution depths 1 (main),
-- 3 (b2) and 8 (b7) as PLAN.md S1 requires.

\set ON_ERROR_STOP on

DROP TABLE IF EXISTS datagit_v_products;

-- UNLOGGED: this is throwaway spike data and we are measuring reads, not
-- durability. It roughly halves build time.
CREATE UNLOGGED TABLE datagit_v_products (
    version_id    bigserial    PRIMARY KEY,
    branch_id     uuid         NOT NULL,
    seq_from      bigint       NOT NULL,
    seq_to        bigint       NOT NULL DEFAULT 9223372036854775807,
    op            smallint     NOT NULL,          -- 1=insert 2=update 3=delete
    commit_id     bytea        NOT NULL,
    session_id    uuid,
    changed_cols  bytea        NOT NULL,

    sku           text         NOT NULL,          -- mirrored PK column
    name          text,
    category      text,
    price         numeric(12,2),
    updated_at    timestamptz
);

-- Deterministic branch ids: main = ...0000, b1..b7 = ...0001 .. ...0007
CREATE OR REPLACE FUNCTION bid(n int) RETURNS uuid LANGUAGE sql IMMUTABLE AS
$$ SELECT ('00000000-0000-0000-0000-' || lpad(n::text, 12, '0'))::uuid $$;
