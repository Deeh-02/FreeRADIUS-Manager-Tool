package handlers

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/freeradius-manager/backend/internal/database"
	"github.com/freeradius-manager/backend/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/sirupsen/logrus"
)

// ─────────────────────────────────────────────────────────────────────────────
// M-Pesa through Safaricom Daraja
// ─────────────────────────────────────────────────────────────────────────────
//
// Two ways money arrives:
//
//   - STK push ("Lipa na M-Pesa Online"): staff press "Send M-Pesa prompt" on an invoice, or
//     the customer presses "Pay with M-Pesa" in the portal; the customer's phone asks for their
//     PIN; Safaricom posts the result to our callback. If the callback never comes, the
//     reconcile loop asks Safaricom (STK query).
//   - C2B: the customer pays the paybill/till from their own phone, typing their account
//     number (username or invoice number); Safaricom posts a confirmation.
//
// Either way applyMpesaMoney records ordinary payments (recordPaymentTx), so everything the
// billing tests prove holds here too: no payment twice, a paid invoice grants its period once,
// the customer is online the moment it commits. Every shilling received is recorded: beyond what
// is owed it pays the plan's next period, and what is left is kept on the customer's account;
// a C2B payment whose account number matches nobody waits, unmatched, for staff to allocate.
//
// Safaricom does not sign callbacks. They are trusted because (1) the URL carries a long secret
// (MPESA_CALLBACK_TOKEN), (2) optionally only Safaricom's addresses may call it
// (MPESA_ALLOWED_IPS), and (3) an STK result is only applied to a prompt we sent ourselves.
//
// Configuration (environment, never the database: these are secrets):
//
//	MPESA_ENV              sandbox (default) or production
//	MPESA_CONSUMER_KEY     from the Daraja app
//	MPESA_CONSUMER_SECRET
//	MPESA_SHORTCODE        paybill or store number (sandbox: 174379)
//	MPESA_C2B_SHORTCODE    where paybill payments are registered, if not MPESA_SHORTCODE
//	                       (the sandbox uses a separate 600xxx test code for C2B)
//	MPESA_PASSKEY          Lipa na M-Pesa Online passkey
//	MPESA_TYPE             paybill (default) or till
//	MPESA_TILL             till number customers pay to, when MPESA_TYPE=till
//	MPESA_CALLBACK_URL     public https address of this system, e.g. https://isp.example.com
//	MPESA_CALLBACK_TOKEN   random secret of at least 24 characters, part of the callback URLs
//	MPESA_ALLOWED_IPS      optional comma-separated addresses/CIDRs allowed to call callbacks
//	MPESA_C2B_REJECT_UNKNOWN  true: the validation URL refuses account numbers we do not know
//	MPESA_BASE_URL         override the Daraja address (tests)

type mpesaConfig struct {
	BaseURL        string
	ConsumerKey    string
	ConsumerSecret string
	ShortCode      string
	C2BShortCode   string // where paybill/till confirmations are registered (sandbox: a 600xxx test code)
	Passkey        string
	TxType         string // CustomerPayBillOnline or CustomerBuyGoodsOnline
	PartyB         string
	CallbackBase   string
	Token          string
	AllowedIPs     []*net.IPNet
	RejectUnknown  bool
	Env            string
}

var errMpesaDisabled = errors.New("M-Pesa is not configured on this server (MPESA_* settings)")

func loadMpesaConfig() (*mpesaConfig, error) {
	c := &mpesaConfig{
		ConsumerKey:    strings.TrimSpace(os.Getenv("MPESA_CONSUMER_KEY")),
		ConsumerSecret: strings.TrimSpace(os.Getenv("MPESA_CONSUMER_SECRET")),
		ShortCode:      strings.TrimSpace(os.Getenv("MPESA_SHORTCODE")),
		Passkey:        strings.TrimSpace(os.Getenv("MPESA_PASSKEY")),
		CallbackBase:   strings.TrimRight(strings.TrimSpace(os.Getenv("MPESA_CALLBACK_URL")), "/"),
		Token:          strings.TrimSpace(os.Getenv("MPESA_CALLBACK_TOKEN")),
		RejectUnknown:  os.Getenv("MPESA_C2B_REJECT_UNKNOWN") == "true",
		Env:            strings.ToLower(strings.TrimSpace(os.Getenv("MPESA_ENV"))),
	}
	if c.ConsumerKey == "" {
		return nil, errMpesaDisabled
	}
	switch c.Env {
	case "", "sandbox":
		c.Env, c.BaseURL = "sandbox", "https://sandbox.safaricom.co.ke"
	case "production":
		c.BaseURL = "https://api.safaricom.co.ke"
	default:
		return nil, fmt.Errorf("MPESA_ENV must be sandbox or production, not %q", c.Env)
	}
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("MPESA_BASE_URL")), "/"); v != "" {
		c.BaseURL = v
	}
	c.C2BShortCode = c.ShortCode
	if v := strings.TrimSpace(os.Getenv("MPESA_C2B_SHORTCODE")); v != "" {
		c.C2BShortCode = v
	}
	c.TxType, c.PartyB = "CustomerPayBillOnline", c.ShortCode
	if strings.EqualFold(os.Getenv("MPESA_TYPE"), "till") {
		c.TxType, c.PartyB = "CustomerBuyGoodsOnline", strings.TrimSpace(os.Getenv("MPESA_TILL"))
	}
	var missing []string
	for k, v := range map[string]string{"MPESA_CONSUMER_SECRET": c.ConsumerSecret, "MPESA_SHORTCODE": c.ShortCode,
		"MPESA_PASSKEY": c.Passkey, "MPESA_CALLBACK_URL": c.CallbackBase, "MPESA_CALLBACK_TOKEN": c.Token} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if c.PartyB == "" {
		missing = append(missing, "MPESA_TILL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("M-Pesa is half configured; missing %s", strings.Join(missing, ", "))
	}
	if len(c.Token) < 24 {
		return nil, errors.New("MPESA_CALLBACK_TOKEN must be at least 24 random characters (openssl rand -hex 16)")
	}
	if w := darajaForbiddenWord(c.CallbackBase); w != "" {
		return nil, fmt.Errorf("MPESA_CALLBACK_URL contains %q, which Safaricom refuses in callback addresses; use another host name", w)
	}
	// Safaricom refuses plain-http callbacks, and each refused request counts towards its
	// firewall blocking this server (see darajaMinGap), so this is checked here, not by them.
	// MPESA_BASE_URL (a local fake Daraja in tests) is the only exception.
	if !strings.HasPrefix(c.CallbackBase, "https://") && os.Getenv("MPESA_BASE_URL") == "" {
		return nil, errors.New("MPESA_CALLBACK_URL must start with https:// (Safaricom refuses other addresses)")
	}
	for _, s := range strings.Split(os.Getenv("MPESA_ALLOWED_IPS"), ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("MPESA_ALLOWED_IPS: %q is not an address or CIDR", s)
		}
		c.AllowedIPs = append(c.AllowedIPs, n)
	}
	return c, nil
}

