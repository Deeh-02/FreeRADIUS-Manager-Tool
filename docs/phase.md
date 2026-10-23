# Work plan by phase

The to-do list for getting FreeRADIUS Manager ready to bill real customers. It is built from the
review in [ARCHITECTURE.md § 10 "Known gaps and risks"](ARCHITECTURE.md#10-known-gaps-and-risks);
the codes in brackets (B1, S2, …) point to the rows there.

**Where things stand (version 1.1, October 2026):** Phases 1 and 2 of the original plan are
done in code and tested against real PostgreSQL and FreeRADIUS. Billing now controls access:

- a paid-up customer gets online
- a customer who doesn't pay is refused after their paid-until date
- paying lets them back on

Since then the billing code has automated tests (concurrency, a 560-day time simulation, edge
cases: [ARCHITECTURE.md § 10.8](ARCHITECTURE.md#108-automated-billing-tests)). They found and
fixed about ten real bugs (B11–B19, Q8). **Passing them does not make it ready to deploy:** they
cannot show how real routers, your server, your timezone and your staff behave. Order of work:

- your decisions (Phase 0)
- a trial on real hardware, including failure and load tests (Phase A)
- a small pilot with real customers (Phase P): the gate before full launch
- hardening after launch (Phase B)
- larger features for later (Phase C)

Sizes are rough: **S** = hours, **M** = a few days, **L** = a week or more.

---

## Done in 1.1

| Original task | What was done |
|---|---|
| 1.1 Cut access on expiry/non-payment [B1] | Paid-until written as `Expiration`; expired/suspended as `Auth-Type := Reject`; sessions disconnected. |
| 1.2 Payment → invoice → renewal [B3, B6] | One transaction; partial payments; reversal takes days back; invoice selector in the UI. |
| 1.3 Assign plan button [B6] | Assign Plan dialog; no duplicate invoices; extends from paid-until. |
| 1.4 Receipts [B4] | Escaped HTML, opened with the auth header. |
| 1.5 Missing tables [D1, D2] | Migration 010; CSV import fixed. |
| 1.6 Secrets [S2, N5] | Refuse weak secrets everywhere; seed NAS removed; no passwords in logs; cryptographic generator on Windows. |
| 1.7 Backups [D4] | Daily `pg_dump` with retention, download from Settings, failure alert. *A test restore on your server is still to do (Phase A).* |
| 1.8 HTTPS, close 8088 [S8] | Optional Caddy; API on 127.0.0.1; CORS off by default. |
| 1.9 Setup wizard [S4] | Only while no staff account exists, setup token, never deletes. |
| 2.1 Client IP / portal lockout [S3] | Trusted proxies; per-account lockout. |
| 2.3 Three suspected bugs [N1, N2, D3] | All three were real (accounting recorded nothing at all); all fixed and tested. |
| 2.4 CSV import [D2] | Fixed. |
| 2.5 Real Disconnect [N3] | RFC 5176 Disconnect-Request, also on cut-off and over quota. |
| 2.6 Data caps [B10] | FreeRADIUS `sqlcounter` + hourly over-quota disconnect. |
| 2.7 Customer-facing billing [B7] (part) | Reminders by SMS/email, invoices in the portal. *Gateway still open.* |
| 2.9 RADIUS client trust [N4] (part) | Per-NAS secrets and generator, Message-Authenticator `auto`. |
| Phase 3 items | Promotions [B5], vouchers [B9], sequential numbers [B8 part], GeoIP/credential stuffing/API keys [S10], Settings and MFA [S6], tokens [S5], webhooks [D6, S7 part], migrations [D5, Q4]. |
| Clean-ups | UI rough edges [Q6], SMS variables passed through, dead code [Q7], versions [Q5], README and router guides rewritten, binary removed [Q2]. |

The 1.1 work also found and fixed issues the review missed. Highlights:

- staff were signed out every hour in Docker
- the rate limiter blocked anyone who kept the dashboard open
- there was no way to set up MFA
- old-format expiry rows locked out paying customers

The full list is at the end of [ARCHITECTURE.md § 10.5](ARCHITECTURE.md#105-quality-and-operations).

---

## Phase 0: your decisions

These were defaulted in code; all are changeable on the **Settings** page.

- [x] **Prepaid or postpaid?** **Decided: prepaid** — no payment, no access. Postpaid per customer:
      tick *Start now* when assigning the plan.
- [x] **Grace period** after the paid-until date. **Decided:** a system default (`billing_grace_days`,
      Settings) plus a per-customer value that staff set or extend on the customer dialog when a
      customer asks. **Default for new customers: none** (`billing_grace_days` = 0); only staff give
      grace days, one customer at a time.
- [x] **Free trial for new customers:** **built and flexible.** The team sets the default in
      Settings → *Free trial for new customers (days)* (0 = off), and staff can give a different
      number, or none, for any one customer in the New customer / Assign Plan dialogs.
      **Default: 5 days** (migration 014); the team can change it any time.
- [ ] **Renewal invoices:** how many days before paid-until. Default: 3 (`renewal_invoice_days`).
- [ ] **Overdue postpaid invoices block service?** Default: no (`suspend_overdue`).
- [ ] **Reminders:** days before expiry and the message. Default: 3 days, SMS/email.
- [ ] **How customers pay:** staff-recorded (works now), or a gateway such as M-Pesa (Phase C).
- [x] **Currency:** **KES** (new installs and new rows default to KES; existing plans keep their
      currency, so check Plans after upgrading).
- [ ] **Tax:** decide whether invoices need VAT (Phase C).
- [x] **Deleting a customer** [B20]: **decided and built.** Prepaid means nothing is owed, so
      unpaid invoices are cancelled with the customer. If an invoice has money on it or service
      was started on credit, the delete is refused until staff refund, collect or write it off.
- [x] **Who may reverse payments and delete customers?** **Decided:** admin and super admin
      (operators cannot). Reversal was super-admin only before; it now allows admin too.

**Done when** the Settings page reflects these choices.

---

## Phase A: trial on real hardware (before the first paying customer)

Do this on a test server that matches production, with one real router.

**A-1. Set up**

| # | Task | Size |
|---|---|---|
| A.1 | Build and run the Docker images on a test server (`./setup.sh`); check every container is healthy; enable HTTPS with `COMPOSE_PROFILES=https`. Nothing in the Docker setup has been run yet. | S |
| A.1a | **Before applying migration 011** to any database that already holds payments: check there are no duplicate payment references (`SELECT gateway, upper(gateway_ref), count(*) FROM payments WHERE status='confirmed' AND gateway_ref IS NOT NULL GROUP BY 1,2 HAVING count(*)>1`). The new unique index fails to create if there are. Fix them first. | S |
| A.1b | **Timezone:** set `TZ` once in `.env` to your local zone and confirm PostgreSQL, FreeRADIUS and the backend all use it. Then check a customer whose paid-until is today is refused at 00:00 local time, not at 00:00 UTC. | S |
| A.5 | Set up SMTP (and SMS if used); send a test email; create alert rules for `nas_down` and `backup_failed`. Confirm the alert actually reaches you. | S |

**A-2. Real router and the customer journey**

| # | Task | Size |
|---|---|---|
| A.2 | Connect one test MikroTik (hotspot and/or PPPoE) as in [DEVICE-INTEGRATION.md](../DEVICE-INTEGRATION.md): accounting with interim updates, `/radius incoming` for Disconnect. Check login, "online now", usage, speed profile, device limit, Disconnect button. | S–M |
| A.3 | Walk a test customer through: assign plan → offline until paid → record payment → online → expire (set paid-until to yesterday, run *Expire Accounts*) → kicked off → pay again → back online. | S |
| A.3a | Real routers retry and time out in their own ways: record a payment while the browser is throttled or the request is cut off, and check the Record Payment dialog's retry produces one payment and one receipt. | S |

**A-3. Failure and recovery** (the tests the repository does not have yet)

| # | Task | Size |
|---|---|---|
| A.7 | Restart PostgreSQL while payments are being recorded (script a loop of payments, restart the container). Afterwards run the invariant checks: no payment without its days, no days without a payment. | S–M |
| A.8 | Stop FreeRADIUS, record payments and an expiry, start it again: the paying customer must get online, the expired one must not. | S |
| A.9 | Make the router ignore Disconnect (firewall port 3799): billing must still complete, and the failed disconnect must be visible to staff. If it is not visible, that is a bug to fix. | S |
| A.4 | **Restore a backup** into a scratch database, then re-run the invariant checks against it (`backend/internal/handlers/invariants_test.go`, `checkMoney` and `checkEnforcement`). Time how long a restore takes. | S |
| A.10 | Kill the backend container in the middle of a payment (repeatedly, under a payment loop): after each restart the books must balance. | S |

**A-4. Load and volume** (measure on the production-class server, not a laptop)

| # | Task | Target | Size |
|---|---|---|---|
| A.11 | Logins through a RADIUS load tool (`radclient`/`radperf`). Every login runs about 5 queries including a data-usage total. | Several hundred per second at your expected customer count; tell me your number. | M |
| A.12 | Accounting flood: all sessions sending interim updates every 5 minutes, sustained for hours. | 5,000 sessions ≈ 17 updates/second with no backlog. | M |
| A.13 | Fill the usage table (`radacct`) with 1–5 million rows; check login speed and the data-cap total. Probable fix: an index on `(username, acctstarttime)`. | Both stay in milliseconds. | S–M |
| A.14 | Renewal and expiry tasks over your customer count, run while staff use the panel. | No lock waits that block staff. | S |
| A.15 | Billing and Payments pages and reports with 100,000 invoices. | Pages stay quick. | S–M |

**A-5. Operations**

| # | Task | Size |
|---|---|---|
| A.16 | Write down and rehearse your recovery steps: reverse a wrong payment, resync a customer's access (`resync-access`), reset a password and MFA, restore from backup. | S |
| A.17 | Confirm the daily backup runs by itself, is copied off the server, and a failed backup alerts you. | S |
| A.6 | **Upgrading an existing 1.0 install:** run `./deploy-update.sh` and read the list of customers who will be refused before confirming; extend anyone who has paid. | S |

**Done when** A.1–A.5 and A.7–A.17 pass on the test server. (A.6 only if you have a 1.0 install.)
Any failure in A.7–A.10 or A.13 is a bug to fix before continuing.

---

## Phase P: pilot (the gate before full launch)

Real customers, real money, but few of them, and every payment checked by hand.

- [ ] Start with about 10–20 friendly customers on one router. Do not move everyone at once.
- [ ] Run for at least **one full billing cycle** and ideally two, so you see renewals, expiry,
      late payers and at least one reversal for real.
- [ ] **Every day**, reconcile: the Payments page total against your cash, bank and mobile-money
      records, per currency. Any difference is a bug or a staff error; find out which.
- [ ] **Every day**, run `access-report` and check nobody who paid is blocked and nobody who
      didn't pay is online.
- [ ] Keep a log of every surprise (a customer cut off wrongly, a duplicate, a confusing screen).
- [ ] Watch the alerts and the backup status each day.

**Go-live gate: all of these must be true**

- [ ] Phase A is done and every failure test passed.
- [ ] The pilot ran a full cycle with **no unexplained difference** in the daily reconciliation.
- [ ] No customer was wrongly cut off, or wrongly left online, without a cause you understand.
- [ ] You have restored a backup, and rehearsed the recovery steps.
- [ ] You know your answers to Phase 0 (including tax and deleted-customer invoices).
- [ ] The billing tests (`make test-integration`) pass on the version you are about to run.

Then onboard in stages (for example a quarter of customers, then half, then all), repeating the
daily reconciliation for the first week of each stage.

---

## Phase B: soon after launch

| # | Task | Why | Size |
|---|---|---|---|
| B.1 | **CI** [Q1]: run the billing tests (`make test-integration`) on every push. They are in the repository ([ARCHITECTURE.md § 10.8](ARCHITECTURE.md#108-automated-billing-tests)) but nothing runs them automatically. Then extend tests to the rest of the API, FreeRADIUS and a browser smoke test. | The billing and enforcement paths cost money if they break. | S–M |
| B.1a | **Turn the Phase A failure tests (A.7–A.10) into repeatable scripts** so they run before each release. | They are manual until then. | M |
| B.1b | **More simulation coverage:** other seeds, `suspend_overdue` on, non-default `invoice_due_days`, and a customer deleted while a payment is being recorded. Give back a promo use when an unpaid invoice is cancelled on a plan switch. | Found while writing the tests; not covered yet. | S–M |
| B.2 | **Audit the remaining handlers for ignored errors** [D7]. | A failure should never look like "saved". | M |
| B.3 | **EAP/PEAP** [N6], if you use WPA-Enterprise: certificates and a test with a real client. | Untested today. | M |
| B.4 | **NAS monitor via Status-Server** [N7], so routers that block TCP probes are not shown "down". | Cosmetic but confusing. | S |
| B.5 | **Scanner-imported routers get their own secret** [N4]. | They reuse `RADIUS_SECRET` until edited. | S |

---

## Phase C: larger features

- [ ] **Payment gateway** [B7], e.g. M-Pesa STK push / C2B callbacks calling `recordPaymentTx`, so
      customers can pay themselves and get reconnected automatically. **L**
- [ ] **Money and tax** [B8]: exact decimal amounts in code, tax/VAT lines, proration, credit
      notes. **L**
- [ ] **Real multi-tenancy for resellers** [S9]: scope every query by organisation. Until then,
      do not give resellers staff accounts. **L**
- [ ] **Encrypt TOTP and webhook secrets at rest** [S1] (customer passwords must stay readable for
      PAP/CHAP; protect the database and backups). **M**
- [ ] **Scaling beyond one backend** [Q3]: shared rate limiting/lockout (e.g. in PostgreSQL or
      Redis). **M**
- [ ] **Run the API container as non-root** [S7] (nmap capabilities via `setcap`, backup folder
      ownership). **S–M**
- [ ] **License** for the code: ask the original author to add one before you redistribute it.
