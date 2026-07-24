-- +goose Up

-- Global kill switch. The boolean-primary-key trick makes a second row
-- impossible: "the" pause flag is enforced by the schema, not by convention.
CREATE TABLE flags (
    id         boolean     PRIMARY KEY DEFAULT true CHECK (id),
    pause_all  boolean     NOT NULL DEFAULT false,
    reason     text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO flags DEFAULT VALUES;

-- One row per task execution. The row is inserted as 'running' before the
-- task starts, so a killed process leaves a visibly stale running row rather
-- than nothing (invariant #2's audit trail).
CREATE TABLE task_runs (
    id             bigserial   PRIMARY KEY,
    account_id     bigint      NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    task_name      text        NOT NULL,
    started_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz,
    status         text        NOT NULL DEFAULT 'running',
    error          text,
    screenshot_ids bigint[]    NOT NULL DEFAULT '{}',

    CONSTRAINT task_runs_status_valid
        CHECK (status IN ('running', 'succeeded', 'failed', 'paused'))
);
CREATE INDEX task_runs_account_started_idx ON task_runs (account_id, started_at DESC);

-- +goose Down
DROP TABLE task_runs;
DROP TABLE flags;
