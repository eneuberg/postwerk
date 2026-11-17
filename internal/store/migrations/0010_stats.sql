-- Version 10: statistics (D73–D75).
--
-- The statistics read mails by received time with their sender, and a few
-- event kinds by time. Both indexes cover their reads, so a range of a
-- month is read from the index alone, without the mail rows (which may
-- still hold a sealed message) or every event of the range.

CREATE INDEX mails_stats ON mails (received_at, sender, id);
CREATE INDEX events_kind_time ON events (kind, time);