// callbackURL is where Safaricom posts: kind is result (STK), confirm or validate (C2B). The
// path must avoid the words Daraja refuses in URLs (mpesa, safaricom, query, sql, exe, cmd).
// darajaForbiddenWord returns a word Daraja refuses in URLs, if s contains one.
func darajaForbiddenWord(s string) string {
	l := strings.ToLower(s)
	for _, w := range []string{"m-pesa", "mpesa", "safaricom", "query", "sql", "exe", "cmd"} {
		if strings.Contains(l, w) {
			return w
		}
	}
	return ""
}

func (c *mpesaConfig) callbackURL(kind string) string {
	return c.CallbackBase + "/api/v1/pay/" + kind + "/" + c.Token
}

// ── Daraja client ───────────────────────────────────────────────────────────

type darajaClient struct {
	cfg      *mpesaConfig
	http     *http.Client
	mu       sync.Mutex
	token    string
	tokenExp time.Time

	paceMu       sync.Mutex
	lastSend     time.Time
	blockedUntil time.Time // Safaricom's firewall blocked us: don't call until then
}

// Two limits found against the real sandbox:
//   - Daraja's gateway allows 30 requests a minute with bursts of 3, and answers more with
//     429 "Spike arrest violation" (JSON). Such a request never reached M-Pesa, so it is safe
//     to send again, even a prompt.
//   - Safaricom's firewall (Incapsula) blocks an address that keeps sending too quickly, for
//     every API including sign-in, for a few minutes, with an HTML page (403).
// So calls are spaced out, a 429 is retried after a pause, and after a block the client waits
// instead of hammering, which would only extend it.
var (
	darajaMinGap    = time.Second
	darajaSpikeWait = 2 * time.Second
	darajaBlockWait = 2 * time.Minute
)

const darajaSpikeRetries = 4

// errFirewall means Safaricom's firewall refused the request; nothing reached M-Pesa.
type errFirewall struct{ until time.Time }

func (e *errFirewall) Error() string {
	return fmt.Sprintf("Safaricom's firewall is refusing this server's requests for now (HTTP 403); nothing was sent to M-Pesa. "+
		"It does this for a few minutes after too many requests, or a few refused ones, in a short time, and it clears by itself; "+
		"trying again after %s. If it keeps happening, check MPESA_SHORTCODE and MPESA_PASSKEY.",
		e.until.In(nairobi).Format("15:04"))
}

// send performs one HTTP request to Daraja: spaced out from the previous one, refused at once
// while a firewall block lasts, and turning the firewall's HTML page into errFirewall.
func (d *darajaClient) send(req *http.Request) (int, []byte, error) {
	d.paceMu.Lock()
	if now := time.Now(); now.Before(d.blockedUntil) {
		until := d.blockedUntil
		d.paceMu.Unlock()
		return 0, nil, &errFirewall{until}
	}
	if wait := time.Until(d.lastSend.Add(darajaMinGap)); wait > 0 {
		time.Sleep(wait)
	}
	d.lastSend = time.Now()
	d.paceMu.Unlock()

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "radius-manager/1.0")
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusForbidden && looksLikeHTML(raw) {
		d.paceMu.Lock()
		d.blockedUntil = time.Now().Add(darajaBlockWait)
		until := d.blockedUntil
		d.paceMu.Unlock()
		return resp.StatusCode, raw, &errFirewall{until}
	}
	return resp.StatusCode, raw, nil
}

func looksLikeHTML(b []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(b)))
	return strings.HasPrefix(s, "<") || strings.Contains(s, "incapsula")
}

// shortBody keeps an unexpected answer readable in a staff message.
func shortBody(b []byte) string {
	if looksLikeHTML(b) {
		return "(an HTML page)"
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty answer)"
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

var (
	mpesaMu     sync.Mutex
	mpesaCached *darajaClient
	mpesaErr    error
	mpesaLoaded bool
)

// mpesaClient returns the client for the current configuration (read once per process).
func mpesaClient() (*darajaClient, error) {
	mpesaMu.Lock()
	defer mpesaMu.Unlock()
	if !mpesaLoaded {
		cfg, err := loadMpesaConfig()
		mpesaErr = err
		if err == nil {
			mpesaCached = &darajaClient{cfg: cfg, http: defaultHTTPClient()}
		}
		mpesaLoaded = true
	}
	return mpesaCached, mpesaErr
}

// resetMpesaClient makes the next call read the configuration again (tests).
func resetMpesaClient() {
	mpesaMu.Lock()
	mpesaLoaded, mpesaCached, mpesaErr = false, nil, nil
	mpesaMu.Unlock()
}

// darajaError is an error answer from Safaricom.
type darajaError struct {
	Status  int
	Code    string
	Message string
}

func (e *darajaError) Error() string {
	return fmt.Sprintf("Safaricom answered %d %s %s", e.Status, e.Code, e.Message)
}

func (d *darajaClient) accessToken(ctx context.Context, force bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !force && d.token != "" && time.Now().Before(d.tokenExp) {
		return d.token, nil
	}
	var status int
	var raw []byte
	var err error
	for spikes := 0; ; spikes++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", d.cfg.BaseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
		req.SetBasicAuth(d.cfg.ConsumerKey, d.cfg.ConsumerSecret)
		status, raw, err = d.send(req)
		if err != nil || status != http.StatusTooManyRequests || spikes == darajaSpikeRetries {
			break
		}
		select {
		case <-time.After(darajaSpikeWait * time.Duration(spikes+1)):
		case <-ctx.Done():
			return "", fmt.Errorf("Safaricom sign-in: %w", ctx.Err())
		}
	}
	if err != nil {
		var fw *errFirewall
		if errors.As(err, &fw) {
			return "", err
		}
		return "", fmt.Errorf("Safaricom sign-in: %w", err)
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		// A wrong key or secret: 400 with an empty body (real sandbox).
		return "", &darajaError{Status: status, Message: "sign-in refused; check MPESA_CONSUMER_KEY and MPESA_CONSUMER_SECRET " + shortBody(raw)}
	}
	if status != 200 {
		return "", &darajaError{Status: status, Message: "sign-in failed " + shortBody(raw)}
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("Safaricom sign-in: unexpected answer %s", shortBody(raw))
	}
	secs, _ := strconv.Atoi(out.ExpiresIn)
	if secs <= 0 {
		secs = 3599
	}
	d.token, d.tokenExp = out.AccessToken, time.Now().Add(time.Duration(secs-60)*time.Second)
	return d.token, nil
}

