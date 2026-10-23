package handlers

import (
	"crypto/md5"
	"encoding/binary"
	"net"
	"sync"
	"testing"

	"github.com/freeradius-manager/backend/internal/database"
)

// fakeNAS stands in for a router's Disconnect-Request listener (MikroTik `/radius incoming`,
// RFC 5176). It is registered in the nas table for 127.0.0.1 so disconnectSession finds its
// port and secret, and it answers the way it is told to.
type fakeNAS struct {
	conn   *net.UDPConn
	secret string
	mu     sync.Mutex
	mode   string   // "ack", "nak" or "ignore" (a firewall dropping port 3799)
	users  []string // User-Name of every valid request received
	forged int      // requests whose authenticator did not check out
}

func startFakeNAS(t *testing.T, db *database.DB, secret, mode string) *fakeNAS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNAS{conn: conn, secret: secret, mode: mode}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	mustExec(t, db, `DELETE FROM nas WHERE nasname = '127.0.0.1'`)
	mustExec(t, db, `INSERT INTO nas (nasname, shortname, secret, coa_port) VALUES ('127.0.0.1','fake-nas',$1,$2)`, secret, port)
	t.Cleanup(func() {
		conn.Close()
		db.Exec(`DELETE FROM nas WHERE nasname = '127.0.0.1'`)
	})
	go n.serve()
	return n
}

func (n *fakeNAS) setMode(m string) {
	n.mu.Lock()
	n.mode = m
	n.mu.Unlock()
}

func (n *fakeNAS) received() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.users...)
}

func (n *fakeNAS) serve() {
	buf := make([]byte, 4096)
	for {
		size, from, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:size]...)
		if len(pkt) < 20 || pkt[0] != codeDisconnectRequest {
			continue
		}
		// RFC 5176 §3.5: Request Authenticator = MD5(Code+Identifier+Length+16 zero octets+
		// Attributes+Secret).
		check := append([]byte(nil), pkt...)
		copy(check[4:20], make([]byte, 16))
		h := md5.New()
		h.Write(check)
		h.Write([]byte(n.secret))
		n.mu.Lock()
		if string(h.Sum(nil)) != string(pkt[4:20]) {
			n.forged++
			n.mu.Unlock()
			continue
		}
		user := ""
		for i := 20; i+2 <= len(pkt); {
			typ, l := pkt[i], int(pkt[i+1])
			if l < 2 || i+l > len(pkt) {
				break
			}
			if typ == attrUserName {
				user = string(pkt[i+2 : i+l])
			}
			i += l
		}
		n.users = append(n.users, user)
		mode := n.mode
		n.mu.Unlock()

		var code byte
		switch mode {
		case "ack":
			code = codeDisconnectACK
		case "nak":
			code = codeDisconnectNAK
		default:
			continue
		}
		reply := make([]byte, 20)
		reply[0], reply[1] = code, pkt[1]
		binary.BigEndian.PutUint16(reply[2:4], 20)
		rh := md5.New()
		rh.Write(reply[0:4])
		rh.Write(pkt[4:20])
		rh.Write([]byte(n.secret))
		copy(reply[4:20], rh.Sum(nil))
		n.conn.WriteToUDP(reply, from)
	}
}
