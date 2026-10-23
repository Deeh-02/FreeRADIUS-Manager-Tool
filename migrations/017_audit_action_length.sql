-- audit_log.action holds "METHOD /api/v1/<route pattern>". One route
-- (POST /api/v1/network/scans/hosts/:hostId/import-nas, 51 characters) did not fit
-- varchar(50), so its audit rows were silently lost. Found by the route fuzzer.
ALTER TABLE audit_log ALTER COLUMN action TYPE VARCHAR(150);