// call posts JSON to Daraja with a bearer token, signing in again once if the token was
// refused (Safaricom expires them early sometimes). Daraja reports a bad token as HTTP 404
// with errorCode 404.001.03 ("Invalid Access Token"), not 401; both are handled.
func (d *darajaClient) call(ctx context.Context, path string, body, out interface{}) error {
	raw, _ := json.Marshal(body)
	spikes := 0
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := d.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", d.cfg.BaseURL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		status, reply, err := d.send(req)
		if err != nil {
			var fw *errFirewall
			if errors.As(err, &fw) {
				return err
			}
			return fmt.Errorf("Safaricom %s: %w", path, err)
		}
		var e struct {
			ErrorCode    string `json:"errorCode"`
			ErrorMessage string `json:"errorMessage"`
			Fault        struct {
				FaultString string `json:"faultstring"`
			} `json:"fault"`
		}
		if status != 200 {
			json.Unmarshal(reply, &e)
		}
		if status == http.StatusTooManyRequests {
			if spikes < darajaSpikeRetries {
				spikes++
				attempt--
				select {
				case <-time.After(darajaSpikeWait * time.Duration(spikes)):
				case <-ctx.Done():
					return fmt.Errorf("Safaricom %s: %w", path, ctx.Err())
				}
				continue
			}
			return &darajaError{Status: status, Code: "429",
				Message: "Safaricom is refusing requests for a moment (too many in the last minute); try again shortly"}
		}
		if e.ErrorMessage == "" && e.Fault.FaultString != "" {
			e.ErrorMessage = e.Fault.FaultString
		}
		badToken := status == http.StatusUnauthorized || strings.HasPrefix(e.ErrorCode, "404.001.0") ||
			strings.Contains(strings.ToLower(e.ErrorMessage), "invalid access token")
		if badToken && attempt == 0 {
			continue
		}
		if status != 200 {
			if e.ErrorMessage == "" {
				e.ErrorMessage = shortBody(reply)
			}
			return &darajaError{Status: status, Code: e.ErrorCode, Message: e.ErrorMessage}
		}
		if err := json.Unmarshal(reply, out); err != nil {
			return fmt.Errorf("Safaricom %s: unexpected answer %s", path, shortBody(reply))
		}
		return nil
	}
	return errors.New("Safaricom refused the access token twice")
}

var nairobi = func() *time.Location {
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.FixedZone("EAT", 3*3600)
}()

// stkPassword is base64(shortcode + passkey + timestamp), timestamp in Kenyan time.
func (d *darajaClient) stkPassword() (password, timestamp string) {
	timestamp = time.Now().In(nairobi).Format("20060102150405")
	return base64.StdEncoding.EncodeToString([]byte(d.cfg.ShortCode + d.cfg.Passkey + timestamp)), timestamp
}

type stkPushReply struct {
	MerchantRequestID   string `json:"MerchantRequestID"`
	CheckoutRequestID   string `json:"CheckoutRequestID"`
	ResponseCode        string `json:"ResponseCode"`
	ResponseDescription string `json:"ResponseDescription"`
	CustomerMessage     string `json:"CustomerMessage"`
}

