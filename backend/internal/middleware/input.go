package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// RejectUnstorableText refuses requests carrying text PostgreSQL cannot store: a NUL
// character or invalid UTF-8, in the path, the query or a JSON body. Without this, such text
// reaches a query and fails there, and the caller gets a 500 "server error" for their own bad
// input (found by the route fuzzer: ?status=%00 on every list page). Other bodies (uploads)
// are left alone.
func RejectUnstorableText() gin.HandlerFunc {
	return func(c *gin.Context) {
		bad := func(s string) bool { return strings.ContainsRune(s, 0) || !utf8.ValidString(s) }
		if bad(c.Request.URL.Path) {
			abortBadText(c)
			return
		}
		if q, err := url.ParseQuery(c.Request.URL.RawQuery); err == nil {
			for k, vs := range q {
				if bad(k) {
					abortBadText(c)
					return
				}
				for _, v := range vs {
					if bad(v) {
						abortBadText(c)
						return
					}
				}
			}
		}
		// Safaricom's payment callbacks are let through and cleaned by their handler: a payment
		// confirmation must never be refused.
		if c.Request.Body != nil && strings.Contains(c.GetHeader("Content-Type"), "json") &&
			!strings.HasPrefix(c.Request.URL.Path, "/api/v1/pay/") {
			raw, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large or unreadable"})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) || jsonHasNUL(raw) {
				abortBadText(c)
				return
			}
		}
		c.Next()
	}
}

// jsonHasNUL finds the escape \u0000 in a JSON text (an escaped backslash before it, as in
// "\\u0000", is the literal text and fine).
func jsonHasNUL(raw []byte) bool {
	for i := 0; i+5 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] == 'u' && string(raw[i+2:i+6]) == "0000" {
			return true
		}
		i++ // skip the escaped character
	}
	return false
}

func abortBadText(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "the request contains a NUL character or invalid text"})
}
