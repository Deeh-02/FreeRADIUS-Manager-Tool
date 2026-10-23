-- ============================================================
-- KES as the default currency; grace days per customer
-- Idempotent: safe to re-run (deploy-update.sh applies migrations/*.sql once each)
-- ============================================================

-- 1. Currency. Existing rows keep the currency they were created with; only the defaults for
--    new rows change. The default_currency setting is switched only if it is still the
--    factory value, so a currency chosen on the Settings page is never overwritten.
ALTER TABLE user_plans ALTER COLUMN currency SET DEFAULT 'KES';
ALTER TABLE invoices   ALTER COLUMN currency SET DEFAULT 'KES';
ALTER TABLE payments   ALTER COLUMN currency SET DEFAULT 'KES';
ALTER TABLE vouchers   ALTER COLUMN currency SET DEFAULT 'KES';
UPDATE system_settings SET value = 'KES' WHERE key = 'default_currency' AND value = 'USD';

-- 2. Grace days per customer. NULL = use the system default (billing_grace_days). Staff can
--    give one customer extra days after their paid-until date when they ask for it; the
--    change is made on the customer's edit dialog and is recorded in the audit log.
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS grace_days INTEGER;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'radius_users_grace_days_check') THEN
        ALTER TABLE radius_users ADD CONSTRAINT radius_users_grace_days_check
            CHECK (grace_days IS NULL OR grace_days BETWEEN 0 AND 60);
    END IF;
END $$;
