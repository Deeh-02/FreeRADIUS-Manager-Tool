package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeradius-manager/backend/internal/auth"
	"github.com/freeradius-manager/backend/internal/database"
	"github.com/gin-gonic/gin"
)

// M-Pesa end to end, against fakeDaraja: the backend's routes run in this process behind a
// real HTTP server, so Daraja's callbacks arrive over HTTP exactly as Safaricom's would.

const mpesaToken = "cb-secret-0123456789abcdef0123"

type mpesaRig struct {
	t      *testing.T
	db     *database.DB
	h      *Handler
	api    *httptest.Server
	daraja *fakeDaraja
	plan   *planRow
}

func newMpesaRig(t *testing.T) *mpesaRig {
	t.Helper()
	db := resetTestDB(t)
	setTestSettings(t, db, map[string]string{"trial_days": "0", "billing_grace_days": "0", "renewal_invoice_days": "3"})
	h := newTestHandler(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	staff := func(c *gin.Context) {
		c.Set("claims", &auth.Claims{UserID: 1, Username: "staff", Role: "admin"})
		c.Next()
	}
	r.POST("/api/v1/pay/result/:token", h.MpesaSTKCallback)
	r.POST("/api/v1/pay/validate/:token", h.MpesaC2BValidation)
	r.POST("/api/v1/pay/confirm/:token", h.MpesaC2BConfirmation)
	r.POST("/api/v1/portal/login", h.PortalLogin)
	r.POST("/api/v1/portal/pay", h.PortalAuthMiddleware(), h.PortalPay)
	r.GET("/api/v1/portal/pay/:id", h.PortalAuthMiddleware(), h.PortalPayStatus)
	a := r.Group("/api/v1/mpesa-admin", staff)
	a.GET("/status", h.MpesaStatus)
	a.POST("/prompt", h.MpesaSendPrompt)
	a.GET("/prompt/:id", h.MpesaPromptStatus)
	a.GET("/c2b", h.MpesaListC2B)
	a.POST("/c2b/:id/allocate", h.MpesaAllocateC2B)
	a.POST("/register-c2b", h.MpesaRegisterC2B)
	api := httptest.NewServer(r)
	t.Cleanup(api.Close)

	f := newFakeDaraja(t)
	for k, v := range map[string]string{"MPESA_CONSUMER_KEY": f.key, "MPESA_CONSUMER_SECRET": f.secret, "MPESA_SHORTCODE": f.short,
		"MPESA_PASSKEY": f.pass, "MPESA_CALLBACK_URL": api.URL, "MPESA_CALLBACK_TOKEN": mpesaToken, "MPESA_BASE_URL": f.srv.URL,
		"MPESA_ENV": "sandbox", "MPESA_ALLOWED_IPS": "", "MPESA_TYPE": "", "MPESA_C2B_REJECT_UNKNOWN": ""} {
		t.Setenv(k, v)
	}
	resetMpesaClient()
	t.Cleanup(resetMpesaClient)
	// The real spacing (1 s) would make these tests slow; the fake still checks it is kept.
	gap, spike := darajaMinGap, darajaSpikeWait
	darajaMinGap, darajaSpikeWait = 120*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { darajaMinGap, darajaSpikeWait = gap, spike })
	plan := seedPlan(t, db, "Home 10", 1500, 30)
	mustExec(t, db, `UPDATE user_plans SET currency='KES' WHERE id=$1`, plan.ID)
	plan, _ = loadPlan(db, plan.ID)
	return &mpesaRig{t: t, db: db, h: h, api: api, daraja: f, plan: plan}
}

