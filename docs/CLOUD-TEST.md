# Cloud test: Supabase + Render + M-Pesa sandbox

A test deployment on hosted services, to see the system working over the internet and
to test M-Pesa, which needs a public https address that Safaricom can call. It uses
test money and a throwaway database. **Do not put real customers on it.**

```
 your phone ──M-Pesa──► Safaricom sandbox ──callback──► Render: API ◄──── Render: web UI ◄── browser
                                                            │
                                                            ▼
 test router / radtest ──UDP──► FreeRADIUS (this laptop) ──► Supabase: PostgreSQL
```

**What this setup cannot do**, by design of the platforms:

- **FreeRADIUS does not run on Render.** Render carries no UDP, and RADIUS is UDP. FreeRADIUS
  runs next to the routers (here: this laptop) and uses the same Supabase database.
- **The API cannot disconnect routers on your LAN.** Disconnect-Requests (UDP 3799) can't reach
  them from Render. Billing still blocks new logins. Staff see such sessions as "should be
  offline" on *Online now*. A real deployment puts the API next to the routers, or adds a VPN.
- **Free plans sleep.** Render's free web service stops after 15 idle minutes, and then
  scheduled tasks don't run and an M-Pesa callback can arrive while it wakes. A lost phone
  prompt (STK) is caught later by the reconcile loop, but a lost paybill confirmation is not:
  nothing can ask Safaricom for it afterwards, so staff would credit it by hand. Not tested
  here; the real server must stay on. Supabase's free
  project pauses after a week without use. The blueprint uses Render's *free* plan for the
  API and the UI, which is enough for a test: open the API's /health page to wake it before
  you start, and set `plan: starter` in render.yaml (about $7 a month) before real customers.

## 1. Accounts and keys (about 20 minutes)

1. **Safaricom Daraja:** sign up at <https://developer.safaricom.co.ke>, then *My Apps → Create
   app*, ticking **Lipa Na M-Pesa Sandbox** and **M-Pesa Sandbox**. Note the
   **Consumer Key** and **Consumer Secret**. Under *APIs → M-Pesa Express → Simulate* (or the
   test credentials page), note:
   - the **Lipa na M-Pesa shortcode** `174379` and its **passkey**
   - the **C2B test shortcode** (600xxx).
2. **Supabase:** create a project (region: *Frankfurt* or *Ireland*; there is no African
   region). Keep the database password. Open *Connect → Session pooler* and note the host
   (`aws-0-<region>.pooler.supabase.com`), port `5432` and user (`postgres.<project-ref>`).
   Use the **session pooler**: the direct address is IPv6-only, which Render can't reach, and
   transaction mode (port 6543) drops the session settings the backend uses.
3. **Render:** sign up at <https://render.com> and connect your GitHub account.
4. **GitHub:** Render deploys from a GitHub repository, so push the branch:
   `git push origin production-fixes`.

## 1a. Check the Daraja keys first (this laptop, no deployment)

The client alone against the sandbox: sign-in, a dropped token, prompts to Safaricom's test
number (no phone rings, nothing is paid), the result query and paybill registration.

```bash
export MPESA_SANDBOX_CLIENT=1 MPESA_CONSUMER_KEY=... MPESA_CONSUMER_SECRET=... MPESA_PASSKEY=...
cd backend
for t in SignIn RefusedToken PromptAndQuery; do go test ./internal/handlers -run "SandboxClient$t\$" -count=1 -v; done
```

**Safaricom's firewall** blocks your address for every API for 2–4 minutes after a few
*refused* requests in a short time (it answers an HTML page, 403). `RefusedPrompts` sends
refused requests on purpose, so it will trigger that; run it on its own and expect to wait.
The gateway also allows only 30 requests a minute (429 "Spike arrest"); the client spaces its
calls and retries. Both are handled in the backend; see ARCHITECTURE.md 5.5a.

## 2. Database (this laptop, 1 minute)

```bash
export DATABASE_URL='postgresql://postgres.<ref>:<password>@aws-0-<region>.pooler.supabase.com:5432/postgres?sslmode=require'
TZ=Africa/Nairobi scripts/db-setup.sh
```

It loads the schema and every migration, and sets the database's timezone to Nairobi.
Supabase defaults to UTC, which would cut customers off three hours late. Running it again is
safe.

## 3. Render (10 minutes)

1. *New → Blueprint*, pick the repository and branch `production-fixes`. Render reads
   [render.yaml](../render.yaml) and proposes two services: `radius-manager-api` (Docker) and
   `radius-manager-ui` (static).
