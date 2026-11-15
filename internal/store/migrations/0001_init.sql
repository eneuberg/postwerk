-- Version 1: mails, recipients, deliveries, append-only events, sink cursors.
-- Times are Unix milliseconds (UTC).

CREATE TABLE mails (
    id            TEXT    PRIMARY KEY,
    received_at   INTEGER NOT NULL,
    sender        TEXT    NOT NULL,          -- login name of the sending application
    envelope_from TEXT    NOT NULL,          -- MAIL FROM, empty for the null sender
    message_id    TEXT,
    ref           TEXT,                      -- X-Postwerk-Ref
    size          INTEGER NOT NULL,          -- bytes of raw
    raw           BLOB                       -- NULL once purged (later phase)
) STRICT;

CREATE TABLE recipients (
    mail_id  TEXT    NOT NULL REFERENCES mails (id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    address  TEXT    NOT NULL,
    PRIMARY KEY (mail_id, position)
) STRICT;

CREATE TABLE deliveries (
    mail_id         TEXT    NOT NULL REFERENCES mails (id) ON DELETE CASCADE,
    target          TEXT    NOT NULL,
    status          TEXT    NOT NULL
        CHECK (status IN ('queued', 'sending', 'handed_off', 'failed', 'unknown', 'skipped')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER,                 -- set while queued
    data_sent_at    INTEGER,                 -- set right before the message is sent (D5)
    last_code       INTEGER,
    last_text       TEXT,                    -- redacted
    provider_id     TEXT,                    -- later phase
    provider_status TEXT,                    -- later phase
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (mail_id, target)
) STRICT;

CREATE INDEX deliveries_due ON deliveries (target, status, next_attempt_at);

CREATE TABLE events (
    seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    time    INTEGER NOT NULL,
    mail_id TEXT,
    target  TEXT,
    kind    TEXT    NOT NULL,
    level   TEXT    NOT NULL CHECK (level IN ('info', 'warn', 'error')),
    attempt INTEGER,
    code    INTEGER,
    text    TEXT,                            -- redacted, never an address
    sender  TEXT                             -- login name of the sending application
) STRICT;

CREATE INDEX events_mail ON events (mail_id);

-- The event log is append-only (S3). The retention job (later phase) will
-- change this in its own migration.
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TABLE cursors (
    sink TEXT    PRIMARY KEY,
    seq  INTEGER NOT NULL
) STRICT;
