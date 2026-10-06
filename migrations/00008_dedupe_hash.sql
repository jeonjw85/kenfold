-- Bound deduplication index entries so every valid memory fits, including
-- incompressible content near the 8000-character tool limit. Lookups still
-- compare the full content_key to resolve hash collisions.

-- +goose Up
DROP INDEX memory_dedupe_idx;
CREATE INDEX memory_dedupe_idx ON memory (scope, type, md5(content_key))
    WHERE status IN ('active', 'proposed');

-- +goose Down
-- The old schema cannot index long active/proposed memories. PostgreSQL
-- rejects the downgrade atomically if such rows exist; no content is truncated.
DROP INDEX memory_dedupe_idx;
CREATE INDEX memory_dedupe_idx ON memory (scope, type, content_key)
    WHERE status IN ('active', 'proposed');
