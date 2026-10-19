package handlers

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// RADIUS Simulator — builds and sends real UDP Access-Request packets
// ─────────────────────────────────────────────────────────────────────────────

// SimResult holds the full breakdown of a simulated RADIUS exchange.
type SimResult struct {
	Success      bool              `json:"success"`
	Reply        string            `json:"reply"`
	LatencyMs    float64           `json:"latency_ms"`
	RequestAttrs map[string]string `json:"request_attrs"`
	ReplyAttrs   map[string]string `json:"reply_attrs"`
	RawRequest   string            `json:"raw_request_hex"`
	RawReply     string            `json:"raw_reply_hex"`
	Error        string            `json:"error,omitempty"`
}

// SimulateAuth sends an Access-Request and returns a detailed breakdown.
// Any admin can aim this at any host:port with a caller-supplied secret, so it doubles as a
// UDP probe from the backend's network position. A real RADIUS user's password is sent
// (encrypted with the shared secret) exactly as a NAS would, so test accounts are safer to use.
func (h *Handler) SimulateAuth(c *gin.Context) {
	var req struct {
		Username    string `json:"username" binding:"required"`
		Password    string `json:"password" binding:"required"`
		NASIP       string `json:"nas_ip"`
		NASPort     int    `json:"nas_port"`
		Secret      string `json:"secret"`
		CalledID    string `json:"called_station_id"`
		CallingID   string `json:"calling_station_id"`
		TimeoutMs   int    `json:"timeout_ms"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	radiusHost := os.Getenv("RADIUS_HOST")
	if radiusHost == "" {
		radiusHost = "freeradius"
	}
	if req.NASIP == "" {
		req.NASIP = radiusHost
	}
	if req.NASPort == 0 {
		req.NASPort = getRadiusPort()
	}
	if req.Secret == "" {
		req.Secret = os.Getenv("RADIUS_SECRET")
	}
	if req.TimeoutMs == 0 {
		req.TimeoutMs = 5000
	}

	start := time.Now()
	result := runRADIUSSimulation(req.NASIP, req.NASPort, req.Secret, req.Username, req.Password,
		req.CalledID, req.CallingID, time.Duration(req.TimeoutMs)*time.Millisecond)
	result.LatencyMs = float64(time.Since(start).Milliseconds())

	c.JSON(http.StatusOK, result)
}

// SimulateBatch tests up to 20 username/password pairs.
func (h *Handler) SimulateBatch(c *gin.Context) {
	var req struct {
		Pairs []struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"pairs" binding:"required,min=1,max=20"`
		Secret string `json:"secret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}

	radiusHost := os.Getenv("RADIUS_HOST")
	if radiusHost == "" {
		radiusHost = "freeradius"
	}
	secret := req.Secret
	if secret == "" {
		secret = os.Getenv("RADIUS_SECRET")
	}

	results := []SimResult{}
	for _, pair := range req.Pairs {
		r := runRADIUSSimulation(radiusHost, getRadiusPort(), secret, pair.Username, pair.Password, "", "", 3*time.Second)
		results = append(results, r)
	}

	accepts := 0
	for _, r := range results {
		if r.Reply == "Access-Accept" {
			accepts++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"results":  results,
		"total":    len(results),
		"accepted": accepts,
		"rejected": len(results) - accepts,
	})
}

// runRADIUSSimulation builds an Access-Request and decodes the reply.
func runRADIUSSimulation(host string, port int, secret, username, password, calledID, callingID string, timeout time.Duration) SimResult {
	result := SimResult{
		RequestAttrs: map[string]string{},
		ReplyAttrs:   map[string]string{},
	}

	authenticator := make([]byte, 16)
	rand.Read(authenticator)

	attrs := []radAttr{
		{attrUserName, []byte(username)},
		{attrUserPassword, encodePassword(password, secret, authenticator)},
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			attrs = append(attrs, radAttr{attrNASIPAddress, ip4})
		}
	}
	portBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(portBytes, 1)
	attrs = append(attrs, radAttr{attrNASPort, portBytes}, radAttr{attrServiceType, []byte{0, 0, 0, 2}})
	if calledID != "" {
		attrs = append(attrs, radAttr{attrCalledStationID, []byte(calledID)})
	}
	if callingID != "" {
		attrs = append(attrs, radAttr{attrCallingStationID, []byte(callingID)})
	}
	pkt, err := buildPacket(codeAccessRequest, secret, attrs, authenticator)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	result.RawRequest = fmt.Sprintf("%x", pkt)
	result.RequestAttrs["User-Name"] = username
	result.RequestAttrs["NAS-IP-Address"] = host
	result.RequestAttrs["Service-Type"] = "Framed-User"
	result.RequestAttrs["Message-Authenticator"] = "(signed)"
	if calledID != "" {
		result.RequestAttrs["Called-Station-Id"] = calledID
	}
	if callingID != "" {
		result.RequestAttrs["Calling-Station-Id"] = callingID
	}

	addr := net.JoinHostPort(host, fmt.Sprint(port))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	reply, err := sendRADIUSPacket(ctx, addr, pkt, timeout)
	if err != nil {
		result.Error = "no reply (timeout, unreachable, or wrong shared secret): " + err.Error()
		return result
	}

	result.RawReply = fmt.Sprintf("%x", reply)
	if err := verifyResponse(pkt, reply, secret); err != nil {
		result.Error = err.Error()
		return result
	}
	result.Success = true

	switch reply[0] {
	case codeAccessAccept:
		result.Reply = "Access-Accept"
	case codeAccessReject:
		result.Reply = "Access-Reject"
	case codeAccessChallenge:
		result.Reply = "Access-Challenge"
	default:
		result.Reply = fmt.Sprintf("Code-%d", reply[0])
	}

	if len(reply) > 20 {
		decodeReplyAttrs(reply[20:], result.ReplyAttrs)
	}

	return result
}

// decodeReplyAttrs parses RADIUS TLV attributes into a human-readable map.
func decodeReplyAttrs(data []byte, attrs map[string]string) {
	attrNames := map[byte]string{
		1: "User-Name", 4: "NAS-IP-Address", 5: "NAS-Port",
		6: "Service-Type", 8: "Framed-IP-Address", 18: "Reply-Message", 80: "Message-Authenticator",
		25: "Class", 26: "Vendor-Specific", 27: "Session-Timeout",
		28: "Idle-Timeout", 30: "Called-Station-Id", 31: "Calling-Station-Id",
	}
	for i := 0; i+2 <= len(data); {
		t := data[i]
		l := int(data[i+1])
		if l < 2 || i+l > len(data) {
			break
		}
		val := data[i+2 : i+l]
		name, ok := attrNames[t]
		if !ok {
			name = fmt.Sprintf("Attr-%d", t)
		}
		attrs[name] = printableVal(val)
		i += l
	}
}

func printableVal(b []byte) string {
	for _, c := range b {
		if c < 32 || c > 126 {
			return fmt.Sprintf("0x%x", b)
		}
	}
	return string(b)
}
