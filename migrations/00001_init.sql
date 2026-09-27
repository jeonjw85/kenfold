-- Kenfold initial schema. Requires PostgreSQL 18+ (uuidv7) and pgvector 0.8+.
-- Enum-like CHECK constraints mirror internal/memory/types.go; keep both in sync.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE memory (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    type           text NOT NULL
                   CHECK (type IN ('semantic', 'episodic', 'project', 'preference', 'codebase', 'temporary')),
    -- 'user' | 'project:<id>' | 'repo:<id>' (see docs/adr/0001-architecture.md)
    scope          text NOT NULL CHECK (scope <> ''),
    content        text NOT NULL CHECK (content <> ''),
    -- Dimension is fixed per install (default model: bge-m3, 1024).
    -- Changing the embedding model requires a migration and re-embedding.
    embedding      vector(1024),
    attrs          jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Provenance: who wrote this, in which session, based on what.
    source_agent   text NOT NULL CHECK (source_agent <> ''),
    source_session text,
    evidence_uri   text,
    trust          text NOT NULL DEFAULT 'agent' CHECK (trust IN ('user', 'agent', 'external')),
    confidence     real NOT NULL DEFAULT 0.5 CHECK (confidence >= 0 AND confidence <= 1),

    -- Lifecycle. Conflicts are resolved by superseding, never by overwriting.
    status         text NOT NULL DEFAULT 'active'
                   CHECK (status IN ('proposed', 'active', 'superseded', 'deleted')),
    supersedes     uuid REFERENCES memory (id),

    -- valid_* = when the fact was true in the world; expires_at = TTL for temporary memories.
    valid_from     timestamptz,
    valid_to       timestamptz,
    expires_at     timestamptz,

    -- 'simple' config: no stemming, works for mixed Korean/English as a baseline.
    content_tsv    tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT memory_valid_range CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to >= valid_from),
    CONSTRAINT memory_temporary_has_ttl CHECK (type <> 'temporary' OR expires_at IS NOT NULL),
    CONSTRAINT memory_no_self_supersede CHECK (supersedes IS NULL OR supersedes <> id)
);

-- Primary read path: active memories of given types within a scope, newest first.
CREATE INDEX memory_scope_type_active_idx ON memory (scope, type, created_at DESC) WHERE status = 'active';
CREATE INDEX memory_embedding_hnsw_idx    ON memory USING hnsw (embedding vector_cosine_ops);
CREATE INDEX memory_content_tsv_idx       ON memory USING gin (content_tsv);
CREATE INDEX memory_expires_at_idx        ON memory (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX memory_supersedes_idx        ON memory (supersedes) WHERE supersedes IS NOT NULL;
CREATE INDEX memory_source_session_idx    ON memory (source_session) WHERE source_session IS NOT NULL;

-- Graph layer: typed, directed edges between memories (e.g. 'derived_from', 'relates_to', 'mentions').
CREATE TABLE memory_edge (
    src          uuid NOT NULL REFERENCES memory (id) ON DELETE CASCADE,
    dst          uuid NOT NULL REFERENCES memory (id) ON DELETE CASCADE,
    relation     text NOT NULL CHECK (relation <> ''),
    weight       real NOT NULL DEFAULT 1.0,
    source_agent text NOT NULL CHECK (source_agent <> ''),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src, relation, dst),
    CONSTRAINT memory_edge_no_self_loop CHECK (src <> dst)
);

-- Reverse traversal (dst -> src); forward traversal uses the primary key.
CREATE INDEX memory_edge_dst_idx ON memory_edge (dst, relation);

-- +goose StatementBegin
CREATE FUNCTION kenfold_touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER memory_touch_updated_at
    BEFORE UPDATE ON memory
    FOR EACH ROW EXECUTE FUNCTION kenfold_touch_updated_at();

-- +goose Down
DROP TABLE memory_edge;
DROP TABLE memory;
DROP FUNCTION kenfold_touch_updated_at();
-- The vector extension is intentionally left installed; other schemas may use it.
