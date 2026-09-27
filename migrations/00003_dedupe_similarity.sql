-- Phase 2: normalized duplicate detection and lexical similarity hints.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- content_key identifies memories that differ only in case, whitespace, or
-- trailing punctuation, so "We use pgx v5." and "we use  pgx v5" are one memory.
ALTER TABLE memory ADD COLUMN content_key text
    GENERATED ALWAYS AS (
        lower(regexp_replace(regexp_replace(btrim(content), '\s+', ' ', 'g'), '[.!。]+$', ''))
    ) STORED;

CREATE INDEX memory_dedupe_idx ON memory (scope, type, content_key)
    WHERE status IN ('active', 'proposed');

-- Trigram similarity for "similar memories" hints on remember.
CREATE INDEX memory_content_trgm_idx ON memory USING gin (content gin_trgm_ops)
    WHERE status = 'active';

-- Session summaries written by hooks are looked up by source_session.
-- (memory_source_session_idx from 00001 already covers this.)

-- +goose Down
DROP INDEX memory_content_trgm_idx;
DROP INDEX memory_dedupe_idx;
ALTER TABLE memory DROP COLUMN content_key;
-- pg_trgm is intentionally left installed; other schemas may use it.
