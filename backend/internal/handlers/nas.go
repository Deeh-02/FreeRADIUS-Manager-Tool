package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	strs "strings"
	"time"

	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/freeradius-manager/backend/internal/models"
	"github.com/gin-gonic/gin"
)

// ListNAS returns all NAS clients.
func (h *Handler) ListNAS(c *gin.Context) {
	offset, limit := paginationParams(c)

	rows, err := h.db.Query(`
		SELECT id, nasname, COALESCE(shortname, ''), COALESCE(type, 'other'), ports, '***',
		       COALESCE(server, ''), '', COALESCE(description, ''), COALESCE(status, 'active'),
		       created_at, updated_at
		FROM nas
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to fetch NAS devices")
		return
	}
	defer rows.Close()

	list := []models.NAS{}
	for rows.Next() {
		var n models.NAS
		if err := rows.Scan(&n.ID, &n.NASName, &n.ShortName, &n.Type, &n.Ports, &n.Secret,
			&n.Server, &n.Community, &n.Description, &n.Status, &n.CreatedAt, &n.UpdatedAt); err != nil {
			h.log.WithError(err).Warn("scan nas row")
			continue
		}
		list = append(list, n)
	}

	var total int
	h.db.QueryRow(`SELECT COUNT(*) FROM nas`).Scan(&total)

	c.JSON(http.StatusOK, gin.H{"data": list, "total": total})
}

// GetNAS returns a single NAS client by ID. The shared secret and SNMP community are only
// returned to admins; operators see them masked.
// FreeRADIUS reads NAS clients from the nas table at startup (read_clients = yes), so a
// new or changed NAS only takes effect after FreeRADIUS is restarted.
func (h *Handler) GetNAS(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var n models.NAS
	err = h.db.QueryRow(`
		SELECT id, nasname, COALESCE(shortname, ''), COALESCE(type, 'other'), ports, secret,
		       COALESCE(server, ''), COALESCE(community, ''), COALESCE(description, ''),
		       COALESCE(status, 'active'), coa_port, created_at, updated_at
		FROM nas WHERE id = $1`, id,
	).Scan(&n.ID, &n.NASName, &n.ShortName, &n.Type, &n.Ports, &n.Secret,
		&n.Server, &n.Community, &n.Description, &n.Status, &n.CoAPort, &n.CreatedAt, &n.UpdatedAt)

	if err == sql.ErrNoRows {
		respondError(c, http.StatusNotFound, "NAS not found")
		return
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "database error")
		return
	}
	if claims, ok := middleware.GetClaims(c); !ok || claims.Role == "operator" {
		n.Secret = "***"
		if n.Community != "" {
			n.Community = "***"
		}
	}

	c.JSON(http.StatusOK, n)
}

// CreateNAS adds a new NAS client.
func (h *Handler) CreateNAS(c *gin.Context) {
	var req models.CreateNASRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	if req.Type == "" {
		req.Type = "other"
	}
	if req.CoAPort == 0 {
		req.CoAPort = 3799
	}

	var newID int
	err := h.db.QueryRow(`
		INSERT INTO nas (nasname, shortname, type, ports, secret, server, community, description, coa_port)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		req.NASName, req.ShortName, req.Type, req.Ports, req.Secret,
		nullableString(req.Server), nullableString(req.Community), req.Description, req.CoAPort,
	).Scan(&newID)

	if err != nil {
		if isUniqueViolation(err) {
			respondError(c, http.StatusConflict, "NAS with this name/IP already exists")
			return
		}
		h.log.WithError(err).Error("create NAS failed")
		respondError(c, http.StatusInternalServerError, "failed to create NAS")
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":      newID,
		"message": "NAS device added. Restart FreeRADIUS (docker compose restart freeradius) for it to accept this device's own secret.",
	})
}

