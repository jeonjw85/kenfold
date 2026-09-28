-- Phase 4: built-in OAuth 2.1 authorization server for remote clients
-- (ChatGPT, claude.ai, any MCP client that speaks OAuth).
--
-- Kenfold has one owner. The owner approves each client on a consent page
-- (authenticated with the owner password) and chooses the access: read-only
-- or read and write. Every token, code, and secret is stored as a SHA-256
-- hash; nothing here can be used to authenticate if the table leaks.

-- +goose Up

-- The owner credential (a single row). Argon2id parameters are stored with the hash.
CREATE TABLE oauth_owner (
    id              int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    password_hash   text NOT NULL CHECK (password_hash <> ''),
    failed_attempts int  NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    locked_until    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER oauth_owner_touch_updated_at
    BEFORE UPDATE ON oauth_owner
    FOR EACH ROW EXECUTE FUNCTION kenfold_touch_updated_at();

-- Clients. client_id is either a URL (Client ID Metadata Document, the
-- metadata is fetched from it) or an opaque id issued by dynamic
-- registration (the metadata is what the client registered).
CREATE TABLE oauth_client (
    client_id     text PRIMARY KEY CHECK (client_id <> '' AND length(client_id) <= 2048),
    kind          text NOT NULL CHECK (kind IN ('cimd', 'dcr')),
    client_name   text NOT NULL DEFAULT '',
    redirect_uris text[] NOT NULL CHECK (cardinality(redirect_uris) BETWEEN 1 AND 20),
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- CIMD documents are re-fetched once this passes.
    fetched_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- A grant is the owner's approval of one client, with an access level and an
-- agent name that becomes memory.source_agent for its writes. Revoking a
-- grant invalidates every token issued under it.
CREATE TABLE oauth_grant (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    client_id    text NOT NULL REFERENCES oauth_client (client_id) ON DELETE CASCADE,
    agent        text NOT NULL CHECK (agent ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
    scopes       text[] NOT NULL CHECK (cardinality(scopes) >= 1),
    resource     text NOT NULL CHECK (resource <> ''),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX oauth_grant_client_idx ON oauth_grant (client_id);

-- Authorization codes: single use, short-lived, PKCE-bound.
CREATE TABLE oauth_code (
    code_hash      bytea PRIMARY KEY CHECK (length(code_hash) = 32),
    grant_id       uuid NOT NULL REFERENCES oauth_grant (id) ON DELETE CASCADE,
    redirect_uri   text NOT NULL,
    code_challenge text NOT NULL CHECK (length(code_challenge) = 43),
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Access and refresh tokens. A refresh token is replaced on every use (its
-- successor records it in parent); presenting a replaced refresh token again
-- revokes the whole grant (reuse detection).
CREATE TABLE oauth_token (
    token_hash bytea PRIMARY KEY CHECK (length(token_hash) = 32),
    kind       text NOT NULL CHECK (kind IN ('access', 'refresh')),
    grant_id   uuid NOT NULL REFERENCES oauth_grant (id) ON DELETE CASCADE,
    scopes     text[] NOT NULL CHECK (cardinality(scopes) >= 1),
    parent     bytea,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX oauth_token_grant_idx ON oauth_token (grant_id);
CREATE INDEX oauth_token_expires_idx ON oauth_token (expires_at);

-- +goose Down
DROP TABLE oauth_token;
DROP TABLE oauth_code;
DROP TABLE oauth_grant;
DROP TABLE oauth_client;
DROP TABLE oauth_owner;
