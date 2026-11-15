-- Version 3: MAIL parameters as the sender declared them, so that delivery
-- can pass them on (store-and-forward re-issues MAIL and RCPT itself).

ALTER TABLE mails ADD COLUMN body TEXT CHECK (body IN ('7BIT', '8BITMIME'));          -- BODY=, NULL if not given
ALTER TABLE mails ADD COLUMN smtputf8 INTEGER NOT NULL DEFAULT 0 CHECK (smtputf8 IN (0, 1)); -- SMTPUTF8 given
ALTER TABLE mails ADD COLUMN declared_size INTEGER;                                    -- SIZE=, NULL if not given
