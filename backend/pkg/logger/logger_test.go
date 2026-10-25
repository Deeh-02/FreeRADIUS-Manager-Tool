package logger

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// The M-Pesa callback addresses end in a secret; whoever reads the request log must not learn it.
func TestCallbackSecretNotLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var buf bytes.Buffer
	log := logrus.New()
	log.SetOutput(&buf)
	r := gin.New()
	r.Use(GinLogger(log))
	r.POST("/api/v1/pay/result/:token", func(c *gin.Context) { c.Status(200) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/pay/result/s3cr3t-Token_value?x=1", nil))
	out := buf.String()
	if strings.Contains(out, "s3cr3t") {
		t.Errorf("callback secret in the log: %s", out)
	}
	if !strings.Contains(out, "/api/v1/pay/result/REDACTED") {
		t.Errorf("log line does not show the masked path: %s", out)
	}
}