func (d *darajaClient) stkPush(ctx context.Context, phone string, amount int, accountRef, desc string) (*stkPushReply, error) {
	pw, ts := d.stkPassword()
	if len(accountRef) > 12 {
		accountRef = accountRef[:12]
	}
	if len(desc) > 13 {
		desc = desc[:13]
	}
	var out stkPushReply
	err := d.call(ctx, "/mpesa/stkpush/v1/processrequest", map[string]interface{}{
		"BusinessShortCode": d.cfg.ShortCode, "Password": pw, "Timestamp": ts,
		"TransactionType": d.cfg.TxType, "Amount": amount, "PartyA": phone, "PartyB": d.cfg.PartyB,
		"PhoneNumber": phone, "CallBackURL": d.cfg.callbackURL("result"),
		"AccountReference": accountRef, "TransactionDesc": desc,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.ResponseCode != "0" || out.CheckoutRequestID == "" {
		return nil, fmt.Errorf("Safaricom did not accept the prompt: %s %s", out.ResponseCode, out.ResponseDescription)
	}
	return &out, nil
}

// stkQuery asks for the result of a prompt. pending is true while the customer has not
// answered yet (Safaricom reports that as an error).
func (d *darajaClient) stkQuery(ctx context.Context, checkoutID string) (code, desc string, pending bool, err error) {
	pw, ts := d.stkPassword()
	var out struct {
		ResultCode string `json:"ResultCode"`
		ResultDesc string `json:"ResultDesc"`
	}
	err = d.call(ctx, "/mpesa/stkpushquery/v1/query", map[string]interface{}{
		"BusinessShortCode": d.cfg.ShortCode, "Password": pw, "Timestamp": ts, "CheckoutRequestID": checkoutID,
	}, &out)
	var de *darajaError
	if errors.As(err, &de) && (de.Code == "500.001.1001" || strings.Contains(strings.ToLower(de.Message), "being processed")) {
		return "", de.Message, true, nil
	}
	if err != nil {
		return "", "", false, err
	}
	// The real sandbox also answers an unanswered prompt with HTTP 200 and ResultCode 4999
	// "The transaction is still under processing". That is not a result.
	if out.ResultCode == "" || out.ResultCode == "4999" || strings.Contains(strings.ToLower(out.ResultDesc), "under processing") {
		return "", out.ResultDesc, true, nil
	}
	return out.ResultCode, out.ResultDesc, false, nil
}

func (d *darajaClient) registerC2B(ctx context.Context) error {
	var out map[string]interface{}
	return d.call(ctx, "/mpesa/c2b/v1/registerurl", map[string]interface{}{
		"ShortCode": d.cfg.C2BShortCode, "ResponseType": "Completed",
		"ConfirmationURL": d.cfg.callbackURL("confirm"),
		"ValidationURL":   d.cfg.callbackURL("validate"),
	}, &out)
}

// normalizePhone turns the ways Kenyans write a number (0712 345 678, +254712345678,
// 254712345678, 712345678, and the 01… range) into the 2547…/2541… form Daraja wants.
func normalizePhone(s string) (string, error) {
	d := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", "+", "").Replace(strings.TrimSpace(s))
	for _, r := range d {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%q is not a phone number", s)
		}
	}
	switch {
	case len(d) == 10 && (strings.HasPrefix(d, "07") || strings.HasPrefix(d, "01")):
		d = "254" + d[1:]
	case len(d) == 9 && (d[0] == '7' || d[0] == '1'):
		d = "254" + d
	}
	if len(d) != 12 || !(strings.HasPrefix(d, "2547") || strings.HasPrefix(d, "2541")) {
		return "", fmt.Errorf("%q is not a Kenyan mobile number (07XX XXX XXX)", s)
	}
	return d, nil
}

// ── Applying money ──────────────────────────────────────────────────────────

// errAlreadyRecorded: the M-Pesa code is already in the books (typed in by hand, say).
var errAlreadyRecorded = errors.New("this M-Pesa code is already recorded")

// applyMpesaMoney records amount received under M-Pesa code receipt for a customer: first on
// invoiceID (if given and open), then on their oldest open invoices, then on new invoices for
// their plan's next periods, and whatever is left as a payment on their account. It returns
// the payments it created. The caller holds the transaction.
func applyMpesaMoney(q dbtx, username string, invoiceID *int, amount float64, receipt, phone string, by *int) ([]int, error) {
	amount = round2(amount)
	if amount <= 0 {
		return nil, errors.New("nothing to apply")
	}
	var dup int
	err := q.QueryRow(`SELECT id FROM payments WHERE gateway = 'mpesa' AND upper(gateway_ref) = upper($1) AND status = 'confirmed' LIMIT 1`, receipt).Scan(&dup)
	if err == nil {
		return []int{dup}, errAlreadyRecorded
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	var uid int
	var planID sql.NullInt64
	err = q.QueryRow(`SELECT id, plan_id FROM radius_users WHERE username = $1`, username).Scan(&uid, &planID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		// Same lock the renewal task and plan assignment take, so no second invoice appears
		// for this customer while we raise one.
		if err := lockUser(q, uid); err != nil {
			return nil, err
		}
	}

	remaining := amount
	var ids []int
	part := 0
	pay := func(inv *int, amt float64, note string) error {
		part++
		ref := receipt
		if part > 1 {
			ref = fmt.Sprintf("%s-%d", receipt, part)
		}
		out, err := recordPaymentTx(q, paymentInput{InvoiceID: inv, Username: username, Amount: amt, Currency: "KES",
			PaymentMethod: "mobile_money", Gateway: "mpesa", GatewayRef: ref, ProcessedBy: by,
			Notes:          strings.TrimSpace("M-Pesa " + receipt + " from " + phone + ". " + note),
			IdempotencyKey: "mpesa:" + ref})
		if err != nil {
			return err
		}
		ids = append(ids, out.PaymentID)
		remaining = round2(remaining - amt)
		return nil
	}
	owedOn := func(id int) (float64, string, error) {
		var owed float64
		var cur string
		err := q.QueryRow(`SELECT i.amount - COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id = i.id AND p.status = 'confirmed'), 0), i.currency
			FROM invoices i WHERE i.id = $1 AND i.status IN ('pending','overdue')`, id).Scan(&owed, &cur)
		return round2(owed), cur, err
	}

	if invoiceID != nil {
		owed, cur, err := owedOn(*invoiceID)
		if err == nil && owed > 0 && cur == "KES" {
			if err := pay(invoiceID, math.Min(owed, remaining), ""); err != nil {
				return ids, err
			}
		} else if err != nil && err != sql.ErrNoRows {
			return ids, err
		}
	}
	for i := 0; uid != 0 && remaining >= 0.01 && i < 24; i++ {
		var next int
		err := q.QueryRow(`SELECT id FROM invoices WHERE user_id = $1 AND status IN ('pending','overdue') AND currency = 'KES'
			AND amount > COALESCE((SELECT SUM(p.amount) FROM payments p WHERE p.invoice_id = invoices.id AND p.status = 'confirmed'), 0)
			ORDER BY COALESCE(due_date, created_at::date), id LIMIT 1`, uid).Scan(&next)
		if err == sql.ErrNoRows {
			// Nothing open: if there is enough for something, raise the plan's next period. Less
			// than a shilling (M-Pesa rounds up) is not worth an invoice.
			if !planID.Valid || remaining < 1 {
				break
			}
			p, err := loadPlan(q, int(planID.Int64))
			if err != nil || !p.IsActive || p.Price <= 0 || !strings.EqualFold(p.Currency, "KES") {
				break
			}
			pid, pname, days := p.ID, p.Name, p.ValidityDays
			today := time.Now()
			inv, err := createInvoiceTx(q, newInvoice{UserID: &uid, Username: username, PlanID: &pid, PlanName: &pname,
				Amount: p.Price, Currency: "KES", DueDate: &today, PlanDays: &days,
				Notes: "raised for M-Pesa payment " + receipt, CreatedBy: by})
			if err != nil {
				return ids, err
			}
			next = inv.ID
		} else if err != nil {
			return ids, err
		}
		owed, _, err := owedOn(next)
		if err != nil {
			return ids, err
		}
		if err := pay(&next, math.Min(owed, remaining), ""); err != nil {
			return ids, err
		}
	}
	if remaining >= 0.01 {
		if err := pay(nil, remaining, "More than is owed; kept on the customer's account."); err != nil {
			return ids, err
		}
	}
	return ids, nil
}

// ── STK push ────────────────────────────────────────────────────────────────

var errPromptTooSoon = errors.New("a payment prompt for this invoice was sent less than 2 minutes ago; wait for the customer to answer it")

type stkStarted struct {
	ID              int     `json:"id"`
	Amount          float64 `json:"amount"`
	Phone           string  `json:"phone"`
	CustomerMessage string  `json:"customer_message"`
}

// startSTK sends a payment prompt for what is still owed on an invoice.
func startSTK(db *database.DB, invoiceID int, phoneIn string, by *int, viaPortal bool) (*stkStarted, error) {
	cl, err := mpesaClient()
	if err != nil {
		return nil, err
	}
	phone, err := normalizePhone(phoneIn)
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var amount, paid float64
	var status, currency, username, number string
	err = tx.QueryRow(`SELECT amount, status, currency, username, invoice_number FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).
		Scan(&amount, &status, &currency, &username, &number)
	if err == sql.ErrNoRows {
		return nil, errors.New("invoice not found")
	}
	if err != nil {
		return nil, err
	}
	if status != "pending" && status != "overdue" {
		return nil, errInvoiceNotOpen
	}
	if !strings.EqualFold(currency, "KES") {
		return nil, fmt.Errorf("M-Pesa takes Kenyan shillings; this invoice is in %s", currency)
	}
	tx.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM payments WHERE invoice_id=$1 AND status='confirmed'`, invoiceID).Scan(&paid)
	owed := round2(amount - paid)
	if owed <= 0 {
		return nil, errors.New("nothing is owed on this invoice")
	}
	var recent int
	tx.QueryRow(`SELECT COUNT(*) FROM mpesa_stk WHERE invoice_id = $1 AND status IN ('sending','pending')
		AND created_at > NOW() - INTERVAL '2 minutes'`, invoiceID).Scan(&recent)
	if recent > 0 {
		return nil, errPromptTooSoon
	}
	// M-Pesa takes whole shillings: round up; the cents over are kept on the account.
	whole := int(math.Ceil(owed - 0.000001))
	var id int
	if err := tx.QueryRow(`INSERT INTO mpesa_stk (invoice_id, username, phone, amount, requested_by, via_portal)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, invoiceID, username, phone, whole, by, viaPortal).Scan(&id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// The network call happens outside any transaction. The row says 'sending' until Safaricom
	// confirms; if that answer is lost, the reconcile loop deals with it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reply, err := cl.stkPush(ctx, phone, whole, number, "Internet")
	if err != nil {
		db.Exec(`UPDATE mpesa_stk SET status='failed', result_desc=$1, updated_at=NOW() WHERE id=$2 AND status='sending'`, err.Error(), id)
		return nil, err
	}
	if _, err := db.Exec(`UPDATE mpesa_stk SET checkout_request_id=$1, merchant_request_id=$2,
		status = CASE WHEN status='sending' THEN 'pending' ELSE status END, updated_at=NOW() WHERE id=$3`,
		reply.CheckoutRequestID, reply.MerchantRequestID, id); err != nil {
		return nil, err
	}
	return &stkStarted{ID: id, Amount: float64(whole), Phone: phone, CustomerMessage: reply.CustomerMessage}, nil
}

type stkCallbackBody struct {
	Body struct {
		StkCallback struct {
			MerchantRequestID string `json:"MerchantRequestID"`
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResultCode        int    `json:"ResultCode"`
			ResultDesc        string `json:"ResultDesc"`
			CallbackMetadata  struct {
				Item []struct {
					Name  string      `json:"Name"`
					Value interface{} `json:"Value"`
				} `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

func (b *stkCallbackBody) meta(name string) string {
	for _, it := range b.Body.StkCallback.CallbackMetadata.Item {
		if it.Name == name {
			switch v := it.Value.(type) {
			case float64:
				return strconv.FormatFloat(v, 'f', -1, 64)
			case string:
				return v
			}
		}
	}
	return ""
}

// settleSTK applies a prompt's final result. It is called by the callback and by the
// reconcile loop, possibly at the same time; the row lock makes exactly one of them apply it.
// receipt is empty when only a query (which does not return it) said it was paid.
func settleSTK(db *database.DB, log *logrus.Logger, checkoutID string, resultCode, resultDesc string, amount float64,
	receipt, phone string, raw []byte) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int
	var status, username string
	var invoiceID sql.NullInt64
	var requested float64
	var storedPhone string
	var by sql.NullInt64
	err = tx.QueryRow(`SELECT id, status, username, invoice_id, amount, phone, requested_by FROM mpesa_stk
		WHERE checkout_request_id = $1 FOR UPDATE`, checkoutID).
		Scan(&id, &status, &username, &invoiceID, &requested, &storedPhone, &by)
	if err == sql.ErrNoRows {
		return errUnknownCheckout
	}
	if err != nil {
		return err
	}
	var rawArg interface{}
	if raw != nil {
		rawArg = raw
	}
	if status == "paid" {
		// Already applied (a repeated callback, or the query got there first). If the query
		// did, the payments carry a stand-in reference; now that the M-Pesa code is known, put
		// it on them so staff can find the payment by the code on the customer's SMS.
		if receipt != "" && resultCode == "0" {
			res, err := tx.Exec(`UPDATE mpesa_stk SET mpesa_receipt=$1, raw_callback=COALESCE($2::jsonb, raw_callback), updated_at=NOW()
				WHERE id=$3 AND mpesa_receipt IS NULL`, receipt, rawArg, id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				if _, err := tx.Exec(`UPDATE payments SET gateway_ref = $1::text || substr(gateway_ref, length($2::text) + 1)
					WHERE id IN (SELECT unnest(payment_ids) FROM mpesa_stk WHERE id=$3) AND starts_with(gateway_ref, $2::text)`,
					receipt, "STK-"+checkoutID, id); err != nil {
					return err
				}
			}
			return tx.Commit()
		}
		return nil
	}
	if resultCode != "0" {
		// A failure after a success is impossible; after a failure it changes nothing.
		_, err := tx.Exec(`UPDATE mpesa_stk SET status='failed', result_code=$1, result_desc=$2,
			raw_callback=COALESCE($3::jsonb, raw_callback), updated_at=NOW() WHERE id=$4`, resultCode, resultDesc, rawArg, id)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	// Paid. Use the amount Safaricom says was paid (it should equal what we asked).
	if amount <= 0 {
		amount = requested
	}
	if phone == "" {
		phone = storedPhone
	}
	ref := receipt
	if ref == "" {
		ref = "STK-" + checkoutID // found by a query, which does not return the M-Pesa code
	}
	var inv *int
	if invoiceID.Valid {
		v := int(invoiceID.Int64)
		inv = &v
	}
	var byPtr *int
	if by.Valid {
		v := int(by.Int64)
		byPtr = &v
	}
	ids, err := applyMpesaMoney(tx, username, inv, amount, ref, phone, byPtr)
	if err != nil && err != errAlreadyRecorded {
		return err
	}
	if err == errAlreadyRecorded {
		log.Warnf("mpesa: %s for %s was already recorded by hand; not recorded again", ref, username)
	}
	if _, err := tx.Exec(`UPDATE mpesa_stk SET status='paid', result_code='0', result_desc=$1, mpesa_receipt=NULLIF($2,''),
		payment_ids=$3, raw_callback=COALESCE($4::jsonb, raw_callback), updated_at=NOW() WHERE id=$5`,
		resultDesc, receipt, pq.Array(ids), rawArg, id); err != nil {
		return err
	}
	return tx.Commit()
}

var errUnknownCheckout = errors.New("no payment prompt with this CheckoutRequestID")

// callbackAllowed checks the URL secret and, if configured, the caller's address. A wrong
// secret answers 404 so the endpoint does not reveal itself.
func (h *Handler) callbackAllowed(c *gin.Context) (*darajaClient, bool) {
	cl, err := mpesaClient()
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(c.Param("token")), []byte(cl.cfg.Token)) != 1 {
		c.AbortWithStatus(http.StatusNotFound)
		return nil, false
	}
	if len(cl.cfg.AllowedIPs) > 0 {
		ip := net.ParseIP(c.ClientIP())
		ok := false
		for _, n := range cl.cfg.AllowedIPs {
			if ip != nil && n.Contains(ip) {
				ok = true
			}
		}
		if !ok {
			h.log.Warnf("mpesa: callback from %s refused (not in MPESA_ALLOWED_IPS)", c.ClientIP())
			c.AbortWithStatus(http.StatusForbidden)
			return nil, false
		}
	}
	return cl, true
}

func mpesaAccepted(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"ResultCode": 0, "ResultDesc": "Accepted"})
}

// MpesaSTKCallback receives the result of a payment prompt.
func (h *Handler) MpesaSTKCallback(c *gin.Context) {
	if _, ok := h.callbackAllowed(c); !ok {
		return
	}
	raw, err := readCallback(c)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var b stkCallbackBody
	if err := json.Unmarshal(raw, &b); err != nil || b.Body.StkCallback.CheckoutRequestID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ResultCode": 1, "ResultDesc": "unreadable callback"})
		return
	}
	cb := b.Body.StkCallback
	amount, _ := strconv.ParseFloat(b.meta("Amount"), 64)
	receipt := strings.ToUpper(strings.TrimSpace(b.meta("MpesaReceiptNumber")))
	phone := b.meta("PhoneNumber")
	code := strconv.Itoa(cb.ResultCode)

	err = settleSTK(h.db, h.log, cb.CheckoutRequestID, code, cb.ResultDesc, amount, receipt, phone, raw)
	// The callback can overtake our own record of the prompt (Safaricom answered the push and
	// the customer paid before we saved the CheckoutRequestID). Wait a little for it.
	for i := 0; err == errUnknownCheckout && i < 10; i++ {
		time.Sleep(300 * time.Millisecond)
		err = settleSTK(h.db, h.log, cb.CheckoutRequestID, code, cb.ResultDesc, amount, receipt, phone, raw)
	}
	switch {
	case err == errUnknownCheckout && code == "0" && receipt != "":
		// Money was taken for a prompt we have no record of (our record of sending it was lost).
		// Keep it where staff will see it, never drop it.
		h.recordC2B(c, c2bConfirmation{TransID: receipt, TransAmount: b.meta("Amount"), BillRefNumber: "STK " + cb.CheckoutRequestID,
			MSISDN: phone, TransTime: b.meta("TransactionDate")}, raw, false)
		h.log.Warnf("mpesa: paid prompt %s not found; kept as unmatched payment %s", cb.CheckoutRequestID, receipt)
		return
	case err == errUnknownCheckout:
		h.log.Warnf("mpesa: result %s for unknown prompt %s ignored", code, cb.CheckoutRequestID)
	case err != nil:
		// Our side failed (database down): answer an error so Safaricom may retry; the
		// reconcile loop will ask for the result anyway.
		h.log.WithError(err).Errorf("mpesa: could not apply prompt %s", cb.CheckoutRequestID)
		c.JSON(http.StatusInternalServerError, gin.H{"ResultCode": 1, "ResultDesc": "temporary failure"})
		return
	}
	if code == "0" {
		go h.dispatchWebhook("payment.received", gin.H{"gateway": "mpesa", "checkout_request_id": cb.CheckoutRequestID,
			"receipt": receipt, "amount": amount})
	}
	mpesaAccepted(c)
}

