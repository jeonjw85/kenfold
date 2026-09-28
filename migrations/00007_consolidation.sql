-- Phase 5: consolidation proposals.
--
-- A background worker asks the chat model about memories that may say the
-- same thing (duplicate), contradict each other (conflict), or are old
-- session summaries (digest), and records what it proposes. Nothing changes
-- until the owner applies a proposal; applying retires memories (status
-- 'superseded', kept as history) and links the memory that replaces them.
--
-- member_ids is sorted and unique, so a set of memories is judged once: a
-- proposal the owner rejected, or a pair the model found distinct
-- ('dismissed'), is not made again.

-- +goose Up
CREATE TABLE consolidation (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    kind        text NOT NULL CHECK (kind IN ('duplicate', 'conflict', 'digest', 'distinct')),
    scope       text NOT NULL CHECK (scope <> ''),
    member_ids  uuid[] NOT NULL CHECK (cardinality(member_ids) >= 2),
    -- duplicate, conflict: the member that stays active.
    keep_id     uuid,
    -- digest: the summary that replaces the members.
    content     text NOT NULL DEFAULT '',
    reason      text NOT NULL DEFAULT '',
    model       text NOT NULL CHECK (model <> ''),
    status      text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'applied', 'rejected', 'dismissed', 'stale')),
    -- The memory that replaced the members (applied proposals).
    result_id   uuid REFERENCES memory (id) ON DELETE SET NULL,
    decided_by  text,
    decided_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT consolidation_keep CHECK ((kind IN ('duplicate', 'conflict')) = (keep_id IS NOT NULL)),
    CONSTRAINT consolidation_keep_member CHECK (keep_id IS NULL OR keep_id = ANY (member_ids)),
    CONSTRAINT consolidation_digest_content CHECK ((kind = 'digest') = (content <> '')),
    CONSTRAINT consolidation_distinct_dismissed CHECK (kind <> 'distinct' OR status = 'dismissed')
);

CREATE UNIQUE INDEX consolidation_members_idx ON consolidation (member_ids);
CREATE INDEX consolidation_pending_idx ON consolidation (created_at) WHERE status = 'pending';
CREATE INDEX consolidation_member_gin_idx ON consolidation USING gin (member_ids);

CREATE TRIGGER consolidation_touch_updated_at
    BEFORE UPDATE ON consolidation
    FOR EACH ROW EXECUTE FUNCTION kenfold_touch_updated_at();

-- +goose Down
DROP TABLE consolidation;
