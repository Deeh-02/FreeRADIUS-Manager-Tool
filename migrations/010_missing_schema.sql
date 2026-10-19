-- ============================================================
-- 010: tables/columns the application uses that no earlier SQL file created,
--      plus the schema for billing enforcement, payments↔invoices, quotas and backups.
-- Idempotent: safe to re-run (deploy-update.sh applies every migration file).
-- ============================================================
--
-- Column lists were derived from the queries in backend/internal/handlers. If your live
-- database already had some of these tables (built by hand or by another version), the
-- CREATE TABLE IF NOT EXISTS statements leave them alone; compare their columns with the
-- definitions below if a screen misbehaves.

-- ── Organisations (labels only: nothing filters data by organisation) ─────────
CREATE TABLE IF NOT EXISTS organizations (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(100) UNIQUE NOT NULL,
    slug        VARCHAR(50)  UNIQUE NOT NULL,
    email       VARCHAR(100),
    phone       VARCHAR(30),
    address     TEXT,
    logo_url    TEXT,
    user_limit  INTEGER      NOT NULL DEFAULT 0,
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    parent_id   INTEGER      REFERENCES organizations(id) ON DELETE SET NULL,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

-- ── Hotspot zones ──────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS hotspot_zones (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    location    VARCHAR(200),
    max_clients INTEGER      NOT NULL DEFAULT 0,
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

-- ── Columns on existing tables ────────────────────────────────────────────
ALTER TABLE nas ADD COLUMN IF NOT EXISTS ping_status     VARCHAR(10);
ALTER TABLE nas ADD COLUMN IF NOT EXISTS ping_latency_ms DOUBLE PRECISION;
ALTER TABLE nas ADD COLUMN IF NOT EXISTS last_ping       TIMESTAMP;
ALTER TABLE nas ADD COLUMN IF NOT EXISTS zone_id         INTEGER REFERENCES hotspot_zones(id) ON DELETE SET NULL;
ALTER TABLE nas ADD COLUMN IF NOT EXISTS org_id          INTEGER REFERENCES organizations(id) ON DELETE SET NULL;
-- Disconnect-Request / CoA port on the router (RFC 5176). MikroTik and most others use 3799.
ALTER TABLE nas ADD COLUMN IF NOT EXISTS coa_port        INTEGER NOT NULL DEFAULT 3799;

ALTER TABLE app_users ADD COLUMN IF NOT EXISTS org_id INTEGER REFERENCES organizations(id) ON DELETE SET NULL;

ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS org_id INTEGER REFERENCES organizations(id) ON DELETE SET NULL;
-- First day of the current paid period: the data cap (mods-available/quota) counts from here.
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS period_start DATE;
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS notes TEXT;
-- Expiry date the last reminder was sent for, so each period is reminded once.
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS last_reminder_for DATE;
-- Per-account lockout for the customer self-service portal.
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS portal_failed_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE radius_users ADD COLUMN IF NOT EXISTS portal_locked_until TIMESTAMP;
CREATE INDEX IF NOT EXISTS radius_users_expiry ON radius_users (account_expiry);

-- ── Billing: link payments to invoices and invoices to service periods ──────
-- service_applied: TRUE once the plan period this invoice pays for has been added to the
-- customer's expiry (either when it was paid, or up front for "start now" assignments).
-- applied_days lets a reversed payment take the same days back off.
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS service_applied BOOLEAN       NOT NULL DEFAULT FALSE;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS applied_days    INTEGER       NOT NULL DEFAULT 0;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS period_end      DATE;
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS promo_code      VARCHAR(50);
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS discount_amount NUMERIC(12,2) NOT NULL DEFAULT 0;
-- A paid invoice is settled: paying it again must not add another period.
UPDATE invoices SET service_applied = TRUE WHERE status = 'paid' AND service_applied = FALSE;

-- Payments are reversed, not deleted, so the ledger keeps its history.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS reversed_at     TIMESTAMP;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS reversed_by     INTEGER REFERENCES app_users(id) ON DELETE SET NULL;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS reversal_reason TEXT;
CREATE INDEX IF NOT EXISTS payments_status ON payments (status);
CREATE UNIQUE INDEX IF NOT EXISTS payments_gateway_ref_unique ON payments (gateway, gateway_ref)
    WHERE gateway_ref IS NOT NULL AND status = 'confirmed';

-- Sequential, gap-tolerant document numbers (INV-2026-000001, RCP-2026-000001).
CREATE SEQUENCE IF NOT EXISTS invoice_number_seq;
CREATE SEQUENCE IF NOT EXISTS receipt_number_seq;

-- Vouchers can carry a plan (speed, device limit) and a sale price.
ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS plan_id  INTEGER REFERENCES user_plans(id) ON DELETE SET NULL;
ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS price    NUMERIC(12,2) NOT NULL DEFAULT 0;
ALTER TABLE vouchers ADD COLUMN IF NOT EXISTS currency VARCHAR(10)   NOT NULL DEFAULT 'USD';

-- ── FreeRADIUS: NAS reboot tracking used by simul_count_query (queries.conf) ──
CREATE TABLE IF NOT EXISTS nasreload (
    nasipaddress INET PRIMARY KEY,
    reloadtime   TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Routers send longer NAS-Port-Id / Connect-Info / station ids than the old widths allowed;
-- an over-long value made the whole accounting INSERT fail. VARCHAR -> TEXT is metadata-only.
ALTER TABLE radacct ALTER COLUMN nasportid         TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN connectinfo_start TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN connectinfo_stop  TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN calledstationid   TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN callingstationid  TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN acctsessionid     TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN acctuniqueid      TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN nasporttype       TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN servicetype       TYPE TEXT;
ALTER TABLE radacct ALTER COLUMN framedprotocol    TYPE TEXT;
ALTER TABLE radpostauth ALTER COLUMN calledstationid  TYPE TEXT;
ALTER TABLE radpostauth ALTER COLUMN callingstationid TYPE TEXT;
CREATE INDEX IF NOT EXISTS radacct_open_sessions ON radacct (username) WHERE acctstoptime IS NULL;
CREATE INDEX IF NOT EXISTS radpostauth_reply ON radpostauth (reply, authdate);

-- ── Customer portal sessions ───────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS portal_sessions (
    token       VARCHAR(64) PRIMARY KEY,
    username    VARCHAR(64) NOT NULL,
    expires_at  TIMESTAMP   NOT NULL,
    created_at  TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS portal_sessions_expires ON portal_sessions (expires_at);

-- ── Alerts (email to staff) ────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS alert_rules (
    id             SERIAL PRIMARY KEY,
    name           VARCHAR(100) NOT NULL,
    event_type     VARCHAR(50)  NOT NULL,
    notify_email   BOOLEAN      NOT NULL DEFAULT TRUE,
    email_address  VARCHAR(200),
    is_active      BOOLEAN      NOT NULL DEFAULT TRUE,
    last_triggered TIMESTAMP,
    created_at     TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);
-- threshold: e.g. failed logins per 5 minutes for login_failure. cooldown: minimum minutes
-- between two emails for the same rule, so a lasting condition does not mail every 5 min.
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS threshold        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS cooldown_minutes INTEGER NOT NULL DEFAULT 360;

-- ── SMS log ────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS sms_logs (
    id          SERIAL PRIMARY KEY,
    recipient   VARCHAR(30)  NOT NULL,
    message     TEXT         NOT NULL,
    status      VARCHAR(20)  NOT NULL,
    gateway_ref VARCHAR(120),
    username    VARCHAR(64),
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS sms_logs_created ON sms_logs (created_at DESC);

-- ── Bulk operation history ─────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS bulk_operations (
    id            SERIAL PRIMARY KEY,
    operation     VARCHAR(50) NOT NULL,
    target_count  INTEGER     NOT NULL DEFAULT 0,
    success_count INTEGER     NOT NULL DEFAULT 0,
    fail_count    INTEGER     NOT NULL DEFAULT 0,
    params        JSONB,
    performed_by  INTEGER     REFERENCES app_users(id) ON DELETE SET NULL,
    created_at    TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);

-- ── IP pools ───────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ip_pools (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    network     VARCHAR(45)  NOT NULL,
    gateway     VARCHAR(45),
    dns1        VARCHAR(45),
    dns2        VARCHAR(45),
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS ip_pool_assignments (
    id          SERIAL PRIMARY KEY,
    pool_id     INTEGER     NOT NULL REFERENCES ip_pools(id) ON DELETE CASCADE,
    ip_address  VARCHAR(45) NOT NULL UNIQUE,
    username    VARCHAR(64),
    user_id     INTEGER     REFERENCES radius_users(id) ON DELETE SET NULL,
    is_static   BOOLEAN     NOT NULL DEFAULT FALSE,
    leased_at   TIMESTAMP,
    created_at  TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS ip_pool_assignments_pool ON ip_pool_assignments (pool_id);
CREATE INDEX IF NOT EXISTS ip_pool_assignments_user ON ip_pool_assignments (username);

-- ── RADIUS attribute templates ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS radius_templates (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(100) UNIQUE NOT NULL,
    description TEXT,
    attributes  JSONB        NOT NULL DEFAULT '[]',
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

-- ── API keys (sha256 of the key; the key itself is shown once) ─────────────
CREATE TABLE IF NOT EXISTS api_keys (
    id          SERIAL PRIMARY KEY,
    name        VARCHAR(100) NOT NULL,
    key_hash    VARCHAR(64)  UNIQUE NOT NULL,
    key_prefix  VARCHAR(16)  NOT NULL,
    permissions TEXT[]       NOT NULL DEFAULT '{read}',
    created_by  INTEGER      REFERENCES app_users(id) ON DELETE SET NULL,
    last_used   TIMESTAMP,
    expires_at  TIMESTAMP,
    is_active   BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

-- ── Captive portal pages ───────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS captive_portals (
    id            SERIAL PRIMARY KEY,
    zone_id       INTEGER      REFERENCES hotspot_zones(id) ON DELETE SET NULL,
    name          VARCHAR(100) NOT NULL,
    title         VARCHAR(200) NOT NULL DEFAULT 'WiFi Login',
    subtitle      TEXT,
    logo_url      TEXT,
    bg_color      VARCHAR(20)  NOT NULL DEFAULT '#f0f4ff',
    primary_color VARCHAR(20)  NOT NULL DEFAULT '#3b82f6',
    auth_type     VARCHAR(20)  NOT NULL DEFAULT 'userpass',
    redirect_url  TEXT         NOT NULL DEFAULT '',
    terms_text    TEXT,
    footer_text   TEXT,
    is_active     BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);
-- Where the login form posts. The router's hotspot does the RADIUS login, so this must be the
-- router's login URL (MikroTik: http://<hotspot-dns-name>/login).
ALTER TABLE captive_portals ADD COLUMN IF NOT EXISTS login_url TEXT NOT NULL DEFAULT '';

-- ── CRM: customers and tickets ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS customers (
    id             SERIAL PRIMARY KEY,
    username       VARCHAR(64)  UNIQUE,
    full_name      VARCHAR(100) NOT NULL,
    email          VARCHAR(100),
    phone          VARCHAR(30),
    id_number      VARCHAR(50),
    address        TEXT,
    city           VARCHAR(100),
    country        VARCHAR(100),
    org_id         INTEGER      REFERENCES organizations(id) ON DELETE SET NULL,
    contract_start DATE,
    contract_end   DATE,
    notes          TEXT,
    created_at     TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS tickets (
    id          SERIAL PRIMARY KEY,
    customer_id INTEGER      REFERENCES customers(id) ON DELETE SET NULL,
    username    VARCHAR(64),
    title       VARCHAR(200) NOT NULL,
    description TEXT,
    status      VARCHAR(20)  NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','in_progress','resolved','closed')),
    priority    VARCHAR(20)  NOT NULL DEFAULT 'medium'
                    CHECK (priority IN ('low','medium','high','urgent')),
    assigned_to INTEGER      REFERENCES app_users(id) ON DELETE SET NULL,
    resolved_at TIMESTAMP,
    created_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS tickets_customer ON tickets (customer_id);
CREATE INDEX IF NOT EXISTS tickets_status   ON tickets (status);

-- ── Webhooks ───────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS webhooks (
    id             SERIAL PRIMARY KEY,
    name           VARCHAR(100) NOT NULL,
    url            TEXT         NOT NULL,
    secret         TEXT,
    events         JSONB        NOT NULL DEFAULT '[]',
    is_active      BOOLEAN      NOT NULL DEFAULT TRUE,
    last_triggered TIMESTAMP,
    fail_count     INTEGER      NOT NULL DEFAULT 0,
    created_at     TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);
-- An older hand-made table may hold events as text[]; the code now reads JSONB.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name = 'webhooks' AND column_name = 'events' AND data_type = 'ARRAY') THEN
        ALTER TABLE webhooks ALTER COLUMN events DROP DEFAULT;
        ALTER TABLE webhooks ALTER COLUMN events TYPE JSONB USING to_jsonb(events);
        ALTER TABLE webhooks ALTER COLUMN events SET DEFAULT '[]';
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS webhook_logs (
    id          SERIAL PRIMARY KEY,
    webhook_id  INTEGER     NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event       VARCHAR(50) NOT NULL,
    payload     JSONB,
    status_code INTEGER     NOT NULL DEFAULT 0,
    success     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS webhook_logs_hook ON webhook_logs (webhook_id, created_at DESC);

-- ── Security suite ─────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS security_alerts (
    id              SERIAL PRIMARY KEY,
    alert_type      VARCHAR(50) NOT NULL,
    severity        VARCHAR(10) NOT NULL DEFAULT 'medium',
    ip_address      VARCHAR(45),
    username        VARCHAR(64),
    country_code    VARCHAR(2),
    details         JSONB       NOT NULL DEFAULT '{}',
    is_acknowledged BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS security_alerts_created ON security_alerts (created_at DESC);

-- Blocks apply to the staff login and the customer portal login (handlers/security.go).
CREATE TABLE IF NOT EXISTS cred_stuffing_blocks (
    id            SERIAL PRIMARY KEY,
    ip_address    VARCHAR(45) UNIQUE NOT NULL,
    fail_count    INTEGER     NOT NULL DEFAULT 0,
    blocked_until TIMESTAMP,
    reason        TEXT,
    auto_blocked  BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMP   DEFAULT CURRENT_TIMESTAMP,
    updated_at    TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS geoip_cache (
    ip_address   VARCHAR(45) PRIMARY KEY,
    country_code VARCHAR(2)   NOT NULL DEFAULT '',
    country_name VARCHAR(100) NOT NULL DEFAULT '',
    city         VARCHAR(100) NOT NULL DEFAULT '',
    isp          VARCHAR(200) NOT NULL DEFAULT '',
    is_vpn       BOOLEAN      NOT NULL DEFAULT FALSE,
    looked_up_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS geoip_rules (
    id           SERIAL PRIMARY KEY,
    country_code VARCHAR(2)   UNIQUE NOT NULL,
    country_name VARCHAR(100) NOT NULL,
    action       VARCHAR(10)  NOT NULL DEFAULT 'block' CHECK (action IN ('block','allow','flag')),
    is_active    BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS honeypot_logs (
    id          SERIAL PRIMARY KEY,
    source_ip   VARCHAR(45) NOT NULL,
    username    VARCHAR(253),
    nas_ip      VARCHAR(45),
    packet_type VARCHAR(30) NOT NULL DEFAULT 'Access-Request',
    attributes  JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMP   DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS honeypot_logs_source  ON honeypot_logs (source_ip);
CREATE INDEX IF NOT EXISTS honeypot_logs_created ON honeypot_logs (created_at DESC);

-- ── updated_at triggers for the new tables ─────────────────────────────────
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['customers','tickets','cred_stuffing_blocks'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS update_%1$s_updated_at ON %1$s', t);
        EXECUTE format('CREATE TRIGGER update_%1$s_updated_at BEFORE UPDATE ON %1$s
                        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column()', t);
    END LOOP;
END $$;

-- ── Settings that now take effect (read by the API; editable on the Settings page) ──
INSERT INTO system_settings (key, value, description) VALUES
    ('default_currency',      'USD',  'Currency used when a plan or invoice does not name one'),
    ('invoice_due_days',      '7',    'Days after issue an invoice is due'),
    ('billing_grace_days',    '0',    'Days a customer stays connected after their paid-until date'),
    ('renewal_invoice_days',  '3',    'Raise the next invoice this many days before a plan expires'),
    ('reminder_days',         '3',    'Send the expiry reminder this many days before the paid-until date'),
    ('reminder_message',      'Dear {username}, your internet subscription expires on {expiry}. Please pay {amount} to stay connected.', 'Reminder text ({username}, {expiry}, {amount}, {plan})'),
    ('suspend_overdue',       'false','Block customers whose service was started on credit once their invoice is overdue'),
    ('portal_max_attempts',   '5',    'Failed customer-portal logins before the account is locked'),
    ('portal_lockout_minutes','15',   'Customer-portal lockout duration in minutes'),
    ('backup_retention_days', '14',   'Delete automatic backups older than this many days'),
    ('report_email',          '',     'Email address for the daily report (needs SMTP)'),
    ('honeypot_enabled',      'true', 'Run the fake RADIUS listener on HONEYPOT_PORT'),
    ('cs_max_fails',          '10',   'Failed logins from one IP within the window before it is blocked'),
    ('cs_window_secs',        '300',  'Credential-stuffing detection window in seconds'),
    ('cs_block_mins',         '60',   'How long an automatically blocked IP stays blocked')
ON CONFLICT (key) DO NOTHING;
UPDATE system_settings SET description = 'Cron-style text, informational only; the backup_database scheduled task runs daily'
 WHERE key = 'backup_schedule';
UPDATE system_settings SET description = 'Superseded by backup_retention_days'
 WHERE key = 'backup_retention';

-- ── Scheduled tasks ────────────────────────────────────────────────────────
-- renew_plans no longer extends anything for free: it raises the renewal invoice for paid
-- plans (the period is added when the invoice is paid) and only extends free plans.
UPDATE scheduled_tasks SET name = 'Raise Renewal Invoices', is_active = TRUE
 WHERE task_type = 'renew_plans' AND name LIKE 'Auto-Renew Plans%';
INSERT INTO scheduled_tasks (name, task_type, schedule, is_active) VALUES
    ('Expiry Reminders (SMS/email)', 'expiry_reminders',     'daily',  TRUE),
    ('Voucher Housekeeping',         'voucher_housekeeping', 'hourly', TRUE),
    ('Disconnect Over-Quota Users',  'enforce_quota',        'hourly', TRUE),
    ('Database Backup',              'backup_database',      'daily',  TRUE),
    ('Purge Old Logs',               'purge_logs',           'weekly', TRUE)
ON CONFLICT (task_type) DO NOTHING;

-- ── Remove the well-known seed NAS secret ──────────────────────────────────
-- 127.0.0.1 is already a client in clients.conf with RADIUS_SECRET; this row only shadowed it.
DELETE FROM nas WHERE nasname = '127.0.0.1' AND secret = 'testing123';

-- ── Migration bookkeeping used by deploy-update.sh ─────────────────────────
CREATE TABLE IF NOT EXISTS schema_migrations (
    filename   VARCHAR(200) PRIMARY KEY,
    applied_at TIMESTAMP    DEFAULT CURRENT_TIMESTAMP
);
