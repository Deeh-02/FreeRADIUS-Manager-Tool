-- Router hostnames a captive portal may send the customer's login to (link-login-only), for
-- hotspots whose login page has a public-looking DNS name, such as hotspot.myisp.co.ke.
-- Private addresses, local names and registered NAS addresses are always allowed.
INSERT INTO system_settings (key, value, description) VALUES
    ('portal_router_hosts', '', 'Hotspot login hostnames the captive portal may post to, comma-separated (private IPs and local names are always allowed)')
ON CONFLICT (key) DO NOTHING;
