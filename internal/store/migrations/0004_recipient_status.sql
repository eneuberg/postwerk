-- Version 4: one status per (mail, target, recipient).
--
-- The recipient of a delivery is the unit of tracking: a target answers
-- every RCPT on its own, and providers such as Mailjet keep one message per
-- recipient. delivery_recipients.status is the single source of truth; the
-- status of a delivery or a mail is derived when reading and never stored.
-- deliveries keeps what one SMTP transaction shares: attempts, the next
-- attempt (set while a recipient is queued) and data_sent_at.

CREATE TABLE delivery_recipients (
    mail_id    TEXT    NOT NULL,
    target     TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    status     TEXT    NOT NULL
        CHECK (status IN ('queued', 'sending', 'handed_off', 'failed', 'unknown', 'skipped')),
    last_code  INTEGER,
    last_text  TEXT,                         -- redacted
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (mail_id, target, position),
    FOREIGN KEY (mail_id, target) REFERENCES deliveries (mail_id, target) ON DELETE CASCADE,
    FOREIGN KEY (mail_id, position) REFERENCES recipients (mail_id, position) ON DELETE CASCADE
) STRICT;

CREATE INDEX delivery_recipients_status ON delivery_recipients (status, mail_id);
CREATE INDEX delivery_recipients_mail ON delivery_recipients (mail_id, status);

-- Every recipient takes the status its delivery had.
INSERT INTO delivery_recipients (mail_id, target, position, status, last_code, last_text, updated_at)
SELECT d.mail_id, d.target, r.position, d.status, d.last_code, d.last_text, d.updated_at
FROM deliveries d JOIN recipients r ON r.mail_id = d.mail_id;

DROP INDEX deliveries_due;
ALTER TABLE deliveries DROP COLUMN status;
-- Provider status will be kept per recipient (later phase).
ALTER TABLE deliveries DROP COLUMN provider_id;
ALTER TABLE deliveries DROP COLUMN provider_status;
CREATE INDEX deliveries_due ON deliveries (target, next_attempt_at) WHERE next_attempt_at IS NOT NULL;

-- Events about one recipient name its position, never its address.
ALTER TABLE events ADD COLUMN recipient INTEGER;
