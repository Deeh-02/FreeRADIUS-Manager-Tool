package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaraja imitates the Safaricom Daraja endpoints the backend calls, and checks every
// request the way Safaricom does (credentials, STK password and timestamp, amounts, phone
// numbers, callback URL). Tests make "the customer" pay, cancel or stay silent and decide
// whether the callback arrives.
type fakeDaraja struct {
	t      *testing.T
	srv    *httptest.Server
	key    string
	secret string
	short  string
	pass   string

	mu         sync.Mutex
	tokens     map[string]bool
	oauthCalls int
	pushes     []fakePush
	query      map[string]string // checkout id -> result code; "" = still processing
	pushFail   int               // HTTP status to answer pushes with (0 = accept)
	instantPay bool              // post a success callback before answering the push
	problems   []string
	nextID     int
	registered map[string]string
	firewall   bool      // answer everything with Incapsula's HTML 403, as the real sandbox does to bursts
	requests   int       // every request that reached the fake
	lastReq    time.Time // to check the backend spaces its requests out
	spikes     int       // answer this many coming requests with Daraja's 429 spike arrest
	waitingAnswers int   // rotates the forms of "not answered yet"
}

type fakePush struct {
	CheckoutID string
	Phone      string
	Amount     int
	AccountRef string
	Callback   string
}

func newFakeDaraja(t *testing.T) *fakeDaraja {
	f := &fakeDaraja{t: t, key: "test-consumer-key", secret: "test-consumer-secret", short: "174379",
		pass:   "test-passkey-not-a-real-one",
		tokens: map[string]bool{}, query: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/generate", f.oauth)
	mux.HandleFunc("/mpesa/stkpush/v1/processrequest", f.authed(f.push))
	mux.HandleFunc("/mpesa/stkpushquery/v1/query", f.authed(f.stkQuery))
	mux.HandleFunc("/mpesa/c2b/v1/registerurl", f.authed(f.register))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		if gap := time.Since(f.lastReq); !f.lastReq.IsZero() && darajaMinGap > 0 && gap < darajaMinGap-100*time.Millisecond {
			f.problems = append(f.problems, fmt.Sprintf("requests only %s apart; Safaricom's firewall blocks bursts", gap.Round(time.Millisecond)))
		}
		f.lastReq = time.Now()
		blocked, spike := f.firewall, f.spikes > 0
		if spike {
			f.spikes--
		}
		f.mu.Unlock()
		if spike {
			// The real answer, from sandbox.safaricom.co.ke.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			w.Write([]byte(`{"fault":{"faultstring":"Spike arrest violation. Allowed rate : MessageRate{messagesPerPeriod=30, periodInMicroseconds=60000000, maxBurstMessageCount=3.0}","detail":{"errorcode":"policies.ratelimit.SpikeArrestViolation"}}}`))
			return
		}
		if blocked {
			// Shape of the real answer (sandbox.safaricom.co.ke, October 2026).
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(403)
			w.Write([]byte(`<html style="height:100%"><head><META NAME="ROBOTS" CONTENT="NOINDEX, NOFOLLOW"></head><body><iframe id="main-iframe" src="/_Incapsula_Resource?CWUDNSAI=23">Request unsuccessful. Incapsula incident ID: 942000580920878110-518748877376851634</iframe></body></html>`))
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDaraja) setFirewall(on bool) {
	f.mu.Lock()
	f.firewall = on
	f.mu.Unlock()
}

func (f *fakeDaraja) setSpikes(n int) {
	f.mu.Lock()
	f.spikes = n
	f.mu.Unlock()
}

func (f *fakeDaraja) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeDaraja) problem(format string, a ...interface{}) {
	f.mu.Lock()
	f.problems = append(f.problems, fmt.Sprintf(format, a...))
	f.mu.Unlock()
}

// check fails the test if any request the backend sent was not one Safaricom would accept.
func (f *fakeDaraja) check() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.problems {
		f.t.Errorf("Daraja would refuse: %s", p)
	}
	f.problems = nil
}