func (m *mpesaRig) do(method, path string, body interface{}, hdr map[string]string) (int, map[string]interface{}) {
	m.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, m.api.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// customer creates a customer with an unpaid invoice for the plan.
func (m *mpesaRig) customer(name, phone string) (uid, inv int) {
	m.t.Helper()
	code, body := callHandler(m.h.CreateRadiusUser, nil, nil, map[string]interface{}{"username": name, "password": "pw-" + name,
		"plan_id": m.plan.ID, "trial_days": 0, "phone": phone})
	if code != 201 {
		m.t.Fatalf("create %s: %d %v", name, code, body)
	}
	uid = int(body["id"].(float64))
	inv = scanInt(m.t, m.db, `SELECT id FROM invoices WHERE user_id=$1 AND status='pending'`, uid)
	return
}

func (m *mpesaRig) prompt(inv int, phone string) (int, map[string]interface{}) {
	return m.do("POST", "/api/v1/mpesa-admin/prompt", map[string]interface{}{"invoice_id": inv, "phone": phone}, nil)
}

func (m *mpesaRig) mustPrompt(inv int, phone string) (int, fakePush) {
	m.t.Helper()
	code, out := m.prompt(inv, phone)
	if code != 201 {
		m.t.Fatalf("prompt: %d %v", code, out)
	}
	return int(out["id"].(float64)), m.daraja.lastPush()
}

func (m *mpesaRig) stkStatus(id int) string {
	return scanString(m.t, m.db, `SELECT status FROM mpesa_stk WHERE id=$1`, id)
}

func (m *mpesaRig) paidUntil(uid int) int {
	return scanInt(m.t, m.db, `SELECT COALESCE(account_expiry - CURRENT_DATE, -999) FROM radius_users WHERE id=$1`, uid)
}

func (m *mpesaRig) mpesaPayments(user string) (int, float64) {
	var n int
	var sum float64
	m.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM payments WHERE username=$1 AND gateway='mpesa' AND status='confirmed'`, user).Scan(&n, &sum)
	return n, sum
}

func (m *mpesaRig) c2b(transID, account, amount, msisdn string) int {
	return postJSON(m.api.URL+"/api/v1/pay/confirm/"+mpesaToken, map[string]string{
		"TransactionType": "Pay Bill", "TransID": transID, "TransTime": "20261023150405", "TransAmount": amount,
		"BusinessShortCode": "174379", "BillRefNumber": account, "MSISDN": msisdn, "FirstName": "JOHN", "LastName": "DOE"})
}

func TestNormalizePhone(t *testing.T) {
	for in, want := range map[string]string{
		"0712345678": "254712345678", "0712 345 678": "254712345678", "+254712345678": "254712345678",
		"254712345678": "254712345678", "712345678": "254712345678", "0110123456": "254110123456",
		"+254 110-123-456": "254110123456", "": "", "0712": "", "0812345678": "", "2557123456789": "", "07123456ab": "",
	} {
		got, err := normalizePhone(in)
		if want == "" {
			if err == nil {
				t.Errorf("normalizePhone(%q) = %q, want an error", in, got)
			}
		} else if got != want || err != nil {
			t.Errorf("normalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// The ordinary case: staff send a prompt, the customer enters their PIN, Safaricom calls back.
func TestMpesaPromptPaid(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("njeri", "0712345678")
	id, p := m.mustPrompt(inv, "0712 345 678")
	if p.Amount != 1500 || p.Phone != "254712345678" || !strings.Contains(p.Callback, "/pay/result/"+mpesaToken) {
		t.Errorf("push %+v", p)
	}
	if m.stkStatus(id) != "pending" {
		t.Errorf("status %s after the push, want pending", m.stkStatus(id))
	}
	if m.paidUntil(uid) >= 0 {
		t.Error("customer online before paying")
	}
	// Safaricom delivers the callback, and (as it can) delivers it again, several at once.
	var codes sync.Map
	parallel(6, func(i int) { codes.Store(i, m.daraja.sendResult(p, 0, "QJK1ABC2DE")) })
	codes.Range(func(k, v interface{}) bool {
		if v.(int) != 200 {
			t.Errorf("callback answered %v", v)
		}
		return true
	})
	if n, sum := m.mpesaPayments("njeri"); n != 1 || sum != 1500 {
		t.Errorf("%d M-Pesa payments totalling %.2f, want 1 of 1500", n, sum)
	}
	if ref := scanString(t, m.db, `SELECT gateway_ref FROM payments WHERE username='njeri'`); ref != "QJK1ABC2DE" {
		t.Errorf("payment reference %q, want the M-Pesa code", ref)
	}
	if m.stkStatus(id) != "paid" || scanString(t, m.db, `SELECT status FROM invoices WHERE id=$1`, inv) != "paid" {
		t.Error("prompt or invoice not marked paid")
	}
	if d := m.paidUntil(uid); d != 30 {
		t.Errorf("paid until today%+d, want +30 (today plus the plan's 30 days)", d)
	}
	if r := optionalRadius(); r != nil && !r.accepts(t, "njeri", "pw-njeri") {
		t.Errorf("real FreeRADIUS refuses the customer who just paid by M-Pesa; radcheck: %s", radcheckOf(m.db, "njeri"))
	}
	// The same code typed in by hand afterwards is refused.
	if _, err := payTx(m.db, paymentInput{InvoiceID: nil, Username: "njeri", Amount: 1500, Gateway: "mpesa", GatewayRef: "qjk1abc2de"}); err == nil {
		t.Error("the M-Pesa code was accepted again by hand")
	}
	code, body := m.do("GET", fmt.Sprintf("/api/v1/mpesa-admin/prompt/%d", id), nil, nil)
	if code != 200 || body["status"] != "paid" || body["mpesa_receipt"] != "QJK1ABC2DE" {
		t.Errorf("prompt status: %d %v", code, body)
	}
	m.daraja.check()
	failIf(t, checkMoney(m.db))
	failIf(t, checkEnforcement(m.db))
}

func TestMpesaPromptRefusedByCustomer(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("baraka", "0722000111")
	for _, code := range []int{1032, 1037, 2001, 1} {
		id, p := m.mustPrompt(inv, "0722000111")
		if c := m.daraja.sendResult(p, code, ""); c != 200 {
			t.Errorf("callback %d answered %d", code, c)
		}
		if m.stkStatus(id) != "failed" {
			t.Errorf("result %d: prompt %s, want failed", code, m.stkStatus(id))
		}
		// A late success for a failed prompt is still money received: it is applied.
		if code == 1 {
			m.daraja.sendResult(p, 0, "QLATE00001")
		}
	}
	if n, _ := m.mpesaPayments("baraka"); n != 1 {
		t.Errorf("%d payments, want only the late success", n)
	}
	if m.paidUntil(uid) != 30 {
		t.Errorf("paid until today%+d", m.paidUntil(uid))
	}
	failIf(t, checkMoney(m.db))
}

// The callback never arrives (Safaricom's side, the network, or our server was down). The
// reconcile loop asks Safaricom; when the callback turns up late, nothing is recorded twice and
// the real M-Pesa code replaces the stand-in reference.
func TestMpesaCallbackLostReconciled(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("wekesa", "0733111222")
	id, p := m.mustPrompt(inv, "0733111222")

	// Too early to ask: nothing happens.
	if msg, _ := reconcileMpesa(m.db, testLogger()); !strings.HasPrefix(msg, "0 prompt") {
		t.Errorf("reconcile asked about a fresh prompt: %s", msg)
	}
	mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '90 seconds' WHERE id=$1`, id)
	// The customer has not answered yet: Safaricom says "being processed".
	reconcileMpesa(m.db, testLogger())
	if m.stkStatus(id) != "pending" {
		t.Fatalf("status %s while the customer has not answered", m.stkStatus(id))
	}
	// Asked again within the minute: not asked (rate of queries is limited).
	m.daraja.settle(p.CheckoutID, 0)
	if msg, _ := reconcileMpesa(m.db, testLogger()); !strings.HasPrefix(msg, "0 prompt") {
		t.Errorf("asked twice within a minute: %s", msg)
	}
	mustExec(t, m.db, `UPDATE mpesa_stk SET last_query_at = NOW() - INTERVAL '61 seconds' WHERE id=$1`, id)
	if msg, n := reconcileMpesa(m.db, testLogger()); n != 1 {
		t.Fatalf("reconcile: %s", msg)
	}
	if m.stkStatus(id) != "paid" || m.paidUntil(uid) != 30 {
		t.Fatalf("after reconcile: %s, paid until today%+d", m.stkStatus(id), m.paidUntil(uid))
	}
	// The late callback.
	if c := m.daraja.sendResult(p, 0, "QLATE12345"); c != 200 {
		t.Errorf("late callback answered %d", c)
	}
	if n, _ := m.mpesaPayments("wekesa"); n != 1 {
		t.Errorf("%d payments after the late callback, want 1", n)
	}
	if ref := scanString(t, m.db, `SELECT gateway_ref FROM payments WHERE username='wekesa'`); ref != "QLATE12345" {
		t.Errorf("payment reference %q, want the M-Pesa code from the late callback", ref)
	}
	failIf(t, checkMoney(m.db))
}

