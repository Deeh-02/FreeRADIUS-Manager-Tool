package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// RADIUS packets (RFC 2865 / 3579 / 5176), hand-rolled to avoid a library dependency.
// ─────────────────────────────────────────────────────────────────────────────

const (
	codeAccessRequest     = 1
	codeAccessAccept      = 2
	codeAccessReject      = 3
	codeAccessChallenge   = 11
	codeStatusServer      = 12
	codeDisconnectRequest = 40
	codeDisconnectACK     = 41
	codeDisconnectNAK     = 42

	attrUserName             = 1
	attrUserPassword         = 2
	attrNASIPAddress         = 4
	attrNASPort              = 5
	attrServiceType          = 6
	attrFramedIPAddress      = 8
	attrReplyMessage         = 18
	attrCalledStationID      = 30
	attrCallingStationID     = 31
	attrAcctSessionID        = 44
	attrMessageAuthenticator = 80
)

type radAttr struct {
	Type  byte
	Value []byte
}

func appendAttr(buf []byte, attrType byte, value []byte) []byte {
	if len(value) > 253 {
		value = value[:253]
	}
	buf = append(buf, attrType, byte(2+len(value)))
	return append(buf, value...)
}

// encodePassword encodes User-Password per RFC 2865 §5.2.
func encodePassword(password, secret string, authenticator []byte) []byte {
	pw := []byte(password)
	if len(pw) > 128 {
		pw = pw[:128]
	}
	padLen := ((len(pw) + 15) / 16) * 16
	if padLen == 0 {
		padLen = 16
	}
	padded := make([]byte, padLen)
	copy(padded, pw)

	result := make([]byte, padLen)
	prev := authenticator
	for i := 0; i < padLen; i += 16 {
		h := md5.New()
		h.Write([]byte(secret))
		h.Write(prev)
		b := h.Sum(nil)
		for j := 0; j < 16; j++ {
			result[i+j] = padded[i+j] ^ b[j]
		}
		prev = result[i : i+16]
	}
	return result
}

// buildPacket assembles a packet with a trailing Message-Authenticator. For Access-Request
// the Request Authenticator is random and the Message-Authenticator is the HMAC over the
// packet. For Disconnect/CoA requests (RFC 5176 §3.5) both are computed with the
// authenticator field zeroed, then the Request Authenticator is the MD5 of the packet with
// the final Message-Authenticator in place.
func buildPacket(code byte, secret string, attrs []radAttr, authenticator []byte) ([]byte, error) {
	idb := make([]byte, 1)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	body := []byte{}
	for _, a := range attrs {
		body = appendAttr(body, a.Type, a.Value)
	}
	maOffset := 20 + len(body) + 2
	body = appendAttr(body, attrMessageAuthenticator, make([]byte, 16))

	pkt := make([]byte, 20, 20+len(body))
	pkt[0] = code
	pkt[1] = idb[0]
	binary.BigEndian.PutUint16(pkt[2:4], uint16(20+len(body)))
	randomAuth := code == codeAccessRequest || code == codeStatusServer
	if randomAuth {
		copy(pkt[4:20], authenticator)
	} // otherwise left as 16 zero octets for the HMAC
	pkt = append(pkt, body...)

	mac := hmac.New(md5.New, []byte(secret))
	mac.Write(pkt)
	copy(pkt[maOffset:maOffset+16], mac.Sum(nil))

	if !randomAuth {
		h := md5.New()
		h.Write(pkt)
		h.Write([]byte(secret))
		copy(pkt[4:20], h.Sum(nil))
	}
	return pkt, nil
}

// buildAccessRequest constructs an Access-Request with User-Name, User-Password,
// NAS-IP-Address and Message-Authenticator (so it is accepted when the client entry has
// require_message_authenticator = yes).
func buildAccessRequest(username, password, secret string, extra ...radAttr) []byte {
	authenticator := make([]byte, 16)
	rand.Read(authenticator)
	attrs := []radAttr{
		{attrUserName, []byte(username)},
		{attrUserPassword, encodePassword(password, secret, authenticator)},
		{attrNASIPAddress, net.ParseIP("127.0.0.1").To4()},
	}
	attrs = append(attrs, extra...)
	pkt, _ := buildPacket(codeAccessRequest, secret, attrs, authenticator)
	return pkt
}

