package handlers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Our Daraja client against the real Safaricom sandbox, with no backend or database: sign-in,
// a refused token, prompts (good and bad), the result query, and paybill URL registration.
// It checks that what Safaricom really answers is what the code expects, which the fake
// Daraja in fake_daraja_test.go copies.
//
//	MPESA_SANDBOX_CLIENT=1 MPESA_CONSUMER_KEY=... MPESA_CONSUMER_SECRET=... MPESA_PASSKEY=... \
//	  [MPESA_SHORTCODE=174379] [MPESA_C2B_SHORTCODE=600xxx] go test ./internal/handlers -run SandboxClient -v
//
// The prompts go to Safaricom's test number 254708374149, not a real phone; nothing is paid.

func sandboxClient(t *testing.T) *darajaClient {
	t.Helper()
	if os.Getenv("MPESA_SANDBOX_CLIENT") == "" {
		t.Skip("MPESA_SANDBOX_CLIENT not set")
	}
	short := os.Getenv("MPESA_SHORTCODE")
	if short == "" {
		short = "174379"
	}
	cfg := &mpesaConfig{BaseURL: "https://sandbox.safaricom.co.ke", ShortCode: short, PartyB: short,
		TxType: "CustomerPayBillOnline", ConsumerKey: os.Getenv("MPESA_CONSUMER_KEY"),
		ConsumerSecret: os.Getenv("MPESA_CONSUMER_SECRET"), Passkey: os.Getenv("MPESA_PASSKEY"),
		CallbackBase: "https://example.com", Token: "sandbox-client-test-0123456789", Env: "sandbox"}
	cfg.C2BShortCode = os.Getenv("MPESA_C2B_SHORTCODE")
	if cfg.ConsumerKey == "" || cfg.ConsumerSecret == "" || cfg.Passkey == "" {
		t.Fatal("set MPESA_CONSUMER_KEY, MPESA_CONSUMER_SECRET and MPESA_PASSKEY")
	}
	return &darajaClient{cfg: cfg, http: defaultHTTPClient()}
}

const sandboxTestPhone = "254708374149"

func sbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 40*time.Second)
}

func TestSandboxClientSignIn(t *testing.T) {
	d := sandboxClient(t)
	ctx, cancel := sbCtx()
	defer cancel()
	tok, err := d.accessToken(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(d.tokenExp); until < 50*time.Minute || until > 61*time.Minute {
		t.Errorf("token kept for %s; Safaricom gives an hour, we keep it a minute less", until)
	}
	again, _ := d.accessToken(ctx, false)
	if again != tok {
		t.Error("a second call signed in again instead of reusing the token")
	}

	// Wrong secret: refused, and the message says which settings to check.
	bad := &darajaClient{cfg: &mpesaConfig{BaseURL: d.cfg.BaseURL, ConsumerKey: d.cfg.ConsumerKey, ConsumerSecret: "wrong"}, http: defaultHTTPClient()}
	_, err = bad.accessToken(ctx, false)
	var de *darajaError
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "MPESA_CONSUMER_KEY") {
		t.Errorf("wrong secret: %v", err)
	}
	t.Logf("wrong secret answers: %v", err)
}

// Safaricom drops tokens before they expire. A refused token must be replaced silently.
func TestSandboxClientRefusedToken(t *testing.T) {
	d := sandboxClient(t)
	d.token, d.tokenExp = "notAValidTokenAtAll0000", time.Now().Add(time.Hour)
	ctx, cancel := sbCtx()
	defer cancel()
	reply, err := d.stkPush(ctx, sandboxTestPhone, 1, "TOKENTEST", "Internet")
	if err != nil {
		t.Fatalf("with a dropped token the prompt failed instead of signing in again: %v", err)
	}
	if d.token == "notAValidTokenAtAll0000" {
		t.Error("the refused token is still cached")
	}
	t.Logf("signed in again and sent: %+v", *reply)
}