2. Fill in the values it asks for:

   | Key | Value |
   |---|---|
   | `DB_HOST` | the session pooler host |
   | `DB_USER` | `postgres.<project-ref>` |
   | `DB_PASSWORD` | the database password |
   | `MPESA_CONSUMER_KEY`, `MPESA_CONSUMER_SECRET` | from the Daraja app |
   | `MPESA_PASSKEY` | the sandbox passkey for 174379 |
   | `MPESA_C2B_SHORTCODE` | the 600xxx paybill test code |
   | `MPESA_CALLBACK_URL` | the API's address, e.g. `https://radius-manager-api.onrender.com` (no trailing slash) |

3. **If Render gave the API a different address** (the name was taken), change the rewrite
   destination at the bottom of `render.yaml` to match, push, and fix `MPESA_CALLBACK_URL`.
4. Check the API: `https://<api>/health` should say `"database":"ok"`. In the API's logs
   there must be **no** `clock:` error. If there is one, the database and the backend disagree
   on the timezone; rerun step 2.

## 4. First login

Open the UI's address. The setup wizard asks for the **setup token**: it's `SETUP_TOKEN` in
the API's environment on Render. Create the super admin. Then go to *Settings → M-Pesa*. It
should say *Sandbox · Paybill 174379*. Press **Tell Safaricom where to send paybill
payments**.

## 5. FreeRADIUS here, on the cloud database

FreeRADIUS needs `RADIUS_SECRET` from the API's environment on Render, so that the API's probes
and Disconnect-Requests match it.

```bash
TZ=Africa/Nairobi \
TEST_RADIUS_DB_HOST=aws-0-<region>.pooler.supabase.com TEST_PG_PORT=5432 TEST_PG_DB=postgres \
TEST_RADIUS_DB_USER=postgres.<ref> TEST_RADIUS_DB_PASS='<password>' TEST_RADIUS_DB_SSLMODE=require \
TEST_RADIUS_POOL_MAX=5 TEST_RADIUS_SECRET='<RADIUS_SECRET from Render>' \
TEST_RADIUS_DIR=/tmp/radius-cloud TEST_RADIUS_LISTEN=0.0.0.0 \
scripts/test-radius.sh start
```

Then create a customer in the UI and try `radtest <username> <password> 127.0.0.1:18120 0 '<RADIUS_SECRET>'`.
A real router on your LAN can use this laptop's address, port 18120/18121, and that secret.
`TEST_RADIUS_POOL_MAX=5` matters here: Supabase's free session pooler allows about 15
connections, shared with the API (6).

## 6. The automated tests, against the cloud

From `backend/`, with the same values. **These wipe the cloud database**: they're for this
throwaway project only. Afterwards, sign up again through the wizard. The tests also switch the
scheduled tasks off (they run them by hand); switch them back on on the *Scheduler* page.

```bash
export TEST_DATABASE_URL="$DATABASE_URL" TEST_DATABASE_WIPE_OK=yes
export TEST_API_URL=https://<api>.onrender.com
export TEST_RADIUS_ADDR=127.0.0.1:18120 TEST_RADIUS_SECRET='<RADIUS_SECRET>'

go test ./internal/handlers -run 'StackCustomerJourney|RealRadius' -count=1 -v    # the whole path over the internet

# M-Pesa sandbox, a real prompt: your phone rings, you type your PIN (test money)
MPESA_SANDBOX_PHONE=07XXXXXXXX go test ./internal/handlers -run CloudMpesaSandboxPrompt -count=1 -v

# M-Pesa sandbox, paybill payments simulated by Safaricom (known and unknown account)
MPESA_SANDBOX_C2B=1 MPESA_C2B_SHORTCODE=600xxx MPESA_CONSUMER_KEY=... MPESA_CONSUMER_SECRET=... \
  go test ./internal/handlers -run CloudMpesaSandboxPaybill -count=1 -v

# How fast logins are when FreeRADIUS is in Kenya and the database in Europe
LOAD_TEST=1 LOAD_CUSTOMERS=300 LOAD_SECONDS=15 LOAD_CONCURRENCY=16 go test ./internal/handlers -run LoadRadiusLogins -count=1 -v
```

The crash tests (`StackPostgresCrash…`, `StackBackendKilled…`) skip themselves: they need to
kill processes on this machine.

**Expect** each login to take about five database round trips, so with Europe ~150 ms away
that's roughly 0.5–1 s per login. That is the main thing this test will tell you. A production
FreeRADIUS should sit close to its database: both on one server near the routers, or a
database in the same region as FreeRADIUS.

## 7. Without Render: a tunnel to this laptop

To try M-Pesa before setting up Render, expose the local test stack with a temporary public
address. Cloudflare's quick tunnel needs no account:

