package handlers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A.1b The cut-off happens at midnight, and FreeRADIUS decides it by its own clock and zone
// (the Expiration text in radcheck has no zone). These tests run a FreeRADIUS whose clock is
// faked with libfaketime, so midnight can be crossed on demand:
//
//	TEST_FAKETIME_LIB=/path/to/libfaketimeMT.so.1 go test ./internal/handlers -run Midnight
//
// (build it with: curl -L https://github.com/wolfcw/libfaketime/archive/refs/tags/v0.9.10.tar.gz | tar xz; make -C libfaketime-0.9.10/src)

type fakeClockRadius struct {
	*liveRadius
	dir string
}

func startFakeClockRadius(t *testing.T, tz string, port int) *fakeClockRadius {
	t.Helper()
	lib := os.Getenv("TEST_FAKETIME_LIB")
	if lib == "" {
		t.Skip("TEST_FAKETIME_LIB not set")
	}
	openTestDB(t) // needs the scratch database
	script, _ := filepath.Abs("../../../scripts/test-radius.sh")
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("radius-manager-midnight-%d", port))
	env := append(os.Environ(), "TEST_RADIUS_DIR="+dir, "TEST_RADIUS_PORT="+strconv.Itoa(port),
		"TEST_RADIUS_FAKETIME_LIB="+lib, "TZ="+tz, "TEST_PG_PORT="+testPGPort())
	cmd := exec.Command(script, "start")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start FreeRADIUS (TZ=%s): %v\n%s", tz, err, out)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Env = env
		stop.Run()
	})
	return &fakeClockRadius{liveRadius: &liveRadius{host: "127.0.0.1", port: port, secret: "testing123-e2e"}, dir: dir}
}

// setClock makes FreeRADIUS's clock read the given wall time in its own zone, and run on.
func (f *fakeClockRadius) setClock(t *testing.T, wall string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "faketime"), []byte("@"+wall+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRealRadiusMidnightCutoff(t *testing.T) {
	db := resetTestDB(t)
	// Paid up to and including 23 October: online until midnight at the start of 24 October.
	lastPaid := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO radcheck (username, attribute, op, value) VALUES
		('mwangi','Cleartext-Password',':=','pw'), ('mwangi','Expiration',':=',$1)`, radiusExpiration(lastPaid, 0))

	nairobi := startFakeClockRadius(t, "Africa/Nairobi", 18140)
	cases := []struct {
		wall string
		want bool
	}{
		{"2026-10-23 08:00:00", true},
		{"2026-10-23 23:59:50", true},
		{"2026-10-24 00:00:01", false},
		{"2026-10-24 02:59:00", false}, // still 23 October in UTC: must not matter
		{"2026-10-23 23:59:55", true},  // clock set back (NTP correction): fine again
	}
	for _, c := range cases {
		nairobi.setClock(t, c.wall)
		res := nairobi.login("mwangi", "pw")
		if !res.Success {
			t.Fatalf("%s: no answer: %s", c.wall, res.Error)
		}
		if got := res.Reply == "Access-Accept"; got != c.want {
			t.Errorf("FreeRADIUS in Africa/Nairobi at %s local: accepted=%v, want %v", c.wall, got, c.want)
		}
		if c.wall == "2026-10-23 23:59:50" {
			st, _ := strconv.ParseInt(res.ReplyAttrs["Session-Timeout"], 0, 64)
			if st < 1 || st > 15 {
				t.Errorf("10 s before midnight Session-Timeout is %d; the session should end at midnight", st)
			}
		}
	}

	// The same customer on a FreeRADIUS left in UTC (TZ not set on the server): at 00:30 in
	// Nairobi it is 21:30 UTC, still 23 October, and the customer stays online three hours too
	// long. This is why TZ must be set once for all three programs (A.1b).
	utc := startFakeClockRadius(t, "UTC", 18150)
	utc.setClock(t, "2026-10-23 21:30:00") // = 00:30 on 24 October in Nairobi
	if res := utc.login("mwangi", "pw"); res.Reply != "Access-Accept" {
		t.Errorf("expected the misconfigured (UTC) FreeRADIUS to still accept at 21:30 UTC, got %s", res.Reply)
	} else {
		t.Log("confirmed: FreeRADIUS left in UTC keeps a Nairobi customer online until 03:00 local; set TZ")
	}
}
