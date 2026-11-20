-- content_key is also computed in Go (store.ContentKey), so both sides must
-- normalize the same way. The previous expression trimmed only U+0020 before
-- collapsing whitespace: a leading or trailing tab, newline, or non-ASCII
-- space survived as a space after the collapse, so "\tWe use pnpm" and
-- "We use pnpm" got different keys. Paths that compare the Go key and paths
-- that look the key up in SQL then disagreed about whether two memories say
-- the same thing.
--
-- Collapse every Unicode space to one ASCII space first, trim it, then remove
-- trailing '.', '!' or '。' together with any space left in front of them, so
-- "We use pnpm ." and "We use pnpm" also agree.

-- +goose Up
ALTER TABLE memory ALTER COLUMN content_key SET EXPRESSION AS (
    lower(regexp_replace(
        btrim(regexp_replace(content, '[\u0009\u000A\u000B\u000C\u000D\u0020\u0085\u00A0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A\u2028\u2029\u202F\u205F\u3000]+', ' ', 'g'), ' '),
        '[.!。 ]+$', ''))
);

-- +goose Down
ALTER TABLE memory ALTER COLUMN content_key SET EXPRESSION AS (
    lower(regexp_replace(regexp_replace(btrim(content), '\s+', ' ', 'g'), '[.!。]+$', ''))
);