// radiusAlive sends Status-Server (RFC 5997) to FreeRADIUS and reports whether it answered.
func radiusAlive() error {
	host := os.Getenv("RADIUS_HOST")
	if host == "" {
		host = "freeradius"
	}
	secret := os.Getenv("RADIUS_SECRET")
	auth := make([]byte, 16)
	rand.Read(auth)
	pkt, err := buildPacket(codeStatusServer, secret, nil, auth)
	if err != nil {
		return err
	}
	reply, err := sendRADIUSPacket(context.Background(), net.JoinHostPort(host, fmt.Sprint(getRadiusPort())), pkt, 2*time.Second)
	if err != nil {
		return err
	}
	return verifyResponse(pkt, reply, secret)
}

// verifyResponse checks a reply's Response Authenticator (and Message-Authenticator when
// present) against the request, so a spoofed or garbled reply is not trusted.
func verifyResponse(request, reply []byte, secret string) error {
	if len(reply) < 20 {
		return errors.New("short reply")
	}
	if reply[1] != request[1] {
		return errors.New("reply identifier does not match request")
	}
	length := int(binary.BigEndian.Uint16(reply[2:4]))
	if length < 20 || length > len(reply) {
		return errors.New("bad reply length")
	}
	reply = reply[:length]
	h := md5.New()
	h.Write(reply[0:4])
	h.Write(request[4:20])
	h.Write(reply[20:])
	h.Write([]byte(secret))
	if !hmac.Equal(h.Sum(nil), reply[4:20]) {
		return errors.New("reply authenticator invalid (wrong shared secret?)")
	}
	for i := 20; i+2 <= len(reply); {
		t, l := reply[i], int(reply[i+1])
		if l < 2 || i+l > len(reply) {
			break
		}
		if t == attrMessageAuthenticator && l == 18 {
			cp := append([]byte(nil), reply...)
			copy(cp[4:20], request[4:20])
			for j := i + 2; j < i+18; j++ {
				cp[j] = 0
			}
			mac := hmac.New(md5.New, []byte(secret))
			mac.Write(cp)
			if !hmac.Equal(mac.Sum(nil), reply[i+2:i+18]) {
				return errors.New("reply Message-Authenticator invalid")
			}
		}
		i += l
	}
	return nil
}

// sendRADIUSPacket sends a RADIUS packet over UDP and waits for a response.
func sendRADIUSPacket(ctx context.Context, addr string, packet []byte, timeout time.Duration) ([]byte, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)

	if _, err := conn.Write(packet); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Disconnect-Request (RFC 5176): actually kicking a session off the router
// ─────────────────────────────────────────────────────────────────────────────
//
// The router must accept it: MikroTik `/radius incoming set accept=yes port=3799`; most
// others have a "dynamic authorization" / "CoA" setting. The secret is the NAS row's own
// secret if the router is registered on the NAS Devices page, otherwise RADIUS_SECRET.

type disconnectResult struct {
	Acked   bool   // router confirmed (Disconnect-ACK)
	Message string // human-readable outcome
}

func disconnectSession(db dbtx, nasIP, username, acctSessionID, framedIP string) disconnectResult {
	secret := os.Getenv("RADIUS_SECRET")
	port := 3799
	var nasSecret sql.NullString
	var coaPort sql.NullInt64
	if err := db.QueryRow(`SELECT secret, coa_port FROM nas WHERE nasname = $1`, nasIP).Scan(&nasSecret, &coaPort); err == nil {
		if nasSecret.Valid && nasSecret.String != "" {
			secret = nasSecret.String
		}
		if coaPort.Valid && coaPort.Int64 > 0 {
			port = int(coaPort.Int64)
		}
	}
	if secret == "" {
		return disconnectResult{Message: "no shared secret for NAS " + nasIP}
	}

	attrs := []radAttr{{attrUserName, []byte(username)}}
	if acctSessionID != "" {
		attrs = append(attrs, radAttr{attrAcctSessionID, []byte(acctSessionID)})
	}
	if ip := net.ParseIP(nasIP).To4(); ip != nil {
		attrs = append(attrs, radAttr{attrNASIPAddress, ip})
	}
	if ip := net.ParseIP(framedIP).To4(); ip != nil {
		attrs = append(attrs, radAttr{attrFramedIPAddress, ip})
	}
	pkt, err := buildPacket(codeDisconnectRequest, secret, attrs, nil)
	if err != nil {
		return disconnectResult{Message: err.Error()}
	}

	addr := net.JoinHostPort(nasIP, fmt.Sprint(port))
	reply, err := sendRADIUSPacket(context.Background(), addr, pkt, 3*time.Second)
	if err != nil {
		return disconnectResult{Message: "NAS " + addr + " did not answer the Disconnect-Request: " + err.Error()}
	}
	if err := verifyResponse(pkt, reply, secret); err != nil {
		return disconnectResult{Message: "NAS reply rejected: " + err.Error()}
	}
	switch reply[0] {
	case codeDisconnectACK:
		return disconnectResult{Acked: true, Message: "disconnected by NAS " + nasIP}
	case codeDisconnectNAK:
		return disconnectResult{Message: "NAS " + nasIP + " refused the Disconnect-Request (session unknown or not allowed)"}
	}
	return disconnectResult{Message: fmt.Sprintf("unexpected reply code %d from NAS %s", reply[0], nasIP)}
}