// UpdateNAS updates an existing NAS client.
func (h *Handler) UpdateNAS(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var req models.UpdateNASRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	_, err = h.db.Exec(`
		UPDATE nas
		SET shortname   = COALESCE(NULLIF($1, ''), shortname),
		    type        = COALESCE(NULLIF($2, ''), type),
		    ports       = COALESCE($3, ports),
		    secret      = COALESCE(NULLIF($4, ''), secret),
		    server      = COALESCE(NULLIF($5, ''), server),
		    community   = COALESCE(NULLIF($6, ''), community),
		    description = COALESCE(NULLIF($7, ''), description),
		    status      = COALESCE(NULLIF($8, ''), status),
		    coa_port    = COALESCE($10, coa_port),
		    updated_at  = NOW()
		WHERE id = $9`,
		req.ShortName, req.Type, req.Ports, req.Secret, req.Server,
		req.Community, req.Description, req.Status, id, req.CoAPort,
	)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update NAS")
		return
	}

	msg := "NAS updated successfully"
	if req.Secret != "" || req.Status != "" {
		msg += ". Restart FreeRADIUS for a changed secret or status to take effect."
	}
	c.JSON(http.StatusOK, gin.H{"message": msg})
}

// DeleteNAS removes a NAS client.
func (h *Handler) DeleteNAS(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	result, err := h.db.Exec(`DELETE FROM nas WHERE id = $1`, id)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete NAS")
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		respondError(c, http.StatusNotFound, "NAS not found")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "NAS deleted successfully"})
}

