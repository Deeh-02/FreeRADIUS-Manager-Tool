package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Customers who must go offline (expired, suspended, deleted) are blocked in radcheck at once;
// the Disconnect-Request only ends the session they already have on the router. When the router
// does not confirm it (port 3799 firewalled, wrong secret, router busy), billing must still
// complete and staff must be told the customer may still be online.

const nasSecret = "fake-nas-secret-1"

// onlineCustomers creates n paid customers, each with an open session on the fake NAS.
func onlineCustomers(t *testing.T, n int) (map[string]int, *planRow) {
	t.Helper()
	db := openTestDB(t)
	plan := seedPlan(t, db, "Home", 1500, 30)
	ids := map[string]int{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("online%03d", i)
		uid := seedUser(t, db, name, intPtr(10))
		ids[name] = uid
		mustExec(t, db, `UPDATE radius_users SET plan_id=$1 WHERE id=$2`, plan.ID, uid)
		mustExec(t, db, `INSERT INTO radacct (acctsessionid, acctuniqueid, username, nasipaddress, acctstarttime, acctupdatetime)
			VALUES ($1, $2, $3, '127.0.0.1', NOW() - INTERVAL '1 hour', NOW())`, "sess-"+name, "uniq-"+name, name)
	}
	return ids, plan
}

func openSessions(t *testing.T, user string) int {
	return scanInt(t, openTestDB(t), `SELECT COUNT(*) FROM radacct WHERE username=$1 AND acctstoptime IS NULL`, user)
}

