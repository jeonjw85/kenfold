-- Phase 3: code references and commit-based invalidation.

-- +goose Up

-- Files and symbols a memory mentions ("RequireAPIKey in internal/auth/middleware.go").
-- The server extracts them when the memory is written; it cannot read the
-- repository, so a client that can (the session hook, `kenfold refs sync`)
-- anchors each reference to a content hash at a commit and later reports
-- whether that content changed. States:
--   pending     not yet anchored
--   current     the anchored code is unchanged at checked_commit
--   changed     the anchored code differs at checked_commit
--   missing     the file or symbol no longer exists at checked_commit
--   unresolved  never found in the repository (a typo, generated code, another repo)
CREATE TABLE memory_ref (
    memory_id      uuid NOT NULL REFERENCES memory (id) ON DELETE CASCADE,
    -- Denormalized from memory.scope: clients sync one project at a time.
    scope          text NOT NULL CHECK (scope LIKE 'project:%' OR scope LIKE 'repo:%'),
    -- Repository-relative path; '' for a symbol mentioned without a file.
    path           text NOT NULL DEFAULT '',
    -- Identifier, e.g. RequireAPIKey or Server.Dedupe; '' for a whole file.
    symbol         text NOT NULL DEFAULT '',
    state          text NOT NULL DEFAULT 'pending'
                   CHECK (state IN ('pending', 'current', 'changed', 'missing', 'unresolved')),
    -- Where a path-less symbol was found.
    resolved_path  text,
    anchor_hash    text,
    anchor_commit  text,
    checked_commit text,
    checked_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, path, symbol),
    CONSTRAINT memory_ref_target CHECK (path <> '' OR symbol <> ''),
    CONSTRAINT memory_ref_anchor CHECK ((anchor_hash IS NULL) = (anchor_commit IS NULL))
);

CREATE INDEX memory_ref_target_idx ON memory_ref (scope, path, symbol);

-- +goose Down
DROP TABLE memory_ref;