func (f *fakeDaraja) oauth(w http.ResponseWriter, r *http.Request) {
	user, pw, ok := r.BasicAuth()
	if !ok || user != f.key || pw != f.secret || r.URL.Query().Get("grant_type") != "client_credentials" {
		w.WriteHeader(400) // the real sandbox: 400 with an empty body
		return
	}
	f.mu.Lock()
	f.oauthCalls++
	tok := fmt.Sprintf("tok-%d", f.oauthCalls)
	f.tokens[tok] = true
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"access_token": tok, "expires_in": "3599"})
}

// revokeTokens makes every issued token invalid, as Safaricom sometimes does early.
func (f *fakeDaraja) revokeTokens() {
	f.mu.Lock()
	f.tokens = map[string]bool{}
	f.mu.Unlock()
}

func (f *fakeDaraja) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.tokens[tok]
		f.mu.Unlock()
		if !ok {
			// What the real sandbox answers (checked against sandbox.safaricom.co.ke): 404, not 401.
			w.WriteHeader(404)
			w.Write([]byte(`{"requestId":"b65b-47a4","errorCode": "404.001.03","errorMessage": "Invalid Access Token"}`))
			return
		}
		next(w, r)
	}
}

func (f *fakeDaraja) checkPassword(pw, ts string) {
	t, err := time.ParseInLocation("20060102150405", ts, nairobi)
	if err != nil {
		f.problem("timestamp %q not YYYYMMDDHHmmss", ts)
	} else if d := time.Since(t); d > 5*time.Minute || d < -5*time.Minute {
		f.problem("timestamp %s is %s off Kenyan time", ts, d.Round(time.Second))
	}
	if want := base64.StdEncoding.EncodeToString([]byte(f.short + f.pass + ts)); pw != want {
		f.problem("wrong STK password")
	}
}

func (f *fakeDaraja) push(w http.ResponseWriter, r *http.Request) {
	var in map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		f.problem("push body not JSON")
		w.WriteHeader(400)
		return
	}
	s := func(k string) string { v, _ := in[k].(string); return v }
	f.checkPassword(s("Password"), s("Timestamp"))
	if s("BusinessShortCode") != f.short {
		f.problem("BusinessShortCode %q", s("BusinessShortCode"))
	}
	amt, ok := in["Amount"].(float64)
	if !ok || amt != float64(int(amt)) || amt < 1 {
		f.problem("Amount %v must be a whole number of shillings ≥ 1", in["Amount"])
	}
	phone := s("PhoneNumber")
	if len(phone) != 12 || !strings.HasPrefix(phone, "254") || s("PartyA") != phone {
		f.problem("phone %q / PartyA %q", phone, s("PartyA"))
	}
	if s("TransactionType") != "CustomerPayBillOnline" && s("TransactionType") != "CustomerBuyGoodsOnline" {
		f.problem("TransactionType %q", s("TransactionType"))
	}
	if len(s("AccountReference")) > 12 || len(s("TransactionDesc")) > 13 {
		f.problem("AccountReference/TransactionDesc too long")
	}
	cb := s("CallBackURL")
	if !strings.HasPrefix(cb, "http") {
		f.problem("CallBackURL %q", cb)
	}
	f.mu.Lock()
	if f.pushFail != 0 {
		code := f.pushFail
		f.mu.Unlock()
		w.WriteHeader(code)
		w.Write([]byte(`{"errorCode":"500.003.02","errorMessage":"System is busy. Please try again in few minutes."}`))
		return
	}
	f.nextID++
	id := fmt.Sprintf("ws_CO_%s_%05d", time.Now().Format("02012006150405"), f.nextID)
	p := fakePush{CheckoutID: id, Phone: phone, Amount: int(amt), AccountRef: s("AccountReference"), Callback: cb}
	f.pushes = append(f.pushes, p)
	f.query[id] = ""
	instant := f.instantPay
	f.mu.Unlock()
	if instant {
		// The customer is quicker than our own bookkeeping: the result arrives before the
		// backend has stored the CheckoutRequestID.
		receipt := fmt.Sprintf("QI%08d", f.nextID)
		go f.sendResult(p, 0, receipt)
		time.Sleep(100 * time.Millisecond)
	}
	json.NewEncoder(w).Encode(map[string]string{"MerchantRequestID": "29115-34620561-1", "CheckoutRequestID": id,
		"ResponseCode": "0", "ResponseDescription": "Success. Request accepted for processing",
		"CustomerMessage": "Success. Request accepted for processing"})
}

