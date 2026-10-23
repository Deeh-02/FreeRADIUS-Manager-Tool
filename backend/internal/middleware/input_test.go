package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRejectUnstorableText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RejectUnstorableText())
	echo := func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		c.String(200, string(b))
	}
	r.Any("/api/v1/*rest", echo)

	cases := []struct {
		method, path, ctype, body string
		want                      int
	}{
		{"GET", "/api/v1/payments?status=paid", "", "", 200},
		{"GET", "/api/v1/payments?status=%00", "", "", 400},
		{"GET", "/api/v1/payments?search=%FF%FE", "", "", 400},
		{"GET", "/api/v1/payments/%00", "", "", 400},
		{"POST", "/api/v1/plans", "application/json", `{"name":"Home"}`, 200},
		{"POST", "/api/v1/plans", "application/json", `{"name":"a\u0000b"}`, 400},
		{"POST", "/api/v1/plans", "application/json", `{"name":"a\\u0000b"}`, 200}, // the literal text \u0000
		{"POST", "/api/v1/plans", "application/json; charset=utf-8", "{\"name\":\"a\x00\"}", 400},
		{"POST", "/api/v1/plans", "application/json", "{\"name\":\"\xff\"}", 400},
		{"POST", "/api/v1/restore", "multipart/form-data; boundary=x", "\x00\x01binary", 200}, // uploads untouched
		{"POST", "/api/v1/pay/confirm/tok", "application/json", `{"FirstName":"a\u0000"}`, 200}, // M-Pesa: cleaned by the handler, never refused
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.ctype != "" {
			req.Header.Set("Content-Type", tc.ctype)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s %s %q: %d, want %d", tc.method, tc.path, tc.body, w.Code, tc.want)
		}
		if w.Code == http.StatusOK && w.Body.String() != tc.body {
			t.Errorf("%s %s: the handler got %q, want the body unchanged", tc.method, tc.path, w.Body.String())
		}
	}
}
