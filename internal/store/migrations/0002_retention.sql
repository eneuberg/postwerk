-- Version 2: retention (S6–S11).
--
-- mails.raw now holds a sealed blob (see internal/seal); size stays the
-- length of the plaintext message.

ALTER TABLE mails ADD COLUMN raw_purged_at INTEGER;     -- set when raw is removed
ALTER TABLE mails ADD COLUMN pseudonymized_at INTEGER;  -- set when addresses are replaced by keyed hashes

CREATE INDEX mails_received ON mails (received_at);
CREATE INDEX mails_raw_kept ON mails (received_at) WHERE raw IS NOT NULL;
CREATE INDEX mails_plain ON mails (received_at) WHERE pseudonymized_at IS NULL;
CREATE INDEX events_time ON events (time);

-- Events stay append-only, except for retention. The single row below holds
-- a cutoff time that can only grow; only events older than the cutoff can be
-- deleted. Updates of events stay forbidden (events_no_update from version 1).
CREATE TABLE event_retention (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    cutoff INTEGER NOT NULL                  -- Unix ms; events with time < cutoff may be deleted
) STRICT;

INSERT INTO event_retention (id, cutoff) VALUES (1, 0);

CREATE TRIGGER event_retention_no_insert BEFORE INSERT ON event_retention
BEGIN
    SELECT RAISE(ABORT, 'event retention cutoff: single row');
END;

CREATE TRIGGER event_retention_no_delete BEFORE DELETE ON event_retention
BEGIN
    SELECT RAISE(ABORT, 'event retention cutoff: single row');
END;

CREATE TRIGGER event_retention_monotonic BEFORE UPDATE ON event_retention
WHEN NEW.id IS NOT OLD.id OR NEW.cutoff IS NULL OR NEW.cutoff < OLD.cutoff
BEGIN
    SELECT RAISE(ABORT, 'event retention cutoff cannot go backwards');
END;

DROP TRIGGER events_no_delete;

-- A missing cutoff row counts as 0, so nothing can be deleted.
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
WHEN OLD.time >= COALESCE((SELECT cutoff FROM event_retention WHERE id = 1), 0)
BEGIN
    SELECT RAISE(ABORT, 'events are append-only (only events older than the retention cutoff can be deleted)');
END;
