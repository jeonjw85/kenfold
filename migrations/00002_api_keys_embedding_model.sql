-- Phase 1: API keys (agent identity for HTTP clients) and embedding provenance.

-- +goose Up

-- One key per agent install. Only a SHA-256 hash of the key is stored; the
-- plaintext is shown once at creation. `agent` becomes memory.source_agent for
-- every write made with the key, so clients cannot claim another agent's name.
CREATE TABLE api_key (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    agent        text NOT NULL CHECK (agent ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    -- Leading characters of the key, shown in listings to identify it.
    prefix       text NOT NULL CHECK (prefix <> ''),
    key_hash     bytea NOT NULL UNIQUE CHECK (length(key_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX api_key_agent_idx ON api_key (agent);

-- Which model produced memory.embedding. Vectors from different models are not
-- comparable, so search only uses rows whose model matches the configured one,
-- and reindexing re-embeds rows with a missing or different model.
ALTER TABLE memory ADD COLUMN embedding_model text;

-- +goose Down
ALTER TABLE memory DROP COLUMN embedding_model;
DROP TABLE api_key;