// Safaricom says "not answered yet" in three ways, one of them an HTTP 200 with ResultCode
// 4999 (seen against the real sandbox). None may settle the prompt as failed: the customer
// may still be typing the PIN, and if the callback is then lost, nobody would ask again and
// a paid customer would never be credited.
func TestMpesaQueryStillWaiting(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("patient", "0733444555")
	id, p := m.mustPrompt(inv, "0733444555")
	mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE id=$1`, id)
	for i := 0; i < 6; i++ { // every form of the answer, twice
		mustExec(t, m.db, `UPDATE mpesa_stk SET last_query_at = NULL WHERE id=$1`, id)
		msg, _ := reconcileMpesa(m.db, testLogger())
		if st := m.stkStatus(id); st != "pending" {
			t.Fatalf("query %d (%s): the prompt became %q while the customer had not answered", i+1, msg, st)
		}
	}
	m.daraja.settle(p.CheckoutID, 0) // pays; the callback is lost
	mustExec(t, m.db, `UPDATE mpesa_stk SET last_query_at = NULL WHERE id=$1`, id)
	reconcileMpesa(m.db, testLogger())
	if m.stkStatus(id) != "paid" || m.paidUntil(uid) != 30 {
		t.Errorf("after paying: %s, paid until today%+d", m.stkStatus(id), m.paidUntil(uid))
	}
	failIf(t, checkMoney(m.db))
}

// The callback and the reconcile query settle the same prompt at the same moment.
func TestMpesaCallbackRacesReconcile(t *testing.T) {
	m := newMpesaRig(t)
	for round := 0; round < 25; round++ {
		name := fmt.Sprintf("race%02d", round)
		_, inv := m.customer(name, "0700000001")
		id, p := m.mustPrompt(inv, "0700000001")
		mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE id=$1`, id)
		m.daraja.settle(p.CheckoutID, 0)
		parallel(4, func(i int) {
			if i%2 == 0 {
				m.daraja.sendResult(p, 0, fmt.Sprintf("QRACE%05d", round))
			} else {
				reconcileMpesa(m.db, testLogger())
			}
		})
		if n, _ := m.mpesaPayments(name); n != 1 {
			t.Fatalf("round %d: %d payments for one prompt", round, n)
		}
	}
	failIf(t, checkMoney(m.db))
}

// The customer pays so fast that Safaricom's callback arrives before the backend has stored
// the CheckoutRequestID from the push's own answer.
func TestMpesaCallbackBeforePushAnswer(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("haraka", "0711222333")
	m.daraja.instantPay = true
	id, _ := m.mustPrompt(inv, "0711222333")
	waitFor(5*time.Second, func() bool { return m.stkStatus(id) == "paid" })
	if m.stkStatus(id) != "paid" || m.paidUntil(uid) != 30 {
		t.Errorf("early callback lost: prompt %s, paid until today%+d", m.stkStatus(id), m.paidUntil(uid))
	}
	if n := scanInt(t, m.db, `SELECT COUNT(*) FROM mpesa_c2b`); n != 0 {
		t.Errorf("early callback ended up as %d unmatched payment(s)", n)
	}
}