// kickResult is what happened to a user's open sessions.
type kickResult struct {
	Closed      int      // sessions marked ended in radacct
	Unconfirmed int      // sessions the router did not confirm ending: the user may still be online
	Msgs        []string // one line per session, for staff
}

// stillOnlineNote is appended to a staff message when the router did not confirm.
func (k kickResult) stillOnlineNote() string {
	if k.Unconfirmed == 0 {
		return ""
	}
	return fmt.Sprintf("; the router did not confirm the disconnect of %d session(s), so the customer may still be online "+
		"until that session ends (new logins are already refused). Check the router's /radius incoming (port 3799) and secret",
		k.Unconfirmed)
}

// disconnectMany disconnects several users at once (a router that does not answer costs the
// timeout per session, so doing them one by one held the expiry task for minutes) and returns
// the users with sessions the router did not confirm ending.
func disconnectMany(db dbtx, usernames []string) (stillOnline []string) {
	var mu sync.Mutex
	work := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < 32 && w < len(usernames); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				if r := disconnectUserSessions(db, u, false); r.Unconfirmed > 0 {
					mu.Lock()
					stillOnline = append(stillOnline, u)
					mu.Unlock()
				}
			}
		}()
	}
	for _, u := range usernames {
		work <- u
	}
	close(work)
	wg.Wait()
	sort.Strings(stillOnline)
	return stillOnline
}

// stillOnlineSummary describes disconnectMany's result for a task's result line.
func stillOnlineSummary(users []string) string {
	if len(users) == 0 {
		return ""
	}
	list := users
	if len(list) > 10 {
		list = list[:10]
	}
	more := ""
	if len(users) > len(list) {
		more = fmt.Sprintf(" and %d more", len(users)-len(list))
	}
	return fmt.Sprintf("; %d may still be online (router did not confirm the disconnect): %s%s",
		len(users), strings.Join(list, ", "), more)
}

// disconnectUserSessions sends a Disconnect-Request for each open session of a user. When
// the router acknowledges, the accounting row is closed (the router will usually also send
// its own Stop). force closes rows the router did not acknowledge, for sessions that are
// really gone but never got a Stop; they still count as Unconfirmed.
func disconnectUserSessions(db dbtx, username string, force bool) kickResult {
	rows, err := db.Query(`
		SELECT radacctid, acctsessionid, host(nasipaddress), COALESCE(host(framedipaddress), '')
		FROM radacct WHERE username = $1 AND acctstoptime IS NULL`, username)
	if err != nil {
		return kickResult{Msgs: []string{err.Error()}}
	}
	type sess struct {
		id                    int64
		sid, nasIP, framedIP  string
	}
	var list []sess
	for rows.Next() {
		var s sess
		if rows.Scan(&s.id, &s.sid, &s.nasIP, &s.framedIP) == nil {
			list = append(list, s)
		}
	}
	rows.Close()

	var res kickResult
	for _, s := range list {
		r := disconnectSession(db, s.nasIP, username, s.sid, s.framedIP)
		res.Msgs = append(res.Msgs, r.Message)
		if !r.Acked {
			res.Unconfirmed++
		}
		if r.Acked || force {
			cause := "Admin-Reset"
			if !r.Acked {
				cause = "Admin-Reset-Forced"
			}
			db.Exec(`UPDATE radacct SET acctstoptime = NOW(), acctterminatecause = $1
				WHERE radacctid = $2 AND acctstoptime IS NULL`, cause, s.id)
			res.Closed++
		}
	}
	return res
}
