-- Version 9: where each mail came from (D65).
--
-- key_id and key_source name the credential the sender logged in with
-- (key_id NULL for a password line); client_ip is the address of the
-- connection (a pod or host of the sending application, without the port);
-- tls_version and tls_cipher are empty without TLS; ehlo is the name the
-- client gave (redacted, at most 255 bytes); auth_mech the SASL mechanism.
-- All of it is removed with the mail record (record retention). Rows written
-- before this version have NULL.

ALTER TABLE mails ADD COLUMN key_id TEXT;
ALTER TABLE mails ADD COLUMN key_source TEXT CHECK (key_source IS NULL OR key_source IN ('file', 'db'));
ALTER TABLE mails ADD COLUMN client_ip TEXT;
ALTER TABLE mails ADD COLUMN tls_version TEXT;
ALTER TABLE mails ADD COLUMN tls_cipher TEXT;
ALTER TABLE mails ADD COLUMN ehlo TEXT;
ALTER TABLE mails ADD COLUMN auth_mech TEXT;

CREATE INDEX mails_sender ON mails (sender, received_at);
CREATE INDEX mails_key ON mails (key_id) WHERE key_id IS NOT NULL;