func TestMpesaCallbackSecurity(t *testing.T) {
	m := newMpesaRig(t)
	_, inv := m.customer("salama", "0712000999")
	_, p := m.mustPrompt(inv, "0712000999")
	good := p.Callback

	forged := p
	forged.Callback = strings.Replace(good, mpesaToken, "guessed-token-guessed-token-0000", 1)
	if c := m.daraja.sendResult(forged, 0, "QFORGED001"); c != 404 {
		t.Errorf("callback with the wrong token answered %d, want 404", c)
	}
	forged.Callback = strings.TrimSuffix(good, mpesaToken)
	if c := m.daraja.sendResult(forged, 0, "QFORGED002"); c != 404 {
		t.Errorf("callback without a token answered %d, want 404", c)
	}
	if c := postJSON(m.api.URL+"/api/v1/pay/confirm/wrong", map[string]string{"TransID": "QFORGED003", "TransAmount": "99999", "BillRefNumber": "salama"}); c != 404 {
		t.Errorf("C2B with the wrong token answered %d", c)
	}
	// Garbage with the right token.
	resp, _ := http.Post(good, "application/json", strings.NewReader("{not json"))
	if resp.StatusCode != 400 {
		t.Errorf("garbage callback answered %d", resp.StatusCode)
	}
	resp.Body.Close()
	// A success for a prompt we never sent: never applied to anyone's invoice, but the money is
	// kept for staff to look at.
	unknown := fakePush{CheckoutID: "ws_CO_NOT_OURS", Amount: 5000, Callback: good}
	m.daraja.sendResult(unknown, 0, "QUNKNOWN01")
	if n := scanInt(t, m.db, `SELECT COUNT(*) FROM payments`); n != 0 {
		t.Errorf("%d payments recorded from forged or unknown callbacks", n)
	}
	if n := scanInt(t, m.db, `SELECT COUNT(*) FROM mpesa_c2b WHERE trans_id='QUNKNOWN01' AND status='unmatched'`); n != 1 {
		t.Error("money for an unknown prompt was not kept as unmatched")
	}
	// Only Safaricom's addresses, when configured.
	t.Setenv("MPESA_ALLOWED_IPS", "196.201.214.0/24, 196.201.213.114")
	resetMpesaClient()
	if c := m.daraja.sendResult(p, 0, "QFROMHERE1"); c != 403 {
		t.Errorf("callback from a non-Safaricom address answered %d, want 403", c)
	}
	t.Setenv("MPESA_ALLOWED_IPS", "127.0.0.1")
	resetMpesaClient()
	if c := m.daraja.sendResult(p, 0, "QFROMHERE2"); c != 200 {
		t.Errorf("callback from an allowed address answered %d", c)
	}
	if n, _ := m.mpesaPayments("salama"); n != 1 {
		t.Errorf("%d payments, want 1", n)
	}
}

func TestMpesaPromptGuards(t *testing.T) {
	m := newMpesaRig(t)
	_, inv := m.customer("chege", "0712345000")
	if c, out := m.prompt(inv, "0812 345 678"); c != 400 {
		t.Errorf("bad phone: %d %v", c, out)
	}
	m.mustPrompt(inv, "0712345000")
	if c, out := m.prompt(inv, "0712345000"); c != 409 {
		t.Errorf("second prompt within 2 minutes: %d %v, want 409", c, out)
	}
	usd := seedPlan(t, m.db, "Dollar", 10, 30)
	code, body := callHandler(m.h.CreateRadiusUser, nil, nil, map[string]interface{}{"username": "tourist", "password": "pw-tourist", "plan_id": usd.ID, "trial_days": 0})
	if code != 201 {
		t.Fatal(body)
	}
	usdInv := scanInt(t, m.db, `SELECT id FROM invoices WHERE username='tourist'`)
	if c, out := m.prompt(usdInv, "0712345000"); c != 400 || !strings.Contains(fmt.Sprint(out), "shillings") {
		t.Errorf("USD invoice: %d %v", c, out)
	}
	if c, _ := m.prompt(999999, "0712345000"); c != 400 {
		t.Errorf("unknown invoice: %d", c)
	}
	// Safaricom busy: staff get a clear error; nothing is left half-done; a retry works.
	_, inv2 := m.customer("subira", "0712345001")
	m.daraja.pushFail = 503
	c, out := m.prompt(inv2, "0712345001")
	if c != 502 {
		t.Errorf("Daraja down: %d %v, want 502", c, out)
	}
	if n := scanInt(t, m.db, `SELECT COUNT(*) FROM mpesa_stk WHERE invoice_id=$1 AND status <> 'failed'`, inv2); n != 0 {
		t.Error("failed push left an open prompt behind")
	}
	m.daraja.pushFail = 0
	m.mustPrompt(inv2, "0712345001")
	// Odd cents (a percentage discount): M-Pesa takes whole shillings, so it asks for the
	// next shilling and keeps the extra on the account.
	_, inv3 := m.customer("senti", "0712345002")
	mustExec(t, m.db, `UPDATE invoices SET amount = 1004.33 WHERE id=$1`, inv3)
	_, p := m.mustPrompt(inv3, "0712345002")
	if p.Amount != 1005 {
		t.Errorf("asked for %d, want 1005", p.Amount)
	}
	m.daraja.sendResult(p, 0, "QCENTS0001")
	if n, sum := m.mpesaPayments("senti"); n != 2 || sum != 1005 {
		t.Errorf("%d payments totalling %.2f, want the invoice plus 0.67 on account", n, sum)
	}
	m.daraja.check()
	failIf(t, checkMoney(m.db))
}

func TestMpesaTokenHandling(t *testing.T) {
	m := newMpesaRig(t)
	for i := 0; i < 3; i++ {
		_, inv := m.customer(fmt.Sprintf("tok%d", i), "0712345678")
		m.mustPrompt(inv, "0712345678")
	}
	if m.daraja.oauthCalls != 1 {
		t.Errorf("signed in %d times for 3 prompts; the token should be reused", m.daraja.oauthCalls)
	}
	m.daraja.revokeTokens()
	_, inv := m.customer("tok-after", "0712345678")
	m.mustPrompt(inv, "0712345678")
	if m.daraja.oauthCalls != 2 {
		t.Errorf("after Safaricom dropped the token: %d sign-ins, want 2", m.daraja.oauthCalls)
	}
	t.Setenv("MPESA_CONSUMER_SECRET", "wrong")
	resetMpesaClient()
	_, inv = m.customer("tok-bad", "0712345678")
	if c, out := m.prompt(inv, "0712345678"); c != 502 || !strings.Contains(fmt.Sprint(out), "MPESA_CONSUMER") {
		t.Errorf("wrong credentials: %d %v", c, out)
	}
}