// reconcileMpesa settles prompts whose callback has not come: asks Safaricom about prompts
// pending for over a minute (at most once a minute each), and gives up on prompts that never
// left (no answer to the push) or that nobody answered in a day.
func reconcileMpesa(db *database.DB, log *logrus.Logger) (string, int) {
	cl, err := mpesaClient()
	if err != nil {
		return "M-Pesa not configured", 0
	}
	res, _ := db.Exec(`UPDATE mpesa_stk SET status='failed', result_desc='Safaricom did not confirm the prompt was sent', updated_at=NOW()
		WHERE status='sending' AND created_at < NOW() - INTERVAL '2 minutes'`)
	stale, _ := res.RowsAffected()
	res, _ = db.Exec(`UPDATE mpesa_stk SET status='failed', result_desc='no answer from the customer within a day', updated_at=NOW()
		WHERE status='pending' AND created_at < NOW() - INTERVAL '1 day'`)
	old, _ := res.RowsAffected()

	// Claim the rows to ask about, so two backends do not both ask.
	rows, err := db.Query(`UPDATE mpesa_stk SET last_query_at = NOW()
		WHERE id IN (SELECT id FROM mpesa_stk WHERE status='pending' AND checkout_request_id IS NOT NULL
		             AND created_at < NOW() - INTERVAL '60 seconds'
		             AND (last_query_at IS NULL OR last_query_at < NOW() - INTERVAL '60 seconds')
		             ORDER BY created_at LIMIT 20 FOR UPDATE SKIP LOCKED)
		RETURNING checkout_request_id`)
	if err != nil {
		return "mpesa_reconcile failed: " + err.Error(), 0
	}
	var ids []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		ids = append(ids, s)
	}
	rows.Close()
	settled := 0
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		code, desc, pending, err := cl.stkQuery(ctx, id)
		cancel()
		var fw *errFirewall
		if errors.As(err, &fw) {
			// Asked again a minute after the block ends (last_query_at); asking now would extend it.
			log.Warn("mpesa: " + err.Error())
			db.Exec(`UPDATE mpesa_stk SET last_query_at = $1 WHERE status='pending' AND last_query_at > NOW() - INTERVAL '5 seconds'`, fw.until)
			break
		}
		if err != nil {
			log.WithError(err).Warnf("mpesa: query %s", id)
			continue
		}
		if pending {
			continue
		}
		if err := settleSTK(db, log, id, code, desc, 0, "", "", nil); err != nil {
			log.WithError(err).Warnf("mpesa: settle %s", id)
			continue
		}
		settled++
	}
	return fmt.Sprintf("%d prompt(s) checked, %d settled, %d never sent, %d unanswered for a day", len(ids), settled, stale, old),
		settled
}

