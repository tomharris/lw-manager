-- +goose Up

-- Finding 2 of the task-11 follow-up: a fact below the 0.80 blended-
-- confidence gate is never written, only queued for review, and the review
-- row carries that confidence so a human can see how bad the read was.
-- Nullable, not NOT NULL: most review reasons (an unparseable field, an
-- ambiguous name match, a full group) carry no such score at all, and NULL
-- says that plainly rather than a claimed zero-confidence read would.
ALTER TABLE review_queue ADD COLUMN confidence real;

-- +goose Down
ALTER TABLE review_queue DROP COLUMN confidence;
