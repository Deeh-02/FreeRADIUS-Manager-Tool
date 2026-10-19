# Device Integration Guide
## Connecting Network Devices to FreeRADIUS Manager

This guide explains how to configure routers, switches, access points, and firewalls to authenticate users against your FreeRADIUS Manager system.

---

## Table of Contents

1. [Before You Start — Checklist](#1-before-you-start--checklist)
2. [How RADIUS Authentication Works](#2-how-radius-authentication-works)
3. [Add Your Device in FreeRADIUS Manager](#3-add-your-device-in-freeradius-manager)
4. [MikroTik RouterOS](#4-mikrotik-routeros)
5. [Cisco IOS / IOS-XE](#5-cisco-ios--ios-xe)
6. [Cisco Catalyst Switches (802.1X)](#6-cisco-catalyst-switches-8021x)
7. [Ubiquiti UniFi Access Points](#7-ubiquiti-unifi-access-points)
8. [Ubiquiti EdgeRouter](#8-ubiquiti-edgerouter)
9. [pfSense / OPNsense](#9-pfsense--opnsense)
10. [Huawei Switches & Routers](#10-huawei-switches--routers)
11. [TP-Link Omada (EAP Series)](#11-tp-link-omada-eap-series)
12. [FortiGate Firewall](#12-fortigate-firewall)
13. [Windows Server NPS (802.1X)](#13-windows-server-nps-8021x)
14. [Linux (hostapd) Access Point](#14-linux-hostapd-access-point)
15. [Generic Device Template](#15-generic-device-template)
16. [Creating RADIUS Users](#16-creating-radius-users)
17. [Testing Authentication](#17-testing-authentication)
18. [Troubleshooting](#18-troubleshooting)

---

## 1. Before You Start — Checklist

Before configuring any device, confirm the following:

- [ ] FreeRADIUS Manager is running (`docker compose ps` shows all containers healthy)
- [ ] The setup wizard has been completed at `http://YOUR-SERVER-IP:8081`
- [ ] You know your **RADIUS server IP** (the server running Docker)
- [ ] The device is registered under **NAS Devices** with its **own shared secret** (section 3)
- [ ] Port **1812/UDP** (authentication) and **1813/UDP** (accounting) are open on the server firewall, for your routers' addresses
- [ ] The router can receive **Disconnect requests on UDP 3799** from the server (section 3)
- [ ] At least one RADIUS user has been created in the web interface

### Key Values You Will Need

| Setting | Value | Where to Find |
|---------|-------|---------------|
| RADIUS Server IP | `YOUR-SERVER-IP` | IP of the server running Docker |
| Auth Port | `1812` | Fixed — do not change |
| Accounting Port | `1813` | Fixed — do not change |
| Disconnect (CoA) Port | `3799` | On the router: accept requests from the server |
| Shared Secret | `your-radius-secret` | The secret on the device's **NAS Devices** entry (fallback: `RADIUS_SECRET` in `.env`) |
| Protocol | `PAP` or `CHAP` | PAP recommended for simplicity |
| Accounting | On, with **interim updates** every 5 minutes | Needed for "online now", usage, data caps and device limits |

> **Security Note:** The shared secret must be the same in both FreeRADIUS Manager and on the device. It must never be shared publicly. Give each router its own secret.

---

## 2. How RADIUS Authentication Works

```
User enters                Network Device             FreeRADIUS Manager
credentials           (Router / Switch / AP)          (RADIUS Server)
     │                          │                            │
     │──── username/password ──►│                            │
     │                          │──── Access-Request ───────►│
     │                          │     (username, password,   │
     │                          │      NAS-IP, secret)       │
     │                          │                            │── Check DB
     │                          │◄─── Access-Accept ─────────│   (user found,
     │◄──── Internet Access ────│     (or Access-Reject)     │    password OK)
```

The network device (called a **NAS — Network Access Server**) forwards the user's credentials to FreeRADIUS. FreeRADIUS checks the credentials against the database and replies with Accept or Reject.

---

## 3. Add Your Device in FreeRADIUS Manager

Every device that talks to RADIUS must be registered first.

1. Log in to the web interface at `http://YOUR-SERVER-IP:8081`
2. Go to **NAS Devices** → **Add Device**
3. Fill in:
   - **Name**: Descriptive name (e.g., `Office-AP-Floor1`)
   - **IP Address**: The address the device sends RADIUS packets **from** (or a CIDR range for several devices)
   - **Shared Secret**: Click the generate button, then enter the same secret on the device
   - **Disconnect (CoA) port**: `3799` unless the device uses another
   - **Type**: Choose the vendor (mikrotik, cisco, ubiquiti, etc.)
   - **Description**: Optional notes
4. Click **Save**
5. **Restart FreeRADIUS** so it loads the new device: `docker compose restart freeradius`
   (FreeRADIUS reads the device list at startup; this takes a few seconds and only drops
   requests in flight).

> **Unregistered devices.** Requests from private addresses (10.x, 172.16–31.x, 192.168.x)
> that are not registered are still accepted with the fallback `RADIUS_SECRET` from `.env`.
> Requests from any other address are dropped with "Ignoring request from unknown client".

### Accounting and Disconnect — needed on every device

- **Accounting** (port 1813) with **interim updates** tells the system who is online and how
  much data they used. Without it, device limits, data caps, usage reports and "online now"
  do not work.
- **Disconnect / CoA** (RFC 5176, UDP 3799): the server sends a Disconnect-Request to kick a
  session — the **Disconnect** button, a customer who stops paying, and data-cap enforcement
  all use it. On the device this is usually called "incoming RADIUS", "dynamic
  authorization", "CoA" or "RFC 3576". Allow UDP 3799 from the server in the device's firewall.

---

## 4. MikroTik RouterOS

### Hotspot Authentication (Captive Portal)

This is the most common use case — users connect to WiFi and are prompted to log in.

```routeros
# Step 1: Add RADIUS server
/radius
add address=YOUR-SERVER-IP secret=your-radius-secret service=hotspot \
    authentication-port=1812 accounting-port=1813 timeout=3000ms

# Step 2: Enable RADIUS and accounting (with interim updates) on your Hotspot profile
/ip hotspot profile
set [find name=default] use-radius=yes radius-accounting=yes radius-interim-update=5m

# Step 3: Accept Disconnect requests from the server (Disconnect button, unpaid customers)
/radius incoming
set accept=yes port=3799

# Step 4: Verify RADIUS is active
/radius print
```

To use a branded login page from **Captive Portal**, replace the body of the hotspot's
`login.html` (Files → `hotspot/login.html`) with a redirect to the portal, passing the
router's variables:

```html
<html><head><meta http-equiv="refresh" content="0; url=http://YOUR-SERVER-IP:8081/api/v1/captive/serve/PORTAL-ID?link-login-only=$(link-login-only)&dst=$(link-orig-esc)"></head></html>
```

The portal posts the customer's login back to `link-login-only` (leave the portal's
"Router login URL" empty for this) and sends them on to `dst` afterwards. Add the server to
the hotspot's **walled garden** (`/ip hotspot walled-garden ip add dst-address=YOUR-SERVER-IP`)
so customers can load the page before logging in.

### PPPoE Authentication

For ISP PPPoE subscriber authentication:

```routeros
# Add RADIUS for PPPoE
/radius
add address=YOUR-SERVER-IP secret=your-radius-secret service=ppp \
    authentication-port=1812 accounting-port=1813

# Use RADIUS for PPP logins, with accounting and interim updates
/ppp aaa
set use-radius=yes accounting=yes interim-update=5m

# Accept Disconnect requests from the server
/radius incoming
set accept=yes port=3799
```

If both hotspot and PPPoE run on one router, add one `/radius` entry with
`service=hotspot,ppp`.

### Login Authentication (Admin Access)

To use RADIUS for router admin login (staff logging in to the router itself — not
usually wanted for customer accounts):

```routeros
/radius
add address=YOUR-SERVER-IP secret=your-radius-secret service=login \
    authentication-port=1812 accounting-port=1813

/user aaa
set use-radius=yes
```

### Winbox — Quick Test

In Winbox: **RADIUS** → Add → fill in server IP, secret, tick **Hotspot** or **PPP** service → Apply.

---

## 5. Cisco IOS / IOS-XE

### AAA with RADIUS for VPN / Admin Login

```cisco
! Step 1: Enable AAA
aaa new-model

! Step 2: Define RADIUS server
radius server FREERADIUS
 address ipv4 YOUR-SERVER-IP auth-port 1812 acct-port 1813
 key your-radius-secret

! Step 3: Create server group
aaa group server radius RADIUS-GROUP
 server name FREERADIUS

! Step 4: Set authentication methods
aaa authentication login default group RADIUS-GROUP local
aaa authorization exec default group RADIUS-GROUP local

! Step 5: Enable accounting
aaa accounting exec default start-stop group RADIUS-GROUP

! Step 6: Apply to VTY lines
line vty 0 15
 login authentication default

! Verify
test aaa group RADIUS-GROUP testuser testpassword new-code
```

### Wireless LAN Controller (WLC) — Enterprise WiFi

```cisco
! On Cisco WLC GUI:
! Security → AAA → RADIUS → Authentication → New
!   Server IP:   YOUR-SERVER-IP
!   Shared Secret: your-radius-secret
!   Port:         1812
!
! Then: WLANs → Your WLAN → Security → AAA Servers
!   Authentication Server 1: (select your RADIUS server)
```

---

## 6. Cisco Catalyst Switches (802.1X)

802.1X forces connected devices/users to authenticate before getting network access.

```cisco
! Enable AAA
aaa new-model

! RADIUS server
radius server FREERADIUS
 address ipv4 YOUR-SERVER-IP auth-port 1812 acct-port 1813
 key your-radius-secret

aaa group server radius RADIUS-GROUP
 server name FREERADIUS

! 802.1X authentication
aaa authentication dot1x default group RADIUS-GROUP
aaa authorization network default group RADIUS-GROUP

! Enable 802.1X globally
dot1x system-auth-control

! Apply to access port
interface GigabitEthernet1/0/1
 switchport mode access
 authentication port-control auto
 dot1x pae authenticator
 spanning-tree portfast

! Show 802.1X status
show dot1x all
show authentication sessions
```

---

## 7. Ubiquiti UniFi Access Points

### Method A — UniFi Controller (Recommended)

1. Open **UniFi Network** application
2. Go to **Settings** → **Profiles** → **RADIUS**
3. Click **Create New RADIUS Profile**:
   - **Name**: `FreeRADIUS-Manager`
   - **IP**: `YOUR-SERVER-IP`
   - **Port**: `1812`
   - **Shared Secret**: `your-radius-secret`
   - **Accounting Port**: `1813`
   - **Accounting**: Enabled
4. Go to **Settings** → **WiFi** → select your SSID (or create new)
5. Under **Security Protocol** select **WPA2 Enterprise**
6. Set **RADIUS Profile** to `FreeRADIUS-Manager`
7. Click **Apply Changes** and wait for APs to reprovision

> Users will now be prompted for a username and password when connecting to that SSID.

### Method B — UniFi Hotspot (Guest Portal)

1. **Settings** → **Guest Control** → Enable Guest Portal
2. Select **RADIUS** as authentication
3. Set the RADIUS server details as above
4. Enable **RADIUS Accounting**

---

## 8. Ubiquiti EdgeRouter

### PPPoE Server with RADIUS

```bash
# Via EdgeOS CLI
configure

set service pppoe-server authentication mode radius
set service pppoe-server authentication radius-server YOUR-SERVER-IP secret your-radius-secret
set service pppoe-server authentication radius-server YOUR-SERVER-IP port 1812

commit
save
```

### Admin Login via RADIUS

```bash
configure

set system login radius-server YOUR-SERVER-IP secret your-radius-secret
set system login radius-server YOUR-SERVER-IP port 1812
set system login radius-server YOUR-SERVER-IP timeout 5

commit
save
```

---

## 9. pfSense / OPNsense

### Captive Portal

#### pfSense

1. **Services** → **Captive Portal** → Add Zone
2. Under **Authentication**, select **RADIUS Authentication**
3. Fill in:
   - **Primary RADIUS Server**: `YOUR-SERVER-IP`
   - **Primary RADIUS Port**: `1812`
   - **Primary RADIUS Shared Secret**: `your-radius-secret`
   - **Authentication Protocol**: `PAP`
4. Enable **Send RADIUS accounting packets**
   - **Accounting Port**: `1813`
5. Save and apply

#### OPNsense

1. **Services** → **Captive Portal** → Add
2. Under **Authentication Server**: click the `+` to add a new RADIUS server
3. Fill in server IP, port 1812, shared secret
4. Select the new server in the portal settings
5. Apply

### VPN (OpenVPN) with RADIUS

#### pfSense

1. **System** → **User Manager** → **Authentication Servers** → Add
   - **Type**: RADIUS
   - **Hostname**: `YOUR-SERVER-IP`
   - **Shared Secret**: `your-radius-secret`
   - **Authentication Port**: `1812`
2. When creating OpenVPN server: set **Backend for authentication** to your RADIUS server

---

## 10. Huawei Switches & Routers

### 802.1X on Huawei Switch

```huawei
# Define RADIUS server group
radius-server template FREERADIUS
 radius-server authentication YOUR-SERVER-IP 1812
 radius-server accounting YOUR-SERVER-IP 1813
 radius-server shared-key cipher your-radius-secret

# Create AAA domain
aaa
 domain default
  authentication-scheme radius
  accounting-scheme radius
  radius-server FREERADIUS

# Enable 802.1X on interface
interface GigabitEthernet0/0/1
 dot1x enable
 dot1x authentication-method eap
 port access vlan 100

# Globally enable 802.1X
dot1x enable
```

### Admin Login via RADIUS (Huawei VRP)

```huawei
# RADIUS template
radius-server template ADMIN-RADIUS
 radius-server authentication YOUR-SERVER-IP 1812
 radius-server shared-key cipher your-radius-secret

# Apply to admin login
aaa
 authentication-scheme admin-auth
  authentication-mode radius local
 domain default_admin
  authentication-scheme admin-auth
  radius-server ADMIN-RADIUS
```

---

## 11. TP-Link Omada (EAP Series)

### Via Omada Controller

1. Open **Omada Controller**
2. Go to **Settings** → **Authentication** → **RADIUS Setting**
3. Click **Create New RADIUS Profile**:
   - **Auth Server IP**: `YOUR-SERVER-IP`
   - **Auth Server Port**: `1812`
   - **Auth Shared Secret**: `your-radius-secret`
   - **Acct Server IP**: `YOUR-SERVER-IP`
   - **Acct Server Port**: `1813`
   - **Acct Shared Secret**: `your-radius-secret`
4. Go to **Wireless Networks** → Edit your SSID
5. Set **Security** to **WPA2-Enterprise**
6. Select the RADIUS profile you created
7. Save and let the EAPs re-provision

### Via EAP Standalone Web Interface

1. Browse to the EAP's IP address
2. **Wireless** → **SSID** → Edit
3. Set **Security**: `WPA2-Enterprise`
4. **RADIUS Server IP**: `YOUR-SERVER-IP`
5. **RADIUS Server Port**: `1812`
6. **RADIUS Server Password**: `your-radius-secret`
7. Save

---

## 12. FortiGate Firewall

### RADIUS for Admin Login

```
# Via FortiGate GUI:
# User & Device → RADIUS Servers → Create New
#   Name:         FreeRADIUS
#   Server IP:    YOUR-SERVER-IP
#   Server Secret: your-radius-secret
#   Primary Server Port: 1812
#
# System → Administrators → Create New
#   Authentication: RADIUS Server → FreeRADIUS
```

### RADIUS for SSL-VPN / IPsec VPN

```
# User & Device → RADIUS Servers → Create New (as above)
#
# VPN → SSL-VPN Settings → Authentication/Portal Mapping
#   Add group: RADIUS group → Full Access portal
#
# Policy & Objects → IPv4 Policy
#   Source: SSL VPN tunnel interface + user group
```

### FortiGate CLI

```bash
config user radius
    edit "FreeRADIUS"
        set server "YOUR-SERVER-IP"
        set secret "your-radius-secret"
        set auth-port 1812
        set acct-port 1813
    next
end

config user group
    edit "RADIUS-Users"
        set member "FreeRADIUS"
    next
end
```

---

## 13. Windows Server NPS (802.1X)

Windows Network Policy Server can act as a RADIUS proxy, forwarding requests to FreeRADIUS Manager.

### Configure NPS as RADIUS Proxy

1. Open **Network Policy Server** console
2. **RADIUS Clients and Servers** → **Remote RADIUS Server Groups** → New
   - Group Name: `FreeRADIUS`
   - Add server: `YOUR-SERVER-IP`, port `1812`, secret `your-radius-secret`
3. **RADIUS Clients** → New (add your switches/APs as clients)
4. **Connection Request Policies** → New
   - Conditions: match your client IPs
   - Settings → Authentication → Forward requests to remote RADIUS group: `FreeRADIUS`

### Direct RADIUS Client (Add Switch/AP)

1. **RADIUS Clients** → New:
   - Friendly Name: your device name
   - Address: device IP
   - Shared Secret: `your-radius-secret`

---

## 14. Linux (hostapd) Access Point

For a Linux-based WiFi access point using `hostapd` with WPA2-Enterprise:

### /etc/hostapd/hostapd.conf

```ini
interface=wlan0
driver=nl80211
ssid=MySecureWiFi
hw_mode=g
channel=6
ieee80211n=1

# WPA2-Enterprise
wpa=2
wpa_key_mgmt=WPA-EAP
wpa_pairwise=CCMP
rsn_pairwise=CCMP
ieee8021x=1

# RADIUS settings
auth_server_addr=YOUR-SERVER-IP
auth_server_port=1812
auth_server_shared_secret=your-radius-secret
acct_server_addr=YOUR-SERVER-IP
acct_server_port=1813
acct_server_shared_secret=your-radius-secret
```

```bash
# Start hostapd
systemctl enable hostapd
systemctl start hostapd

# Test
hostapd_cli ping
```

---

## 15. Generic Device Template

If your device is not listed above, use these universal settings:

| Field | Value |
|-------|-------|
| RADIUS / Authentication Server | `YOUR-SERVER-IP` |
| Authentication Port | `1812` |
| Accounting Server | `YOUR-SERVER-IP` |
| Accounting Port | `1813` |
| Shared Secret / Password | `your-radius-secret` |
| Protocol | `PAP` (try `CHAP` if PAP fails) |
| Timeout | `5` seconds |
| Retries | `3` |
| NAS Identifier | Device hostname or IP |

---

## 16. Creating RADIUS Users

Before testing, you need users in the system.

### Via Web Interface

1. Log in at `http://YOUR-SERVER-IP:8081`
2. Go to **RADIUS Users** → **Add User**
3. Fill in:
   - **Username**: e.g. `john.doe` (no spaces or quotes)
   - **Password**: at least 6 characters
   - **Plan** (optional): sets speed, data cap and device count, and raises the invoice;
     without a plan, set **Paid until** (empty = never expires)
   - **Device Limit**: how many devices may be online at once
4. Click **Create**. With a paid plan you are offered to record the payment straight away;
   until it is paid the user cannot log in (unless you ticked **Start service now**).

### Via CSV Import

1. **RADIUS Users** → **Import CSV**
2. The first line must be a header. Required columns: `username,password`. Optional:
   `email,full_name,department,phone,device_limit,expiry,validity_days,plan_name,data_limit_mb,notes`
   (`expiry` as `YYYY-MM-DD`; `plan_name` applies the plan's limits but bills nothing).
3. Example:
   ```csv
   username,password,expiry,device_limit,plan_name
   john.doe,Pass@1234,2026-12-31,2,Home 10
   jane.smith,Jane@5678,2026-12-31,1,
   guest01,Guest@999,,1,
   ```

---

## 17. Testing Authentication

### Test from FreeRADIUS Manager (Built-in)

1. Go to **Settings** → **RADIUS Test** (or **Security** → **RADIUS Simulator** for the full
   attribute exchange)
2. Enter a customer's username and password
3. You should see `Access-Accept` with green status. This proves the account works; a login
   from the router additionally needs the router's secret and registration to be right.

### Test from Linux Command Line

Install `freeradius-utils` on a machine whose address FreeRADIUS accepts (a registered NAS,
or a private address using the fallback secret), then:

```bash
# Basic authentication test
radtest username password YOUR-SERVER-IP 0 your-radius-secret

# Expected success output:
# Sent Access-Request Id 123 from 0.0.0.0:port to YOUR-SERVER-IP:1812
# Received Access-Accept Id 123 from YOUR-SERVER-IP:1812

# If you get Access-Reject, the user credentials are wrong
# If you get no response / timeout, check firewall / shared secret
```

### Test from Windows PowerShell

```powershell
# Using NTRadPing (download separately) or Test-NetConnection for port check:
Test-NetConnection -ComputerName YOUR-SERVER-IP -Port 1812
# (UDP ports cannot be tested this way — use radtest from WSL or Linux)
```

---

## 18. Troubleshooting

### Authentication Fails — "Access-Reject"

| Cause | Fix |
|-------|-----|
| Wrong username or password | Check credentials in RADIUS Users |
| Paid-until date has passed | Record the payment for their invoice (or extend **Paid until**) |
| User is suspended or expired | **RADIUS Users** → Activate |
| Data cap used up | It resets when the next period is paid; or assign a plan with a bigger cap |
| Already online on the maximum number of devices | Disconnect an old session, or raise the device limit |

`docker exec radius_backend ./radius-manager access-report` lists every customer that
FreeRADIUS will refuse and why.

```bash
# Check FreeRADIUS logs for the exact reason
docker logs radius_freeradius 2>&1 | grep -i "reject\|error\|wrong" | tail -20
```

### No Response / Timeout

| Cause | Fix |
|-------|-----|
| Device not registered | Add device in NAS Devices with correct IP, then `docker compose restart freeradius` |
| Wrong shared secret | Secret on device must match its NAS Devices entry (or `RADIUS_SECRET` in `.env` for unregistered private addresses) |
| Firewall blocking UDP 1812 | Open port: `ufw allow 1812/udp && ufw allow 1813/udp` |
| Wrong server IP on device | Use the Docker host's IP, not `172.x.x.x` |
| FreeRADIUS container not running | `docker compose ps` → restart if down |

```bash
# Check if ports are open on the server
ss -ulnp | grep -E "1812|1813"

# Check Docker container status (run in the install folder)
docker compose ps

# Open firewall ports
ufw allow 1812/udp
ufw allow 1813/udp
ufw reload
```

### "Unknown Client" Error in Logs

```bash
docker logs radius_freeradius 2>&1 | grep "unknown client"
```

This means the device IP is not in the NAS Devices list (and not a private address). Register
the device in the web interface with the exact IP address the device uses to send packets,
then restart FreeRADIUS (`docker compose restart freeradius`).

### MikroTik — "No Response" Despite Correct Config

Check the MikroTik's outgoing interface IP:
```routeros
/radius print
# The "src-address" must be the IP that FreeRADIUS sees
# Register THAT IP in NAS Devices, not the management IP
```

### UniFi — Users Not Being Authenticated

- Ensure the SSID is set to **WPA2-Enterprise**, not WPA2-Personal
- Check that the UniFi controller can reach the RADIUS server IP
- Look at the UniFi controller logs: **Settings** → **Maintenance** → **Support** → **Download Logs**

### Checking Live RADIUS Traffic

```bash
# Run FreeRADIUS in debug mode in the foreground (shows every packet); Ctrl+C to stop
docker compose stop freeradius
docker compose run --rm --service-ports freeradius -X
docker compose up -d freeradius     # back to normal afterwards
```

Look for lines like:
- `Received Access-Request` — packet arrived
- `Found Auth-Type = PAP` — protocol detected
- `Expiration` / `Simultaneous-Use` / `Max-Octets` checks — why a paid-up rule refused
- `Sent Access-Accept` / `Sent Access-Reject` — final verdict

### Firewall Quick Reference

```bash
# UFW (Ubuntu/Debian)
# Better: limit RADIUS to your routers, e.g. ufw allow from 203.0.113.10 to any port 1812 proto udp
ufw allow 1812/udp comment "RADIUS Auth"
ufw allow 1813/udp comment "RADIUS Acct"
ufw allow 8081/tcp comment "RADIUS Manager Web"
# (the API, 8088, is only reachable from the server itself; do not open it)

# firewalld (CentOS/RHEL)
firewall-cmd --permanent --add-port=1812/udp
firewall-cmd --permanent --add-port=1813/udp
firewall-cmd --permanent --add-port=8081/tcp
firewall-cmd --reload

# iptables
iptables -A INPUT -p udp --dport 1812 -j ACCEPT
iptables -A INPUT -p udp --dport 1813 -j ACCEPT
```

---

## Quick Reference Card

```
╔══════════════════════════════════════════════════════════╗
║           FreeRADIUS Manager — Device Quick Config       ║
╠══════════════════════════════════════════════════════════╣
║  RADIUS Server IP   :  YOUR-SERVER-IP                    ║
║  Auth Port          :  1812 / UDP                        ║
║  Accounting Port    :  1813 / UDP                        ║
║  Disconnect (CoA)   :  3799 / UDP (on the router)        ║
║  Shared Secret      :  per device, NAS Devices page      ║
║  Protocol           :  PAP                               ║
║  Accounting         :  on, interim update 5 min          ║
║  Timeout            :  5s    Retries: 3                  ║
╠══════════════════════════════════════════════════════════╣
║  Web Interface      :  http://YOUR-SERVER-IP:8081        ║
╚══════════════════════════════════════════════════════════╝
```

---

*FreeRADIUS Manager — Device Integration Guide*
*Replace `YOUR-SERVER-IP` and `your-radius-secret` with your actual values throughout this document.*