// StartMpesaLoop checks unanswered prompts every minute (the scheduler's own loop is too slow
// for a customer waiting to get online).
func StartMpesaLoop(db *database.DB, log *logrus.Logger) {
	if _, err := mpesaClient(); err != nil {
		if err != errMpesaDisabled {
			log.Error("mpesa: " + err.Error())
		}
		return
	}
	go func() {
		for range time.Tick(time.Minute) {
			reconcileMpesa(db, log)
		}
	}()
}

// ── C2B (customer pays the paybill/till from their phone) ───────────────────

type c2bConfirmation struct {
	TransactionType   string `json:"TransactionType"`
	TransID           string `json:"TransID"`
	TransTime         string `json:"TransTime"`
	TransAmount       string `json:"TransAmount"`
	BusinessShortCode string `json:"BusinessShortCode"`
	BillRefNumber     string `json:"BillRefNumber"`
	InvoiceNumber     string `json:"InvoiceNumber"`
	MSISDN            string `json:"MSISDN"`
	FirstName         string `json:"FirstName"`
	MiddleName        string `json:"MiddleName"`
	LastName          string `json:"LastName"`
}

// matchAccount finds whose money a C2B payment is: an invoice number, else a username (any
// letter case, spaces ignored), else a customer whose phone is the payer's.
func matchAccount(q dbtx, billRef, msisdn string) (username string, invoiceID *int) {
	ref := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(billRef), " ", ""))
	if ref != "" {
		var id int
		var u sql.NullString
		if q.QueryRow(`SELECT id, username FROM invoices WHERE upper(invoice_number) = $1 AND user_id IS NOT NULL`, ref).Scan(&id, &u) == nil && u.Valid {
			return u.String, &id
		}
		var name string
		if q.QueryRow(`SELECT username FROM radius_users WHERE upper(username) = $1`, ref).Scan(&name) == nil {
			return name, nil
		}
	}
	if p, err := normalizePhone(msisdn); err == nil {
		rows, err := q.Query(`SELECT username, phone FROM radius_users WHERE phone IS NOT NULL AND phone <> ''`)
		if err == nil {
			var hits []string
			for rows.Next() {
				var name, ph string
				rows.Scan(&name, &ph)
				if n, err := normalizePhone(ph); err == nil && n == p {
					hits = append(hits, name)
				}
			}
			rows.Close()
			if len(hits) == 1 {
				return hits[0], nil
			}
		}
	}
	return "", nil
}

