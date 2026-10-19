# Work plan by phase

This is the to-do list for getting FreeRADIUS Manager ready to bill real customers. It is built from
the review in [ARCHITECTURE.md § 10 "Known gaps and risks"](ARCHITECTURE.md#10-known-gaps-and-risks).
The codes in brackets (B1, S2, …) point to the matching rows there, which give the detail and the
evidence.

**Where things stand today:** the network side works. Customers can log in through FreeRADIUS and
get online. The business side does not. Nothing links money to access, so a customer who stops
paying stays online.

Sizes are rough: **S** = hours, **M** = a few days, **L** = a week or more.

---

## Phase 0: decide and check (before writing any code)

These answers decide how much work Phases 1 and 2 involve.

- [ ] **Check the live database for the 20 missing tables** listed in
      [ARCHITECTURE.md § 6.2](ARCHITECTURE.md#62-tables-the-code-uses-that-no-sql-file-creates-20).
      If they exist, export their definitions (`pg_dump --schema-only`) so they can be added to the
      repo. If not, the customer portal login and about a dozen screens are broken.
- [ ] **Decide the non-payment rule:** cut off on the expiry date, or allow a grace period of N days?
      Is there a warning before cut-off (SMS or email), and how many days before?
- [ ] **Decide how customers pay:** cash or bank transfer recorded by staff, mobile money, or card.
      This decides whether a payment-gateway integration is needed (B7).
- [ ] **Decide currency and tax:** one currency or several, and whether invoices need tax/VAT (B8).
- [ ] **Decide what renewal means:** does a new period start from the old expiry date (extend) or
      from the payment date (reset)? Today it always resets (B6).

**Done when** the answers are written down, here or in an issue, and agreed.

---

## Phase 1: must fix before the first paying customer

| # | Task | Why | Size |
|---|---|---|---|
| 1.1 | **Cut access when a plan expires or an invoice goes overdue** [B1]. When the scheduler marks an account expired or overdue, also write `Auth-Type := Reject` (or `Expiration`) to `radcheck`, and remove it on payment. | Without this, billing is only a record and non-payers stay online. | M |
| 1.2 | **Link payment → invoice → renewal** [B3, B6]. Recording a payment against an invoice marks it paid and extends the customer's plan in one database transaction. Add an invoice selector to the Payments form and a "Mark Paid" that records a payment. Reject payments on cancelled or already-paid invoices. | Today staff update three places by hand and they drift apart. | M |
| 1.3 | **Add an "Assign plan" button** to the user screen [B6]. It must not create a duplicate invoice each time it is used. | No screen can do this today; the API exists but nothing calls it. | S |
| 1.4 | **Fix receipts** [B4]. Make the receipt link work from the UI (it returns 401 today) and escape customer data in the receipt HTML. | Customers and staff cannot open receipts. | S |
| 1.5 | **Create the missing tables and columns** [D1, D2] in a new migration, then click through every screen on a fresh database. | A new install, or a rebuild after a server failure, otherwise comes up broken. | M (S if Phase 0 finds them in the live DB) |
| 1.6 | **Lock down secrets** [S2, N5]. Refuse to start without a real `JWT_SECRET`, database password and RADIUS secret; remove the `testing123` seed NAS; set `auth_goodpass`/`auth_badpass = no` in `radiusd.conf`; switch `setup.ps1` to a cryptographic generator. | Anyone who knows the built-in JWT key can log in as admin. | S |
| 1.7 | **Real backups** [D4]. Run a scheduled `pg_dump`, copy it off the server, and do one test restore. Hide or remove the fake Backup button. | The Backup button does nothing, so a server failure loses all customer and payment data. | S–M |
| 1.8 | **HTTPS, and close port 8088** [S8]. Put TLS in front of nginx and stop publishing the API port directly. Restrict CORS to your own domain. | Logins and tokens currently travel unencrypted. | S |
| 1.9 | **Secure the setup wizard** [S4]. Make sure it cannot be re-run on a live system. | Re-running it deletes all admins and lets whoever gets there first take over. | S |

**Done when:** on a fresh install, a test customer is created, assigned a plan, invoiced and paid,
and can connect. When their plan lapses unpaid they are refused by FreeRADIUS. When they pay again
they reconnect. A backup has been restored successfully at least once.

---

## Phase 2: soon after launch

| # | Task | Why | Size |
|---|---|---|---|
| 2.1 | **Fix client-IP handling behind nginx** [S3]: set Gin's trusted proxies and have nginx overwrite `X-Forwarded-For`. Add a per-account lockout on the customer portal. | The login rate limit can currently be bypassed by faking the IP. | S |
| 2.2 | **Tests for billing and enforcement** [Q1], plus a CI job that runs them on every push. | These paths cost money if they break, and there are no tests today. | M |
| 2.3 | **Verify three likely bugs on a running stack** [N1, N2, D3]: interim accounting (`frameipaddress` column typo), fixed-IP / template assignment (`ON CONFLICT` on a non-unique index), and the usage report (`INTERVAL $1`). | If N1 is real, active users get disconnected after 24 hours and usage figures are wrong. | S–M |
| 2.4 | **Fix the CSV user import** [D2]. | It currently fails on every row. | S |
| 2.5 | **Make "Disconnect" actually disconnect** [N3] by sending a CoA/Disconnect packet to the router. Also apply it on cut-off from 1.1, so a blocked customer is kicked off immediately rather than at their next login. | Today it only edits the database record. | M |
| 2.6 | **Enforce data caps** [B10], if you sell plans with a GB limit: add a FreeRADIUS counter (`sqlcounter`) module. | A "5 GB" plan is currently unlimited. | M |
| 2.7 | **Customer-facing billing** [B7]: payment reminders, invoices visible in the portal, and the payment gateway chosen in Phase 0. | Customers have no way to see or pay what they owe. | L |
| 2.8 | **Stop hiding errors** [D7]: handlers that ignore database errors should return an error, not "saved". | Failures currently look like successes. | M |
| 2.9 | **Tighten RADIUS client trust** [N4]: give each NAS its own secret, set `require_message_authenticator = yes` where routers support it, and narrow the trusted IP ranges. | Any device in a private range that knows the one shared secret is accepted. | S–M |

**Done when** the tests run in CI, N1/N2/D3 are confirmed fixed or confirmed not to be bugs, and
customers receive reminders before being cut off.

---

## Phase 3: later, or don't advertise until done

- [ ] **Promotions** [B5]: they never reduce an invoice. Fix them or hide the page.
- [ ] **Vouchers** [B9]: add a price or plan, track redemption, and enforce expiry and caps.
- [ ] **Money handling** [B8]: store amounts as integers or decimals instead of floats, use
      sequential invoice numbers, add refunds and credit notes.
- [ ] **Reseller/organisation accounts** [S9]: they are not isolated from each other. Do not sell them as
      separate tenants until they are.
- [ ] **Security features that look active but are not** [S10]: GeoIP rules, credential-stuffing
      blocks, API keys. Wire them up or remove them from the UI.
- [ ] **Settings page** [S6]: make the options actually take effect (or remove them); allow MFA to
      be enforced.
- [ ] **Tokens** [S5]: stop accepting `?token=` on every route and stop logging query strings.
- [ ] **Server-side requests** [S7]: restrict webhook URLs and scanners from reaching internal
      networks; run the API container as non-root.
- [ ] **Webhooks** [D6]: fix the `events` column handling and send the events the UI offers.
- [ ] **Migrations** [D5, Q4]: adopt a migration tool, and make `deploy-update.sh` stop on errors.
- [ ] **Scaling** [Q3]: move background jobs and rate limiting out of the API process before
      running more than one copy.

## Clean-ups (small, any time)

- [ ] Remove the committed 12.7 MB binary `backend/radius-manager` and add it to `.gitignore` [Q2].
- [ ] UI rough edges [Q6]: duplicate "Import CSV" button, the "Send Test Email" button on Settings
      (it does nothing), the Network Map token key (`token` should be `access_token`), the three
      Alert Rules events that never fire, `Math.random` in the NAS secret generator.
- [ ] Pass the `SMS_*` variables through `docker-compose.yml`, and remove unused settings
      (`ADMIN_IP_WHITELIST`, `BACKUP_*`) or implement them.
- [ ] Remove dead code [Q7] and align version strings and ports across files [Q5].
- [ ] Audit `README.md`, `DEVICE-INTEGRATION.md` and `NETWORK_SETUP.md` against the code.

---

## Recommended order

1. Phase 0.
2. Task 1.6 (secrets): small, low-risk, and closes the most dangerous hole.
3. Tasks 1.1–1.4 together, since enforcement and payments are one feature: a paid customer gets
   online and an unpaid one doesn't.
4. The rest of Phase 1, then Phase 2 in table order.
