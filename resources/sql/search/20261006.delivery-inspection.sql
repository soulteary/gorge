-- Apply once in existing search control databases before restarting the service.
-- Historical ages are unknown: zero must not be backfilled with migration time.
ALTER TABLE search_projection_delivery ADD COLUMN createdEpoch BIGINT NOT NULL DEFAULT 0;
