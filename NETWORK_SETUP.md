# Network Device Configuration Guide

This is the short version. [DEVICE-INTEGRATION.md](DEVICE-INTEGRATION.md) has step-by-step
guides for more devices, testing and troubleshooting.

## Prerequisites

1. Note your server's IP address (where FreeRADIUS Manager is running)
2. Add the device in the web UI under **NAS Devices → Add Device** with its **own** shared
   secret (use the generate button), then run `docker compose restart freeradius`.
   Unregistered devices on private addresses fall back to `RADIUS_SECRET` from `.env`.
3. On every device, turn on **accounting with interim updates** (≈5 min) and allow
   **Disconnect/CoA requests on UDP 3799** from the server. Without accounting, usage, data
   caps and device limits do not work; without CoA, the Disconnect button and cutting off
   unpaid customers do not work.

---

## MikroTik RouterOS

### WiFi Hotspot
```routeros
/radius
add address=YOUR_SERVER_IP secret=YOUR_SECRET service=hotspot

/ip hotspot profile
set hsprof1 use-radius=yes radius-accounting=yes radius-interim-update=5m

/radius incoming
set accept=yes port=3799
```

### 802.1X WiFi (WPA-Enterprise)
EAP/PEAP needs FreeRADIUS's EAP certificates; it is not yet tested with this system, so try it
on a test SSID first.
```routeros
/radius
add address=YOUR_SERVER_IP secret=YOUR_SECRET service=wireless

/interface wireless
set wlan1 security-profile=radius-auth

/interface wireless security-profiles
set radius-auth mode=dynamic-keys eap-methods=peap
```

### PPPoE / VPN
```routeros
/radius
add address=YOUR_SERVER_IP secret=YOUR_SECRET service=ppp

/ppp aaa
set use-radius=yes accounting=yes interim-update=5m

/radius incoming
set accept=yes port=3799
```

---

## Cisco IOS / IOS-XE

```cisco
! Global RADIUS configuration
aaa new-model

radius-server host YOUR_SERVER_IP auth-port 1812 acct-port 1813 key YOUR_SECRET

! For IOS-XE:
radius server RADIUS_MANAGER
 address ipv4 YOUR_SERVER_IP auth-port 1812 acct-port 1813
 key YOUR_SECRET

aaa group server radius RADIUS_GROUP
 server name RADIUS_MANAGER

! Authentication for 802.1X
aaa authentication dot1x default group RADIUS_GROUP
aaa authorization network default group RADIUS_GROUP
aaa accounting dot1x default start-stop group RADIUS_GROUP

! Enable dot1x globally
dot1x system-auth-control
```

---

## Cisco WLC (Wireless LAN Controller)

1. Go to **Security → AAA → RADIUS → Authentication**
2. Click **New** and enter:
   - Server IP: `YOUR_SERVER_IP`
   - Port: `1812`
   - Shared Secret: `YOUR_SECRET`
3. Go to **Security → AAA → RADIUS → Accounting**
4. Add server with port `1813`
5. Apply RADIUS to your WLAN: **WLANs → Edit → Security → AAA Servers**

---

## Ubiquiti UniFi

1. Open UniFi Network Controller
2. Go to **Settings → Profiles → RADIUS**
3. Create new profile:
   - Authentication Server: `YOUR_SERVER_IP`
   - Authentication Port: `1812`
   - Password (Shared Secret): `YOUR_SECRET`
   - Accounting Server: `YOUR_SERVER_IP`
   - Accounting Port: `1813`
4. Apply to WiFi network: **Settings → WiFi → Edit Network → Security → WPA Enterprise**

---

## pfSense / OPNsense

### Captive Portal
1. **Services → Captive Portal → Add Zone**
2. Under **Authentication**, select **RADIUS Authentication**
3. Primary RADIUS server: `YOUR_SERVER_IP`
4. Port: `1812`
5. Shared Key: `YOUR_SECRET`
6. Enable RADIUS accounting: `YOUR_SERVER_IP:1813`

### VPN (OpenVPN with RADIUS)
**System → User Manager → Authentication Servers → Add**, type RADIUS, with the server IP,
ports 1812/1813 and the shared secret; then select that server in the OpenVPN server's
**Backend for authentication**. (No FreeRADIUS package is needed on pfSense.)

---

## OpenVPN (Linux Server)

Install the radiusplugin:
```bash
apt-get install openvpn-auth-radius
```

`/etc/openvpn/radiusplugin.cnf`:
```ini
NAS-Identifier=vpn-server
NAS-IP-Address=YOUR_VPN_SERVER_IP
server
{
    acctport=1813
    authport=1812
    name=YOUR_SERVER_IP
    retry=3
    wait=3
    sharedsecret=YOUR_SECRET
}
```

`/etc/openvpn/server.conf` — add:
```
plugin /usr/lib/openvpn/radiusplugin.so /etc/openvpn/radiusplugin.cnf
```

---

## WireGuard (with Pre-Auth RADIUS)

WireGuard doesn't natively support RADIUS, but you can use a captive portal or pre-auth script. A common approach is to use `wg-dynamic` with a RADIUS-authenticated portal.

---

## Testing RADIUS Authentication

Easiest: **Settings → RADIUS Test** in the web UI.

From the command line (on Linux with `freeradius-utils`, from an address FreeRADIUS accepts):
```bash
radtest USERNAME PASSWORD YOUR_SERVER_IP 0 YOUR_SHARED_SECRET
```

Expected responses:
- `Access-Accept` — Authentication successful ✓
- `Access-Reject` — refused: wrong password, suspended/expired, paid-until passed, data cap
  used up, or already on the maximum number of devices ✗
- No reply (timeout) — FreeRADIUS not running, firewall, unregistered device, or wrong secret

---

## VLAN Assignment (Dynamic VLAN)

To assign VLANs based on user group, add RADIUS reply attributes:

Group membership is not managed in the web UI; put users in a group with SQL
(`INSERT INTO radusergroup (username, groupname, priority) VALUES ('alice', 'staff', 1);`)
and add group reply attributes:

```sql
INSERT INTO radgroupreply (groupname, attribute, op, value)
VALUES ('staff', 'Tunnel-Type', '=', '13');          -- VLAN

INSERT INTO radgroupreply (groupname, attribute, op, value)
VALUES ('staff', 'Tunnel-Medium-Type', '=', '6');    -- IEEE-802

INSERT INTO radgroupreply (groupname, attribute, op, value)
VALUES ('staff', 'Tunnel-Private-Group-Id', '=', '100');  -- VLAN ID 100
```

Or per-user via radreply:
```sql
INSERT INTO radreply (username, attribute, op, value)
VALUES ('alice', 'Tunnel-Private-Group-Id', '=', '200');
```

---

## Troubleshooting

### Authentication failing
1. Check the NAS device is added in the web UI with the correct shared secret
2. Test with `radtest` from the server
3. Check FreeRADIUS logs: `docker compose logs freeradius`
4. `docker exec radius_backend ./radius-manager access-report` lists refused customers and why

### "Invalid shared secret"
- The device secret must exactly match what's in NAS Devices in the web UI
- Secrets are case-sensitive
- No leading/trailing spaces

### "Access-Reject" for valid credentials
- User may be suspended or expired → activate in RADIUS Users, or record their payment
- Paid-until date passed → record the payment for their invoice
- Data cap used up → resets when the next period is paid
- Device limit may be reached → check active sessions

### NAS not reaching RADIUS server
- Firewall must allow UDP 1812 and 1813 from the NAS device IP
- Check `iptables -L` or firewall rules
- Ensure Docker port mappings are correct: `docker compose ps`
