-- ============================================================
-- Free trial days for brand-new customers
-- Idempotent: safe to re-run (deploy-update.sh applies migrations/*.sql once each)
-- ============================================================

-- trial_started: the day a customer's one free trial began. A customer gets at most one trial,
-- so assigning plans again cannot restart it.
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS trial_started DATE;

-- trial_days: how long the trial is, set by the team on the Settings page. 0 = no trial.
INSERT INTO system_settings (key, value, description) VALUES
    ('trial_days', '0', 'Free days a brand-new customer gets before their first payment (0 = none; one trial per customer)')
ON CONFLICT (key) DO NOTHING;