// Safaricom's firewall blocks the server (the real sandbox did this to us after a burst of
// requests). Staff get a clear message, not an HTML page or "check your keys"; the prompt is
// marked failed (nothing reached M-Pesa); nothing more is sent until the block should be
// over, including by the reconcile loop; afterwards everything works again.
func TestMpesaFirewallBlock(t *testing.T) {
	m := newMpesaRig(t)
	defer func(w time.Duration) { darajaBlockWait = w }(darajaBlockWait)
	darajaBlockWait = 3 * time.Second
	uid, inv := m.customer("blocked", "0712000111")
	_, inv2 := m.customer("blocked2", "0712000222")
	pid, _ := m.mustPrompt(inv2, "0712000222") // pending, for the reconcile loop to ask about
	mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE id=$1`, pid)

	m.daraja.setFirewall(true)
	code, out := m.prompt(inv, "0712000111")
	msg := fmt.Sprint(out)
	if code != 503 || !strings.Contains(msg, "firewall") || strings.Contains(msg, "<html") || strings.Contains(msg, "MPESA_CONSUMER") {
		t.Errorf("blocked prompt answered %d %v", code, out)
	}
	if st := scanString(t, m.db, `SELECT status FROM mpesa_stk WHERE invoice_id=$1`, inv); st != "failed" {
		t.Errorf("blocked prompt is %q, want failed", st)
	}
	before := m.daraja.requestCount()
	mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE invoice_id=$1`, inv) // past the 2-minute guard
	if code, out := m.prompt(inv, "0712000111"); code != 503 {
		t.Errorf("second prompt during the block: %d %v", code, out)
	}
	reconcileMpesa(m.db, testLogger())
	if n := m.daraja.requestCount() - before; n != 0 {
		t.Errorf("%d requests sent to Safaricom during the block; each one extends it", n)
	}
	if st := m.stkStatus(pid); st != "pending" {
		t.Errorf("the pending prompt became %q during the block; it must wait and be asked later", st)
	}

	m.daraja.setFirewall(false)
	time.Sleep(darajaBlockWait + 200*time.Millisecond)
	mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE invoice_id=$1`, inv)
	id, p := m.mustPrompt(inv, "0712000111")
	m.daraja.sendResult(p, 0, "QFWOK00001")
	if m.stkStatus(id) != "paid" || m.paidUntil(uid) != 30 {
		t.Errorf("after the block: %s, paid until today%+d", m.stkStatus(id), m.paidUntil(uid))
	}
	m.daraja.check()
	failIf(t, checkMoney(m.db))
}

// Daraja's gateway refuses requests over its rate (429). A few in a row are waited out and the
// prompt still goes; a long run of them gives staff a clear message and a failed prompt.
func TestMpesaSpikeArrest(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("spiky", "0712000333")
	m.daraja.setSpikes(3)
	id, p := m.mustPrompt(inv, "0712000333")
	m.daraja.sendResult(p, 0, "QSPIKE0001")
	if m.stkStatus(id) != "paid" || m.paidUntil(uid) != 30 {
		t.Errorf("after 3 refusals: %s, paid until today%+d", m.stkStatus(id), m.paidUntil(uid))
	}
	if n := len(m.daraja.pushes); n != 1 {
		t.Errorf("%d prompts reached M-Pesa, want 1", n)
	}

	_, inv2 := m.customer("spiky2", "0712000444")
	m.daraja.setSpikes(100)
	code, out := m.prompt(inv2, "0712000444")
	if code != 502 || !strings.Contains(fmt.Sprint(out), "too many") || strings.Contains(fmt.Sprint(out), "faultstring") {
		t.Errorf("long run of 429s: %d %v", code, out)
	}
	if st := scanString(t, m.db, `SELECT status FROM mpesa_stk WHERE invoice_id=$1`, inv2); st != "failed" {
		t.Errorf("prompt is %q, want failed", st)
	}
	m.daraja.setSpikes(0)
	m.daraja.check()
	failIf(t, checkMoney(m.db))
}

func TestMpesaConfigChecks(t *testing.T) {
	newMpesaRig(t)
	for k, v := range map[string]string{"MPESA_CALLBACK_TOKEN": "short", "MPESA_ENV": "staging", "MPESA_ALLOWED_IPS": "not-an-ip"} {
		old := map[string]string{"MPESA_CALLBACK_TOKEN": mpesaToken, "MPESA_ENV": "sandbox", "MPESA_ALLOWED_IPS": ""}[k]
		t.Setenv(k, v)
		if _, err := loadMpesaConfig(); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
		t.Setenv(k, old)
	}
	t.Setenv("MPESA_BASE_URL", "") // the real Safaricom
	t.Setenv("MPESA_CALLBACK_URL", "http://plain.example.com")
	for _, env := range []string{"sandbox", "production"} {
		t.Setenv("MPESA_ENV", env)
		if _, err := loadMpesaConfig(); err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("%s with an http callback: %v", env, err)
		}
	}
	t.Setenv("MPESA_CALLBACK_URL", "https://isp.example.com")
	if _, err := loadMpesaConfig(); err != nil {
		t.Errorf("https callback refused: %v", err)
	}
	t.Setenv("MPESA_CONSUMER_KEY", "")
	if _, err := loadMpesaConfig(); err != errMpesaDisabled {
		t.Errorf("no key: %v, want disabled", err)
	}
}

// Render generates the callback secret as base64, which has "/", "+" and "=" in it. Put in a
// URL as it is, "/" splits the address, no route matches and Safaricom only ever sees a 404
// (found on the first cloud deployment, October 2026).
func TestMpesaTokenWithUrlUnsafeCharacters(t *testing.T) {
	newMpesaRig(t)
	t.Setenv("MPESA_CALLBACK_URL", "https://isp.example.com")
	t.Setenv("MPESA_BASE_URL", "")
	t.Setenv("MPESA_CALLBACK_TOKEN", "pYWLbTOoimUqW/KU5c/JsUIG746zz+Zjvwp3iPBkXqg=")
	c, err := loadMpesaConfig()
	if err != nil {
		t.Fatal(err)
	}
	u := c.callbackURL("confirm")
	if rest := strings.TrimPrefix(u, "https://isp.example.com/api/v1/pay/confirm/"); rest == u || strings.ContainsAny(rest, "/+=?#% ") || len(rest) < 24 {
		t.Errorf("callback address %q is not safe to use in a path", u)
	}
	c2, _ := loadMpesaConfig()
	if c2.callbackURL("confirm") != u {
		t.Error("the same secret gave a different address the second time")
	}
	t.Setenv("MPESA_CALLBACK_TOKEN", mpesaToken)
	if c3, _ := loadMpesaConfig(); c3.Token != mpesaToken {
		t.Errorf("a URL-safe secret was changed to %q", c3.Token)
	}
}

// Customers paying the paybill themselves, with every kind of account number.
func TestMpesaPaybill(t *testing.T) {
	m := newMpesaRig(t)
	uidA, _ := m.customer("Akinyi", "0712000001")
	uidB, invB := m.customer("bwire", "0712000002")
	uidC, _ := m.customer("cherono", "0712000003")
	uidD, _ := m.customer("dida", "0712000004")
	numB := scanString(t, m.db, `SELECT invoice_number FROM invoices WHERE id=$1`, invB)

	cases := []struct{ trans, account, amount, msisdn string }{
		{"QPB0000001", " akinyi ", "1500.00", "254799999999"},         // username, other case and spaces
		{"QPB0000002", strings.ToLower(numB), "1500", "254799999999"}, // invoice number
		{"QPB0000003", "wrong-account", "1500.00", "254712000003"},    // known phone
		{"QPB0000004", "dida", "3000.00", "2547 ***** 004"},           // two months at once (masked MSISDN)
		{"QPB0000005", "nobody-here", "700.00", "254799999999"},       // unknown: held for staff
		{"QPB0000006", "akinyi", "200.00", "254799999999"},            // nothing owed and too little for a period: kept on account
	}
	for _, c := range cases {
		if code := m.c2b(c.trans, c.account, c.amount, c.msisdn); code != 200 {
			t.Errorf("%s answered %d", c.trans, code)
		}
	}
	// Safaricom repeats a confirmation, ten times at once.
	parallel(10, func(int) { m.c2b("QPB0000001", "akinyi", "1500.00", "254799999999") })

	for _, c := range []struct {
		uid, days int
		who       string
	}{{uidA, 30, "username"}, {uidB, 30, "invoice number"}, {uidC, 30, "phone"}, {uidD, 60, "two months"}} {
		if got := m.paidUntil(c.uid); got != c.days {
			t.Errorf("%s: paid until today%+d, want %+d", c.who, got, c.days)
		}
	}
	if n, sum := m.mpesaPayments("Akinyi"); n != 2 || sum != 1700 {
		t.Errorf("Akinyi: %d payments %.2f, want 1500 + 200 on account", n, sum)
	}
	if n := scanInt(t, m.db, `SELECT COUNT(*) FROM invoices WHERE username='dida' AND status='paid'`); n != 2 {
		t.Errorf("dida: %d paid invoices, want 2", n)
	}
	code, list := m.do("GET", "/api/v1/mpesa-admin/c2b?status=unmatched", nil, nil)
	if code != 200 || list["unmatched"] != float64(1) || list["unmatched_amount"] != float64(700) {
		t.Errorf("unmatched list: %v", list)
	}
	// Staff allocate the unknown one; doing it twice is refused.
	cid := scanInt(t, m.db, `SELECT id FROM mpesa_c2b WHERE trans_id='QPB0000005'`)
	_, invE := m.customer("emuria", "0712000005")
	if code, out := m.do("POST", fmt.Sprintf("/api/v1/mpesa-admin/c2b/%d/allocate", cid), map[string]interface{}{"username": "emuria", "invoice_id": invE}, nil); code != 200 {
		t.Errorf("allocate: %d %v", code, out)
	}
	if code, _ := m.do("POST", fmt.Sprintf("/api/v1/mpesa-admin/c2b/%d/allocate", cid), map[string]interface{}{"username": "emuria"}, nil); code != 409 {
		t.Errorf("second allocation: %d, want 409", code)
	}
	if paid := scanFloat(t, m.db, `SELECT COALESCE(SUM(amount),0) FROM payments WHERE invoice_id=$1`, invE); paid != 700 {
		t.Errorf("allocated part payment %.2f, want 700", paid)
	}
	// The books: every unique shilling received is either a payment or held unmatched.
	received := scanFloat(t, m.db, `SELECT SUM(amount) FROM mpesa_c2b`)
	recorded := scanFloat(t, m.db, `SELECT SUM(amount) FROM payments WHERE gateway='mpesa' AND status='confirmed'`)
	held := scanFloat(t, m.db, `SELECT COALESCE(SUM(amount),0) FROM mpesa_c2b WHERE status='unmatched'`)
	if received != recorded+held || received != 1500+1500+1500+3000+700+200 {
		t.Errorf("received %.2f, recorded %.2f + held %.2f", received, recorded, held)
	}
	// Validation: unknown accounts are refused before the money moves, when asked to.
	t.Setenv("MPESA_C2B_REJECT_UNKNOWN", "true")
	resetMpesaClient()
	_, out := m.do("POST", "/api/v1/pay/validate/"+mpesaToken, map[string]string{"BillRefNumber": "nobody"}, nil)
	if out["ResultCode"] != "C2B00012" {
		t.Errorf("validation of an unknown account: %v", out)
	}
	_, out = m.do("POST", "/api/v1/pay/validate/"+mpesaToken, map[string]string{"BillRefNumber": "BWIRE"}, nil)
	if out["ResultCode"] != "0" {
		t.Errorf("validation of a known account: %v", out)
	}
	failIf(t, checkMoney(m.db))
	failIf(t, checkEnforcement(m.db))
}

// A confirmation with a NUL in a name (PostgreSQL cannot store one) is still recorded and
// applied: Safaricom's word that money arrived is never refused.
func TestMpesaPaybillOddText(t *testing.T) {
	m := newMpesaRig(t)
	uid, _ := m.customer("oddtext", "0711000999")
	code := postJSON(m.api.URL+"/api/v1/pay/confirm/"+mpesaToken, map[string]string{
		"TransactionType": "Pay Bill", "TransID": "QNUL000001", "TransTime": "20261023150405", "TransAmount": "1500.00",
		"BusinessShortCode": "174379", "BillRefNumber": "oddtext", "MSISDN": "254711000999", "FirstName": "JO\x00HN", "LastName": "D\xffOE"})
	if code != 200 {
		t.Fatalf("confirmation with a NUL answered %d", code)
	}
	if st := scanString(t, m.db, `SELECT status FROM mpesa_c2b WHERE trans_id='QNUL000001'`); st != "applied" {
		t.Errorf("confirmation is %q, want applied", st)
	}
	if m.paidUntil(uid) != 30 {
		t.Errorf("paid until today%+d, want +30", m.paidUntil(uid))
	}
	failIf(t, checkMoney(m.db))
}

// A code staff already typed in by hand, then confirmed by Safaricom: recorded once.
func TestMpesaPaybillAlreadyEnteredByHand(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("faraji", "0712000010")
	if _, err := payTx(m.db, paymentInput{InvoiceID: &inv, Amount: 1500, PaymentMethod: "mobile_money", Gateway: "mpesa", GatewayRef: "QHAND00001"}); err != nil {
		t.Fatal(err)
	}
	m.c2b("QHAND00001", "faraji", "1500.00", "254712000010")
	if n, _ := m.mpesaPayments("faraji"); n != 1 {
		t.Errorf("%d payments, want 1", n)
	}
	if st := scanString(t, m.db, `SELECT status FROM mpesa_c2b WHERE trans_id='QHAND00001'`); st != "applied" {
		t.Errorf("confirmation %s, want applied (to the payment entered by hand)", st)
	}
	if m.paidUntil(uid) != 30 {
		t.Error("period granted twice or not at all")
	}
}

// A lapsed customer renews themselves from the portal: the invoice is raised, the prompt
// sent, and after paying they are online.
func TestMpesaPortalSelfService(t *testing.T) {
	m := newMpesaRig(t)
	uid, inv := m.customer("pendo", "0712000020")
	if _, err := payTx(m.db, paymentInput{InvoiceID: &inv, Amount: 1500}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, m.db, `UPDATE radius_users SET account_expiry = CURRENT_DATE - 3 WHERE id=$1`, uid)
	runTask(m.db, testLogger(), "expire_accounts")

	code, out := m.do("POST", "/api/v1/portal/login", map[string]string{"username": "pendo", "password": "pw-pendo"}, nil)
	if code != 200 {
		t.Fatalf("portal login: %d %v", code, out)
	}
	tok, _ := out["token"].(string)
	hdr := map[string]string{"X-Portal-Token": tok}
	code, out = m.do("POST", "/api/v1/portal/pay", map[string]string{}, hdr)
	if code != 201 {
		t.Fatalf("portal pay: %d %v", code, out)
	}
	id := int(out["id"].(float64))
	p := m.daraja.lastPush()
	if p.Phone != "254712000020" || p.Amount != 1500 {
		t.Errorf("push %+v (the phone on file should be used)", p)
	}
	m.daraja.sendResult(p, 0, "QPORTAL001")
	code, out = m.do("GET", fmt.Sprintf("/api/v1/portal/pay/%d", id), nil, hdr)
	if code != 200 || out["status"] != "paid" {
		t.Errorf("portal status: %d %v", code, out)
	}
	if m.paidUntil(uid) != 30 {
		t.Errorf("paid until today%+d after self-service renewal", m.paidUntil(uid))
	}
	// Another customer cannot read this prompt.
	m.customer("mgeni", "0712000021")
	_, out2 := m.do("POST", "/api/v1/portal/login", map[string]string{"username": "mgeni", "password": "pw-mgeni"}, nil)
	if code, _ := m.do("GET", fmt.Sprintf("/api/v1/portal/pay/%d", id), nil, map[string]string{"X-Portal-Token": out2["token"].(string)}); code != 404 {
		t.Errorf("another customer read the prompt: %d", code)
	}
	if r := optionalRadius(); r != nil && !r.accepts(t, "pendo", "pw-pendo") {
		t.Errorf("FreeRADIUS refuses the customer who renewed from the portal")
	}
}

func TestMpesaRegisterC2B(t *testing.T) {
	m := newMpesaRig(t)
	if code, out := m.do("POST", "/api/v1/mpesa-admin/register-c2b", nil, nil); code != 200 {
		t.Fatalf("register: %d %v", code, out)
	}
	r := m.daraja.registered
	for _, u := range []string{r["ConfirmationURL"], r["ValidationURL"], m.daraja.lastPushOrEmpty().Callback} {
		if w := darajaForbiddenWord(u); w != "" {
			t.Errorf("callback URL %s contains %q; Safaricom would refuse it", u, w)
		}
	}
	if r["ShortCode"] != "174379" || r["ResponseType"] != "Completed" ||
		r["ConfirmationURL"] != m.api.URL+"/api/v1/pay/confirm/"+mpesaToken {
		t.Errorf("registered %v", r)
	}
	t.Setenv("MPESA_C2B_SHORTCODE", "600981")
	resetMpesaClient()
	m.do("POST", "/api/v1/mpesa-admin/register-c2b", nil, nil)
	if m.daraja.registered["ShortCode"] != "600981" {
		t.Errorf("MPESA_C2B_SHORTCODE not used for registration: %v", m.daraja.registered)
	}
	t.Setenv("MPESA_CALLBACK_URL", "https://mpesa.example.com")
	if _, err := loadMpesaConfig(); err == nil {
		t.Error("a callback host containing \"mpesa\" was accepted")
	}
	t.Setenv("MPESA_CALLBACK_URL", m.api.URL)
	resetMpesaClient()
	_, st := m.do("GET", "/api/v1/mpesa-admin/status", nil, nil)
	if st["enabled"] != true || strings.Contains(fmt.Sprint(st), mpesaToken) {
		t.Errorf("status shows %v (must not reveal the token)", st)
	}
}

// Many customers, prompts and paybill payments at once, with duplicates, lost callbacks,
// refusals and late answers in random order. At the end every shilling Safaricom reports is
// in the books exactly once, and every customer's access matches.
func TestMpesaStorm(t *testing.T) {
	m := newMpesaRig(t)
	rng := rand.New(rand.NewSource(42))
	n := simEnv("MPESA_STORM_CUSTOMERS", 60)
	type cust struct {
		name string
		uid  int
		inv  int
	}
	var cs []cust
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("storm%03d", i)
		uid, inv := m.customer(name, fmt.Sprintf("07%08d", i))
		cs = append(cs, cust{name, uid, inv})
	}
	var expected int64 // shillings Safaricom says it took, unique per code
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func(i int, c cust, seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			if i%3 == 0 { // pays the paybill instead, maybe twice the confirmation, maybe double
				amt := []int{1500, 3000, 700}[r.Intn(3)]
				trans := fmt.Sprintf("QST%07d", i)
				for k := 0; k <= r.Intn(3); k++ {
					m.c2b(trans, c.name, strconv.Itoa(amt), "254700000000")
				}
				atomic.AddInt64(&expected, int64(amt))
				return
			}
			code, out := m.prompt(c.inv, fmt.Sprintf("07%08d", i))
			if code != 201 {
				t.Errorf("%s prompt: %d %v", c.name, code, out)
				return
			}
			id := int(out["id"].(float64))
			var p fakePush
			m.daraja.mu.Lock()
			for _, x := range m.daraja.pushes {
				if strings.HasSuffix(x.Phone, fmt.Sprintf("%08d", i)) {
					p = x
				}
			}
			m.daraja.mu.Unlock()
			switch r.Intn(4) {
			case 0: // pays, callback delivered 1-3 times
				for k := 0; k <= r.Intn(3); k++ {
					m.daraja.sendResult(p, 0, fmt.Sprintf("QSK%07d", i))
				}
				atomic.AddInt64(&expected, int64(p.Amount))
			case 1: // pays, callback lost: reconcile must find it
				m.daraja.settle(p.CheckoutID, 0)
				mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE id=$1`, id)
				atomic.AddInt64(&expected, int64(p.Amount))
			case 2: // cancels
				m.daraja.sendResult(p, 1032, "")
			case 3: // pays; callback and reconcile collide
				m.daraja.settle(p.CheckoutID, 0)
				mustExec(t, m.db, `UPDATE mpesa_stk SET created_at = NOW() - INTERVAL '5 minutes' WHERE id=$1`, id)
				go reconcileMpesa(m.db, testLogger())
				m.daraja.sendResult(p, 0, fmt.Sprintf("QSK%07d", i))
				atomic.AddInt64(&expected, int64(p.Amount))
			}
		}(i, c, rng.Int63())
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)
	mustExec(t, m.db, `UPDATE mpesa_stk SET last_query_at = NULL`)
	reconcileMpesa(m.db, testLogger())

	recorded := scanFloat(t, m.db, `SELECT COALESCE(SUM(amount),0) FROM payments WHERE gateway='mpesa' AND status='confirmed'`)
	held := scanFloat(t, m.db, `SELECT COALESCE(SUM(amount),0) FROM mpesa_c2b WHERE status='unmatched'`)
	if recorded+held != float64(expected) {
		t.Errorf("Safaricom took %d; the books have %.2f recorded + %.2f held", expected, recorded, held)
	}
	if left := scanInt(t, m.db, `SELECT COUNT(*) FROM mpesa_stk WHERE status IN ('sending','pending')
		AND checkout_request_id IN (SELECT checkout_request_id FROM mpesa_stk)`); left > 0 {
		t.Logf("%d prompts still pending (cancelled ones are failed; these were never answered)", left)
	}
	t.Logf("%d customers; Safaricom took KES %d; recorded %.0f, held %.0f", n, expected, recorded, held)
	m.daraja.check()
	failIf(t, checkMoney(m.db))
	failIf(t, checkEnforcement(m.db))
	if r := optionalRadius(); r != nil {
		pw := map[string]string{}
		for _, c := range cs {
			pw[c.name] = "pw-" + c.name
		}
		failIf(t, checkRealRadius(r, m.db, pw))
	}
}
