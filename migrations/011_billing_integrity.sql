-- ============================================================
-- Billing integrity: duplicate-payment guards
-- Idempotent: safe to re-run (deploy-update.sh applies migrations/*.sql once each)
-- ============================================================

-- 1. A payment reference may be recorded once per gateway, whether or not the gateway field
--    was filled in. The old index (gateway, gateway_ref) treated two NULL gateways as
--    different, so the same M-Pesa code typed without a gateway could be recorded any number
--    of times. Case is ignored (QGH7XK29LP and qgh7xk29lp are the same code). Only confirmed
--    payments count, so a reversed payment's reference can be entered again.
DROP INDEX IF EXISTS payments_gateway_ref_unique;
CREATE UNIQUE INDEX IF NOT EXISTS payments_gateway_ref_unique
    ON payments ((COALESCE(gateway, '')), (UPPER(gateway_ref)))
    WHERE gateway_ref IS NOT NULL AND status = 'confirmed';

-- 2. Idempotency key: the client sends a one-time ID with every "record payment" request. If
--    the request is retried (timeout, double click, flaky network) the same key comes back and
--    the server returns the first result instead of recording the money twice. Cash payments
--    have no other reference to deduplicate on.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(80);
CREATE UNIQUE INDEX IF NOT EXISTS payments_idempotency_key_unique
    ON payments (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- 3. Looking up a customer's open invoices (assign plan, renewal task) by user and status.
CREATE INDEX IF NOT EXISTS invoices_user_status ON invoices (user_id, status);

-- 4. An invoice remembers how many days it buys (copied from the plan when it is raised), so
--    editing the plan while the invoice is open cannot change what the customer paid for.
--    NULL on older invoices: they fall back to the plan's current length.
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS plan_days INTEGER;
