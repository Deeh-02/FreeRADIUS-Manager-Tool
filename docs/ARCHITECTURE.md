# FreeRADIUS Manager — Architecture and Operations Guide

This guide explains what the system is made of, how a request travels through it, how billing
works from plan to payment, what lives in the database, and how to run it. The last section,
[Known gaps and risks](#10-known-gaps-and-risks), tracks every issue found in the October 2026
review: what was fixed in version 1.1, and what is still open. **Read it before you charge a
customer.**

How the claims in this guide were checked is in [10.7](#107-what-was-and-was-not-verified).

**Contents**

1. [What this system is](#1-what-this-system-is)
2. [Plain-language glossary](#2-plain-language-glossary)
3. [The parts](#3-the-parts)
4. [How a request flows](#4-how-a-request-flows)
5. [Billing end to end](#5-billing-end-to-end)
6. [The database](#6-the-database)
7. [Users, roles and security model](#7-users-roles-and-security-model)
8. [How to run it](#8-how-to-run-it)
9. [Repository map](#9-repository-map)
10. [Known gaps and risks](#10-known-gaps-and-risks)

---

## 1. What this system is

A **network access control and billing tool for an internet or Wi-Fi provider**.

- Customers connect through your routers and access points. For each connection the router
  asks a **RADIUS server** (FreeRADIUS): "is this username and password allowed?" The answer
  is *yes* or *no*, possibly with extra instructions such as a speed limit.
- This project wraps FreeRADIUS in a **web application**. Staff use it to create customer
  logins, define plans and prices, issue invoices, record payments, see who is online, and
  manage the routers.
- The web application and FreeRADIUS **share one PostgreSQL database**. The application writes
  rows, and FreeRADIUS reads them when someone connects. The only direct traffic from the
  application to the network is test packets and **Disconnect requests** to routers
  ([4.4](#44-direct-paths-from-the-api-to-the-network)).

> **The key fact for a business owner:** since version 1.1 billing **controls access**. A
> customer on a paid plan gets online only while their *paid-until* date has not passed.
> Recording a payment moves that date forward, and reversing a payment moves it back. See
> [5.9](#59-what-blocks-a-customer).

---

## 2. Plain-language glossary

| Term | Meaning |
|---|---|
| **RADIUS** | The standard protocol routers use to ask a central server "may this user connect?" Runs over UDP: 1812 (login), 1813 (usage accounting); routers listen on 3799 for Disconnect requests. |
| **FreeRADIUS** | The open-source RADIUS server used here (version 3.2.10, in a container). |
| **NAS** | "Network Access Server": the device the customer connects to (router, access point, hotspot controller). It is the RADIUS *client* and shares a secret with the server. |
| **Shared secret** | A password known to both a NAS and the RADIUS server, protecting their messages. Not a customer password. |
| **Access-Request / Accept / Reject** | The question "may this user connect?" and the answer. |
| **Accounting** | Start / interim-update / stop messages from the NAS recording how long and how much data each session used. Stored in `radacct`. |
| **Session** | One connected device, from login until disconnect. |
| **radcheck / radreply** | The two tables FreeRADIUS reads at login. *radcheck*: conditions (password, "reject", expiry date, device limit, data cap). *radreply*: instructions sent back to the router (speed, fixed IP). |
| **Paid-until** | `radius_users.account_expiry`: the last day the customer has paid for. |
| **CoA / Disconnect-Request** | A RADIUS message telling a router to drop a connected user *now* (RFC 5176). |
| **Cleartext-Password** | The radcheck row holding the customer's network password, in plain text because PAP/CHAP/MS-CHAP need it. |
| **Plan** | A product you sell: price, validity in days, data cap, device limit, speed profile. |
| **Voucher** | A prepaid code that works as a login on its own. |
| **JWT** | A signed token the browser sends with each request to prove which staff member is signed in. |
| **bcrypt** | A deliberately slow, irreversible way of storing passwords. Used for staff passwords and the portal copy of customer passwords. |

---

## 3. The parts

```
                         ┌──────────────────────── Docker network ───────────────────────────┐
 Staff/customer browser  │                                                                    │
   ──(Caddy 443, opt.)──▶│ [frontend: nginx + Vue :8081] ──/api/──▶ [backend: Go API :8088] ─┐│
                         │                                                                   ▼│
                         │                                                [postgres: PostgreSQL 15]
                         │                                                                   ▲│
 Customer device ─Wi-Fi─▶ Router/AP (NAS) ──UDP 1812/1813──▶ [freeradius: FreeRADIUS 3.2.10] ┘│
                  ◀── UDP 3799 Disconnect-Request from the backend ────────────────────────────┘
        Published host ports: 8081 (web UI), 1812/udp, 1813/udp, 127.0.0.1:8088 (API, local only),
        80/443 when the https profile is on. PostgreSQL is not published.
```

| Part | Folder | Container | What it does |
|---|---|---|---|
| **Web UI** | [`frontend/`](../frontend) | `radius_frontend` (nginx + Vue 3) | Staff screens and the customer portal. nginx forwards `/api/` to the backend ([`nginx.conf`](../frontend/nginx.conf)). |
| **API** | [`backend/`](../backend) | `radius_backend` (Go + Gin) | Business logic, background jobs, backups (`pg_dump`), maintenance commands. Starts at [`main.go`](../backend/main.go). |
| **Database** | [`init-db.sql`](../init-db.sql), [`migrations/`](../migrations) | `radius_postgres` | App data *and* the tables FreeRADIUS reads. |
| **RADIUS server** | [`freeradius/`](../freeradius) | `radius_freeradius` | Answers logins from the database; records accounting into it. |
| **HTTPS (optional)** | [`Caddyfile`](../Caddyfile) | `radius_caddy` | Let's Encrypt certificate in front of the UI (`COMPOSE_PROFILES=https`). |
| **Your network gear** | — | — | Pointed at this server; see [`DEVICE-INTEGRATION.md`](../DEVICE-INTEGRATION.md). |

Inside the API (`backend/`):

| Folder / file | Role |
|---|---|
| `main.go` | Startup checks (refuses weak secrets), every route with its allowed roles, background jobs, maintenance commands (`access-report`, `resync-access`, `reset-password`, `reset-mfa`). |
| `internal/handlers/` | One file per feature. The shared core: [`access.go`](../backend/internal/handlers/access.go) (writes the RADIUS rows that enforce status, paid-until, devices, plan limits), [`billing_core.go`](../backend/internal/handlers/billing_core.go) (invoices, payments, reversals, plan assignment, promo codes — all inside the caller's transaction), [`radius_packet.go`](../backend/internal/handlers/radius_packet.go) (RADIUS packets, Disconnect-Request), [`backup.go`](../backend/internal/handlers/backup.go). |
| `internal/settings/` | Reads business settings from `system_settings` (30-second cache), falling back to environment variables, then defaults. |
| `internal/middleware/` | Rate limiting, session/API-key check, MFA-required check, role check, audit logging. |
| `internal/auth/` | Password hashing and rules, JWT, sessions. |
| `internal/scanner/` | `nmap` wrapper for finding routers on a subnet. |

**Background jobs** (inside the API process; set `RUN_WORKERS=false` on extra copies):

| Job | Interval | What it does |
|---|---|---|
| NAS monitor ([`nas_monitor.go`](../backend/internal/handlers/nas_monitor.go)) | 60 s | TCP-probes each active NAS, records up/down + latency, fires `nas.down`/`nas.up` webhooks. |
| Scheduler ([`scheduler.go`](../backend/internal/handlers/scheduler.go)) | polls every 10 min | Runs due tasks ([5.8](#58-scheduled-tasks)); each task holds a PostgreSQL advisory lock while it runs. |
| Alert worker ([`alerts.go`](../backend/internal/handlers/alerts.go)) | 5 min | Emails staff when a rule's condition is met (9 events, per-rule threshold and cooldown). |
| Credential-stuffing detector ([`security.go`](../backend/internal/handlers/security.go)) | 5 min | Scans failed logins, raises alerts, blocks IPs. |
| Honeypot ([`honeypot.go`](../backend/internal/handlers/honeypot.go)) | always | Decoy RADIUS listener on UDP 11812 (not published by default). |

---

## 4. How a request flows

### 4.1 Staff action: UI → API → database

Example: an admin adds a customer on a paid plan.

1. **Browser.** [`UserModal.vue`](../frontend/src/components/users/UserModal.vue) calls
   `POST /api/v1/radius/users` with `Authorization: Bearer <token>`
   ([`api/index.js`](../frontend/src/api/index.js)).
2. **nginx** forwards `/api/` to `backend:8088`, appending the client address to
   `X-Forwarded-For`.
3. **Gin** ([`main.go`](../backend/main.go)) runs:
   - the request logger, which masks query parameters
   - the 20 MB body cap and the rate limiter, keyed on the real client IP (only
     `TRUSTED_PROXIES` may set `X-Forwarded-For`)
   - **RequireAuth**: the signature must be valid, the session alive and not idle, the account
     active, the role taken from the database, and MFA set up if it's required
   - **AuditLogger**: changes are logged, with secrets redacted
   - **RequireRole**
4. **Handler** `CreateRadiusUser` ([`radius_users.go`](../backend/internal/handlers/radius_users.go))
   runs in **one transaction**:
   - inserts `radius_users` (bcrypt copy of the password) and the `Cleartext-Password` row
   - assigns the plan (`assignPlanTx`):
     - writes the limits (devices, data cap, speed)
     - creates the invoice
     - leaves the customer blocked until it's paid, unless `start_now`
   - calls `syncAccess`, which writes the enforcement rows
5. The response includes the invoice ID. The UI offers to record the payment straight away.

When the access token expires, the browser refreshes it once and retries. A revoked or invalid
session sends the user back to the login page.

### 4.2 Customer device: network → FreeRADIUS → database

1. A customer connects; the NAS sends an **Access-Request** to UDP 1812.
2. FreeRADIUS checks the sender against [`clients.conf`](../freeradius/clients.conf) (private and
   Docker ranges, fallback `RADIUS_SECRET`) and the `nas` table (each router's own secret,
   loaded at startup). `require_message_authenticator = auto` and `limit_proxy_state = auto`
   protect against forged packets (CVE-2024-3596).
3. **authorize** ([`sites-available/default`](../freeradius/sites-available/default)) runs the
   `sql` module with [`queries.conf`](../freeradius/mods-config/sql/main/postgresql/queries.conf):
   radcheck, radreply, group membership.
4. The checks are applied:
   - the password, against `Cleartext-Password`
   - `Auth-Type := Reject`, written when the customer is suspended or expired
   - `Expiration` (the paid-until date)
   - `Simultaneous-Use` (the device limit, counted from open accounting rows)
   - the **datacounter** (data cap since the paid period started) and **timecounter**
     (voucher online time) from [`mods-available/quota`](../freeradius/mods-available/quota)

   FreeRADIUS answers **Accept**, with reply rows such as the speed and a `Session-Timeout`
   for the remaining voucher time, or **Reject**.
5. The attempt is written to `radpostauth` (without the password).
6. The NAS sends **accounting** (start, interim, stop) to UDP 1813, written to `radacct` by the
   FreeRADIUS 3 accounting queries. This is the only source of "who is online" and "how
   much data", and the device-limit and data-cap checks depend on it.

### 4.3 What is enforced on the network

FreeRADIUS reads only `radcheck`, `radreply`, the group tables, `nas`, `radacct` and
`radius_users.period_start` (for the data-cap counter). Everything else becomes enforceable
only because the application writes matching rows (`syncAccess`, `applyPlanLimits`) in the
same transaction as the business change.

| Business rule | Written as | Enforced? |
|---|---|---|
| Customer password | `Cleartext-Password` | **Yes** |
| Suspended or expired status | `Auth-Type := Reject` | **Yes** (tested) |
| Paid-until date (+ grace days) | `Expiration := "Mon DD YYYY 00:00:00"` = midnight after the last paid day, FreeRADIUS local time (`TZ`) | **Yes** (tested) |
| Device limit | `Simultaneous-Use` | **Yes**, needs accounting (tested) |
| Data cap (plan or voucher) | `Max-Octets` → `datacounter` | **Yes at login** (tested); mid-session by the hourly *Disconnect Over-Quota Users* task |
| Voucher time limit | `Max-All-Session` → `timecounter`, replies `Session-Timeout` | **Yes** (tested) |
| Speed | `radreply` `Mikrotik-Rate-Limit` + WISPr | By the router, if it understands the attributes (MikroTik does) |
| Fixed IP from an IP pool | `radreply` `Framed-IP-Address` | Sent to the router (tested); the router must honour it |
| Kick a session now | Disconnect-Request to the NAS (UDP 3799) | If the router accepts CoA (tested against a FreeRADIUS CoA listener, not a real router) |
| Customer password expiry | `radius_users.password_expiry` | **No** (recorded only) |

### 4.4 Direct paths from the API to the network

- **Probes.** The RADIUS Test, NAS Test and Simulator screens send real Access-Requests (with
  Message-Authenticator) and verify the reply. `GET /health?full=1` sends a Status-Server.
- **Disconnect-Request** (`disconnectUserSessions` in
  [`radius_packet.go`](../backend/internal/handlers/radius_packet.go)): for each open session
  the backend sends RFC 5176 Disconnect to the session's NAS. It uses that NAS's own secret
  and CoA port, falling back to `RADIUS_SECRET` and 3799, and verifies the reply. The
  accounting row is closed when the router acknowledges. Callers:
  - the **Disconnect** button (with an optional *force*)
  - suspending a customer, and deleting or disabling a voucher
  - the expire and overdue tasks
  - the over-quota task
- **Customer portal** (`/portal`): customers sign in with their network username and password
  (checked against the bcrypt copy). Failed logins count toward a per-account lockout and the
  IP and GeoIP guard. A 24-hour token shows plan, paid-until date, usage, sessions and
  invoices. It is read-only.

---

## 5. Billing end to end

### 5.1 The lifecycle at a glance

```
 Plan ──assign──▶ Invoice (pending) ──payment(s)──▶ paid ──▶ paid-until += plan days ──▶ online
  │                    │  └─due date passes──▶ overdue (blocks only if postpaid + suspend_overdue)
  │                    └─ customer with no paid days stays blocked until paid (unless start_now)
  └─renewal task raises the next invoice N days before paid-until ──▶ (same loop)
 Reverse a payment ──▶ invoice re-opens ──▶ the days it bought are taken back
```

All of this is in [`billing_core.go`](../backend/internal/handlers/billing_core.go), and each
step runs in one database transaction together with the RADIUS rows it affects.

### 5.2 Plans — [`plans.go`](../backend/internal/handlers/plans.go)

A plan has:

- a name, a price and a currency
- validity in days
- an optional data cap (MB)
- a maximum number of devices
- an optional **bandwidth profile** (upload/download kbps, sent as MikroTik and WISPr
  attributes)
- an active flag

Editing a plan re-applies its limits to every customer on it. A plan can't be deleted while
customers are on it.

### 5.3 Assigning a plan — `POST /radius/users/:id/plan` (UI: RADIUS Users → Assign Plan)

`assignPlanTx`, in order:

1. Sets `plan_id` and writes the limits (device count, `Max-Octets` or none, speed).
2. A **free plan** extends paid-until straight away.
3. A **paid plan** gets an invoice for the plan price, less any promo code (redeemed
   atomically). If an invoice for this plan is already open, it's reused, not duplicated.
4. If the customer has **no current paid period**, they're blocked until the invoice is paid:
   paid-until is set to yesterday and the status to expired. With **start_now** the period is
   granted at once instead (postpaid), and the invoice is marked `service_applied`.

### 5.4 Invoices — [`billing.go`](../backend/internal/handlers/billing.go)

- **Statuses:** `pending`, `paid`, `overdue`, `cancelled`.
- **Numbers** come from a sequence: `INV-2026-000001`.
- **Becoming paid:** an invoice becomes `paid` only through payments. Editing it to `paid` is
  refused, so the money, the receipts and the paid-until date always agree.
- **Editing:** amount, due date and status can be edited while the invoice is open and nothing
  has been paid on it.
- **Deleting:** only invoices with no payments can be deleted; otherwise cancel them.
- **List page:** the summary shows collected and outstanding money **per currency**, plus the
  count in each status.

### 5.5 Payments and receipts — [`payments.go`](../backend/internal/handlers/payments.go)

`recordPaymentTx` records money received (cash, bank transfer, mobile money, card, online,
voucher, other):

- **Checks:**
  - the invoice must be open, and the currency must match
  - it refuses to overpay
  - `gateway_ref` (for example an M-Pesa code) is unique among confirmed payments, so the
    same transfer can't be recorded twice
- **Partial payments** are allowed.
- **When fully paid:** the invoice becomes `paid` and **`grantPlanPeriod`** runs. Paid-until
  becomes the later of the current paid-until date and today, plus the plan's validity. The
  data-cap period restarts. The number of days actually added is stored in
  `invoices.applied_days`.
- **Receipts:** each payment gets a receipt number from a sequence (`RCP-2026-000001`). The
  receipt is an escaped HTML page, fetched by the UI with the auth header.
- **Reversal** (`reversePaymentTx`, admin or super admin; operators cannot): the payment is kept and marked `reversed`
  with a reason. Its invoice re-opens and `applied_days` are taken back off paid-until.
  Payments are never deleted.
- **Webhooks:** `payment.received`, `invoice.paid` and `payment.reversed` fire.

### 5.5a M-Pesa — [`mpesa.go`](../backend/internal/handlers/mpesa.go)

Safaricom Daraja, configured by `MPESA_*` in `.env` (see `.env.example`); off until
`MPESA_CONSUMER_KEY` is set. Two ways in, both ending in `recordPaymentTx`:

- **Prompt (STK push).** Staff press *M-Pesa prompt* on an invoice (Billing), or the customer
  presses *Pay with M-Pesa* in the portal. A customer with nothing open gets an invoice for
  their plan's next period first. Their phone asks for the PIN, and Safaricom posts the result
  to `/api/v1/pay/result/<token>`. Each prompt is a row in `mpesa_stk` that becomes `paid`
  or `failed` exactly once (row lock), so a repeated callback changes nothing. If the callback
  never comes, a loop asks Safaricom every minute (STK query, also the *mpesa_reconcile* task).
  When the real M-Pesa code turns up later, it replaces the stand-in reference. One prompt per
  invoice per 2 minutes. M-Pesa takes whole shillings, so cents are rounded up and the extra is
  kept on the account.
- **Paybill/till (C2B).** The customer pays from their phone with their username (or an
  invoice number) as account. Safaricom posts to `/api/v1/pay/confirm/<token>`; each M-Pesa
  code is stored once in `mpesa_c2b`. The account is matched by invoice number, then username
  (any case), then a unique phone number. Unmatched money waits on the Payments page for an
  admin to *Give* it to a customer. It is never dropped. Settings → M-Pesa registers the
  addresses with Safaricom.

**Where the money goes:** the named invoice, then the customer's oldest open invoices, then new
invoices for their plan's next periods (3,000 on a 1,500 plan buys two months), then a payment
on their account. Payments carry gateway `mpesa` and the M-Pesa code, so the same code can't be
typed in again by hand. A code that was typed in already isn't recorded a second time.

**Trust:** Safaricom does not sign callbacks. Ours are accepted only with the secret
`MPESA_CALLBACK_TOKEN` in the path (wrong token: 404), optionally only from `MPESA_ALLOWED_IPS`.
A result is only applied to a prompt we sent. They are exempt from the per-IP rate limit
(Safaricom sends from few addresses). Paths avoid the words Daraja refuses in URLs ("mpesa",
"safaricom", "query", "sql", "exe", "cmd").

**Safaricom's limits** (measured against the real sandbox): its gateway takes 30 requests a
minute with bursts of 3 and answers more with 429 *Spike arrest*; its firewall (Incapsula)
blocks the server's address for every API, sign-in included, for 2–4 minutes after a few
refused requests in a short time, answering with an HTML 403 page. So the client spaces calls
1 s apart, retries a 429 after a pause (it never reached M-Pesa, so even a prompt is safe to
resend), and after a 403 block sends nothing for 2 minutes, telling staff so in plain words
(503). The reconcile loop asks about at most 20 prompts a run and stops at a block. Anything
Safaricom would refuse is checked here first (phone, amount, an `https://` callback address),
since each refusal counts towards a block. An unanswered prompt is reported three ways (500
`500.001.1001` with or without a message, and 200 with `ResultCode` 4999 "still under
processing"); all three mean "ask again".

Tested against a fake Daraja that checks every request the way Safaricom does
(`fake_daraja_test.go`, `mpesa_test.go`): paid, cancelled, timed out, callback lost,
repeated, early (before the push's own answer), and racing the reconcile query; forged
callbacks; Daraja down; tokens dropped; paybill by username, invoice number and phone; unknown
accounts and allocation; overpayment; codes already typed in; the portal; and a storm of 60
customers at once with real FreeRADIUS checked afterwards; Safaricom's 429s and firewall
block. The client was also run against the **real sandbox** (`sandbox_client_test.go`): sign-in,
a dropped token, prompts accepted, refused (amount, phone), the result query, 429s and a
firewall block. Then through a tunnel, on 23 October 2026, with Safaricom calling the backend:
a simulated paybill payment to a customer's username was applied within 3 seconds; one to an
unknown account waited on the Payments page; prompts to a real phone left unanswered came back
as 1037 "No response from user" and were marked failed with nothing recorded. **Not seen:** a
prompt paid with a PIN. The sandbox accepted the PIN but never completed the payment (no SMS,
no callback, its status query still "being processed" minutes later); the prompt correctly
stayed pending. That step can only be proven on the live paybill.

In live paybill notices Safaricom masks the payer's phone number, so the phone-number match
will usually find nobody there; customers should use their username (or invoice number) as the
account. The sandbox sends the number in full.

### 5.6 Promotions — [`promotions.go`](../backend/internal/handlers/promotions.go)

- **Discount:** a percent or a fixed amount.
- **Restrictions:** an optional plan, a usage limit and a validity window.
- **Applying a code:** given with an invoice or a plan assignment, it's redeemed in a single
  `UPDATE … RETURNING` that checks the limit, the dates and the plan. The discount is then
  stored on the invoice (`discount_amount`, `promo_code`).
- **Validate** only previews the discount.

### 5.7 Vouchers — [`vouchers.go`](../backend/internal/handlers/vouchers.go)

Codes are generated in batches of up to 500. Each code is a login (username = password). Its
rules are written as RADIUS rows:

- `Expiration` (validity)
- `Simultaneous-Use` (devices)
- `Max-Octets` (data)
- `Max-All-Session` (total online time)
- optionally a plan's speed

A voucher can have a price and a plan, recorded for your sales figures. Status: housekeeping
marks codes `used` after their first session and `expired` after validity. Disabling or
deleting a voucher also disconnects it.

### 5.8 Scheduled tasks

Rows in `scheduled_tasks`, run by [`scheduler.go`](../backend/internal/handlers/scheduler.go) and
managed on the **Scheduler** page. "Daily" means 24 hours after the last run.

| Task | Default | What it does |
|---|---|---|
| `expire_accounts` | daily | Active customers past paid-until + their grace days (own, else `billing_grace_days`) → `expired`, access rows rewritten, sessions disconnected, `user.expired` webhook |
| `mark_overdue_invoices` | daily | `pending` past due → `overdue`; if `suspend_overdue`, blocks customers whose overdue invoice was for a period already given (`service_applied`) |
| `renew_plans` ("Raise Renewal Invoices") | daily | For paid plans: an invoice due on the paid-until date, `renewal_invoice_days` ahead (never a free extension). Free plans are extended. |
| `expiry_reminders` | daily | SMS (phone) or email `reminder_days` before paid-until, once per period |
| `enforce_quota` | hourly | Disconnects online customers over their data cap |
| `voucher_housekeeping` | hourly | Marks vouchers used / expired |
| `cleanup_sessions` | hourly | Closes sessions with no accounting update for 24 h |
| `backup_database` | daily | `pg_dump` to `BACKUP_DIR`, prunes after `backup_retention_days` |
| `daily_report` | daily | Totals (payments per currency), emailed to `report_email` if set |
| `purge_logs` | weekly | Old auth logs, honeypot/webhook logs, expired portal sessions and tokens |

### 5.9 What blocks a customer

FreeRADIUS refuses a login when any of these is true:

- the customer is **suspended**
- the customer is **expired**, which the expire task, an unpaid plan assignment or a payment
  reversal all set
- the **paid-until** date (plus grace) has passed
- the customer is online on the maximum number of **devices**
- the **data cap** for the current period is used up
- a voucher's **time** is used up

Online sessions are kicked by Disconnect-Request when the customer is suspended or expires, and
hourly when they are over quota.

### 5.10 Revenue numbers

Payments and Billing both total **confirmed payments**, so the two pages agree. Every total is
split by currency.

### 5.11 What an operator does

1. Create plans (and bandwidth profiles).
2. Add customers on a plan, or assign a plan to existing customers.
3. When money arrives, **Record payment** against the customer's invoice. Their paid-until
   date moves, and they're online.
4. Let the scheduler raise renewal invoices and send reminders. Customers who don't pay drop
   off at midnight after their paid-until date plus grace.
5. Mistake or refund: **Reverse** the payment.
6. Download a backup now and then.

---

## 6. The database

PostgreSQL 15, created from these files, in order:

1. [`init-db.sql`](../init-db.sql): the base schema; can be re-run.
2. [`migrations/009_billing.sql`](../migrations/009_billing.sql): billing.
3. [`migrations/010_missing_schema.sql`](../migrations/010_missing_schema.sql):
   - the 20 tables the code used without any file creating them
   - the billing-enforcement columns, and sequences for invoice and receipt numbers
   - the `nasreload` table and wider accounting columns
   - indexes, triggers, setting seeds and scheduled tasks
   - `schema_migrations`

[`migrations/008_network_scanner.sql`](../migrations/008_network_scanner.sql) repeats tables
already in `init-db.sql`, for old installs.

All migration files can safely be re-applied. On a fresh Docker install they run from
`docker-entrypoint-initdb.d`. On updates, [`deploy-update.sh`](../deploy-update.sh) applies each
file once, in one transaction, and records it in `schema_migrations`.

### 6.1 Tables

**FreeRADIUS tables** (column names fixed by FreeRADIUS):

| Table | Purpose |
|---|---|
| `radcheck` | Per-user conditions: password, `Auth-Type := Reject`, `Expiration`, `Simultaneous-Use`, `Max-Octets`, `Max-All-Session`. Unique on (username, attribute). |
| `radreply` | Per-user reply attributes (speed, fixed IP). The application replaces rows by delete + insert. |
| `radgroupcheck`, `radgroupreply`, `radusergroup` | Groups. FreeRADIUS reads them; the UI does not manage them (use SQL). |
| `nas` | Routers with their own secrets, CoA port, ping status, zone/org labels. FreeRADIUS loads active rows at startup. |
| `radacct`, `nasreload` | Accounting (one row per session) and NAS reboot times (used by the device-limit count). |
| `radpostauth` | One row per login attempt and result (no passwords). |

**Application tables** (main groups):

| Group | Tables |
|---|---|
| Staff | `app_users` (roles, lockout, TOTP), `refresh_tokens` (sessions), `audit_log`, `api_keys` |
| Customers | `radius_users` (status, paid-until, period start, device limit, plan, phone, notes, portal lockout), `radius_user_password_history`, `portal_sessions` |
| Billing | `user_plans`, `invoices`, `payments`, `promotions`, `vouchers`, `bandwidth_profiles`, `scheduled_tasks` |
| Network | `ip_pools`, `ip_pool_assignments`, `hotspot_zones`, `captive_portals`, `radius_templates`, `network_scans`, `network_scan_hosts` |
| Notifications | `alert_rules`, `webhooks`, `webhook_logs`, `sms_logs` |
| Security | `security_alerts`, `cred_stuffing_blocks` (manual and automatic IP blocks), `geoip_rules`, `geoip_cache`, `honeypot_logs` |
| CRM | `customers`, `tickets`, `organizations` |
| Other | `system_settings` (business settings), `bulk_operations`, `schema_migrations` |

**How they relate:**

- `radius_users.plan_id` → `user_plans`
- `invoices.user_id` / `plan_id` → the customer and the plan
- `payments.invoice_id` → `invoices`

Deleting a customer, plan or invoice sets these links to NULL rather than deleting history.
Invoices and payments also copy the username, so they stay readable afterwards. Customers are
tied to `radcheck`, `radreply` and `radacct` by **username text**.

---

## 7. Users, roles and security model

### 7.1 Staff roles

| Role | Can do (route table in `main.go`) |
|---|---|
| `operator` | View customers, sessions, NAS, plans, invoices, payments, reports, vouchers, zones; reset a customer's password; tickets; validate promo codes; acknowledge alerts. |
| `admin` | Plus: create/edit/delete customers, NAS, plans, invoices, record and **reverse payments**, vouchers, profiles, pools, templates, promotions, zones, portals; suspend/activate/disconnect; bulk operations; scanner; simulator; IP blocks and GeoIP rules. |
| `super_admin` | Plus: staff accounts, settings, backups, API keys, alert rules, webhooks, organisations, scheduler changes. |

The UI hides pages by role; the **API enforces roles per route**, reading the role from the
database on every request (a demoted or disabled account loses access immediately).

### 7.2 Staff login and sessions

**Signing in:**

- **Password and MFA:** a bcrypt-checked password, then a TOTP code if MFA is on.
- **Throttling:** the IP-block and GeoIP guard, a per-account lockout (`brute_force_*`), and
  the credential-stuffing window.
- **Result:** a one-hour access token (JWT) and a seven-day refresh token that rotates on use.

**Staying signed in:**

- Every call checks that the session is alive, not idle beyond `session_timeout`, and that the
  account is active.
- Several devices can be signed in at once.

**Password and MFA rules:**

- Passwords need the configured minimum length (default 12) plus upper case, lower case, a
  digit and a symbol. The setup wizard applies the same rules.
- With **Require MFA** on, accounts without MFA can only use their Profile page (where MFA is
  set up) until they enable it.
- Lost access is fixed on the server with `radius-manager reset-password` or `reset-mfa`.

**API keys** (`Authorization: ApiKey …`): a `read` key may only use GET routes at operator
level; a `write` key acts as admin. Tokens are never accepted in URLs.

### 7.3 Customer access

Customers never sign in to the staff app. They have their network credentials and, optionally,
the read-only `/portal`.

### 7.4 Secrets and settings

- **Infrastructure and secrets** come from `.env`: database credentials, `JWT_SECRET`,
  `RADIUS_SECRET`, `SETUP_TOKEN`, SMTP, SMS, `TZ`, and proxies. The backend and the FreeRADIUS
  container refuse to start with empty or published example secrets, unless
  `APP_ENV=development`.
- **Business rules** live in `system_settings` and are edited on the Settings page, taking effect
  within 30 seconds:
  - billing, reminders and the daily report
  - staff password length, idle sign-out, lockout, required MFA
  - device-limit ceiling, customer portal lockout, credential-stuffing thresholds
  - backup retention

  Where a setting also has an environment variable, the database value wins.

---

## 8. How to run it

The [README](../README.md) has the step-by-step for installing, connecting routers, updating,
backups and troubleshooting. This section adds the internals.

### 8.1 What you need

- **Docker** with the Compose plugin, `openssl` and `curl`.
- **Without Docker** (development only): PostgreSQL 14+, Go 1.21+, Node 18+.
- **Network:** UDP 1812/1813 reachable from your routers; the routers reachable on UDP 3799 from
  the server; TCP 8081 for the UI, or 80/443 with HTTPS.

### 8.2 Install

**Setup.** `./setup.sh` (on Windows, `setup.ps1`) writes `.env` from `.env.example`, filling in:

- cryptographically random database password, JWT secret and RADIUS secret
- a setup token
- the host timezone

It then builds the images, starts the stack and waits for every healthcheck. Compose refuses to
start at all if the three secrets are missing.

**First login.** The first-run wizard needs the setup token and creates the first
`super_admin`. It locks itself once any staff account exists.

### 8.3 Check that it works

```bash
docker compose ps                                   # all containers "healthy"
curl -s 'http://127.0.0.1:8088/health?full=1'       # database + FreeRADIUS (Status-Server)
docker compose logs -f backend freeradius
docker exec radius_backend ./radius-manager access-report
```

Then log in as a test customer from **Settings → RADIUS Test**.

### 8.4 Connect routers

See the README and [`DEVICE-INTEGRATION.md`](../DEVICE-INTEGRATION.md). In short:

- Register each router with its own secret, then restart FreeRADIUS.
- Turn on accounting with interim updates.
- Accept CoA on UDP 3799.

### 8.5 Local development without Docker

`./run-local.sh` creates a local database and applies all SQL (stopping on any error). It then
starts the API on `:8080` with `APP_ENV=development` and the UI on `:3000`. FreeRADIUS isn't
started.

### 8.6 Backups

**Daily:**

- The `backup_database` task writes `radius_backup_YYYYMMDD_HHMMSS.sql.gz` to `./backups`
  (`BACKUP_DIR=/backups` in the container).
- It prunes old dumps after `backup_retention_days`.
- Failures raise the `backup_failed` alert.

**By hand:** staff can start, download and delete backups on the Settings page. Restoring is a
deliberate manual step (README → Backups and restore). Copy backups off the server.

**Updates:** `deploy-update.sh` takes its own dump before updating.

**`docker compose down -v` deletes the database volume.**

### 8.7 Updating

`./deploy-update.sh` stops at the first error. It:

1. backs up the database
2. fast-forwards `DEPLOY_BRANCH`, refusing on local changes
3. builds the new images
4. shows `access-report` and asks for confirmation
5. applies pending migrations, each in one transaction
6. restarts the containers
7. runs `resync-access`

The running stack is untouched until step 6.

### 8.8 Configuration that matters

See [`.env.example`](../.env.example) for everything. Notable:

| Variable | Notes |
|---|---|
| `DB_PASSWORD` | Fixed into the database volume on first start; changing `.env` later doesn't change it inside PostgreSQL. |
| `JWT_SECRET` | 32 or more characters. Changing it signs everyone out (the UI then returns to the login page). |
| `RADIUS_SECRET` | Fallback secret for unregistered private-range routers and API probes. Recreate the FreeRADIUS container after changing it (`docker compose up -d`). |
| `TZ` | Same zone for every container; decides at which midnight access ends. |
| `TRUSTED_PROXIES` | Who may set `X-Forwarded-For` (default loopback + Docker ranges). |
| `RATE_LIMIT` | API requests per minute per client IP (default 300, fixed window). |
| `RUN_WORKERS` | `false` on additional backend copies. |

### 8.9 Tests

The repository has **no automated tests yet** ([Q1](#105-quality-and-operations)). Version 1.1
was checked with throwaway scripts against a real PostgreSQL and FreeRADIUS ([10.7](#107-what-was-and-was-not-verified));
turning them into repository tests and CI is planned work ([phase.md](phase.md)).

---

## 9. Repository map

```
.
├── backend/                  Go API  (module github.com/freeradius-manager/backend)
│   ├── main.go               startup checks, routes + roles, background jobs, maintenance commands
│   ├── internal/handlers/    one file per feature; access.go, billing_core.go, radius_packet.go,
│   │                         backup.go and maintenance.go are the shared core
│   ├── internal/settings/    business settings (database → env → default)
│   ├── internal/middleware/  rate limit, session/API-key check, role check, audit log
│   ├── internal/auth/        JWT, passwords, sessions
│   ├── internal/scanner/     nmap wrapper
│   ├── internal/models/      request/response structs
│   └── Dockerfile, Makefile
├── frontend/                 Vue 3 + Vite + Tailwind
│   ├── src/api/index.js      every API call, token refresh, live-stats stream, receipts
│   ├── src/router/index.js   pages and which roles may open them
│   ├── src/views/            one file per page
│   ├── src/components/       modals (users, billing, NAS), layout
│   └── nginx.conf, security-headers.conf, Dockerfile
├── freeradius/               RADIUS server config baked into the container
│   ├── sites-available/      default (logins, accounting) and inner-tunnel (PEAP/TTLS)
│   ├── mods-available/       sql (database), quota (data and time counters)
│   ├── mods-config/sql/main/postgresql/queries.conf   SQL FreeRADIUS runs (FreeRADIUS 3 format)
│   ├── dictionary            local attributes (Max-Octets, Max-All-Session, Octets-Remaining)
│   ├── clients.conf          which addresses may talk to the server
│   └── init.sh               fills in secrets at container start
├── init-db.sql               base schema + defaults
├── migrations/               008 network scanner, 009 billing, 010 missing schema + enforcement
├── docker-compose.yml        the containers (+ optional Caddy for HTTPS)
├── Caddyfile                 HTTPS front (https profile)
├── setup.sh / setup.ps1      first-time install (Linux / Windows)
├── deploy-update.sh          production update
├── run-local.sh              run without Docker (development)
├── .env.example              configuration template
├── README.md                 install, operation, billing rules
├── DEVICE-INTEGRATION.md, NETWORK_SETUP.md   router/AP set-up
└── docs/                     ARCHITECTURE.md (this file), phase.md (work plan)
```

---

## 10. Known gaps and risks

Each finding from the October 2026 review keeps its ID. Severities as before: **Critical** =
can lose money, data or control; **High** = will bite in normal use; **Medium**; **Low**.
**Status:** *Fixed* (in 1.1, with how it was checked), *Partly*, or *Open*.

### 10.1 Billing and revenue

| # | Severity | Finding | Status |
|---|---|---|---|
| B1 | Critical | Expiry and non-payment did not cut service. | **Fixed.** Paid-until → `Expiration`, expired/suspended → `Auth-Type := Reject`, sessions disconnected. Tested against FreeRADIUS. |
| B2 | Critical | `renew_plans` gave free renewals. | **Fixed.** It now raises invoices; days are added only by payment. Tested. |
| B3 | High | Payments and invoices were disconnected; Mark Paid made no payment; deleting payments left invoices paid. | **Fixed.** Payments against invoices (partial, no overpay, currency check, unique reference), paid only via payments, reversal instead of delete, one transaction. Tested (API + browser). |
| B4 | High | Receipts could not be opened; HTML not escaped. | **Fixed.** `html/template`, fetched with the auth header. Tested in browser. |
| B5 | High | Promotions never reduced an invoice; Apply unguarded. | **Fixed.** Atomic redemption on invoices and plan assignment. Tested. |
| B6 | High | No UI to assign a plan; reset expiry; duplicate invoices. | **Fixed.** Assign Plan dialog; extends from paid-until; reuses an open invoice; transactional. Tested. |
| B7 | High | No gateway, no customer invoice view, no reminders, daily report not delivered. | **Partly.** Portal shows invoices; SMS/email reminders; daily report email. **M-Pesa built** ([5.5a](#55a-m-pesa--mpesago)): prompts from staff and the portal, paybill/till payments, reconciliation, unmatched payments for staff. Tested against a fake Daraja, and against the real sandbox with Safaricom calling the backend (paybill paid and unmatched, prompts timed out). **Still to prove:** a prompt paid with a PIN (the sandbox never completes those), on the live paybill with a small amount. |
| B8 | Medium | Basic money handling. | **Partly.** Totals per currency, sequential invoice/receipt numbers, cents rounding. **Open:** amounts are `float64` in Go (stored as NUMERIC); no tax/VAT, proration, credit notes or line items. |
| B9 | Medium | Vouchers: no price/plan, no redemption, limits not enforced. | **Fixed.** Price/plan, used/expired housekeeping, expiry, data and time limits enforced. Tested. |
| B10 | Medium | Data caps not enforced. | **Fixed.** `datacounter` at login + hourly over-quota disconnect. Tested at login. |

### 10.2 Network and FreeRADIUS

| # | Severity | Finding | Status |
|---|---|---|---|
| N1 | High | Accounting queries were in FreeRADIUS 2 format, so **no accounting was recorded at all** (worse than first thought). | **Fixed.** FreeRADIUS 3 accounting queries. Tested (start/interim/stop). |
| N2 | High | IP assignment and templates used `ON CONFLICT` on a non-unique index. | **Fixed.** Replace by delete + insert in a transaction. Tested. |
| N3 | Medium | Disconnect did not disconnect. | **Fixed.** RFC 5176 Disconnect-Request with verified reply. Tested against a FreeRADIUS CoA listener; **not yet with a real router**. |
| N4 | Medium | Broad client trust; no Message-Authenticator requirement; shared secret everywhere. | **Partly.** `require_message_authenticator = auto`, `limit_proxy_state = auto`, per-NAS secrets with a generator, seed `testing123` NAS removed. **Open:** private ranges still accept the fallback secret; routers imported by the network scanner get `RADIUS_SECRET` until edited; a new NAS needs a FreeRADIUS restart. |
| N5 | Medium | FreeRADIUS logged customer passwords. | **Fixed.** `auth_badpass`/`auth_goodpass = no`; post-auth stores no password. |
| N6 | Medium | EAP/PEAP, groups, attribute formats untested. | **Partly.** Group-membership query added; `Expiration` format and time limits tested. **Open: EAP/PEAP untested.** |
| N8 | High | After any database restart FreeRADIUS kept refusing every customer for about 25 seconds more (`retry_delay = 30` in its connection pool). | **Fixed.** `retry_delay = 1`: back within 200 ms of PostgreSQL. Tested by crashing PostgreSQL under load. |
| N7 | Low | Port 18120 published to nothing; honeypot unpublished; TCP-only NAS monitor. | **Partly.** 18120 removed. **Open:** honeypot port not published (by design); NAS monitor still uses TCP probes, so a router that blocks them shows "down". |

### 10.3 Data and schema

| # | Severity | Finding | Status |
|---|---|---|---|
| D1 | Critical | 20 tables and 7 columns missing from the SQL. | **Fixed.** Migration 010. Tested on fresh install, upgrade from the old schema, and re-run (45 tables). |
| D2 | High | CSV import failed on every row. | **Fixed.** Per-row transactions, documented columns. Tested. |
| D3 | High | Usage report returned 500. | **Fixed.** `make_interval`. Tested. |
| D4 | High | No real backups. | **Fixed.** Daily `pg_dump`, retention, download/delete, failure alert. Tested (pg_dump 16 against the test database). |
| D5 | Medium | No migration system; errors ignored. | **Fixed.** `schema_migrations`, one transaction per file, stop on error; `init-db.sql` re-runnable. Tested with a deliberately broken migration. |
| D6 | Medium | Webhook event column type mixed; only two events sent. | **Fixed.** JSONB, ten events, retries, SSRF guard. |
| D7 | Low | Handlers ignored database errors and still answered 200. | **Partly.** Fixed in every handler rewritten for 1.1; not yet audited across all remaining handlers. |

### 10.4 Security

| # | Severity | Finding | Status |
|---|---|---|---|
| S1 | Critical | Secrets stored in clear. | **Partly.** NAS secrets masked for operators; logs and audit redact secrets. **Open:** customer network passwords (inherent to PAP/CHAP), TOTP and webhook secrets are plain in the database — protect the database and its backups. |
| S2 | Critical | Insecure defaults started silently. | **Fixed.** Compose, backend and FreeRADIUS refuse missing/example secrets; setup scripts generate strong ones (cryptographic generator on Windows too); no JWT fallback key in production. |
| S3 | High | Client IP spoofable through nginx; no portal lockout. | **Fixed.** Trusted proxies only; per-account portal lockout. Tested. |
| S4 | High | First-come setup wizard deleted admins and could reopen. | **Fixed.** Runs only while no staff account exists, optional `SETUP_TOKEN`, never deletes. Tested in browser. |
| S5 | Medium | Tokens accepted in `?token=` and logged. | **Fixed.** Header only; query strings masked in logs; live stats read with `fetch`. |
| S6 | Medium | Settings page and MFA-required were decorative; MFA setup overwrote secrets. | **Fixed.** Settings read from the database; MFA enforced; MFA setup page added; setup refused when already enabled. Tested in browser (enable + sign-in with code). |
| S7 | Medium | Admin features can probe internal networks; container runs as root. | **Partly.** Webhooks block private addresses (dial-time check) unless `WEBHOOK_ALLOW_PRIVATE`. **Open:** scanner/simulator are admin tools by nature; the API container runs as root with `NET_RAW` (needed by nmap and to write backups). |
| S8 | Medium | No TLS; CORS `*`; API port published. | **Fixed.** Optional Caddy HTTPS; CORS off unless `CORS_ORIGINS`; API bound to 127.0.0.1; nginx security headers on every response. Caddy **not yet run** here. |
| S9 | Medium | Organizations are not multi-tenant. | **Open.** User limits per organisation are enforced, but nothing isolates data. Do not give resellers staff accounts. |
| S10 | Medium | GeoIP, credential-stuffing blocks and API keys not enforced. | **Fixed** for staff and portal logins and API keys. Note: they do not filter RADIUS logins from routers. |
| B11 | Critical | An early renewal recorded the wrong number of days (the days still left plus one extra), so reversing that payment took back too many and cut off time the customer had paid for. | **Fixed** (found by the new tests). `grantPlanPeriod` read the old date inside the same SQL statement that changed it, which returned nothing. Covered by the concurrency and time-simulation tests. |
| B12 | Critical | The renewal task and staff "Assign plan" could each raise an invoice for the same customer at the same moment (check-then-insert with nothing behind it); in the test one customer ended with 31 open invoices. | **Fixed.** Both now lock the customer first and re-check. Not a unique index, because reversals legitimately re-open an invoice. Tested. |
| B13 | High | Retrying a cash payment (browser timeout, double click) recorded it twice; the same M-Pesa reference typed without a "gateway" could be recorded any number of times. | **Fixed.** `Idempotency-Key` on `POST /payments` (the Record Payment dialog sends one) and a reference index that treats an empty gateway and letter case properly (migration 011). Tested with 10 simultaneous copies. |
| B14 | High | Two backend copies could each run the same scheduled task, one after the other. | **Fixed.** A task is now claimed with one atomic `UPDATE` before it runs (15-minute lease if the claimer dies). Tested. |
| B15 | High | Cancelling or re-pricing an invoice while a payment landed on it could leave a cancelled invoice with money on it. | **Fixed.** The check and the change now happen in one transaction with the invoice locked. Tested. |
| B16 | Medium | Percentage discounts were a cent out on exact half-cents (33% of 25.50). | **Fixed.** Whole-cent arithmetic; checked against exact fractions for 440,000 price/discount pairs. |
| B17 | Medium | Editing a plan changed what its open invoices bought (shortening a plan to 7 days after an invoice for 30 days was raised). | **Fixed.** The invoice remembers its days (`plan_days`, migration 011). |
| B18 | Medium | Switching plans left the old plan's unpaid invoice open beside the new one. | **Fixed.** Unpaid, no-payment invoices for the old plan are cancelled automatically; invoices with money on them, or started on credit, are left for staff. |
| B19 | Low | With grace days, a brand-new unpaid customer's paid-until date was set to "yesterday", which grace then made look current. | **Fixed.** `blockUntilPaid` also subtracts the grace days. |
| B20 | Medium | Deleting a customer left their unpaid invoices open and unlinked, still counted as outstanding revenue; money on a part-paid invoice could be orphaned. | **Fixed.** Deleting cancels invoices nobody has paid on (the normal prepaid case). If an open invoice has money on it, or service was started on credit (*Start now*), the delete is **refused** (HTTP 409, with the invoice numbers) until staff settle it on purpose: reverse the payment to refund, collect the rest, or cancel a credit invoice to write it off. Suspend is the alternative. Tested. |
| B22 | Feature | Free trial for brand-new customers. | **Built.** Settings → *Free trial for new customers (days)*, default 5 (migration 014; 0 turns it off). A new customer on a paid plan is online until today + N days. Staff can override N per customer in the New customer and Assign Plan dialogs (*Free trial days*: blank = the Settings default, a number = that many days, 0 = none), so a trial can be given or refused case by case; the first invoice falls due then; each customer gets one trial (`trial_started`), changing plan does not restart it, and an existing paying customer never gets one. Paying during the trial keeps the unused trial days on top; reversing the payment takes back only the paid period. Migration 013. Tested, and exercised by the simulation. |
| B23 | High | A customer who used up the plan's data cap and paid for a new period the same day stayed refused until midnight: the cap counter started at the period's *date*. Counting from a timestamp instead would have been worse: `acctstarttime` comes from the router's clock, so a session on a router whose clock is slow would look older than the payment and never count. | **Fixed** (found by the real-FreeRADIUS test). The period remembers the newest usage row at the moment it is paid (`period_acct_floor`, migration 015); only rows after it count, whatever the routers' clocks say. FreeRADIUS `datacounter`, the quota task and the usage alerts all use it. A session already open at payment keeps going uncounted until it ends, which `Session-Timeout` makes happen by the old paid-until midnight. Tested with a router clock 10 minutes slow and with counters over 4 GB (gigawords). |
| B24 | High | When a router did not answer Disconnect (port 3799 firewalled, wrong secret), staff were never told the customer was still online, and the expiry task waited 3 s per session, one after another: 40 customers took 2 minutes, 1,000 would take about 50, past the task's 15-minute lease. | **Fixed.** Disconnects run 32 at a time (40 customers: 7 s). The task result, the Suspend/Delete/Edit messages and the bulk actions say who may still be online and what to check on the router; *Online now* marks such sessions "should be offline" and lists them first. New logins are refused in either case. Tested with a fake router that acknowledges, refuses or ignores. |
| B25 | Medium | Deleting a customer disconnected them (and force-closed their sessions in `radacct`) *before* checking whether the delete was allowed, so a refused delete (money unsettled) still kicked them off. Bulk delete and voucher delete did the same. | **Fixed.** The disconnect happens only after the delete has been committed. Tested. |
| B26 | High | M-Pesa: Safaricom answers a status query for a prompt the customer has not answered yet with HTTP 200 and `ResultCode` 4999 ("still under processing"). The reconcile loop took it as a final answer and marked the prompt **failed** while the customer could still be typing the PIN. If the success callback was then lost, nothing asked again: a customer who paid was never credited, and the screen told them the payment failed, inviting a second one. | **Fixed** (found against the real sandbox). 4999 and "under processing" mean pending. The fake Daraja now answers "not yet" in all three forms seen on the real one; `TestMpesaQueryStillWaiting`. |
| B27 | High | M-Pesa: the client could trip Safaricom's limits and lock the ISP out of M-Pesa for minutes. The reconcile loop asked about up to 50 prompts back to back (the gateway allows 30 a minute), a burst of prompts went out at once, and each refused request counts towards a firewall block of the server's address. During a block, sign-in failed with "check MPESA_CONSUMER_KEY/SECRET" and staff were shown Safaricom's raw HTML page. | **Fixed** (found against the real sandbox, which blocked us three times). Calls are spaced out, 429s retried, blocks waited out without sending, the reconcile loop capped and stopped at a block, refusals prevented by checking first, and messages say what happened ([5.5a](#55a-m-pesa--mpesago)). `TestMpesaSpikeArrest`, `TestMpesaFirewallBlock`; the fake checks the spacing during the 60-customer storm. |
| B28 | Medium | A plan could be created with a negative validity or device count (only 0 was replaced by the default): paying for it would set paid-until in the past. A price or validity beyond the column's range gave a 500. | **Fixed.** Validity 1–3,660 days, devices 1–1,000, price under 10,000,000,000, data cap not negative; on create and edit. Found by the route fuzzer. |
| B21 | Medium | Grace days were one system-wide number. | **Fixed.** Each customer can have their own (customer dialog → *Grace days*, blank = system default), so staff can extend one customer on request. Raising it lets a lapsed customer who has paid before back in at once; lowering it blocks them; it never lets in a customer who has not paid yet. Migration 012. Tested, and exercised by the simulation. |
| S11 | Low | Audit redaction shallow; GeoIP free plan non-commercial over HTTP. | **Partly.** Recursive redaction. **Open:** default GeoIP service is ip-api.com (non-commercial, HTTP) — set `GEOIP_API_URL`. |

### 10.5 Quality and operations

| # | Severity | Finding | Status |
|---|---|---|---|
| Q1 | High | No automated tests or CI. | **Partly.** Billing now has repository tests ([10.8](#108-automated-billing-tests)). **Open:** CI, and tests for the rest of the API, FreeRADIUS and the browser. |
| Q8 | High | Every billing step that reads a setting (grace days, due days) opened a second database connection while already holding one in a transaction. With enough simultaneous payments every connection waited for another, and the backend froze. | **Fixed.** Settings are held in memory and refreshed every 15 seconds; a lookup never touches the database. Found when 100 simultaneous requests hung the test run. |
| Q9 | High | A database blip (restart, network hiccup) signed every member of staff out at once: the session check could not reach the database and answered 401 "session expired or signed out", which the UI treats as a logout. | **Fixed.** It answers 503 "database not reachable, try again" (`DB_UNAVAILABLE`, `Retry-After`) and the session survives. Same for API keys. Tested by stopping PostgreSQL. |
| Q10 | High | Billing days come from the database's clock zone and the cut-off from FreeRADIUS's. With a managed database in UTC (Supabase) and a Kenyan ISP, payments between midnight and 03:00 were counted from yesterday and expiry ran 3 hours late; a FreeRADIUS left in UTC keeps customers online until 03:00. | **Fixed for the backend:** it sets its database sessions to `DB_TIMEZONE`/`PGTZ`/`TZ`, refuses an invalid zone name, and logs an error at startup if its date or UTC offset differs from the database's. FreeRADIUS must still be given the same `TZ` (Compose does). Tested with a fake clock across midnight (`midnight_test.go`). |
| Q11 | Medium | Not ready for a managed database: backups used `pg_dump` 16 (Supabase runs 17, which 16 refuses), dumped the provider's own schemas (unreadable, unrestorable), and FreeRADIUS had no way to use TLS or a password with quotes. | **Fixed.** `pg_dump` 17 in the image; `BACKUP_SCHEMA=public` dumps and restores as a non-owner; FreeRADIUS takes the password and `DB_SSL_MODE` through libpq's environment, `DB_POOL_MAX` caps its connections; `scripts/db-setup.sh` prepares any database and sets its timezone; the backend honours `PORT`. Tested: TLS-only login with a password full of quotes and backslashes; a Supabase-like database with an unreadable provider schema (`TestBackupManagedDatabase`). |
| Q12 | Medium | Bad input gave "server error" (500) instead of "bad request": a NUL character (`%00`) in any filter on six list pages, unknown or out-of-range values in template cloning, promotion and plan edits, the SMS expiry notice. And the audit row of `POST …/network/scans/hosts/:hostId/import-nas` was always lost: its route name is 51 characters and `audit_log.action` held 50. | **Fixed.** A middleware refuses NUL and invalid UTF-8 in paths, queries and JSON bodies with 400 (M-Pesa callbacks are cleaned instead, never refused); value errors from PostgreSQL map to 400/404/409; `audit_log.action` is 150 characters (migration 017). Found by `fuzz_e2e_test.go`: every route in `main.go`, 9,300 hostile requests, no 5xx, no hang, nothing hidden in the log. |
| Q2 | Medium | Compiled binary committed to git. | **Fixed** in the 1.1 commits (removed and ignored). |
| Q3 | Medium | Single-instance design. | **Partly.** Advisory locks on tasks, `RUN_WORKERS`. **Open:** rate limiter and lockout windows are per process; one PostgreSQL. |
| Q4 | Medium | `deploy-update.sh` could pull into a working branch and swallowed errors. | **Fixed.** Tested with a stand-in for Docker. |
| Q5 | Low | Inconsistent versions and docs. | **Fixed.** 1.1.0 everywhere; README and router guides rewritten. |
| Q6 | Low | UI rough edges. | **Fixed** (all listed items). |
| Q7 | Low | Dead code. | **Fixed.** Removed or now used. |

**Found and fixed during the 1.1 work** (not in the original review):

- **The API rate limiter only reset after a quiet minute,** so a dashboard left open got blocked
  after 100 requests.
- **Staff were signed out every hour in Docker:** the token refresh called `/api/v1/api/v1/…`.
- **An invalid or old-key token left the UI stuck** instead of returning to the login page.
- **There was no way to set up MFA in the UI,** so "Require MFA" locked everyone out.
- **Setup wizard:**
  - It accepted passwords the server then refused at the last step.
  - Its RADIUS secret field was ignored.
  - A click could miss the button, because the layout shifted as an error message cleared.
- **Wrong lists offered:** the webhook page offered events the server rejects, and the alerts
  page didn't offer all the events the server supports.
- **Payments page:** it failed completely for operators, because the revenue summary is
  admin-only.
- **Customer expiry:**
  - "Clear expiry" on a customer never cleared it.
  - Old expiry rows in ISO format **locked out paying customers**. `resync-access` rewrites them.
- **Captive portals:** a portal's router login URL couldn't be cleared.
- **Build and setup:**
  - A database password with a space or quote broke the connection.
  - The backend image could only build for amd64.
  - The Windows setup wrote `.env` with a BOM.
  - The local dev proxy sent the `/api-keys` page to the API.

### 10.6 Still to do before relying on it

1. **Try it on a test router:** MikroTik hotspot and PPPoE, interim accounting, Disconnect on
   3799, a speed profile. Also try the real Docker images, including the Caddy HTTPS
   profile.
2. **Decide your billing rules:**
   - prepaid or postpaid
   - grace days
   - renewal lead time
   - whether overdue invoices block service

   Set them on the Settings page.
3. **Add CI** (Q1) to run the billing tests ([10.8](#108-automated-billing-tests)) on every push, and extend them to the API, FreeRADIUS and the browser.
4. **M-Pesa against the real sandbox** (B7), then production keys: [CLOUD-TEST.md](CLOUD-TEST.md).
5. **Tax/VAT and exact-decimal money** (B8), if your jurisdiction needs invoices with tax.
6. **Real multi-tenancy** (S9), if you plan to resell.

### 10.7 What was and was not verified

**Executed for 1.1** (throwaway scripts outside the repository, against real services):

- **Build:** `go build` and `go vet`; a production `vite build`.
- **FreeRADIUS 3.2.5** (Debian package; the image uses 3.2.10, same configuration format)
  with the repository's configuration and PostgreSQL 16. 19 checks:
  - password, suspend, expiry and its date format
  - device limit, data cap, voucher time limit
  - accounting start, interim and stop
  - reply attributes, Status-Server
  - no passwords in the logs
- **API end to end:** 84 checks through HTTP:
  - setup and the security rules
  - customers, plans, invoices, partial payments, reversal (including the exact days taken
    back), promo codes, vouchers
  - CSV import, backups, webhooks, API keys, MFA requirement, captive portals
- **Disconnect-Request:** acknowledged and refused replies, against a FreeRADIUS CoA listener.
- **Migrations:** fresh install, upgrade from the 1.0 schema, re-run, and a failing migration
  rolled back.
- **`deploy-update.sh`:** run against the old schema through a stand-in for `docker`.
  Confirmation, migrations, re-sync and the failure path all checked.
- **Maintenance commands:** `access-report`, `resync-access`, `reset-password`, `reset-mfa`.
- **Browser** (headless Chromium):
  - every page loads with no script errors
  - billing: assign a plan, record a payment, open a receipt
  - backups, saving settings, alert and webhook event lists
  - MFA: enable it, then sign in with a code
  - tokens: refresh after expiry, and return to login on an invalid one
  - the setup wizard on an empty database with a setup token

**Executed since** (in the repository, rerun with `make test-e2e` and `make test-load`; see
[10.8](#108-automated-billing-tests)): the real FreeRADIUS and the real backend binary on one
4-core laptop, with crashes and outages, and the load tests below.

**Not run here:**

- **Docker:** no daemon was available, so the images, healthchecks, Compose file, nginx config
  and Caddy weren't run.
- **Real routers** (a fake one stands in for Disconnect).
- **Protocols and providers:** EAP/PEAP; real SMTP, SMS, webhook receivers and GeoIP; nmap
  scans.

### 10.8 Automated billing tests

The first tests in the repository, in `backend/internal/handlers/*_test.go`. They run against
a real PostgreSQL with the repository's schema (`scripts/test-db.sh` makes a throwaway one) and
skip themselves when `TEST_DATABASE_URL` is not set. Run them all with `make test-integration`
in `backend/` (about 2 minutes).

| File | What it does |
|---|---|
| `concurrency_test.go` | Fires the same actions at the same instant, then checks the books: 20 or 30 partial payments on one invoice; one M-Pesa reference sent 10 times (with and without a gateway); paying and reversing the same invoice together (80 rounds); 100 customers on a promo limited to 10; the renewal task against staff assigning plans; two backend copies running the scheduler; 200 parallel invoice/receipt numbers; one payment retried 10 times with the same idempotency key; cancelling an invoice while it is being paid. |
| `sim_test.go` | **Time simulation.** Moves every date in the database back one day, runs the daily tasks, and repeats (`SIM_DAYS`, `SIM_CUSTOMERS`, `SIM_SEED`). Customers pay on time, late, in part or never; some are reversed, switch plans, use promo codes (33%, 100%, fixed, bigger than the price), have their plan's price changed under an open invoice, or are deleted while owing. After every simulated day it checks the invariants below. |
| `invariants_test.go` | The rules checked after every day: payments never exceed an invoice; paid means fully paid and its period granted exactly once; numbers unique; **what FreeRADIUS would decide from `radcheck` matches the books for every customer** (no paying customer blocked, no non-paying customer allowed); paid-until equals the starting point plus the days of the invoices that still hold a period; one open renewal per customer; revenue per currency equals the tally of operations performed, and the Billing and Payments pages agree. |
| `edge_test.go` | 1,000 payments of 0.01; discount rounding against exact fractions; fully discounted and free plans; the midnight `Expiration` text across month ends, the year end and 29 February; a plan edited under an open invoice; deleting a customer who owes; a payment that fails half-way leaves nothing behind. |
| `trial_delete_test.go` | Free trials (default and per-customer length, one per customer, paying during a trial) and the delete rule for unsettled money. |
| `radius_e2e_test.go` | **Real FreeRADIUS** (`scripts/test-radius.sh` builds the stock 3.2 config plus this repository's files, as the Docker image does, and runs it as your user). One customer from trial to deletion with FreeRADIUS asked after each step (trial, unpaid, paid, `Session-Timeout` ends at paid-until midnight, grace given by staff, suspend, reversal, delete); the device limit from accounting packets, including a router reboot (Accounting-On); the data cap, gigawords, paying again after running out, and a router whose clock is slow. With FreeRADIUS running, the time simulation also asks it about every customer every day (120 days × 300 customers ≈ 31,000 real logins, all matching the books). |
| `disconnect_test.go`, `fake_nas_test.go` | A fake router that acknowledges, refuses or ignores Disconnect-Request (and checks its authenticator). Expiry, suspend and delete with a silent router; a refused delete leaves the customer online; the NAS's own secret is used. |
| `stack_e2e_test.go` | **The whole system** (`scripts/test-stack.sh`: PostgreSQL, FreeRADIUS and the backend binary in production mode). A customer journey through HTTP only, including roles, a retried payment, a duplicate M-Pesa code, a tampered token and logout. **Failure tests:** PostgreSQL killed mid-burst of payments (A.7); the backend killed with `kill -9` mid-payment, 15 times (A.10); FreeRADIUS down while a customer pays and another expires (A.8); PostgreSQL down, then what routers, staff and recovery look like. |
| `load_test.go` | Load (A.11–A.15), only with `LOAD_TEST=1`. Results below. |
| `midnight_test.go` | FreeRADIUS with a fake clock (libfaketime) across midnight: the cut-off happens at 00:00 in its zone, a session online at 23:59:50 ends at midnight, and one left in UTC keeps a Kenyan customer online until 03:00. |
| `backup_restore_test.go` | A.4 (above), and `TestBackupManagedDatabase`: a Supabase-like database where the login owns neither the database nor `public`. |
| `mpesa_test.go`, `fake_daraja_test.go` | M-Pesa ([5.5a](#55a-m-pesa--mpesago)). |
| `fuzz_e2e_test.go` | Every route in `main.go` (184) with hostile bodies, path parameters and query values, one at a time, as a super admin: no 5xx, no hang, nothing logged as an error, books intact. |
| `sandbox_client_test.go` | Our Daraja client against the **real Safaricom sandbox**, no backend needed (`MPESA_SANDBOX_CLIENT=1` and the app's keys). Mind the firewall: run the tests one at a time. |
| `cloud_test.go` | The real Safaricom sandbox through a deployed backend ([CLOUD-TEST.md](CLOUD-TEST.md)); skipped unless asked for. |

**Failure test results:** after a PostgreSQL crash with 24 payments in flight, every client
retry (same idempotency key) was settled, each payment is in the books once, no days without a
payment, FreeRADIUS accepting again within 200 ms. With the backend killed mid-request 15 times
(103 of 120 payments in flight), the same. With PostgreSQL down, routers get Access-Reject,
staff get 503 and keep their session, and both recover with nothing restarted.

**Load test results** on a 4-core laptop running PostgreSQL, FreeRADIUS and the load generator
together (a server will do better):

| Test | Result |
|---|---|
| Logins, 5,000 customers, 64 / 256 clients | 500 / 770 per second, none lost, none wrong; accepted p99 85 ms / 386 ms. Rejects take 1 s on purpose (`reject_delay`). |
| Accounting: 5,000 sessions × start, 4 interims, stop | 1,750 packets/s (5,000 online customers send about 17/s); every update stored, final byte counts exact. |
| 2 million usage rows | Logins of capped customers p95 40 ms; dashboard 0.2 s, daily usage report 0.3 s, other pages under 0.05 s; quota task 3.6 s. |
| Daily tasks over 5,000 customers, staff working | Expire 625 in 4 s, raise 500 renewals in 4.6 s (alone); with 8 staff clients at full speed no lock waits, no errors, staff latency unchanged. |
| 100,000 invoices and payments | Billing and Payments pages 50–350 ms (slowest: page 2,000 of the paid invoices). |

Ran once (not in the normal run, which uses 120 days and 300 customers): 560 simulated days
with 500 customers, crossing 29 February 2028, with real FreeRADIUS asked about every customer
every 7th day (34,454 logins, all matching the books; 13 minutes). Before the fixes above, these tests failed on
B11, B12, B13, B14, B15, B16, B17, B18 and Q8; each fix was confirmed by re-running them.

Before the fixes, these tests found B23, B24, B25, B28, N8, Q9, Q10, Q11 and Q12. Checking
against the real sandbox found B26 and B27, and that Daraja reports an expired token as HTTP
404 (`404.001.03`), not 401; the client now signs in again on either.

**Not covered yet:** the Docker images, a real router, a prompt paid with a PIN (the sandbox
never completes those; prove it on the live paybill), the Render/Supabase deployment, and the
load numbers on your own server.