// MpesaC2BValidation lets Safaricom ask, before taking the money, whether the account number
// is known. Only used if Safaricom enabled validation for the shortcode.
func (h *Handler) MpesaC2BValidation(c *gin.Context) {
	cl, ok := h.callbackAllowed(c)
	if !ok {
		return
	}
	var in c2bConfirmation
	c.ShouldBindJSON(&in)
	if cl.cfg.RejectUnknown {
		if u, _ := matchAccount(h.db, in.BillRefNumber, ""); u == "" {
			c.JSON(http.StatusOK, gin.H{"ResultCode": "C2B00012", "ResultDesc": "Rejected"}) // invalid account number
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ResultCode": "0", "ResultDesc": "Accepted"})
}

// MpesaC2BConfirmation records a payment the customer made to the paybill/till.
func (h *Handler) MpesaC2BConfirmation(c *gin.Context) {
	if _, ok := h.callbackAllowed(c); !ok {
		return
	}
	raw, _ := readCallback(c)
	var in c2bConfirmation
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.TransID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ResultCode": 1, "ResultDesc": "unreadable confirmation"})
		return
	}
	h.recordC2B(c, in, raw, true)
}

func (h *Handler) recordC2B(c *gin.Context, in c2bConfirmation, raw []byte, tryMatch bool) {
	amount, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(in.TransAmount), ",", ""), 64)
	if err != nil || amount <= 0 {
		h.log.Errorf("mpesa: confirmation %s with unreadable amount %q", in.TransID, in.TransAmount)
		c.JSON(http.StatusBadRequest, gin.H{"ResultCode": 1, "ResultDesc": "bad amount"})
		return
	}
	transID := strings.ToUpper(strings.TrimSpace(in.TransID))
	payer := strings.TrimSpace(strings.Join([]string{in.FirstName, in.MiddleName, in.LastName}, " "))

	tx, err := h.db.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ResultCode": 1, "ResultDesc": "temporary failure"})
		return
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRow(`INSERT INTO mpesa_c2b (trans_id, trans_time, amount, bill_ref, msisdn, payer_name, short_code, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (trans_id) DO NOTHING RETURNING id`,
		transID, in.TransTime, round2(amount), in.BillRefNumber, in.MSISDN, payer, in.BusinessShortCode, raw).Scan(&id)
	if err == sql.ErrNoRows {
		mpesaAccepted(c) // Safaricom sent it again; it is already here
		return
	}
	if err != nil {
		h.log.WithError(err).Error("mpesa: store confirmation")
		c.JSON(http.StatusInternalServerError, gin.H{"ResultCode": 1, "ResultDesc": "temporary failure"})
		return
	}
	if tryMatch {
		if username, inv := matchAccount(tx, in.BillRefNumber, in.MSISDN); username != "" {
			// A savepoint keeps the stored confirmation even if applying it fails.
			tx.Exec(`SAVEPOINT apply`)
			ids, err := applyMpesaMoney(tx, username, inv, amount, transID, in.MSISDN, nil)
			if err == nil || err == errAlreadyRecorded {
				tx.Exec(`UPDATE mpesa_c2b SET status='applied', username=$1, payment_ids=$2, updated_at=NOW() WHERE id=$3`,
					username, pq.Array(ids), id)
			} else {
				tx.Exec(`ROLLBACK TO SAVEPOINT apply`)
				h.log.WithError(err).Warnf("mpesa: %s for %s left unmatched", transID, username)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ResultCode": 1, "ResultDesc": "temporary failure"})
		return
	}
	go h.dispatchWebhook("payment.received", gin.H{"gateway": "mpesa", "receipt": transID, "amount": amount, "account": in.BillRefNumber})
	mpesaAccepted(c)
}

// ── Staff and portal endpoints ──────────────────────────────────────────────

func mpesaHTTPError(c *gin.Context, err error) {
	var de *darajaError
	var fw *errFirewall
	switch {
	case err == errMpesaDisabled:
		respondError(c, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &fw):
		c.Header("Retry-After", strconv.Itoa(int(time.Until(fw.until).Seconds())+1))
		respondError(c, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &de):
		respondError(c, http.StatusBadGateway, err.Error())
	case err == errPromptTooSoon:
		respondError(c, http.StatusConflict, err.Error())
	default:
		respondError(c, http.StatusBadRequest, err.Error())
	}
}

// MpesaStatus shows staff how M-Pesa is set up (no secrets).
func (h *Handler) MpesaStatus(c *gin.Context) {
	cl, err := mpesaClient()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "problem": err.Error()})
		return
	}
	mask := func(u string) string { return strings.Replace(u, cl.cfg.Token, cl.cfg.Token[:4]+"…", 1) }
	c.JSON(http.StatusOK, gin.H{"enabled": true, "environment": cl.cfg.Env, "shortcode": cl.cfg.ShortCode,
		"type": cl.cfg.TxType, "pay_to": cl.cfg.PartyB, "stk_callback": mask(cl.cfg.callbackURL("result")),
		"c2b_confirmation": mask(cl.cfg.callbackURL("confirm")), "allowed_ips": len(cl.cfg.AllowedIPs) > 0})
}

