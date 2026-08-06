-- central-config: indexes 003 did not need and the delete/reconcile paths do.
--
-- Deleting an environment or a microservice is refused while localization rows
-- still point at it, and that check has no index to lean on: 003's
-- IX_LOC_SERVICE_ENV_LOCALE leads with MICROSERVICE_ID, so an ENVIRONMENT_ID
-- probe scans the table.

CREATE INDEX IX_LOC_ENVIRONMENT ON CONFIG_LOCALIZATION (ENVIRONMENT_ID);

-- The reconciler's incremental window (WHERE UPDATED_AT >= $1). See the
-- TIMESTAMPTZ note in 001 for why that bind no longer needs converting.
CREATE INDEX IX_LOC_UPDATED_AT ON CONFIG_LOCALIZATION (UPDATED_AT);