func TestRouterIgnoresDisconnectOnExpiry(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `TRUNCATE radacct`)
	nas := startFakeNAS(t, db, nasSecret, "ignore")
	ids, _ := onlineCustomers(t, 40)
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 2`)

	t0 := time.Now()
	msg, n := runTask(db, testLogger(), "expire_accounts")
	took := time.Since(t0)
	if n != len(ids) {
		t.Fatalf("expired %d customers, want %d (%s)", n, len(ids), msg)
	}
	// 40 routers' worth of silence must not hold the task for 40 × the timeout.
	if took > 15*time.Second {
		t.Errorf("expire_accounts took %s with an unresponsive router; the disconnects run one after another", took.Round(time.Second))
	}
	if !strings.Contains(msg, "40") || !strings.Contains(strings.ToLower(msg), "online") {
		t.Errorf("task result %q does not tell staff that 40 customers may still be online", msg)
	}
	if got := len(nas.received()); got < len(ids) {
		t.Errorf("router received %d Disconnect-Requests, want %d", got, len(ids))
	}
	for name := range ids {
		if ok, why := radiusDecisionFor(t, db, name); ok {
			t.Errorf("%s still allowed by radcheck after expiry (%s)", name, why)
		}
		// Not confirmed, so the session is not marked closed: it still shows as online.
		if openSessions(t, name) != 1 {
			t.Errorf("%s: session marked closed although the router never confirmed", name)
		}
	}
	failIf(t, checkEnforcement(db))

	// The Online now page marks these sessions so staff can deal with them on the router.
	_, body := callHandler(newTestHandler(t).ActiveSessions, nil, nil, nil)
	list, _ := body["data"].([]interface{})
	flagged := 0
	for _, it := range list {
		if it.(map[string]interface{})["should_be_offline"] == "blocked" {
			flagged++
		}
	}
	if flagged != len(list) || len(list) == 0 || body["total"] != float64(len(ids)) {
		t.Errorf("Online now flags %d of the %d sessions on its first page as 'should be offline' (total %v)", flagged, len(list), body["total"])
	}
	// One customer still allowed online is listed after the flagged ones.
	mustExec(t, db, `UPDATE radius_users SET account_expiry = CURRENT_DATE + 5, status='active' WHERE username='online000'`)
	syncAccess(db, "online000")
	mustExec(t, db, `UPDATE radacct SET acctstarttime = NOW() WHERE username='online000'`)
	_, body = callHandler(newTestHandler(t).ActiveSessions, nil, nil, nil)
	if first := body["data"].([]interface{})[0].(map[string]interface{}); first["username"] == "online000" {
		t.Error("a session that is fine is listed before the ones that should be offline")
	}
}

func TestRouterIgnoresDisconnectOnSuspend(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `TRUNCATE radacct`)
	h := newTestHandler(t)
	nas := startFakeNAS(t, db, nasSecret, "ignore")
	ids, _ := onlineCustomers(t, 1)
	uid := ids["online000"]
	code, body := callHandler(h.SuspendRadiusUser, gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}, nil, nil)
	if code != 200 {
		t.Fatalf("suspend: %d %v", code, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(strings.ToLower(msg), "online") {
		t.Errorf("suspend answered %q; staff are not told the customer may still be online", msg)
	}
	if len(nas.received()) != 1 {
		t.Errorf("router got %d requests, want 1", len(nas.received()))
	}

	nas.setMode("ack")
	callHandler(h.ActivateRadiusUser, gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}, nil, nil)
	_, body = callHandler(h.SuspendRadiusUser, gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}, nil, nil)
	if msg, _ := body["message"].(string); !strings.Contains(msg, "1 session(s) disconnected") {
		t.Errorf("suspend with a confirming router answered %q", msg)
	}
	if openSessions(t, "online000") != 0 {
		t.Error("confirmed disconnect did not close the session")
	}
}

// A refused delete (money not settled) must leave the customer exactly as they were: online.
func TestRefusedDeleteDoesNotDisconnect(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `TRUNCATE radacct`)
	h := newTestHandler(t)
	nas := startFakeNAS(t, db, nasSecret, "ack")
	ids, plan := onlineCustomers(t, 1)
	uid := ids["online000"]
	inv := seedInvoice(t, db, uid, "online000", plan, 1500)
	if _, err := payTx(db, paymentInput{InvoiceID: &inv, Amount: 500}); err != nil {
		t.Fatal(err)
	}
	code, _ := callHandler(h.DeleteRadiusUser, gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}, nil, nil)
	if code != 409 {
		t.Fatalf("delete with a part-paid invoice: status %d, want 409", code)
	}
	if got := nas.received(); len(got) != 0 {
		t.Errorf("refused delete still sent %d Disconnect-Request(s)", len(got))
	}
	if openSessions(t, "online000") != 1 {
		t.Error("refused delete closed the customer's session")
	}

	// Deleting for real does kick them, and says so when the router does not confirm.
	nas.setMode("ignore")
	mustExec(t, db, `DELETE FROM payments`)
	code, body := callHandler(h.DeleteRadiusUser, gin.Params{{Key: "id", Value: strconv.Itoa(uid)}}, nil, nil)
	if code != 200 {
		t.Fatalf("delete: %d %v", code, body)
	}
	if len(nas.received()) != 1 {
		t.Errorf("router got %d requests on delete, want 1", len(nas.received()))
	}
	if msg, _ := body["message"].(string); !strings.Contains(strings.ToLower(msg), "online") {
		t.Errorf("delete answered %q; staff are not told the customer may still be online", msg)
	}
}

// A forged or mis-keyed request is not something the router acts on; the backend uses the NAS
// row's own secret, not RADIUS_SECRET.
func TestDisconnectUsesNASSecret(t *testing.T) {
	db := resetTestDB(t)
	mustExec(t, db, `TRUNCATE radacct`)
	nas := startFakeNAS(t, db, nasSecret, "ack")
	t.Setenv("RADIUS_SECRET", "a-different-global-secret")
	onlineCustomers(t, 1)
	k := disconnectUserSessions(db, "online000", false)
	if k.Closed != 1 || k.Unconfirmed != 0 || nas.forged != 0 {
		t.Errorf("closed=%d unconfirmed=%d forged=%d msgs=%v; the NAS row's secret was not used", k.Closed, k.Unconfirmed, nas.forged, k.Msgs)
	}
}
