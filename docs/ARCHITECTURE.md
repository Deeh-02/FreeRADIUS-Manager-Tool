# FreeRADIUS Manager — Architecture and Operations Guide

This guide explains what the system is made of, how a request travels through it, how billing
works from plan to payment, what lives in the database, and how to run it. The last section,
[Known gaps and risks](#10-known-gaps-and-risks), lists what is unfinished, untested or unsafe.
**Read that section before you charge a customer.**

How the claims in this guide were established is stated in
[10.7](#107-what-was-and-was-not-verified). In short: everything here comes from reading the
source, SQL and configuration; only a handful of checks were actually executed, and the
application was never run end to end (no database or Docker was available).

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

A **network access control and customer management tool for an internet or Wi-Fi provider**.

- Your customers connect through your routers and access points. Each time a device tries to
  join, the router asks a **RADIUS server** (here, FreeRADIUS): "is this username and password
  allowed?" The answer is *yes* or *no*, optionally with extra instructions such as a speed
  limit.
- This project wraps FreeRADIUS in a **web application**. Staff use it to create customer
  logins, define plans and prices, issue invoices, record payments, watch who is online, and
  manage the routers.
- The web application and FreeRADIUS **never talk to each other directly**. They share one
  PostgreSQL database. The application writes rows; FreeRADIUS reads them when someone tries to
  connect. That single fact explains most of how the system behaves — and most of its gaps
  (see [4.3](#43-where-the-two-sides-meet-what-is-actually-enforced)).

> **The most important thing to understand as a business owner:** billing in this application
> is **bookkeeping** (plans, invoices, payments, reports). It records money; it does **not**, by
> itself, switch a customer off when they do not pay or when their plan ends. Only an explicit
> *Suspend* does that. See [5.9](#59-what-actually-blocks-a-customer).

---

## 2. Plain-language glossary

| Term | Meaning |
|---|---|
| **RADIUS** | The standard protocol routers use to ask a central server "may this user connect?" Runs over UDP, ports 1812 (login) and 1813 (usage accounting). |
| **FreeRADIUS** | The open-source RADIUS server used here (version 3.2.10, in a container). |
| **NAS** | "Network Access Server" — the device the customer actually connects to: a router, access point or hotspot controller. It is the RADIUS *client*. Each NAS shares a secret password with the RADIUS server. |
| **Shared secret** | A password known to both a NAS and the RADIUS server, used to protect their messages. Not a customer password. |
| **Access-Request / Accept / Reject** | The question "may this user connect?" and the answer. |
| **Accounting** | Start / interim-update / stop messages the NAS sends so the server can record how long and how much data each session used. Stored in `radacct`. |
| **Session** | One connected device, from login until disconnect. |
| **radcheck / radreply** | The two database tables FreeRADIUS reads at login. *radcheck* = conditions to check (the password, "reject this user", an expiry date). *radreply* = instructions sent back to the router (speed limit, fixed IP). |
| **Cleartext-Password** | The radcheck row holding the customer's real network password. Stored in plain text because the common login methods (PAP, CHAP, MS-CHAP) need it. |
| **Plan** | A product you sell: price, validity in days, data cap, device limit, speed profile. |
| **Voucher** | A prepaid code that works as a login on its own. |
| **JWT** | A signed token the browser sends with each request to prove which staff member is logged in. |
| **SSE** | "Server-Sent Events": a one-way stream from server to browser, used for live dashboard numbers. |
| **bcrypt** | A deliberately slow way of storing passwords that cannot be reversed. Used for staff passwords. |
| **CoA / Disconnect-Request** | A RADIUS message that tells a router to drop a connected user *now*. The application does **not** send these (see [10.2](#102-network-and-freeradius)). |

---

## 3. The parts

```
                         ┌──────────────────────── Docker network ───────────────────────────┐
                         │                                                                    │
 Staff browser ──8081──▶ │ [frontend: nginx + Vue app] ──/api/──▶ [backend: Go API :8088] ──┐ │
                         │                                                                  ▼ │
                         │                                                        [postgres: PostgreSQL 15]
                         │                                                                  ▲ │
 Customer device ─Wi-Fi─▶ Router/AP (NAS) ──UDP 1812/1813──▶ [freeradius: FreeRADIUS 3.2.10] ┘ │
                         └────────────────────────────────────────────────────────────────────┘
        Host ports published: 8081 (web UI), 8088 (API, directly), 1812/udp, 1813/udp, 18120/udp.
        PostgreSQL is not published to the host.
```

| Part | Folder | Container | Ports | What it does |
|---|---|---|---|---|
| **Web UI** | [`frontend/`](../frontend) | `radius_frontend` — nginx serving a Vue 3 app | 8081 → 80 | The staff screens. nginx also forwards every `/api/*` request to the backend ([`nginx.conf`](../frontend/nginx.conf)). |
| **API** | [`backend/`](../backend) | `radius_backend` — Go + the Gin web framework | 8088 | All business logic: staff login, RADIUS users, NAS devices, plans, invoices, payments, reports. Also runs the background jobs. Starts at [`main.go`](../backend/main.go). |
| **Database** | [`init-db.sql`](../init-db.sql), [`migrations/`](../migrations) | `radius_postgres` — PostgreSQL 15 | 5432 (internal) | Everything: app data *and* the tables FreeRADIUS reads. |
| **RADIUS server** | [`freeradius/`](../freeradius) | `radius_freeradius` — FreeRADIUS 3.2.10 | 1812/udp (login), 1813/udp (accounting) | Answers login requests from routers using the database; records usage into the database. |
| **Your network gear** | not in this repo | — | — | The routers/APs customers connect to. Pointed at this server's IP, ports 1812/1813 and the shared secret. See [`DEVICE-INTEGRATION.md`](../DEVICE-INTEGRATION.md). |

Inside the API (`backend/`):

| Folder | Role |
|---|---|
| `main.go` | Builds the web server, registers every URL ("route") with the roles allowed to call it, starts the background jobs. |
| `internal/handlers/` | One file per feature (users, plans, billing, payments, promotions, scheduler, vouchers, NAS, security…). Each handler validates input, runs SQL, returns JSON. |
| `internal/middleware/` | Cross-cutting checks run before handlers: rate limiting, "is this a valid logged-in session?" ([`auth.go`](../backend/internal/middleware/auth.go)), role check, audit logging. |
| `internal/auth/` | Password hashing, JWT creation/validation, session bookkeeping, role helpers. |
| `internal/scanner/` | Wrapper around `nmap` for finding routers/APs on a subnet. |
| `internal/database/` | Opens the PostgreSQL connection pool. |

**Background jobs** (all run *inside* the API process — start the API and they start):

| Job | Interval | What it does |
|---|---|---|
| NAS monitor ([`nas_monitor.go`](../backend/internal/handlers/nas_monitor.go)) | 60 s | Tries TCP connections to each active NAS and records up/down + latency on the `nas` row. |
| Scheduler ([`scheduler.go`](../backend/internal/handlers/scheduler.go)) | polls every 10 min | Runs the due tasks in `scheduled_tasks` — see [5.8](#58-scheduled-tasks). |
| Alert worker ([`alerts.go`](../backend/internal/handlers/alerts.go)) | 5 min | Emails *staff* when a rule's condition is true (needs SMTP configured). |
| Credential-stuffing scan ([`security.go`](../backend/internal/handlers/security.go)) | 5 min (+ hourly cleanup) | Looks for many failed logins in `radpostauth` and records alerts. |
| Honeypot ([`honeypot.go`](../backend/internal/handlers/honeypot.go)) | always listening | A decoy RADIUS port (UDP 11812) that logs whoever probes it. |

---

## 4. How a request flows

### 4.1 Staff action: UI → API → database

Example: an admin adds a customer login on the **Users** page.

1. **Browser.** The Vue page ([`Users.vue`](../frontend/src/views/Users.vue), form in
   [`UserModal.vue`](../frontend/src/components/users/UserModal.vue)) calls
   `radiusUsersAPI.create(...)` in [`api/index.js`](../frontend/src/api/index.js), which sends
   `POST /api/v1/radius/users` with the staff token in an `Authorization: Bearer …` header.
2. **nginx** (frontend container) forwards `/api/*` to `backend:8088`.
3. **Gin router** ([`main.go`](../backend/main.go)) runs, in order:
   request logger → CORS → rate limiter → **RequireAuth** (token signature valid *and* a live,
   non-idle session row exists in `refresh_tokens`) → **AuditLogger** (records every
   POST/PUT/PATCH/DELETE in `audit_log`) → **RequireRole** (`admin` or `super_admin` for this route).
4. **Handler** `CreateRadiusUser` ([`radius_users.go`](../backend/internal/handlers/radius_users.go)),
   inside one database transaction:
   - inserts the customer into `radius_users` (with a bcrypt hash of the password);
   - inserts a `Cleartext-Password` row into `radcheck` (what FreeRADIUS will actually use);
   - inserts a `Simultaneous-Use` row into `radcheck` (the device limit).
5. The API replies with JSON; the page shows a toast and reloads the list.

If the token has expired the browser silently uses the refresh token once and retries
(see the comments in [`api/index.js`](../frontend/src/api/index.js)). If the session was revoked
(logout, or idle too long) the user is sent back to the login page.

### 4.2 Customer device: network → FreeRADIUS → database

1. A customer joins your Wi-Fi. The router (NAS) sends a RADIUS **Access-Request** (username +
   password, protected by the shared secret) to FreeRADIUS on UDP 1812.
2. FreeRADIUS decides whether to trust the sender: it must match a `client` in
   [`clients.conf`](../freeradius/clients.conf) (private/Docker address ranges, one shared
   secret) or a row in the `nas` table (loaded at startup).
3. The **authorize** step in [`sites-available/default`](../freeradius/sites-available/default)
   runs the `sql` module: it reads that username's rows from `radcheck` (and `radreply`,
   group tables) using the queries in
   [`queries.conf`](../freeradius/mods-config/sql/main/postgresql/queries.conf).
4. FreeRADIUS compares the password with `Cleartext-Password`, applies any `Auth-Type := Reject`,
   `Expiration` or `Simultaneous-Use` rows, and answers **Accept** (plus `radreply` rows such as
   a speed limit) or **Reject**.
5. The attempt is written to `radpostauth`.
6. For an accepted user the router then sends **accounting** packets (start, periodic updates,
   stop) to UDP 1813; the `accounting` section writes them to `radacct`. This is the only source
   of "who is online" and "how much data was used".
7. The staff dashboard reads `radpostauth` and `radacct`.

### 4.3 Where the two sides meet: what is actually enforced

FreeRADIUS reads only `radcheck`, `radreply`, the group tables, and `nas`. It never looks at
`radius_users`, `user_plans`, `invoices` or `payments`. So a rule is enforced on the network
**only if the application also writes a matching `radcheck`/`radreply` row**.

| Business rule | Written where | Enforced at login? |
|---|---|---|
| Customer password | `radcheck` `Cleartext-Password` | **Yes** |
| Device limit | `radcheck` `Simultaneous-Use` | **Yes**, via `session { sql }` — counts open `radacct` rows, so it depends on accounting packets arriving |
| Suspended / voucher disabled | `radcheck` `Auth-Type := Reject` | **Yes** |
| Bulk "Set expiry" | `radcheck` `Expiration` | **Yes**, if FreeRADIUS accepts the date format used (not tested) |
| Account expiry from the user form, a plan, or the scheduler's "expire" task | only `radius_users.account_expiry` / `status` | **No** — FreeRADIUS never reads those columns |
| Plan or voucher data cap | `radcheck` `Max-Octets` | **No** — nothing in the FreeRADIUS config evaluates it |
| Voucher time limit | `radcheck` `Session-Timeout` | **Unverified** — usually a reply attribute; not tested |
| Speed plan | `radreply` `Mikrotik-Rate-Limit` + WISPr attributes | By the router, **only if it understands those attributes** (MikroTik does) |
| Fixed IP from an IP pool | `radreply` `Framed-IP-Address` | **Probably not working** — see [10.2](#102-network-and-freeradius) |
| Password expiry / "force password change" | only `radius_users` | **No** |
| Overdue invoice | only `invoices.status` | **No** |

### 4.4 The two other direct paths into RADIUS

- **Probes from the API.** The "Test RADIUS", "Test NAS" and RADIUS-simulator screens build a
  real Access-Request in Go and send it to FreeRADIUS. This is the only direct API→FreeRADIUS
  traffic.
- **Customer self-service portal** (`/portal`): customers sign in with their RADIUS username and
  password (checked against the bcrypt copy in `radius_users`), get a 24-hour token, and see
  plan, expiry, usage and sessions. It is read-only.

---

## 5. Billing end to end

### 5.1 The lifecycle at a glance

```
 Plan ──assigned to──▶ Customer ──▶ Invoice (pending) ──▶ Payment recorded ──▶ Receipt
 (price, days,          (expiry,       │   due in 7 days         │                (HTML page)
  data, devices)         device cap)   ▼                          ▼
                                  overdue (scheduler)      invoice "paid" + webhook "invoice.paid"
```

Everything above is *records*. The only arrows that touch the network are the ones in
[4.3](#43-where-the-two-sides-meet-what-is-actually-enforced).

### 5.2 Plans — [`plans.go`](../backend/internal/handlers/plans.go), page **Plans**

A plan has: name, price and currency, validity in days, optional data cap (MB), maximum devices,
an optional **bandwidth (speed) profile**, and an active flag. The speed profile is a separate
object ([`bandwidth.go`](../backend/internal/handlers/bandwidth.go)) holding upload/download
kbps; it is converted to a MikroTik rate-limit string.

Deleting a plan unlinks it from users and invoices but leaves the limits it already wrote.

### 5.3 Assigning a plan — `POST /radius/users/:id/plan`

This is the one step that connects a plan to a customer. **No page in the web UI calls it**; it is
reachable only through the API. When called it:

1. sets the user's `plan_id`;
2. sets `account_expiry` to today + the plan's validity days (resetting, not extending);
3. sets `device_limit` and the `Simultaneous-Use` row;
4. writes a `Max-Octets` row from the data cap (not enforced) or removes it if the plan is unlimited;
5. applies the speed profile asynchronously (`radreply` rows);
6. if the plan costs more than zero, **creates a pending invoice** due in 7 days.

Each step is a separate statement; none is wrapped in a transaction, and errors are ignored.

### 5.4 Invoices — [`billing.go`](../backend/internal/handlers/billing.go), page **Billing**

Statuses: `pending`, `paid`, `overdue`, `cancelled`. Created automatically in 5.3 or manually
(username, amount, currency, due date, notes — the manual form has no plan field). Invoice numbers
look like `INV-202610-123456`: the month plus six digits taken from the clock, **not a sequence**.
The page's **Mark Paid** button only changes the invoice status; it does not create a payment.

### 5.5 Payments and receipts — [`payments.go`](../backend/internal/handlers/payments.go), page **Payments**

A payment is a manual record of money received (cash, bank transfer, mobile money, card, online).
It gets a receipt number (`RCP-20261019-123456`, random). If the API is called with an
`invoice_id` the invoice is marked paid and the `invoice.paid` webhook fires. The **Payments page
does not send an invoice id**, so payments entered there never touch an invoice. There is no
payment-gateway integration. The receipt is an HTML page (organisation name and email come from
`system_settings`); the "view receipt" link in the UI currently returns a 401 because a plain link
cannot carry the login token ([10.1](#101-billing-and-revenue)).

### 5.6 Promotions — [`promotions.go`](../backend/internal/handlers/promotions.go), page **Promotions**

Discount codes: percent or fixed amount, optional plan restriction, usage limit, validity window.
`Validate` calculates a discounted price for display. `Apply` increments the use counter. **Neither
is connected to invoice creation**, and no invoice records a code. Today a promotion changes what a
customer pays only if staff type the discounted amount into an invoice by hand.

### 5.7 Vouchers — [`vouchers.go`](../backend/internal/handlers/vouchers.go), page **Vouchers**

Prepaid codes generated in batches (up to 500). Each code is simultaneously the username and the
password of a login in `radcheck`. A voucher has data limit, time limit and validity days — but
**no price, no plan, and no redemption step**; status never becomes "used". Codes can be exported
to CSV or printed from the page.

### 5.8 Scheduled tasks

Defined in `scheduled_tasks` ([`009_billing.sql`](../migrations/009_billing.sql)), run by
[`scheduler.go`](../backend/internal/handlers/scheduler.go); managed on the **Scheduler** page
(run now, pause, change frequency). The poller wakes every 10 minutes; "daily" means "24 hours
after the last run".

| Task | Default | On? | What it does | Important caveat |
|---|---|---|---|---|
| `expire_accounts` | daily | yes | Sets `status = 'expired'` for active users whose `account_expiry` is before today | **Does not block the customer on the network** ([5.9](#59-what-actually-blocks-a-customer)) |
| `mark_overdue_invoices` | daily | yes | `pending` → `overdue` when `due_date` has passed | No reminder, no suspension |
| `cleanup_sessions` | hourly | yes | Closes `radacct` sessions with no update for 24 h | Relies on interim accounting updates working (see [10.2](#102-network-and-freeradius)) |
| `daily_report` | daily | yes | Counts active users, open sessions, bytes today | Only stored in `last_result`; nothing is sent |
| `renew_plans` | daily | **no** | Extends expiry by one plan period for active users near expiry | **Has no payment check** — enabling it gives free renewals |

### 5.9 What actually blocks a customer

Only two things stop a customer at the RADIUS server: **Suspend** (Users page → pause button, or
bulk suspend, or disabling a voucher), which writes `Auth-Type := Reject`; and the bulk
**Set Expiry** action, which also writes an `Expiration` row. Everything else — plan end,
`expired` status, overdue invoice — is a label in the application. To cut off non-payers today, a
person must press Suspend.

### 5.10 Revenue numbers (two that can disagree)

- **Billing page** summary: sum of invoices with status `paid`, across all currencies, labelled "USD".
- **Payments page** summary: sum of `confirmed` payment rows, shown with a fixed "$".

Because "Mark Paid" creates no payment, and payments recorded in the UI link to no invoice, the two
totals are independent and will drift apart.

### 5.11 What an operator has to do by hand today

1. Create plans (Plans page).
2. Create each customer (Users page) and set their account expiry and device limit manually — the
   user form has no plan selector.
3. Create an invoice (Billing → New Invoice).
4. Receive money outside the system. Record it (Payments → Record Payment) **and** press
   *Mark Paid* on the invoice.
5. To renew: edit the user's expiry date.
6. To stop a non-payer: press *Suspend*. To restore: *Activate*.
7. Run backups yourself ([8.6](#86-backups)).

---

## 6. The database

PostgreSQL 15. Created by [`init-db.sql`](../init-db.sql) (base schema) plus
[`migrations/009_billing.sql`](../migrations/009_billing.sql) (billing); the Docker setup runs both
once, on an empty data volume. [`migrations/008_network_scanner.sql`](../migrations/008_network_scanner.sql)
repeats tables already in `init-db.sql`.

### 6.1 Tables defined in this repository (23)

**FreeRADIUS tables** (read/written by FreeRADIUS itself — keep their column names):

| Table | Purpose |
|---|---|
| `radcheck` | Per-user conditions: password, reject flag, expiry, device limit. Unique on (username, attribute). |
| `radreply` | Per-user reply attributes (speed limit, fixed IP). **Index is not unique** (matters, see [10.2](#102-network-and-freeradius)). |
| `radgroupcheck`, `radgroupreply`, `radusergroup` | Group equivalents. Present, but the application does not manage groups. |
| `nas` | Routers/APs allowed to talk to FreeRADIUS, with their shared secrets. Loaded by FreeRADIUS at startup. |
| `radacct` | Accounting: one row per session (times, bytes in/out, NAS, calling MAC, IP). |
| `radpostauth` | One row per login attempt and its result. |

**Application tables:**

| Group | Tables | Purpose |
|---|---|---|
| Staff | `app_users`, `refresh_tokens`, `audit_log` | Admin/operator accounts (roles, lockout, optional TOTP), login sessions, a log of every change made through the API. |
| Customers | `radius_users`, `radius_user_password_history` | The customer record (status, expiry, device limit, plan, phone) and old password hashes. |
| Billing | `user_plans`, `invoices`, `payments`, `promotions`, `scheduled_tasks` | See [section 5](#5-billing-end-to-end). |
| Speed & prepaid | `bandwidth_profiles`, `vouchers` | Rate limits and prepaid codes. |
| Network scanner | `network_scans`, `network_scan_hosts` | Results of nmap scans. |
| Settings | `system_settings` | Key/value rows. Only a few keys are read by code ([7.4](#74-secrets-and-settings)). |

How they relate: `radius_users.plan_id → user_plans`; `invoices.user_id/plan_id → radius_users/user_plans`;
`payments.invoice_id → invoices`; `radius_users.bandwidth_profile_id → bandwidth_profiles`. Deleting
a user, plan or invoice *sets the link to NULL* rather than deleting history; invoices and payments
also copy the username so they stay readable. Customers are tied to `radcheck`/`radreply`/`radacct`
only by the **username text**, not by a foreign key.

### 6.2 Tables the code uses that no SQL file creates (20)

`alert_rules`, `api_keys`, `bulk_operations`, `captive_portals`, `cred_stuffing_blocks`,
`customers`, `geoip_cache`, `geoip_rules`, `honeypot_logs`, `hotspot_zones`,
`ip_pool_assignments`, `ip_pools`, `organizations`, `portal_sessions`, `radius_templates`,
`security_alerts`, `sms_logs`, `tickets`, `webhook_logs`, `webhooks`.

Columns the code also expects but the schema lacks: `nas.ping_status`, `nas.ping_latency_ms`,
`nas.last_ping`, `nas.zone_id`, `nas.org_id`, `radius_users.org_id`, `app_users.org_id`.

I searched every commit for these definitions and found none. They may exist in a database someone
built by hand; on a database created from this repository they do not, and the features that depend
on them will fail. Details and the list of affected screens: [10.3](#103-data-and-schema).

---

## 7. Users, roles and security model

### 7.1 Staff roles

| Role | Can do (from the route table in `main.go`) |
|---|---|
| `operator` | View most things (users, sessions, NAS, plans, invoices, payments, reports, vouchers, zones); open tickets; reset a customer's password; validate promo codes. |
| `admin` | Everything an operator can, plus create/edit/delete RADIUS users, NAS devices, plans, invoices, payments, vouchers, bandwidth profiles, IP pools, templates, promotions, zones, captive portals; suspend/activate; bulk operations; network scanner; RADIUS simulator; security tools. |
| `super_admin` | Everything, plus staff accounts, system settings, backups (stubs), API keys, alert rules, webhooks, organisations, scheduler changes, deleting payments. |

The UI hides pages by role, but the **API enforces roles per route** — the UI is a convenience, not
the security boundary.

### 7.2 Staff login and sessions

Username + password (bcrypt; new passwords need 12+ chars with upper/lower/digit/symbol — except in
the first-run wizard, which accepts 8) → optional TOTP code →
a one-hour **access token (JWT)** and a seven-day **refresh token**. Each login creates a *session*;
every API call checks that the session is still active and not idle for more than an hour
(`SESSION_TIMEOUT`). Five wrong passwords lock the account for 15 minutes. Several devices can be
signed in at once.

### 7.3 Customer access

Customers never log in to the staff app. They have (a) their RADIUS credentials for the network
and (b) optionally the read-only `/portal` page.

### 7.4 Secrets and settings

Real configuration comes from **environment variables** (`.env`): database credentials,
`JWT_SECRET`, `RADIUS_SECRET`, SMTP, rate limit, lockout, session timeout. The Settings page and
setup wizard store many similar values in the `system_settings` table, but **the code mostly does
not read them** — changing them there has no effect. Keys actually read from the table:
`setup_complete`, `org_name`, `org_email`, `honeypot_enabled` and the `cs_*` thresholds.

---

## 8. How to run it

### 8.1 What you need

- Docker with the Compose plugin (or `docker-compose`), `curl`, `openssl`. A Linux server (or WSL) is easiest.
- For local development without Docker: PostgreSQL 14+ running, Go 1.21+, Node 18+.
- Open to your network gear: UDP 1812 and 1813 (RADIUS), TCP 8081 (web UI). Put HTTPS in front of
  8081 for anything beyond a private network — nothing in this stack provides TLS.

### 8.2 Docker (recommended)

```bash
./setup.sh
```

It checks Docker, writes a `.env` with **random** database password, JWT secret and RADIUS secret
(only if `.env` does not exist), builds the images, starts everything, waits for health, and sends
a test RADIUS packet. (On Windows use `setup.ps1`; its generated secrets are weaker, see
[10.4](#104-security).)

Manual alternative:

```bash
cp .env.example .env      # then change every secret and password in it
docker compose up -d --build
```

Then open `http://<server>:8081`. The **setup wizard** appears; the first person to complete it
becomes the `super_admin` (there is no default login), so do this before exposing the server.

### 8.3 Check that it works

```bash
docker compose ps                          # all four containers "Up"
curl http://localhost:8088/health          # {"status":"ok", ... "database":"ok"}
docker compose logs -f backend             # API log
docker compose logs -f freeradius          # shows each login attempt (see 10.2: it logs passwords)
```

Test a real login against FreeRADIUS (use the `RADIUS_SECRET` from `.env`):

```bash
docker compose exec freeradius radtest <username> <password> 127.0.0.1 0 '<RADIUS_SECRET>'
```

`Access-Accept` means the customer record and FreeRADIUS agree. `Access-Reject` for an unknown user is
also what `setup.sh` treats as success — it only proves the server is up.

### 8.4 Connect a router / access point

1. In the web UI create a user, then add the device under **NAS Devices** (or find it with the
   **Network Scanner**).
2. On the device, set RADIUS server = this server's IP, auth port 1812, accounting port 1813,
   secret = the shared secret. Walk-throughs per vendor: [`DEVICE-INTEGRATION.md`](../DEVICE-INTEGRATION.md),
   [`NETWORK_SETUP.md`](../NETWORK_SETUP.md).
3. A device on a private address range that uses the global `RADIUS_SECRET` works immediately (see
   [`clients.conf`](../freeradius/clients.conf)). A device with its **own** secret needs a `nas` row
   and a FreeRADIUS restart: `docker compose restart freeradius`.
4. Turn on **interim accounting** on the device; usage, the device limit and the stale-session
   cleanup all depend on it.

### 8.5 Local development without Docker

```bash
./run-local.sh        # needs PostgreSQL running; creates .env.local with random secrets
```

API on `http://localhost:8080`, UI on `http://localhost:3000`. FreeRADIUS is **not** started, so
billing, plans and users work but no one can connect. Manual: `cd backend && go run ./main.go`
and `cd frontend && npm install && npm run dev`.

### 8.6 Backups

The application does **not** back anything up (the Backup button is a stub). Make your own, and
copy it off the server:

```bash
docker exec radius_postgres pg_dump -U radius_user radius > backup_$(date +%F).sql
# restore into an empty database:
docker exec -i radius_postgres psql -U radius_user radius < backup_2026-01-01.sql
```

(Use your `DB_USER` / `DB_NAME` if they differ. These commands were not run during this review.)
`./deploy-update.sh` takes one such dump into `./backups/` before it updates, and never deletes old
ones. **`docker compose down -v` deletes the database volume.**

### 8.7 Updating

```bash
./deploy-update.sh
```

On the server only: it dumps the database, runs `git pull origin main` (after `git checkout main`),
re-applies every file in `migrations/`, rebuilds the backend, frontend and FreeRADIUS images with no cache, and restarts them. A failing
migration does **not** stop it — read the output. There is a short outage while containers restart.

### 8.8 Configuration that matters

| Variable | Used for | Notes |
|---|---|---|
| `DB_USER`, `DB_PASSWORD`, `DB_NAME` | PostgreSQL, API and FreeRADIUS | The password is fixed into the database volume on first start; changing `.env` later does not change it. |
| `JWT_SECRET` | Signing staff tokens | **Must** be set; otherwise a publicly known fallback is used. |
| `RADIUS_SECRET` | FreeRADIUS `clients.conf`, API probes, newly imported NAS rows | Change from `testing123`. Recreate (not just restart) the container after changing. |
| `SMTP_HOST/PORT/USER/PASS/FROM` | Alert emails | Optional. |
| `RATE_LIMIT`, `BRUTE_FORCE_ATTEMPTS`, `BRUTE_FORCE_LOCKOUT`, `SESSION_TIMEOUT`, `BCRYPT_COST` | Throttling and sessions | Defaults 100/min, 5, 15 min, 3600 s, 12. |
| `APP_ENV` | `production` = JSON logs, release mode | |
| `NMAP_MAX_PREFIX`, `NMAP_MAX_PORTSCAN_HOSTS` | Network scanner limits | /18 and 512 hosts. |
| `SMS_GATEWAY`, `SMS_API_KEY`, `SMS_SENDER_ID`, `SMS_BODY_TEMPLATE` | SMS | **Not passed by `docker-compose.yml`** — add them to the backend's `environment:` to use SMS. |
| `ADMIN_IP_WHITELIST`, `BACKUP_SCHEDULE`, `BACKUP_RETENTION_DAYS`, `WEB_PORT` (under Docker), `VITE_API_URL` (at runtime) | — | **Have no effect.** |

### 8.9 Tests

There are none. `go test ./...` reports "no test files" for every package and the frontend has no
test runner.

---

## 9. Repository map

```
.
├── backend/                  Go API  (module github.com/freeradius-manager/backend)
│   ├── main.go               server start-up, every route + allowed roles, background jobs
│   ├── internal/handlers/    one file per feature (billing: plans, billing, payments, promotions,
│   │                         scheduler, vouchers, portal, bulk_ops)
│   ├── internal/middleware/  rate limit, session check, role check, audit log
│   ├── internal/auth/        JWT, passwords, sessions, roles
│   ├── internal/scanner/     nmap wrapper
│   ├── internal/models/      request/response structs
│   ├── radius-manager        a compiled binary committed to git (see 10.5)
│   └── Dockerfile, Makefile
├── frontend/                 Vue 3 + Vite + Tailwind
│   ├── src/api/index.js      every API call + token-refresh logic
│   ├── src/router/index.js   pages and which roles may open them
│   ├── src/store/auth.js     signed-in user
│   ├── src/views/            one file per page
│   └── nginx.conf, Dockerfile
├── freeradius/               RADIUS server config baked into the container
│   ├── sites-available/      default (real logins) and inner-tunnel (PEAP/TTLS inner auth)
│   ├── mods-available/sql    database connection
│   ├── mods-config/sql/main/postgresql/queries.conf   SQL FreeRADIUS runs
│   ├── clients.conf          who may talk to the server
│   └── init.sh               fills in secrets at container start
├── init-db.sql               base schema + defaults
├── migrations/               008 network scanner, 009 billing
├── docker-compose.yml        the four containers
├── setup.sh / setup.ps1      first-time install (Linux / Windows)
├── run-local.sh              run without Docker
├── deploy-update.sh          production update
├── .env.example              configuration template
├── README.md                 feature overview and API list (not audited; see 10.7)
├── DEVICE-INTEGRATION.md, NETWORK_SETUP.md   router/AP set-up guides (not audited)
└── docs/ARCHITECTURE.md      this file
```

---

## 10. Known gaps and risks

Findings are grouped by area and tagged by severity (**Critical** = can lose money, data or control
of the system; **High** = will bite in normal use; **Medium**; **Low**). The **Basis** column says how
sure I am:

- **Ran** — I executed a check and saw the result.
- **Read** — evident from the source/SQL/config, but not executed. Where a result depends on
  PostgreSQL or FreeRADIUS behaviour I could not test, the entry says so.

### 10.1 Billing and revenue

| # | Severity | Gap | Basis |
|---|---|---|---|
| B1 | **Critical** | **Expiry and non-payment do not cut service.** The scheduler only changes `radius_users.status`; FreeRADIUS never reads it. Overdue invoices trigger nothing. Only *Suspend* (and the bulk expiry action) is enforced. | Read: [`scheduler.go`](../backend/internal/handlers/scheduler.go), [`sites-available/default`](../freeradius/sites-available/default), [`queries.conf`](../freeradius/mods-config/sql/main/postgresql/queries.conf) |
| B2 | **Critical** | **`renew_plans` gives free renewals** if anyone enables it (no payment check). It ships off. Also nothing renews a customer when a payment is recorded. | Read |
| B3 | High | **Payments and invoices are disconnected.** The Payments form has no invoice field; *Mark Paid* creates no payment; the API accepts any `invoice_id` (even a cancelled invoice, a partial amount, or a repeat payment). Not transactional. Deleting a payment leaves the invoice "paid". The two revenue totals can disagree. | Read |
| B4 | High | **Receipts cannot be opened from the UI** (plain link, no token → 401), and receipt/captive-portal HTML is built without escaping. | Read |
| B5 | High | **Promotions never reduce an invoice.** `Apply` is unguarded (ignores limits/dates) and the UI never calls it; omitted fields on update re-enable a code or reset its limit. | Read |
| B6 | High | **No UI to assign a plan.** The API call also resets (not extends) expiry, creates a new invoice every time, is not atomic, and silently skips the invoice if the random invoice number collides. | Read |
| B7 | High | **No payment gateway, no customer-facing invoice or payment page, no reminders.** The portal is read-only; expiry SMS is manual; alert emails go to staff; the "daily report" is not delivered. | Read |
| B8 | Medium | **Money handling is basic.** Amounts are `float64` in code; totals mix currencies; the UI hard-codes "$"/"USD"; invoice and receipt numbers are random, not sequential, and can collide; no tax/VAT, refunds, credit notes, proration or line items. | Read |
| B9 | Medium | **Vouchers** have no price/plan, no redemption state, unenforced expiry and data cap, and a batch can silently come back smaller than requested. | Read |
| B10 | Medium | **Data caps are not enforced** for plans, vouchers or imports: no counter module is configured, so a "5 GB" customer is not limited. | Read (FreeRADIUS behaviour not tested) |

### 10.2 Network and FreeRADIUS

| # | Severity | Gap | Basis |
|---|---|---|---|
| N1 | High | **Interim accounting update probably fails.** [`queries.conf`](../freeradius/mods-config/sql/main/postgresql/queries.conf) updates column `frameipaddress`; the table has `framedipaddress`. If so, usage counters and "last update" are not refreshed mid-session, which also makes the 24-hour stale-session cleanup close genuinely connected users. | Read — **Unverified** (needs a live FreeRADIUS + Postgres) |
| N2 | High | **Fixed-IP assignment and template application likely fail silently.** They `INSERT … ON CONFLICT (username, attribute)` on `radreply`, whose index is not unique, so PostgreSQL has nothing to match. Errors are ignored; the UI reports success. | Read — **Unverified** (needs PostgreSQL) |
| N3 | Medium | **"Disconnect" does not disconnect.** It marks `radacct` rows stopped; no CoA/Disconnect packet is sent. | Read |
| N4 | Medium | **Broad client trust.** Any device in the Docker or private ranges that knows the one shared secret is accepted; `require_message_authenticator = no` leaves the server without protection against forged replies (CVE-2024-3596); scanner-imported NAS rows all reuse `RADIUS_SECRET`; a new NAS needs a FreeRADIUS restart. | Read |
| N5 | Medium | **FreeRADIUS logs customer passwords** (`auth_badpass`/`auth_goodpass = yes`) to container output. | Read: [`radiusd.conf`](../freeradius/radiusd.conf) |
| N6 | Medium | **Not tested:** EAP/PEAP (needs certificates from the base image), group lookups (no group-membership query is defined), `Session-Timeout`/`Expiration` value formats. | Unverified |
| N7 | Low | The published `18120/udp` reaches nothing; the honeypot port 11812 is not published; the NAS monitor uses TCP probes, so a healthy NAS that blocks those ports shows "down". | Read |

### 10.3 Data and schema

| # | Severity | Gap | Basis |
|---|---|---|---|
| D1 | **Critical** (fresh install) | **20 tables and 7 columns are missing from the SQL** ([6.2](#62-tables-the-code-uses-that-no-sql-file-creates-20)). On a database built from this repo these screens will error or show nothing: customer **portal login** (`portal_sessions`), Security Center, Honeypot, Zones, Organizations, Customers/Tickets, Webhooks, API keys, Alert rules, IP pools, Templates, SMS logs, Captive portals, Bulk history, and NAS health/monitoring. The dashboard still loads, but its security, honeypot and NAS-health panels show zeros. Billing, plans, users, NAS list, vouchers and the scheduler do not depend on the missing tables. **Check your live database against this list.** | Read (searched all commits) |
| D2 | High | **CSV user import fails for every row** — its INSERT names a `description` column `radius_users` does not have (and the import screen advertises it). | Read |
| D3 | High | **Usage report endpoint is expected to return 500** (`INTERVAL $1` is invalid PostgreSQL). Other reports are fine. | Read — **Unverified** |
| D4 | High | **No real backups.** The Backup/Restore/List endpoints are stubs and `BACKUP_*` settings are unused. The only dump is the one `deploy-update.sh` takes. | Read |
| D5 | Medium | **No migration system.** Init scripts run only on an empty volume; `init-db.sql` cannot be re-run; `deploy-update.sh` ignores migration errors. | Read |
| D6 | Medium | **Webhooks:** the `events` column is treated as JSON in some places and as a PostgreSQL array in others, so event filtering may not work; only `invoice.paid` and `ticket.created` are ever sent although the UI offers nine events. | Read |
| D7 | Low | Many handlers ignore database errors and still answer 200 ("saved"), so failures can look like successes. | Read |

### 10.4 Security

| # | Severity | Gap | Basis |
|---|---|---|---|
| S1 | **Critical** | **Secrets stored in the clear:** customers' network passwords (`radcheck`), NAS secrets (and `GET /nas/:id` shows them to *operators*), TOTP secrets, webhook secrets. Cleartext customer passwords are inherent to PAP/CHAP/MS-CHAP; protect the database accordingly (access control, encryption at rest, backups). | Read |
| S2 | **Critical** | **Insecure defaults start silently:** the code falls back to a fixed JWT key if `JWT_SECRET` is unset, and Compose falls back to fixed database and RADIUS passwords; the seed NAS uses `testing123`. Anyone knowing the fallback JWT key can forge admin tokens. `setup.ps1` generates secrets with a non-cryptographic generator. | Read |
| S3 | High | **Client IP can be spoofed** through the bundled nginx, defeating the per-IP rate limit, the setup limiter, and the IPs in logs/audit. The customer portal has no per-account lockout. | **Ran** (small Gin 1.9.1 program) + Read |
| S4 | High | **First-come setup wizard** creates the super-admin and deletes all existing admins; reopens if `setup_complete` is reset. | Read |
| S5 | Medium | Tokens live in `localStorage`; the API accepts `?token=` on **every** route and logs query strings, so tokens reach access logs. | Read |
| S6 | Medium | **The Settings page and wizard options are mostly decorative** (password length, MFA-required, lockout, session timeout are read from environment variables). MFA cannot be enforced; starting MFA setup overwrites an existing secret. | Read |
| S7 | Medium | Admin-reachable server-side requests (webhook URLs, scanner, simulator, NAS discover) can probe internal networks; the API container runs as root with `NET_RAW`. | Read |
| S8 | Medium | No TLS in the stack; CORS is `*`; the API port 8088 is published directly next to the nginx proxy. | Read |
| S9 | Medium | **"Organizations/resellers" is not multi-tenant:** nothing filters by organisation, and user limits are not enforced. Do not resell it as isolated tenants. | Read |
| S10 | Medium | **Security features that look active but are not:** GeoIP rules and credential-stuffing blocks are never enforced; API keys can be created but no route accepts them. | Read |
| S11 | Low | Audit log redacts only a fixed list of top-level fields (e.g. NAS SNMP `community` is stored); GeoIP uses plain-HTTP ip-api.com, whose free plan is non-commercial. | Read |

### 10.5 Quality and operations

| # | Severity | Gap | Basis |
|---|---|---|---|
| Q1 | High | **No automated tests** (backend or frontend) and no CI. Every claim of correctness rests on manual use. | **Ran** (`go test ./...` → no test files) |
| Q2 | Medium | **A 12.7 MB compiled binary, `backend/radius-manager`, is committed to git** and not ignored. It cannot be reviewed, can go stale, and bloats the repository. Not changed here. | **Ran** (`git ls-files`) |
| Q3 | Medium | **Single-instance design:** in-memory rate limiter and setup cache, background jobs and the honeypot inside the API process (a second replica would double-run them), one PostgreSQL container, no monitoring beyond a health endpoint. | Read |
| Q4 | Medium | `deploy-update.sh` pulls `origin/main` into whichever checkout it runs in and swallows migration errors. | Read |
| Q5 | Low | Inconsistent documentation/values: ports in `.env.example` vs Compose, version strings `2.0.0-pro` vs `1.0.0`, README not audited. | Read |
| Q6 | Low | UI rough edges: two "Import CSV" buttons on Users; "Send Test Email" on Settings does nothing; "Create Backup" claims success; the customer form shows the staff password rule; the Network Map's live-stats panel reads the wrong token key (`token` instead of `access_token`) so it stays empty; the Alert Rules form offers three events (`data_limit_80pct`, `login_failure`, `new_user`) that the backend never evaluates; the NAS form's "generate secret" button uses `Math.random`. | Read |
| Q7 | Low | Dead code: `HasActiveSession`, `RevokeAllUserSessions`, `HasMinRole`, `ValidateAPIKey`, `CheckCredStuffing`. | **Ran** (grep for callers) |

### 10.6 Suggested order before taking money

1. Decide how non-payment/expiry is enforced, then implement it end to end (write `Auth-Type := Reject`
   or `Expiration` when a plan ends or an invoice goes overdue).
2. Connect payment → invoice → renewal (one action, one transaction), and fix the receipt link.
3. Create the missing tables, then walk every screen on a clean database.
4. Set real secrets, remove default NAS secret, turn off password logging, add HTTPS.
5. Set up scheduled, off-machine, *restore-tested* database backups.
6. Fix the proxy IP handling, then add tests for the billing and enforcement paths.
7. Verify interim accounting, IP assignment and the usage report against a live stack (N1, N2, D3).

### 10.7 What was and was not verified

**Executed during this review (all outputs clean):**

- `go build` and `go vet ./...` on the backend; `go test ./...` (no tests exist).
- A production `vite build` of the frontend.
- A small Go program using the project's pinned Gin version, which confirmed that a client-supplied
  `X-Forwarded-For` through nginx becomes the "client IP" ([S3](#104-security)).
- Syntax/structure checks of the documentation-only changes in this branch: all shell scripts parse
  (`bash -n`); `docker-compose.yml` parses to exactly the same structure as before; and for every
  changed file, the file with comments removed is identical to the committed version
  (so the commit changes comments and this document only, no behaviour).

**Not possible here, so findings marked *Read* rest on reading alone:** no PostgreSQL server or
Docker daemon was available, so no SQL was run, no container started, and FreeRADIUS was not started
or sent packets. The web UI was never opened in a browser. Email, SMS, webhook delivery, `nmap`
scanning, EAP and real router integration were not exercised.

**Read in full:** all backend Go code, `init-db.sql`, both migrations, `docker-compose.yml`, every
file under `freeradius/`, all shell/PowerShell scripts, `.env.example`, and the frontend
infrastructure (API client, router, store, nginx, Vite, Dockerfiles). **Frontend pages:** the
complete files for Billing, Payments, Promotions, Plans, Scheduler, Users, Vouchers and Login;
for every other page and component I read the `<script>` section (all logic and API calls) but not
every line of HTML markup, and for Settings, Setup wizard, Webhooks, User portal, the layout and
the user form I read only part of the markup. **Not audited:** `README.md`,
`DEVICE-INTEGRATION.md` and `NETWORK_SETUP.md` (their claims may run ahead of the code), CSS, and
third-party dependencies.
