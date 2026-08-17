-- +goose Up

-- The alliance we observe. member_count is the "96/100" read off the alliance
-- screen, which is the reconciliation ground truth for the roster route.
CREATE TABLE alliances (
  id           bigserial PRIMARY KEY,
  tag          text NOT NULL,
  name         text NOT NULL,
  server       text,
  member_count int,
  observed_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tag, name)
);

-- Members are a dimension, not a fact: they are mutable and soft-deleted.
-- Only the roster route writes this table. A VS row that matches nothing goes
-- to review rather than creating a member, because one OCR misread would
-- otherwise mint a phantom that then accumulates facts and corrupts the very
-- count meant to catch it.
CREATE TABLE members (
  id              bigserial PRIMARY KEY,
  alliance_id     bigint NOT NULL REFERENCES alliances(id),
  name            text NOT NULL,
  name_normalized text NOT NULL,
  rank            text,
  first_seen_at   timestamptz NOT NULL DEFAULT now(),
  left_at         timestamptz,
  active          boolean NOT NULL DEFAULT true
);
CREATE INDEX members_alliance_active_idx ON members (alliance_id, active);
CREATE INDEX members_normalized_idx ON members (name_normalized);

-- Every human confirmation writes one of these, which is the mechanism that
-- makes matching accuracy compound rather than needing to be re-tuned.
CREATE TABLE member_aliases (
  id               bigserial PRIMARY KEY,
  member_id        bigint NOT NULL REFERENCES members(id),
  alias            text NOT NULL,
  alias_normalized text NOT NULL,
  source           text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (member_id, alias_normalized)
);

CREATE TABLE captures (
  id            bigserial PRIMARY KEY,
  account_id    bigint NOT NULL REFERENCES accounts(id),
  route         text NOT NULL,
  started_at    timestamptz NOT NULL DEFAULT now(),
  ended_at      timestamptz,
  status        text NOT NULL DEFAULT 'running',
  expected_rows int,
  parsed_rows   int,
  error         text,
  CONSTRAINT captures_status_check
    CHECK (status IN ('running','complete','partial','failed'))
);
CREATE INDEX captures_route_started_idx ON captures (route, started_at DESC);

-- offset_px is stored rather than recomputed at ingest time on purpose: a
-- later change to vision.ScrollOffset would otherwise silently re-segment
-- historical captures into different rows and make old facts unreproducible.
-- group_key carries the rank group, because on the roster route the rank
-- belongs to the frame's sticky header rather than to any row.
CREATE TABLE capture_frames (
  id            bigserial PRIMARY KEY,
  capture_id    bigint NOT NULL REFERENCES captures(id) ON DELETE CASCADE,
  seq           int NOT NULL,
  screenshot_id bigint NOT NULL REFERENCES screenshots(id),
  offset_px     int NOT NULL DEFAULT 0,
  group_key     text,
  UNIQUE (capture_id, seq)
);

CREATE TABLE participation_facts (
  id            bigserial PRIMARY KEY,
  member_id     bigint NOT NULL REFERENCES members(id),
  metric        text NOT NULL,
  value         numeric NOT NULL,
  observed_at   timestamptz NOT NULL,
  period_key    text NOT NULL,
  source        text NOT NULL,
  screenshot_id bigint REFERENCES screenshots(id),
  confidence    real NOT NULL,
  superseded_by bigint REFERENCES participation_facts(id),
  UNIQUE (member_id, metric, period_key, source, observed_at)
);
CREATE INDEX facts_member_metric_idx ON participation_facts (member_id, metric, period_key);
CREATE INDEX facts_live_idx ON participation_facts (metric, period_key) WHERE superseded_by IS NULL;

CREATE TABLE review_queue (
  id              bigserial PRIMARY KEY,
  capture_id      bigint REFERENCES captures(id) ON DELETE CASCADE,
  screenshot_id   bigint NOT NULL REFERENCES screenshots(id),
  row_y0          int NOT NULL,
  row_y1          int NOT NULL,
  raw_text        text NOT NULL,
  candidates_json jsonb NOT NULL DEFAULT '[]'::jsonb,
  reason          text NOT NULL,
  status          text NOT NULL DEFAULT 'pending',
  resolved_by     text,
  resolved_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT review_status_check
    CHECK (status IN ('pending','resolved','rejected'))
);
CREATE INDEX review_pending_idx ON review_queue (status, created_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE review_queue;
DROP TABLE participation_facts;
DROP TABLE capture_frames;
DROP TABLE captures;
DROP TABLE member_aliases;
DROP TABLE members;
DROP TABLE alliances;