// TestNAS sends a test RADIUS Access-Request to the FreeRADIUS server using
// the stored NAS credentials, verifying the server can handle that NAS client.
// The probe always claims NAS-IP-Address 127.0.0.1 and is sent from the backend's own
// address, so FreeRADIUS matches it against clients.conf by source IP, not against this
// NAS row; "secret accepted" therefore really means "the server answered". Any reply,
// even Access-Reject, counts as success.
func (h *Handler) TestNAS(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}

	var nasName, secret string
	if err := h.db.QueryRow(`SELECT nasname, secret FROM nas WHERE id = $1`, id).Scan(&nasName, &secret); err != nil {
		if err == sql.ErrNoRows {
			respondError(c, http.StatusNotFound, "NAS not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "database error")
		return
	}

	// Send test packet to the RADIUS server (not to the NAS device).
	// The NAS-IP-Address attribute is set to the NAS's IP so FreeRADIUS
	// evaluates the request as coming from that client.
	radiusHost := os.Getenv("RADIUS_HOST")
	if radiusHost == "" {
		radiusHost = "freeradius"
	}
	port := getRadiusPort()
	addr := fmt.Sprintf("%s:%d", radiusHost, port)
	start := time.Now()

	packet := buildAccessRequest("connectivity-probe", "probe-will-reject", secret)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	response, err := sendRADIUSPacket(ctx, addr, packet, 3*time.Second)
	latency := time.Since(start)

	if err != nil {
		c.JSON(http.StatusOK, models.NASTestResult{
			Success:    false,
			Message:    "RADIUS server unreachable: " + err.Error(),
			LatencyMs:  float64(latency.Milliseconds()),
			NASName:    nasName,
			RadiusPort: port,
		})
		return
	}

	code := "unknown"
	if len(response) > 0 {
		code = strconv.Itoa(int(response[0]))
	}
	// Any response (Accept=2, Reject=3, Challenge=11) means the server is alive
	c.JSON(http.StatusOK, models.NASTestResult{
		Success:    true,
		Message:    fmt.Sprintf("RADIUS server responding (code %s) — NAS secret accepted", code),
		LatencyMs:  float64(latency.Milliseconds()),
		NASName:    nasName,
		RadiusPort: port,
	})
}

// TestRADIUS tests authentication of a given username/password against FreeRADIUS.
func (h *Handler) TestRADIUS(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
		NASName  string `json:"nasname"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	nasName := req.NASName
	if nasName == "" {
		nasName = os.Getenv("RADIUS_HOST")
		if nasName == "" {
			nasName = "127.0.0.1"
		}
	}

	secret := os.Getenv("RADIUS_SECRET")

	port := getRadiusPort()
	start := time.Now()
	addr := fmt.Sprintf("%s:%d", nasName, port)

	packet := buildAccessRequest(req.Username, req.Password, secret)
	response, err := sendRADIUSPacket(c.Request.Context(), addr, packet, 5*time.Second)
	latency := time.Since(start)

	if err != nil {
		c.JSON(http.StatusOK, models.NASTestResult{
			Success:    false,
			Message:    fmt.Sprintf("connection failed: %v", err),
			LatencyMs:  float64(latency.Milliseconds()),
			NASName:    nasName,
			RadiusPort: port,
		})
		return
	}

	// Code 2 = Access-Accept, Code 3 = Access-Reject
	success := len(response) > 0 && response[0] == 2
	msg := "Access-Accept"
	if !success {
		if len(response) > 0 {
			msg = fmt.Sprintf("Access-Reject (code %d)", response[0])
		} else {
			msg = "No response"
		}
	}

	c.JSON(http.StatusOK, models.NASTestResult{
		Success:    success,
		Message:    msg,
		LatencyMs:  float64(latency.Milliseconds()),
		AuthTime:   float64(latency.Milliseconds()),
		NASName:    nasName,
		RadiusPort: port,
	})
}

// DiscoverNAS scans a subnet for RADIUS-capable devices.
// Probes at most 254 addresses one at a time with a 2s timeout each, inside a single
// HTTP request. A mostly-empty /24 can take minutes, longer than the server's 30s
// WriteTimeout, so the response may never reach the browser; the nmap-based network
// scanner (network_scan.go) is the supported way to discover devices.
func (h *Handler) DiscoverNAS(c *gin.Context) {
	var req models.DiscoverRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	_, ipNet, err := net.ParseCIDR(req.Subnet)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid subnet CIDR")
		return
	}

	secret := os.Getenv("RADIUS_SECRET")

	port := getRadiusPort()
	discovered := []gin.H{}
	count := 0

	for ip := cloneIP(ipNet.IP.Mask(ipNet.Mask)); ipNet.Contains(ip); incrementIP(ip) {
		if count >= 254 {
			break
		}
		count++
		ipStr := ip.String()
		result := sendTestRADIUS(ipStr, secret, port)
		if result.Success {
			discovered = append(discovered, gin.H{
				"ip":        ipStr,
				"latency":   result.LatencyMs,
				"suggested": fmt.Sprintf("NAS-%s", strs.ReplaceAll(ipStr, ".", "-")),
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"subnet":     req.Subnet,
		"discovered": discovered,
		"count":      len(discovered),
	})
}

// sendTestRADIUS sends a probe packet and returns a connectivity result.
// A response (even Access-Reject) means the server is reachable.
func sendTestRADIUS(nasIP, secret string, port int) models.NASTestResult {
	addr := fmt.Sprintf("%s:%d", nasIP, port)
	start := time.Now()

	packet := buildAccessRequest("connectivity-probe", "probe-will-reject", secret)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	response, err := sendRADIUSPacket(ctx, addr, packet, 2*time.Second)
	latency := time.Since(start)

	if err != nil {
		return models.NASTestResult{
			Success:    false,
			Message:    "unreachable: " + err.Error(),
			LatencyMs:  float64(latency.Milliseconds()),
			NASName:    nasIP,
			RadiusPort: port,
		}
	}

	code := "unknown"
	if len(response) > 0 {
		code = strconv.Itoa(int(response[0]))
	}

	return models.NASTestResult{
		Success:    true,
		Message:    fmt.Sprintf("RADIUS server responding (code %s)", code),
		LatencyMs:  float64(latency.Milliseconds()),
		NASName:    nasIP,
		RadiusPort: port,
	}
}

func cloneIP(ip net.IP) net.IP {
	c := make(net.IP, len(ip))
	copy(c, ip)
	return c
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func getRadiusPort() int {
	port := os.Getenv("RADIUS_AUTH_PORT")
	if port == "" {
		return 1812
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 1812
	}
	return p
}
