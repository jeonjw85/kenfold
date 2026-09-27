-- Phase 2b: model-based extraction of memories from session summaries.

-- +goose Up

-- One row per episodic memory (session summary) the extractor has claimed.
-- Summaries without a row are pending. Extracted memories link back to their
-- source with a memory_edge (relation 'derived_from').
CREATE TABLE extraction (
    source_id       uuid PRIMARY KEY REFERENCES memory (id) ON DELETE CASCADE,
    status          text NOT NULL CHECK (status IN ('running', 'done', 'failed')),
    attempts        int  NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    model           text,
    candidates      int  NOT NULL DEFAULT 0,  -- proposals that passed validation
    stored          int  NOT NULL DEFAULT 0,  -- memories written
    error           text,
    -- running: lease expiry (a crashed worker's claim becomes available again)
    locked_until    timestamptz,
    -- failed: when to retry (NULL = do not retry)
    next_attempt_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX extraction_retry_idx ON extraction (next_attempt_at) WHERE status = 'failed';

CREATE TRIGGER extraction_touch_updated_at
    BEFORE UPDATE ON extraction
    FOR EACH ROW EXECUTE FUNCTION kenfold_touch_updated_at();

-- Pending summaries are found by type and status.
CREATE INDEX memory_episodic_active_idx ON memory (created_at) WHERE type = 'episodic' AND status = 'active';

-- +goose Down
DROP INDEX memory_episodic_active_idx;
DROP TABLE extraction;
