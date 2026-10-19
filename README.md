# FreeRADIUS Manager

A web app for running a small ISP or Wi-Fi hotspot business on top of **FreeRADIUS** and
**PostgreSQL**: customers, plans, invoices and payments, vouchers, routers, and the RADIUS
rules that decide who gets online.

- **Backend:** Go 1.21 (Gin)
- **Frontend:** Vue 3 + Tailwind, served by nginx
- **Database:** PostgreSQL 15
- **RADIUS:** FreeRADIUS 3.2
- Everything runs in Docker Compose.

> **Status.** Version 1.1.0. The core billing loop (plan → invoice → payment → access) is
> tested end to end against a real FreeRADIUS and PostgreSQL. Read
> [Limits](#limits-read-before-you-sell-service) before charging real customers, and
> [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how it works inside.

---

## Contents

1. [How it fits together](#how-it-fits-together)
2. [Install](#install)
3. [Connect your routers](#connect-your-routers)
4. [How billing works](#how-billing-works)
5. [Everyday operation](#everyday-operation)
6. [Updating](#updating)
7. [Backups and restore](#backups-and-restore)
8. [Configuration](#configuration)
9. [Staff roles](#staff-roles)
10. [Features at a glance](#features-at-a-glance)
11. [API](#api)
12. [Running without Docker (development)](#running-without-docker-development)
13. [Troubleshooting](#troubleshooting)
14. [Limits: read before you sell service](#limits-read-before-you-sell-service)
15. [License](#license)

---

## How it fits together

```
 Staff browser ──HTTPS (optional Caddy)──► frontend (nginx :8081) ──/api──► backend (Go)
 Customer browser ──► /portal (self-service)                                  │
                                                                             ▼
 Routers / APs ──UDP 1812 auth, 1813 accounting──► FreeRADIUS ◄──────► PostgreSQL
               ◄──UDP 3799 Disconnect (from the backend)────────────────────┘
```

- **FreeRADIUS decides who gets online.** It answers every login from your routers. It reads
  the `radcheck`/`radreply` rows the backend writes: password, paid-until date
  (`Expiration`), device limit (`Simultaneous-Use`), data cap, speed, and a block when a
  customer is suspended or expired.
- **The backend is the business side.** It handles customers, plans, invoices, payments,
  scheduled tasks, alerts and backups. Every change that affects access is written to the
  RADIUS tables in the same database transaction.
- **Accounting** (start, interim and stop packets from your routers) is what powers "who is
  online", usage reports, data caps and device limits. Routers must send it; see
  [Connect your routers](#connect-your-routers).

---

## Install

**You need:**

- A Linux server (Ubuntu 22.04/24.04 is fine) with 2 GB RAM and 20 GB disk.
- Docker with the Compose plugin.
- A static IP that your routers can reach.

```bash
git clone <this repository> radius-manager
cd radius-manager
./setup.sh
```

`setup.sh`:

- writes `.env` with generated secrets, a **setup token** and your server's timezone
- builds the images and starts the four containers
- waits until they are healthy
- prints the web address and the setup token

On Windows with Docker Desktop, run `setup.ps1` instead.

Then:

1. **Edit `.env`** if needed. At least set `DEFAULT_CURRENCY` (e.g. `KES`), and `SMTP_*` if you
   want email. Apply changes with `docker compose up -d`.
2. **Open `http://SERVER-IP:8081`.** The setup wizard asks for the setup token, your
   business name and the first admin account. It refuses to run again once an admin
   exists.
3. **Firewall.**
   - Allow UDP **1812/1813** only from your routers.
   - Allow TCP **8081** (or 80/443 with HTTPS) only from where staff and customers need it.
   - Don't expose port 8088: compose binds it to `127.0.0.1` already.
4. **Set up two-factor authentication** for yourself: **My Profile → Two-Factor
   Authentication**. Then consider turning on **Settings → Require two-factor
   authentication** for all staff.

### HTTPS

Logins and customer data shouldn't cross the internet unencrypted. With a DNS name pointing
at the server:

```bash
# in .env
DOMAIN=billing.example.com
COMPOSE_PROFILES=https
```

Then `docker compose up -d`. The bundled Caddy container fetches a Let's Encrypt
certificate and serves the UI at `https://billing.example.com`. Ports 80 and 443 must be
open.

---

## Connect your routers

Full per-vendor steps are in [DEVICE-INTEGRATION.md](DEVICE-INTEGRATION.md). For every
router:

1. **Register it** under **NAS Devices** with its IP and its **own** shared secret (use the
   generator). Then run `docker compose restart freeradius`, because FreeRADIUS loads routers at
   startup.
   - Routers on private addresses (10.x, 172.16–31.x, 192.168.x) that are not registered are
     accepted with the fallback `RADIUS_SECRET` from `.env`.
   - Routers on public IPs must be registered.
2. **Point the router at the server:** auth port 1812, accounting port 1813, same secret.
3. **Turn on accounting with interim updates** (every 5 minutes is typical). Without
   accounting, device limits, data caps, usage and "online now" do not work.
4. **Accept Disconnect requests (CoA) on UDP 3799** from the server. The Disconnect button,
   blocking a customer who stops paying, and data-cap enforcement all use it.

MikroTik example:

```routeros
/radius add address=SERVER-IP secret=THE-SECRET service=hotspot,ppp \
    authentication-port=1812 accounting-port=1813 timeout=3s
/radius incoming set accept=yes port=3799
/ip hotspot profile set [find] use-radius=yes radius-accounting=yes radius-interim-update=5m
/ppp aaa set use-radius=yes accounting=yes interim-update=5m
```

To test it, go to **Settings → RADIUS Test** or **Security → RADIUS Simulator** and log in as a
customer. The router's own login page should then work with the same username and
password.

---

## How billing works

The defaults are **prepaid**: a customer on a paid plan is online while their **paid-until**
date has not passed.

| Step | What happens |
|---|---|
| **Create a plan** (Plans) | Price, currency, validity (days), speed (bandwidth profile), data cap, device count. |
| **Assign it** (RADIUS Users → Assign Plan, or when creating the user) | The plan's limits are written to RADIUS at once. A paid plan raises an **invoice**. If the customer has no paid days left, they stay offline until it is paid, unless you tick **Start now** (postpaid). |
| **Record the payment** (Billing → Record payment, or Payments) | Payments are recorded against an invoice, and an invoice can be paid in several parts. When fully paid, the invoice is marked paid and the plan's days are added to the paid-until date: from the current date if it is still running, otherwise from today. You get a printable receipt. |
| **Renewal** | The **Raise Renewal Invoices** task bills each customer a few days (`renewal_invoice_days`) before they run out. Nothing is ever extended without payment. |
| **Expiry** | At midnight (server timezone, `TZ`) after the paid-until date plus `billing_grace_days`, FreeRADIUS refuses the customer. The **Expire Accounts** task marks them expired and disconnects open sessions. |
| **Mistakes and refunds** | Payments are never deleted. **Reverse** one (super admin): its invoice re-opens and the days it bought are taken back. |
| **Promo codes** (Promotions) | Give a code when creating an invoice or assigning a plan; the discount is applied to the invoice and the code's use is counted. |
| **Vouchers** | Printable codes sold offline. Each code is a login with its own validity, data and time limits, optionally from a plan. |

Settings that change these rules (on the **Settings** page):

- `default_currency`
- `invoice_due_days`
- `renewal_invoice_days`
- `billing_grace_days`
- `suspend_overdue` (blocks postpaid customers whose invoices are overdue)
- `reminder_days` and the reminder text, sent by SMS or email

Money is never added up across currencies: every total is shown per currency.

---

## Everyday operation

| Task | Where |
|---|---|
| Add a customer | **RADIUS Users → Add User** (optionally with a plan) |
| See who is online, kick someone | **Dashboard**, **Monitoring**, **RADIUS Users → Disconnect** |
| Unpaid invoices | **Billing**, filter "Open" |
| Today's money | **Payments** (totals per currency) |
| Suspend / reactivate | **RADIUS Users** (takes effect at the next login; Disconnect to kick now) |
| Scheduled jobs and their last result | **Scheduler** |
| Alerts by email | **Alert Rules** (needs `SMTP_*` in `.env`) |

**Scheduled tasks** run inside the backend (checked every 10 minutes):

| Task | When | What it does |
|---|---|---|
| Expire Accounts | daily | Marks customers past paid-until + grace as expired, blocks and disconnects them |
| Mark Overdue Invoices | daily | Pending invoices past their due date become overdue (and block postpaid customers if `suspend_overdue`) |
| Raise Renewal Invoices | daily | Bills customers whose paid-until date is near |
| Expiry Reminders | daily | SMS/email reminder `reminder_days` before expiry |
| Disconnect Over-Quota Users | hourly | Kicks sessions of customers over their data cap |
| Voucher Housekeeping | hourly | Marks vouchers used or expired |
| Cleanup Stale Sessions | hourly | Closes sessions with no accounting update for 24 hours |
| Database Backup | daily | `pg_dump` to `./backups`, deletes ones older than `backup_retention_days` |
| Daily Report | daily | Summary, emailed to `report_email` if set |
| Purge Old Logs | weekly | Deletes old auth logs, webhook logs, expired sessions |

**Maintenance commands** (run on the server):

```bash
docker exec radius_backend ./radius-manager access-report     # who RADIUS will refuse, and why
docker exec radius_backend ./radius-manager resync-access     # rewrite access rows for every customer
docker exec -it radius_backend ./radius-manager reset-password admin   # locked out? set a new staff password
docker exec radius_backend ./radius-manager reset-mfa admin   # lost the authenticator phone
```

Logs: `docker compose logs -f backend`, `docker compose logs -f freeradius`.

---

## Updating

```bash
./deploy-update.sh
```

It stops at the first problem, and nothing running is touched until the new version has
built and you have confirmed. In order, it:

1. Backs up the database to `./backups`.
2. Pulls `main` (fast-forward only; it refuses if the checkout has local changes).
3. Builds the new images.
4. **Shows which customers will be refused** and asks you to confirm.
5. Applies new migrations once each, each in a single transaction.
6. Restarts the containers.
7. Rewrites every customer's RADIUS access rows.

Options:

- `--yes` skips the question (for automation).
- `--no-pull` deploys the code already checked out.
- `DEPLOY_BRANCH=…` pulls another branch.

> **Upgrading from 1.0.** In 1.0 a customer's status and expiry date were only labels:
> FreeRADIUS didn't enforce them. After this update it does. Customers marked
> expired or suspended, or past their paid-until date, **are refused at their next login**.
> The update script lists them first. Extend anyone who has paid before you confirm.

---

## Backups and restore

- **Automatic.** A daily `pg_dump` goes to `./backups` on the server and is kept for
  `backup_retention_days` (default 14).
- **On demand.** **Settings → Backups → Back up now**, then download from the same list.
- **Off-site.** A backup on the same disk doesn't survive losing that disk. Download one
  regularly, or sync `./backups` to other storage.

To restore (replaces all data):

```bash
docker compose stop backend freeradius
gunzip -c backups/radius_backup_YYYYMMDD_HHMMSS.sql.gz | docker exec -i radius_postgres psql -U radius_user -d radius
docker compose start backend freeradius
```

---

## Configuration

**`.env`** holds infrastructure and secrets. Change it, then run `docker compose up -d`.
[.env.example](.env.example) documents every variable. The important ones:

| Variable | Purpose |
|---|---|
| `DB_PASSWORD`, `JWT_SECRET`, `RADIUS_SECRET` | Required. The stack refuses to start with empty or example values. `JWT_SECRET` ≥ 32 characters; changing it signs everyone out. |
| `SETUP_TOKEN` | Asked for by the first-run wizard, so nobody else can claim a fresh server. |
| `TZ` | Timezone for all containers; customers are cut off at midnight in this zone. |
| `DEFAULT_CURRENCY` | Currency when a plan or invoice does not name one. |
| `DOMAIN`, `COMPOSE_PROFILES=https` | Optional HTTPS through Caddy. |
| `SMTP_*` | Email for alerts, reminders and the daily report. |
| `SMS_*` | Any HTTP SMS gateway (see `backend/internal/handlers/sms.go`). |
| `TRUSTED_PROXIES` | Add your load balancer here if one sits in front of the UI. |
| `RATE_LIMIT` | API requests per minute per IP (default 300). |

**The Settings page** (super admin) holds business rules that apply immediately:

- billing (currency, due days, grace days, renewals, overdue blocking, reminders, daily report email)
- staff password length, idle sign-out, lockout, and required MFA
- customer-portal lockout, credential-stuffing thresholds and backup retention

---

## Staff roles

The server enforces these on every request; the menu only hides what you can't use.

| | Operator | Admin | Super admin |
|---|:-:|:-:|:-:|
| View customers, sessions, invoices, payments, reports | ✓ | ✓ | ✓ |
| Reset a customer's password, work tickets | ✓ | ✓ | ✓ |
| Create/edit customers, plans, invoices; record payments; vouchers; routers | | ✓ | ✓ |
| Bulk operations, RADIUS simulator, block IPs | | ✓ | ✓ |
| Reverse payments, settings, backups, staff accounts, API keys, webhooks, alert rules, scheduler | | | ✓ |

---

## Features at a glance

| Area | Notes |
|---|---|
| **Customers** | Status, paid-until, device limit, plan, notes; CSV import/export; reset password; Disconnect. |
| **Plans & bandwidth profiles** | Speed sent as `Mikrotik-Rate-Limit` and WISPr attributes; data cap counted from the start of the paid period. |
| **Billing, payments, promotions** | See [How billing works](#how-billing-works). Sequential invoice and receipt numbers (`INV-2026-000001`). |
| **Vouchers** | Batches, printable, CSV export, per-voucher validity, data and time limits. |
| **Customer portal** (`/portal`) | Customers sign in with their network username and password to see their plan, paid-until date, usage, sessions and invoices. Read-only. |
| **Captive portal builder** | Branded hotspot login pages at `/api/v1/captive/serve/:id`; works as a MikroTik external login page. |
| **Routers** | NAS list with ping monitoring, network map, hotspot zones, IP pools (static IPs), RADIUS attribute templates. |
| **Notifications** | Email alert rules (9 events), SMS, expiry reminders, signed webhooks (`X-RADIUS-Signature`). |
| **Security** | Staff MFA, login lockouts, IP blocks, country (GeoIP) rules, credential-stuffing detection, audit log, honeypot listener. |
| **CRM** | Customers (contact records) and support tickets, separate from network logins. |

---

## API

All endpoints are under `/api/v1`. Authenticate with either:

- `Authorization: Bearer <access token>` (from `POST /auth/login`; refresh with `POST /auth/refresh`), or
- `Authorization: ApiKey <key>` (created under **API Keys**). A `read` key may only GET; a
  `write` key acts with the admin role.

The route list, with the role each route needs, is in [backend/main.go](backend/main.go).
`GET /health` reports database status, and `GET /health?full=1` also checks FreeRADIUS.

---

## Running without Docker (development)

```bash
./run-local.sh     # needs PostgreSQL 14+, Go 1.21+, Node 18+
```

This creates a local database, applies the schema, and starts the API on `:8080` and the UI on
`http://localhost:3000`. It doesn't run FreeRADIUS and uses development mode (weak secrets
only warn), so **don't use it for production**.

---

## Troubleshooting

| Symptom | Check |
|---|---|
| `docker compose` says `DB_PASSWORD`/`JWT_SECRET`/`RADIUS_SECRET` must be set | Run `./setup.sh`, or fill them in `.env`. |
| Backend exits with "insecure configuration" | A secret is empty, too short or a published example value. Generate new ones (see `.env.example`). |
| Router logins always rejected | **Monitoring → Auth logs** shows whether the request reached FreeRADIUS and was rejected. Check that the router's secret matches its NAS entry (and that you restarted FreeRADIUS after adding it), that the customer is active and paid, and try the RADIUS Simulator. `docker compose logs freeradius` shows "unknown client" for unregistered public IPs. |
| Customer online but not shown as online / no usage | The router is not sending accounting, or no interim updates. |
| Disconnect says the router did not answer | Enable CoA/"incoming" on the router (UDP 3799) and allow it in the router's firewall; the NAS entry's CoA port must match. |
| Locked out of the admin account | `docker exec -it radius_backend ./radius-manager reset-password <username>` (and `reset-mfa` for a lost phone). |
| "Last backup failed" on Settings | `docker compose logs backend`; check disk space and that `./backups` is writable. |
| UI shows old screens after an update | Hard-refresh the browser (Ctrl+Shift+R). |

---

## Limits: read before you sell service

- **No payment gateway.** Payments (M-Pesa, bank, cash) are recorded by staff. Nothing
  receives payment callbacks yet.
- **No tax/VAT, proration or credit notes.** Invoices are a single amount.
- **Organizations are labels, not tenants.** Every staff account sees every customer, so
  don't give resellers staff accounts expecting isolation.
- **Customer passwords are stored readable** (`Cleartext-Password`), because PAP/CHAP need
  them. Protect the database and its backups accordingly.
- **The free GeoIP service** (ip-api.com) is for non-commercial use. Set `GEOIP_API_URL` to a
  paid endpoint if you use country rules.
- **One backend instance.** Rate limits and lockout counters live in memory. To run more
  than one copy, set `RUN_WORKERS=false` on all but one.
- **The honeypot** listens on UDP 11812 inside the backend container. Publish that port in
  `docker-compose.yml` if you want it reachable.
- **Tested with:**
  - FreeRADIUS 3.2.5 with PAP; the image uses 3.2.10, the same configuration format.
  - The full API flow and the main UI flows in a browser.

  **Not yet tested** with real routers, EAP/PEAP or real SMS/SMTP providers: try them on a
  test router first.

Known gaps and planned work: [docs/ARCHITECTURE.md §10](docs/ARCHITECTURE.md) and
[docs/phase.md](docs/phase.md).

---

## License

This repository has no license file. Until the original author adds one, you have no
explicit right to redistribute it. Ask them before selling or sharing the software itself
(running it for your own business is a separate question worth asking at the same time).
