package handlers

import (
	"bytes"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Captive Portal
// ─────────────────────────────────────────────────────────────────────────────

type CaptivePortal struct {
	ID           int       `json:"id"`
	ZoneID       *int      `json:"zone_id"`
	ZoneName     *string   `json:"zone_name"`
	Name         string    `json:"name"`
	Title        string    `json:"title"`
	Subtitle     *string   `json:"subtitle"`
	LogoURL      *string   `json:"logo_url"`
	BgColor      string    `json:"bg_color"`
	PrimaryColor string    `json:"primary_color"`
	AuthType     string    `json:"auth_type"`
	RedirectURL  string    `json:"redirect_url"`
	TermsText    *string   `json:"terms_text"`
	FooterText   *string   `json:"footer_text"`
	LoginURL     string    `json:"login_url"`
	IsActive     bool      `json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
}

// ListCaptivePortals returns all captive portal configs.
func (h *Handler) ListCaptivePortals(c *gin.Context) {
	rows, err := h.db.Query(`
		SELECT cp.id, cp.zone_id, hz.name, cp.name, cp.title, cp.subtitle,
		       cp.logo_url, cp.bg_color, cp.primary_color, cp.auth_type,
		       cp.redirect_url, cp.terms_text, cp.footer_text, cp.login_url, cp.is_active, cp.created_at
		FROM captive_portals cp
		LEFT JOIN hotspot_zones hz ON hz.id = cp.zone_id
		ORDER BY cp.is_active DESC, cp.name`)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch portals")
		return
	}
	defer rows.Close()

	portals := []CaptivePortal{}
	for rows.Next() {
		var p CaptivePortal
		if rows.Scan(&p.ID, &p.ZoneID, &p.ZoneName, &p.Name, &p.Title, &p.Subtitle,
			&p.LogoURL, &p.BgColor, &p.PrimaryColor, &p.AuthType,
			&p.RedirectURL, &p.TermsText, &p.FooterText, &p.LoginURL, &p.IsActive, &p.CreatedAt) == nil {
			portals = append(portals, p)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": portals})
}

// CreateCaptivePortal creates a new portal configuration.
func (h *Handler) CreateCaptivePortal(c *gin.Context) {
	var req struct {
		ZoneID       *int   `json:"zone_id"`
		Name         string `json:"name" binding:"required,min=2"`
		Title        string `json:"title"`
		Subtitle     string `json:"subtitle"`
		LogoURL      string `json:"logo_url"`
		BgColor      string `json:"bg_color"`
		PrimaryColor string `json:"primary_color"`
		AuthType     string `json:"auth_type"`
		RedirectURL  string `json:"redirect_url"`
		TermsText    string `json:"terms_text"`
		FooterText   string `json:"footer_text"`
		LoginURL     string `json:"login_url"`
		IsActive     *bool  `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePortalInput(req.BgColor, req.PrimaryColor, req.AuthType, req.LoginURL, req.RedirectURL, req.LogoURL); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if req.BgColor == "" {
		req.BgColor = "#f0f4ff"
	}
	if req.PrimaryColor == "" {
		req.PrimaryColor = "#3b82f6"
	}
	if req.AuthType == "" {
		req.AuthType = "userpass"
	}
	if req.Title == "" {
		req.Title = "WiFi Login"
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	var id int
	if err := h.db.QueryRow(`INSERT INTO captive_portals
		(zone_id,name,title,subtitle,logo_url,bg_color,primary_color,auth_type,redirect_url,terms_text,footer_text,login_url,is_active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		req.ZoneID, req.Name, req.Title,
		nullableString(req.Subtitle), nullableString(req.LogoURL),
		req.BgColor, req.PrimaryColor, req.AuthType,
		req.RedirectURL, nullableString(req.TermsText),
		nullableString(req.FooterText), req.LoginURL, isActive).Scan(&id); err != nil {
		h.log.WithError(err).Error("create captive portal")
		respondError(c, http.StatusInternalServerError, "failed to create portal")
		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": id, "message": "captive portal created"})
}

// UpdateCaptivePortal updates portal settings.
func (h *Handler) UpdateCaptivePortal(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name         string  `json:"name"`
		Title        string  `json:"title"`
		Subtitle     string  `json:"subtitle"`
		LogoURL      string  `json:"logo_url"`
		BgColor      string  `json:"bg_color"`
		PrimaryColor string  `json:"primary_color"`
		AuthType     string  `json:"auth_type"`
		RedirectURL  string  `json:"redirect_url"`
		TermsText    string  `json:"terms_text"`
		FooterText   string  `json:"footer_text"`
		LoginURL     *string `json:"login_url"` // omitted = unchanged, "" = use the router's link-login-only
		IsActive     *bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePortalInput(req.BgColor, req.PrimaryColor, req.AuthType, derefStr(req.LoginURL), req.RedirectURL, req.LogoURL); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	res, err := h.db.Exec(`UPDATE captive_portals SET
		name=COALESCE(NULLIF($1,''),name),
		title=COALESCE(NULLIF($2,''),title),
		subtitle=COALESCE(NULLIF($3,''),subtitle),
		logo_url=COALESCE(NULLIF($4,''),logo_url),
		bg_color=COALESCE(NULLIF($5,''),bg_color),
		primary_color=COALESCE(NULLIF($6,''),primary_color),
		auth_type=COALESCE(NULLIF($7,''),auth_type),
		redirect_url=COALESCE(NULLIF($8,''),redirect_url),
		terms_text=COALESCE(NULLIF($9,''),terms_text),
		footer_text=COALESCE(NULLIF($10,''),footer_text),
		is_active=$11,
		login_url=COALESCE($13,login_url)
		WHERE id=$12`,
		req.Name, req.Title, req.Subtitle, req.LogoURL,
		req.BgColor, req.PrimaryColor, req.AuthType,
		req.RedirectURL, req.TermsText, req.FooterText, isActive, id, req.LoginURL)
	if err != nil {
		h.log.WithError(err).Error("update captive portal")
		respondError(c, http.StatusInternalServerError, "failed to update portal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(c, http.StatusNotFound, "portal not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "portal updated"})
}

// DeleteCaptivePortal removes a portal config.
func (h *Handler) DeleteCaptivePortal(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	res, err := h.db.Exec(`DELETE FROM captive_portals WHERE id=$1`, id)
	if !writeOK(c, res, err, "portal") {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "portal deleted"})
}

// ServeCaptivePortal generates and returns the live HTML captive portal page.
// This is a PUBLIC endpoint — no auth required.
func (h *Handler) ServeCaptivePortal(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		c.String(http.StatusBadRequest, "invalid portal ID")
		return
	}

	var p CaptivePortal
	err = h.db.QueryRow(`SELECT id,zone_id,NULL,name,title,subtitle,logo_url,bg_color,primary_color,
		auth_type,redirect_url,terms_text,footer_text,login_url,is_active,created_at
		FROM captive_portals WHERE id=$1 AND is_active=true`, id).
		Scan(&p.ID, &p.ZoneID, &p.ZoneName, &p.Name, &p.Title, &p.Subtitle,
			&p.LogoURL, &p.BgColor, &p.PrimaryColor, &p.AuthType,
			&p.RedirectURL, &p.TermsText, &p.FooterText, &p.LoginURL, &p.IsActive, &p.CreatedAt)
	if err != nil {
		c.String(http.StatusNotFound, "portal not found or inactive")
		return
	}

	// MikroTik "external login page" setups redirect here with the router's own login URL in
	// ?link-login-only=...; it is used when the portal has no fixed login_url, and only if it
	// points at a router (see routerLoginHost). Any address used to be taken, so a crafted link
	// showed this genuine page with the form sending the customer's password elsewhere.
	if p.LoginURL == "" {
		if l := c.Query("link-login-only"); l != "" {
			if u, err := url.Parse(l); err == nil && (u.Scheme == "http" || u.Scheme == "https") && routerLoginHost(h.db, u.Hostname()) {
				p.LoginURL = l
			} else {
				h.log.Warnf("captive portal %d: refused link-login-only %q (not a router address; add its hostname to the portal_router_hosts setting or set the portal's login URL)", id, l)
			}
		}
	}
	html, err := buildCaptivePortalHTML(p, c.Query("dst"))
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to render portal")
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

func validatePortalInput(bg, primary, authType, loginURL, redirectURL, logoURL string) error {
	for _, col := range []string{bg, primary} {
		if col != "" && !hexColorRe.MatchString(col) {
			return fmt.Errorf("colours must be hex values like #3b82f6")
		}
	}
	if authType != "" && authType != "userpass" && authType != "voucher" && authType != "both" {
		return fmt.Errorf("auth_type must be userpass, voucher or both")
	}
	for name, u := range map[string]string{"login_url": loginURL, "redirect_url": redirectURL, "logo_url": logoURL} {
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s must be an http(s) URL", name)
		}
	}
	return nil
}

// captivePortalTemplate is the public hotspot login page. html/template escapes every value
// (titles, terms, URLs, colours) for its context. The form posts the credentials to the
// ROUTER's hotspot login (login_url, e.g. MikroTik http://<hotspot-dns>/login), because it is
// the router that asks FreeRADIUS and then lets the device online. A voucher code is sent as
// both username and password, which is how vouchers are stored (see GenerateVouchers).
var captivePortalTemplate = template.Must(template.New("portal").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { min-height: 100vh; display: flex; flex-direction: column; align-items: center;
         justify-content: center; background: {{.BgColor}}; font-family: -apple-system,sans-serif; padding: 20px; }
  .card { background: #fff; border-radius: 20px; padding: 40px 36px; width: 100%;
          max-width: 380px; box-shadow: 0 20px 60px rgba(0,0,0,0.1); }
  input { width:100%; padding:12px 16px; border:1.5px solid #e2e8f0; border-radius:10px; font-size:15px; margin-bottom:12px; }
  button[type=submit] { width: 100%; padding: 14px; background: {{.PrimaryColor}}; color: #fff;
                        border: none; border-radius: 10px; font-size: 16px; font-weight: 600; cursor: pointer; }
  button[type=submit]:hover { opacity: 0.9; }
  .tabs { display:flex; gap:8px; margin-bottom:16px; }
  .tabs button { flex:1; padding:8px; border-radius:8px; border:2px solid #e2e8f0; background:#fff; color:#64748b; cursor:pointer; font-size:13px; }
  .tabs button.on { border-color: {{.PrimaryColor}}; background: {{.PrimaryColor}}; color:#fff; }
  .footer { margin-top: 24px; font-size: 12px; color: #94a3b8; }
  .terms { font-size:11px; color:#94a3b8; text-align:center; margin-top:16px; }
</style>
</head>
<body>
<div class="card">
  <div style="text-align:center;margin-bottom:24px;">
    {{if .LogoURL}}<img src="{{.LogoURL}}" alt="logo" style="max-height:80px;margin-bottom:16px;border-radius:8px;">{{end}}
    <h1 style="font-size:22px;font-weight:700;color:#0f172a;">{{.Title}}</h1>
    {{if .Subtitle}}<p style="color:#64748b;font-size:14px;margin-top:4px;">{{.Subtitle}}</p>{{end}}
  </div>
  {{if not .LoginURL}}<p style="color:#b91c1c;font-size:13px;margin-bottom:12px;">This page has no router login URL configured yet.</p>{{end}}
  <form id="f" method="POST" action="{{.LoginURL}}" onsubmit="return prep()">
    <input type="hidden" name="dst" value="{{.Dst}}">
    <input type="hidden" name="popup" value="true">
    {{if eq .AuthType "both"}}
    <div class="tabs"><button type="button" id="t-user" class="on" onclick="tab('user')">Username</button>
      <button type="button" id="t-voucher" onclick="tab('voucher')">Voucher</button></div>
    {{end}}
    <div id="fields-user" {{if eq .AuthType "voucher"}}style="display:none"{{end}}>
      <input name="username" id="username" type="text" placeholder="Username" autocomplete="username">
      <input name="password" id="password" type="password" placeholder="Password" autocomplete="current-password">
    </div>
    <div id="fields-voucher" {{if ne .AuthType "voucher"}}style="display:none"{{end}}>
      <input id="voucher" type="text" placeholder="Voucher code" autocomplete="off">
    </div>
    <button type="submit">Connect to Internet</button>
  </form>
  {{if .Terms}}<p class="terms">{{.Terms}}</p>{{end}}
</div>
<p class="footer">{{.Footer}}</p>
<script>
var mode = {{.AuthType}} === 'voucher' ? 'voucher' : 'user';
function tab(t) {
  mode = t;
  document.getElementById('fields-user').style.display = t === 'user' ? '' : 'none';
  document.getElementById('fields-voucher').style.display = t === 'voucher' ? '' : 'none';
  document.getElementById('t-user').className = t === 'user' ? 'on' : '';
  document.getElementById('t-voucher').className = t === 'voucher' ? 'on' : '';
}
function prep() {
  if (mode === 'voucher') {
    var v = document.getElementById('voucher').value.trim().toUpperCase();
    if (!v) return false;
    document.getElementById('username').value = v;
    document.getElementById('password').value = v;
  }
  return document.getElementById('username').value !== '';
}
</script>
</body>
</html>`))

// buildCaptivePortalHTML renders the public login page. dst is where the router should send
// the device after login (MikroTik passes it to the login page); it falls back to
// redirect_url.
func buildCaptivePortalHTML(p CaptivePortal, dst string) (string, error) {
	str := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	footer := str(p.FooterText)
	if footer == "" {
		footer = "Powered by RADIUS Manager"
	}
	if dst == "" {
		dst = p.RedirectURL
	}
	data := map[string]interface{}{
		"Title": p.Title, "Subtitle": str(p.Subtitle), "BgColor": template.CSS(safeColor(p.BgColor, "#f0f4ff")),
		"PrimaryColor": template.CSS(safeColor(p.PrimaryColor, "#3b82f6")), "LogoURL": str(p.LogoURL),
		"LoginURL": p.LoginURL, "AuthType": p.AuthType, "Dst": dst, "Terms": str(p.TermsText), "Footer": footer,
	}
	var buf bytes.Buffer
	if err := captivePortalTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func safeColor(c, def string) string {
	if hexColorRe.MatchString(c) {
		return c
	}
	return def
}

// routerLoginHost reports whether a link-login-only host can be the hotspot router itself: a
// private, CGNAT or link-local address, a local name (no dot, .local, .lan, .home.arpa,
// .internal), a registered NAS, or a hostname staff listed in the portal_router_hosts setting
// (for a hotspot whose login page has a public-looking DNS name).
func routerLoginHost(q dbtx, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip) {
			return true
		}
	} else {
		// A dot-less name such as "wifi" or "hotspot", starting with a letter: browsers read a
		// bare number (134744072, 0x08080808) as an IPv4 address, which would let any public
		// server through.
		if !strings.Contains(host, ".") {
			return localName.MatchString(host)
		}
		for _, suffix := range []string{".local", ".lan", ".home.arpa", ".internal"} {
			if strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	for _, allowed := range strings.Split(settings.String("portal_router_hosts", "", ""), ",") {
		if strings.ToLower(strings.TrimSuffix(strings.TrimSpace(allowed), ".")) == host {
			return true
		}
	}
	var known bool
	q.QueryRow(`SELECT EXISTS (SELECT 1 FROM nas WHERE LOWER(nasname) = $1)`, host).Scan(&known)
	return known
}

var localName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()
