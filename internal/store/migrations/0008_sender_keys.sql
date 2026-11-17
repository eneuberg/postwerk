-- Version 8: senders managed in the database (D62, D63).
--
-- A sender is one application; its policy says which envelope senders it may
-- use (allowed_from: comma-separated lower-case patterns, empty for any) and
-- how much it may send (0: no limit). A key is stored as the SHA-256 of its
-- secret part, never the key itself. Revoked keys stay, so a key id is never
-- reused and the audit trail keeps its name. Senders from the senders file
-- are not in these tables.

CREATE TABLE senders (
    name              TEXT    PRIMARY KEY,
    allowed_from      TEXT    NOT NULL DEFAULT '',
    max_per_minute    INTEGER NOT NULL DEFAULT 0 CHECK (max_per_minute >= 0),
    max_rcpt_per_hour INTEGER NOT NULL DEFAULT 0 CHECK (max_rcpt_per_hour >= 0),
    created_at        INTEGER NOT NULL,
    created_by        TEXT    NOT NULL,
    updated_at        INTEGER NOT NULL,
    updated_by        TEXT    NOT NULL
) STRICT;

CREATE TABLE sender_keys (
    id         TEXT    PRIMARY KEY,
    sender     TEXT    NOT NULL REFERENCES senders (name),
    digest     BLOB    NOT NULL CHECK (length(digest) = 32),
    created_at INTEGER NOT NULL,
    created_by TEXT    NOT NULL,
    expires_at INTEGER,
    revoked_at INTEGER,
    revoked_by TEXT
) STRICT;

CREATE INDEX sender_keys_sender ON sender_keys (sender);

-- When each credential last logged in, for both sources; written in batches
-- (internal/senders.Usage). key_id is empty for a password line.
CREATE TABLE sender_usage (
    source       TEXT    NOT NULL CHECK (source IN ('file', 'db')),
    sender       TEXT    NOT NULL,
    key_id       TEXT    NOT NULL,
    last_used_at INTEGER NOT NULL,
    PRIMARY KEY (source, sender, key_id)
) STRICT;
