-- 015: the data-cap period starts at the moment of payment, not at midnight.
--
-- The cap counter (freeradius/mods-available/quota, the quota and alert tasks) counted the
-- sessions whose acctstarttime fell on or after radius_users.period_start, a DATE. A customer
-- who used up the cap and paid for a new period the same day stayed refused until midnight
-- (found by the real FreeRADIUS test TestRealRadiusDataCap).
--
-- A timestamp would not do either: acctstarttime comes from the router's clock (Event-
-- Timestamp) in whole seconds, so a session started just after the payment on a router whose
-- clock is behind would look older than the period and never count. Instead the period
-- remembers the last radacct row that existed when it started; the database numbers rows in
-- the order they arrive, whatever the routers' clocks say.
--
-- Existing customers: the floor is the newest row of theirs that started before their current
-- period_start, which keeps today's counts unchanged. Safe to run more than once.

ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS period_acct_floor BIGINT;

UPDATE radius_users ru
SET period_acct_floor = COALESCE((
        SELECT MAX(ra.radacctid) FROM radacct ra
        WHERE ra.username = ru.username AND ra.acctstarttime < ru.period_start), 0)
WHERE ru.period_start IS NOT NULL AND ru.period_acct_floor IS NULL;