```bash
curl -L -o /tmp/cloudflared https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 && chmod +x /tmp/cloudflared
/tmp/cloudflared tunnel --url http://127.0.0.1:18080        # prints https://<random>.trycloudflare.com
```

Then start the stack with the M-Pesa settings in your shell. It copies every `MPESA_*`
variable to the backend.

```bash
export MPESA_CONSUMER_KEY=... MPESA_CONSUMER_SECRET=... MPESA_SHORTCODE=174379 MPESA_PASSKEY=... \
       MPESA_CALLBACK_URL=https://<random>.trycloudflare.com MPESA_CALLBACK_TOKEN=$(openssl rand -hex 16) \
       MPESA_C2B_SHORTCODE=600xxx
scripts/test-stack.sh stop; eval "$(scripts/test-stack.sh start)"
MPESA_SANDBOX_PHONE=07XXXXXXXX go test ./internal/handlers -run CloudMpesaSandboxPrompt -count=1 -v
```

The tunnel publishes the test API to the internet while it runs. Stop it (Ctrl-C) when done.
To publish only the callback paths (not the admin API), put a filter in front and point the
tunnel at it instead of 18080:

```bash
python3 - <<'EOF' &
import http.server, urllib.request
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        if not self.path.startswith('/api/v1/pay/'):
            self.send_response(404); self.end_headers(); return
        body = self.rfile.read(int(self.headers.get('Content-Length') or 0))
        req = urllib.request.Request('http://127.0.0.1:18080' + self.path, body, {'Content-Type': 'application/json'})
        try: r = urllib.request.urlopen(req, timeout=30); code, out = r.status, r.read()
        except urllib.error.HTTPError as e: code, out = e.code, e.read()
        self.send_response(code); self.send_header('Content-Type', 'application/json'); self.end_headers(); self.wfile.write(out)
    do_GET = lambda self: (self.send_response(404), self.end_headers())
http.server.ThreadingHTTPServer(('127.0.0.1', 18300), H).serve_forever()
EOF
/tmp/cloudflared tunnel --url http://127.0.0.1:18300
```

The sandbox's paybill simulator calls back without a phone:

```bash
MPESA_SANDBOX_C2B=1 go test ./internal/handlers -run CloudMpesaSandboxPaybill -count=1 -v
```

## 8. Results to record

Sandbox, the C2B shortcode is the **Party B** code on the test credentials page (600000); 600584
(Party A) is the paying side of B2B tests. A prompt to a real phone gives about 25 seconds to
enter the PIN.

| Check | Result |
|---|---|
| `/health` ok, no `clock:` error in the API log | |
| Setup wizard, login, create plan and customer in the UI | |
| `StackCustomerJourney`, `RealRadius*` against the cloud | |
| Real prompt to your phone: paid, payment recorded once, customer online | 23 Oct 2026, tunnel: the prompt arrived and the PIN was entered, but the sandbox never completed it (no SMS, no callback, status "being processed"); stayed pending, nothing recorded. Prove on the live paybill. |
| Prompt cancelled on the phone: shows "failed", nothing recorded | Unanswered twice: callback 1037 "No response from user" after ~26 s through the tunnel; failed, nothing recorded. |
| Simulated paybill payment: applied to the customer; unknown account waits on *Payments* | Shortcode 600000: applied within 3 s; unknown account kept as unmatched. The sandbox dropped a second simulation sent within a second of the first. |
| Render (free) + Supabase (free) deployed, setup wizard and login, RADIUS user and invoice created | 23 Oct 2026: worked after loading the schema with `scripts/db-setup.sh` (an empty database looks like "setup done" and shows the login page). |
| Paybill registered with Safaricom from Render; simulated KES 1 payment applied to the customer | 23 Oct 2026: applied within seconds. Nothing arrived at first: Render's generated callback secret contained `/`, so the addresses 404ed (B29, fixed; register again after upgrading). |
| Unknown account from Render waits for staff | 24 Oct 2026: `WRONGACCT3` arrived within seconds as unmatched. Before that, three simulations accepted by Safaricom (09:25, 20:35, 20:38) never arrived until *Tell Safaricom where to send paybill payments* was pressed again: the sandbox shortcode 600000 is shared by all Daraja developers, so another app's registration replaces yours. Register again just before each sandbox test. A matched payment sent 25 s after the unknown one was not delivered either (sandbox). |
| Login latency, FreeRADIUS here → Supabase | |
| A backup from *Settings → Backups* restores with `psql` (see `TestBackupManagedDatabase`) | |

## 9. Afterwards

Delete the Render services and the Supabase project, or pause them. Revoke the Daraja app
if you won't use it again. None of the secrets used here should be reused in production.
