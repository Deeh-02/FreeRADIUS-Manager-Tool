-- 016: M-Pesa (Safaricom Daraja) payments. See backend/internal/handlers/mpesa.go.
--
-- mpesa_stk:  one row per payment prompt sent to a customer's phone (STK push). Its result
--             comes back on a callback (or, if that is lost, from the reconcile task's query);
--             the row moves from pending to paid / failed exactly once, and only the move to
--             paid records a payment.
-- mpesa_c2b:  one row per payment a customer made to the paybill/till themselves, as Safaricom
--             confirmed it. trans_id is M-Pesa's own transaction code, so a confirmation sent
--             twice is stored once. A payment whose account number matches no customer stays
--             'unmatched' for staff to allocate; the money is never dropped.
-- Payments recorded from either carry gateway 'mpesa' and the M-Pesa code as gateway_ref, so
-- the same code can never be entered again by hand (payments_gateway_ref_unique).

CREATE TABLE IF NOT EXISTS mpesa_stk (
    id                   SERIAL PRIMARY KEY,
    checkout_request_id  VARCHAR(100) UNIQUE,
    merchant_request_id  VARCHAR(100),
    invoice_id           INTEGER REFERENCES invoices(id) ON DELETE SET NULL,
    username             VARCHAR(64) NOT NULL,
    phone                VARCHAR(20) NOT NULL,
    amount               NUMERIC(12,2) NOT NULL CHECK (amount > 0),
    status               VARCHAR(20) NOT NULL DEFAULT 'sending'
                         CHECK (status IN ('sending','pending','paid','failed')),
    result_code          VARCHAR(20),
    result_desc          TEXT,
    mpesa_receipt        VARCHAR(40),
    payment_ids          INTEGER[] NOT NULL DEFAULT '{}',
    raw_callback         JSONB,
    requested_by         INTEGER REFERENCES app_users(id) ON DELETE SET NULL,
    via_portal           BOOLEAN NOT NULL DEFAULT FALSE,
    last_query_at        TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS mpesa_stk_open ON mpesa_stk (created_at) WHERE status IN ('sending','pending');
CREATE INDEX IF NOT EXISTS mpesa_stk_username ON mpesa_stk (username);

CREATE TABLE IF NOT EXISTS mpesa_c2b (
    id               SERIAL PRIMARY KEY,
    trans_id         VARCHAR(40) NOT NULL UNIQUE,
    trans_time       VARCHAR(20),
    amount           NUMERIC(12,2) NOT NULL,
    bill_ref         VARCHAR(100),
    msisdn           VARCHAR(100),
    payer_name       VARCHAR(200),
    short_code       VARCHAR(20),
    status           VARCHAR(20) NOT NULL DEFAULT 'unmatched'
                     CHECK (status IN ('applied','unmatched')),
    username         VARCHAR(64),
    payment_ids      INTEGER[] NOT NULL DEFAULT '{}',
    allocated_by     INTEGER REFERENCES app_users(id) ON DELETE SET NULL,
    raw              JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS mpesa_c2b_unmatched ON mpesa_c2b (created_at) WHERE status = 'unmatched';

INSERT INTO scheduled_tasks (name, task_type, schedule, is_active) VALUES
    ('M-Pesa: check unanswered payment prompts', 'mpesa_reconcile', 'hourly', TRUE)
ON CONFLICT (task_type) DO NOTHING;
