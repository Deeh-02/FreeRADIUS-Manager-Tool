# Work plan by phase

The to-do list for getting FreeRADIUS Manager ready to bill real customers. It is built from the
review in [ARCHITECTURE.md § 10 "Known gaps and risks"](ARCHITECTURE.md#10-known-gaps-and-risks);
the codes in brackets (B1, S2, …) point to the rows there.

**Where things stand (version 1.1, October 2026):** Phases 1 and 2 of the original plan are
done in code and tested against real PostgreSQL and FreeRADIUS. Billing now controls access:

- a paid-up customer gets online
- a customer who doesn't pay is refused after their paid-until date
- paying lets them back on

What's left:

- your decisions (Phase 0)
- a trial on real hardware (Phase A)
- automated tests (Phase B)
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

- [ ] **Prepaid or postpaid?** Default: prepaid — no payment, no access. Postpaid per customer:
      tick *Start now* when assigning the plan.
- [ ] **Grace period** after the paid-until date. Default: 0 days (`billing_grace_days`).
- [ ] **Renewal invoices:** how many days before paid-until. Default: 3 (`renewal_invoice_days`).
- [ ] **Overdue postpaid invoices block service?** Default: no (`suspend_overdue`).
- [ ] **Reminders:** days before expiry and the message. Default: 3 days, SMS/email.
- [ ] **How customers pay:** staff-recorded (works now), or a gateway such as M-Pesa (Phase C).
- [ ] **Currency and tax:** set `DEFAULT_CURRENCY`; decide whether invoices need VAT (Phase C).

**Done when** the Settings page reflects these choices.

---

## Phase A: trial on real hardware (before the first paying customer)

| # | Task | Size |
|---|---|---|
| A.1 | Build and run the Docker images on a test server (`./setup.sh`); check every container is healthy; enable HTTPS with `COMPOSE_PROFILES=https`. | S |
| A.2 | Connect one test MikroTik (hotspot and/or PPPoE) as in [DEVICE-INTEGRATION.md](../DEVICE-INTEGRATION.md): accounting with interim updates, `/radius incoming` for Disconnect. Check login, "online now", usage, speed profile, device limit, Disconnect button. | S–M |
| A.3 | Walk a test customer through: assign plan → offline until paid → record payment → online → expire (set paid-until to yesterday, run *Expire Accounts*) → kicked off → pay again → back online. | S |
| A.4 | Restore a backup into a scratch database once (README → Backups and restore). | S |
| A.5 | Set up SMTP (and SMS if used); send a test email; create alert rules for `nas_down` and `backup_failed`. | S |
| A.6 | **Upgrading an existing 1.0 install:** run `./deploy-update.sh` and read the list of customers who will be refused before confirming; extend anyone who has paid. | S |

**Done when** A.1–A.5 pass on the test server. (A.6 only if you have a 1.0 install.)

---

## Phase B: soon after launch

| # | Task | Why | Size |
|---|---|---|---|
| B.1 | **Tests in the repository + CI** [Q1]: turn the 1.1 end-to-end checks (API against PostgreSQL + FreeRADIUS, browser smoke test) into repository tests, run on every push. | The billing and enforcement paths cost money if they break. | M |
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