// MpesaSendPrompt (staff): send a payment prompt for an invoice to a phone.
func (h *Handler) MpesaSendPrompt(c *gin.Context) {
	var req struct {
		InvoiceID int    `json:"invoice_id" binding:"required"`
		Phone     string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	claims, _ := middleware.GetClaims(c)
	out, err := startSTK(h.db, req.InvoiceID, req.Phone, &claims.UserID, false)
	if err != nil {
		mpesaHTTPError(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (h *Handler) stkState(c *gin.Context, id int, username string) {
	var s struct {
		Status  string  `json:"status"`
		Desc    *string `json:"result_desc"`
		Receipt *string `json:"mpesa_receipt"`
		Amount  float64 `json:"amount"`
		Phone   string  `json:"phone"`
		Created string  `json:"created_at"`
		Invoice *int    `json:"invoice_id"`
		Expiry  *string `json:"account_expiry"`
	}
	q := `SELECT s.status, s.result_desc, s.mpesa_receipt, s.amount, s.phone, s.created_at::text, s.invoice_id,
		(SELECT account_expiry::text FROM radius_users WHERE username = s.username)
		FROM mpesa_stk s WHERE s.id = $1`
	args := []interface{}{id}
	if username != "" {
		q += ` AND s.username = $2`
		args = append(args, username)
	}
	if err := h.db.QueryRow(q, args...).Scan(&s.Status, &s.Desc, &s.Receipt, &s.Amount, &s.Phone, &s.Created, &s.Invoice, &s.Expiry); err != nil {
		respondError(c, http.StatusNotFound, "payment prompt not found")
		return
	}
	c.JSON(http.StatusOK, s)
}

// MpesaPromptStatus (staff): where a prompt stands (the dialog polls it).
func (h *Handler) MpesaPromptStatus(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	h.stkState(c, id, "")
}

// MpesaListPrompts (staff): recent prompts, optionally for one customer.
func (h *Handler) MpesaListPrompts(c *gin.Context) {
	offset, limit := paginationParams(c)
	rows, err := h.db.Query(`SELECT id, username, phone, amount, status, COALESCE(result_desc,''), COALESCE(mpesa_receipt,''),
		invoice_id, via_portal, created_at FROM mpesa_stk WHERE ($1 = '' OR username = $1) ORDER BY id DESC LIMIT $2 OFFSET $3`,
		c.Query("username"), limit, offset)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list prompts")
		return
	}
	defer rows.Close()
	list := []gin.H{}
	for rows.Next() {
		var id int
		var user, phone, status, desc, receipt string
		var amount float64
		var inv *int
		var portal bool
		var created time.Time
		if rows.Scan(&id, &user, &phone, &amount, &status, &desc, &receipt, &inv, &portal, &created) == nil {
			list = append(list, gin.H{"id": id, "username": user, "phone": phone, "amount": amount, "status": status,
				"result_desc": desc, "mpesa_receipt": receipt, "invoice_id": inv, "via_portal": portal, "created_at": created})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// MpesaListC2B (staff): payments made to the paybill/till; ?status=unmatched for the ones
// staff must allocate.
func (h *Handler) MpesaListC2B(c *gin.Context) {
	offset, limit := paginationParams(c)
	rows, err := h.db.Query(`SELECT id, trans_id, amount, COALESCE(bill_ref,''), COALESCE(msisdn,''), COALESCE(payer_name,''),
		status, COALESCE(username,''), created_at FROM mpesa_c2b WHERE ($1 = '' OR status = $1) ORDER BY id DESC LIMIT $2 OFFSET $3`,
		c.Query("status"), limit, offset)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list M-Pesa payments")
		return
	}
	defer rows.Close()
	list := []gin.H{}
	for rows.Next() {
		var id int
		var trans, ref, msisdn, payer, status, user string
		var amount float64
		var created time.Time
		if rows.Scan(&id, &trans, &amount, &ref, &msisdn, &payer, &status, &user, &created) == nil {
			list = append(list, gin.H{"id": id, "trans_id": trans, "amount": amount, "bill_ref": ref, "msisdn": msisdn,
				"payer_name": payer, "status": status, "username": user, "created_at": created})
		}
	}
	var unmatched int
	var unmatchedSum float64
	h.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM mpesa_c2b WHERE status='unmatched'`).Scan(&unmatched, &unmatchedSum)
	c.JSON(http.StatusOK, gin.H{"data": list, "unmatched": unmatched, "unmatched_amount": unmatchedSum})
}

// MpesaAllocateC2B (admin): apply an unmatched payment to a customer (and optionally one of
// their invoices).
func (h *Handler) MpesaAllocateC2B(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Username  string `json:"username" binding:"required"`
		InvoiceID *int   `json:"invoice_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	claims, _ := middleware.GetClaims(c)
	tx, err := h.db.Begin()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to begin transaction")
		return
	}
	defer tx.Rollback()
	var trans, status, msisdn string
	var amount float64
	if err := tx.QueryRow(`SELECT trans_id, status, amount, COALESCE(msisdn,'') FROM mpesa_c2b WHERE id=$1 FOR UPDATE`, id).
		Scan(&trans, &status, &amount, &msisdn); err != nil {
		respondError(c, http.StatusNotFound, "M-Pesa payment not found")
		return
	}
	if status != "unmatched" {
		respondError(c, http.StatusConflict, "this M-Pesa payment has already been applied")
		return
	}
	var username string
	if err := tx.QueryRow(`SELECT username FROM radius_users WHERE username=$1`, strings.TrimSpace(req.Username)).Scan(&username); err != nil {
		respondError(c, http.StatusBadRequest, "no customer with that username")
		return
	}
	if req.InvoiceID != nil {
		var owner sql.NullString
		if tx.QueryRow(`SELECT username FROM invoices WHERE id=$1`, *req.InvoiceID).Scan(&owner) != nil || owner.String != username {
			respondError(c, http.StatusBadRequest, "that invoice is not this customer's")
			return
		}
	}
	ids, err := applyMpesaMoney(tx, username, req.InvoiceID, amount, trans, msisdn, &claims.UserID)
	if err != nil && err != errAlreadyRecorded {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	tx.Exec(`UPDATE mpesa_c2b SET status='applied', username=$1, payment_ids=$2, allocated_by=$3, updated_at=NOW() WHERE id=$4`,
		username, pq.Array(ids), claims.UserID, id)
	if err := tx.Commit(); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to allocate")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("M-Pesa %s applied to %s", trans, username), "payment_ids": ids})
}

// MpesaRegisterC2B (super admin): tell Safaricom where to send paybill/till confirmations.
func (h *Handler) MpesaRegisterC2B(c *gin.Context) {
	cl, err := mpesaClient()
	if err != nil {
		mpesaHTTPError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	if err := cl.registerC2B(ctx); err != nil {
		mpesaHTTPError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Safaricom will send paybill/till payments to this system"})
}

// PortalPay (customer, portal): pay what is owed, or the next period of their plan, with a
// prompt to their phone.
func (h *Handler) PortalPay(c *gin.Context) {
	username := c.GetString("portal_username")
	var req struct {
		Phone string `json:"phone"`
	}
	c.ShouldBindJSON(&req)
	var uid int
	var phone sql.NullString
	var planID sql.NullInt64
	if err := h.db.QueryRow(`SELECT id, phone, plan_id FROM radius_users WHERE username=$1`, username).Scan(&uid, &phone, &planID); err != nil {
		respondError(c, http.StatusNotFound, "account not found")
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		req.Phone = phone.String
	}
	inv, err := h.invoiceToPay(uid, username, planID)
	if err != nil {
		respondError(c, http.StatusBadRequest, err.Error())
		return
	}
	out, err := startSTK(h.db, inv, req.Phone, nil, true)
	if err != nil {
		mpesaHTTPError(c, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

// invoiceToPay is the customer's oldest open invoice, or a new one for their plan's next
// period when nothing is open (a lapsed prepaid customer renewing themselves).
func (h *Handler) invoiceToPay(uid int, username string, planID sql.NullInt64) (int, error) {
	tx, err := h.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockUser(tx, uid); err != nil {
		return 0, err
	}
	var id int
	err = tx.QueryRow(`SELECT id FROM invoices WHERE user_id=$1 AND status IN ('pending','overdue')
		ORDER BY COALESCE(due_date, created_at::date), id LIMIT 1`, uid).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	if !planID.Valid {
		return 0, errors.New("you have no plan to pay for; contact us")
	}
	p, err := loadPlan(tx, int(planID.Int64))
	if err != nil || !p.IsActive || p.Price <= 0 {
		return 0, errors.New("your plan cannot be paid online; contact us")
	}
	pid, pname, days, today := p.ID, p.Name, p.ValidityDays, time.Now()
	inv, err := createInvoiceTx(tx, newInvoice{UserID: &uid, Username: username, PlanID: &pid, PlanName: &pname,
		Amount: p.Price, Currency: p.Currency, DueDate: &today, PlanDays: &days, Notes: "raised from the customer portal"})
	if err != nil {
		return 0, err
	}
	return inv.ID, tx.Commit()
}

// PortalPayStatus (customer): the portal polls this after sending a prompt.
func (h *Handler) PortalPayStatus(c *gin.Context) {
	id, err := mustInt(c, "id")
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid ID")
		return
	}
	h.stkState(c, id, c.GetString("portal_username"))
}

func defaultHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// readCallback reads a body Safaricom posted. A NUL in it (which PostgreSQL cannot store) is
// dropped rather than refusing the request: a payment confirmation must never be lost.
func readCallback(c *gin.Context) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<16))
	raw = bytes.ReplaceAll(raw, []byte{0}, nil)
	raw = bytes.ReplaceAll(raw, []byte(`\u0000`), nil)
	return bytes.ToValidUTF8(raw, nil), err
}
