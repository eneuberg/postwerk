-- Version 6: UI accounts managed in the admin panel (U17–U24).
--
-- Every account is an admin of the UI. The hash is a PBKDF2 value as in the
-- senders file (pbkdf2-sha256$…), never the password. created_by and
-- updated_by name the account that made the change.

CREATE TABLE ui_accounts (
    name       TEXT    PRIMARY KEY,
    hash       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    created_by TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT    NOT NULL
) STRICT;
