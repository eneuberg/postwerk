-- Version 7: an admin can close a delivery (D53).
--
-- resolution is set when an admin closed the recipient instead of the relay
-- reaching a result: 'confirmed' (checked at the target, it has the mail;
-- the status becomes handed_off) or 'given_up' (the status becomes failed).
-- NULL for every result the relay reached itself. Who closed it, when and
-- why is in the delivery.closed event. The derived summaries do not change:
-- they read the status only.

ALTER TABLE delivery_recipients ADD COLUMN resolution TEXT
    CHECK (resolution IS NULL
        OR (resolution = 'confirmed' AND status = 'handed_off')
        OR (resolution = 'given_up' AND status = 'failed'));
