-- ============================================================
-- Default free trial: 5 days
-- Idempotent: safe to re-run (deploy-update.sh applies migrations/*.sql once each)
-- ============================================================

-- The team's choice: a brand-new customer gets a 5-day free trial unless staff say otherwise.
-- Only the untouched factory value (0) is replaced, so a number the team already set on the
-- Settings page is never overwritten. Grace days are NOT given by default: billing_grace_days
-- stays 0, and only staff give a customer grace days (customer dialog).
UPDATE system_settings SET value = '5' WHERE key = 'trial_days' AND value = '0';
