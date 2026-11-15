-- Version 5: reads for the UI and the query API (U10, U11, U13).
--
-- Lookups by the sender's reference and by Message-ID, and the actor of
-- events that people cause through the UI (the token name; never an
-- address or a query).

CREATE INDEX mails_ref ON mails (ref) WHERE ref IS NOT NULL;
CREATE INDEX mails_message_id ON mails (message_id) WHERE message_id IS NOT NULL;

ALTER TABLE events ADD COLUMN actor TEXT;
