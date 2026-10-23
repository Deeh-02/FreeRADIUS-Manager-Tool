package handlers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Against the real Safaricom sandbox, through a deployed backend (docs/CLOUD-TEST.md)
// ─────────────────────────────────────────────────────────────────────────────
//
// The backend must be reachable by Safaricom (Render, or a tunnel) and have the MPESA_* sandbox
// settings. These tests drive it through TEST_API_URL and read TEST_DATABASE_URL to check the
// result, like the other stack tests. They spend no real money (sandbox).
//
//	MPESA_SANDBOX_PHONE=0712345678   your Safaricom number: you get a real prompt and type your PIN
//	MPESA_SANDBOX_C2B=1 plus MPESA_CONSUMER_KEY, MPESA_CONSUMER_SECRET, MPESA_C2B_SHORTCODE
//	                                 (the 600xxx test code): simulated paybill payments

func cloudPlan(t *testing.T, a *stackAPI) int {
	t.Helper()
	out := a.must("POST", "/plans", map[string]interface{}{"name": "Sandbox 1 bob", "price": 1, "currency": "KES", "validity_days": 1}, 201)
	return int(out["id"].(float64))
}

func needMpesaOn(t *testing.T, a *stackAPI) {
	t.Helper()
	st := a.must("GET", "/mpesa-admin/status", nil, 200)
	if st["enabled"] != true {
		t.Fatalf("M-Pesa is not configured on the deployed backend: %v", st["problem"])
	}
	if st["environment"] != "sandbox" {
		t.Fatalf("refusing to run against %v: these tests are for the sandbox", st["environment"])
	}
}

// A real STK prompt to your phone, answered by you, reported by Safaricom to the deployed
// backend's callback.
func TestCloudMpesaSandboxPrompt(t *testing.T) {
	phone := os.Getenv("MPESA_SANDBOX_PHONE")
	if phone == "" {
		t.Skip("MPESA_SANDBOX_PHONE not set")
	}
	a, db := needStack(t)
	needMpesaOn(t, a)
	planID := cloudPlan(t, a)
	_, inv := a.customerWithInvoice(db, "sandboxer", planID)
	out := a.must("POST", "/mpesa-admin/prompt", map[string]interface{}{"invoice_id": inv, "phone": phone}, 201)
	id := int(out["id"].(float64))
	t.Logf(">>> Check %s now: enter your M-Pesa PIN to pay KES %v (sandbox: test money). Waiting up to 3 minutes…", out["phone"], out["amount"])

	var st map[string]interface{}
	took, ok := waitFor(3*time.Minute, func() bool {
		st = a.must("GET", fmt.Sprintf("/mpesa-admin/prompt/%d", id), nil, 200)
		return st["status"] == "paid" || st["status"] == "failed"
	})
	t.Logf("prompt %s after %s: %v", st["status"], took.Round(time.Second), st)
	if !ok {
		t.Fatal("no answer from Safaricom in 3 minutes: is MPESA_CALLBACK_URL this backend's public address? (the reconcile loop also asks every minute)")
	}
	if st["status"] != "paid" {
		t.Skipf("the prompt was not paid (%v); cancel and timeouts are fine, run again and enter the PIN", st["result_desc"])
	}
	n := scanInt(t, db, `SELECT COUNT(*) FROM payments WHERE invoice_id=$1 AND gateway='mpesa' AND status='confirmed'`, inv)
	if n != 1 {
		t.Errorf("%d payments recorded for the paid prompt", n)
	}
	if days := scanInt(t, db, `SELECT account_expiry - CURRENT_DATE FROM radius_users WHERE username='sandboxer'`); days != 1 {
		t.Errorf("paid until today%+d, want +1", days)
	}
	if r := optionalRadius(); r != nil && !r.accepts(t, "sandboxer", "pw-sandboxer") {
		t.Error("FreeRADIUS (on the same database) refuses the customer who paid")
	}
	failIf(t, checkMoney(db))
}

// Simulated paybill payments (the sandbox's C2B simulator), one to a known account and one to
// an unknown one, confirmed by Safaricom to the deployed backend.
func TestCloudMpesaSandboxPaybill(t *testing.T) {
	if os.Getenv("MPESA_SANDBOX_C2B") == "" {
		t.Skip("MPESA_SANDBOX_C2B not set")
	}
	short := os.Getenv("MPESA_C2B_SHORTCODE")
	if short == "" || os.Getenv("MPESA_CONSUMER_KEY") == "" {
		t.Fatal("set MPESA_C2B_SHORTCODE, MPESA_CONSUMER_KEY and MPESA_CONSUMER_SECRET (the same app as the backend)")
	}
	a, db := needStack(t)
	needMpesaOn(t, a)
	a.must("POST", "/mpesa-admin/register-c2b", nil, 200)
	planID := cloudPlan(t, a)
	a.customerWithInvoice(db, "paybiller", planID)

	sim := &darajaClient{cfg: &mpesaConfig{BaseURL: "https://sandbox.safaricom.co.ke",
		ConsumerKey: os.Getenv("MPESA_CONSUMER_KEY"), ConsumerSecret: os.Getenv("MPESA_CONSUMER_SECRET")}}
	sim.http = defaultHTTPClient()
	pay := func(account string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var out map[string]interface{}
		if err := sim.call(ctx, "/mpesa/c2b/v1/simulate", map[string]interface{}{"ShortCode": short,
			"CommandID": "CustomerPayBillOnline", "Amount": 1, "Msisdn": "254708374149", "BillRefNumber": account}, &out); err != nil {
			t.Fatalf("simulate: %v", err)
		}
		t.Logf("simulated payment to account %q: %v", account, out)
	}
	pay("PAYBILLER")
	// The sandbox drops the confirmation of a second simulation sent right after the first
	// (seen October 2026: the code in between never arrived), so space them out.
	time.Sleep(20 * time.Second)
	pay("no-such-account")
	ok := false
	took, _ := waitFor(90*time.Second, func() bool {
		var applied, unmatched int
		db.QueryRow(`SELECT COUNT(*) FILTER (WHERE status='applied' AND username='paybiller'),
			COUNT(*) FILTER (WHERE status='unmatched' AND upper(bill_ref)='NO-SUCH-ACCOUNT') FROM mpesa_c2b`).Scan(&applied, &unmatched)
		ok = applied == 1 && unmatched == 1
		return ok
	})
	if !ok {
		var rows []string
		r, _ := db.Query(`SELECT trans_id || ' ' || COALESCE(bill_ref,'') || ' ' || status FROM mpesa_c2b`)
		for r != nil && r.Next() {
			var s string
			r.Scan(&s)
			rows = append(rows, s)
		}
		t.Fatalf("after %s the confirmations are %v; want one applied to paybiller and one unmatched", took.Round(time.Second), strings.Join(rows, "; "))
	}
	t.Logf("both confirmations arrived and were handled in %s", took.Round(time.Second))
	failIf(t, checkMoney(db))
}