// A prompt to the test number: accepted, then the query says "being processed" (which must be
// read as pending, not as a failure), then a final result.
func TestSandboxClientPromptAndQuery(t *testing.T) {
	d := sandboxClient(t)
	ctx, cancel := sbCtx()
	reply, err := d.stkPush(ctx, sandboxTestPhone, 1, "INV-SANDBOX-0001-LONG", "Internet subscription")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prompt accepted: %+v", *reply)
	if !strings.HasPrefix(reply.CheckoutRequestID, "ws_CO_") {
		t.Errorf("CheckoutRequestID %q: expected ws_CO_…", reply.CheckoutRequestID)
	}

	deadline := time.Now().Add(3 * time.Minute)
	sawPending := false
	for time.Now().Before(deadline) {
		ctx, cancel := sbCtx()
		code, desc, pending, err := d.stkQuery(ctx, reply.CheckoutRequestID)
		cancel()
		switch {
		case err != nil:
			var de *darajaError
			if errors.As(err, &de) && de.Status == 429 || strings.Contains(err.Error(), "Spike") {
				t.Logf("rate limited by the sandbox: %v", err)
				time.Sleep(10 * time.Second)
				continue
			}
			t.Fatalf("query failed: %v (this would leave the prompt pending forever)", err)
		case pending:
			if !sawPending {
				t.Logf("query while waiting: pending (%s)", desc)
			}
			sawPending = true
			time.Sleep(5 * time.Second)
			continue
		}
		t.Logf("final result from the query: code %q %q (pending seen first: %v)", code, desc, sawPending)
		if code == "" {
			t.Error("a final answer with an empty ResultCode would be settled as a failure with no reason")
		}
		return
	}
	t.Log("no final result within 3 minutes (the sandbox's test number sometimes never answers); the reconcile loop gives up after a day")
}

// What Safaricom answers to prompts it refuses, so staff see something useful.
func TestSandboxClientRefusedPrompts(t *testing.T) {
	d := sandboxClient(t)
	cases := []struct {
		name   string
		mutate func(c *mpesaConfig) (phone string, amount int)
	}{
		{"wrong passkey", func(c *mpesaConfig) (string, int) { c.Passkey = strings.Repeat("0", 64); return sandboxTestPhone, 1 }},
		{"amount 0", func(c *mpesaConfig) (string, int) { return sandboxTestPhone, 0 }},
		{"not a phone", func(c *mpesaConfig) (string, int) { return "254000", 1 }},
		{"callback not https", func(c *mpesaConfig) (string, int) { c.CallbackBase = "http://example.com"; return sandboxTestPhone, 1 }},
		{"callback with a refused word", func(c *mpesaConfig) (string, int) { c.CallbackBase = "https://mpesa.example.com"; return sandboxTestPhone, 1 }},
		{"wrong shortcode", func(c *mpesaConfig) (string, int) { c.ShortCode, c.PartyB = "999999", "999999"; return sandboxTestPhone, 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *d.cfg
			phone, amount := tc.mutate(&cfg)
			c := &darajaClient{cfg: &cfg, http: d.http, token: d.token, tokenExp: d.tokenExp}
			ctx, cancel := sbCtx()
			defer cancel()
			reply, err := c.stkPush(ctx, phone, amount, "REFUSED", "Internet")
			d.token, d.tokenExp = c.token, c.tokenExp
			if err == nil {
				t.Logf("ACCEPTED by the sandbox: %+v", *reply)
				return
			}
			t.Logf("refused: %v", err)
			if strings.Contains(err.Error(), "twice") {
				t.Errorf("a refusal was mistaken for a bad token: %v", err)
			}
		})
	}
}

func TestSandboxClientRegisterPaybill(t *testing.T) {
	d := sandboxClient(t)
	codes := []string{d.cfg.C2BShortCode}
	if codes[0] == "" {
		codes = []string{"600000", "600584", "600998", "174379"} // try the usual sandbox test codes
	}
	for _, code := range codes {
		cfg := *d.cfg
		cfg.C2BShortCode = code
		c := &darajaClient{cfg: &cfg, http: d.http}
		ctx, cancel := sbCtx()
		err := c.registerC2B(ctx)
		cancel()
		t.Logf("register paybill URLs on %s: %v", code, err)
		ctx, cancel = sbCtx()
		err2 := c.registerC2B(ctx)
		cancel()
		t.Logf("  again on %s: %v", code, err2)
	}
}
