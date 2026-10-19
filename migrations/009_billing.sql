-- ============================================================
-- Billing: plans, invoices, payments, promotions, scheduler
-- Idempotent: safe to re-run (deploy-update.sh applies migrations/*.sql)
-- ============================================================

-- Money is NUMERIC(12,2) here, though the Go handlers read/write it as float64.
-- Revenue totals add amounts together without regard to currency.
-- invoices and payments copy username (and plan name) instead of relying only on the
-- foreign key, so history survives deleting a user or plan (FKs are ON DELETE SET NULL).
-- Nothing here enforces access on the network: FreeRADIUS reads radcheck/radreply only.
-- Only the plan's limits are copied there, and only when a plan is assigned via the API.
-- Statuses are free text except invoices.status, which has a CHECK.

CREATE TABLE IF NOT EXISTS user_plans (
    id                   SERIAL PRIMARY KEY,
    name                 VARCHAR(100)   UNIQUE NOT NULL,
    description          TEXT,
    price                NUMERIC(12,2)  NOT NULL DEFAULT 0 CHECK (price >= 0),
    currency             VARCHAR(10)    NOT NULL DEFAULT 'USD',
    data_limit_mb        BIGINT,
    validity_days        INTEGER        NOT NULL DEFAULT 30 CHECK (validity_days >= 0),
    bandwidth_profile_id INTEGER        REFERENCES bandwidth_profiles(id) ON DELETE SET NULL,
    max_devices          INTEGER        NOT NULL DEFAULT 1 CHECK (max_devices BETWEEN 1 AND 500),
    is_active            BOOLEAN        NOT NULL DEFAULT TRUE,
    created_at           TIMESTAMP      DEFAULT CURRENT_TIMESTAMP,
    updated_at           TIMESTAMP      DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS plan_id INTEGER REFERENCES user_plans(id) ON DELETE SET NULL;
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS phone   VARCHAR(30);
CREATE INDEX IF NOT EXISTS radius_users_plan_id ON radius_users (plan_id);

CREATE TABLE IF NOT EXISTS invoices (
    id             SERIAL PRIMARY KEY,
    invoice_number VARCHAR(40)    UNIQUE NOT NULL,
    user_id        INTEGER        REFERENCES radius_users(id) ON DELETE SET NULL,
    username       VARCHAR(64)    NOT NULL,
    plan_id        INTEGER        REFERENCES user_plans(id) ON DELETE SET NULL,
    plan_name      VARCHAR(100),
    amount         NUMERIC(12,2)  NOT NULL DEFAULT 0 CHECK (amount >= 0),
    currency       VARCHAR(10)    NOT NULL DEFAULT 'USD',
    status         VARCHAR(20)    NOT NULL DEFAULT 'pending'
                                  CHECK (status IN ('pending','paid','overdue','cancelled')),
    due_date       DATE,
    paid_at        TIMESTAMP,
    notes          TEXT,
    created_by     INTEGER        REFERENCES app_users(id) ON DELETE SET NULL,
    created_at     TIMESTAMP      DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP      DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS invoices_status   ON invoices (status);
CREATE INDEX IF NOT EXISTS invoices_username ON invoices (username);
CREATE INDEX IF NOT EXISTS invoices_due_date ON invoices (due_date) WHERE status = 'pending';

-- payments.invoice_id is optional: a payment can exist with no invoice (cash on the spot).
-- There is no link from a payment back to the promotion used, and no refund/reversal status.
-- receipt_number is UNIQUE; gateway_ref is not, so repeated gateway callbacks can duplicate.
CREATE TABLE IF NOT EXISTS payments (
    id             SERIAL PRIMARY KEY,
    invoice_id     INTEGER        REFERENCES invoices(id) ON DELETE SET NULL,
    username       VARCHAR(64),
    amount         NUMERIC(12,2)  NOT NULL CHECK (amount > 0),
    currency       VARCHAR(10)    NOT NULL DEFAULT 'USD',
    payment_method VARCHAR(30)    NOT NULL DEFAULT 'cash',
    gateway        VARCHAR(50),
    gateway_ref    VARCHAR(120),
    status         VARCHAR(20)    NOT NULL DEFAULT 'confirmed',
    notes          TEXT,
    receipt_number VARCHAR(40)    UNIQUE NOT NULL,
    processed_by   INTEGER        REFERENCES app_users(id) ON DELETE SET NULL,
    created_at     TIMESTAMP      DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS payments_username   ON payments (username);
CREATE INDEX IF NOT EXISTS payments_invoice_id ON payments (invoice_id);
CREATE INDEX IF NOT EXISTS payments_created_at ON payments (created_at);

-- discount_type is free text; the code understands 'percent' and treats anything else as a
-- fixed amount. max_uses = 0 means unlimited. Not referenced by invoices or payments.
CREATE TABLE IF NOT EXISTS promotions (
    id             SERIAL PRIMARY KEY,
    code           VARCHAR(50)    UNIQUE NOT NULL,
    description    TEXT,
    discount_type  VARCHAR(20)    NOT NULL DEFAULT 'percent',
    discount_value NUMERIC(12,2)  NOT NULL CHECK (discount_value > 0),
    plan_id        INTEGER        REFERENCES user_plans(id) ON DELETE SET NULL,
    max_uses       INTEGER        NOT NULL DEFAULT 0,
    uses_count     INTEGER        NOT NULL DEFAULT 0,
    valid_from     TIMESTAMPTZ,
    valid_until    TIMESTAMPTZ,
    is_active      BOOLEAN        NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMP      DEFAULT CURRENT_TIMESTAMP
);

-- task_type must match a case in handlers/scheduler.go runTask; unknown types just log
-- "unknown task". schedule is one of hourly | daily | weekly (anything else acts as daily).
CREATE TABLE IF NOT EXISTS scheduled_tasks (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(100) NOT NULL,
    task_type   VARCHAR(50)  UNIQUE NOT NULL,
    schedule    VARCHAR(20)  NOT NULL DEFAULT 'daily',
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    last_run    TIMESTAMP,
    next_run    TIMESTAMP,
    last_result TEXT,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

-- renew_plans extends account_expiry without checking payment, so it ships disabled.
INSERT INTO scheduled_tasks (name, task_type, schedule, is_active) VALUES
    ('Expire Accounts',          'expire_accounts',       'daily',  TRUE),
    ('Mark Overdue Invoices',    'mark_overdue_invoices', 'daily',  TRUE),
    ('Cleanup Stale Sessions',   'cleanup_sessions',      'hourly', TRUE),
    ('Daily Report',             'daily_report',          'daily',  TRUE),
    ('Auto-Renew Plans (no payment check)', 'renew_plans', 'daily', FALSE)
ON CONFLICT (task_type) DO NOTHING;

INSERT INTO system_settings (key, value, description) VALUES
    ('org_email', '', 'Contact email printed on payment receipts')
ON CONFLICT (key) DO NOTHING;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['user_plans','invoices'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS update_%1$s_updated_at ON %1$s', t);
        EXECUTE format('CREATE TRIGGER update_%1$s_updated_at BEFORE UPDATE ON %1$s
                        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column()', t);
    END LOOP;
END $$;