func (f *fakeDaraja) stkQuery(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	json.NewDecoder(r.Body).Decode(&in)
	f.checkPassword(in["Password"], in["Timestamp"])
	f.mu.Lock()
	code, ok := f.query[in["CheckoutRequestID"]]
	f.waitingAnswers++
	form := f.waitingAnswers % 3
	f.mu.Unlock()
	switch {
	case !ok:
		w.WriteHeader(500)
		w.Write([]byte(`{"errorCode":"500.001.1001","errorMessage":"Unable to lock subscriber, a transaction is already in process for the current subscriber"}`))
	case code == "":
		// The real sandbox says "not answered yet" in three ways (all seen, October 2026).
		switch form {
		case 0:
			w.WriteHeader(500)
			w.Write([]byte(`{"requestId":"x","errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`))
		case 1:
			w.WriteHeader(500)
			w.Write([]byte(`{"requestId":"` + in["CheckoutRequestID"] + `","errorCode": "500.001.1001","errorMessage": ""}`))
		default: // an ordinary-looking answer with a result code that is not a result
			json.NewEncoder(w).Encode(map[string]string{"ResponseCode": "0", "ResponseDescription": "The service request has been accepted successsfully",
				"MerchantRequestID": "m", "CheckoutRequestID": in["CheckoutRequestID"], "ResultCode": "4999",
				"ResultDesc": "The transaction is still under processing"})
		}
	default:
		json.NewEncoder(w).Encode(map[string]string{"ResponseCode": "0", "ResultCode": code, "ResultDesc": "result " + code,
			"CheckoutRequestID": in["CheckoutRequestID"]})
	}
}

func (f *fakeDaraja) register(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	f.registered = in
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"OriginatorCoversationID": "x", "ResponseCode": "0", "ResponseDescription": "success"})
}

func (f *fakeDaraja) lastPushOrEmpty() fakePush {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushes) == 0 {
		return fakePush{}
	}
	return f.pushes[len(f.pushes)-1]
}

func (f *fakeDaraja) lastPush() fakePush {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushes) == 0 {
		f.t.Fatal("no STK push reached Daraja")
	}
	return f.pushes[len(f.pushes)-1]
}

// settle decides the prompt's result as Safaricom will report it to a query.
func (f *fakeDaraja) settle(id string, code int) {
	f.mu.Lock()
	f.query[id] = fmt.Sprint(code)
	f.mu.Unlock()
}

// sendResult posts the STK callback for a prompt and returns the HTTP status the backend gave.
func (f *fakeDaraja) sendResult(p fakePush, code int, receipt string) int {
	f.settle(p.CheckoutID, code)
	cb := map[string]interface{}{"MerchantRequestID": "29115-34620561-1", "CheckoutRequestID": p.CheckoutID,
		"ResultCode": code, "ResultDesc": map[int]string{0: "The service request is processed successfully.",
			1032: "Request cancelled by user", 1037: "DS timeout user cannot be reached", 2001: "The initiator information is invalid."}[code]}
	if code == 0 {
		cb["CallbackMetadata"] = map[string]interface{}{"Item": []map[string]interface{}{
			{"Name": "Amount", "Value": p.Amount}, {"Name": "MpesaReceiptNumber", "Value": receipt},
			{"Name": "Balance"}, {"Name": "TransactionDate", "Value": 20261023150405}, {"Name": "PhoneNumber", "Value": 254712345678}}}
	}
	return postJSON(p.Callback, map[string]interface{}{"Body": map[string]interface{}{"stkCallback": cb}})
}

func postJSON(url string, body interface{}) int {
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}
